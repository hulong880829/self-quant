package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const asterDefaultURL = "https://fapi.asterdex.com"

type asterAdapter struct {
	client  *signedClient
	now     func() time.Time
	circuit *venueCircuit

	timeMu     sync.Mutex
	timeOffset time.Duration
	timeSynced time.Time
	locks      sync.Map
}

func newAster(client *http.Client, base string) Adapter {
	if strings.TrimSpace(base) == "" {
		base = asterDefaultURL
	}
	return &asterAdapter{
		client: newSignedClient(client, strings.TrimRight(base, "/"), 100*time.Millisecond),
		now:    time.Now, circuit: newVenueCircuit(),
	}
}

func (a *asterAdapter) Capabilities(context.Context, Credentials) (Capabilities, error) {
	return Capabilities{
		Products: []string{"perpetual"}, QuoteAssets: []string{"USDT"},
		TimeInForce: []string{"GTC", "IOC", "POST_ONLY"}, PostOnly: true,
		ReduceOnly: true, MakerTwap: true, PrivateOrderStream: true, OneWayOnly: true,
		Arbitrage: true,
	}, nil
}

func (*asterAdapter) VenueClientOrderID(value string) string {
	return asterClientOrderID(value)
}

func (a *asterAdapter) GetBBO(ctx context.Context, instrument Instrument) (BBO, error) {
	if err := requirePerpetual(instrument); err != nil {
		return BBO{}, err
	}
	values := url.Values{"symbol": {instrument.ExchangeSymbol}}
	raw, err := a.client.do(ctx, http.MethodGet, "/fapi/v1/ticker/bookTicker?"+values.Encode(), nil, nil, nil)
	if err != nil {
		return BBO{}, err
	}
	var payload struct {
		BidPrice string `json:"bidPrice"`
		AskPrice string `json:"askPrice"`
		Time     int64  `json:"time"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return BBO{}, err
	}
	at := a.now()
	if payload.Time > 0 {
		at = time.UnixMilli(payload.Time)
	}
	return bbo(payload.BidPrice, payload.AskPrice, at)
}

func (a *asterAdapter) GetPositionMode(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
) (string, error) {
	if err := requirePerpetual(instrument); err != nil {
		return "", err
	}
	if asterUsesAPIWallet(credentials) {
		return PositionModeOneWay, nil
	}
	raw, err := a.signed(ctx, http.MethodGet, "/fapi/v1/positionSide/dual", credentials, nil)
	if err != nil {
		return "", err
	}
	var payload struct {
		DualSidePosition bool `json:"dualSidePosition"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return "", err
	}
	if payload.DualSidePosition {
		return PositionModeHedge, nil
	}
	return PositionModeOneWay, nil
}

