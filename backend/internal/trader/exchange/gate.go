package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type gateAdapter struct {
	client *signedClient
}

func newGate(client *http.Client, base string) Adapter {
	if base == "" {
		base = "https://api.gateio.ws"
	}
	return &gateAdapter{client: newSignedClient(client, base, 80*time.Millisecond)}
}

func (a *gateAdapter) GetBBO(ctx context.Context, instrument Instrument) (BBO, error) {
	if instrument.ContractType == "spot" {
		values := url.Values{
			"currency_pair": {gateSpotSymbol(instrument)},
			"limit":         {"1"},
		}
		raw, err := a.client.do(ctx, http.MethodGet, "/api/v4/spot/order_book?"+values.Encode(), nil, nil, nil)
		if err != nil {
			return BBO{}, err
		}
		var payload struct {
			Current float64    `json:"current"`
			Bids    [][]string `json:"bids"`
			Asks    [][]string `json:"asks"`
			Label   string     `json:"label"`
			Message string     `json:"message"`
		}
		if err := unmarshalJSON(raw, &payload); err != nil {
			return BBO{}, err
		}
		if payload.Label != "" {
			return BBO{}, fmt.Errorf("%w: Gate market code %s: %s", ErrRejected, payload.Label, payload.Message)
		}
		if len(payload.Bids) == 0 || len(payload.Bids[0]) == 0 || len(payload.Asks) == 0 || len(payload.Asks[0]) == 0 {
			return BBO{}, fmt.Errorf("decode venue BBO: empty Gate spot order book")
		}
		return bbo(payload.Bids[0][0], payload.Asks[0][0], unixTimestamp(strconv.FormatFloat(payload.Current, 'f', -1, 64)))
	}

	values := url.Values{
		"contract": {gateFuturesSymbol(instrument)},
		"limit":    {"1"},
	}
	path := "/api/v4/futures/" + gateSettle(instrument) + "/order_book?" + values.Encode()
	raw, err := a.client.do(ctx, http.MethodGet, path, nil, nil, nil)
	if err != nil {
		return BBO{}, err
	}
	var payload struct {
		Current float64 `json:"current"`
		Bids    []struct {
			Price string `json:"p"`
		} `json:"bids"`
		Asks []struct {
			Price string `json:"p"`
		} `json:"asks"`
		Label   string `json:"label"`
		Message string `json:"message"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return BBO{}, err
	}
	if payload.Label != "" {
		return BBO{}, fmt.Errorf("%w: Gate market code %s: %s", ErrRejected, payload.Label, payload.Message)
	}
	if len(payload.Bids) == 0 || len(payload.Asks) == 0 {
		return BBO{}, fmt.Errorf("decode venue BBO: empty Gate futures order book")
	}
	return bbo(payload.Bids[0].Price, payload.Asks[0].Price, unixTimestamp(strconv.FormatFloat(payload.Current, 'f', -1, 64)))
}

func (a *gateAdapter) GetPositionMode(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
) (string, error) {
	if instrument.ContractType == "spot" {
		return PositionModeOneWay, nil
	}
	path := "/api/v4/futures/" + gateSettle(instrument) + "/accounts"
	raw, err := a.signed(ctx, http.MethodGet, path, credentials, nil, nil)
	if err != nil {
		_, requestErr := gateRequestError(raw, err)
		return "", requestErr
	}
	var payload struct {
		PositionMode string `json:"position_mode"`
		InDualMode   bool   `json:"in_dual_mode"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return "", err
	}
	switch payload.PositionMode {
	case "", "single":
		if payload.InDualMode {
			return PositionModeHedge, nil
		}
		return PositionModeOneWay, nil
	case "dual", "dual_plus":
		return PositionModeHedge, nil
	default:
		return "", fmt.Errorf("unknown Gate position mode %q", payload.PositionMode)
	}
}

