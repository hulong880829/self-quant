package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	polymarketv1 "selfquant/backend/gen/polymarket/v1"
)

func (h *Handler) listPolymarketMarkets(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.polymarket.ListMarkets(ctx, &polymarketv1.ListMarketsRequest{
		Asset:      request.URL.Query().Get("asset"),
		Period:     request.URL.Query().Get("period"),
		ActiveOnly: request.URL.Query().Get("active") != "false",
	})
	if err != nil {
		h.writePolymarketError(writer, err)
		return
	}
	data := make([]map[string]any, 0, len(response.GetItems()))
	for _, market := range response.GetItems() {
		data = append(data, marketJSON(market))
	}
	writer.Header().Set("Cache-Control", "public, max-age=10, stale-while-revalidate=30")
	writeJSON(writer, http.StatusOK, map[string]any{
		"data":       data,
		"serverTime": formatTimestamp(response.GetServerTime()),
		"version":    response.GetSnapshotVersion(),
	})
}

func (h *Handler) getPolymarketSnapshot(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.polymarket.GetMarketSnapshot(
		ctx,
		&polymarketv1.GetMarketSnapshotRequest{MarketId: chi.URLParam(request, "id")},
	)
	if err != nil {
		h.writePolymarketError(writer, err)
		return
	}
	etag := `"` + response.GetSnapshot().GetVersion() + `"`
	if request.Header.Get("If-None-Match") == etag {
		writer.WriteHeader(http.StatusNotModified)
		return
	}
	writer.Header().Set("ETag", etag)
	writer.Header().Set("Cache-Control", "public, max-age=1, stale-while-revalidate=5")
	writeJSON(writer, http.StatusOK, map[string]any{
		"data":       snapshotJSON(response.GetSnapshot()),
		"serverTime": formatTimestamp(response.GetServerTime()),
	})
}

func (h *Handler) streamPolymarketSnapshots(writer http.ResponseWriter, request *http.Request) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeJSON(writer, http.StatusNotImplemented, map[string]string{"error": "stream unsupported"})
		return
	}
	marketID := strings.TrimSpace(request.URL.Query().Get("marketId"))
	if marketID == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "marketId is required"})
		return
	}
	stream, err := h.polymarket.StreamMarketSnapshots(
		request.Context(),
		&polymarketv1.StreamMarketSnapshotsRequest{MarketId: marketID},
	)
	if err != nil {
		h.writePolymarketError(writer, err)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache, no-transform")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(writer)
	for {
		response, receiveErr := stream.Recv()
		if receiveErr != nil {
			return
		}
		if response.GetSnapshot() == nil {
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, writeErr := fmt.Fprintf(writer, ": keepalive\n\n"); writeErr != nil {
				return
			}
			flusher.Flush()
			continue
		}
		payload, marshalErr := json.Marshal(map[string]any{
			"data":       snapshotJSON(response.GetSnapshot()),
			"serverTime": formatTimestamp(response.GetServerTime()),
		})
		if marshalErr != nil {
			return
		}
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, writeErr := fmt.Fprintf(writer, "event: snapshot\ndata: %s\n\n", payload); writeErr != nil {
			return
		}
		flusher.Flush()
	}
}

func (h *Handler) getPolymarketAccountSummary(writer http.ResponseWriter, request *http.Request) {
	token, accountID, ok := h.privatePolymarketRequest(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 12*time.Second)
	defer cancel()
	response, err := h.polymarket.GetAccountSummary(
		ctx, &polymarketv1.GetAccountSummaryRequest{
			Token: token, TradingAccountId: accountID,
		},
	)
	if err != nil {
		h.writePolymarketError(writer, err)
		return
	}
	summary := response.GetSummary()
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": map[string]any{
			"tradingAccountId": summary.GetTradingAccountId(),
			"accountName":      summary.GetAccountName(),
			"walletAddress":    summary.GetWalletAddress(),
			"availableBalance": summary.GetAvailableBalance(),
			"positionValue":    summary.GetPositionValue(),
			"totalAssets":      summary.GetTotalAssets(),
			"sourceUpdatedAt":  formatTimestamp(summary.GetSourceUpdatedAt()),
			"stale":            summary.GetStale(),
			"bindingStatus":    summary.GetBindingStatus(),
		},
	})
}

