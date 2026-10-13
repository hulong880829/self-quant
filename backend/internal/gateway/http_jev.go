package gateway

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	jevv1 "selfquant/backend/gen/jev/v1"
	"selfquant/backend/internal/jev"
)

func (h *Handler) getJevSnapshot(writer http.ResponseWriter, request *http.Request) {
	if h.jev == nil {
		writeJevUnavailable(writer)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.jev.GetSnapshot(ctx, &jevv1.GetSnapshotRequest{
		IfNoneMatch:     request.Header.Get("If-None-Match"),
		StrategyVersion: request.URL.Query().Get("strategyVersion"),
	})
	if err != nil {
		h.writeJevError(writer, err)
		return
	}
	writeJevJSON(writer, request, response.GetEtag(), response.GetNotModified(), jevSnapshotJSON(response))
}

func (h *Handler) listJevPositions(writer http.ResponseWriter, request *http.Request) {
	h.proxyJevList(writer, request, func(ctx context.Context, limit int32, cursor, etag string) (any, string, bool, error) {
		response, err := h.jev.ListPositions(ctx, &jevv1.ListPositionsRequest{
			Limit: limit, Cursor: cursor, IfNoneMatch: etag,
			StrategyVersion: request.URL.Query().Get("strategyVersion"),
		})
		if err != nil {
			return nil, "", false, err
		}
		items := make([]map[string]any, 0, len(response.GetItems()))
		for _, item := range response.GetItems() {
			items = append(items, jevPositionJSON(item))
		}
		return map[string]any{"data": items, "nextCursor": emptyToNil(response.GetNextCursor())}, response.GetEtag(), response.GetNotModified(), nil
	})
}

func (h *Handler) listJevDecisions(writer http.ResponseWriter, request *http.Request) {
	h.proxyJevList(writer, request, func(ctx context.Context, limit int32, cursor, etag string) (any, string, bool, error) {
		response, err := h.jev.ListDecisions(ctx, &jevv1.ListDecisionsRequest{
			Limit: limit, Cursor: cursor, IfNoneMatch: etag,
			StrategyVersion: request.URL.Query().Get("strategyVersion"),
		})
		if err != nil {
			return nil, "", false, err
		}
		items := make([]map[string]any, 0, len(response.GetItems()))
		for _, item := range response.GetItems() {
			items = append(items, jevDecisionJSON(item))
		}
		return map[string]any{"data": items, "nextCursor": emptyToNil(response.GetNextCursor())}, response.GetEtag(), response.GetNotModified(), nil
	})
}

func (h *Handler) listJevTrades(writer http.ResponseWriter, request *http.Request) {
	h.proxyJevList(writer, request, func(ctx context.Context, limit int32, cursor, etag string) (any, string, bool, error) {
		response, err := h.jev.ListTrades(ctx, &jevv1.ListTradesRequest{
			Limit: limit, Cursor: cursor, IfNoneMatch: etag,
			StrategyVersion: request.URL.Query().Get("strategyVersion"),
		})
		if err != nil {
			return nil, "", false, err
		}
		items := make([]map[string]any, 0, len(response.GetItems()))
		for _, item := range response.GetItems() {
			items = append(items, jevTradeJSON(item))
		}
		return map[string]any{"data": items, "nextCursor": emptyToNil(response.GetNextCursor())}, response.GetEtag(), response.GetNotModified(), nil
	})
}

func (h *Handler) getJevEquity(writer http.ResponseWriter, request *http.Request) {
	if h.jev == nil {
		writeJevUnavailable(writer)
		return
	}
	rangeValue := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("range")))
	if rangeValue == "" {
		rangeValue = "24h"
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	response, err := h.jev.GetEquityCurve(ctx, &jevv1.GetEquityCurveRequest{
		Range: rangeValue, IfNoneMatch: request.Header.Get("If-None-Match"),
		StrategyVersion: request.URL.Query().Get("strategyVersion"),
	})
	if err != nil {
		h.writeJevError(writer, err)
		return
	}
	points := make([]map[string]any, 0, len(response.GetPoints()))
	for _, point := range response.GetPoints() {
		points = append(points, map[string]any{
			"ts": protoTimeJSON(point.GetTs()), "equity": point.GetEquity(), "netPnl": point.GetNetPnl(),
		})
	}
	writeJevJSON(writer, request, response.GetEtag(), response.GetNotModified(), map[string]any{
		"range": response.GetRange(), "points": points,
	})
}