func (a *gateAdapter) PlaceOrder(ctx context.Context, credentials Credentials, request OrderRequest) (Result, error) {
	quantity, price, err := gateOrderValues(request)
	if err != nil {
		return Result{}, err
	}
	if request.Instrument.ContractType == "spot" {
		body := map[string]any{
			"currency_pair": gateSpotSymbol(request.Instrument),
			"type":          request.OrderType,
			"account":       "unified",
			"side":          request.Side,
			"amount":        quantity,
			"text":          gateClientText(request.ClientOrderID),
		}
		if request.OrderType == "limit" {
			body["price"] = price
			switch orderTimeInForce(request) {
			case "post_only":
				body["time_in_force"] = "poc"
			case "ioc":
				body["time_in_force"] = "ioc"
			default:
				body["time_in_force"] = "gtc"
			}
		} else {
			body["time_in_force"] = "ioc"
		}
		raw, err := a.signed(ctx, http.MethodPost, "/api/v4/spot/orders", credentials, compactJSON(body), nil)
		if err != nil {
			return gateRequestError(raw, err)
		}
		return parseGateSpot(raw)
	}
	wireQuantity, err := ToVenueQuantity(request.Instrument, quantity)
	if err != nil {
		return Result{}, err
	}
	size, err := decimal.NewFromString(wireQuantity)
	if err != nil || !size.IsPositive() {
		return Result{}, fmt.Errorf("%w: invalid gate contract quantity", ErrRejected)
	}
	if request.Side == "sell" {
		size = size.Neg()
	}
	body := map[string]any{
		"contract": gateFuturesSymbol(request.Instrument),
		"size":     json.Number(size.String()),
		"text":     gateClientText(request.ClientOrderID),
	}
	if request.OrderType == "limit" {
		body["price"] = price
		switch orderTimeInForce(request) {
		case "post_only":
			body["tif"] = "poc"
		case "ioc":
			body["tif"] = "ioc"
		default:
			body["tif"] = "gtc"
		}
	} else {
		body["price"] = "0"
		body["tif"] = "ioc"
	}
	if request.ReduceOnly {
		body["reduce_only"] = true
	}
	raw, err := a.signed(
		ctx, http.MethodPost,
		"/api/v4/futures/"+gateSettle(request.Instrument)+"/orders",
		credentials, compactJSON(body), nil,
	)
	if err != nil {
		return gateRequestError(raw, err)
	}
	return parseGateFutures(raw, request.Instrument)
}

func (a *gateAdapter) GetOrder(ctx context.Context, credentials Credentials, request QueryRequest) (Result, error) {
	if request.Instrument.ContractType == "spot" {
		path := "/api/v4/spot/orders/" + url.PathEscape(gateLookupID(request.VenueOrderID, request.ClientOrderID)) +
			"?currency_pair=" + url.QueryEscape(gateSpotSymbol(request.Instrument))
		raw, err := a.signed(ctx, http.MethodGet, path, credentials, nil, nil)
		if err != nil {
			return unknownQueryError(classifyGateRequestError(raw, err, true))
		}
		return unknownQueryError(parseGateSpot(raw))
	}
	path := "/api/v4/futures/" + gateSettle(request.Instrument) + "/orders/" +
		url.PathEscape(gateLookupID(request.VenueOrderID, request.ClientOrderID))
	raw, err := a.signed(ctx, http.MethodGet, path, credentials, nil, nil)
	if err != nil {
		return unknownQueryError(classifyGateRequestError(raw, err, true))
	}
	return unknownQueryError(parseGateFutures(raw, request.Instrument))
}

func (a *gateAdapter) ResolveOrder(
	ctx context.Context,
	credentials Credentials,
	request QueryRequest,
) (OrderResolution, error) {
	if err := requireOrderLookupID(request); err != nil {
		return OrderResolution{}, err
	}
	return exactOrderResolution(a.GetOrder(ctx, credentials, request))
}

func (a *gateAdapter) CancelOrder(ctx context.Context, credentials Credentials, request CancelRequest) (Result, error) {
	accepted, err := a.sendCancelOrder(ctx, credentials, request)
	return commandOnlyCancelResult(accepted, err)
}

func (a *gateAdapter) CancelAndGetOrder(ctx context.Context, credentials Credentials, request CancelRequest) (Result, error) {
	accepted, err := a.sendCancelOrder(ctx, credentials, request)
	if err != nil {
		return accepted, err
	}
	return getOrderAfterCancel(ctx, a.GetOrder, credentials, request, accepted, "Gate")
}

