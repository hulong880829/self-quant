package ranking

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"selfquant/backend/internal/funding"
)

type recordedWarmQuery struct {
	from  time.Time
	to    time.Time
	pairs []HistoryPair
}

type recordingPairHistorySource struct {
	quotes      []HistoryQuote
	err         error
	from        time.Time
	warm        bool
	pairs       []HistoryPair
	calls       int
	failures    int
	skipWarm    map[HistoryPair]struct{}
	warmQueries []recordedWarmQuery
	warmErr     error
}

func (s *recordingPairHistorySource) QueryPairs(
	ctx context.Context,
	from, to time.Time,
	pairs []HistoryPair,
	consume func(HistoryQuote) error,
) error {
	s.calls++
	if s.failures > 0 {
		s.failures--
		return fmt.Errorf("transient clickhouse failure")
	}
	s.from = from
	s.pairs = append([]HistoryPair(nil), pairs...)
	if s.err != nil {
		return s.err
	}
	allowed := make(map[HistoryPair]struct{}, len(pairs))
	for _, pair := range pairs {
		allowed[pair] = struct{}{}
	}
	values := append([]HistoryQuote(nil), s.quotes...)
	sort.SliceStable(values, func(i, j int) bool {
		if historyPairKey(values[i].Pair) != historyPairKey(values[j].Pair) {
			return historyPairKey(values[i].Pair) < historyPairKey(values[j].Pair)
		}
		return values[i].Quote.Minute < values[j].Quote.Minute
	})
	for _, value := range values {
		if err := ctx.Err(); err != nil {
			return err
		}
		at := time.Unix(value.Quote.Minute*60, 0).UTC()
		if _, ok := allowed[value.Pair]; !ok || at.Before(from) || !at.Before(to) {
			continue
		}
		if err := consume(value); err != nil {
			return err
		}
	}
	return nil
}

func (s *recordingPairHistorySource) QueryWarmPairs(
	ctx context.Context,
	from, to time.Time,
	pairs []HistoryPair,
	consume func(HistoryQuote) error,
) error {
	s.warm = true
	s.warmQueries = append(s.warmQueries, recordedWarmQuery{
		from: from, to: to, pairs: append([]HistoryPair(nil), pairs...),
	})
	if s.warmErr != nil && to.Sub(from) > tailRetention {
		s.calls++
		return s.warmErr
	}
	queryPairs := pairs
	if len(s.skipWarm) > 0 && to.Sub(from) > tailRetention {
		queryPairs = make([]HistoryPair, 0, len(pairs))
		for _, pair := range pairs {
			if _, skip := s.skipWarm[pair]; skip {
				continue
			}
			queryPairs = append(queryPairs, pair)
		}
	}
	return s.QueryPairs(ctx, from, to, queryPairs, consume)
}

func (s *recordingPairHistorySource) Close() error { return nil }

