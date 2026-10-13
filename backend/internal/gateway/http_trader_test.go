package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	traderv1 "selfquant/backend/gen/trader/v1"
)

func TestTraderRoutesRequireSession(t *testing.T) {
	router := testRouter(t, &testFundingServer{}, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/trader/orders", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound && recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestTraderArbitrageJSONMatchesWebContract(t *testing.T) {
	result := traderArbitrageCombinationJSON(&traderv1.ArbitrageCombination{
		Id: "combo-1", Status: "running", MakerLeg: "a",
		ExecutionMode: "maker_then_hedge", AskThresholdBps: "12",
		BidThresholdBps: "-8", TargetNotional: "10000",
		PositionNotional: "0", RuntimeState: "monitoring",
		LegABasePosition: "1", LegBBasePosition: "-1", CarryBaseQuantity: "0",
		GrossTurnoverNotional: "2000",
		LegAAverageEntryPrice: "100", LegBAverageEntryPrice: "101",
		AverageEntrySpreadBps: "100",
		LegAUnrealizedPnl:     "2", LegBUnrealizedPnl: "-1",
		CombinedPositionAnnualized: "0.18", FundingHistoryComplete: true,
		LegAVenueBaselineBasePosition: "10",
		LegBVenueBaselineBasePosition: "-10",
		VenueBaselineCapturedAt:       timestamppb.Now(),
		LegAExpectedBasePosition:      "11",
		LegBExpectedBasePosition:      "-11",
		LegA: &traderv1.ArbitrageLeg{
			ProductName: "ARBITRAGE", TradingAccountId: 1,
			AccountName: "a", Exchange: "binance", InstrumentId: 11,
			ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		},
		LegB: &traderv1.ArbitrageLeg{
			ProductName: "ARBITRAGE", TradingAccountId: 2,
			AccountName: "b", Exchange: "okx", InstrumentId: 22,
			ExchangeSymbol: "BTC-USDT-SWAP", BaseAsset: "BTC", QuoteAsset: "USDT",
		},
		MarketDataStale:                    true,
		EarlyExitFunding_8HAnnualizedFloor: "0.05",
		EarlyExitLiquidationDistancePct:    "20",
		LegALiquidationDistancePct:         "18.42",
		FinalMetrics: &traderv1.ArbitrageFinalMetrics{
			Status: "completed", RetryCount: 1, RealizedSpreadPnl: "12",
			EstimatedFundingPnl: "3", EstimatedTradingFee: "1",
			CombinedPositionAnnualized: "0.18", MetricsQuality: "estimated",
			HoldingSeconds: "3600", NotionalExposureSeconds: "3600000",
			CalculatedAt: timestamppb.Now(), CompletedAt: timestamppb.Now(),
		},
	})
	if result["productName"] != "ARBITRAGE" || result["preferredLeg"] != "a" ||
		result["runtimeState"] != "monitoring" || result["carryBaseQuantity"] != "0" ||
		result["grossTurnoverNotional"] != "2000" ||
		result["averageEntrySpreadBps"] != "100" ||
		result["combinedPositionAnnualized"] != "0.18" ||
		result["legAVenueBaselineBasePosition"] != "10" ||
		result["legAExpectedBasePosition"] != "11" ||
		result["venueBaselineCapturedAt"] == "" ||
		result["fundingHistoryComplete"] != true {
		t.Fatalf("result=%v", result)
	}
	if result["askSpreadBps"] != nil || result["bidSpreadBps"] != nil {
		t.Fatalf("missing spreads must be null: %v", result)
	}
	if _, ok := result["orderNotional"]; ok {
		t.Fatalf("orderNotional must not be serialized: %v", result)
	}
	if _, ok := result["runMode"]; !ok {
		t.Fatalf("runMode missing: %v", result)
	}
	if result["earlyExitFunding8hAnnualizedFloor"] != "0.05" {
		t.Fatalf("earlyExitFunding8hAnnualizedFloor=%v", result["earlyExitFunding8hAnnualizedFloor"])
	}
	if result["earlyExitLiquidationDistancePct"] != "20" {
		t.Fatalf("earlyExitLiquidationDistancePct=%v", result["earlyExitLiquidationDistancePct"])
	}
	if result["legALiquidationDistancePct"] != "18.42" || result["legBLiquidationDistancePct"] != nil {
		t.Fatalf("liquidation distance=%v %v", result["legALiquidationDistancePct"], result["legBLiquidationDistancePct"])
	}
	finalMetrics, _ := result["finalMetrics"].(map[string]any)
	if finalMetrics["status"] != "completed" ||
		finalMetrics["estimatedTradingFee"] != "1" ||
		finalMetrics["metricsQuality"] != "estimated" {
		t.Fatalf("finalMetrics=%v", finalMetrics)
	}
}

func TestTraderArbitrageEventJSONPreservesMessageAndPayloadFallback(t *testing.T) {
	event := traderArbitrageEventJSON(&traderv1.ArbitrageEvent{
		Id: "1", Type: "legacy", Message: `["legacy"]`, CreatedAt: timestamppb.Now(),
	})
	if event["payload"] != nil || event["message"] != `["legacy"]` {
		t.Fatalf("event=%v", event)
	}
}

func TestArbitrageHistoryRequestValidation(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/trader/arbitrage-combinations/combo/events?limit=101",
		nil,
	)
	recorder := httptest.NewRecorder()
	if _, ok := arbitrageHistoryRequest(recorder, request, "token"); ok ||
		recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	request = httptest.NewRequest(
		http.MethodGet,
		"/api/v1/trader/arbitrage-combinations/combo/events?limit=25&from=2026-10-13T00:00:00Z",
		nil,
	)
	recorder = httptest.NewRecorder()
	query, ok := arbitrageHistoryRequest(recorder, request, "token")
	if !ok || query.GetLimit() != 25 || query.GetFrom() == nil {
		t.Fatalf("query=%v status=%d body=%s", query, recorder.Code, recorder.Body.String())
	}
}

