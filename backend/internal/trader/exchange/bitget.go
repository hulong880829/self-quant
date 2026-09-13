package exchange

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type bitgetAdapter struct {
	client *signedClient
}

func newBitget(client *http.Client, base string) Adapter {
	if base == "" {
		base = "https://api.bitget.com"
	}
	return &bitgetAdapter{client: newSignedClient(client, base, 100*time.Millisecond)}
}

func (a *bitgetAdapter) GetBBO(ctx context.Context, instrument Instrument) (BBO, error) {
	values := url.Values{
		"category": {bitgetCategory(instrument)},
		"symbol":   {instrument.ExchangeSymbol},
	}
	raw, err := a.client.do(ctx, http.MethodGet, "/api/v3/market/tickers?"+values.Encode(), nil, nil, nil)
	if err != nil {
		return BBO{}, err
	}
	var payload struct {
		Code        string `json:"code"`
		Msg         string `json:"msg"`
		RequestTime int64  `json:"requestTime"`
		Data        []struct {
			BidPrice  string `json:"bid1Price"`
			AskPrice  string `json:"ask1Price"`
			Timestamp string `json:"ts"`
		} `json:"data"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return BBO{}, err
	}
	if payload.Code != "" && payload.Code != "00000" {
		return BBO{}, fmt.Errorf("%w: Bitget market code %s: %s", ErrRejected, payload.Code, payload.Msg)
	}
	if len(payload.Data) == 0 {
		return BBO{}, fmt.Errorf("decode venue BBO: empty Bitget ticker")
	}
	item := payload.Data[0]
	timestamp := unixTimestamp(item.Timestamp)
	if timestamp.IsZero() {
		timestamp = unixTimestamp(fmt.Sprint(payload.RequestTime))
	}
	return bbo(item.BidPrice, item.AskPrice, timestamp)
}

func (a *bitgetAdapter) GetPositionMode(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
) (string, error) {
	if instrument.ContractType == "spot" {
		return PositionModeOneWay, nil
	}
	raw, err := a.signed(
		ctx, http.MethodGet, "/api/v3/account/settings",
		credentials, nil, nil,
	)
	if err != nil {
		return "", err
	}
	var payload struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			HoldMode string `json:"holdMode"`
		} `json:"data"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return "", err
	}
	if payload.Code != "00000" {
		return "", fmt.Errorf(
			"%w: Bitget account settings code %s: %s",
			ErrRejected, payload.Code, payload.Msg,
		)
	}
	switch payload.Data.HoldMode {
	case "one_way_mode":
		return PositionModeOneWay, nil
	case "hedge_mode":
		return PositionModeHedge, nil
	default:
		return "", fmt.Errorf("unknown Bitget position mode %q", payload.Data.HoldMode)
	}
}

func (a *bitgetAdapter) PlaceOrder(ctx context.Context, credentials Credentials, request OrderRequest) (Result, error) {
	timeInForce := "gtc"
	if request.OrderType == "market" {
		timeInForce = "ioc"
	}
	switch orderTimeInForce(request) {
	case "post_only":
		timeInForce = "post_only"
	case "ioc":
		timeInForce = "ioc"
	}
	body := map[string]string{
		"category":  bitgetCategory(request.Instrument),
		"symbol":    request.Instrument.ExchangeSymbol,
		"side":      request.Side,
		"orderType": request.OrderType,
		"qty":       request.Quantity,
		"clientOid": request.ClientOrderID,
	}
	if request.OrderType == "limit" {
		body["price"] = request.Price
		body["timeInForce"] = timeInForce
	}
	if request.Instrument.ContractType != "spot" {
		body["marginMode"] = "crossed"
		if request.ReduceOnly {
			body["reduceOnly"] = "yes"
		} else {
			body["reduceOnly"] = "no"
		}
	}
	raw, err := a.signed(ctx, http.MethodPost, "/api/v3/trade/place-order", credentials, compactJSON(body), nil)
	if err != nil {
		return bitgetRequestError(raw, err, bitgetCallPlace)
	}
	return parseBitgetOrder(raw, bitgetCallPlace)
}

func (a *bitgetAdapter) GetOrder(ctx context.Context, credentials Credentials, request QueryRequest) (Result, error) {
	if err := requireOrderLookupID(request); err != nil {
		return Result{}, err
	}
	values := url.Values{"category": {bitgetCategory(request.Instrument)}}
	if request.VenueOrderID != "" {
		values.Set("orderId", request.VenueOrderID)
	} else {
		values.Set("clientOid", request.ClientOrderID)
	}
	raw, err := a.signed(ctx, http.MethodGet, "/api/v3/trade/order-info?"+values.Encode(), credentials, nil, nil)
	if err != nil {
		return unknownQueryError(bitgetRequestError(raw, err, bitgetCallQuery))
	}
	return unknownQueryError(parseBitgetOrder(raw, bitgetCallQuery))
}

func (a *bitgetAdapter) ResolveOrder(
	ctx context.Context,
	credentials Credentials,
	request QueryRequest,
) (OrderResolution, error) {
	if err := requireOrderLookupID(request); err != nil {
		return OrderResolution{}, err
	}
	return exactOrderResolution(a.GetOrder(ctx, credentials, request))
}

func (a *bitgetAdapter) CancelOrder(ctx context.Context, credentials Credentials, request CancelRequest) (Result, error) {
	accepted, err := a.sendCancelOrder(ctx, credentials, request)
	return commandOnlyCancelResult(accepted, err)
}

func (a *bitgetAdapter) CancelAndGetOrder(ctx context.Context, credentials Credentials, request CancelRequest) (Result, error) {
	accepted, err := a.sendCancelOrder(ctx, credentials, request)
	if err != nil && !errors.Is(err, ErrAmbiguousCancel) {
		return accepted, err
	}
	return getOrderAfterCancel(ctx, a.GetOrder, credentials, request, accepted, "Bitget")
}