func (a *asterAdapter) PlaceOrder(
	ctx context.Context,
	credentials Credentials,
	request OrderRequest,
) (result Result, resultErr error) {
	if err := a.circuit.before(credentials, a.now()); err != nil {
		return Result{}, err
	}
	defer func() { a.circuit.record(credentials, resultErr, a.now()) }()
	if err := requirePerpetual(request.Instrument); err != nil {
		return Result{}, err
	}
	if err := validateVenueOrderRules(request); err != nil {
		return Result{}, err
	}
	mode, err := a.GetPositionMode(ctx, credentials, request.Instrument)
	if err != nil {
		return Result{}, err
	}
	if mode != PositionModeOneWay {
		return Result{}, fmt.Errorf("%w: Aster hedge mode is not supported", ErrRejected)
	}
	clientOrderID := asterClientOrderID(request.ClientOrderID)
	unlock := a.lock(credentials, clientOrderID)
	defer unlock()
	existing, lookupErr := a.GetOrder(ctx, credentials, QueryRequest{
		Instrument: request.Instrument, ClientOrderID: request.ClientOrderID,
	})
	if lookupErr == nil {
		return existing, nil
	}
	if !errorsIsOrderNotFound(lookupErr) {
		return Result{}, fmt.Errorf(
			"%w: preflight Aster client order lookup: %v",
			ErrUncertain, lookupErr,
		)
	}
	values := url.Values{
		"symbol":           {request.Instrument.ExchangeSymbol},
		"side":             {strings.ToUpper(request.Side)},
		"type":             {strings.ToUpper(request.OrderType)},
		"quantity":         {request.Quantity},
		"newClientOrderId": {clientOrderID},
		"newOrderRespType": {"RESULT"},
	}
	if request.OrderType == "limit" {
		values.Set("price", request.Price)
		switch orderTimeInForce(request) {
		case "post_only":
			values.Set("timeInForce", "GTX")
		case "ioc":
			values.Set("timeInForce", "IOC")
		default:
			values.Set("timeInForce", "GTC")
		}
	}
	if request.ReduceOnly {
		values.Set("reduceOnly", "true")
	}
	raw, err := a.signed(ctx, http.MethodPost, asterOrderPath(credentials), credentials, values)
	if err != nil {
		result, resultErr = classifyAsterPlaceOrderError(asterRequestError(raw, err))
		result.Reference.ClientOrderID = values.Get("newClientOrderId")
		result.Reference.EventAt = a.now()
		return result, resultErr
	}
	result, err = parseAsterOrder(raw)
	result.Reference.ClientOrderID = values.Get("newClientOrderId")
	return result, err
}

func (a *asterAdapter) GetOrder(
	ctx context.Context,
	credentials Credentials,
	request QueryRequest,
) (Result, error) {
	values := url.Values{"symbol": {request.Instrument.ExchangeSymbol}}
	if request.VenueOrderID != "" {
		values.Set("orderId", request.VenueOrderID)
	} else {
		values.Set("origClientOrderId", asterClientOrderID(request.ClientOrderID))
	}
	raw, err := a.signed(ctx, http.MethodGet, asterOrderPath(credentials), credentials, values)
	if err != nil {
		return unknownQueryError(asterRequestError(raw, err))
	}
	return unknownQueryError(parseAsterOrder(raw))
}

func (a *asterAdapter) CancelOrder(
	ctx context.Context,
	credentials Credentials,
	request CancelRequest,
) (Result, error) {
	accepted, err := a.sendCancelOrder(ctx, credentials, request)
	return commandOnlyCancelResult(accepted, err)
}

func (a *asterAdapter) CancelAndGetOrder(
	ctx context.Context,
	credentials Credentials,
	request CancelRequest,
) (Result, error) {
	accepted, err := a.sendCancelOrder(ctx, credentials, request)
	if err != nil {
		return accepted, err
	}
	return getOrderAfterCancel(ctx, a.GetOrder, credentials, request, accepted, "Aster")
}

func (a *asterAdapter) sendCancelOrder(
	ctx context.Context,
	credentials Credentials,
	request CancelRequest,
) (Result, error) {
	values := url.Values{"symbol": {request.Instrument.ExchangeSymbol}}
	if request.VenueOrderID != "" {
		values.Set("orderId", request.VenueOrderID)
	} else {
		values.Set("origClientOrderId", asterClientOrderID(request.ClientOrderID))
	}
	raw, err := a.signed(ctx, http.MethodDelete, asterOrderPath(credentials), credentials, values)
	if err != nil {
		return asterRequestError(raw, err)
	}
	return parseAsterOrder(raw)
}

func (a *asterAdapter) ResolveOrder(
	ctx context.Context,
	credentials Credentials,
	request QueryRequest,
) (OrderResolution, error) {
	if err := requireOrderLookupID(request); err != nil {
		return OrderResolution{}, err
	}
	return exactOrderResolution(a.GetOrder(ctx, credentials, request))
}

