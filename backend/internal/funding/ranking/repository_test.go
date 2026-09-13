package ranking

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestHistoryQueryUsesMinuteAggregationStates(t *testing.T) {
	query := exactHistoryQuery("market_data", "crypto_bbo_minute")
	for _, fragment := range []string{
		"PREWHERE product = 'perpetual'",
		"canonical_symbol IN @unique_symbols",
		"venue IN @unique_venues",
		"(venue, canonical_symbol)",
		"bucket >= toStartOfMinute(@from)",
		"argMaxMerge(bbo_state)",
		"GROUP BY bucket, canonical_symbol, venue",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("query missing %q:\n%s", fragment, query)
		}
	}
	for _, forbidden := range []string{"toStartOfMinute(ts)", "argMax(bid_price", "crypto_bbo\n"} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("query must not aggregate raw BBO rows; found %q:\n%s", forbidden, query)
		}
	}
}

func TestExactHistoryQueryFiltersVenueSymbolTuplesAndStreamsInLegOrder(t *testing.T) {
	query := exactHistoryQuery("market_data", "crypto_bbo_minute")
	for _, fragment := range []string{
		"canonical_symbol IN @unique_symbols",
		"venue IN @unique_venues",
		"arrayMap((venue_key, symbol_key) -> (venue_key, symbol_key)",
		"@pair_venues",
		"@pair_symbols",
		"(venue, canonical_symbol)",
		"ORDER BY venue, canonical_symbol, bucket",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("exact query missing %q:\n%s", fragment, query)
		}
	}
	prewhere, _, ok := strings.Cut(query, "WHERE has(")
	if !ok {
		t.Fatal("tuple filter must be in WHERE, not only PREWHERE")
	}
	if strings.Contains(prewhere, "arrayMap(") {
		t.Fatal("tuple has() must not stay in PREWHERE")
	}
	for _, forbidden := range []string{"canonical_symbol IN @symbols", "venue IN @venues"} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("exact query contains cartesian filter %q:\n%s", forbidden, query)
		}
	}
}

func TestMinuteTable(t *testing.T) {
	if got := minuteTable("crypto_bbo"); got != "crypto_bbo_minute" {
		t.Fatalf("table=%q", got)
	}
	if got := minuteTable("custom_minute"); got != "custom_minute" {
		t.Fatalf("table=%q", got)
	}
}

func TestExactHistoryQuerySelectsRawTable(t *testing.T) {
	raw := exactHistoryQueryForTable("market_data", "crypto_bbo")
	if !strings.Contains(raw, "toStartOfMinute(ts)") ||
		!strings.Contains(raw, "argMax(bid_price, ts)") ||
		!strings.Contains(raw, "canonical_symbol IN @unique_symbols") {
		t.Fatalf("raw exact query is invalid:\n%s", raw)
	}
	minute := exactHistoryQueryForTable("market_data", "crypto_bbo_minute")
	if !strings.Contains(minute, "argMaxMerge(bbo_state)") {
		t.Fatalf("minute query is invalid:\n%s", minute)
	}
}

func TestDecodePrice(t *testing.T) {
	value, ok := decodePrice(123456, 3)
	if !ok || math.Abs(value-123.456) > 1e-9 {
		t.Fatalf("value=%f ok=%v", value, ok)
	}
	if _, ok := decodePrice(0, 2); ok {
		t.Fatal("zero must be rejected")
	}
}

func TestRankingClickHouseSettingsLimitThreads(t *testing.T) {
	settings := rankingClickHouseSettings()
	if settings["readonly"] != 1 {
		t.Fatalf("readonly=%v", settings["readonly"])
	}
	if settings["max_threads"] != 2 {
		t.Fatalf("max_threads=%v", settings["max_threads"])
	}
	options := rankingClickHouseOptions(RepositoryConfig{
		Address: "127.0.0.1:9000", Database: "market_data", PoolSize: 2,
	})
	if options.Settings["readonly"] != 1 || options.Settings["max_threads"] != 2 {
		t.Fatalf("open options settings=%v", options.Settings)
	}
}