func (a *bitgetAdapter) sendCancelOrder(ctx context.Context, credentials Credentials, request CancelRequest) (Result, error) {
	body := map[string]string{"category": bitgetCategory(request.Instrument)}
	if request.VenueOrderID != "" {
		body["orderId"] = request.VenueOrderID
	} else {
		body["clientOid"] = request.ClientOrderID
	}
	raw, err := a.signed(ctx, http.MethodPost, "/api/v3/trade/cancel-order", credentials, compactJSON(body), nil)
	if err != nil {
		return bitgetRequestError(raw, err, bitgetCallCancel)
	}
	return parseBitgetOrder(raw, bitgetCallCancel)
}

func (a *bitgetAdapter) signed(
	ctx context.Context,
	method, path string,
	credentials Credentials,
	body []byte,
	target any,
) ([]byte, error) {
	return a.signedPaths(ctx, method, path, path, credentials, body, target)
}

func (a *bitgetAdapter) signedPaths(
	ctx context.Context,
	method, requestPath, signPath string,
	credentials Credentials,
	body []byte,
	target any,
) ([]byte, error) {
	ts := nowMillis()
	signPayload := ts + method + signPath + string(body)
	headers := http.Header{
		"ACCESS-KEY":        {credentials.APIKey},
		"ACCESS-PASSPHRASE": {credentials.Passphrase},
		"ACCESS-TIMESTAMP":  {ts},
		"ACCESS-SIGN":       {hmacBase64(credentials.APISecret, signPayload)},
		"locale":            {"en-US"},
	}
	return a.client.do(ctx, method, requestPath, headers, body, target)
}

func bitgetSigningQuery(values url.Values) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var buf strings.Builder
	for _, key := range keys {
		for _, value := range values[key] {
			if buf.Len() > 0 {
				buf.WriteByte('&')
			}
			buf.WriteString(key)
			buf.WriteByte('=')
			buf.WriteString(value)
		}
	}
	return buf.String()
}

func bitgetCategory(instrument Instrument) string {
	if instrument.ContractType == "spot" {
		return "SPOT"
	}
	if strings.EqualFold(instrument.SettleAsset, "USDC") || strings.EqualFold(instrument.QuoteAsset, "USDC") {
		return "USDC-FUTURES"
	}
	return "USDT-FUTURES"
}

type bitgetCall string

const (
	bitgetCallPlace  bitgetCall = "place"
	bitgetCallQuery  bitgetCall = "query"
	bitgetCallCancel bitgetCall = "cancel"
)

func parseBitgetOrder(raw []byte, call bitgetCall) (Result, error) {
	var payload struct {
		Code, Msg string
		Data      struct {
			OrderID     string `json:"orderId"`
			ClientOid   string `json:"clientOid"`
			OrderStatus string `json:"orderStatus"`
			Status      string `json:"status"`
			CumExecQty  string `json:"cumExecQty"`
			BaseVolume  string `json:"baseVolume"`
			AvgPrice    string `json:"avgPrice"`
			PriceAvg    string `json:"priceAvg"`
		} `json:"data"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return Result{}, err
	}
	if payload.Code != "" && payload.Code != "00000" {
		result := Result{
			Status: "rejected", ErrorCode: payload.Code,
			ErrorMessage: payload.Msg, Raw: rawMap(raw),
		}
		switch payload.Code {
		case "25204":
			result.Status = "unknown"
			switch call {
			case bitgetCallQuery:
				return result, fmt.Errorf(
					"%w: Bitget code %s: %s", ErrOrderNotFound, payload.Code, payload.Msg,
				)
			case bitgetCallPlace:
				return result, fmt.Errorf(
					"%w: Bitget code %s: %s", ErrUncertain, payload.Code, payload.Msg,
				)
			default:
				return result, ErrAmbiguousCancel
			}
		case "40010", "40725", "45001":
			result.Status = "unknown"
			return result, ErrUncertain
		default:
			if call == bitgetCallQuery {
				result.Status = "unknown"
			}
			return result, ErrRejected
		}
	}
	status := normalizeStatus(firstNonEmpty(payload.Data.OrderStatus, payload.Data.Status))
	if status == "unknown" && payload.Data.OrderID != "" && call == bitgetCallPlace {
		status = "pending"
	}
	return Result{
		VenueOrderID: payload.Data.OrderID, Status: status,
		FilledQuantity: firstNonEmpty(payload.Data.CumExecQty, payload.Data.BaseVolume),
		AveragePrice:   firstNonEmpty(payload.Data.AvgPrice, payload.Data.PriceAvg),
		Raw:            rawMap(raw),
	}, nil
}

func bitgetRequestError(raw []byte, requestErr error, call bitgetCall) (Result, error) {
	result := Result{Raw: rawMap(raw)}
	if len(raw) == 0 {
		if call == bitgetCallQuery {
			return unknownQueryError(result, requestErr)
		}
		return result, requestErr
	}
	parsed, parseErr := parseBitgetOrder(raw, call)
	if parseErr == nil || errors.Is(parseErr, ErrRejected) ||
		errors.Is(parseErr, ErrRateLimited) || errors.Is(parseErr, ErrUncertain) ||
		errors.Is(parseErr, ErrAmbiguousCancel) || errors.Is(parseErr, ErrOrderNotFound) {
		if call == bitgetCallQuery {
			return unknownQueryError(parsed, parseErr)
		}
		return parsed, parseErr
	}
	if call == bitgetCallQuery {
		return unknownQueryError(result, requestErr)
	}
	return result, requestErr
}
