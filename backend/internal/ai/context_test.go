package ai

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
	fundingv1 "selfquant/backend/gen/funding/v1"
)

type fakeFundingSource struct {
	response *fundingv1.ListFundingRatesResponse
	err      error
}

func (f fakeFundingSource) ListFundingRates(
	context.Context,
	*fundingv1.ListFundingRatesRequest,
	...grpc.CallOption,
) (*fundingv1.ListFundingRatesResponse, error) {
	return f.response, f.err
}

func TestFundingContextFiltersStaleAndMatchesAsset(t *testing.T) {
	now := timestamppb.New(time.Now().UTC())
	source := fakeFundingSource{response: &fundingv1.ListFundingRatesResponse{
		SnapshotVersion: "snapshot-1",
		ServerTime:      now,
		Items: []*fundingv1.FundingRate{
			{
				Exchange: "binance", GlobalSymbol: "BTCUSDT", BaseAsset: "BTC",
				FundingRate: "0.001", AnnualizedRate: "1.2", Turnover_24HUsd: "100",
				SourceUpdatedAt: now,
			},
			{
				Exchange: "okx", GlobalSymbol: "ETHUSDT", BaseAsset: "ETH",
				FundingRate: "0.9", AnnualizedRate: "9", Turnover_24HUsd: "999999999",
				SourceUpdatedAt: now, Stale: true,
			},
		},
	}}
	provider := NewFundingContextProvider(source, 24, 40, 5_000_000, 8000)
	result, err := provider.Build(context.Background(), "分析 BTC 资金费")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "BTCUSDT") || strings.Contains(result, "ETHUSDT") ||
		!strings.Contains(result, "snapshot-1") {
		t.Fatalf("context=%s", result)
	}
}

func TestFundingContextAppliesTurnoverAndCharacterLimits(t *testing.T) {
	now := timestamppb.Now()
	source := fakeFundingSource{response: &fundingv1.ListFundingRatesResponse{
		ServerTime: now,
		Items: []*fundingv1.FundingRate{
			{
				Exchange: "binance", GlobalSymbol: "LOWUSDT", BaseAsset: "LOW",
				FundingRate: "1", Turnover_24HUsd: "100", SourceUpdatedAt: now,
			},
			{
				Exchange: "okx", GlobalSymbol: "HIGHUSDT", BaseAsset: "HIGH",
				FundingRate: "0.01", Turnover_24HUsd: "9000000", SourceUpdatedAt: now,
			},
		},
	}}
	provider := NewFundingContextProvider(source, 24, 40, 5_000_000, 300)
	result, err := provider.Build(context.Background(), "当前最高机会")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result, "LOWUSDT") || len(result) > 300 {
		t.Fatalf("len=%d context=%s", len(result), result)
	}
}

func TestFundingContextExplainsEmptyFreshSnapshot(t *testing.T) {
	provider := NewFundingContextProvider(
		fakeFundingSource{response: &fundingv1.ListFundingRatesResponse{
			Items: []*fundingv1.FundingRate{{BaseAsset: "BTC", Stale: true}},
		}},
		24, 40, 5_000_000, 8000,
	)
	result, err := provider.Build(context.Background(), "BTC")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "当前无新鲜") {
		t.Fatalf("context=%s", result)
	}
}
