package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	fundingv1 "selfquant/backend/gen/funding/v1"
)

type tradefiOnlyFundingServer struct {
	fundingv1.UnimplementedFundingServiceServer
	calls int
}

func (s *tradefiOnlyFundingServer) ListTradefiPerpsOpportunities(
	context.Context, *fundingv1.ListTradefiPerpsOpportunitiesRequest,
) (*fundingv1.ListTradefiPerpsOpportunitiesResponse, error) {
	s.calls++
	now := time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC)
	return &fundingv1.ListTradefiPerpsOpportunitiesResponse{
		Total: 1, SnapshotVersion: "tradefi-1", ServerTime: timestamppb.New(now),
		Items: []*fundingv1.TradefiPerpsOpportunity{{
			StockSymbol: "AAPL",
			VerifiedAt:  timestamppb.New(now),
			Spot: &fundingv1.TradefiSpotLeg{
				Exchange: "ibkr", Symbol: "AAPL", ConId: 265598, PrimaryExchange: "NASDAQ",
			},
			Perpetual: &fundingv1.TradefiPerpLeg{
				Exchange: "binance", ExchangeSymbol: "AAPLUSDT", BaseAsset: "AAPL", QuoteAsset: "USDT",
				FundingRate: "0.0001", PerpetualFundingAnnualized: "0.11",
				FundingIntervalSeconds: 28800, NextFundingAt: timestamppb.New(now),
				Volume_24HBase: "1", Turnover_24HUsd: "2",
				PositionQuantity: "3", PositionNotionalUsd: "4",
				Cumulative_24H: "0.001", Cumulative_7D: "0.01",
				SourceUpdatedAt: timestamppb.New(now), Stale: true,
			},
		}},
	}, nil
}

func TestTradefiHTTPReadsPublishedSnapshot(t *testing.T) {
	server := &tradefiOnlyFundingServer{}
	router := testRouter(t, server, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/tradefi-perps-opportunities", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, needle := range []string{`"stockSymbol":"AAPL"`, `"perpetualFundingAnnualized":"0.11"`, `"conId":"265598"`, `"cumulative24h":"0.001"`, `"stale":true`} {
		if !strings.Contains(body, needle) {
			t.Fatalf("body missing %s: %s", needle, body)
		}
	}
	if strings.Contains(body, "markPrice") || strings.Contains(body, "lastPrice") {
		t.Fatalf("price leaked: %s", body)
	}
	if server.calls != 1 {
		t.Fatalf("calls=%d", server.calls)
	}
}
