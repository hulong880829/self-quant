package exchange

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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
	raw, err := a.signed(ctx, http.MethodPost, "/api/v5/trade/order", credentials, compactJSON(body), nil)
	if err != nil {
		return Result{}, err
	}
	return parseOKXOrder(raw, request.Instrument, true)
}

func (a *okxAdapter) GetOrder(ctx context.Context, credentials Credentials, request QueryRequest) (Result, error) {
	values := url.Values{"instId": {okxInstID(request.Instrument)}}
	if request.VenueOrderID != "" {
		values.Set("ordId", request.VenueOrderID)
	} else {
		values.Set("clOrdId", request.ClientOrderID)
	}
	raw, err := a.signed(ctx, http.MethodGet, "/api/v5/trade/order?"+values.Encode(), credentials, nil, nil)
	if err != nil {
		return Result{}, err
	}
	return parseOKXOrder(raw, request.Instrument, false)
}

func (a *okxAdapter) CancelOrder(ctx context.Context, credentials Credentials, request CancelRequest) (Result, error) {
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
	result, err := parseOKXOrder(raw, request.Instrument, true)
	if err != nil {
		return result, err
	}
	latest, queryErr := a.GetOrder(ctx, credentials, QueryRequest{
		Instrument: request.Instrument, ClientOrderID: request.ClientOrderID,
		VenueOrderID: firstNonEmpty(result.VenueOrderID, request.VenueOrderID),
	})
	if queryErr == nil {
		if latest.Status == "pending" || latest.Status == "open" ||
			latest.Status == "partially_filled" || latest.Status == "unknown" {
			latest.Status = "canceled"
		}
		return latest, nil
	}
	result.Status = "canceled"
	return result, nil
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
		return Result{Status: "rejected", ErrorCode: payload.Code, ErrorMessage: payload.Msg, Raw: rawMap(raw)}, ErrRejected
	}
	if len(payload.Data) == 0 {
		return Result{Status: "unknown", Raw: rawMap(raw)}, nil
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
