package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
		OrderNotional: "500", MaxDeltaNotional: "100", CompletedNotional: "0",
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
		MarketDataStale: true,
	})
	if result["productName"] != "ARBITRAGE" || result["preferredLeg"] != "a" {
		t.Fatalf("result=%v", result)
	}
	if result["askSpreadBps"] != nil || result["bidSpreadBps"] != nil {
		t.Fatalf("missing spreads must be null: %v", result)
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