func (a *gateAdapter) sendCancelOrder(ctx context.Context, credentials Credentials, request CancelRequest) (Result, error) {
	if request.Instrument.ContractType == "spot" {
		path := "/api/v4/spot/orders/" + url.PathEscape(gateLookupID(request.VenueOrderID, request.ClientOrderID)) +
			"?currency_pair=" + url.QueryEscape(gateSpotSymbol(request.Instrument))
		raw, err := a.signed(ctx, http.MethodDelete, path, credentials, nil, nil)
		if err != nil {
			return gateRequestError(raw, err)
		}
		return parseGateSpot(raw)
	}
	path := "/api/v4/futures/" + gateSettle(request.Instrument) + "/orders/" +
		url.PathEscape(gateLookupID(request.VenueOrderID, request.ClientOrderID))
	raw, err := a.signed(ctx, http.MethodDelete, path, credentials, nil, nil)
	if err != nil {
		return gateRequestError(raw, err)
	}
	return parseGateFutures(raw, request.Instrument)
}

func (a *gateAdapter) signed(
	ctx context.Context,
	method, path string,
	credentials Credentials,
	body []byte,
	target any,
) ([]byte, error) {
	ts := fmt.Sprintf("%d", timeUnix())
	pathOnly, query := path, ""
	if parts := strings.SplitN(path, "?", 2); len(parts) == 2 {
		pathOnly, query = parts[0], parts[1]
	}
	payload := method + "\n" + pathOnly + "\n" + query + "\n" + sha512Hex(body) + "\n" + ts
	headers := http.Header{
		"KEY":                 {credentials.APIKey},
		"Timestamp":           {ts},
		"SIGN":                {hmacHex512(credentials.APISecret, payload)},
		"X-Gate-Size-Decimal": {"1"},
	}
	return a.client.do(ctx, method, path, headers, body, target)
}

func gateSpotSymbol(instrument Instrument) string {
	if wire := metadataString(instrument, "currency_pair"); wire != "" {
		return wire
	}
	return instrument.BaseAsset + "_" + instrument.QuoteAsset
}

func gateFuturesSymbol(instrument Instrument) string {
	if wire := metadataString(instrument, "contract"); wire != "" {
		return wire
	}
	return instrument.BaseAsset + "_" + instrument.QuoteAsset
}

func gateSettle(instrument Instrument) string {
	settle := strings.ToLower(firstNonEmpty(instrument.SettleAsset, instrument.QuoteAsset))
	switch settle {
	case "btc", "usd":
		return settle
	default:
		return "usdt"
	}
}

func gateClientText(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "t-")
	const maxPayloadBytes = 26 // Gate permits 28 bytes including the required t- prefix.
	if len(id) > maxPayloadBytes {
		id = id[:maxPayloadBytes]
	}
	return "t-" + id
}

func gateLookupID(venueOrderID, clientOrderID string) string {
	if id := strings.TrimSpace(venueOrderID); id != "" {
		return id
	}
	return gateClientText(clientOrderID)
}

func parseGateSpot(raw []byte) (Result, error) {
	var payload struct {
		ID           flexString `json:"id"`
		Status       string     `json:"status"`
		FinishAs     string     `json:"finish_as"`
		FilledAmount string     `json:"filled_amount"`
		FilledTotal  string     `json:"filled_total"`
		AvgDealPrice string     `json:"avg_deal_price"`
		FillPrice    string     `json:"fill_price"`
		Left         string     `json:"left"`
		Label        string     `json:"label"`
		Message      string     `json:"message"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return Result{}, err
	}
	if payload.Label != "" && payload.ID.String() == "" {
		return Result{Status: "rejected", ErrorCode: payload.Label, ErrorMessage: payload.Message, Raw: rawMap(raw)}, ErrRejected
	}
	status := normalizeStatus(firstNonEmpty(payload.FinishAs, payload.Status))
	if finishStatus, ok := gateFinishStatus(payload.FinishAs); ok {
		status = finishStatus
	}
	return Result{
		VenueOrderID: payload.ID.String(), Status: status,
		FilledQuantity: payload.FilledAmount,
		AveragePrice:   firstNonEmpty(payload.AvgDealPrice, payload.FillPrice),
		Raw:            rawMap(raw),
	}, nil
}

func parseGateFutures(raw []byte, instrument Instrument) (Result, error) {
	var payload struct {
		ID        flexString `json:"id"`
		Status    string     `json:"status"`
		FinishAs  string     `json:"finish_as"`
		FillPrice string     `json:"fill_price"`
		Size      flexString `json:"size"`
		Left      flexString `json:"left"`
		Label     string     `json:"label"`
		Message   string     `json:"message"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return Result{}, err
	}
	if payload.Label != "" && payload.ID.String() == "" {
		return Result{Status: "rejected", ErrorCode: payload.Label, ErrorMessage: payload.Message, Raw: rawMap(raw)}, ErrRejected
	}
	size, err := decimal.NewFromString(payload.Size.String())
	if err != nil {
		return Result{}, fmt.Errorf("decode Gate futures size: %w", err)
	}
	left, err := decimal.NewFromString(payload.Left.String())
	if err != nil {
		return Result{}, fmt.Errorf("decode Gate futures left: %w", err)
	}
	filled := size.Sub(left).Abs()
	filledQuantity, err := FromVenueQuantity(instrument, filled.String())
	if err != nil {
		return Result{}, err
	}
	status := gateFuturesStatus(payload.Status, payload.FinishAs, size, left)
	return Result{
		VenueOrderID: payload.ID.String(), Status: status,
		FilledQuantity: filledQuantity, AveragePrice: payload.FillPrice,
		Raw: rawMap(raw),
	}, nil
}

