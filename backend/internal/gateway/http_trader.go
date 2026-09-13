package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	traderv1 "selfquant/backend/gen/trader/v1"
)

func (h *Handler) listTraderInstruments(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	accountID, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(request, "accountId")), 10, 64)
	if err != nil || accountID <= 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid trading account id"})
		return
	}
	contractType := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("type")))
	if contractType == "" {
		contractType = "perpetual"
	}
	ctx, cancel := context.WithTimeout(request.Context(), 8*time.Second)
	defer cancel()
	response, err := h.trader.ListInstruments(ctx, &traderv1.ListInstrumentsRequest{
		Token: token, TradingAccountId: accountID, ContractType: contractType,
	})
	if err != nil {
		h.writeTraderError(writer, err)
		return
	}
	data := make([]map[string]any, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		data = append(data, traderInstrumentJSON(item))
	}
	writer.Header().Set("Cache-Control", "no-store")
	capabilities := response.GetCapabilities()
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": data, "meta": map[string]any{"total": len(data)},
		"serverTime": protoTimeJSON(response.GetServerTime()),
		"capabilities": map[string]any{
			"products": capabilities.GetProducts(), "quoteAssets": capabilities.GetQuoteAssets(),
			"timeInForce": capabilities.GetTimeInForce(), "postOnly": capabilities.GetPostOnly(),
			"reduceOnly": capabilities.GetReduceOnly(), "makerTwap": capabilities.GetMakerTwap(),
			"privateOrderStream": capabilities.GetPrivateOrderStream(),
			"oneWayOnly":         capabilities.GetOneWayOnly(),
		},
	})
}

func (h *Handler) applyTraderAccountProfile(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	accountID, err := strconv.ParseInt(strings.TrimSpace(chi.URLParam(request, "accountId")), 10, 64)
	if err != nil || accountID <= 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid trading account id"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	response, err := h.trader.ApplyAccountProfile(
		ctx,
		&traderv1.ApplyAccountProfileRequest{
			Token: token, TradingAccountId: accountID,
		},
	)
	if err != nil {
		h.writeTraderError(writer, err)
		return
	}
	result := response.GetResult()
	steps := make([]map[string]any, 0, len(result.GetSteps()))
	for _, step := range result.GetSteps() {
		steps = append(steps, map[string]any{
			"step": step.GetStep(), "status": step.GetStatus(),
			"code": step.GetCode(), "message": step.GetMessage(),
		})
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": map[string]any{
			"tradingAccountId": result.GetTradingAccountId(),
			"productName":      result.GetProductName(),
			"accountName":      result.GetAccountName(),
			"exchange":         result.GetExchange(),
			"overallStatus":    result.GetOverallStatus(),
			"steps":            steps,
		},
	})
}

type placeTraderOrderBody struct {
	TradingAccountID int64  `json:"tradingAccountId"`
	InstrumentID     int64  `json:"instrumentId"`
	Side             string `json:"side"`
	OrderType        string `json:"orderType"`
	Quantity         string `json:"quantity"`
	Price            string `json:"price"`
	IdempotencyKey   string `json:"idempotencyKey"`
}

func (h *Handler) placeTraderOrder(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	var body placeTraderOrderBody
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid order payload"})
		return
	}
	key := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if key == "" {
		key = strings.TrimSpace(body.IdempotencyKey)
	}
	if key == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "idempotency key is required"})
		return
	}
	if _, err := uuid.Parse(key); err != nil && len(key) < 8 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid idempotency key"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	response, err := h.trader.PlaceOrder(ctx, &traderv1.PlaceOrderRequest{
		Token: token, TradingAccountId: body.TradingAccountID, InstrumentId: body.InstrumentID,
		Side: body.Side, OrderType: body.OrderType, Quantity: body.Quantity,
		Price: body.Price, IdempotencyKey: key,
	})
	if err != nil {
		h.writeTraderError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusAccepted, map[string]any{"data": traderOrderJSON(response.GetOrder())})
}

