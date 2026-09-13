package exchange

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type bybitAdapter struct {
	client *signedClient
}

func newBybit(client *http.Client, base string) Adapter {
	if base == "" {
		base = "https://api.bybit.com"
	}
	return &bybitAdapter{client: newSignedClient(client, base, 80*time.Millisecond)}
}

func (a *bybitAdapter) GetBBO(ctx context.Context, instrument Instrument) (BBO, error) {
	values := url.Values{
		"category": {bybitCategory(instrument)},
		"symbol":   {instrument.ExchangeSymbol},
	}
	raw, err := a.client.do(ctx, http.MethodGet, "/v5/market/tickers?"+values.Encode(), nil, nil, nil)
	if err != nil {
		return BBO{}, err
	}
	var payload struct {
		RetCode int    `json:"retCode"`
		RetMsg  string `json:"retMsg"`
		Time    int64  `json:"time"`
		Result  struct {
			List []struct {
				BidPrice string `json:"bid1Price"`
				AskPrice string `json:"ask1Price"`
			} `json:"list"`
		} `json:"result"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return BBO{}, err
	}
	if payload.RetCode != 0 {
		return BBO{}, fmt.Errorf("%w: Bybit market code %d: %s", ErrRejected, payload.RetCode, payload.RetMsg)
	}
	if len(payload.Result.List) == 0 {
		return BBO{}, fmt.Errorf("decode venue BBO: empty Bybit ticker")
	}
	item := payload.Result.List[0]
	return bbo(item.BidPrice, item.AskPrice, unixTimestamp(fmt.Sprint(payload.Time)))
}

func (a *bybitAdapter) GetPositionMode(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
) (string, error) {
	if instrument.ContractType == "spot" {
		return PositionModeOneWay, nil
	}
	values := url.Values{
		"category": {bybitCategory(instrument)},
		"symbol":   {instrument.ExchangeSymbol},
	}
	raw, err := a.signed(
		ctx, http.MethodGet, "/v5/position/list?"+values.Encode(),
		credentials, nil, nil,
	)
	if err != nil {
		return "", err
	}
	var payload struct {
		RetCode int    `json:"retCode"`
		RetMsg  string `json:"retMsg"`
		Result  struct {
			List []struct {
				PositionIndex int `json:"positionIdx"`
			} `json:"list"`
		} `json:"result"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return "", err
	}
	if payload.RetCode != 0 || len(payload.Result.List) == 0 {
		return "", fmt.Errorf(
			"%w: Bybit position query code %d: %s",
			ErrRejected, payload.RetCode, payload.RetMsg,
		)
	}
	for _, position := range payload.Result.List {
		if position.PositionIndex != 0 {
			return PositionModeHedge, nil
		}
	}
	return PositionModeOneWay, nil
}

func (a *bybitAdapter) PlaceOrder(ctx context.Context, credentials Credentials, request OrderRequest) (Result, error) {
	timeInForce := "GTC"
	switch orderTimeInForce(request) {
	case "post_only":
		timeInForce = "PostOnly"
	case "ioc":
		timeInForce = "IOC"
	}
	body := map[string]any{
		"category":    bybitCategory(request.Instrument),
		"symbol":      request.Instrument.ExchangeSymbol,
		"side":        bybitSide(request.Side),
		"orderType":   bybitOrderType(request.OrderType),
		"qty":         request.Quantity,
		"orderLinkId": request.ClientOrderID,
		"timeInForce": timeInForce,
	}
	if request.OrderType == "limit" {
		body["price"] = request.Price
	}
	if request.Instrument.ContractType == "spot" && request.OrderType == "market" {
		body["marketUnit"] = "baseCoin"
	}
	if request.Instrument.ContractType != "spot" {
		body["positionIdx"] = 0
		if request.ReduceOnly {
			body["reduceOnly"] = true
		}
	}
	raw, err := a.signed(ctx, http.MethodPost, "/v5/order/create", credentials, compactJSON(body), nil)
	if err != nil {
		return bybitRequestError(raw, err)
	}
	return parseBybitOrder(raw, true)
}

