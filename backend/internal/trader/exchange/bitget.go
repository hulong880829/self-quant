package exchange

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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
		"category":    bitgetCategory(request.Instrument),
		"symbol":      request.Instrument.ExchangeSymbol,
		"side":        request.Side,
		"orderType":   request.OrderType,
		"qty":         request.Quantity,
		"clientOid":   request.ClientOrderID,
		"timeInForce": timeInForce,
	}
	if request.OrderType == "limit" {
		body["price"] = request.Price
	}
	if request.Instrument.ContractType != "spot" {
		if request.Side == "sell" {
			body["posSide"] = "short"
		} else {
			body["posSide"] = "long"
		}
	}
	raw, err := a.signed(ctx, http.MethodPost, "/api/v3/trade/place-order", credentials, compactJSON(body), nil)
	if err != nil {
		return Result{}, err
	}
	return parseBitgetOrder(raw, true)
}

func (a *bitgetAdapter) GetOrder(ctx context.Context, credentials Credentials, request QueryRequest) (Result, error) {
	values := url.Values{"category": {bitgetCategory(request.Instrument)}}
	if request.VenueOrderID != "" {
		values.Set("orderId", request.VenueOrderID)
	} else {
		values.Set("clientOid", request.ClientOrderID)
	}
	raw, err := a.signed(ctx, http.MethodGet, "/api/v3/trade/order-info?"+values.Encode(), credentials, nil, nil)
	if err != nil {
		return Result{}, err
	}
	return parseBitgetOrder(raw, false)
}

func (a *bitgetAdapter) CancelOrder(ctx context.Context, credentials Credentials, request CancelRequest) (Result, error) {
	body := map[string]string{"category": bitgetCategory(request.Instrument)}
	if request.VenueOrderID != "" {
		body["orderId"] = request.VenueOrderID
	} else {
		body["clientOid"] = request.ClientOrderID
	}
	raw, err := a.signed(ctx, http.MethodPost, "/api/v3/trade/cancel-order", credentials, compactJSON(body), nil)
	if err != nil {
		return Result{}, err
	}
	result, err := parseBitgetOrder(raw, true)
	if err == nil && result.Status == "unknown" {
		result.Status = "canceled"
	}
	return result, err
}

func (a *bitgetAdapter) signed(
	ctx context.Context,
	method, path string,
	credentials Credentials,
	body []byte,
	target any,
) ([]byte, error) {
	ts := nowMillis()
	signPayload := ts + method + path + string(body)
	headers := http.Header{
		"ACCESS-KEY":        {credentials.APIKey},
		"ACCESS-PASSPHRASE": {credentials.Passphrase},
		"ACCESS-TIMESTAMP":  {ts},
		"ACCESS-SIGN":       {hmacBase64(credentials.APISecret, signPayload)},
		"locale":            {"en-US"},
	}
	return a.client.do(ctx, method, path, headers, body, target)
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

func parseBitgetOrder(raw []byte, place bool) (Result, error) {
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
		return Result{Status: "rejected", ErrorCode: payload.Code, ErrorMessage: payload.Msg, Raw: rawMap(raw)}, ErrRejected
	}
	status := normalizeStatus(firstNonEmpty(payload.Data.OrderStatus, payload.Data.Status))
	if status == "unknown" && payload.Data.OrderID != "" && place {
		status = "pending"
	}
	return Result{
		VenueOrderID: payload.Data.OrderID, Status: status,
		FilledQuantity: firstNonEmpty(payload.Data.CumExecQty, payload.Data.BaseVolume),
		AveragePrice:   firstNonEmpty(payload.Data.AvgPrice, payload.Data.PriceAvg),
		Raw:            rawMap(raw),
	}, nil
}
