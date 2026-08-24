package rpc

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	fundingv1 "selfquant/backend/gen/funding/v1"
	"selfquant/backend/internal/funding"
	"selfquant/backend/internal/funding/ranking"
)

type fakeHistoryRepository struct {
	points []funding.HistoryPoint
}

func (r *fakeHistoryRepository) ListHistory(
	context.Context,
	string,
	string,
	int,
) ([]funding.HistoryPoint, error) {
	return r.points, nil
}

func TestFundingServerReadsSnapshot(t *testing.T) {
	store := funding.NewSnapshotStore()
	store.Replace([]funding.Rate{{
		Exchange:        "test",
		ExchangeSymbol:  "BTCUSDT",
		GlobalSymbol:    "BTCUSDT",
		SourceUpdatedAt: time.Now().UTC(),
		History: []funding.HistoryPoint{{
			Rate: 0.0001, SettledAt: time.Now().UTC().Add(-8 * time.Hour),
		}},
	}}, 1, time.Unix(100, 0))
	server := NewFundingServer(store, nil, 20*time.Minute)
	response, err := server.ListFundingRates(
		context.Background(), &fundingv1.ListFundingRatesRequest{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.Total != 1 || len(response.Items) != 1 ||
		response.Items[0].ExchangeSymbol != "BTCUSDT" ||
		response.SnapshotVersion == "" || response.Items[0].Stale ||
		len(response.Items[0].History) != 0 {
		t.Fatalf("response=%+v", response)
	}
}

func TestFundingServerReadsHistoryOnDemand(t *testing.T) {
	store := funding.NewSnapshotStore()
	settledAt := time.Now().UTC().Add(-8 * time.Hour)
	server := NewFundingServer(store, &fakeHistoryRepository{
		points: []funding.HistoryPoint{{Rate: 0.0001, SettledAt: settledAt}},
	}, 20*time.Minute)
	response, err := server.GetFundingHistory(
		context.Background(),
		&fundingv1.GetFundingHistoryRequest{
			Exchange: "binance", ExchangeSymbol: "BTCUSDT", Limit: 10,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 || response.Items[0].Rate != "0.0001" ||
		!response.Items[0].SettledAt.AsTime().Equal(settledAt) {
		t.Fatalf("response=%+v", response)
	}
}

func TestFundingServerBuildsSpreadSnapshot(t *testing.T) {
	store := funding.NewSnapshotStore()
	now := time.Now().UTC()
	store.Replace([]funding.Rate{
		{
			Exchange: "binance", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Rate: 0.0001, IntervalHours: 8,
			PositionNotionalUSD: 5_000_000, Turnover24hUSD: 20_000_000,
			LastPrice: 60_000, FundingTime: now.Add(time.Hour), SourceUpdatedAt: now,
		},
		{
			Exchange: "okx", ExchangeSymbol: "BTC-USDT-SWAP", GlobalSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Rate: 0.0002, IntervalHours: 8,
			PositionNotionalUSD: 4_000_000, Turnover24hUSD: 30_000_000,
			LastPrice: 60_010, FundingTime: now.Add(time.Hour), SourceUpdatedAt: now,
		},
	}, 2, time.Unix(100, 0))
	server := NewFundingServer(store, nil, 20*time.Minute)
	response, err := server.ListFundingSpreads(
		context.Background(), &fundingv1.ListFundingSpreadsRequest{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetTotal() != 1 || len(response.GetItems()) != 1 {
		t.Fatalf("response=%+v", response)
	}
	item := response.GetItems()[0]
	if item.GetBaseAsset() != "BTC" || item.GetLongLeg().GetExchange() != "binance" ||
		item.GetShortLeg().GetExchange() != "okx" ||
		item.GetMinPositionNotionalUsd() != "4000000" ||
		item.GetLongLeg().GetLastPrice() != "60000" || item.GetStale() {
		t.Fatalf("spread=%+v", item)
	}
}

func TestFundingServerFiltersRankingSnapshot(t *testing.T) {
	fundingStore := funding.NewSnapshotStore()
	rankingStore := ranking.NewSnapshotStore()
	now := time.Now().UTC()
	rankingStore.Replace(ranking.Period1h, []ranking.Opportunity{
		{
			GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
			Period: ranking.Period1h,
			Long: ranking.Leg{
				Exchange: "binance", ExchangeSymbol: "BTCUSDT",
				IntervalHours: 8, NextFundingAt: now.Add(time.Hour), SourceUpdatedAt: now,
			},
			Short: ranking.Leg{
				Exchange: "okx", ExchangeSymbol: "BTC-USDT-SWAP",
				IntervalHours: 8, NextFundingAt: now.Add(time.Hour), SourceUpdatedAt: now,
			},
			CombinedExpectedAnnualized: 1.2, MinPositionNotionalUSD: 2_000_000,
			MinTurnover24hUSD: 3_000_000, ModelState: "ready", UpdatedAt: now,
		},
		{
			GlobalSymbol: "ETHUSDT", Period: ranking.Period1h,
			MinPositionNotionalUSD: 100, MinTurnover24hUSD: 100,
		},
	}, now)
	server := NewFundingServer(fundingStore, nil, 20*time.Minute, rankingStore)
	response, err := server.ListFundingOpportunities(
		context.Background(),
		&fundingv1.ListFundingOpportunitiesRequest{
			Period: "1h", MinLegNotionalUsd: "1000000", MinLegVolume_24HUsd: "1000000",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetTotal() != 1 || len(response.GetItems()) != 1 {
		t.Fatalf("response=%+v", response)
	}
	item := response.GetItems()[0]
	if item.GetGlobalSymbol() != "BTCUSDT" || item.GetRank() != 1 ||
		item.GetLongLeg().GetExchange() != "binance" {
		t.Fatalf("item=%+v", item)
	}
	if response.GetStatus() != "ready" || response.GetGeneration() == 0 ||
		response.GetLastSuccessfulAt() == nil || response.GetDataThrough() == nil {
		t.Fatalf("snapshot metadata=%+v", response)
	}
}

func TestListFundingOpportunitiesHonorsCanceledContext(t *testing.T) {
	server := NewFundingServer(funding.NewSnapshotStore(), nil, time.Minute, ranking.NewSnapshotStore())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := server.ListFundingOpportunities(
		ctx, &fundingv1.ListFundingOpportunitiesRequest{Period: "1h"},
	)
	if status.Code(err) != codes.Canceled {
		t.Fatalf("error=%v", err)
	}
}

func BenchmarkListFundingOpportunitiesMemory(b *testing.B) {
	rankings := ranking.NewSnapshotStore()
	now := time.Now().UTC()
	items := make([]ranking.Opportunity, 1000)
	for index := range items {
		items[index] = ranking.Opportunity{
			Rank: index + 1, GlobalSymbol: "BTCUSDT", Period: ranking.Period1h,
			Long: ranking.Leg{
				Exchange: "binance", SourceUpdatedAt: now, PositionNotionalUSD: 1_000_000,
			},
			Short: ranking.Leg{
				Exchange: "okx", SourceUpdatedAt: now, PositionNotionalUSD: 1_000_000,
			},
			MinPositionNotionalUSD: 1_000_000, MinTurnover24hUSD: 1_000_000,
			UpdatedAt: now,
		}
	}
	rankings.Replace(ranking.Period1h, items, now)
	server := NewFundingServer(funding.NewSnapshotStore(), nil, time.Minute, rankings)
	request := &fundingv1.ListFundingOpportunitiesRequest{Period: "1h", Limit: 200}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := server.ListFundingOpportunities(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}