func TestHistoryCacheBuildsImmutableBlocksAndDeduplicates(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	binance, okx := testHistoryPairs()
	source := &recordingPairHistorySource{quotes: []HistoryQuote{
		historyQuote(binance, now.Add(-2*time.Minute), 1, 2),
		historyQuote(binance, now.Add(-2*time.Minute), 2, 3),
		historyQuote(okx, now.Add(-2*time.Minute), 3, 4),
	}}
	cache := NewHistoryCache(source, time.Hour, 12_288)
	if err := cache.Update(context.Background(), cacheTestRates(now), now, time.Hour); err != nil {
		t.Fatal(err)
	}
	if !source.warm {
		t.Fatal("cold replay cache must use warm query")
	}
	generation := cache.Generation()
	if generation.ID != 1 || generation.Slots == 0 || generation.ReplaySlots == 0 {
		t.Fatalf("generation=%+v", generation)
	}
	if generation.Slots != generation.TailSlots+generation.ReplaySlots ||
		generation.Slots > cache.maxSlots || generation.Slots > cache.softSlots {
		t.Fatalf("invalid total slot accounting: %+v", generation)
	}
	assertTierBlockCapacity(t, generation.tail, tailQuoteBlockSize, generation.TailSlots)
	assertTierBlockCapacity(t, generation.replay, replayQuoteBlockSize, generation.ReplaySlots)
	view, _, err := cache.SnapshotRange(
		now.Add(-time.Hour), now, []HistoryPair{binance, okx},
	)
	if err != nil {
		t.Fatal(err)
	}
	quote, ok := view[binance].AtOrBefore(now.Add(-2*time.Minute).Unix()/60, 0)
	if !ok || quote.Bid != 2 {
		t.Fatalf("deduplicated quote=%+v ok=%v", quote, ok)
	}
	oldSeries := generation.replay[binance]
	oldBlock := oldSeries.blocks[0]
	source.quotes = append(
		source.quotes,
		historyQuote(binance, now, 4, 5),
		historyQuote(okx, now, 5, 6),
	)
	source.warm = false
	if err := cache.Update(
		context.Background(), cacheTestRates(now), now.Add(time.Minute), time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	if source.warm {
		t.Fatal("incremental existing replay legs must use normal query")
	}
	if oldSeries.blocks[0] != oldBlock || oldBlock.quotes[0].Bid != 2 {
		t.Fatal("published generation was mutated")
	}
	if cache.Generation().ID != 2 {
		t.Fatalf("generation id=%d", cache.Generation().ID)
	}
}

func TestTailProjectionFitsObservedProductionUniverse(t *testing.T) {
	const observedTailLegs = 4_134
	pairs := make([]HistoryPair, observedTailLegs)
	for index := range pairs {
		pairs[index] = HistoryPair{
			Venue:           "binance",
			SourceSymbol:    fmt.Sprintf("TAIL%04dUSDT", index),
			CanonicalSymbol: fmt.Sprintf("TAIL%04dUSDT", index),
		}
	}
	if got := estimateTierSlots(pairs, tailRetention, replayQuoteBlockSize); got != 2_116_608 {
		t.Fatalf("old 512-slot projection=%d, want 2116608", got)
	}
	projected := estimateTierSlots(pairs, tailRetention, tailQuoteBlockSize)
	if projected != 529_152 {
		t.Fatalf("tail projection=%d, want 529152", projected)
	}
	if projected > defaultTailSlots {
		t.Fatalf("tail projection=%d exceeds limit=%d", projected, defaultTailSlots)
	}
}

func TestHistoryCacheProductionTailUniversePassesPreflight(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	rates := productionSizedTailRates(now, 2_067)
	if pairs := historyUniverse(rates); len(pairs) != 4_134 {
		t.Fatalf("history universe=%d, want 4134", len(pairs))
	}
	source := &recordingPairHistorySource{
		err: fmt.Errorf("%w: source reached after tail preflight", ErrInsufficient),
	}
	cache := NewHistoryCache(source, 7*24*time.Hour, defaultHistorySlots)
	err := cache.Update(context.Background(), rates, now, 7*24*time.Hour)
	if err == nil || !strings.Contains(err.Error(), "source reached after tail preflight") {
		t.Fatalf("update error=%v", err)
	}
	if source.calls != 1 {
		t.Fatalf("history source calls=%d, want 1", source.calls)
	}
	if cache.ControlState().CapacityBlocked {
		t.Fatalf("tail preflight unexpectedly capacity blocked: %+v", cache.ControlState())
	}
}

func TestHistoryCacheRejectsProjectedCapacityBeforeReplayWarm(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	binance, okx := testHistoryPairs()
	source := &recordingPairHistorySource{quotes: []HistoryQuote{
		historyQuote(binance, now.Add(-time.Minute), 1, 2),
		historyQuote(okx, now.Add(-time.Minute), 2, 3),
	}}
	cache := NewHistoryCache(source, 7*24*time.Hour, 1024)
	err := cache.Update(context.Background(), cacheTestRates(now), now, 7*24*time.Hour)
	if err == nil {
		t.Fatal("expected replay budget error")
	}
	state := cache.ControlState()
	if !state.CapacityBlocked || state.RetryAfter.IsZero() {
		t.Fatalf("control state=%+v", state)
	}
	if got := state.RetryAfter.Sub(now); got != capacityRetryInterval {
		t.Fatalf("capacity retry=%s, want %s", got, capacityRetryInterval)
	}
	if cache.Generation().ID != 0 {
		t.Fatalf("capacity failure published generation=%+v", cache.Generation())
	}
	if cache.ControlState().NextFullWarmAllowedAt.Sub(now) != coldWarmRetryInterval {
		t.Fatalf("id=0 capacity must set 1h full-warm floor: %+v", cache.ControlState())
	}
	err = cache.Update(
		context.Background(), cacheTestRates(now), now.Add(capacityRetryInterval), 7*24*time.Hour,
	)
	if !errors.Is(err, ErrHistoryRetryDeferred) {
		t.Fatalf("capacity retry before full-warm floor err=%v", err)
	}
	err = cache.Update(
		context.Background(), cacheTestRates(now), now.Add(coldWarmRetryInterval), 7*24*time.Hour,
	)
	if err == nil || strings.Contains(err.Error(), "capacity blocked until") {
		t.Fatalf("capacity preflight did not retry after full-warm floor: %v", err)
	}
}

func TestHistoryCacheCapacityFailurePreservesPublishedGeneration(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	binance, okx := testHistoryPairs()
	source := &recordingPairHistorySource{quotes: []HistoryQuote{
		historyQuote(binance, now.Add(-time.Minute), 1, 2),
		historyQuote(okx, now.Add(-time.Minute), 2, 3),
	}}
	cache := NewHistoryCache(source, time.Hour, 12_288)
	if err := cache.Update(
		context.Background(), cacheTestRates(now), now, time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	published := cache.Generation()
	cache.tailSlots = tailQuoteBlockSize
	err := cache.Update(
		context.Background(), cacheTestRates(now), now.Add(time.Minute), time.Hour,
	)
	if err == nil || !strings.Contains(err.Error(), "tail projected slot limit exceeded") {
		t.Fatalf("capacity error=%v", err)
	}
	if got := cache.Generation(); got != published || got.ID != 1 {
		t.Fatalf("capacity failure replaced published generation: before=%p after=%p id=%d", published, got, got.ID)
	}
	state := cache.ControlState()
	if !state.CapacityBlocked || state.RetryAfter.Sub(now.Add(time.Minute)) != capacityRetryInterval {
		t.Fatalf("control state=%+v", state)
	}
}

func TestHistoryCacheRetriesTransientWarmChunk(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	binance, okx := testHistoryPairs()
	source := &recordingPairHistorySource{
		failures: 1,
		quotes: []HistoryQuote{
			historyQuote(binance, now.Add(-time.Minute), 1, 2),
			historyQuote(okx, now.Add(-time.Minute), 2, 3),
		},
	}
	cache := NewHistoryCache(source, time.Hour, 12_288)
	if err := cache.Update(
		context.Background(), cacheTestRates(now), now, time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	if source.calls < 3 {
		t.Fatalf("expected retry plus tail/replay calls, got %d", source.calls)
	}
}

func TestHistoryCacheUsesExactUSDCAndUSDTSourcePairs(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	rates := []funding.Rate{
		{
			Exchange: "hyperliquid", ExchangeSymbol: "BTC",
			GlobalSymbol: "BTCUSDC", BaseAsset: "BTC", QuoteAsset: "USDC",
			Rate: 0.0001, IntervalHours: 1, PositionNotionalUSD: 1,
			Turnover24hUSD: 1, SourceUpdatedAt: now,
		},
		{
			Exchange: "binance", ExchangeSymbol: "BTCUSDT",
			GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
			Rate: 0.0002, IntervalHours: 8, PositionNotionalUSD: 1,
			Turnover24hUSD: 1, SourceUpdatedAt: now,
		},
	}
	pairs := historyUniverse(rates)
	if len(pairs) != 2 {
		t.Fatalf("pairs=%+v", pairs)
	}
	got := map[string]string{}
	for _, pair := range pairs {
		got[pair.Venue] = pair.SourceSymbol
		if pair.CanonicalSymbol != "BTCUSDT" {
			t.Fatalf("canonical=%q", pair.CanonicalSymbol)
		}
	}
	if got["hyperliquid"] != "BTCUSDC" || got["binance"] != "BTCUSDT" {
		t.Fatalf("source symbols=%v", got)
	}
}

func TestRecordedRankingVenuesIncludeAllEightVenues(t *testing.T) {
	for _, name := range []string{
		"binance", "okx", "bybit", "bitget", "gate", "hyperliquid", "aster", "lighter",
	} {
		if _, ok := recordedRankingVenues[name]; !ok {
			t.Fatalf("missing recorded venue %s", name)
		}
	}
}

func TestCandidateAdmissionChargesDistinctLegsOnceAndRetainsExits(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	rates := append(cacheTestRates(now), funding.Rate{
		Exchange: "bybit", ExchangeSymbol: "BTCUSDT",
		GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		Rate: 0.0003, IntervalHours: 8, PositionNotionalUSD: 1,
		Turnover24hUSD: 1, SourceUpdatedAt: now,
	})
	pairs := historyUniverse(rates)
	tail := make(map[HistoryPair]*LegSeries, len(pairs))
	for index, pair := range pairs {
		tail[pair] = newLegSeries(appendBlocks(nil, []MinuteQuote{{
			Minute: now.Unix() / 60, Bid: 100 + float64(index), Ask: 101 + float64(index),
		}}, tailQuoteBlockSize))
	}
	cache := NewHistoryCache(&recordingPairHistorySource{}, time.Hour, 12_288)
	perLeg := roundSlots(int(cache.retention/time.Minute), replayQuoteBlockSize)
	candidates, replayPairs, retainUntil := cache.selectCandidates(
		rates, tail, nil, now, 3*perLeg,
	)
	if len(candidates) < 2 || len(replayPairs) != 3 {
		t.Fatalf("candidates=%d distinct legs=%d", len(candidates), len(replayPairs))
	}
	previous := &HistoryGeneration{replayRetainUntil: retainUntil}
	_, retainedPairs, retainedUntil := cache.selectCandidates(
		rates[:2], tail, previous, now.Add(30*time.Minute), 3*perLeg,
	)
	if len(retainedPairs) != 3 {
		t.Fatalf("exited leg was not retained: pairs=%+v until=%+v", retainedPairs, retainedUntil)
	}
}

func TestMergeLegSeriesSharesFullBlocksAndCopiesTail(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Minute).Unix() / 60
	values := make([]MinuteQuote, replayQuoteBlockSize+10)
	for index := range values {
		values[index] = MinuteQuote{Minute: start + int64(index), Bid: 1, Ask: 2}
	}
	existing := newLegSeries(appendBlocks(nil, values, replayQuoteBlockSize))
	firstBlock := existing.blocks[0]
	next := mergeLegSeries(existing, []MinuteQuote{{
		Minute: start + int64(len(values)), Bid: 2, Ask: 3,
	}}, start, replayQuoteBlockSize)
	if next.blocks[0] != firstBlock {
		t.Fatal("unchanged full block was not shared")
	}
	if next.blocks[len(next.blocks)-1] == existing.blocks[len(existing.blocks)-1] {
		t.Fatal("tail block must be copied before append")
	}
	if len(existing.blocks[len(existing.blocks)-1].quotes) != 10 {
		t.Fatal("existing tail block was mutated")
	}
}

func TestFilterReplayReadyCandidatesPreservesOrder(t *testing.T) {
	binance, okx := testHistoryPairs()
	bybit := testBybitPair()
	ready := &LegSeries{rows: 1}
	replay := map[HistoryPair]*LegSeries{
		binance: ready,
		okx:     ready,
	}
	btc := orderedCandidatePair(binance, okx)
	missing := orderedCandidatePair(binance, bybit)
	ethBinance := HistoryPair{
		Venue: "binance", SourceSymbol: "ETHUSDT", CanonicalSymbol: "ETHUSDT",
	}
	ethOKX := HistoryPair{
		Venue: "okx", SourceSymbol: "ETHUSDT", CanonicalSymbol: "ETHUSDT",
	}
	replay[ethBinance] = ready
	replay[ethOKX] = ready
	eth := orderedCandidatePair(ethBinance, ethOKX)
	got := filterReplayReadyCandidates(replay, []CandidatePair{btc, missing, eth})
	want := []CandidatePair{btc, eth}
	if len(got) != len(want) {
		t.Fatalf("candidates=%+v, want %+v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("candidates[%d]=%+v, want %+v", index, got[index], want[index])
		}
	}
}

func TestHistoryCachePublishesCompleteCandidatesAndSkipsMissingLegs(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	binance, okx := testHistoryPairs()
	bybit := testBybitPair()
	ethBinance, ethOKX := testETHPairs()
	source := &recordingPairHistorySource{
		skipWarm: map[HistoryPair]struct{}{bybit: {}},
		quotes: []HistoryQuote{
			historyQuote(binance, now.Add(-time.Minute), 1, 2),
			historyQuote(okx, now.Add(-time.Minute), 2, 3),
			historyQuote(bybit, now.Add(-time.Minute), 3, 4),
			historyQuote(ethBinance, now.Add(-time.Minute), 4, 5),
			historyQuote(ethOKX, now.Add(-time.Minute), 5, 6),
		},
	}
	cache := NewHistoryCache(source, 7*24*time.Hour, defaultHistorySlots)
	rates := append(cacheTestRatesWithBybit(now), cacheTestETHRates(now)...)
	if err := cache.Update(context.Background(), rates, now, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	generation := cache.Generation()
	if generation.ID != 1 || generation.CandidateCount != 2 {
		t.Fatalf("generation=%+v", generation)
	}
	got := make(map[CandidatePair]struct{}, len(generation.candidates))
	for _, candidate := range generation.candidates {
		if candidate.First == bybit || candidate.Second == bybit {
			t.Fatalf("missing bybit candidate published: %+v", candidate)
		}
		got[candidate] = struct{}{}
	}
	if _, ok := got[orderedCandidatePair(binance, okx)]; !ok {
		t.Fatalf("complete BTC candidate missing: %+v", generation.candidates)
	}
	if _, ok := got[orderedCandidatePair(ethBinance, ethOKX)]; !ok {
		t.Fatalf("complete ETH candidate missing: %+v", generation.candidates)
	}
	if _, ok := cache.missingReplayUntil[bybit]; !ok {
		t.Fatal("missing bybit leg was not cooled down")
	}
}

func TestHistoryCacheMissingReplayCooldownSkipsWarmUntilExpiry(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	binance, okx := testHistoryPairs()
	bybit := testBybitPair()
	source := &recordingPairHistorySource{
		skipWarm: map[HistoryPair]struct{}{bybit: {}},
		quotes: []HistoryQuote{
			historyQuote(binance, now.Add(-time.Minute), 1, 2),
			historyQuote(okx, now.Add(-time.Minute), 2, 3),
			historyQuote(bybit, now.Add(-time.Minute), 3, 4),
		},
	}
	cache := NewHistoryCache(source, 7*24*time.Hour, defaultHistorySlots)
	if err := cache.Update(
		context.Background(), cacheTestRatesWithBybit(now), now, 7*24*time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	if !replayWarmContains(source.warmQueries, bybit) {
		t.Fatal("first update must warm the missing bybit leg")
	}
	warmAfterFirst := len(source.warmQueries)
	if err := cache.Update(
		context.Background(), cacheTestRatesWithBybit(now), now.Add(time.Minute), 7*24*time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	if replayWarmContains(source.warmQueries[warmAfterFirst:], bybit) {
		t.Fatal("missing bybit leg was warmed again during cooldown")
	}
	later := now.Add(missingReplayRetryInterval)
	warmAfterCooldown := len(source.warmQueries)
	source.quotes = append(source.quotes,
		historyQuote(binance, later.Add(-time.Minute), 1, 2),
		historyQuote(okx, later.Add(-time.Minute), 2, 3),
		historyQuote(bybit, later.Add(-time.Minute), 3, 4),
	)
	if err := cache.Update(
		context.Background(), cacheTestRatesWithBybit(now), later, 7*24*time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	retryPairs := replayWarmPairSet(source.warmQueries[warmAfterCooldown:])
	if _, ok := retryPairs[bybit]; !ok {
		t.Fatal("expired missing leg was not retried")
	}
	if _, ok := retryPairs[binance]; ok {
		t.Fatal("existing complete legs must not be 7d warmed on missing retry")
	}
	if _, ok := retryPairs[okx]; ok {
		t.Fatal("existing complete legs must not be 7d warmed on missing retry")
	}
}

func TestHistoryCacheAllMissingDefersWithoutPublishing(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	binance, okx := testHistoryPairs()
	source := &recordingPairHistorySource{
		skipWarm: map[HistoryPair]struct{}{binance: {}, okx: {}},
		quotes: []HistoryQuote{
			historyQuote(binance, now.Add(-time.Minute), 1, 2),
			historyQuote(okx, now.Add(-time.Minute), 2, 3),
		},
	}
	cache := NewHistoryCache(source, 7*24*time.Hour, defaultHistorySlots)
	err := cache.Update(context.Background(), cacheTestRates(now), now, 7*24*time.Hour)
	if !errors.Is(err, ErrHistoryRetryDeferred) {
		t.Fatalf("update error=%v", err)
	}
	if cache.current.Load() != nil || cache.Generation().ID != 0 {
		t.Fatalf("empty generation was published: %+v", cache.Generation())
	}
	state := cache.ControlState()
	if !state.DataInsufficient || state.CapacityBlocked ||
		state.NextFullWarmAllowedAt.Sub(now) != coldWarmRetryInterval {
		t.Fatalf("control state=%+v", state)
	}
	calls := source.calls
	err = cache.Update(
		context.Background(), cacheTestRates(now), now.Add(time.Minute), 7*24*time.Hour,
	)
	if !errors.Is(err, ErrHistoryRetryDeferred) {
		t.Fatalf("cooldown error=%v", err)
	}
	if source.calls != calls {
		t.Fatalf("deferred update queried clickhouse: calls %d -> %d", calls, source.calls)
	}
}

func TestHistoryCacheUniverseChangeDoesNotBypassFullWarmFloor(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	binance, okx := testHistoryPairs()
	ethBinance, ethOKX := testETHPairs()
	source := &recordingPairHistorySource{
		skipWarm: map[HistoryPair]struct{}{binance: {}, okx: {}},
		quotes: []HistoryQuote{
			historyQuote(binance, now.Add(-time.Minute), 1, 2),
			historyQuote(okx, now.Add(-time.Minute), 2, 3),
			historyQuote(ethBinance, now.Add(-time.Minute), 4, 5),
			historyQuote(ethOKX, now.Add(-time.Minute), 5, 6),
		},
	}
	cache := NewHistoryCache(source, 7*24*time.Hour, defaultHistorySlots)
	if err := cache.Update(
		context.Background(), cacheTestRates(now), now, 7*24*time.Hour,
	); err == nil || !errors.Is(err, ErrHistoryRetryDeferred) {
		t.Fatalf("btc universe error=%v", err)
	}
	calls := source.calls
	if err := cache.Update(
		context.Background(), cacheTestETHRates(now), now.Add(time.Minute), 7*24*time.Hour,
	); err == nil || !errors.Is(err, ErrHistoryRetryDeferred) {
		t.Fatalf("hash change during floor err=%v", err)
	}
	if source.calls != calls {
		t.Fatalf("universe jitter queried clickhouse: %d -> %d", calls, source.calls)
	}
	later := now.Add(coldWarmRetryInterval)
	source.quotes = append(source.quotes,
		historyQuote(ethBinance, later.Add(-time.Minute), 4, 5),
		historyQuote(ethOKX, later.Add(-time.Minute), 5, 6),
	)
	if err := cache.Update(
		context.Background(), cacheTestETHRates(now), later, 7*24*time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	if source.calls <= calls {
		t.Fatal("expired full-warm floor must query latest universe")
	}
	generation := cache.Generation()
	if generation.ID != 1 || generation.CandidateCount != 1 {
		t.Fatalf("generation=%+v", generation)
	}
	if generation.candidates[0] != orderedCandidatePair(ethBinance, ethOKX) {
		t.Fatalf("candidates=%+v", generation.candidates)
	}
}

func TestHistoryCacheUpdateFailurePreservesPublishedGeneration(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	binance, okx := testHistoryPairs()
	source := &recordingPairHistorySource{quotes: []HistoryQuote{
		historyQuote(binance, now.Add(-time.Minute), 1, 2),
		historyQuote(okx, now.Add(-time.Minute), 2, 3),
	}}
	cache := NewHistoryCache(source, 7*24*time.Hour, defaultHistorySlots)
	if err := cache.Update(
		context.Background(), cacheTestRates(now), now, 7*24*time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	published := cache.Generation()

	t.Run("query error", func(t *testing.T) {
		source.err = errors.New("clickhouse unavailable")
		err := cache.Update(
			context.Background(), cacheTestRates(now), now.Add(time.Minute), 7*24*time.Hour,
		)
		if err == nil || errors.Is(err, ErrHistoryRetryDeferred) {
			t.Fatalf("query error=%v", err)
		}
		if got := cache.Generation(); got != published || got.ID != 1 {
			t.Fatalf("query error replaced generation: before=%p after=%p id=%d", published, got, got.ID)
		}
		if cache.ControlState().DataInsufficient {
			t.Fatal("query error must not start missing-data cooldown")
		}
		source.err = nil
	})

	t.Run("all missing new universe", func(t *testing.T) {
		ethBinance, ethOKX := testETHPairs()
		at := now.Add(2 * time.Minute)
		source.skipWarm = map[HistoryPair]struct{}{ethBinance: {}, ethOKX: {}}
		source.quotes = append(source.quotes,
			historyQuote(ethBinance, at.Add(-time.Minute), 4, 5),
			historyQuote(ethOKX, at.Add(-time.Minute), 5, 6),
		)
		err := cache.Update(
			context.Background(), cacheTestETHRates(now), at, 7*24*time.Hour,
		)
		if !errors.Is(err, ErrHistoryRetryDeferred) {
			t.Fatalf("missing universe error=%v", err)
		}
		if got := cache.Generation(); got != published || got.ID != 1 {
			t.Fatalf("missing update replaced generation: before=%p after=%p id=%d", published, got, got.ID)
		}
	})
}

func TestHistoryCacheMissingCooldownExpiryRepublishesLeg(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	binance, okx := testHistoryPairs()
	bybit := testBybitPair()
	source := &recordingPairHistorySource{
		skipWarm: map[HistoryPair]struct{}{bybit: {}},
		quotes: []HistoryQuote{
			historyQuote(binance, now.Add(-time.Minute), 1, 2),
			historyQuote(okx, now.Add(-time.Minute), 2, 3),
			historyQuote(bybit, now.Add(-time.Minute), 3, 4),
		},
	}
	cache := NewHistoryCache(source, 7*24*time.Hour, defaultHistorySlots)
	if err := cache.Update(
		context.Background(), cacheTestRatesWithBybit(now), now, 7*24*time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	if cache.Generation().CandidateCount != 1 {
		t.Fatalf("candidates=%d", cache.Generation().CandidateCount)
	}
	later := now.Add(missingReplayRetryInterval)
	source.skipWarm = nil
	source.quotes = append(source.quotes,
		historyQuote(binance, later.Add(-time.Minute), 1, 2),
		historyQuote(okx, later.Add(-time.Minute), 2, 3),
		historyQuote(bybit, later.Add(-time.Minute), 3, 4),
	)
	if err := cache.Update(
		context.Background(), cacheTestRatesWithBybit(now), later, 7*24*time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	if cache.missingReplay(bybit, later) {
		t.Fatal("found replay data must clear missing cooldown")
	}
	if cache.Generation().CandidateCount != 3 {
		t.Fatalf("candidates=%+v", cache.Generation().candidates)
	}
}

func TestErrHistoryRetryDeferredIsDistinctFromInsufficient(t *testing.T) {
	err := fmt.Errorf("%w until %s", ErrHistoryRetryDeferred, time.Now().UTC().Format(time.RFC3339))
	if !errors.Is(err, ErrHistoryRetryDeferred) {
		t.Fatalf("errors.Is deferred=%v", err)
	}
	if errors.Is(err, ErrInsufficient) {
		t.Fatal("deferred sentinel must not match insufficient")
	}
}

func TestHistoryCacheSelectCandidatesSkipsCooledMissingLegs(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	rates := cacheTestRatesWithBybit(now)
	pairs := historyUniverse(rates)
	tail := make(map[HistoryPair]*LegSeries, len(pairs))
	for index, pair := range pairs {
		tail[pair] = newLegSeries(appendBlocks(nil, []MinuteQuote{{
			Minute: now.Unix() / 60, Bid: 100 + float64(index), Ask: 101 + float64(index),
		}}, tailQuoteBlockSize))
	}
	cache := NewHistoryCache(&recordingPairHistorySource{}, time.Hour, 12_288)
	bybit := testBybitPair()
	cache.missingReplayUntil[bybit] = now.Add(time.Hour)
	perLeg := roundSlots(int(cache.retention/time.Minute), replayQuoteBlockSize)
	candidates, replayPairs, retainUntil := cache.selectCandidates(
		rates, tail, nil, now, 3*perLeg,
	)
	if len(candidates) != 1 || len(replayPairs) != 2 {
		t.Fatalf("candidates=%+v pairs=%+v", candidates, replayPairs)
	}
	if _, ok := retainUntil[bybit]; ok {
		t.Fatal("missing leg must not be retained")
	}
	for _, pair := range replayPairs {
		if pair == bybit {
			t.Fatal("missing leg was selected for replay")
		}
	}
}

func TestWarmQueryChunksAreHalfOpenAndDoNotOverlap(t *testing.T) {
	from := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	chunks := warmQueryChunks(from, to)
	if len(chunks) != 8 {
		t.Fatalf("7d from noon chunks=%d, want 8", len(chunks))
	}
	if !chunks[0][0].Equal(from) || !chunks[len(chunks)-1][1].Equal(to) {
		t.Fatalf("chunks=%v", chunks)
	}
	for index := 1; index < len(chunks); index++ {
		if !chunks[index][0].Equal(chunks[index-1][1]) {
			t.Fatalf("gap or overlap at %d: %v -> %v", index, chunks[index-1], chunks[index])
		}
		if chunks[index][0].Equal(chunks[index-1][0].Add(-time.Minute)) {
			t.Fatal("warm chunks must not subtract one minute")
		}
	}
	midnightFrom := time.Date(2026, 8, 22, 23, 0, 0, 0, time.UTC)
	midnightTo := time.Date(2026, 8, 23, 1, 0, 0, 0, time.UTC)
	split := warmQueryChunks(midnightFrom, midnightTo)
	if len(split) != 2 || !split[0][1].Equal(time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)) ||
		!split[1][0].Equal(time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("midnight chunks=%v", split)
	}
}

func TestHistoryCacheIncrementalKeepsDataThroughOverlap(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	binance, okx := testHistoryPairs()
	source := &recordingPairHistorySource{quotes: []HistoryQuote{
		historyQuote(binance, now.Add(-time.Minute), 1, 2),
		historyQuote(okx, now.Add(-time.Minute), 2, 3),
	}}
	cache := NewHistoryCache(source, time.Hour, 12_288)
	if err := cache.Update(context.Background(), cacheTestRates(now), now, time.Hour); err != nil {
		t.Fatal(err)
	}
	through := cache.Generation().DataThrough
	next := now.Add(time.Minute)
	source.quotes = append(source.quotes,
		historyQuote(binance, next, 3, 4),
		historyQuote(okx, next, 4, 5),
	)
	if err := cache.Update(context.Background(), cacheTestRates(now), next, time.Hour); err != nil {
		t.Fatal(err)
	}
	if !source.from.Equal(through.Add(-time.Minute)) {
		t.Fatalf("incremental from=%s, want %s", source.from, through.Add(-time.Minute))
	}
}

func TestHistoryCacheDeadlineExceededSetsFullWarmFloor(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	source := &recordingPairHistorySource{err: context.DeadlineExceeded}
	cache := NewHistoryCache(source, 7*24*time.Hour, defaultHistorySlots)
	err := cache.Update(context.Background(), cacheTestRates(now), now, 7*24*time.Hour)
	if !errors.Is(err, ErrHistoryRetryDeferred) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if cache.ControlState().NextFullWarmAllowedAt.Sub(now) != coldWarmRetryInterval {
		t.Fatalf("control=%+v", cache.ControlState())
	}
	calls := source.calls
	if err := cache.Update(
		context.Background(), cacheTestRates(now), now.Add(time.Minute), 7*24*time.Hour,
	); err == nil || !errors.Is(err, ErrHistoryRetryDeferred) {
		t.Fatalf("cooldown err=%v", err)
	}
	if source.calls != calls {
		t.Fatalf("cooldown queried source: %d -> %d", calls, source.calls)
	}
}

func TestHistoryCacheCanceledDoesNotSetFullWarmFloor(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	binance, okx := testHistoryPairs()
	source := &recordingPairHistorySource{quotes: []HistoryQuote{
		historyQuote(binance, now.Add(-time.Minute), 1, 2),
		historyQuote(okx, now.Add(-time.Minute), 2, 3),
	}}
	cache := NewHistoryCache(source, 7*24*time.Hour, defaultHistorySlots)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := cache.Update(ctx, cacheTestRates(now), now, 7*24*time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if !cache.ControlState().NextFullWarmAllowedAt.IsZero() {
		t.Fatalf("canceled set full-warm floor: %+v", cache.ControlState())
	}
}

func TestHistoryCacheExistingGenerationIgnoresFullWarmFloor(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	binance, okx := testHistoryPairs()
	source := &recordingPairHistorySource{quotes: []HistoryQuote{
		historyQuote(binance, now.Add(-time.Minute), 1, 2),
		historyQuote(okx, now.Add(-time.Minute), 2, 3),
	}}
	cache := NewHistoryCache(source, 7*24*time.Hour, defaultHistorySlots)
	if err := cache.Update(
		context.Background(), cacheTestRates(now), now, 7*24*time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	cache.control.NextFullWarmAllowedAt = now.Add(coldWarmRetryInterval)
	calls := source.calls
	next := now.Add(time.Minute)
	source.quotes = append(source.quotes,
		historyQuote(binance, next, 3, 4),
		historyQuote(okx, next, 4, 5),
	)
	if err := cache.Update(
		context.Background(), cacheTestRates(now), next, 7*24*time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	if source.calls <= calls {
		t.Fatal("existing generation must still run short incremental")
	}
	if cache.Generation().ID != 2 {
		t.Fatalf("generation=%d", cache.Generation().ID)
	}
}

func TestHistoryCacheAddedWarmFailureCoolsLegsWithoutMissingMark(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	binance, okx := testHistoryPairs()
	bybit := testBybitPair()
	source := &recordingPairHistorySource{quotes: []HistoryQuote{
		historyQuote(binance, now.Add(-time.Minute), 1, 2),
		historyQuote(okx, now.Add(-time.Minute), 2, 3),
		historyQuote(bybit, now.Add(-time.Minute), 3, 4),
	}}
	cache := NewHistoryCache(source, 7*24*time.Hour, defaultHistorySlots)
	if err := cache.Update(
		context.Background(), cacheTestRates(now), now, 7*24*time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	published := cache.Generation()
	source.warmErr = errors.New("added warm timeout")
	atFail := now.Add(time.Minute)
	source.quotes = append(source.quotes,
		historyQuote(binance, atFail.Add(-time.Minute), 1, 2),
		historyQuote(okx, atFail.Add(-time.Minute), 2, 3),
		historyQuote(bybit, atFail.Add(-time.Minute), 3, 4),
	)
	err := cache.Update(
		context.Background(), cacheTestRatesWithBybit(now), atFail, 7*24*time.Hour,
	)
	if err == nil {
		t.Fatal("expected added warm failure")
	}
	if errors.Is(err, ErrHistoryRetryDeferred) {
		t.Fatalf("existing generation must not enter global full-warm floor: %v", err)
	}
	if got := cache.Generation(); got != published || got.ID != 1 {
		t.Fatalf("added warm replaced generation: before=%p after=%p", published, got)
	}
	if !cache.warmFailed(bybit, atFail) {
		t.Fatal("added leg was not cooled")
	}
	if cache.missingReplay(bybit, atFail) {
		t.Fatal("warm timeout must not mark missing data")
	}
	warmAfterFail := len(source.warmQueries)
	calls := source.calls
	source.warmErr = nil
	atRetry := now.Add(2 * time.Minute)
	source.quotes = append(source.quotes,
		historyQuote(binance, atRetry.Add(-time.Minute), 1, 2),
		historyQuote(okx, atRetry.Add(-time.Minute), 2, 3),
	)
	if err := cache.Update(
		context.Background(), cacheTestRatesWithBybit(now), atRetry, 7*24*time.Hour,
	); err != nil {
		t.Fatal(err)
	}
	if replayWarmContains(source.warmQueries[warmAfterFail:], bybit) {
		t.Fatal("cooled added leg was 7d warmed again")
	}
	if source.calls <= calls {
		t.Fatal("existing legs must still increment")
	}
	if cache.Generation().ID != 2 {
		t.Fatalf("generation=%d", cache.Generation().ID)
	}
}

func BenchmarkMergeLegSeriesIncremental(b *testing.B) {
	start := time.Now().UTC().Truncate(time.Minute).Unix() / 60
	values := make([]MinuteQuote, 10_000)
	for index := range values {
		values[index] = MinuteQuote{
			Minute: start + int64(index-10_000), Bid: 1, Ask: 2,
		}
	}
	previous := newLegSeries(appendBlocks(nil, values, replayQuoteBlockSize))
	b.ReportAllocs()
	b.ResetTimer()
	for index := range b.N {
		next := mergeLegSeries(previous, []MinuteQuote{{
			Minute: start + int64(index), Bid: 1, Ask: 2,
		}}, start-10_000, replayQuoteBlockSize)
		if next.rows != previous.rows+1 {
			b.Fatal(next.rows)
		}
	}
}

func testHistoryPairs() (HistoryPair, HistoryPair) {
	return HistoryPair{
			Venue: "binance", SourceSymbol: "BTCUSDT", CanonicalSymbol: "BTCUSDT",
		}, HistoryPair{
			Venue: "okx", SourceSymbol: "BTCUSDT", CanonicalSymbol: "BTCUSDT",
		}
}

func testBybitPair() HistoryPair {
	return HistoryPair{
		Venue: "bybit", SourceSymbol: "BTCUSDT", CanonicalSymbol: "BTCUSDT",
	}
}

func testETHPairs() (HistoryPair, HistoryPair) {
	return HistoryPair{
			Venue: "binance", SourceSymbol: "ETHUSDT", CanonicalSymbol: "ETHUSDT",
		}, HistoryPair{
			Venue: "okx", SourceSymbol: "ETHUSDT", CanonicalSymbol: "ETHUSDT",
		}
}

func replayWarmPairSet(queries []recordedWarmQuery) map[HistoryPair]struct{} {
	seen := make(map[HistoryPair]struct{})
	for _, query := range queries {
		if query.to.Sub(query.from) <= tailRetention {
			continue
		}
		for _, pair := range query.pairs {
			seen[pair] = struct{}{}
		}
	}
	return seen
}

func replayWarmContains(queries []recordedWarmQuery, pair HistoryPair) bool {
	_, ok := replayWarmPairSet(queries)[pair]
	return ok
}

func historyQuote(
	pair HistoryPair, at time.Time, bid, ask float64,
) HistoryQuote {
	return HistoryQuote{
		Pair:  pair,
		Quote: MinuteQuote{Minute: at.Unix() / 60, Bid: bid, Ask: ask},
	}
}

func cacheTestRates(now time.Time) []funding.Rate {
	return []funding.Rate{
		{
			Exchange: "binance", ExchangeSymbol: "BTCUSDT",
			GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
			Rate: 0.0001, IntervalHours: 8, PositionNotionalUSD: 1,
			Turnover24hUSD: 1, SourceUpdatedAt: now,
		},
		{
			Exchange: "okx", ExchangeSymbol: "BTCUSDT",
			GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
			Rate: 0.0002, IntervalHours: 8, PositionNotionalUSD: 1,
			Turnover24hUSD: 1, SourceUpdatedAt: now,
		},
	}
}

func cacheTestRatesWithBybit(now time.Time) []funding.Rate {
	return append(cacheTestRates(now), funding.Rate{
		Exchange: "bybit", ExchangeSymbol: "BTCUSDT",
		GlobalSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		Rate: 0.0003, IntervalHours: 8, PositionNotionalUSD: 1,
		Turnover24hUSD: 1, SourceUpdatedAt: now,
	})
}

func cacheTestETHRates(now time.Time) []funding.Rate {
	return []funding.Rate{
		{
			Exchange: "binance", ExchangeSymbol: "ETHUSDT",
			GlobalSymbol: "ETHUSDT", BaseAsset: "ETH", QuoteAsset: "USDT",
			Rate: 0.0001, IntervalHours: 8, PositionNotionalUSD: 1,
			Turnover24hUSD: 1, SourceUpdatedAt: now,
		},
		{
			Exchange: "okx", ExchangeSymbol: "ETHUSDT",
			GlobalSymbol: "ETHUSDT", BaseAsset: "ETH", QuoteAsset: "USDT",
			Rate: 0.0002, IntervalHours: 8, PositionNotionalUSD: 1,
			Turnover24hUSD: 1, SourceUpdatedAt: now,
		},
	}
}

func productionSizedTailRates(now time.Time, symbols int) []funding.Rate {
	rates := make([]funding.Rate, 0, symbols*2)
	for index := 0; index < symbols; index++ {
		base := fmt.Sprintf("TAIL%04d", index)
		symbol := base + "USDT"
		for venueIndex, venue := range []string{"binance", "okx"} {
			rates = append(rates, funding.Rate{
				Exchange: venue, ExchangeSymbol: symbol,
				GlobalSymbol: symbol, BaseAsset: base, QuoteAsset: "USDT",
				Rate: 0.0001 * float64(venueIndex+1), IntervalHours: 8,
				PositionNotionalUSD: 1, Turnover24hUSD: 1, SourceUpdatedAt: now,
			})
		}
	}
	return rates
}

func assertTierBlockCapacity(
	t *testing.T,
	tier map[HistoryPair]*LegSeries,
	wantBlockCapacity int,
	wantSlots int,
) {
	t.Helper()
	slots := 0
	for pair, series := range tier {
		for _, block := range series.blocks {
			if got := cap(block.quotes); got != wantBlockCapacity {
				t.Fatalf("%s block capacity=%d, want %d", historyPairKey(pair), got, wantBlockCapacity)
			}
			slots += cap(block.quotes)
		}
	}
	if slots != wantSlots {
		t.Fatalf("physical slots=%d, recorded slots=%d", slots, wantSlots)
	}
}

func ExampleHistoryPair() {
	pair := HistoryPair{
		Venue: "binance", SourceSymbol: "BTCUSDT", CanonicalSymbol: "BTCUSDT",
	}
	fmt.Println(pair.Venue, pair.SourceSymbol)
	// Output: binance BTCUSDT
}