func (a *bybitAdapter) GetOrder(ctx context.Context, credentials Credentials, request QueryRequest) (Result, error) {
	if err := requireOrderLookupID(request); err != nil {
		return Result{}, err
	}
	result, err := a.queryBybitOrder(ctx, credentials, request, "/v5/order/realtime")
	if err != nil {
		return unknownQueryError(result, err)
	}
	if resolvedVenueResult(result) {
		return result, nil
	}
	return Result{Status: "unknown", Raw: result.Raw}, fmt.Errorf(
		"%w: Bybit realtime returned no matching order",
		ErrOrderNotFound,
	)
}

func (a *bybitAdapter) ResolveOrder(
	ctx context.Context,
	credentials Credentials,
	request QueryRequest,
) (OrderResolution, error) {
	if err := requireOrderLookupID(request); err != nil {
		return OrderResolution{}, err
	}
	return exactOrderResolution(a.queryBybitOrder(
		ctx, credentials, request, "/v5/order/history",
	))
}

func (a *bybitAdapter) queryBybitOrder(
	ctx context.Context,
	credentials Credentials,
	request QueryRequest,
	path string,
) (Result, error) {
	values := url.Values{"category": {bybitCategory(request.Instrument)}}
	if request.Instrument.ExchangeSymbol != "" {
		values.Set("symbol", request.Instrument.ExchangeSymbol)
	}
	if request.VenueOrderID != "" {
		values.Set("orderId", request.VenueOrderID)
	} else {
		values.Set("orderLinkId", request.ClientOrderID)
	}
	raw, requestErr := a.signed(
		ctx, http.MethodGet, path+"?"+values.Encode(), credentials, nil, nil,
	)
	if requestErr != nil {
		return bybitRequestError(raw, requestErr)
	}
	return parseBybitOrder(raw, false)
}

func (a *bybitAdapter) CancelOrder(ctx context.Context, credentials Credentials, request CancelRequest) (Result, error) {
	accepted, err := a.sendCancelOrder(ctx, credentials, request)
	return commandOnlyCancelResult(accepted, err)
}

func (a *bybitAdapter) CancelAndGetOrder(ctx context.Context, credentials Credentials, request CancelRequest) (Result, error) {
	accepted, err := a.sendCancelOrder(ctx, credentials, request)
	if err != nil {
		return accepted, err
	}
	return getOrderAfterCancel(ctx, a.GetOrder, credentials, request, accepted, "Bybit")
}

func (a *bybitAdapter) sendCancelOrder(ctx context.Context, credentials Credentials, request CancelRequest) (Result, error) {
	body := map[string]any{
		"category": bybitCategory(request.Instrument),
		"symbol":   request.Instrument.ExchangeSymbol,
	}
	if request.VenueOrderID != "" {
		body["orderId"] = request.VenueOrderID
	} else {
		body["orderLinkId"] = request.ClientOrderID
	}
	raw, err := a.signed(ctx, http.MethodPost, "/v5/order/cancel", credentials, compactJSON(body), nil)
	if err != nil {
		return bybitRequestError(raw, err)
	}
	return parseBybitOrder(raw, true)
}

func (a *bybitAdapter) signed(
	ctx context.Context,
	method, path string,
	credentials Credentials,
	body []byte,
	target any,
) ([]byte, error) {
	ts, window := nowMillis(), "5000"
	query := ""
	if parts := strings.SplitN(path, "?", 2); len(parts) == 2 {
		query = parts[1]
	}
	payload := ts + credentials.APIKey + window + query + string(body)
	headers := http.Header{
		"X-BAPI-API-KEY":     {credentials.APIKey},
		"X-BAPI-TIMESTAMP":   {ts},
		"X-BAPI-RECV-WINDOW": {window},
		"X-BAPI-SIGN":        {hmacHex256(credentials.APISecret, payload)},
	}
	return a.client.do(ctx, method, path, headers, body, target)
}