func (h *Handler) listTraderOrders(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	accountID, err := strconv.ParseInt(strings.TrimSpace(request.URL.Query().Get("accountId")), 10, 64)
	if err != nil || accountID <= 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid trading account id"})
		return
	}
	limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
	view := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("view")))
	if view == "" {
		view = "open"
	}
	cursor := strings.TrimSpace(request.URL.Query().Get("cursor"))
	ctx, cancel := context.WithTimeout(request.Context(), 8*time.Second)
	defer cancel()
	response, err := h.trader.ListOrders(ctx, &traderv1.ListOrdersRequest{
		Token: token, TradingAccountId: accountID, Limit: int32(limit),
		View: view, Cursor: cursor,
	})
	if err != nil {
		h.writeTraderError(writer, err)
		return
	}
	data := make([]map[string]any, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		data = append(data, traderOrderJSON(item))
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{
		"data":       data,
		"meta":       map[string]any{"nextCursor": response.GetNextCursor()},
		"serverTime": protoTimeJSON(response.GetServerTime()),
	})
}

func (h *Handler) getTraderOrder(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 8*time.Second)
	defer cancel()
	response, err := h.trader.GetOrder(ctx, &traderv1.GetOrderRequest{
		Token: token, OrderId: chi.URLParam(request, "id"),
	})
	if err != nil {
		h.writeTraderError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{"data": traderOrderJSON(response.GetOrder())})
}

func (h *Handler) cancelTraderOrder(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	response, err := h.trader.CancelOrder(ctx, &traderv1.CancelOrderRequest{
		Token: token, OrderId: chi.URLParam(request, "id"),
	})
	if err != nil {
		h.writeTraderError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{"data": traderOrderJSON(response.GetOrder())})
}

type createTraderTwapBody struct {
	TradingAccountID    int64  `json:"tradingAccountId"`
	InstrumentID        int64  `json:"instrumentId"`
	Side                string `json:"side"`
	TotalQuantity       string `json:"totalQuantity"`
	StartAt             string `json:"startAt"`
	EndAt               string `json:"endAt"`
	IntervalSeconds     int32  `json:"intervalSeconds"`
	LimitPrice          string `json:"limitPrice"`
	MaxQuantity         string `json:"maxQuantity"`
	ExecutionType       string `json:"executionType"`
	OrderTimeoutSeconds int32  `json:"orderTimeoutSeconds"`
}

func (h *Handler) createTraderTwap(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	key := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if key == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "idempotency key is required"})
		return
	}
	var body createTraderTwapBody
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid twap payload"})
		return
	}
	startAt, startErr := time.Parse(time.RFC3339Nano, body.StartAt)
	endAt, endErr := time.Parse(time.RFC3339Nano, body.EndAt)
	if startErr != nil || endErr != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid execution window"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 12*time.Second)
	defer cancel()
	response, err := h.trader.CreateTwap(ctx, &traderv1.CreateTwapRequest{
		Token: token, TradingAccountId: body.TradingAccountID, InstrumentId: body.InstrumentID,
		Side: body.Side, TotalQuantity: body.TotalQuantity,
		StartAt: timestamppb.New(startAt), EndAt: timestamppb.New(endAt),
		IntervalSeconds: body.IntervalSeconds, LimitPrice: body.LimitPrice,
		MaxQuantity: body.MaxQuantity, ExecutionType: body.ExecutionType,
		OrderTimeoutSeconds: body.OrderTimeoutSeconds, IdempotencyKey: key,
	})
	if err != nil {
		h.writeTraderError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusCreated, map[string]any{"data": traderTwapJSON(response.GetJob())})
}

func (h *Handler) listTraderTwaps(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	accountID := int64(0)
	if raw := strings.TrimSpace(request.URL.Query().Get("accountId")); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid trading account id"})
			return
		}
		accountID = value
	}
	limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
	ctx, cancel := context.WithTimeout(request.Context(), 8*time.Second)
	defer cancel()
	response, err := h.trader.ListTwaps(ctx, &traderv1.ListTwapsRequest{
		Token: token, TradingAccountId: accountID,
		View: request.URL.Query().Get("view"), Status: request.URL.Query().Get("status"),
		Limit: int32(limit), Cursor: request.URL.Query().Get("cursor"),
	})
	if err != nil {
		h.writeTraderError(writer, err)
		return
	}
	data := make([]map[string]any, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		data = append(data, traderTwapJSON(item))
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": data, "meta": map[string]any{"nextCursor": response.GetNextCursor()},
		"serverTime": protoTimeJSON(response.GetServerTime()),
	})
}