func TestUpdateArbitrageCombinationRejectsUnknownAndEmptyPayloads(t *testing.T) {
	handler := &Handler{sessionCookieName: "sq_session"}
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown field", body: `{"targetNotional":"1000","unexpected":true}`},
		{name: "empty update", body: `{}`},
		{name: "empty exit annualized rate", body: `{"exitAnnualizedRate":""}`},
		{name: "empty 8h floor", body: `{"earlyExitFunding8hAnnualizedFloor":""}`},
		{name: "empty liquidation distance", body: `{"earlyExitLiquidationDistancePct":""}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodPatch,
				"/api/v1/trader/arbitrage-combinations/combo-1",
				strings.NewReader(test.body),
			)
			request.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
			recorder := httptest.NewRecorder()
			handler.updateArbitrageCombination(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

type capturingUpdateTrader struct {
	traderv1.TraderServiceClient
	got *traderv1.UpdateArbitrageCombinationRequest
}

func (s *capturingUpdateTrader) UpdateArbitrageCombination(
	_ context.Context,
	in *traderv1.UpdateArbitrageCombinationRequest,
	_ ...grpc.CallOption,
) (*traderv1.UpdateArbitrageCombinationResponse, error) {
	s.got = in
	createdAt := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	return &traderv1.UpdateArbitrageCombinationResponse{
		Combination: &traderv1.ArbitrageCombination{
			Id:                              in.GetCombinationId(),
			TargetNotional:                  in.GetTargetNotional(),
			ExitAfterSeconds:                in.GetExitAfterSeconds(),
			EarlyExitLiquidationDistancePct: in.GetEarlyExitLiquidationDistancePct(),
			CreatedAt:                       timestamppb.New(createdAt),
			ScheduledExitAt:                 timestamppb.New(createdAt.Add(4 * time.Hour)),
		},
	}, nil
}

func TestUpdateArbitrageCombinationForwardsP0Fields(t *testing.T) {
	stub := &capturingUpdateTrader{}
	handler := &Handler{sessionCookieName: "sq_session", trader: stub}
	router := chi.NewRouter()
	router.Patch(
		"/api/v1/trader/arbitrage-combinations/{id}",
		handler.updateArbitrageCombination,
	)
	request := httptest.NewRequest(
		http.MethodPatch,
		"/api/v1/trader/arbitrage-combinations/combo-1",
		strings.NewReader(
			`{"targetNotional":"12000","exitAfterSeconds":14400,`+
				`"earlyExitLiquidationDistancePct":"15"}`,
		),
	)
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if stub.got.GetCombinationId() != "combo-1" ||
		stub.got.GetTargetNotional() != "12000" ||
		stub.got.GetExitAfterSeconds() != 14400 ||
		stub.got.GetEarlyExitLiquidationDistancePct() != "15" {
		t.Fatalf("forwarded=%v", stub.got)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	data, _ := payload["data"].(map[string]any)
	if data["targetNotional"] != "12000" ||
		data["earlyExitLiquidationDistancePct"] != "15" ||
		data["scheduledExitAt"] != "2026-10-13T04:00:00Z" {
		t.Fatalf("data=%v", data)
	}
}

type capturingPreflightTrader struct {
	traderv1.TraderServiceClient
	got *traderv1.PreflightArbitrageCombinationRequest
}

func (s *capturingPreflightTrader) PreflightArbitrageCombination(
	_ context.Context,
	in *traderv1.PreflightArbitrageCombinationRequest,
	_ ...grpc.CallOption,
) (*traderv1.PreflightArbitrageCombinationResponse, error) {
	s.got = in
	now := time.Now().UTC()
	return &traderv1.PreflightArbitrageCombinationResponse{
		Executable: true, CheckedAt: timestamppb.New(now),
		ExpiresAt: timestamppb.New(now.Add(3 * time.Second)),
		LegA: &traderv1.PreflightLeg{
			AvailableFunds: &traderv1.PreflightDecimal{Value: "500", Status: "known"},
			EstimatedFee:   &traderv1.PreflightDecimal{Status: "unknown"},
		},
		LegB: &traderv1.PreflightLeg{},
		Warnings: []*traderv1.PreflightWarning{{
			Code: "fee_unknown", Leg: "a", Exchange: "binance",
		}},
	}, nil
}

func TestPreflightArbitrageCombinationHTTP(t *testing.T) {
	stub := &capturingPreflightTrader{}
	handler := &Handler{sessionCookieName: "sq_session", trader: stub}
	request := httptest.NewRequest(
		http.MethodPost, "/api/v1/trader/arbitrage-combinations/preflight",
		strings.NewReader(`{"legAAccountId":1,"legAInstrumentId":11,"legBAccountId":2,`+
			`"legBInstrumentId":22,"targetNotional":"1000","executionMode":"maker_then_hedge",`+
			`"preferredLeg":"a"}`),
	)
	request.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
	recorder := httptest.NewRecorder()
	handler.preflightArbitrageCombination(recorder, request)
	if recorder.Code != http.StatusOK || stub.got.GetTargetNotional() != "1000" {
		t.Fatalf("status=%d request=%v body=%s", recorder.Code, stub.got, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	data, _ := payload["data"].(map[string]any)
	legA, _ := data["legA"].(map[string]any)
	fee, _ := legA["estimatedFee"].(map[string]any)
	if data["executable"] != true || fee["status"] != "unknown" || fee["value"] != nil {
		t.Fatalf("data=%v", data)
	}
}

func TestWriteTraderErrorDoesNotLeakVenuePayload(t *testing.T) {
	handler := &Handler{}
	recorder := httptest.NewRecorder()
	handler.writeTraderError(recorder, status.Error(codes.Internal, "apikey=secret raw={\"order\":1}"))
	body := recorder.Body.String()
	if recorder.Code != http.StatusBadGateway || strings.Contains(body, "apikey") || strings.Contains(body, "raw=") {
		t.Fatalf("status=%d body=%s", recorder.Code, body)
	}
}

func TestWriteTraderErrorMappings(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		statusCode int
		message    string
	}{
		{
			name: "persistence", err: status.Error(codes.Internal, "order persistence failed"),
			statusCode: http.StatusInternalServerError, message: "order persistence failed",
		},
		{
			name: "venue unavailable", err: status.Error(codes.Unavailable, "venue unavailable"),
			statusCode: http.StatusBadGateway, message: "exchange service unavailable",
		},
		{
			name: "uncertain", err: status.Error(codes.Unavailable, "order result is uncertain"),
			statusCode: http.StatusGatewayTimeout, message: "order result is uncertain",
		},
		{
			name: "rate limited", err: status.Error(codes.ResourceExhausted, "venue rate limited"),
			statusCode: http.StatusTooManyRequests, message: "venue rate limited",
		},
		{
			name: "rejected", err: status.Error(codes.FailedPrecondition, "venue rejected order"),
			statusCode: http.StatusUnprocessableEntity, message: "venue rejected order",
		},
		{
			name: "unauthenticated", err: status.Error(codes.Unauthenticated, "invalid session"),
			statusCode: http.StatusUnauthorized, message: "authentication required",
		},
		{
			name: "not found", err: status.Error(codes.NotFound, "missing"),
			statusCode: http.StatusNotFound, message: "not found",
		},
	}
	handler := &Handler{}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.writeTraderError(recorder, test.err)
			if recorder.Code != test.statusCode ||
				!strings.Contains(recorder.Body.String(), test.message) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestWriteTraderActiveInstrumentConflict(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&Handler{}).writeTraderError(
		recorder,
		status.Error(
			codes.AlreadyExists,
			"active_arbitrage_instrument_conflict: 账户 main 的 okx BTC-USDT-SWAP 已被运行中套利组合 combo-1 占用",
		),
	)
	body := recorder.Body.String()
	if recorder.Code != http.StatusConflict ||
		!strings.Contains(body, `"code":"active_arbitrage_instrument_conflict"`) ||
		!strings.Contains(body, "BTC-USDT-SWAP") ||
		strings.Contains(body, "active_arbitrage_instrument_conflict: 账户") {
		t.Fatalf("status=%d body=%s", recorder.Code, body)
	}
}

func TestWriteTraderErrorCreateFailureMapping(t *testing.T) {
	margin, err := status.New(codes.FailedPrecondition, "insufficient margin").WithDetails(
		&traderv1.ArbitrageCreateFailure{
			Code: "insufficient_margin", Message: "okx available margin is insufficient", Leg: "b",
			Details: map[string]string{"exchange": "okx"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	(&Handler{}).writeTraderError(recorder, margin.Err())
	if recorder.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(recorder.Body.String(), `"code":"insufficient_margin"`) ||
		!strings.Contains(recorder.Body.String(), `"leg":"b"`) {
		t.Fatalf("margin status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	leverage, err := status.New(codes.InvalidArgument, "spot leverage must be 1").WithDetails(
		&traderv1.ArbitrageCreateFailure{Code: "invalid_leverage", Message: "spot leverage must be 1", Leg: "a"},
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	(&Handler{}).writeTraderError(recorder, leverage.Err())
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), `"code":"invalid_leverage"`) {
		t.Fatalf("leverage status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	apply, err := status.New(codes.Unavailable, "leverage apply failed").WithDetails(
		&traderv1.ArbitrageCreateFailure{
			Code: "leverage_apply_failed", Message: "partial apply", Leg: "b",
			Details: map[string]string{"appliedLegs": `["a"]`},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	(&Handler{}).writeTraderError(recorder, apply.Err())
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("apply status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	capacity, err := status.New(codes.FailedPrecondition, "capacity exceeded").WithDetails(
		&traderv1.ArbitrageCreateFailure{
			Code: "position_capacity_exceeded", Message: "binance position capacity is insufficient",
			Leg: "a", Details: map[string]string{"appliedLegs": `["a"]`},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	(&Handler{}).writeTraderError(recorder, capacity.Err())
	if recorder.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(recorder.Body.String(), `"code":"position_capacity_exceeded"`) ||
		!strings.Contains(recorder.Body.String(), `appliedLegs`) {
		t.Fatalf("capacity status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	unavailable, err := status.New(codes.Unavailable, "preview failed").WithDetails(
		&traderv1.ArbitrageCreateFailure{
			Code: "venue_unavailable", Message: "query bitget leverage preview failed", Leg: "a",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	(&Handler{}).writeTraderError(recorder, unavailable.Err())
	if recorder.Code != http.StatusBadGateway ||
		!strings.Contains(recorder.Body.String(), `"code":"venue_unavailable"`) {
		t.Fatalf("unavailable status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	(&Handler{}).writeTraderError(
		recorder,
		status.Error(codes.FailedPrecondition, "arbitrage combination is not closable"),
	)
	if recorder.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(recorder.Body.String(), "arbitrage combination is not closable") ||
		strings.Contains(recorder.Body.String(), `"leg"`) {
		t.Fatalf("generic failed precondition status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestCreateArbitrageCombinationRejectsOrderNotional(t *testing.T) {
	handler := &Handler{sessionCookieName: "sq_session"}
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/trader/arbitrage-combinations",
		strings.NewReader(`{"targetNotional":"10000","orderNotional":"500","askThresholdBps":"12","bidThresholdBps":"-8"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "create-order-notional")
	request.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
	recorder := httptest.NewRecorder()
	handler.createArbitrageCombination(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

type stubCreateTrader struct {
	traderv1.TraderServiceClient
	got *traderv1.CreateArbitrageCombinationRequest
	err error
}

func (s *stubCreateTrader) CreateArbitrageCombination(
	_ context.Context,
	in *traderv1.CreateArbitrageCombinationRequest,
	_ ...grpc.CallOption,
) (*traderv1.CreateArbitrageCombinationResponse, error) {
	s.got = in
	if s.err != nil {
		return nil, s.err
	}
	return &traderv1.CreateArbitrageCombinationResponse{
		Combination: &traderv1.ArbitrageCombination{
			Id:                                 "combo-1",
			EarlyExitFunding_8HAnnualizedFloor: in.GetEarlyExitFunding_8HAnnualizedFloor(),
		},
	}, nil
}

func TestCreateArbitrageCombinationPassesFunding8hFloor(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{name: "missing", body: `{"targetNotional":"10000","runMode":"one_shot"}`, want: ""},
		{name: "positive", body: `{"targetNotional":"10000","earlyExitFunding8hAnnualizedFloor":"0.05"}`, want: "0.05"},
		{name: "negative", body: `{"targetNotional":"10000","earlyExitFunding8hAnnualizedFloor":"-0.10"}`, want: "-0.10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubCreateTrader{}
			handler := &Handler{sessionCookieName: "sq_session", trader: stub}
			request := httptest.NewRequest(
				http.MethodPost,
				"/api/v1/trader/arbitrage-combinations",
				strings.NewReader(tc.body),
			)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "create-funding-8h")
			request.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
			recorder := httptest.NewRecorder()
			handler.createArbitrageCombination(recorder, request)
			if recorder.Code != http.StatusCreated {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if stub.got.GetEarlyExitFunding_8HAnnualizedFloor() != tc.want {
				t.Fatalf("rpc floor=%q want=%q", stub.got.GetEarlyExitFunding_8HAnnualizedFloor(), tc.want)
			}
			var payload map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			data := payload["data"].(map[string]any)
			if data["earlyExitFunding8hAnnualizedFloor"] != tc.want {
				t.Fatalf("json floor=%v want=%q", data["earlyExitFunding8hAnnualizedFloor"], tc.want)
			}
		})
	}
}

type capturingPlaceTrader struct {
	traderv1.TraderServiceClient
	got *traderv1.PlaceOrderRequest
	err error
}

func (s *capturingPlaceTrader) PlaceOrder(
	_ context.Context,
	in *traderv1.PlaceOrderRequest,
	_ ...grpc.CallOption,
) (*traderv1.PlaceOrderResponse, error) {
	s.got = in
	if s.err != nil {
		return nil, s.err
	}
	return &traderv1.PlaceOrderResponse{
		Order: &traderv1.Order{Id: "ord-1", IdempotencyKey: in.GetIdempotencyKey()},
	}, nil
}

type capturingTwapTrader struct {
	traderv1.TraderServiceClient
	got *traderv1.CreateTwapRequest
	err error
}

func (s *capturingTwapTrader) CreateTwap(
	_ context.Context,
	in *traderv1.CreateTwapRequest,
	_ ...grpc.CallOption,
) (*traderv1.CreateTwapResponse, error) {
	s.got = in
	if s.err != nil {
		return nil, s.err
	}
	return &traderv1.CreateTwapResponse{Job: &traderv1.TwapJob{Id: "twap-1"}}, nil
}

type capturingCloseTrader struct {
	traderv1.TraderServiceClient
}

func (s *capturingCloseTrader) CloseArbitrageCombination(
	_ context.Context,
	_ *traderv1.CloseArbitrageCombinationRequest,
	_ ...grpc.CallOption,
) (*traderv1.CloseArbitrageCombinationResponse, error) {
	return &traderv1.CloseArbitrageCombinationResponse{
		Combination: &traderv1.ArbitrageCombination{Id: "combo-1", Status: "closing"},
	}, nil
}

func TestGatewayIdempotencyKeyValidationAndForwarding(t *testing.T) {
	stub := &capturingPlaceTrader{}
	handler := &Handler{sessionCookieName: "sq_session", trader: stub}
	missing := httptest.NewRequest(
		http.MethodPost, "/api/v1/trader/orders",
		strings.NewReader(`{"tradingAccountId":1,"instrumentId":2,"side":"buy","orderType":"limit","quantity":"1","price":"10"}`),
	)
	missing.Header.Set("Content-Type", "application/json")
	missing.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
	recorder := httptest.NewRecorder()
	handler.placeTraderOrder(recorder, missing)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "idempotency key is required") {
		t.Fatalf("missing key status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	headerKey := "11111111-1111-1111-1111-111111111111"
	withHeader := httptest.NewRequest(
		http.MethodPost, "/api/v1/trader/orders",
		strings.NewReader(`{"tradingAccountId":1,"instrumentId":2,"side":"buy","orderType":"limit","quantity":"1","price":"10"}`),
	)
	withHeader.Header.Set("Content-Type", "application/json")
	withHeader.Header.Set("Idempotency-Key", headerKey)
	withHeader.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
	recorder = httptest.NewRecorder()
	handler.placeTraderOrder(recorder, withHeader)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("header key status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if stub.got.GetIdempotencyKey() != headerKey {
		t.Fatalf("forwarded=%q", stub.got.GetIdempotencyKey())
	}

	bodyKey := "22222222-2222-2222-2222-222222222222"
	withBody := httptest.NewRequest(
		http.MethodPost, "/api/v1/trader/orders",
		strings.NewReader(`{"tradingAccountId":1,"instrumentId":2,"side":"buy","orderType":"limit","quantity":"1","price":"10","idempotencyKey":"`+bodyKey+`"}`),
	)
	withBody.Header.Set("Content-Type", "application/json")
	withBody.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
	recorder = httptest.NewRecorder()
	handler.placeTraderOrder(recorder, withBody)
	if recorder.Code != http.StatusAccepted || stub.got.GetIdempotencyKey() != bodyKey {
		t.Fatalf("body key status=%d forwarded=%q", recorder.Code, stub.got.GetIdempotencyKey())
	}

	stub.err = status.Error(codes.AlreadyExists, "idempotency key conflict")
	conflict := httptest.NewRequest(
		http.MethodPost, "/api/v1/trader/orders",
		strings.NewReader(`{"tradingAccountId":1,"instrumentId":2,"side":"buy","orderType":"limit","quantity":"1","price":"10"}`),
	)
	conflict.Header.Set("Content-Type", "application/json")
	conflict.Header.Set("Idempotency-Key", headerKey)
	conflict.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
	recorder = httptest.NewRecorder()
	handler.placeTraderOrder(recorder, conflict)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestGatewayTwapAndArbitrageIdempotencyMapping(t *testing.T) {
	twap := &capturingTwapTrader{}
	handler := &Handler{sessionCookieName: "sq_session", trader: twap}
	missing := httptest.NewRequest(
		http.MethodPost, "/api/v1/trader/twaps",
		strings.NewReader(`{"tradingAccountId":1,"instrumentId":2,"side":"buy","totalQuantity":"1","startAt":"2026-01-01T00:00:00Z","endAt":"2026-01-01T01:00:00Z","intervalSeconds":60,"executionType":"market"}`),
	)
	missing.Header.Set("Content-Type", "application/json")
	missing.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
	recorder := httptest.NewRecorder()
	handler.createTraderTwap(recorder, missing)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "idempotency key is required") {
		t.Fatalf("twap missing key status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	key := "33333333-3333-3333-3333-333333333333"
	ok := httptest.NewRequest(
		http.MethodPost, "/api/v1/trader/twaps",
		strings.NewReader(`{"tradingAccountId":1,"instrumentId":2,"side":"buy","totalQuantity":"1","startAt":"2026-01-01T00:00:00Z","endAt":"2026-01-01T01:00:00Z","intervalSeconds":60,"executionType":"market"}`),
	)
	ok.Header.Set("Content-Type", "application/json")
	ok.Header.Set("Idempotency-Key", key)
	ok.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
	recorder = httptest.NewRecorder()
	handler.createTraderTwap(recorder, ok)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("twap create status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if twap.got.GetIdempotencyKey() != key {
		t.Fatalf("twap forwarded=%q", twap.got.GetIdempotencyKey())
	}

	twap.err = status.Error(codes.AlreadyExists, "idempotency key conflict")
	conflict := httptest.NewRequest(
		http.MethodPost, "/api/v1/trader/twaps",
		strings.NewReader(`{"tradingAccountId":1,"instrumentId":2,"side":"buy","totalQuantity":"1","startAt":"2026-01-01T00:00:00Z","endAt":"2026-01-01T01:00:00Z","intervalSeconds":60,"executionType":"market"}`),
	)
	conflict.Header.Set("Content-Type", "application/json")
	conflict.Header.Set("Idempotency-Key", key)
	conflict.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
	recorder = httptest.NewRecorder()
	handler.createTraderTwap(recorder, conflict)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("twap conflict status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	arb := &stubCreateTrader{}
	arbHandler := &Handler{sessionCookieName: "sq_session", trader: arb}
	arbMissing := httptest.NewRequest(
		http.MethodPost, "/api/v1/trader/arbitrage-combinations",
		strings.NewReader(`{"targetNotional":"10000"}`),
	)
	arbMissing.Header.Set("Content-Type", "application/json")
	arbMissing.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
	recorder = httptest.NewRecorder()
	arbHandler.createArbitrageCombination(recorder, arbMissing)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "idempotency key is required") {
		t.Fatalf("arb missing key status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	arbOK := httptest.NewRequest(
		http.MethodPost, "/api/v1/trader/arbitrage-combinations",
		strings.NewReader(`{"legAAccountId":1,"legAInstrumentId":11,"legBAccountId":2,"legBInstrumentId":22,"askThresholdBps":"12","bidThresholdBps":"-8","targetNotional":"10000","executionMode":"maker_then_hedge","preferredLeg":"a","runMode":"spread"}`),
	)
	arbOK.Header.Set("Content-Type", "application/json")
	arbOK.Header.Set("Idempotency-Key", key)
	arbOK.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
	recorder = httptest.NewRecorder()
	arbHandler.createArbitrageCombination(recorder, arbOK)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("arb create status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if arb.got.GetIdempotencyKey() != key {
		t.Fatalf("arb forwarded=%q", arb.got.GetIdempotencyKey())
	}

	arb.err = status.Error(codes.AlreadyExists, "idempotency key conflict")
	arbConflict := httptest.NewRequest(
		http.MethodPost, "/api/v1/trader/arbitrage-combinations",
		strings.NewReader(`{"legAAccountId":1,"legAInstrumentId":11,"legBAccountId":2,"legBInstrumentId":22,"askThresholdBps":"12","bidThresholdBps":"-8","targetNotional":"10000","executionMode":"maker_then_hedge","preferredLeg":"a","runMode":"spread"}`),
	)
	arbConflict.Header.Set("Content-Type", "application/json")
	arbConflict.Header.Set("Idempotency-Key", key)
	arbConflict.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
	recorder = httptest.NewRecorder()
	arbHandler.createArbitrageCombination(recorder, arbConflict)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("arb conflict status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	closeHandler := &Handler{sessionCookieName: "sq_session", trader: &capturingCloseTrader{}}
	closeReq := httptest.NewRequest(http.MethodDelete, "/api/v1/trader/arbitrage-combinations/combo-1", nil)
	closeReq.AddCookie(&http.Cookie{Name: "sq_session", Value: "token"})
	recorder = httptest.NewRecorder()
	closeHandler.closeArbitrageCombination(recorder, closeReq)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("close status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("close must set Cache-Control no-store")
	}
	if !strings.Contains(recorder.Body.String(), `"status":"closing"`) {
		t.Fatalf("close body=%s", recorder.Body.String())
	}
}