func bybitCategory(instrument Instrument) string {
	if instrument.ContractType == "spot" {
		return "spot"
	}
	return "linear"
}

func bybitSide(side string) string {
	if side == "sell" {
		return "Sell"
	}
	return "Buy"
}

func bybitOrderType(orderType string) string {
	if orderType == "market" {
		return "Market"
	}
	return "Limit"
}

func parseBybitOrder(raw []byte, place bool) (Result, error) {
	var payload struct {
		RetCode int    `json:"retCode"`
		RetMsg  string `json:"retMsg"`
		Result  struct {
			OrderID, OrderLinkID, OrderStatus, CumExecQty, AvgPrice string
			RejectReason, CancelType                                string
			List                                                    []struct {
				OrderID, OrderStatus, CumExecQty, AvgPrice string
				RejectReason, CancelType                   string
			} `json:"list"`
		} `json:"result"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return Result{}, err
	}
	if payload.RetCode != 0 {
		result := Result{
			Status: "rejected", ErrorCode: fmt.Sprintf("%d", payload.RetCode),
			ErrorMessage: payload.RetMsg, Raw: rawMap(raw),
		}
		switch payload.RetCode {
		case 10006:
			result.Status = "unknown"
			return result, ErrRateLimited
		case 10016, 110001:
			result.Status = "unknown"
			return result, ErrUncertain
		default:
			return result, ErrRejected
		}
	}
	orderID := payload.Result.OrderID
	status := payload.Result.OrderStatus
	filled := payload.Result.CumExecQty
	avg := payload.Result.AvgPrice
	rejectReason := payload.Result.RejectReason
	cancelType := payload.Result.CancelType
	if len(payload.Result.List) > 0 {
		item := payload.Result.List[0]
		orderID = firstNonEmpty(item.OrderID, orderID)
		status = firstNonEmpty(item.OrderStatus, status)
		filled = firstNonEmpty(item.CumExecQty, filled)
		avg = firstNonEmpty(item.AvgPrice, avg)
		rejectReason = firstNonEmpty(item.RejectReason, rejectReason)
		cancelType = firstNonEmpty(item.CancelType, cancelType)
	}
	normalized := normalizeStatus(status)
	if strings.EqualFold(status, "PartiallyFilledCanceled") {
		normalized = "canceled"
	}
	if normalized == "unknown" && orderID != "" && place {
		normalized = "pending"
	}
	errorCode := bybitOrderErrorCode(rejectReason)
	errorMessage := ""
	if errorCode != "" && !bybitBenignOrderMetadata(cancelType) {
		errorMessage = strings.TrimSpace(cancelType)
	}
	return Result{
		VenueOrderID: orderID, Status: normalized,
		FilledQuantity: filled, AveragePrice: avg, Raw: rawMap(raw),
		ErrorCode: errorCode, ErrorMessage: errorMessage,
	}, nil
}

func bybitOrderErrorCode(value string) string {
	value = strings.TrimSpace(value)
	if bybitBenignOrderMetadata(value) {
		return ""
	}
	return value
}

func bybitBenignOrderMetadata(value string) bool {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "", "EC_NOERROR", "UNKNOWN", "CANCEL_BY_UNKNOWN", "NOTAVAILABLE",
		"CANCELBYUNKNOWN", "CANCEL_BY_USER", "CANCELBYUSER":
		return true
	default:
		return false
	}
}

func bybitRequestError(raw []byte, requestErr error) (Result, error) {
	result := Result{Raw: rawMap(raw)}
	if len(raw) == 0 {
		return result, requestErr
	}
	parsed, parseErr := parseBybitOrder(raw, true)
	if parseErr == nil || errors.Is(parseErr, ErrRejected) ||
		errors.Is(parseErr, ErrRateLimited) || errors.Is(parseErr, ErrUncertain) {
		return parsed, parseErr
	}
	return result, requestErr
}