func (h *Handler) getTraderTwap(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 8*time.Second)
	defer cancel()
	response, err := h.trader.GetTwap(ctx, &traderv1.GetTwapRequest{
		Token: token, TwapId: chi.URLParam(request, "id"),
	})
	if err != nil {
		h.writeTraderError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": traderTwapJSON(response.GetJob())})
}

func (h *Handler) listTraderTwapOrders(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 8*time.Second)
	defer cancel()
	response, err := h.trader.ListTwapOrders(ctx, &traderv1.ListTwapOrdersRequest{
		Token: token, TwapId: chi.URLParam(request, "id"),
	})
	if err != nil {
		h.writeTraderError(writer, err)
		return
	}
	data := make([]map[string]any, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		data = append(data, traderOrderJSON(item))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": data})
}

func (h *Handler) cancelTraderTwap(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	response, err := h.trader.CancelTwap(ctx, &traderv1.CancelTwapRequest{
		Token: token, TwapId: chi.URLParam(request, "id"),
	})
	if err != nil {
		h.writeTraderError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": traderTwapJSON(response.GetJob())})
}

type createArbitrageCombinationBody struct {
	ProductName                       string `json:"productName"`
	LegATradingAccountID              int64  `json:"legAAccountId"`
	LegAInstrumentID                  int64  `json:"legAInstrumentId"`
	LegBTradingAccountID              int64  `json:"legBAccountId"`
	LegBInstrumentID                  int64  `json:"legBInstrumentId"`
	AskThresholdBps                   string `json:"askThresholdBps"`
	BidThresholdBps                   string `json:"bidThresholdBps"`
	TargetNotional                    string `json:"targetNotional"`
	ExecutionMode                     string `json:"executionMode"`
	MakerLeg                          string `json:"preferredLeg"`
	RunMode                           string `json:"runMode"`
	EntryDirection                    string `json:"entryDirection"`
	LegALeverage                      string `json:"legALeverage"`
	LegBLeverage                      string `json:"legBLeverage"`
	ExitPolicy                        string `json:"exitPolicy"`
	ExitAnnualizedRate                string `json:"exitAnnualizedRate"`
	ExitAfterSeconds                  int    `json:"exitAfterSeconds"`
	EarlyExitFunding8hAnnualizedFloor string `json:"earlyExitFunding8hAnnualizedFloor"`
}

func (h *Handler) createArbitrageCombination(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	key := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if key == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "idempotency key is required"})
		return
	}
	var body createArbitrageCombinationBody
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid arbitrage combination payload"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	response, err := h.trader.CreateArbitrageCombination(ctx, &traderv1.CreateArbitrageCombinationRequest{
		Token: token, IdempotencyKey: key,
		LegATradingAccountId: body.LegATradingAccountID,
		LegAInstrumentId:     body.LegAInstrumentID,
		LegBTradingAccountId: body.LegBTradingAccountID,
		LegBInstrumentId:     body.LegBInstrumentID,
		AskThresholdBps:      body.AskThresholdBps, BidThresholdBps: body.BidThresholdBps,
		TargetNotional:                     body.TargetNotional,
		ExecutionMode:                      body.ExecutionMode,
		MakerLeg:                           body.MakerLeg,
		RunMode:                            body.RunMode,
		EntryDirection:                     body.EntryDirection,
		LegALeverage:                       body.LegALeverage,
		LegBLeverage:                       body.LegBLeverage,
		ExitPolicy:                         body.ExitPolicy,
		ExitAnnualizedRate:                 body.ExitAnnualizedRate,
		ExitAfterSeconds:                   int32(body.ExitAfterSeconds),
		EarlyExitFunding_8HAnnualizedFloor: body.EarlyExitFunding8hAnnualizedFloor,
	})
	if err != nil {
		h.writeTraderError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusCreated, map[string]any{
		"data": traderArbitrageCombinationJSON(response.GetCombination()),
	})
}

type updateArbitrageCombinationBody struct {
	AskThresholdBps *string `json:"askThresholdBps"`
	BidThresholdBps *string `json:"bidThresholdBps"`
	TargetNotional  *string `json:"targetNotional"`
}

