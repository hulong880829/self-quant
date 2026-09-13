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

func TestFundingServerHidesUnpublishedVenues(t *testing.T) {
	store := funding.NewSnapshotStore()
	now := time.Now().UTC()
	store.Replace([]funding.Rate{
		{Exchange: "binance", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT", SourceUpdatedAt: now, IntervalHours: 8},
		{Exchange: "aster", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT", SourceUpdatedAt: now, IntervalHours: 8},
	}, 2, now)
	server := NewFundingServer(store, nil, 20*time.Minute).WithPublication(
		map[string]bool{"binance": true},
		map[string]bool{"binance": true},
	)
	rates, err := server.ListFundingRates(context.Background(), &fundingv1.ListFundingRatesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if rates.GetTotal() != 1 || rates.GetItems()[0].GetExchange() != "binance" {
		t.Fatalf("published filter=%+v", rates)
	}
	spreads, err := server.ListFundingSpreads(context.Background(), &fundingv1.ListFundingSpreadsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if spreads.GetTotal() != 0 {
		t.Fatalf("unpublished aster must not appear in spreads: %+v", spreads)
	}
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
		response.Items[0].VenueContractType != "PERPETUAL" ||
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
		item.GetLongLeg().GetGlobalSymbol() != "BTCUSDT" ||
		item.GetLongLeg().GetQuoteAsset() != "USDT" ||
		item.GetMinPositionNotionalUsd() != "4000000" ||
		item.GetLongLeg().GetLastPrice() != "60000" || item.GetStale() {
		t.Fatalf("spread=%+v", item)
	}
}

func TestFundingServerFiltersRankingSnapshot(t *testing.T) {
	fundingStore := funding.NewSnapshotStore()
	rankingStore := ranking.NewSnapshotStore()
	now := time.Now().UTC()
	rankingStore.Replace(ranking.Period8h, []ranking.Opportunity{
		{
			GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
			Period: ranking.Period8h,
			Long: ranking.Leg{
				Exchange: "binance", ExchangeSymbol: "BTCUSDT",
				IntervalHours: 8, NextFundingAt: now.Add(time.Hour), SourceUpdatedAt: now,
			},
			Short: ranking.Leg{
				Exchange: "okx", ExchangeSymbol: "BTC-USDT-SWAP",
				IntervalHours: 8, NextFundingAt: now.Add(time.Hour), SourceUpdatedAt: now,
			},
			CombinedExpectedAnnualized: 1.2, MinPositionNotionalUSD: 2_000_000,
			MinTurnover24hUSD: 3_000_000, ModelState: "replay_7d",
			SampleCount: 120, ExpectedPaybackMinutes: 32.5,
			PaybackStatus: ranking.PaybackReady, UpdatedAt: now,
		},
		{
			GlobalSymbol: "ETHUSDT", Period: ranking.Period8h,
			MinPositionNotionalUSD: 100, MinTurnover24hUSD: 100,
		},
	}, now)
	server := NewFundingServer(fundingStore, nil, 20*time.Minute, rankingStore)
	response, err := server.ListFundingOpportunities(
		context.Background(),
		&fundingv1.ListFundingOpportunitiesRequest{
			Period: "8h", MinLegNotionalUsd: "1000000", MinLegVolume_24HUsd: "1000000",
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
		item.GetLongLeg().GetExchange() != "binance" ||
		item.GetSampleCount() != 120 ||
		item.GetExpectedPaybackMinutes() != "32.5" ||
		item.GetPaybackStatus() != "ready" {
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
		ctx, &fundingv1.ListFundingOpportunitiesRequest{Period: "8h"},
	)
	if status.Code(err) != codes.Canceled {
		t.Fatalf("error=%v", err)
	}
}

func TestFundingOpportunityContractDefaultsToEightHoursAndCapsOneHundred(t *testing.T) {
	rankings := ranking.NewSnapshotStore()
	now := time.Now().UTC()
	items := make([]ranking.Opportunity, 150)
	for index := range items {
		items[index] = ranking.Opportunity{
			Rank: index + 1, GlobalSymbol: "BTCUSDT", Period: ranking.Period8h,
			Long:      ranking.Leg{Exchange: "binance", SourceUpdatedAt: now},
			Short:     ranking.Leg{Exchange: "okx", SourceUpdatedAt: now},
			UpdatedAt: now,
		}
	}
	rankings.Replace(ranking.Period8h, items, now)
	server := NewFundingServer(funding.NewSnapshotStore(), nil, time.Minute, rankings)
	response, err := server.ListFundingOpportunities(
		context.Background(),
		&fundingv1.ListFundingOpportunitiesRequest{Limit: 1000},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.GetItems()) != 100 {
		t.Fatalf("items=%d", len(response.GetItems()))
	}
	if _, err := server.ListFundingOpportunities(
		context.Background(),
		&fundingv1.ListFundingOpportunitiesRequest{Period: "1h"},
	); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("legacy period error=%v", err)
	}
	for _, invalid := range []string{"NaN", "+Inf", "-Inf"} {
		if _, err := server.ListFundingOpportunities(
			context.Background(),
			&fundingv1.ListFundingOpportunitiesRequest{
				Period: "8h", MinLegNotionalUsd: invalid,
			},
		); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("filter %q error=%v", invalid, err)
		}
	}
}

func BenchmarkListFundingOpportunitiesMemory(b *testing.B) {
	rankings := ranking.NewSnapshotStore()
	now := time.Now().UTC()
	items := make([]ranking.Opportunity, 100)
	for index := range items {
		items[index] = ranking.Opportunity{
			Rank: index + 1, GlobalSymbol: "BTCUSDT", Period: ranking.Period8h,
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
	rankings.Replace(ranking.Period8h, items, now)
	server := NewFundingServer(funding.NewSnapshotStore(), nil, time.Minute, rankings)
	request := &fundingv1.ListFundingOpportunitiesRequest{Period: "8h", Limit: 100}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := server.ListFundingOpportunities(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}

type recordingCurrentRates struct {
	fakeHistoryRepository
	calls int
	keys  []funding.RateLookupKey
	rates []*funding.Rate
	err   error
}

func (r *recordingCurrentRates) ListCurrentByKeys(
	_ context.Context,
	keys []funding.RateLookupKey,
) ([]*funding.Rate, error) {
	r.calls++
	r.keys = append([]funding.RateLookupKey(nil), keys...)
	if r.err != nil {
		return nil, r.err
	}
	if r.rates != nil {
		return r.rates, nil
	}
	return make([]*funding.Rate, len(keys)), nil
}

func TestBatchGetFundingRatesHitsMemoryWithoutDB(t *testing.T) {
	store := funding.NewSnapshotStore()
	now := time.Now().UTC()
	store.Replace([]funding.Rate{{
		Exchange: "aster", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", Cumulative24h: 0.003, IntervalHours: 8,
		SourceUpdatedAt: now,
	}}, 1, now)
	repo := &recordingCurrentRates{}
	server := NewFundingServer(store, repo, 20*time.Minute).WithPublication(
		map[string]bool{"binance": true},
		map[string]bool{"binance": true},
	)
	response, err := server.BatchGetFundingRates(context.Background(), &fundingv1.BatchGetFundingRatesRequest{
		Keys: []*fundingv1.FundingRateLookupKey{{
			Exchange: "aster", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if repo.calls != 0 {
		t.Fatalf("memory hit must not query db: calls=%d", repo.calls)
	}
	if len(response.GetResults()) != 1 || response.GetResults()[0].GetStatus() != funding.RateLookupHit {
		t.Fatalf("response=%+v", response)
	}
	if response.GetResults()[0].GetItem().GetExchange() != "aster" {
		t.Fatalf("unpublished aster must still hit: %+v", response.GetResults()[0])
	}
}

func TestBatchGetFundingRatesLoadsEntireBatchFromDBOnAnyMiss(t *testing.T) {
	store := funding.NewSnapshotStore()
	now := time.Now().UTC()
	store.Replace([]funding.Rate{{
		Exchange: "binance", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", Cumulative24h: 0.001, IntervalHours: 8,
		SourceUpdatedAt: now,
	}}, 1, now)
	repo := &recordingCurrentRates{rates: []*funding.Rate{
		{Exchange: "binance", ExchangeSymbol: "BTCUSDT", Cumulative24h: 0.009},
		{Exchange: "okx", ExchangeSymbol: "BTC-USDT-SWAP", Cumulative24h: 0.008},
	}}
	server := NewFundingServer(store, repo, 20*time.Minute)
	response, err := server.BatchGetFundingRates(context.Background(), &fundingv1.BatchGetFundingRatesRequest{
		Keys: []*fundingv1.FundingRateLookupKey{
			{Exchange: "binance", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT"},
			{Exchange: "okx", ExchangeSymbol: "BTC-USDT-SWAP", BaseAsset: "BTC", QuoteAsset: "USDT"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if repo.calls != 1 || len(repo.keys) != 2 {
		t.Fatalf("any miss must query all keys: calls=%d keys=%+v", repo.calls, repo.keys)
	}
	if response.GetResults()[0].GetItem().GetCumulative_24H() != "0.009" {
		t.Fatalf("db generation must replace memory hit: %+v", response.GetResults()[0])
	}
	if response.GetResults()[1].GetStatus() != funding.RateLookupHit {
		t.Fatalf("second key=%+v", response.GetResults()[1])
	}
}

func TestBatchGetFundingRatesPrefersExactOverHigherAnnualizedFallback(t *testing.T) {
	store := funding.NewSnapshotStore()
	now := time.Now().UTC()
	store.Replace([]funding.Rate{
		{
			Exchange: "binance", ExchangeSymbol: "BTCUSDC", BaseAsset: "BTC", QuoteAsset: "USDT",
			AnnualizedRate: 0.9, Cumulative24h: 0.01, IntervalHours: 8, SourceUpdatedAt: now,
		},
		{
			Exchange: "binance", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
			AnnualizedRate: 0.1, Cumulative24h: 0.002, IntervalHours: 8, SourceUpdatedAt: now,
		},
	}, 2, now)
	server := NewFundingServer(store, nil, 20*time.Minute)
	response, err := server.BatchGetFundingRates(context.Background(), &fundingv1.BatchGetFundingRatesRequest{
		Keys: []*fundingv1.FundingRateLookupKey{{
			Exchange: "binance", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := response.GetResults()[0].GetItem()
	if item.GetExchangeSymbol() != "BTCUSDT" || item.GetCumulative_24H() != "0.002" {
		t.Fatalf("exact must win: %+v", item)
	}
}

func TestBatchGetFundingRatesRejectsEmptyAndOversizedKeys(t *testing.T) {
	server := NewFundingServer(funding.NewSnapshotStore(), nil, time.Minute)
	if _, err := server.BatchGetFundingRates(context.Background(), &fundingv1.BatchGetFundingRatesRequest{}); err == nil ||
		status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty keys err=%v", err)
	}
	keys := make([]*fundingv1.FundingRateLookupKey, funding.MaxRateLookupKeys+1)
	for index := range keys {
		keys[index] = &fundingv1.FundingRateLookupKey{Exchange: "binance", ExchangeSymbol: "BTCUSDT"}
	}
	if _, err := server.BatchGetFundingRates(context.Background(), &fundingv1.BatchGetFundingRatesRequest{
		Keys: keys,
	}); err == nil || status.Code(err) != codes.InvalidArgument {
		t.Fatalf("oversized keys err=%v", err)
	}
}

func TestBatchGetFundingRatesDBErrorIsNotMissing(t *testing.T) {
	store := funding.NewSnapshotStore()
	repo := &recordingCurrentRates{err: context.DeadlineExceeded}
	server := NewFundingServer(store, repo, time.Minute)
	_, err := server.BatchGetFundingRates(context.Background(), &fundingv1.BatchGetFundingRatesRequest{
		Keys: []*fundingv1.FundingRateLookupKey{{Exchange: "binance", ExchangeSymbol: "BTCUSDT"}},
	})
	if err == nil || status.Code(err) != codes.Internal {
		t.Fatalf("db error must not become missing: %v", err)
	}
	if repo.calls != 1 {
		t.Fatalf("db calls=%d", repo.calls)
	}
}

