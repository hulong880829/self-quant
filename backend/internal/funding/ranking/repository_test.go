package ranking

import (
	"context"
	"errors"
	"fmt"
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
	if settings["max_memory_usage"] != uint64(1<<30) {
		t.Fatalf("max_memory_usage=%v", settings["max_memory_usage"])
	}
	options := rankingClickHouseOptions(RepositoryConfig{
		Address: "127.0.0.1:9000", Database: "market_data", PoolSize: 2,
	})
	if options.Settings["readonly"] != 1 || options.Settings["max_threads"] != 2 ||
		options.Settings["max_memory_usage"] != uint64(1<<30) {
		t.Fatalf("open options settings=%v", options.Settings)
	}
}

func TestQueryPairsBatchesHistoryAfterDedup(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	from := now.Add(-time.Hour)
	repository := &Repository{}
	if err := repository.QueryPairs(
		context.Background(), from, now, nil, func(HistoryQuote) error { return nil },
	); err != nil {
		t.Fatal(err)
	}

	var calls []recordedHistoryBatch
	repository.queryBatch = func(
		_ context.Context, batchFrom, batchTo time.Time, pairs []HistoryPair,
		timeout time.Duration, consume func(HistoryQuote) error,
	) error {
		calls = append(calls, recordedHistoryBatch{
			from: batchFrom, to: batchTo, pairs: append([]HistoryPair(nil), pairs...),
			timeout: timeout,
		})
		return consume(HistoryQuote{Pair: pairs[0]})
	}
	pairs := []HistoryPair{
		{Venue: "okx", SourceSymbol: "BTCUSDT", CanonicalSymbol: "BTCUSDT"},
		{Venue: "binance", SourceSymbol: "BTCUSDT", CanonicalSymbol: "BTCUSDT"},
		{Venue: "binance", SourceSymbol: "BTCUSDT", CanonicalSymbol: "BTCUSDT"},
	}
	if err := repository.QueryPairs(
		context.Background(), from, now, pairs, func(HistoryQuote) error { return nil },
	); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || len(calls[0].pairs) != 2 {
		t.Fatalf("calls=%d pairs=%d", len(calls), len(calls[0].pairs))
	}
	if calls[0].pairs[0].Venue != "binance" || calls[0].pairs[1].Venue != "okx" {
		t.Fatalf("batch order=%v", calls[0].pairs)
	}
	if !calls[0].from.Equal(from) || !calls[0].to.Equal(now) {
		t.Fatalf("window=%s..%s", calls[0].from, calls[0].to)
	}

	calls = nil
	wide := sequencedHistoryPairs(129)
	if err := repository.QueryPairs(
		context.Background(), from, now, wide, func(HistoryQuote) error { return nil },
	); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || len(calls[0].pairs) != 128 || len(calls[1].pairs) != 1 {
		t.Fatalf("129 pairs calls=%d sizes=%d,%d", len(calls), len(calls[0].pairs), len(calls[1].pairs))
	}
	assertHistoryBatchesSorted(t, calls, from, now)

	calls = nil
	production := sequencedHistoryPairs(4673)
	if err := repository.QueryPairs(
		context.Background(), from, now, production, func(HistoryQuote) error { return nil },
	); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 37 || len(calls[0].pairs) != 128 || len(calls[36].pairs) != 65 {
		t.Fatalf("4673 pairs calls=%d last=%d", len(calls), len(calls[len(calls)-1].pairs))
	}
	assertHistoryBatchesSorted(t, calls, from, now)

	calls = nil
	batchErr := errors.New("second batch failed")
	repository.queryBatch = func(
		_ context.Context, batchFrom, batchTo time.Time, pairs []HistoryPair,
		timeout time.Duration, consume func(HistoryQuote) error,
	) error {
		calls = append(calls, recordedHistoryBatch{
			from: batchFrom, to: batchTo, pairs: append([]HistoryPair(nil), pairs...),
			timeout: timeout,
		})
		if len(calls) == 2 {
			return batchErr
		}
		return consume(HistoryQuote{Pair: pairs[0]})
	}
	err := repository.QueryPairs(
		context.Background(), from, now, sequencedHistoryPairs(257),
		func(HistoryQuote) error { return nil },
	)
	if !errors.Is(err, batchErr) || !strings.Contains(err.Error(), "batch_index=2") ||
		!strings.Contains(err.Error(), "batch_count=3") ||
		!strings.Contains(err.Error(), "batch_pairs=128") {
		t.Fatalf("err=%v calls=%d", err, len(calls))
	}
	if len(calls) != 2 {
		t.Fatalf("failed query continued, calls=%d", len(calls))
	}

	calls = nil
	ctx, cancel := context.WithCancel(context.Background())
	repository.queryBatch = func(
		_ context.Context, batchFrom, batchTo time.Time, pairs []HistoryPair,
		timeout time.Duration, _ func(HistoryQuote) error,
	) error {
		calls = append(calls, recordedHistoryBatch{
			from: batchFrom, to: batchTo, pairs: append([]HistoryPair(nil), pairs...),
			timeout: timeout,
		})
		cancel()
		return nil
	}
	err = repository.QueryPairs(ctx, from, now, wide, func(HistoryQuote) error { return nil })
	if !errors.Is(err, context.Canceled) || len(calls) != 1 {
		t.Fatalf("err=%v calls=%d", err, len(calls))
	}

	calls = nil
	repository.timeout = 5 * time.Minute
	repository.queryBatch = func(
		_ context.Context, batchFrom, batchTo time.Time, pairs []HistoryPair,
		timeout time.Duration, _ func(HistoryQuote) error,
	) error {
		calls = append(calls, recordedHistoryBatch{
			from: batchFrom, to: batchTo, pairs: append([]HistoryPair(nil), pairs...),
			timeout: timeout,
		})
		return nil
	}
	if err := repository.QueryWarmPairs(
		context.Background(), from, now, wide, func(HistoryQuote) error { return nil },
	); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].timeout != 5*time.Minute || calls[1].timeout != 5*time.Minute {
		t.Fatalf("warm calls=%d timeout=%s", len(calls), calls[0].timeout)
	}
	repository.timeout = time.Minute
	calls = nil
	if err := repository.QueryWarmPairs(
		context.Background(), from, now, pairs, func(HistoryQuote) error { return nil },
	); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].timeout != 2*time.Minute {
		t.Fatalf("warm floor timeout=%s", calls[0].timeout)
	}
}

type recordedHistoryBatch struct {
	from, to time.Time
	pairs    []HistoryPair
	timeout  time.Duration
}

func sequencedHistoryPairs(count int) []HistoryPair {
	pairs := make([]HistoryPair, count)
	for index := range pairs {
		symbol := fmt.Sprintf("P%04dUSDT", index)
		pairs[index] = HistoryPair{
			Venue: "binance", SourceSymbol: symbol, CanonicalSymbol: symbol,
		}
	}
	return pairs
}

func assertHistoryBatchesSorted(t *testing.T, calls []recordedHistoryBatch, from, to time.Time) {
	t.Helper()
	var previous string
	for _, call := range calls {
		if !call.from.Equal(from) || !call.to.Equal(to) {
			t.Fatalf("window=%s..%s", call.from, call.to)
		}
		for _, pair := range call.pairs {
			key := pair.Venue + "\x00" + pair.SourceSymbol
			if key < previous {
				t.Fatalf("pair %s follows %s", key, previous)
			}
			previous = key
		}
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
