package ranking

import (
	"context"
	"fmt"
	"testing"
	"time"

	"selfquant/backend/internal/funding"
)

type fakeHistoryStore struct {
	quotes []Quote
	err    error
}

type fakeFundingHistoryStore struct {
	now time.Time
}

func (s fakeFundingHistoryStore) ListSettledHistoryRange(
	_ context.Context,
	_, _ time.Time,
	keys []funding.HistoryKey,
) (map[funding.HistoryKey][]funding.HistoryPoint, error) {
	result := make(map[funding.HistoryKey][]funding.HistoryPoint, len(keys))
	for _, key := range keys {
		result[key] = []funding.HistoryPoint{
			{Rate: 0.0001, SettledAt: s.now.Add(-3 * time.Hour)},
			{Rate: 0.0001, SettledAt: s.now.Add(-2 * time.Hour)},
			{Rate: 0.0001, SettledAt: s.now.Add(-time.Hour)},
		}
	}
	return result, nil
}

func (s *fakeHistoryStore) Query(
	context.Context,
	time.Time,
	time.Time,
	[]string,
	[]string,
) ([]Quote, error) {
	return s.quotes, s.err
}

func (s *fakeHistoryStore) Close() error { return nil }

func (s *fakeHistoryStore) SnapshotRange(
	from, to time.Time, pairs []HistoryPair,
) (map[HistoryPair]MinuteSeriesView, *HistoryGeneration, error) {
	if s.err != nil {
		return nil, nil, s.err
	}
	series := make(map[HistoryPair]*LegSeries, len(pairs))
	var dataThrough time.Time
	for _, pair := range pairs {
		var values []MinuteQuote
		for _, quote := range s.quotes {
			if quote.Venue != pair.Venue ||
				(quote.Symbol != pair.CanonicalSymbol && quote.Symbol != pair.SourceSymbol) ||
				quote.TS.Before(from) || !quote.TS.Before(to) {
				continue
			}
			values = append(values, MinuteQuote{
				Minute: quote.TS.Unix() / 60, Bid: quote.Bid, Ask: quote.Ask,
			})
			if quote.TS.After(dataThrough) {
				dataThrough = quote.TS
			}
		}
		if len(values) > 0 {
			series[pair] = newLegSeries(appendBlocks(
				nil, normalizeMinutes(values, from.Unix()/60), replayQuoteBlockSize,
			))
		}
	}
	generation := &HistoryGeneration{
		ID: 1, replay: series, ReplaySlots: tierSlots(series),
		Rows: tierRows(series), Slots: tierSlots(series), DataThrough: dataThrough,
	}
	views := make(map[HistoryPair]MinuteSeriesView, len(series))
	for pair, value := range series {
		views[pair] = MinuteSeriesView{
			series: value, fromMinute: from.Unix() / 60, toMinute: to.Unix() / 60,
		}
	}
	return views, generation, nil
}

