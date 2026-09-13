package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
}

func TestUpdateArbitrageCombinationRejectsUnknownAndEmptyPayloads(t *testing.T) {
	handler := &Handler{sessionCookieName: "sq_session"}
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown field", body: `{"targetNotional":"1000","unexpected":true}`},
		{name: "empty update", body: `{}`},
		{name: "order notional", body: `{"targetNotional":"1000","orderNotional":"500"}`},
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
}

func (s *stubCreateTrader) CreateArbitrageCombination(
	_ context.Context,
	in *traderv1.CreateArbitrageCombinationRequest,
	_ ...grpc.CallOption,
) (*traderv1.CreateArbitrageCombinationResponse, error) {
	s.got = in
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