func gateFuturesStatus(status, finishAs string, size, left decimal.Decimal) string {
	normalized := normalizeStatus(firstNonEmpty(finishAs, status))
	filled := size.Sub(left).Abs()
	total := size.Abs()
	if filled.IsPositive() && filled.GreaterThanOrEqual(total) {
		return "filled"
	}
	if finishStatus, ok := gateFinishStatus(finishAs); ok {
		return finishStatus
	}
	if normalized != "unknown" {
		return normalized
	}
	if filled.IsPositive() {
		return "partially_filled"
	}
	return normalized
}

func gateFinishStatus(finishAs string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(finishAs)) {
	case "filled":
		return "filled", true
	case "ioc", "fok", "cancelled", "cancelled_by_user", "reduce_only",
		"position_close", "stp", "liquidated", "auto_deleveraged":
		return "canceled", true
	default:
		return "", false
	}
}

func gateOrderValues(request OrderRequest) (quantity, price string, err error) {
	quantity, err = canonicalDecimalMultiple(
		request.Quantity, request.Instrument.QuantityStep, true,
	)
	if err != nil {
		return "", "", err
	}
	if request.OrderType != "limit" {
		return quantity, "", nil
	}
	price, err = canonicalDecimalMultiple(
		request.Price, request.Instrument.PriceTick, true,
	)
	if err != nil {
		return "", "", err
	}
	return quantity, price, nil
}

func gateRequestError(raw []byte, requestErr error) (Result, error) {
	return classifyGateRequestError(raw, requestErr, false)
}

func classifyGateRequestError(raw []byte, requestErr error, query bool) (Result, error) {
	result := Result{Raw: rawMap(raw)}
	var payload struct {
		Label   string `json:"label"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if len(raw) == 0 || unmarshalJSON(raw, &payload) != nil || payload.Label == "" {
		return result, requestErr
	}
	label := strings.ToUpper(strings.TrimSpace(payload.Label))
	switch label {
	case "ORDER_NOT_FOUND":
		result.Status = "unknown"
		return result, fmt.Errorf(
			"%w: Gate code %s: %s",
			ErrOrderNotFound, payload.Label, payload.Message,
		)
	case "ORDER_CLOSED", "ORDER_CANCELLED":
		result.Status = "unknown"
		result.ErrorCode = payload.Label
		result.ErrorMessage = firstNonEmpty(payload.Message, payload.Detail)
		if query {
			return result, fmt.Errorf(
				"%w: Gate code %s: %s",
				ErrUncertain, payload.Label, result.ErrorMessage,
			)
		}
		return result, fmt.Errorf(
			"%w: Gate code %s: %s",
			ErrOrderNotFound, payload.Label, payload.Message,
		)
	}
	result.Status = "rejected"
	result.ErrorCode = payload.Label
	result.ErrorMessage = firstNonEmpty(payload.Message, payload.Detail)
	return result, fmt.Errorf(
		"%w: Gate code %s: %s", ErrRejected, payload.Label, result.ErrorMessage,
	)
}

func timeUnix() int64 {
	return timeNowUnix()
}