func (a *asterAdapter) ListFills(
	ctx context.Context,
	credentials Credentials,
	instrument Instrument,
	since time.Time,
) ([]Fill, error) {
	values := url.Values{"symbol": {instrument.ExchangeSymbol}, "limit": {"1000"}}
	if !since.IsZero() {
		values.Set("startTime", strconv.FormatInt(since.UnixMilli(), 10))
	}
	raw, err := a.signed(ctx, http.MethodGet, asterTradesPath(credentials), credentials, values)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID      flexString `json:"id"`
		OrderID flexString `json:"orderId"`
		Qty     string     `json:"qty"`
		Price   string     `json:"price"`
		Time    int64      `json:"time"`
	}
	if err := unmarshalJSON(raw, &rows); err != nil {
		return nil, err
	}
	result := make([]Fill, 0, len(rows))
	for _, row := range rows {
		result = append(result, Fill{
			TradeID: row.ID.String(), VenueOrderID: row.OrderID.String(),
			Quantity: row.Qty, Price: row.Price, ExecutedAt: time.UnixMilli(row.Time),
		})
	}
	return result, nil
}

func (a *asterAdapter) ListPositions(ctx context.Context, credentials Credentials) ([]Position, error) {
	raw, err := a.signed(ctx, http.MethodGet, asterPositionsPath(credentials), credentials, nil)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Symbol       string `json:"symbol"`
		PositionAmt  string `json:"positionAmt"`
		EntryPrice   string `json:"entryPrice"`
		PositionSide string `json:"positionSide"`
	}
	if err := unmarshalJSON(raw, &rows); err != nil {
		return nil, err
	}
	result := make([]Position, 0, len(rows))
	for _, row := range rows {
		if row.PositionAmt != "0" && row.PositionAmt != "0.0" {
			result = append(result, Position{
				Instrument: row.Symbol, Quantity: row.PositionAmt, EntryPrice: row.EntryPrice,
			})
		}
	}
	return result, nil
}

func (a *asterAdapter) ListBalances(ctx context.Context, credentials Credentials) ([]Balance, error) {
	raw, err := a.signed(ctx, http.MethodGet, asterBalancePath(credentials), credentials, nil)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Asset            string `json:"asset"`
		Balance          string `json:"balance"`
		AvailableBalance string `json:"availableBalance"`
	}
	if err := unmarshalJSON(raw, &rows); err != nil {
		return nil, err
	}
	result := make([]Balance, 0, len(rows))
	for _, row := range rows {
		result = append(result, Balance{Asset: row.Asset, Total: row.Balance, Available: row.AvailableBalance})
	}
	return result, nil
}

func (a *asterAdapter) Health(ctx context.Context, credentials Credentials) Health {
	if a.circuit.open(credentials, a.now()) {
		return Health{Healthy: false, CheckedAt: a.now(), Message: "new order circuit open"}
	}
	_, err := a.client.do(ctx, http.MethodGet, "/fapi/v1/ping", nil, nil, nil)
	return Health{Healthy: err == nil, CheckedAt: a.now(), Message: sanitizeVenueHealth(err)}
}

func (a *asterAdapter) signed(
	ctx context.Context,
	method, path string,
	credentials Credentials,
	values url.Values,
) ([]byte, error) {
	if asterUsesAPIWallet(credentials) {
		return a.signedV3(ctx, method, path, credentials, values)
	}
	if strings.TrimSpace(credentials.APIKey) == "" || strings.TrimSpace(credentials.APISecret) == "" {
		return nil, fmt.Errorf("%w: Aster API key and secret are required", ErrRejected)
	}
	if values == nil {
		values = url.Values{}
	}
	timestamp := a.now().Add(a.currentTimeOffset()).UnixMilli()
	values.Set("timestamp", strconv.FormatInt(timestamp, 10))
	values.Set("recvWindow", "5000")
	values.Set("signature", hmacHex256(credentials.APISecret, values.Encode()))
	headers := http.Header{"X-MBX-APIKEY": {credentials.APIKey}}
	var body []byte
	requestPath := path
	if method == http.MethodGet || method == http.MethodDelete {
		requestPath += "?" + values.Encode()
	} else {
		body = []byte(values.Encode())
		headers.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	raw, err := a.client.do(ctx, method, requestPath, headers, body, nil)
	if err != nil && strings.Contains(strings.ToLower(string(raw)), "timestamp") {
		if syncErr := a.syncTime(ctx); syncErr == nil {
			return a.signed(ctx, method, path, credentials, valuesWithoutAuth(values))
		}
	}
	return raw, err
}

func (a *asterAdapter) currentTimeOffset() time.Duration {
	a.timeMu.Lock()
	defer a.timeMu.Unlock()
	if a.now().Sub(a.timeSynced) > 30*time.Minute {
		return 0
	}
	return a.timeOffset
}

func (a *asterAdapter) syncTime(ctx context.Context) error {
	raw, err := a.client.do(ctx, http.MethodGet, "/fapi/v1/time", nil, nil, nil)
	if err != nil {
		return err
	}
	var payload struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return err
	}
	a.timeMu.Lock()
	a.timeOffset = time.UnixMilli(payload.ServerTime).Sub(a.now())
	a.timeSynced = a.now()
	a.timeMu.Unlock()
	return nil
}