func (h *Handler) updateArbitrageCombination(
	writer http.ResponseWriter,
	request *http.Request,
) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	var body updateArbitrageCombinationBody
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid arbitrage combination update"})
		return
	}
	if body.AskThresholdBps == nil && body.BidThresholdBps == nil &&
		body.TargetNotional == nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "at least one arbitrage parameter is required"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 8*time.Second)
	defer cancel()
	response, err := h.trader.UpdateArbitrageCombination(
		ctx,
		&traderv1.UpdateArbitrageCombinationRequest{
			Token: token, CombinationId: chi.URLParam(request, "id"),
			AskThresholdBps: body.AskThresholdBps,
			BidThresholdBps: body.BidThresholdBps,
			TargetNotional:  body.TargetNotional,
		},
	)
	if err != nil {
		h.writeTraderError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": traderArbitrageCombinationJSON(response.GetCombination()),
	})
}

func (h *Handler) listArbitrageCombinations(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
	ctx, cancel := context.WithTimeout(request.Context(), 8*time.Second)
	defer cancel()
	response, err := h.trader.ListArbitrageCombinations(ctx, &traderv1.ListArbitrageCombinationsRequest{
		Token: token, View: request.URL.Query().Get("view"), Limit: int32(limit),
		Cursor: request.URL.Query().Get("cursor"),
	})
	if err != nil {
		h.writeTraderError(writer, err)
		return
	}
	data := make([]map[string]any, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		data = append(data, traderArbitrageCombinationJSON(item))
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": data, "meta": map[string]any{
			"nextCursor": response.GetNextCursor(), "total": response.GetTotal(),
		},
		"serverTime": protoTimeJSON(response.GetServerTime()),
	})
}

func (h *Handler) getArbitrageCombination(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 8*time.Second)
	defer cancel()
	response, err := h.trader.GetArbitrageCombination(ctx, &traderv1.GetArbitrageCombinationRequest{
		Token: token, CombinationId: chi.URLParam(request, "id"),
	})
	if err != nil {
		h.writeTraderError(writer, err)
		return
	}
	orders := make([]map[string]any, 0, len(response.GetOrders()))
	for _, order := range response.GetOrders() {
		orders = append(orders, traderOrderJSON(order))
	}
	data := traderArbitrageCombinationJSON(response.GetCombination())
	data["orders"] = orders
	executions := make([]map[string]any, 0, len(response.GetRecentExecutions()))
	for _, execution := range response.GetRecentExecutions() {
		executions = append(executions, map[string]any{
			"id": execution.GetId(), "direction": execution.GetDirection(),
			"status": execution.GetStatus(), "triggerSpreadBps": execution.GetTriggerSpreadBps(),
			"targetBaseQuantity": execution.GetTargetBaseQuantity(),
			"filledBaseQuantity": execution.GetFilledBaseQuantity(),
			"deltaNotional":      execution.GetDeltaNotional(), "errorMessage": execution.GetErrorMessage(),
			"createdAt":         protoTimeJSON(execution.GetCreatedAt()),
			"updatedAt":         protoTimeJSON(execution.GetUpdatedAt()),
			"requestedNotional": execution.GetRequestedNotional(),
			"positionEffect":    execution.GetPositionEffect(),
			"reduceOnly":        execution.GetReduceOnly(),
		})
	}
	events := make([]map[string]any, 0, len(response.GetRecentEvents()))
	for _, event := range response.GetRecentEvents() {
		events = append(events, map[string]any{
			"id": event.GetId(), "type": event.GetType(), "message": event.GetMessage(),
			"createdAt": protoTimeJSON(event.GetCreatedAt()),
		})
	}
	data["recentExecutions"] = executions
	data["recentEvents"] = events
	writeJSON(writer, http.StatusOK, map[string]any{"data": data})
}

func (h *Handler) closeArbitrageCombination(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	response, err := h.trader.CloseArbitrageCombination(ctx, &traderv1.CloseArbitrageCombinationRequest{
		Token: token, CombinationId: chi.URLParam(request, "id"),
	})
	if err != nil {
		h.writeTraderError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{
		"data": traderArbitrageCombinationJSON(response.GetCombination()),
	})
}

func traderInstrumentJSON(item *traderv1.Instrument) map[string]any {
	return map[string]any{
		"id": item.GetId(), "exchange": item.GetExchange(), "contractType": item.GetContractType(),
		"exchangeSymbol": item.GetExchangeSymbol(), "baseAsset": item.GetBaseAsset(),
		"quoteAsset": item.GetQuoteAsset(), "settleAsset": item.GetSettleAsset(),
		"contractSize": item.GetContractSize(), "priceTick": item.GetPriceTick(),
		"quantityStep": item.GetQuantityStep(),
	}
}

