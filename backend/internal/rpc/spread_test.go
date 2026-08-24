package rpc

import (
	"context"
	"testing"
	"time"

	shopspring "github.com/shopspring/decimal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	spreadv1 "selfquant/backend/gen/spread/v1"
	"selfquant/backend/internal/spread"
)

type stubSpreadService struct {
	last    spread.HistoryRequest
	history spread.History
	err     error
}

func (s *stubSpreadService) GetHistory(
	_ context.Context,
	request spread.HistoryRequest,
) (spread.History, error) {
	s.last = request
	return s.history, s.err
}

func TestSpreadServerMapsHistory(t *testing.T) {
	now := time.Date(2026, 8, 22, 7, 0, 0, 0, time.UTC)
	server := NewSpreadServer(&stubSpreadService{history: spread.History{
		Venue: "binance", BaseAsset: "BTC", QuoteAsset: "USDT",
		CanonicalSymbol: "BTCUSDT", Range: spread.Range1h, ResolutionSeconds: 5,
		Availability: spread.AvailabilityAvailable, AsOf: now,
		Points: []spread.Point{{
			TS: now, SpreadBps: shopspring.RequireFromString("12.5"),
			SpotAsk: shopspring.RequireFromString("100"), PerpetualAsk: shopspring.RequireFromString("100.125"),
			Samples: 2,
		}},
		Summary: spread.Summary{
			CurrentBps: shopspring.RequireFromString("12.5"),
			MinBps:     shopspring.RequireFromString("12.5"),
			MaxBps:     shopspring.RequireFromString("12.5"),
			AvgBps:     shopspring.RequireFromString("12.5"),
			Coverage:   shopspring.RequireFromString("0.01"),
		},
	}})
	response, err := server.GetBasisSpreadHistory(context.Background(), &spreadv1.GetBasisSpreadHistoryRequest{
		Venue: "binance", BaseAsset: "BTC", QuoteAsset: "USDT",
		Range: spreadv1.BasisSpreadRange_BASIS_SPREAD_RANGE_1H,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetAvailability() != spreadv1.BasisSpreadAvailability_BASIS_SPREAD_AVAILABILITY_AVAILABLE ||
		response.GetCanonicalSymbol() != "BTCUSDT" ||
		len(response.GetPoints()) != 1 ||
		response.GetPoints()[0].GetSpreadBps() != "12.5" {
		t.Fatalf("response=%v", response)
	}
}

func TestSpreadServerForwardsCompareVenue(t *testing.T) {
	stub := &stubSpreadService{history: spread.History{
		Venue: "binance", CompareVenue: "okx", BaseAsset: "BTC", QuoteAsset: "USDT",
		CanonicalSymbol: "BTCUSDT", Range: spread.Range24h, ResolutionSeconds: 60,
		Availability: spread.AvailabilityAvailable, AsOf: time.Date(2026, 8, 22, 7, 0, 0, 0, time.UTC),
	}}
	server := NewSpreadServer(stub)
	response, err := server.GetBasisSpreadHistory(context.Background(), &spreadv1.GetBasisSpreadHistoryRequest{
		Venue: "binance", CompareVenue: "okx", BaseAsset: "BTC", QuoteAsset: "USDT",
		Range: spreadv1.BasisSpreadRange_BASIS_SPREAD_RANGE_24H,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stub.last.CompareVenue != "okx" || response.GetCompareVenue() != "okx" {
		t.Fatalf("last=%+v response=%v", stub.last, response)
	}
}

func TestSpreadServerRejectsMissingRange(t *testing.T) {
	server := NewSpreadServer(&stubSpreadService{})
	_, err := server.GetBasisSpreadHistory(context.Background(), &spreadv1.GetBasisSpreadHistoryRequest{
		Venue: "binance", BaseAsset: "BTC", QuoteAsset: "USDT",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err=%v", err)
	}
}

func TestMapSpreadError(t *testing.T) {
	if status.Code(mapSpreadError(spread.ErrInvalidArgument)) != codes.InvalidArgument {
		t.Fatal("invalid argument mapping")
	}
	if status.Code(mapSpreadError(spread.ErrTimeout)) != codes.DeadlineExceeded {
		t.Fatal("timeout mapping")
	}
	if status.Code(mapSpreadError(spread.ErrQueryFailed)) != codes.Unavailable {
		t.Fatal("query mapping")
	}
}