func (h *Handler) proxyJevList(
	writer http.ResponseWriter,
	request *http.Request,
	call func(ctx context.Context, limit int32, cursor, etag string) (any, string, bool, error),
) {
	if h.jev == nil {
		writeJevUnavailable(writer)
		return
	}
	limit := int32(jev.DefaultListLimit)
	if raw := strings.TrimSpace(request.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "limit must be a positive integer"})
			return
		}
		limit = int32(jev.ClampLimit(parsed))
	}
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	payload, etag, notModified, err := call(ctx, limit, request.URL.Query().Get("cursor"), request.Header.Get("If-None-Match"))
	if err != nil {
		h.writeJevError(writer, err)
		return
	}
	writeJevJSON(writer, request, etag, notModified, payload)
}

func (h *Handler) writeJevError(writer http.ResponseWriter, err error) {
	httpStatus := http.StatusServiceUnavailable
	message := "jev unavailable"
	switch status.Code(err) {
	case codes.InvalidArgument:
		httpStatus = http.StatusBadRequest
		message = "invalid jev request"
	case codes.DeadlineExceeded:
		httpStatus = http.StatusGatewayTimeout
		message = "jev query timed out"
	}
	writeJSON(writer, httpStatus, map[string]any{"error": message})
}

func writeJevUnavailable(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusServiceUnavailable, map[string]any{"error": "jev unavailable"})
}