func (s *fakeHistoryStore) QueryPairs(
	ctx context.Context,
	from, to time.Time,
	pairs []HistoryPair,
	consume func(HistoryQuote) error,
) error {
	if s.err != nil {
		return s.err
	}
	for _, pair := range pairs {
		for _, quote := range s.quotes {
			if quote.Venue != pair.Venue ||
				(quote.Symbol != pair.CanonicalSymbol && quote.Symbol != pair.SourceSymbol) ||
				quote.TS.Before(from) || !quote.TS.Before(to) {
				continue
			}
			if err := consume(HistoryQuote{
				Pair: pair,
				Quote: MinuteQuote{
					Minute: quote.TS.Unix() / 60, Bid: quote.Bid, Ask: quote.Ask,
				},
			}); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (s *fakeHistoryStore) QueryWarmPairs(
	ctx context.Context,
	from, to time.Time,
	pairs []HistoryPair,
	consume func(HistoryQuote) error,
) error {
	return s.QueryPairs(ctx, from, to, pairs, consume)
}

func TestEngineRanksBestReplayDirection(t *testing.T) {
	now := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	points := syntheticPairPoints(now, replayLookback, 28)
	quotes := make([]Quote, 0, len(points)*2)
	for _, point := range points {
		quotes = append(quotes, point.Long, point.Short)
	}
	nextRate := 0.0003
	rates := []funding.Rate{
		{
			Exchange: "binance", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Rate: 0.0001, IntervalHours: 8,
			FundingTime: now.Add(30 * time.Minute), PositionNotionalUSD: 5_000_000,
			Turnover24hUSD: 20_000_000, SourceUpdatedAt: now,
		},
		{
			Exchange: "okx", ExchangeSymbol: "BTC-USDT-SWAP", GlobalSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Rate: 0.0002, NextRate: &nextRate,
			IntervalHours: 8, FundingTime: now.Add(30 * time.Minute),
			PositionNotionalUSD: 4_000_000, Turnover24hUSD: 30_000_000,
			SourceUpdatedAt: now,
		},
	}
	store := NewSnapshotStore()
	engine := NewEngine(
		&fakeHistoryStore{quotes: quotes}, fakeFundingHistoryStore{now: now},
		store, 20*time.Minute,
	)
	if err := engine.Refresh(context.Background(), []Period{Period8h}, rates, now); err != nil {
		t.Fatal(err)
	}
	snapshot := store.Get(Period8h)
	if len(snapshot.Items) != 2 {
		t.Fatalf("items=%d", len(snapshot.Items))
	}
	item := snapshot.Items[0]
	if item.Long.Exchange == item.Short.Exchange ||
		(item.Long.Exchange != "binance" && item.Long.Exchange != "okx") ||
		(item.Short.Exchange != "binance" && item.Short.Exchange != "okx") {
		t.Fatalf("direction=%s/%s", item.Long.Exchange, item.Short.Exchange)
	}
	if item.MinPositionNotionalUSD != 4_000_000 || item.Rank != 1 {
		t.Fatalf("item=%+v", item)
	}
	if item.ProfitProbability < 0 || item.ProfitProbability > 1 {
		t.Fatalf("profit probability=%f", item.ProfitProbability)
	}
	if item.SampleCount == 0 || item.ModelState != CurrentModelVersion {
		t.Fatalf("replay metadata=%+v", item)
	}
}

func TestEngineRanksHyperliquidUSDCAgainstUSDTAndPreservesLegSymbols(t *testing.T) {
	now := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	points := syntheticPairPoints(now, replayLookback, 28)
	quotes := make([]Quote, 0, len(points)*2)
	for _, point := range points {
		point.Long.Venue = "hyperliquid"
		quotes = append(quotes, point.Long, point.Short)
	}
	rates := []funding.Rate{
		{
			Exchange: "hyperliquid", ExchangeSymbol: "BTC", GlobalSymbol: "BTCUSDC",
			BaseAsset: "BTC", QuoteAsset: "USDC", Rate: 0.0001, IntervalHours: 1,
			FundingTime: now.Add(30 * time.Minute), PositionNotionalUSD: 5_000_000,
			Turnover24hUSD: 20_000_000, SourceUpdatedAt: now,
		},
		{
			Exchange: "okx", ExchangeSymbol: "BTC-USDT-SWAP", GlobalSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Rate: 0.0003, IntervalHours: 8,
			FundingTime: now.Add(30 * time.Minute), PositionNotionalUSD: 4_000_000,
			Turnover24hUSD: 30_000_000, SourceUpdatedAt: now,
		},
	}
	store := NewSnapshotStore()
	engine := NewEngine(
		&fakeHistoryStore{quotes: quotes}, fakeFundingHistoryStore{now: now},
		store, 20*time.Minute,
	)
	if err := engine.Refresh(context.Background(), []Period{Period8h}, rates, now); err != nil {
		t.Fatal(err)
	}
	items := store.Get(Period8h).Items
	if len(items) != 2 {
		t.Fatalf("items=%+v", items)
	}
	item := items[0]
	if item.GlobalSymbol != "BTCUSDT" || item.QuoteAsset != "USDT" {
		t.Fatalf("display identity=%s/%s", item.GlobalSymbol, item.QuoteAsset)
	}
	legs := map[string]Leg{item.Long.Exchange: item.Long, item.Short.Exchange: item.Short}
	if legs["hyperliquid"].GlobalSymbol != "BTCUSDC" ||
		legs["hyperliquid"].QuoteAsset != "USDC" ||
		legs["okx"].GlobalSymbol != "BTCUSDT" {
		t.Fatalf("native leg identities not preserved: %+v", legs)
	}
}

func TestEngineRanksWithShortAvailableHistory(t *testing.T) {
	now := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	points := syntheticPairPoints(now, 6*24*time.Hour, 8)
	quotes := make([]Quote, 0, len(points)*2)
	for _, point := range points {
		quotes = append(quotes, point.Long, point.Short)
	}
	rates := []funding.Rate{
		{
			Exchange: "binance", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Rate: 0.0001, IntervalHours: 8,
			FundingTime: now.Add(30 * time.Minute), PositionNotionalUSD: 5_000_000,
			Turnover24hUSD: 20_000_000, SourceUpdatedAt: now,
		},
		{
			Exchange: "okx", ExchangeSymbol: "BTC-USDT-SWAP", GlobalSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Rate: 0.0002, IntervalHours: 8,
			FundingTime: now.Add(30 * time.Minute), PositionNotionalUSD: 4_000_000,
			Turnover24hUSD: 30_000_000, SourceUpdatedAt: now,
		},
	}
	store := NewSnapshotStore()
	engine := NewEngine(
		&fakeHistoryStore{quotes: quotes}, fakeFundingHistoryStore{now: now},
		store, 20*time.Minute,
	)
	if err := engine.Refresh(context.Background(), []Period{Period8h}, rates, now); err != nil {
		t.Fatal(err)
	}
	snapshot := store.Get(Period8h)
	if len(snapshot.Items) != 2 {
		t.Fatalf("items=%d", len(snapshot.Items))
	}
	item := snapshot.Items[0]
	if item.SampleCount < 3 || item.Coverage < 0.7 {
		t.Fatalf("short history ranking=%+v", item)
	}
	if item.Stale {
		t.Fatalf("fresh short history marked stale: %+v", item)
	}
}

func TestEngineRanksAgainstCacheDataThroughWhenWarmCompletesLater(t *testing.T) {
	dataThrough := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	now := dataThrough.Add(6 * time.Minute)
	points := syntheticPairPoints(dataThrough, replayLookback, 8)
	quotes := make([]Quote, 0, len(points)*2)
	for _, point := range points {
		quotes = append(quotes, point.Long, point.Short)
	}
	rates := []funding.Rate{
		{
			Exchange: "binance", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Rate: 0.0001, IntervalHours: 8,
			FundingTime: now.Add(30 * time.Minute), PositionNotionalUSD: 5_000_000,
			Turnover24hUSD: 20_000_000, SourceUpdatedAt: now,
		},
		{
			Exchange: "okx", ExchangeSymbol: "BTC-USDT-SWAP", GlobalSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Rate: 0.0002, IntervalHours: 8,
			FundingTime: now.Add(30 * time.Minute), PositionNotionalUSD: 4_000_000,
			Turnover24hUSD: 30_000_000, SourceUpdatedAt: now,
		},
	}
	store := NewSnapshotStore()
	engine := NewEngine(
		&fakeHistoryStore{quotes: quotes}, fakeFundingHistoryStore{now: now},
		store, 20*time.Minute,
	)
	periods := []Period{Period8h, Period24h}
	if err := engine.Refresh(context.Background(), periods, rates, now); err != nil {
		t.Fatal(err)
	}
	for _, period := range periods {
		if items := store.Get(period).Items; len(items) != 2 {
			t.Fatalf("period=%s items=%d", period, len(items))
		}
	}
}

func TestEngineKeepsPreviousSnapshotStaleWhenBBOIsUnavailable(t *testing.T) {
	now := time.Now().UTC()
	store := NewSnapshotStore()
	store.Replace(Period8h, []Opportunity{{GlobalSymbol: "BTCUSDT"}}, now.Add(-time.Minute))
	engine := NewEngine(&fakeHistoryStore{}, nil, store, 20*time.Minute)
	err := engine.Refresh(context.Background(), []Period{Period8h}, []funding.Rate{{
		Exchange: "binance", GlobalSymbol: "BTCUSDT", IntervalHours: 8,
	}}, now)
	if err == nil {
		t.Fatal("expected unavailable BBO error")
	}
	snapshot := store.Get(Period8h)
	if !snapshot.Stale || len(snapshot.Items) != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func TestEngineDoesNotPublishReadyEmptyWhenAllReplaySamplesAreRejected(t *testing.T) {
	now := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	points := syntheticPairPoints(now, replayLookback, 8)
	quotes := make([]Quote, 0, len(points)*2)
	for _, point := range points {
		quotes = append(quotes, point.Long, point.Short)
	}
	rates := []funding.Rate{
		{
			Exchange: "binance", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Rate: 0.0001, IntervalHours: 8,
			FundingTime: now.Add(time.Hour), PositionNotionalUSD: 1,
			Turnover24hUSD: 1, SourceUpdatedAt: now,
		},
		{
			Exchange: "okx", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Rate: 0.0002, IntervalHours: 8,
			FundingTime: now.Add(time.Hour), PositionNotionalUSD: 1,
			Turnover24hUSD: 1, SourceUpdatedAt: now,
		},
	}
	store := NewSnapshotStore()
	store.Replace(
		Period8h, []Opportunity{{GlobalSymbol: "LASTGOOD", Period: Period8h}},
		now.Add(-time.Hour),
	)
	engine := NewEngine(&fakeHistoryStore{quotes: quotes}, nil, store, 20*time.Minute)
	if err := engine.Refresh(context.Background(), []Period{Period8h}, rates, now); err == nil {
		t.Fatal("expected all replay samples to be rejected")
	}
	snapshot := store.Get(Period8h)
	if snapshot.Status != SnapshotStale || len(snapshot.Items) != 1 ||
		snapshot.Items[0].GlobalSymbol != "LASTGOOD" {
		t.Fatalf("last-good overwritten: %+v", snapshot)
	}
}

func TestOpportunityHeapKeepsDeterministicTopOneHundred(t *testing.T) {
	top := make(opportunityMinHeap, 0, 100)
	for index := 0; index < 250; index++ {
		pushOpportunity(&top, Opportunity{
			GlobalSymbol: fmt.Sprintf("S%03dUSDT", index),
			Long:         Leg{Exchange: "binance", ExchangeSymbol: fmt.Sprintf("L%03d", index)},
			Short:        Leg{Exchange: "okx", ExchangeSymbol: fmt.Sprintf("S%03d", index)},
			Score:        float64(index),
		})
	}
	if len(top) != 100 {
		t.Fatalf("heap size=%d", len(top))
	}
	for _, item := range top {
		if item.Score < 150 {
			t.Fatalf("heap retained score=%f", item.Score)
		}
	}
}
