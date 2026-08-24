package exchange

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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

func (a *gateAdapter) PlaceOrder(ctx context.Context, credentials Credentials, request OrderRequest) (Result, error) {
	if request.Instrument.ContractType == "spot" {
		body := map[string]any{
			"currency_pair": gateSpotSymbol(request.Instrument),
			"type":          request.OrderType,
			"account":       "unified",
			"side":          request.Side,
			"amount":        request.Quantity,
			"text":          gateClientText(request.ClientOrderID),
		}
		if request.OrderType == "limit" {
			body["price"] = request.Price
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
			return Result{}, err
		}
		return parseGateSpot(raw)
	}
	wireQuantity, err := ToVenueQuantity(request.Instrument, request.Quantity)
	if err != nil {
		return Result{}, err
	}
	size, err := strconv.ParseInt(wireQuantity, 10, 64)
	if err != nil || size <= 0 {
		return Result{}, fmt.Errorf("%w: invalid gate contract quantity", ErrRejected)
	}
	if request.Side == "sell" {
		size = -size
	}
	body := map[string]any{
		"contract": gateFuturesSymbol(request.Instrument),
		"size":     size,
		"text":     gateClientText(request.ClientOrderID),
	}
	if request.OrderType == "limit" {
		body["price"] = request.Price
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
	raw, err := a.signed(ctx, http.MethodPost, "/api/v4/futures/usdt/orders", credentials, compactJSON(body), nil)
	if err != nil {
		return Result{}, err
	}
	return parseGateFutures(raw, request.Instrument)
}

func (a *gateAdapter) GetOrder(ctx context.Context, credentials Credentials, request QueryRequest) (Result, error) {
	if request.Instrument.ContractType == "spot" {
		path := "/api/v4/spot/orders/" + url.PathEscape(gateLookupID(request.VenueOrderID, request.ClientOrderID)) +
			"?currency_pair=" + url.QueryEscape(gateSpotSymbol(request.Instrument))
		raw, err := a.signed(ctx, http.MethodGet, path, credentials, nil, nil)
		if err != nil {
			return Result{}, err
		}
		return parseGateSpot(raw)
	}
	path := "/api/v4/futures/usdt/orders/" + url.PathEscape(gateLookupID(request.VenueOrderID, request.ClientOrderID))
	raw, err := a.signed(ctx, http.MethodGet, path, credentials, nil, nil)
	if err != nil {
		return Result{}, err
	}
	return parseGateFutures(raw, request.Instrument)
}

func (a *gateAdapter) CancelOrder(ctx context.Context, credentials Credentials, request CancelRequest) (Result, error) {
	if request.Instrument.ContractType == "spot" {
		path := "/api/v4/spot/orders/" + url.PathEscape(gateLookupID(request.VenueOrderID, request.ClientOrderID)) +
			"?currency_pair=" + url.QueryEscape(gateSpotSymbol(request.Instrument))
		raw, err := a.signed(ctx, http.MethodDelete, path, credentials, nil, nil)
		if err != nil {
			return Result{}, err
		}
		return parseGateSpot(raw)
	}
	path := "/api/v4/futures/usdt/orders/" + url.PathEscape(gateLookupID(request.VenueOrderID, request.ClientOrderID))
	raw, err := a.signed(ctx, http.MethodDelete, path, credentials, nil, nil)
	if err != nil {
		return Result{}, err
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
	id = strings.TrimPrefix(id, "t-")
	if len(id) > 16 {
		id = id[:16]
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
		ID, Status, FilledTotal, AvgDealPrice, Left string
		Label, Message                              string
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return Result{}, err
	}
	if payload.Label != "" && payload.ID == "" {
		return Result{Status: "rejected", ErrorCode: payload.Label, ErrorMessage: payload.Message, Raw: rawMap(raw)}, ErrRejected
	}
	return Result{
		VenueOrderID: payload.ID, Status: normalizeStatus(payload.Status),
		FilledQuantity: payload.FilledTotal, AveragePrice: payload.AvgDealPrice, Raw: rawMap(raw),
	}, nil
}

func parseGateFutures(raw []byte, instrument Instrument) (Result, error) {
	var payload struct {
		ID                          any `json:"id"`
		Status, FinishAs, FillPrice string
		Size, Left                  flexInt64
		Label, Message              string
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return Result{}, err
	}
	if payload.Label != "" && payload.ID == nil {
		return Result{Status: "rejected", ErrorCode: payload.Label, ErrorMessage: payload.Message, Raw: rawMap(raw)}, ErrRejected
	}
	size, left := payload.Size.Int64(), payload.Left.Int64()
	filled := size - left
	if filled < 0 {
		filled = -filled
	}
	filledQuantity, err := FromVenueQuantity(instrument, strconv.FormatInt(filled, 10))
	if err != nil {
		return Result{}, err
	}
	status := gateFuturesStatus(payload.Status, payload.FinishAs, size, left)
	return Result{
		VenueOrderID: fmt.Sprint(payload.ID), Status: status,
		FilledQuantity: filledQuantity, AveragePrice: payload.FillPrice,
		Raw: rawMap(raw),
	}, nil
}

func gateFuturesStatus(status, finishAs string, size, left int64) string {
	normalized := normalizeStatus(firstNonEmpty(finishAs, status))
	if normalized != "unknown" {
		return normalized
	}
	filled := size - left
	if filled < 0 {
		filled = -filled
	}
	total := size
	if total < 0 {
		total = -total
	}
	if filled > 0 && filled >= total {
		return "filled"
	}
	if filled > 0 {
		return "partially_filled"
	}
	switch strings.ToLower(strings.TrimSpace(finishAs)) {
	case "ioc", "cancelled", "cancelled_by_user", "reduce_only":
		return "canceled"
	default:
		return normalized
	}
}

func timeUnix() int64 {
	return timeNowUnix()
}