func traderOrderJSON(item *traderv1.Order) map[string]any {
	return map[string]any{
		"id": item.GetId(), "idempotencyKey": item.GetIdempotencyKey(),
		"tradingAccountId": item.GetTradingAccountId(), "productName": item.GetProductName(),
		"exchange": item.GetExchange(), "instrumentId": item.GetInstrumentId(),
		"contractType": item.GetContractType(), "exchangeSymbol": item.GetExchangeSymbol(),
		"baseAsset": item.GetBaseAsset(), "quoteAsset": item.GetQuoteAsset(),
		"clientOrderId": item.GetClientOrderId(), "venueOrderId": item.GetVenueOrderId(),
		"side": item.GetSide(), "orderType": item.GetOrderType(), "quantity": item.GetQuantity(),
		"price": item.GetPrice(), "filledQuantity": item.GetFilledQuantity(),
		"averagePrice": item.GetAveragePrice(), "status": item.GetStatus(),
		"errorCode": item.GetErrorCode(), "errorMessage": item.GetErrorMessage(),
		"createdAt": protoTimeJSON(item.GetCreatedAt()), "updatedAt": protoTimeJSON(item.GetUpdatedAt()),
		"lastReconciledAt": protoTimeJSON(item.GetLastReconciledAt()), "syncState": item.GetSyncState(),
		"twapJobId": item.GetTwapJobId(), "twapSliceIndex": item.GetTwapSliceIndex(),
		"twapAttemptIndex":     item.GetTwapAttemptIndex(),
		"arbitrageExecutionId": item.GetArbitrageExecutionId(),
		"arbitrageLeg":         item.GetArbitrageLeg(), "arbitrageRole": item.GetArbitrageRole(),
	}
}

func traderTwapJSON(item *traderv1.TwapJob) map[string]any {
	return map[string]any{
		"id": item.GetId(), "idempotencyKey": item.GetIdempotencyKey(),
		"tradingAccountId": item.GetTradingAccountId(), "productName": item.GetProductName(),
		"exchange": item.GetExchange(), "instrumentId": item.GetInstrumentId(),
		"contractType": item.GetContractType(), "exchangeSymbol": item.GetExchangeSymbol(),
		"baseAsset": item.GetBaseAsset(), "quoteAsset": item.GetQuoteAsset(),
		"side": item.GetSide(), "totalQuantity": item.GetTotalQuantity(),
		"filledQuantity": item.GetFilledQuantity(), "averagePrice": item.GetAveragePrice(),
		"startAt": protoTimeJSON(item.GetStartAt()), "endAt": protoTimeJSON(item.GetEndAt()),
		"intervalSeconds": item.GetIntervalSeconds(), "limitPrice": item.GetLimitPrice(),
		"maxQuantity": item.GetMaxQuantity(), "executionType": item.GetExecutionType(),
		"orderTimeoutSeconds": item.GetOrderTimeoutSeconds(), "status": item.GetStatus(),
		"currentSlice": item.GetCurrentSlice(), "currentAttempt": item.GetCurrentAttempt(),
		"nextActionAt":  protoTimeJSON(item.GetNextActionAt()),
		"activeOrderId": item.GetActiveOrderId(), "errorMessage": item.GetErrorMessage(),
		"createdAt": protoTimeJSON(item.GetCreatedAt()), "updatedAt": protoTimeJSON(item.GetUpdatedAt()),
		"startedAt": protoTimeJSON(item.GetStartedAt()), "closedAt": protoTimeJSON(item.GetClosedAt()),
		"progress": item.GetProgress(),
	}
}

