package exchange

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"
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

func (a *binanceAdapter) GetPositionMode(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
) (string, error) {
	if instrument.ContractType == "spot" {
		return PositionModeOneWay, nil
	}
	raw, err := a.signed(
		ctx, http.MethodGet, "/papi/v1/um/positionSide/dual",
		credentials, url.Values{}, nil,
	)
	if err != nil {
		return "", err
	}
	var payload struct {
		DualSidePosition bool   `json:"dualSidePosition"`
		Code             int    `json:"code"`
		Message          string `json:"msg"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return "", err
	}
	if payload.Code != 0 {
		return "", fmt.Errorf("%w: Binance code %d: %s", ErrRejected, payload.Code, payload.Message)
	}
	if payload.DualSidePosition {
		return PositionModeHedge, nil
	}
	return PositionModeOneWay, nil
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
	} else if request.ReduceOnly {
		values.Set("reduceOnly", "true")
	}
	raw, err := a.signed(ctx, http.MethodPost, binancePath(request.Instrument.ContractType), credentials, values, nil)
	if err != nil {
		result, reqErr := binanceRequestError(raw, err)
		if binanceUnknownOrder(result) {
			return a.queryAfterUnknownOrder(ctx, credentials, QueryRequest{
				Instrument:    request.Instrument,
				ClientOrderID: request.ClientOrderID,
			})
		}
		return result, reqErr
	}
	parsed, parseErr := parseBinanceOrder(raw)
	if binanceUnknownOrder(parsed) {
		return a.queryAfterUnknownOrder(ctx, credentials, QueryRequest{
			Instrument:    request.Instrument,
			ClientOrderID: request.ClientOrderID,
		})
	}
	return parsed, parseErr
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
		return unknownQueryError(binanceRequestError(raw, err))
	}
	return unknownQueryError(parseBinanceOrder(raw))
}

func (a *binanceAdapter) ResolveOrder(
	ctx context.Context,
	credentials Credentials,
	request QueryRequest,
) (OrderResolution, error) {
	if err := requireOrderLookupID(request); err != nil {
		return OrderResolution{}, err
	}
	return exactOrderResolution(a.GetOrder(ctx, credentials, request))
}

func (a *binanceAdapter) CancelOrder(
	ctx context.Context,
	credentials Credentials,
	request CancelRequest,
) (Result, error) {
	accepted, err := a.sendCancelOrder(ctx, credentials, request)
	if binanceUnknownOrder(accepted) {
		if !errors.Is(err, ErrUncertain) {
			err = fmt.Errorf("%w: Binance code -2011: %s", ErrUncertain, accepted.ErrorMessage)
		}
		return commandOnlyCancelResult(accepted, err)
	}
	if err != nil {
		return commandOnlyCancelResult(accepted, err)
	}
	if trustedBinanceCancelTerminal(accepted) {
		accepted.LocalCommandAck = false
		return accepted, nil
	}
	return commandOnlyCancelResult(accepted, nil)
}

func (a *binanceAdapter) CancelAndGetOrder(
	ctx context.Context,
	credentials Credentials,
	request CancelRequest,
) (Result, error) {
	accepted, err := a.sendCancelOrder(ctx, credentials, request)
	if binanceUnknownOrder(accepted) {
		return a.queryAfterUnknownOrder(ctx, credentials, QueryRequest{
			Instrument:    request.Instrument,
			ClientOrderID: request.ClientOrderID,
			VenueOrderID:  request.VenueOrderID,
		})
	}
	if err != nil {
		return accepted, err
	}
	return getOrderAfterCancel(ctx, a.GetOrder, credentials, request, accepted, "Binance")
}

func (a *binanceAdapter) sendCancelOrder(
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
		return binanceRequestError(raw, err)
	}
	return parseBinanceOrder(raw)
}

func trustedBinanceCancelTerminal(result Result) bool {
	if !trustedCancelTerminal(result) {
		return false
	}
	filled := strings.TrimSpace(result.FilledQuantity)
	avg := strings.TrimSpace(result.AveragePrice)
	if avg != "" && avg != "0" {
		return true
	}
	quantity, err := decimal.NewFromString(filled)
	return err == nil && quantity.IsZero()
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
		OrderID            flexString `json:"orderId"`
		Status             string     `json:"status"`
		ExecutedQty        string     `json:"executedQty"`
		CumulativeQuoteQty string     `json:"cummulativeQuoteQty"`
		AvgPrice           string     `json:"avgPrice"`
		Price              string     `json:"price"`
		Code               int        `json:"code"`
		Msg                string     `json:"msg"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return Result{}, err
	}
	if payload.Code != 0 && payload.Status == "" {
		return Result{Status: "rejected", ErrorCode: fmt.Sprintf("%d", payload.Code), ErrorMessage: payload.Msg, Raw: rawMap(raw)}, ErrRejected
	}
	filled := firstNonEmpty(payload.ExecutedQty)
	avg := firstNonEmpty(payload.AvgPrice)
	if (avg == "" || avg == "0") && filled != "" && payload.CumulativeQuoteQty != "" {
		executed, quantityErr := decimal.NewFromString(filled)
		quote, quoteErr := decimal.NewFromString(payload.CumulativeQuoteQty)
		if quantityErr == nil && quoteErr == nil && executed.IsPositive() && !quote.IsNegative() {
			avg = quote.Div(executed).String()
		}
	}
	return Result{
		VenueOrderID:   payload.OrderID.String(),
		Status:         normalizeStatus(payload.Status),
		FilledQuantity: filled,
		AveragePrice:   avg,
		Raw:            rawMap(raw),
	}, nil
}

func (a *binanceAdapter) queryAfterUnknownOrder(
	ctx context.Context,
	credentials Credentials,
	request QueryRequest,
) (Result, error) {
	latest, queryErr := a.GetOrder(ctx, credentials, request)
	if queryErr == nil && terminalOrderStatus(latest.Status) {
		return latest, nil
	}
	if queryErr == nil {
		return latest, fmt.Errorf(
			"%w: Binance -2011 then order remains %s", ErrUncertain, latest.Status,
		)
	}
	return Result{
		Status: "unknown", ErrorCode: "-2011", ErrorMessage: latest.ErrorMessage,
		Raw: latest.Raw,
	}, fmt.Errorf("%w: Binance -2011 query failed: %v", ErrUncertain, queryErr)
}

func binanceUnknownOrder(result Result) bool {
	return strings.TrimSpace(result.ErrorCode) == "-2011"
}

func binanceRequestError(raw []byte, requestErr error) (Result, error) {
	result := Result{Raw: rawMap(raw)}
	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if len(raw) == 0 || unmarshalJSON(raw, &payload) != nil || payload.Code == 0 {
		return result, requestErr
	}
	result.Status = "rejected"
	result.ErrorCode = fmt.Sprintf("%d", payload.Code)
	result.ErrorMessage = payload.Msg
	switch payload.Code {
	case -2013:
		result.Status = "unknown"
		return result, fmt.Errorf(
			"%w: Binance code %d: %s", ErrOrderNotFound, payload.Code, payload.Msg,
		)
	case -2011, -1001, -1007:
		result.Status = "unknown"
		return result, fmt.Errorf(
			"%w: Binance code %d: %s", ErrUncertain, payload.Code, payload.Msg,
		)
	case -1008, -1015:
		result.Status = "unknown"
		return result, fmt.Errorf(
			"%w: Binance code %d: %s", ErrRateLimited, payload.Code, payload.Msg,
		)
	}
	return result, fmt.Errorf(
		"%w: Binance code %d: %s", ErrRejected, payload.Code, payload.Msg,
	)
}
