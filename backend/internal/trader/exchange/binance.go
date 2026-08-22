package exchange

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type binanceAdapter struct {
	client          *signedClient
	spotMarket      *signedClient
	perpetualMarket *signedClient
}

func newBinance(client *http.Client, base string) Adapter {
	tradingBase := base
	spotMarketBase := base
	perpetualMarketBase := base
	if tradingBase == "" {
		tradingBase = "https://papi.binance.com"
	}
	if base == "" || strings.Contains(strings.ToLower(base), "papi.binance.com") {
		spotMarketBase = "https://api.binance.com"
		perpetualMarketBase = "https://fapi.binance.com"
	}
	return &binanceAdapter{
		client:          newSignedClient(client, tradingBase, 80*time.Millisecond),
		spotMarket:      newSignedClient(client, spotMarketBase, 80*time.Millisecond),
		perpetualMarket: newSignedClient(client, perpetualMarketBase, 80*time.Millisecond),
	}
}

func (a *binanceAdapter) GetBBO(ctx context.Context, instrument Instrument) (BBO, error) {
	values := url.Values{"symbol": {instrument.ExchangeSymbol}}
	client, path := a.perpetualMarket, "/fapi/v1/ticker/bookTicker"
	if instrument.ContractType == "spot" {
		client, path = a.spotMarket, "/api/v3/ticker/bookTicker"
	}
	raw, err := client.do(ctx, http.MethodGet, path+"?"+values.Encode(), nil, nil, nil)
	if err != nil {
		return BBO{}, err
	}
	var payload struct {
		BidPrice string `json:"bidPrice"`
		AskPrice string `json:"askPrice"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return BBO{}, err
	}
	return bbo(payload.BidPrice, payload.AskPrice, time.Now())
}

func (a *binanceAdapter) PlaceOrder(
	ctx context.Context,
	credentials Credentials,
	request OrderRequest,
) (Result, error) {
	values := url.Values{
		"symbol":           {request.Instrument.ExchangeSymbol},
		"side":             {strings.ToUpper(request.Side)},
		"type":             {strings.ToUpper(request.OrderType)},
		"quantity":         {request.Quantity},
		"newClientOrderId": {request.ClientOrderID},
		"newOrderRespType": {"RESULT"},
	}
	if request.OrderType == "limit" {
		values.Set("price", request.Price)
		switch orderTimeInForce(request) {
		case "post_only":
			if request.Instrument.ContractType == "spot" {
				values.Set("type", "LIMIT_MAKER")
			} else {
				values.Set("timeInForce", "GTX")
			}
		case "ioc":
			values.Set("timeInForce", "IOC")
		default:
			values.Set("timeInForce", "GTC")
		}
	}
	if request.Instrument.ContractType == "spot" {
		values.Set("sideEffectType", "NO_SIDE_EFFECT")
		values.Set("autoRepayAtCancel", "false")
	}
	raw, err := a.signed(ctx, http.MethodPost, binancePath(request.Instrument.ContractType), credentials, values, nil)
	if err != nil {
		return Result{}, err
	}
	return parseBinanceOrder(raw)
}

func (a *binanceAdapter) GetOrder(
	ctx context.Context,
	credentials Credentials,
	request QueryRequest,
) (Result, error) {
	values := url.Values{"symbol": {request.Instrument.ExchangeSymbol}}
	if request.VenueOrderID != "" {
		values.Set("orderId", request.VenueOrderID)
	} else {
		values.Set("origClientOrderId", request.ClientOrderID)
	}
	raw, err := a.signed(ctx, http.MethodGet, binancePath(request.Instrument.ContractType), credentials, values, nil)
	if err != nil {
		return Result{}, err
	}
	return parseBinanceOrder(raw)
}

func (a *binanceAdapter) CancelOrder(
	ctx context.Context,
	credentials Credentials,
	request CancelRequest,
) (Result, error) {
	values := url.Values{"symbol": {request.Instrument.ExchangeSymbol}}
	if request.VenueOrderID != "" {
		values.Set("orderId", request.VenueOrderID)
	} else {
		values.Set("origClientOrderId", request.ClientOrderID)
	}
	raw, err := a.signed(ctx, http.MethodDelete, binancePath(request.Instrument.ContractType), credentials, values, nil)
	if err != nil {
		return Result{}, err
	}
	return parseBinanceOrder(raw)
}

func (a *binanceAdapter) signed(
	ctx context.Context,
	method, path string,
	credentials Credentials,
	values url.Values,
	target any,
) ([]byte, error) {
	values.Set("timestamp", nowMillis())
	values.Set("recvWindow", "5000")
	unsigned := values.Encode()
	signed := unsigned + "&signature=" + hmacHex256(credentials.APISecret, unsigned)
	urlPath := path + "?" + signed
	headers := http.Header{"X-MBX-APIKEY": {credentials.APIKey}}
	raw, err := a.client.do(ctx, method, urlPath, headers, nil, target)
	if err != nil {
		return raw, err
	}
	return raw, nil
}

func binancePath(contractType string) string {
	if contractType == "spot" {
		return "/papi/v1/margin/order"
	}
	return "/papi/v1/um/order"
}

func parseBinanceOrder(raw []byte) (Result, error) {
	var payload struct {
		OrderID     any    `json:"orderId"`
		Status      string `json:"status"`
		ExecutedQty string `json:"executedQty"`
		AvgPrice    string `json:"avgPrice"`
		Price       string `json:"price"`
		Code        int    `json:"code"`
		Msg         string `json:"msg"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return Result{}, err
	}
	if payload.Code != 0 && payload.Status == "" {
		return Result{Status: "rejected", ErrorCode: fmt.Sprintf("%d", payload.Code), ErrorMessage: payload.Msg, Raw: rawMap(raw)}, ErrRejected
	}
	filled := firstNonEmpty(payload.ExecutedQty)
	avg := firstNonEmpty(payload.AvgPrice)
	return Result{
		VenueOrderID:   fmt.Sprint(payload.OrderID),
		Status:         normalizeStatus(payload.Status),
		FilledQuantity: filled,
		AveragePrice:   avg,
		Raw:            rawMap(raw),
	}, nil
}