func writeJevJSON(writer http.ResponseWriter, request *http.Request, etag string, notModified bool, payload any) {
	if etag != "" {
		writer.Header().Set("ETag", etag)
	}
	writer.Header().Set("Cache-Control", "no-store")
	if notModified || (etag != "" && request.Header.Get("If-None-Match") == etag) {
		writer.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(writer, http.StatusOK, payload)
}

func jevSnapshotJSON(item *jevv1.GetSnapshotResponse) map[string]any {
	return map[string]any{
		"status": item.GetStatus(), "degradedReasons": item.GetDegradedReasons(),
		"lastSuccessfulAt": protoTimeJSON(item.GetLastSuccessfulAt()),
		"lastDecisionAt":   protoTimeJSON(item.GetLastDecisionAt()),
		"lastMarketDataAt": protoTimeJSON(item.GetLastMarketDataAt()),
		"serverTime":       protoTimeJSON(item.GetServerTime()), "model": item.GetModel(),
		"modelRequested": item.GetModelRequested(), "promptVersion": item.GetPromptVersion(),
		"policyVersion": item.GetPolicyVersion(), "simulated": item.GetSimulated(),
		"equity": item.GetEquity(), "netPnl": item.GetNetPnl(), "fundingPnl": item.GetFundingPnl(),
		"realizedSpreadPnl": item.GetRealizedSpreadPnl(), "unrealizedSpreadPnl": item.GetUnrealizedSpreadPnl(),
		"cumulativeCost": item.GetCumulativeCost(), "maxDrawdown": item.GetMaxDrawdown(),
		"initialEquity": item.GetInitialEquity(), "paperRoundTripCostBps": item.GetPaperRoundTripCostBps(),
		"usdtUsdcOneToOne": item.GetUsdtUsdcOneToOne(), "singleEntryEmptyReason": emptyToNil(item.GetSingleEntryEmptyReason()),
		"crossEntryEmptyReason": emptyToNil(item.GetCrossEntryEmptyReason()),
		"strategySingleStatus":  item.GetStrategySingleStatus(), "strategyCrossStatus": item.GetStrategyCrossStatus(),
		"openPositionCount": item.GetOpenPositionCount(), "entryCandidateCount": item.GetEntryCandidateCount(),
		"typesafeConfigured": item.GetTypesafeConfigured(), "snapshotVersion": item.GetSnapshotVersion(),
		"singleFilter": jevSingleFilterJSON(item.GetSingleFilter()),
		"crossFilter":  jevCrossFilterJSON(item.GetCrossFilter()),
		"decisionJobs": item.GetDecisionJobs(), "typesafeRequests": item.GetTypesafeRequests(),
		"typesafeSuccess": item.GetTypesafeSuccess(), "typesafeError": item.GetTypesafeError(),
		"modelStatus": item.GetModelStatus(), "lastModelCallAt": protoTimeJSON(item.GetLastModelCallAt()),
		"strategyVersion": item.GetStrategyVersion(),
		"lifecycleEquity": item.GetLifecycleEquity(), "lifecycleNetPnl": item.GetLifecycleNetPnl(),
		"lifecycleFundingPnl":          item.GetLifecycleFundingPnl(),
		"lifecycleRealizedSpreadPnl":   item.GetLifecycleRealizedSpreadPnl(),
		"lifecycleUnrealizedSpreadPnl": item.GetLifecycleUnrealizedSpreadPnl(),
		"lifecycleCumulativeCost":      item.GetLifecycleCumulativeCost(),
		"availableStrategyVersions":    item.GetAvailableStrategyVersions(),
	}
}

func jevSingleFilterJSON(item *jevv1.JevSingleFilterStats) map[string]any {
	if item == nil {
		return map[string]any{
			"input": 0, "contractPassed": 0, "positive24hPassed": 0, "freshPassed": 0,
			"turnoverPassed": 0, "spotMatched": 0, "bboFreshPassed": 0, "depthPassed": 0,
			"basisForecastPassed": 0, "final": 0,
		}
	}
	return map[string]any{
		"input": item.GetInput(), "contractPassed": item.GetContractPassed(),
		"positive24hPassed": item.GetPositive_24HPassed(), "freshPassed": item.GetFreshPassed(),
		"turnoverPassed": item.GetTurnoverPassed(), "spotMatched": item.GetSpotMatched(),
		"bboFreshPassed": item.GetBboFreshPassed(), "depthPassed": item.GetDepthPassed(),
		"basisForecastPassed": item.GetBasisForecastPassed(), "final": item.GetFinal(),
	}
}

func jevCrossFilterJSON(item *jevv1.JevCrossFilterStats) map[string]any {
	if item == nil {
		return map[string]any{
			"input": 0, "contractPassed": 0, "positiveEdgePassed": 0, "freshPassed": 0,
			"turnoverPassed": 0, "bboFreshPassed": 0, "depthPassed": 0,
			"basisForecastPassed": 0, "final": 0,
		}
	}
	return map[string]any{
		"input": item.GetInput(), "contractPassed": item.GetContractPassed(),
		"positiveEdgePassed": item.GetPositiveEdgePassed(), "freshPassed": item.GetFreshPassed(),
		"turnoverPassed": item.GetTurnoverPassed(), "bboFreshPassed": item.GetBboFreshPassed(),
		"depthPassed": item.GetDepthPassed(), "basisForecastPassed": item.GetBasisForecastPassed(),
		"final": item.GetFinal(),
	}
}

func jevPositionJSON(item *jevv1.JevPosition) map[string]any {
	return map[string]any{
		"id": item.GetId(), "kind": item.GetKind(), "status": item.GetStatus(), "candidateKey": item.GetCandidateKey(),
		"baseAsset": item.GetBaseAsset(), "quoteAsset": item.GetQuoteAsset(), "globalSymbol": item.GetGlobalSymbol(),
		"longExchange": item.GetLongExchange(), "longSymbol": item.GetLongSymbol(), "longProduct": item.GetLongProduct(),
		"shortExchange": item.GetShortExchange(), "shortSymbol": item.GetShortSymbol(), "shortProduct": item.GetShortProduct(),
		"sharedBaseQty": item.GetSharedBaseQty(), "longQty": item.GetLongQty(), "shortQty": item.GetShortQty(),
		"longEntryPrice": item.GetLongEntryPrice(), "shortEntryPrice": item.GetShortEntryPrice(),
		"longMarkPrice": item.GetLongMarkPrice(), "shortMarkPrice": item.GetShortMarkPrice(),
		"realizedSpreadPnl": item.GetRealizedSpreadPnl(), "unrealizedSpreadPnl": item.GetUnrealizedSpreadPnl(),
		"fundingPnl": item.GetFundingPnl(), "costPaid": item.GetCostPaid(), "netPnl": item.GetNetPnl(),
		"exitPendingReason": emptyToNil(item.GetExitPendingReason()), "degradedReason": emptyToNil(item.GetDegradedReason()),
		"openedAt": protoTimeJSON(item.GetOpenedAt()), "closedAt": protoTimeJSON(item.GetClosedAt()),
		"lastMarkAt":       protoTimeJSON(item.GetLastMarkAt()),
		"entryMidBasisBps": emptyToNil(item.GetEntryMidBasisBps()),
		"strategyVersion":  emptyToNil(item.GetStrategyVersion()),
		"netIfExit":        emptyToNil(item.GetNetIfExit()),
		"netIfExitAt":      protoTimeJSON(item.GetNetIfExitAt()),
		"exitConfirmCount": item.GetExitConfirmCount(),
	}
}

func jevDecisionJSON(item *jevv1.JevDecision) map[string]any {
	return map[string]any{
		"id": item.GetId(), "positionId": emptyToNil(item.GetPositionId()), "candidateKey": item.GetCandidateKey(),
		"kind": item.GetKind(), "decision": item.GetDecision(), "policyStatus": item.GetPolicyStatus(),
		"executionStatus": item.GetExecutionStatus(), "enterProbability": item.GetEnterProbability(),
		"exitProbability": item.GetExitProbability(), "confidence": item.GetConfidence(),
		"expectedNetReturn24h": emptyToNil(item.GetExpectedNetReturn_24H()), "rationale": item.GetRationale(),
		"promptVersion": item.GetPromptVersion(), "policyVersion": item.GetPolicyVersion(),
		"modelRequested": item.GetModelRequested(), "modelActual": item.GetModelActual(),
		"errorMessage": emptyToNil(item.GetErrorMessage()), "decisionAt": protoTimeJSON(item.GetDecisionAt()),
		"executionStartedAt":       protoTimeJSON(item.GetExecutionStartedAt()),
		"fillDeadline":             protoTimeJSON(item.GetFillDeadline()),
		"fundingReturn24h":         emptyToNil(item.GetFundingReturn_24H()),
		"holdingHorizonNetReturn":  emptyToNil(item.GetHoldingHorizonNetReturn()),
		"expectedPaybackMinutes":   emptyToNil(item.GetExpectedPaybackMinutes()),
		"currentMidBasisBps":       emptyToNil(item.GetCurrentMidBasisBps()),
		"entryMidBasisBps":         emptyToNil(item.GetEntryMidBasisBps()),
		"basisChangeSinceEntryBps": emptyToNil(item.GetBasisChangeSinceEntryBps()),
		"expectedBasisReturn24h":   emptyToNil(item.GetExpectedBasisReturn_24H()),
		"basisReturnP10":           emptyToNil(item.GetBasisReturnP10()),
		"basisProfitProbability":   emptyToNil(item.GetBasisProfitProbability()),
		"basisForecastConfidence":  emptyToNil(item.GetBasisForecastConfidence()),
		"basisSampleCount":         optionalInt32JSON(item.BasisSampleCount),
		"basisModelVersion":        emptyToNil(item.GetBasisModelVersion()),
		"strategyVersion":          emptyToNil(item.GetStrategyVersion()),
		"parseErrorType":           emptyToNil(item.GetParseErrorType()),
	}
}

func optionalInt32JSON(value *int32) any {
	if value == nil {
		return nil
	}
	return *value
}

func jevTradeJSON(item *jevv1.JevTrade) map[string]any {
	return map[string]any{
		"id": item.GetId(), "positionId": item.GetPositionId(), "decisionId": emptyToNil(item.GetDecisionId()),
		"side": item.GetSide(), "sharedBaseQty": item.GetSharedBaseQty(), "longFillPrice": item.GetLongFillPrice(),
		"shortFillPrice": item.GetShortFillPrice(), "notional": item.GetNotional(), "cost": item.GetCost(),
		"filledAt":        protoTimeJSON(item.GetFilledAt()),
		"strategyVersion": emptyToNil(item.GetStrategyVersion()),
		"baseAsset":       emptyToNil(item.GetBaseAsset()),
		"kind":            emptyToNil(item.GetKind()),
		"longExchange":    emptyToNil(item.GetLongExchange()),
		"longSymbol":      emptyToNil(item.GetLongSymbol()),
		"longProduct":     emptyToNil(item.GetLongProduct()),
		"shortExchange":   emptyToNil(item.GetShortExchange()),
		"shortSymbol":     emptyToNil(item.GetShortSymbol()),
		"shortProduct":    emptyToNil(item.GetShortProduct()),
	}
}

func emptyToNil(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