func traderArbitrageCombinationJSON(item *traderv1.ArbitrageCombination) map[string]any {
	if item == nil {
		return map[string]any{}
	}
	legJSON := func(leg *traderv1.ArbitrageLeg) map[string]any {
		if leg == nil {
			return map[string]any{}
		}
		return map[string]any{
			"tradingAccountId": leg.GetTradingAccountId(), "instrumentId": leg.GetInstrumentId(),
			"productName": leg.GetProductName(), "accountName": leg.GetAccountName(),
			"exchange": leg.GetExchange(), "contractType": leg.GetContractType(),
			"exchangeSymbol": leg.GetExchangeSymbol(), "baseAsset": leg.GetBaseAsset(),
			"quoteAsset": leg.GetQuoteAsset(),
		}
	}
	return map[string]any{
		"id": item.GetId(), "idempotencyKey": item.GetIdempotencyKey(),
		"productName": item.GetLegA().GetProductName(),
		"legA":        legJSON(item.GetLegA()), "legB": legJSON(item.GetLegB()),
		"askThresholdBps": item.GetAskThresholdBps(), "bidThresholdBps": item.GetBidThresholdBps(),
		"targetNotional": item.GetTargetNotional(),
		"executionMode":  item.GetExecutionMode(),
		"preferredLeg":   item.GetMakerLeg(), "makerLeg": item.GetMakerLeg(), "status": item.GetStatus(),
		"positionNotional":           item.GetPositionNotional(),
		"cumulativeTurnoverNotional": item.GetCumulativeTurnoverNotional(),
		"grossTurnoverNotional":      item.GetGrossTurnoverNotional(),
		"runtimeState":               item.GetRuntimeState(),
		"legABasePosition":           item.GetLegABasePosition(),
		"legBBasePosition":           item.GetLegBBasePosition(),
		"carryBaseQuantity":          item.GetCarryBaseQuantity(),
		"legAVenueBasePosition":      item.GetLegAVenueBasePosition(),
		"legBVenueBasePosition":      item.GetLegBVenueBasePosition(),
		"legAPositionDifference":     item.GetLegAPositionDifference(),
		"legBPositionDifference":     item.GetLegBPositionDifference(),
		"lastPositionReconciledAt":   protoTimeJSON(item.GetLastPositionReconciledAt()),
		"legAAverageEntryPrice":      nullableTraderDecimal(item.GetLegAAverageEntryPrice()),
		"legBAverageEntryPrice":      nullableTraderDecimal(item.GetLegBAverageEntryPrice()),
		"averageEntrySpreadBps":      nullableTraderDecimal(item.GetAverageEntrySpreadBps()),
		"legAUnrealizedPnl":          nullableTraderDecimal(item.GetLegAUnrealizedPnl()),
		"legBUnrealizedPnl":          nullableTraderDecimal(item.GetLegBUnrealizedPnl()),
		"realizedSpreadPnl":          item.GetRealizedSpreadPnl(),
		"estimatedFundingPnl":        item.GetEstimatedFundingPnl(),
		"combinedPositionAnnualized": nullableTraderDecimal(item.GetCombinedPositionAnnualized()),
		"fundingHistoryComplete":     item.GetFundingHistoryComplete(),
		"legAVenueBaselineBasePosition": nullableTraderDecimal(
			item.GetLegAVenueBaselineBasePosition(),
		),
		"legBVenueBaselineBasePosition": nullableTraderDecimal(
			item.GetLegBVenueBaselineBasePosition(),
		),
		"venueBaselineCapturedAt":  protoTimeJSON(item.GetVenueBaselineCapturedAt()),
		"legAExpectedBasePosition": nullableTraderDecimal(item.GetLegAExpectedBasePosition()),
		"legBExpectedBasePosition": nullableTraderDecimal(item.GetLegBExpectedBasePosition()),
		"legAVenueNotional":        nullableTraderDecimal(item.GetLegAVenueNotional()),
		"legBVenueNotional":        nullableTraderDecimal(item.GetLegBVenueNotional()),
		"legAVenueValuationPrice": nullableTraderDecimal(
			item.GetLegAVenueValuationPrice(),
		),
		"legBVenueValuationPrice": nullableTraderDecimal(
			item.GetLegBVenueValuationPrice(),
		),
		"legAVenueValuationAt": protoTimeJSON(item.GetLegAVenueValuationAt()),
		"legBVenueValuationAt": protoTimeJSON(item.GetLegBVenueValuationAt()),
		"consecutiveFailures":  item.GetConsecutiveFailures(),
		"nextRetryAt":          protoTimeJSON(item.GetNextRetryAt()),
		"positionUncertain":    item.GetPositionUncertain(),
		"askSpreadBps":         nullableTraderDecimal(item.GetCurrentAskSpreadBps()),
		"bidSpreadBps":         nullableTraderDecimal(item.GetCurrentBidSpreadBps()),
		"currentAskSpreadBps":  item.GetCurrentAskSpreadBps(),
		"currentBidSpreadBps":  item.GetCurrentBidSpreadBps(),
		"marketDataStale":      item.GetMarketDataStale(), "errorMessage": item.GetErrorMessage(),
		"createdAt": protoTimeJSON(item.GetCreatedAt()), "updatedAt": protoTimeJSON(item.GetUpdatedAt()),
		"closedAt":                          protoTimeJSON(item.GetClosedAt()),
		"runMode":                           item.GetRunMode(),
		"entryDirection":                    item.GetEntryDirection(),
		"legALeverage":                      item.GetLegALeverage(),
		"legBLeverage":                      item.GetLegBLeverage(),
		"exitPolicy":                        item.GetExitPolicy(),
		"exitAnnualizedRate":                item.GetExitAnnualizedRate(),
		"exitAfterSeconds":                  item.GetExitAfterSeconds(),
		"targetReachedAt":                   protoTimeJSON(item.GetTargetReachedAt()),
		"scheduledExitAt":                   protoTimeJSON(item.GetScheduledExitAt()),
		"oneShotPhase":                      item.GetOneShotPhase(),
		"earlyExitFunding8hAnnualizedFloor": item.GetEarlyExitFunding_8HAnnualizedFloor(),
	}
}

