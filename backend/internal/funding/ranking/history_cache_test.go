package ranking

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"selfquant/backend/internal/funding"
)

type recordingHistoryStore struct {
	quotes []Quote
	err    error
	from   time.Time
	warm   bool
}

func (s *recordingHistoryStore) Query(
	_ context.Context, from, _ time.Time, _, _ []string,
) ([]Quote, error) {
	s.from = from
	return s.quotes, s.err
}

func (s *recordingHistoryStore) Close() error { return nil }

func (s *recordingHistoryStore) QueryWarm(
	ctx context.Context, from, to time.Time, symbols, venues []string,
) ([]Quote, error) {
	s.warm = true
	return s.Query(ctx, from, to, symbols, venues)
}

func TestHistoryCacheDeduplicatesAndClipsIncrementalRows(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	source := &recordingHistoryStore{quotes: []Quote{
		{TS: now.Add(-2 * time.Minute), Symbol: "BTCUSDT", Venue: "binance", Bid: 1, Ask: 2},
		{TS: now.Add(-2 * time.Minute), Symbol: "BTCUSDT", Venue: "binance", Bid: 2, Ask: 3},
		{TS: now.Add(-time.Minute), Symbol: "BTCUSDT", Venue: "okx", Bid: 1, Ask: 2},
	}}
	cache := NewHistoryCache(source, time.Hour, 10)
	rates := cacheTestRates(now)
	if err := cache.Update(context.Background(), rates, now, time.Hour); err != nil {
		t.Fatal(err)
	}
	if !source.warm {
		t.Fatal("cold cache must use the warm-query timeout")
	}
	generation := cache.Generation()
	if generation.Rows != 2 || generation.ID != 1 {
		t.Fatalf("generation=%+v", generation)
	}
	if got := generation.Series["BTCUSDT"]["binance"][0].Bid; got != 2 {
		t.Fatalf("deduplicated bid=%f", got)
	}
	source.quotes = []Quote{{
		TS: now, Symbol: "BTCUSDT", Venue: "binance", Bid: 4, Ask: 5,
	}}
	source.warm = false
	if err := cache.Update(context.Background(), rates, now.Add(time.Minute), time.Hour); err != nil {
		t.Fatal(err)
	}
	if source.from.Before(now.Add(-6 * time.Minute)) {
		t.Fatalf("incremental query unexpectedly started at %s", source.from)
	}
	if source.warm {
		t.Fatal("incremental cache update must use the normal query timeout")
	}
	if cache.Generation().Rows != 3 {
		t.Fatalf("rows=%d", cache.Generation().Rows)
	}
}

func TestHistoryCacheRetainsPreviousGenerationWhenLimitExceeded(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	source := &recordingHistoryStore{quotes: []Quote{
		{TS: now.Add(-time.Minute), Symbol: "BTCUSDT", Venue: "binance", Bid: 1, Ask: 2},
	}}
	cache := NewHistoryCache(source, time.Hour, 1)
	if err := cache.Update(context.Background(), cacheTestRates(now), now, time.Hour); err != nil {
		t.Fatal(err)
	}
	source.quotes = []Quote{{
		TS: now, Symbol: "BTCUSDT", Venue: "okx", Bid: 1, Ask: 2,
	}}
	if err := cache.Update(
		context.Background(), cacheTestRates(now), now.Add(time.Minute), time.Hour,
	); err == nil {
		t.Fatal("expected row limit error")
	}
	generation := cache.Generation()
	if !generation.Degraded || generation.Rows != 1 || generation.LastError == "" {
		t.Fatalf("generation=%+v", generation)
	}
}

func TestHistoryCacheRejectsUnrecordedUniverse(t *testing.T) {
	cache := NewHistoryCache(&recordingHistoryStore{err: errors.New("must not query")}, 0, 0)
	err := cache.Update(context.Background(), []funding.Rate{
		{Exchange: "hyperliquid", GlobalSymbol: "BTCUSDT"},
		{Exchange: "binance", GlobalSymbol: "BTCUSDT"},
	}, time.Now(), time.Hour)
	if !errors.Is(err, ErrInsufficient) {
		t.Fatalf("error=%v", err)
	}
}

func cacheTestRates(now time.Time) []funding.Rate {
	return []funding.Rate{
		{Exchange: "binance", GlobalSymbol: "BTCUSDT", SourceUpdatedAt: now},
		{Exchange: "okx", GlobalSymbol: "BTCUSDT", SourceUpdatedAt: now},
	}
}

func BenchmarkMergeHistoryIncremental(b *testing.B) {
	now := time.Now().UTC().Truncate(time.Minute)
	symbols := make([]string, 600)
	venues := []string{"binance", "okx", "bybit", "bitget", "gate"}
	series := make(map[string]map[string][]MinuteQuote, len(symbols))
	rows := 0
	for symbolIndex := range symbols {
		symbol := fmt.Sprintf("S%03dUSDT", symbolIndex)
		symbols[symbolIndex] = symbol
		series[symbol] = make(map[string][]MinuteQuote, len(venues))
		for _, venue := range venues {
			quotes := make([]MinuteQuote, 500)
			for minute := range quotes {
				quotes[minute] = MinuteQuote{
					Minute: now.Unix()/60 + int64(minute-500), Bid: 1, Ask: 2,
				}
			}
			series[symbol][venue] = quotes
			rows += len(quotes)
		}
	}
	previous := &HistoryGeneration{
		ID: 1, Series: series, Rows: rows, From: now.Add(-500 * time.Minute),
		DataThrough: now.Add(-time.Minute),
	}
	updates := make([]Quote, 0, len(symbols)*len(venues))
	for _, symbol := range symbols {
		for _, venue := range venues {
			updates = append(updates, Quote{
				TS: now, Symbol: symbol, Venue: venue, Bid: 1, Ask: 2,
			})
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		next := mergeHistory(
			previous, updates, now.Add(-7*24*time.Hour), previous.From, now,
			symbols, venues,
		)
		if next.Rows != rows+len(updates) {
			b.Fatal(next.Rows)
		}
	}
}