func (h *Handler) listPolymarketPositions(writer http.ResponseWriter, request *http.Request) {
	token, accountID, ok := h.privatePolymarketRequest(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 12*time.Second)
	defer cancel()
	response, err := h.polymarket.ListPositions(
		ctx, &polymarketv1.ListPositionsRequest{
			Token: token, TradingAccountId: accountID,
		},
	)
	if err != nil {
		h.writePolymarketError(writer, err)
		return
	}
	data := make([]map[string]any, 0, len(response.GetItems()))
	for _, position := range response.GetItems() {
		data = append(data, positionJSON(position))
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": data, "stale": response.GetStale(),
		"serverTime": formatTimestamp(response.GetServerTime()),
	})
}

func (h *Handler) listPolymarketOpenOrders(writer http.ResponseWriter, request *http.Request) {
	token, accountID, ok := h.privatePolymarketRequest(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 12*time.Second)
	defer cancel()
	response, err := h.polymarket.ListOpenOrders(
		ctx, &polymarketv1.ListOpenOrdersRequest{
			Token: token, TradingAccountId: accountID,
		},
	)
	if err != nil {
		h.writePolymarketError(writer, err)
		return
	}
	data := make([]map[string]any, 0, len(response.GetItems()))
	for _, order := range response.GetItems() {
		data = append(data, openOrderJSON(order))
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": data, "stale": response.GetStale(),
		"serverTime": formatTimestamp(response.GetServerTime()),
	})
}

func (h *Handler) streamPolymarketAccountEvents(writer http.ResponseWriter, request *http.Request) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeJSON(writer, http.StatusNotImplemented, map[string]string{"error": "stream unsupported"})
		return
	}
	token, accountID, ok := h.privatePolymarketRequest(writer, request)
	if !ok {
		return
	}
	stream, err := h.polymarket.StreamAccountEvents(
		request.Context(),
		&polymarketv1.StreamAccountEventsRequest{
			Token: token, TradingAccountId: accountID,
		},
	)
	if err != nil {
		h.writePolymarketError(writer, err)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache, no-transform")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(writer)
	for {
		event, receiveErr := stream.Recv()
		if receiveErr != nil {
			return
		}
		orders := make([]map[string]any, 0, len(event.GetOpenOrders()))
		for _, order := range event.GetOpenOrders() {
			orders = append(orders, openOrderJSON(order))
		}
		payload := map[string]any{
			"type": event.GetType(), "openOrders": orders,
			"portfolioChanged": event.GetPortfolioChanged(),
			"serverTime":       formatTimestamp(event.GetServerTime()),
		}
		if event.GetOrder() != nil {
			payload["order"] = openOrderJSON(event.GetOrder())
		}
		body, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return
		}
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, writeErr := fmt.Fprintf(writer, "event: account\ndata: %s\n\n", body); writeErr != nil {
			return
		}
		flusher.Flush()
	}
}

func (h *Handler) cancelPolymarketOrder(writer http.ResponseWriter, request *http.Request) {
	token, accountID, ok := h.privatePolymarketRequest(writer, request)
	if !ok {
		return
	}
	orderID := strings.TrimSpace(chi.URLParam(request, "id"))
	if orderID == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "order id is required"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 12*time.Second)
	defer cancel()
	response, err := h.polymarket.CancelOrder(ctx, &polymarketv1.CancelOrderRequest{
		Token: token, TradingAccountId: accountID, OrderId: orderID,
	})
	if err != nil {
		h.writePolymarketError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{"data": map[string]string{
		"orderId": response.GetOrderId(),
		"status":  response.GetStatus(),
		"message": response.GetMessage(),
	}})
}