func nullableTraderDecimal(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func traderCreateFailure(err error) *traderv1.ArbitrageCreateFailure {
	for _, detail := range status.Convert(err).Details() {
		if failure, ok := detail.(*traderv1.ArbitrageCreateFailure); ok {
			return failure
		}
	}
	return nil
}

func (h *Handler) writeTraderError(writer http.ResponseWriter, err error) {
	if failure := traderCreateFailure(err); failure != nil {
		httpStatus := http.StatusUnprocessableEntity
		switch failure.GetCode() {
		case "invalid_leverage":
			httpStatus = http.StatusBadRequest
		case "insufficient_margin", "insufficient_spot_balance", "position_capacity_exceeded":
			httpStatus = http.StatusUnprocessableEntity
		case "leverage_apply_failed", "venue_unavailable":
			httpStatus = http.StatusBadGateway
		}
		if status.Code(err) == codes.DeadlineExceeded {
			httpStatus = http.StatusGatewayTimeout
		}
		body := map[string]any{
			"error":   failure.GetMessage(),
			"code":    failure.GetCode(),
			"leg":     failure.GetLeg(),
			"details": failure.GetDetails(),
		}
		writeJSON(writer, httpStatus, body)
		return
	}
	httpStatus := http.StatusBadGateway
	message := "trader service unavailable"
	errorCode := ""
	switch status.Code(err) {
	case codes.Unauthenticated:
		httpStatus = http.StatusUnauthorized
		message = "authentication required"
	case codes.InvalidArgument:
		httpStatus = http.StatusBadRequest
		message = status.Convert(err).Message()
	case codes.NotFound:
		httpStatus = http.StatusNotFound
		message = "not found"
	case codes.AlreadyExists:
		httpStatus = http.StatusConflict
		raw := status.Convert(err).Message()
		const activeConflictPrefix = "active_arbitrage_instrument_conflict: "
		if strings.HasPrefix(raw, activeConflictPrefix) {
			errorCode = "active_arbitrage_instrument_conflict"
			message = strings.TrimPrefix(raw, activeConflictPrefix)
		} else {
			message = "idempotency key conflict"
		}
	case codes.Aborted:
		httpStatus = http.StatusConflict
		message = status.Convert(err).Message()
	case codes.FailedPrecondition:
		httpStatus = http.StatusUnprocessableEntity
		message = status.Convert(err).Message()
	case codes.ResourceExhausted:
		httpStatus = http.StatusTooManyRequests
		message = status.Convert(err).Message()
	case codes.DeadlineExceeded:
		httpStatus = http.StatusGatewayTimeout
		message = "trader request timed out"
	case codes.Unavailable:
		if status.Convert(err).Message() == "order result is uncertain" {
			httpStatus = http.StatusGatewayTimeout
			message = "order result is uncertain"
		} else {
			httpStatus = http.StatusBadGateway
			message = "exchange service unavailable"
		}
	case codes.Internal:
		if status.Convert(err).Message() == "order persistence failed" {
			httpStatus = http.StatusInternalServerError
			message = "order persistence failed"
		}
	}
	body := map[string]string{"error": message}
	if errorCode != "" {
		body["code"] = errorCode
	}
	writeJSON(writer, httpStatus, body)
}
