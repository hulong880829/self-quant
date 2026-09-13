package exchange

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type okxAdapter struct {
	client *signedClient
}

func newOKX(client *http.Client, base string) Adapter {
	if base == "" {
		base = "https://www.okx.com"
	}
	return &okxAdapter{client: newSignedClient(client, base, 80*time.Millisecond)}
}

func (a *okxAdapter) GetBBO(ctx context.Context, instrument Instrument) (BBO, error) {
	values := url.Values{"instId": {okxInstID(instrument)}}
	raw, err := a.client.do(ctx, http.MethodGet, "/api/v5/market/ticker?"+values.Encode(), nil, nil, nil)
	if err != nil {
		return BBO{}, err
	}
	var payload struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			BidPrice  string `json:"bidPx"`
			AskPrice  string `json:"askPx"`
			Timestamp string `json:"ts"`
		} `json:"data"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return BBO{}, err
	}
	if payload.Code != "" && payload.Code != "0" {
		return BBO{}, fmt.Errorf("%w: OKX market code %s: %s", ErrRejected, payload.Code, payload.Msg)
	}
	if len(payload.Data) == 0 {
		return BBO{}, fmt.Errorf("decode venue BBO: empty OKX ticker")
	}
	item := payload.Data[0]
	return bbo(item.BidPrice, item.AskPrice, unixTimestamp(item.Timestamp))
}

func (a *okxAdapter) GetPositionMode(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
) (string, error) {
	if instrument.ContractType == "spot" {
		return PositionModeOneWay, nil
	}
	raw, err := a.signed(
		ctx, http.MethodGet, "/api/v5/account/config", credentials, nil, nil,
	)
	if err != nil {
		return "", err
	}
	var payload struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			PositionMode string `json:"posMode"`
		} `json:"data"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return "", err
	}
	if payload.Code != "0" || len(payload.Data) == 0 {
		return "", fmt.Errorf(
			"%w: OKX account config code %s: %s", ErrRejected, payload.Code, payload.Msg,
		)
	}
	switch payload.Data[0].PositionMode {
	case "net_mode":
		return PositionModeOneWay, nil
	case "long_short_mode":
		return PositionModeHedge, nil
	default:
		return "", fmt.Errorf("unknown OKX position mode %q", payload.Data[0].PositionMode)
	}
}

func (a *okxAdapter) PlaceOrder(ctx context.Context, credentials Credentials, request OrderRequest) (Result, error) {
	quantity, err := ToVenueQuantity(request.Instrument, request.Quantity)
	if err != nil {
		return Result{}, err
	}
	body := map[string]string{
		"instId":  okxInstID(request.Instrument),
		"tdMode":  okxTradeMode(request.Instrument.ContractType),
		"clOrdId": request.ClientOrderID,
		"side":    request.Side,
		"ordType": request.OrderType,
		"sz":      quantity,
	}
	if request.OrderType == "limit" {
		body["px"] = request.Price
		switch orderTimeInForce(request) {
		case "post_only":
			body["ordType"] = "post_only"
		case "ioc":
			body["ordType"] = "ioc"
		}
	}
	if request.Instrument.ContractType != "spot" && request.ReduceOnly {
		body["reduceOnly"] = "true"
	}
	raw, err := a.signed(ctx, http.MethodPost, "/api/v5/trade/order", credentials, compactJSON(body), nil)
	if err != nil {
		return Result{}, err
	}
	return parseOKXOrder(raw, request.Instrument, true)
}

func (a *okxAdapter) GetOrder(ctx context.Context, credentials Credentials, request QueryRequest) (Result, error) {
	if err := requireOrderLookupID(request); err != nil {
		return Result{}, err
	}
	values := url.Values{"instId": {okxInstID(request.Instrument)}}
	if request.VenueOrderID != "" {
		values.Set("ordId", request.VenueOrderID)
	} else {
		values.Set("clOrdId", request.ClientOrderID)
	}
	raw, err := a.signed(ctx, http.MethodGet, "/api/v5/trade/order?"+values.Encode(), credentials, nil, nil)
	if err != nil {
		parsed, parseErr := parseOKXOrder(raw, request.Instrument, false)
		if parseErr != nil {
			return unknownQueryError(parsed, parseErr)
		}
		return unknownQueryError(Result{Raw: rawMap(raw)}, err)
	}
	return unknownQueryError(parseOKXOrder(raw, request.Instrument, false))
}

func (a *okxAdapter) ResolveOrder(
	ctx context.Context,
	credentials Credentials,
	request QueryRequest,
) (OrderResolution, error) {
	if err := requireOrderLookupID(request); err != nil {
		return OrderResolution{}, err
	}
	return exactOrderResolution(a.GetOrder(ctx, credentials, request))
}

func (a *okxAdapter) CancelOrder(ctx context.Context, credentials Credentials, request CancelRequest) (Result, error) {
	result, err := a.sendCancelOrder(ctx, credentials, request)
	if strings.TrimSpace(result.ErrorCode) == "51400" {
		result.Status = "unknown"
		result.LocalCommandAck = false
		result.VenueOrderID = firstNonEmpty(result.VenueOrderID, request.VenueOrderID)
		return result, fmt.Errorf("%w: OKX sCode 51400: %s", ErrAmbiguousCancel, result.ErrorMessage)
	}
	return commandOnlyCancelResult(result, err)
}

func (a *okxAdapter) CancelAndGetOrder(ctx context.Context, credentials Credentials, request CancelRequest) (Result, error) {
	result, err := a.sendCancelOrder(ctx, credentials, request)
	if err != nil && result.ErrorCode == "51400" {
		return a.confirmOKXCancelAfter51400(ctx, credentials, request, result)
	}
	if err != nil {
		return result, err
	}
	return getOrderAfterCancel(ctx, a.GetOrder, credentials, request, result, "OKX")
}