type placePolymarketOrderBody struct {
	TradingAccountID int64  `json:"tradingAccountId"`
	MarketID         string `json:"marketId"`
	Outcome          string `json:"outcome"`
	Side             string `json:"side"`
	Amount           string `json:"amount"`
	AmountUnit       string `json:"amountUnit"`
	IdempotencyKey   string `json:"idempotencyKey"`
	ExecutionType    string `json:"executionType"`
	LimitPrice       string `json:"limitPrice"`
}

func (h *Handler) placePolymarketOrder(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	var body placePolymarketOrderBody
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
		key = uuid.NewString()
	}
	ctx, cancel := context.WithTimeout(request.Context(), 35*time.Second)
	defer cancel()
	response, err := h.polymarket.PlaceOrder(ctx, &polymarketv1.PlaceOrderRequest{
		Token: token, TradingAccountId: body.TradingAccountID,
		MarketId: body.MarketID, Outcome: body.Outcome, Side: body.Side,
		Amount: body.Amount, AmountUnit: body.AmountUnit, IdempotencyKey: key,
		ExecutionType: body.ExecutionType, LimitPrice: body.LimitPrice,
	})
	if err != nil {
		slog.Warn(
			"polymarket order request failed",
			"request_id", middleware.GetReqID(request.Context()),
			"account_id", body.TradingAccountID, "market_id", body.MarketID,
			"grpc_code", status.Code(err).String(), "error", err,
		)
		h.writePolymarketError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusAccepted, map[string]any{"data": orderJSON(response.GetOrder())})
}

func (h *Handler) getPolymarketOrder(writer http.ResponseWriter, request *http.Request) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
	defer cancel()
	response, err := h.polymarket.GetOrder(ctx, &polymarketv1.GetOrderRequest{
		Token: token, OrderId: chi.URLParam(request, "id"),
	})
	if err != nil {
		h.writePolymarketError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{"data": orderJSON(response.GetOrder())})
}

func (h *Handler) privatePolymarketRequest(
	writer http.ResponseWriter,
	request *http.Request,
) (string, int64, bool) {
	token, ok := h.sessionToken(request)
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return "", 0, false
	}
	accountID, err := strconv.ParseInt(chi.URLParam(request, "accountId"), 10, 64)
	if err != nil || accountID <= 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid account id"})
		return "", 0, false
	}
	return token, accountID, true
}

func (h *Handler) writePolymarketError(writer http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	message := "polymarket service unavailable"
	switch status.Code(err) {
	case codes.InvalidArgument:
		code, message = http.StatusBadRequest, status.Convert(err).Message()
	case codes.Unauthenticated:
		code, message = http.StatusUnauthorized, status.Convert(err).Message()
	case codes.PermissionDenied:
		code, message = http.StatusForbidden, "account access denied"
	case codes.NotFound:
		code, message = http.StatusNotFound, "resource not found"
	case codes.AlreadyExists, codes.Aborted:
		code, message = http.StatusConflict, status.Convert(err).Message()
	case codes.ResourceExhausted:
		code, message = http.StatusTooManyRequests, status.Convert(err).Message()
	case codes.FailedPrecondition:
		code, message = http.StatusConflict, status.Convert(err).Message()
	case codes.DeadlineExceeded:
		code, message = http.StatusGatewayTimeout, "polymarket request timed out"
	}
	writeJSON(writer, code, map[string]string{"error": message})
}

func marketJSON(market *polymarketv1.Market) map[string]any {
	return map[string]any{
		"id": market.GetId(), "conditionId": market.GetConditionId(),
		"slug": market.GetSlug(), "asset": market.GetAsset(), "period": market.GetPeriod(),
		"title": market.GetTitle(), "windowStart": formatTimestamp(market.GetWindowStart()),
		"windowEnd": formatTimestamp(market.GetWindowEnd()),
		"upTokenId": market.GetUpTokenId(), "downTokenId": market.GetDownTokenId(),
		"tickSize": market.GetTickSize(), "negativeRisk": market.GetNegativeRisk(),
		"active": market.GetActive(),
	}
}