func TestQueryPairsInvokesSingleBatchForAllVenues(t *testing.T) {
	var calls int
	var got []HistoryPair
	repository := &Repository{
		queryBatch: func(
			_ context.Context, _, _ time.Time, pairs []HistoryPair, _ time.Duration,
			_ func(HistoryQuote) error,
		) error {
			calls++
			got = append([]HistoryPair(nil), pairs...)
			return nil
		},
	}
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	err := repository.QueryPairs(
		context.Background(), now.Add(-time.Hour), now,
		[]HistoryPair{
			{Venue: "binance", SourceSymbol: "BTCUSDT", CanonicalSymbol: "BTCUSDT"},
			{Venue: "okx", SourceSymbol: "BTCUSDT", CanonicalSymbol: "BTCUSDT"},
			{Venue: "bybit", SourceSymbol: "BTCUSDT", CanonicalSymbol: "BTCUSDT"},
		},
		func(HistoryQuote) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("queryPairBatch calls=%d, want 1", calls)
	}
	if len(got) != 3 {
		t.Fatalf("batch pairs=%d, want 3", len(got))
	}
}

func TestQueryPairsPropagatesBatchErrorWithoutPartialSuccess(t *testing.T) {
	want := errors.New("clickhouse unavailable")
	repository := &Repository{
		queryBatch: func(
			context.Context, time.Time, time.Time, []HistoryPair, time.Duration,
			func(HistoryQuote) error,
		) error {
			return want
		},
	}
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	err := repository.QueryPairs(
		context.Background(), now.Add(-time.Hour), now,
		[]HistoryPair{
			{Venue: "binance", SourceSymbol: "BTCUSDT", CanonicalSymbol: "BTCUSDT"},
			{Venue: "okx", SourceSymbol: "BTCUSDT", CanonicalSymbol: "BTCUSDT"},
		},
		func(HistoryQuote) error {
			t.Fatal("failed batch must not consume rows")
			return nil
		},
	)
	if !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
}

func TestHistoryQueryBindingsUseSourceSymbolNotCanonical(t *testing.T) {
	binding := historyQueryBindings([]HistoryPair{
		{Venue: "binance", SourceSymbol: "BTCUSDT", CanonicalSymbol: "BTCUSDT"},
		{Venue: "hyperliquid", SourceSymbol: "BTCUSDC", CanonicalSymbol: "BTCUSDT"},
		{Venue: "lighter", SourceSymbol: "ETHUSDC", CanonicalSymbol: "ETHUSDT"},
	})
	if strings.Join(binding.uniqueSymbols, ",") != "BTCUSDC,BTCUSDT,ETHUSDC" {
		t.Fatalf("unique_symbols=%v", binding.uniqueSymbols)
	}
	if strings.Join(binding.uniqueVenues, ",") != "binance,hyperliquid,lighter" {
		t.Fatalf("unique_venues=%v", binding.uniqueVenues)
	}
	for _, symbol := range binding.pairSymbols {
		if symbol == "ETHUSDT" {
			t.Fatal("coarse and tuple filters must not bind CanonicalSymbol ETHUSDT")
		}
	}
	foundUSDC := false
	for _, symbol := range binding.pairSymbols {
		if symbol == "BTCUSDC" {
			foundUSDC = true
		}
	}
	if !foundUSDC {
		t.Fatal("hyperliquid BTCUSDC must stay in pair_symbols")
	}
	if binding.canonicalBySource["hyperliquid\x00BTCUSDC"] != "BTCUSDT" {
		t.Fatalf("canonical map=%v", binding.canonicalBySource)
	}
	if binding.canonicalBySource["lighter\x00ETHUSDC"] != "ETHUSDT" {
		t.Fatalf("lighter canonical map=%v", binding.canonicalBySource)
	}
}