func (a *okxAdapter) sendCancelOrder(ctx context.Context, credentials Credentials, request CancelRequest) (Result, error) {
	body := map[string]string{"instId": okxInstID(request.Instrument)}
	if request.VenueOrderID != "" {
		body["ordId"] = request.VenueOrderID
	} else {
		body["clOrdId"] = request.ClientOrderID
	}
	raw, err := a.signed(ctx, http.MethodPost, "/api/v5/trade/cancel-order", credentials, compactJSON(body), nil)
	if err != nil {
		return Result{}, err
	}
	return parseOKXOrder(raw, request.Instrument, true)
}

func (a *okxAdapter) confirmOKXCancelAfter51400(
	ctx context.Context,
	credentials Credentials,
	request CancelRequest,
	result Result,
) (Result, error) {
	pending := Result{
		Status:       "unknown",
		VenueOrderID: firstNonEmpty(result.VenueOrderID, request.VenueOrderID),
		ErrorCode:    "51400",
		ErrorMessage: result.ErrorMessage,
		Raw:          result.Raw,
	}
	latest, queryErr := a.GetOrder(ctx, credentials, QueryRequest{
		Instrument:    request.Instrument,
		ClientOrderID: request.ClientOrderID,
		VenueOrderID:  pending.VenueOrderID,
	})
	if queryErr == nil {
		switch normalizeStatus(latest.Status) {
		case "filled", "canceled", "expired":
			return latest, nil
		case "rejected":
			return pending, fmt.Errorf(
				"%w: OKX 51400 query returned rejected", ErrAmbiguousCancel,
			)
		default:
			return latest, fmt.Errorf(
				"%w: OKX 51400 order remains %s", ErrAmbiguousCancel, latest.Status,
			)
		}
	}
	return pending, fmt.Errorf(
		"%w: OKX 51400 query failed: %v", ErrAmbiguousCancel, queryErr,
	)
}

func (a *okxAdapter) signed(
	ctx context.Context,
	method, path string,
	credentials Credentials,
	body []byte,
	target any,
) ([]byte, error) {
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	signPayload := ts + method + path + string(body)
	headers := http.Header{
		"OK-ACCESS-KEY":        {credentials.APIKey},
		"OK-ACCESS-PASSPHRASE": {credentials.Passphrase},
		"OK-ACCESS-TIMESTAMP":  {ts},
		"OK-ACCESS-SIGN":       {hmacBase64(credentials.APISecret, signPayload)},
	}
	return a.client.do(ctx, method, path, headers, body, target)
}

func okxInstID(instrument Instrument) string {
	if wire := metadataString(instrument, "instId"); wire != "" {
		return wire
	}
	if instrument.ContractType == "perpetual" {
		return instrument.BaseAsset + "-" + instrument.QuoteAsset + "-SWAP"
	}
	return instrument.BaseAsset + "-" + instrument.QuoteAsset
}

func okxTradeMode(contractType string) string {
	if contractType == "spot" {
		return "cash"
	}
	return "cross"
}

func parseOKXOrder(raw []byte, instrument Instrument, place bool) (Result, error) {
	var payload struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			OrdID, ClOrdID, SCode, SMsg, State, AccFillSz, AvgPx, FillSz string
		} `json:"data"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return Result{}, err
	}
	if payload.Code != "0" {
		sCode, sMsg := "", ""
		if len(payload.Data) > 0 {
			sCode = payload.Data[0].SCode
			sMsg = payload.Data[0].SMsg
		}
		errorCode := firstNonEmpty(sCode, payload.Code)
		errorMessage := firstNonEmpty(sMsg, payload.Msg)
		result := Result{
			ErrorCode: errorCode, ErrorMessage: errorMessage, Raw: rawMap(raw),
		}
		if !place && okxOrderMissing(payload.Code, sCode) {
			result.Status = "unknown"
			return result, fmt.Errorf(
				"%w: OKX code %s: %s", ErrOrderNotFound, errorCode, errorMessage,
			)
		}
		if place {
			result.Status = "rejected"
		} else {
			result.Status = "unknown"
		}
		return result, ErrRejected
	}
	if len(payload.Data) == 0 {
		if place {
			return Result{Status: "unknown", Raw: rawMap(raw)}, nil
		}
		return Result{Status: "unknown", Raw: rawMap(raw)}, fmt.Errorf(
			"%w: OKX order query returned empty data", ErrUncertain,
		)
	}
	item := payload.Data[0]
	if place && item.SCode != "" && item.SCode != "0" {
		return Result{Status: "rejected", ErrorCode: item.SCode, ErrorMessage: item.SMsg, Raw: rawMap(raw)}, ErrRejected
	}
	status := normalizeStatus(item.State)
	if status == "unknown" && item.OrdID != "" && place {
		status = "pending"
	}
	filled := firstNonEmpty(item.AccFillSz, item.FillSz)
	if filled != "" {
		converted, conversionErr := FromVenueQuantity(instrument, filled)
		if conversionErr != nil {
			return Result{}, conversionErr
		}
		filled = converted
	}
	return Result{
		VenueOrderID:   item.OrdID,
		Status:         status,
		FilledQuantity: filled,
		AveragePrice:   item.AvgPx,
		Raw:            rawMap(raw),
	}, nil
}

func okxOrderMissing(code, sCode string) bool {
	return code == "51603" || sCode == "51603"
}