func snapshotJSON(snapshot *polymarketv1.MarketSnapshot) map[string]any {
	points := make([]map[string]any, 0, len(snapshot.GetPriceSeries()))
	for _, point := range snapshot.GetPriceSeries() {
		points = append(points, map[string]any{
			"timestamp": formatTimestamp(point.GetTimestamp()),
			"openPrice": point.GetOpenPrice(), "chainlinkPrice": point.GetChainlinkPrice(),
		})
	}
	return map[string]any{
		"market":    marketJSON(snapshot.GetMarket()),
		"openPrice": snapshot.GetOpenPrice(), "chainlinkPrice": snapshot.GetChainlinkPrice(),
		"upBid": snapshot.GetUpBid(), "upAsk": snapshot.GetUpAsk(),
		"downBid": snapshot.GetDownBid(), "downAsk": snapshot.GetDownAsk(),
		"priceSeries":     points,
		"sourceUpdatedAt": formatTimestamp(snapshot.GetSourceUpdatedAt()),
		"stale":           snapshot.GetStale(), "version": snapshot.GetVersion(),
	}
}

func positionJSON(position *polymarketv1.Position) map[string]any {
	return map[string]any{
		"id": position.GetId(), "tradingAccountId": position.GetTradingAccountId(),
		"conditionId": position.GetConditionId(), "tokenId": position.GetTokenId(),
		"market": position.GetMarket(), "outcome": position.GetOutcome(),
		"size": position.GetSize(), "averagePrice": position.GetAveragePrice(),
		"currentPrice": position.GetCurrentPrice(), "initialValue": position.GetInitialValue(),
		"currentValue": position.GetCurrentValue(), "cashPnl": position.GetCashPnl(),
		"percentPnl": position.GetPercentPnl(), "redeemable": position.GetRedeemable(),
		"sourceUpdatedAt": formatTimestamp(position.GetSourceUpdatedAt()),
	}
}

func orderJSON(order *polymarketv1.Order) map[string]any {
	return map[string]any{
		"id": order.GetId(), "clobOrderId": order.GetClobOrderId(),
		"tradingAccountId": order.GetTradingAccountId(), "marketId": order.GetMarketId(),
		"tokenId": order.GetTokenId(), "outcome": order.GetOutcome(), "side": order.GetSide(),
		"requestedAmount": order.GetRequestedAmount(), "amountUnit": order.GetAmountUnit(),
		"filledSize": order.GetFilledSize(), "averagePrice": order.GetAveragePrice(),
		"status": order.GetStatus(), "errorCode": order.GetErrorCode(),
		"executionType": order.GetExecutionType(), "limitPrice": order.GetLimitPrice(),
		"clobOrderType": order.GetClobOrderType(), "errorMessage": order.GetErrorMessage(),
		"createdAt": formatTimestamp(order.GetCreatedAt()),
		"updatedAt": formatTimestamp(order.GetUpdatedAt()),
	}
}

func openOrderJSON(order *polymarketv1.OpenOrder) map[string]any {
	return map[string]any{
		"id": order.GetId(), "conditionId": order.GetConditionId(),
		"tokenId": order.GetTokenId(), "marketTitle": order.GetMarketTitle(),
		"outcome": order.GetOutcome(), "side": order.GetSide(),
		"price": order.GetPrice(), "originalSize": order.GetOriginalSize(),
		"matchedSize": order.GetMatchedSize(), "remainingSize": order.GetRemainingSize(),
		"status": order.GetStatus(), "orderType": order.GetOrderType(),
		"createdAt": formatTimestamp(order.GetCreatedAt()),
	}
}

type timestampValue interface {
	AsTime() time.Time
	IsValid() bool
}

func formatTimestamp(value timestampValue) string {
	if value == nil || !value.IsValid() {
		return ""
	}
	return value.AsTime().UTC().Format(time.RFC3339Nano)
}