func parseAsterOrder(raw []byte) (Result, error) {
	var payload struct {
		OrderID       flexString `json:"orderId"`
		ClientOrderID string     `json:"clientOrderId"`
		Status        string     `json:"status"`
		ExecutedQty   string     `json:"executedQty"`
		AveragePrice  string     `json:"avgPrice"`
		CumQuote      string     `json:"cumQuote"`
		Code          int        `json:"code"`
		Message       string     `json:"msg"`
		UpdateTime    int64      `json:"updateTime"`
	}
	if err := unmarshalJSON(raw, &payload); err != nil {
		return Result{}, err
	}
	if payload.Code != 0 {
		return Result{ErrorCode: strconv.Itoa(payload.Code), ErrorMessage: payload.Message, Raw: rawMap(raw)},
			fmt.Errorf("%w: Aster code %d: %s", ErrRejected, payload.Code, payload.Message)
	}
	average := payload.AveragePrice
	if average == "" || average == "0" {
		average = quotientDecimal(payload.CumQuote, payload.ExecutedQty)
	}
	result := Result{
		VenueOrderID: payload.OrderID.String(), Status: normalizeStatus(payload.Status),
		FilledQuantity: payload.ExecutedQty, AveragePrice: average, Raw: rawMap(raw),
		Reference: VenueReference{
			ClientOrderID: payload.ClientOrderID, VenueOrderID: payload.OrderID.String(),
			EventAt: time.UnixMilli(payload.UpdateTime), ReconcileStatus: normalizeStatus(payload.Status),
		},
	}
	return result, nil
}

func asterRequestError(raw []byte, err error) (Result, error) {
	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(raw, &payload)
	result := Result{
		Status: "unknown", ErrorCode: strconv.Itoa(payload.Code),
		ErrorMessage: payload.Msg, Raw: rawMap(raw),
	}
	if payload.Code == -2013 || payload.Code == -2011 {
		return result, fmt.Errorf("%w: Aster code %d: %s", ErrOrderNotFound, payload.Code, payload.Msg)
	}
	return result, err
}

func classifyAsterPlaceOrderError(result Result, err error) (Result, error) {
	if result.ErrorCode == "-5018" && errors.Is(err, ErrRejected) {
		result.Status = "rejected"
		result.FilledQuantity = "0"
	}
	return result, err
}

func asterClientOrderID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 36 {
		return value
	}
	return "sq" + stableHex(value, 32)
}

func (a *asterAdapter) lock(credentials Credentials, clientOrderID string) func() {
	key := strings.ToLower(strings.TrimSpace(credentials.APIKey)) + ":" + clientOrderID
	value, _ := a.locks.LoadOrStore(key, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

func valuesWithoutAuth(values url.Values) url.Values {
	copy := make(url.Values, len(values))
	for key, items := range values {
		if key == "timestamp" || key == "recvWindow" || key == "signature" {
			continue
		}
		copy[key] = append([]string(nil), items...)
	}
	return copy
}
