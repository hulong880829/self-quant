package ranking

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"selfquant/backend/internal/funding"
)

const (
	defaultHistorySlots        = 12_000_000
	defaultSoftSlots           = 9_000_000
	defaultTailSlots           = 1_000_000
	tailQuoteBlockSize         = 128
	replayQuoteBlockSize       = 512
	quoteBytesEstimate         = 24
	tailRetention              = 2 * time.Hour
	candidateRetention         = time.Hour
	capacityRetryInterval      = 10 * time.Minute
	missingReplayRetryInterval = time.Hour
	coldWarmRetryInterval      = time.Hour
	defaultCandidateTarget     = 1_000
	defaultCandidateMax        = 3_000
)

var recordedRankingVenues = map[string]struct{}{
	"binance": {}, "okx": {}, "bybit": {}, "bitget": {},
	"gate": {}, "hyperliquid": {}, "aster": {}, "lighter": {},
}

type HistoryPair struct {
	Venue           string
	SourceSymbol    string
	CanonicalSymbol string
}

type MinuteQuote struct {
	Minute int64
	Bid    float64
	Ask    float64
}

type HistoryQuote struct {
	Pair  HistoryPair
	Quote MinuteQuote
}

type PairHistorySource interface {
	QueryPairs(
		context.Context,
		time.Time,
		time.Time,
		[]HistoryPair,
		func(HistoryQuote) error,
	) error
	QueryWarmPairs(
		context.Context,
		time.Time,
		time.Time,
		[]HistoryPair,
		func(HistoryQuote) error,
	) error
	Close() error
}

type CandidatePair struct {
	First  HistoryPair
	Second HistoryPair
}

type QuoteBlock struct {
	quotes []MinuteQuote
}

type LegSeries struct {
	blocks        []*QuoteBlock
	rows          int
	slots         int
	fromMinute    int64
	throughMinute int64
}

type MinuteSeriesView struct {
	series     *LegSeries
	fromMinute int64
	toMinute   int64
}

type HistoryGeneration struct {
	ID                uint64
	tail              map[HistoryPair]*LegSeries
	replay            map[HistoryPair]*LegSeries
	candidates        []CandidatePair
	replayRetainUntil map[HistoryPair]time.Time
	CandidateCount    int
	Rows              int
	Slots             int
	TailSlots         int
	ReplaySlots       int
	Bytes             int64
	From              time.Time
	DataThrough       time.Time
	BuiltAt           time.Time
}

type HistoryControlState struct {
	CapacityBlocked       bool
	DataInsufficient      bool
	RetryAfter            time.Time
	NextFullWarmAllowedAt time.Time
	UniverseHash          uint64
	LastError             string
}

type HistoryCache struct {
	source    PairHistorySource
	retention time.Duration
	maxSlots  int
	softSlots int
	tailSlots int
	maxBytes  int64

	updateMu           sync.Mutex
	control            HistoryControlState
	missingReplayUntil map[HistoryPair]time.Time
	warmFailureUntil   map[HistoryPair]time.Time
	current            atomic.Pointer[HistoryGeneration]
}

func NewHistoryCache(source PairHistorySource, retention time.Duration, maxSlots int) *HistoryCache {
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	if maxSlots <= 0 {
		maxSlots = defaultHistorySlots
	}
	softSlots := defaultSoftSlots
	if maxSlots < defaultHistorySlots {
		softSlots = max(1, maxSlots*3/4)
	}
	tailSlots := defaultTailSlots
	if maxSlots < defaultHistorySlots {
		tailSlots = max(tailQuoteBlockSize, maxSlots/12)
	}
	return &HistoryCache{
		source: source, retention: retention, maxSlots: maxSlots,
		softSlots: softSlots, tailSlots: tailSlots,
		maxBytes:           int64(maxSlots * quoteBytesEstimate * 2),
		missingReplayUntil: make(map[HistoryPair]time.Time),
		warmFailureUntil:   make(map[HistoryPair]time.Time),
	}
}

func (c *HistoryCache) Close() error {
	if c == nil || c.source == nil {
		return nil
	}
	return c.source.Close()
}

func (c *HistoryCache) Generation() *HistoryGeneration {
	if generation := c.current.Load(); generation != nil {
		return generation
	}
	return &HistoryGeneration{}
}

func (c *HistoryCache) ControlState() HistoryControlState {
	c.updateMu.Lock()
	defer c.updateMu.Unlock()
	return c.control
}

func (c *HistoryCache) Update(
	ctx context.Context, rates []funding.Rate, now time.Time, lookback time.Duration,
) error {
	c.updateMu.Lock()
	defer c.updateMu.Unlock()
	if c.source == nil {
		return fmt.Errorf("%w: history source is unavailable", ErrInsufficient)
	}
	now = now.UTC()
	wallStart := time.Now()
	if lookback <= 0 || lookback > c.retention {
		lookback = c.retention
	}
	allPairs := historyUniverse(rates)
	if len(allPairs) < 2 {
		return fmt.Errorf("%w: no recorded cross-venue perpetual universe", ErrInsufficient)
	}
	universeHash := hashHistoryPairs(allPairs)
	if c.control.UniverseHash != universeHash {
		c.control.DataInsufficient = false
	}
	previous := c.current.Load()
	if previous == nil && now.Before(c.control.NextFullWarmAllowedAt) {
		return fmt.Errorf(
			"%w until %s",
			ErrHistoryRetryDeferred, c.control.NextFullWarmAllowedAt.UTC().Format(time.RFC3339),
		)
	}
	if previous != nil && c.control.DataInsufficient &&
		c.control.UniverseHash == universeHash && now.Before(c.control.RetryAfter) {
		return fmt.Errorf("%w until %s", ErrHistoryRetryDeferred, c.control.RetryAfter.UTC().Format(time.RFC3339))
	}
	if c.control.CapacityBlocked && c.control.UniverseHash == universeHash &&
		now.Before(c.control.RetryAfter) {
		return fmt.Errorf("history cache capacity blocked until %s", c.control.RetryAfter)
	}
	queryFail := func(err error) error {
		if previous == nil {
			return c.failUnpublished(universeHash, now, err, false, time.Since(wallStart))
		}
		return c.recordFailure(universeHash, now, err, false)
	}
	capacityFail := func(err error) error {
		if previous == nil {
			return c.failUnpublished(universeHash, now, err, true, time.Since(wallStart))
		}
		return c.recordFailure(universeHash, now, err, true)
	}
	noPublish := func(err error) error {
		if previous == nil {
			return c.failUnpublished(universeHash, now, err, false, time.Since(wallStart))
		}
		return c.deferReplayRetry(universeHash, now)
	}
	estimatedTailSlots := len(allPairs) * roundSlots(
		int(tailRetention/time.Minute), tailQuoteBlockSize,
	)
	if estimatedTailSlots > c.tailSlots {
		return capacityFail(
			fmt.Errorf(
				"history tail projected slot limit exceeded: %d > %d",
				estimatedTailSlots, c.tailSlots,
			),
		)
	}
	if previous == nil {
		slog.Default().Info(
			"ranking cold warm started",
			"universe_hash", universeHash, "pairs", len(allPairs), "lookback", lookback,
		)
	}
	tailFrom := now.Add(-tailRetention)
	tailPrevious := map[HistoryPair]*LegSeries(nil)
	tailQueryFrom := tailFrom
	if previous != nil {
		tailPrevious = previous.tail
		if !previous.DataThrough.IsZero() {
			tailQueryFrom = previous.DataThrough.Add(-time.Minute)
			if tailQueryFrom.Before(tailFrom) {
				tailQueryFrom = tailFrom
			}
		}
	}
	tailTo := now.Truncate(time.Minute)
	tail, err := c.streamTier(
		ctx, tailPrevious, allPairs, tailQueryFrom, tailTo,
		tailFrom, previous == nil, tailQuoteBlockSize,
	)
	if err != nil {
		return queryFail(err)
	}
	tailUsed := tierSlots(tail)
	if tailUsed > c.tailSlots {
		return capacityFail(
			fmt.Errorf("history tail slot limit exceeded: %d > %d", tailUsed, c.tailSlots),
		)
	}

	replayBudget := c.maxSlots - tailUsed
	if preferred := c.softSlots - tailUsed; preferred > 0 && preferred < replayBudget {
		replayBudget = preferred
	}
	candidates, replayPairs, retainUntil := c.selectCandidates(
		rates, tail, previous, now, replayBudget,
	)
	if len(candidates) == 0 || len(replayPairs) < 2 {
		return noPublish(fmt.Errorf("%w: no candidates fit replay budget", ErrInsufficient))
	}
	estimated := tailUsed + estimateTierSlots(replayPairs, lookback, replayQuoteBlockSize)
	if estimated > c.maxSlots {
		return capacityFail(
			fmt.Errorf("history cache projected slot limit exceeded: %d > %d", estimated, c.maxSlots),
		)
	}

	replay, err := c.updateReplayTier(
		ctx, previous, replayPairs, now, lookback,
	)
	if err != nil {
		return queryFail(err)
	}
	discovered := c.markMissingReplay(replayPairs, replay, now)
	retainUntil = pruneMissingRetainUntil(retainUntil, c.missingReplayUntil, now)
	skipped := len(candidates)
	candidates = filterReplayReadyCandidates(replay, candidates)
	skipped -= len(candidates)
	if len(discovered) > 0 {
		c.logReplayCandidatesFiltered(discovered, skipped, now.Add(missingReplayRetryInterval))
	}
	if len(candidates) == 0 {
		return noPublish(fmt.Errorf("%w: no replay-ready candidates", ErrInsufficient))
	}
	replayUsed := tierSlots(replay)
	totalSlots := tailUsed + replayUsed
	if totalSlots > c.maxSlots {
		return capacityFail(
			fmt.Errorf("history cache slot limit exceeded: %d > %d", totalSlots, c.maxSlots),
		)
	}
	rows := tierRows(tail) + tierRows(replay)
	dataThrough := candidateDataThrough(replay, candidates)
	if dataThrough.IsZero() {
		return noPublish(
			fmt.Errorf("%w: candidate replay data has no common endpoint", ErrInsufficient),
		)
	}
	id := uint64(1)
	if previous != nil {
		id = previous.ID + 1
	}
	estimatedBytes := estimateGenerationBytes(tail, replay, candidates)
	if estimatedBytes > c.maxBytes {
		return capacityFail(
			fmt.Errorf("history cache byte limit exceeded: %d > %d", estimatedBytes, c.maxBytes),
		)
	}
	next := &HistoryGeneration{
		ID: id, tail: tail, replay: replay, candidates: candidates,
		replayRetainUntil: retainUntil, CandidateCount: len(candidates),
		Rows: rows, Slots: totalSlots, TailSlots: tailUsed, ReplaySlots: replayUsed,
		Bytes: estimatedBytes,
		From:  now.Add(-lookback), DataThrough: dataThrough, BuiltAt: now,
	}
	c.current.Store(next)
	c.control = HistoryControlState{UniverseHash: universeHash}
	if previous == nil {
		slog.Default().Info(
			"ranking cold warm completed",
			"queries_expected", expectedWarmQueryCount(now, lookback),
			"candidates", next.CandidateCount, "rows", next.Rows, "slots", next.Slots,
			"duration", time.Since(wallStart),
		)
	} else {
		slog.Default().Info(
			"ranking history incremental refresh completed",
			"from", tailQueryFrom, "to", tailTo,
			"history_generation", next.ID, "duration", time.Since(wallStart),
		)
	}
	return nil
}

func (c *HistoryCache) recordFailure(
	universeHash uint64, now time.Time, err error, capacity bool,
) error {
	c.control.LastError = err.Error()
	c.control.UniverseHash = universeHash
	c.control.CapacityBlocked = capacity
	if capacity {
		c.control.RetryAfter = now.Add(capacityRetryInterval)
	}
	return err
}

func (c *HistoryCache) failUnpublished(
	universeHash uint64, now time.Time, err error, capacity bool, duration time.Duration,
) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	alreadyCooling := now.Before(c.control.NextFullWarmAllowedAt)
	retryAfter := now.Add(coldWarmRetryInterval)
	c.control.LastError = err.Error()
	c.control.UniverseHash = universeHash
	c.control.CapacityBlocked = capacity
	c.control.DataInsufficient = errors.Is(err, ErrInsufficient)
	c.control.NextFullWarmAllowedAt = retryAfter
	if capacity {
		c.control.RetryAfter = now.Add(capacityRetryInterval)
	} else {
		c.control.RetryAfter = retryAfter
	}
	if !alreadyCooling {
		slog.Default().Warn(
			"ranking cold warm failed",
			"error_kind", unpublishedErrorKind(err),
			"duration", duration,
			"retry_after", retryAfter,
		)
	}
	if errors.Is(err, ErrHistoryRetryDeferred) {
		return err
	}
	return fmt.Errorf("%w: %w until %s", ErrHistoryRetryDeferred, err, retryAfter.UTC().Format(time.RFC3339))
}

func unpublishedErrorKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, ErrInsufficient):
		return "insufficient"
	default:
		return "query"
	}
}

func expectedWarmQueryCount(now time.Time, lookback time.Duration) int {
	to := now.Truncate(time.Minute)
	return len(warmQueryChunks(now.Add(-tailRetention), to)) +
		len(warmQueryChunks(now.Add(-lookback), to))
}

func warmQueryChunks(from, to time.Time) [][2]time.Time {
	from = from.UTC()
	to = to.UTC()
	var chunks [][2]time.Time
	chunkFrom := from
	for chunkFrom.Before(to) {
		nextDay := chunkFrom.Truncate(24 * time.Hour).Add(24 * time.Hour)
		chunkTo := nextDay
		if chunkTo.After(to) {
			chunkTo = to
		}
		chunks = append(chunks, [2]time.Time{chunkFrom, chunkTo})
		chunkFrom = chunkTo
	}
	return chunks
}

func (c *HistoryCache) missingReplay(pair HistoryPair, now time.Time) bool {
	until, ok := c.missingReplayUntil[pair]
	return ok && until.After(now)
}

func (c *HistoryCache) hasActiveMissingReplay(now time.Time) bool {
	for _, until := range c.missingReplayUntil {
		if until.After(now) {
			return true
		}
	}
	return false
}

func (c *HistoryCache) deferReplayRetry(universeHash uint64, now time.Time) error {
	retryAfter := now.Add(missingReplayRetryInterval)
	c.control.DataInsufficient = true
	c.control.RetryAfter = retryAfter
	c.control.UniverseHash = universeHash
	c.control.LastError = ErrHistoryRetryDeferred.Error()
	c.control.CapacityBlocked = false
	return fmt.Errorf("%w until %s", ErrHistoryRetryDeferred, retryAfter.UTC().Format(time.RFC3339))
}

func (c *HistoryCache) warmFailed(pair HistoryPair, now time.Time) bool {
	until, ok := c.warmFailureUntil[pair]
	return ok && until.After(now)
}

func (c *HistoryCache) markWarmFailure(pairs []HistoryPair, now time.Time) {
	if c.warmFailureUntil == nil {
		c.warmFailureUntil = make(map[HistoryPair]time.Time)
	}
	retryAfter := now.Add(coldWarmRetryInterval)
	for _, pair := range pairs {
		c.warmFailureUntil[pair] = retryAfter
	}
	slog.Default().Info(
		"ranking added replay warm failed",
		"legs", len(pairs),
		"retry_after", retryAfter,
	)
}

func (c *HistoryCache) markMissingReplay(
	pairs []HistoryPair,
	replay map[HistoryPair]*LegSeries,
	now time.Time,
) []HistoryPair {
	if c.missingReplayUntil == nil {
		c.missingReplayUntil = make(map[HistoryPair]time.Time)
	}
	retryAfter := now.Add(missingReplayRetryInterval)
	var discovered []HistoryPair
	for _, pair := range pairs {
		if replaySeriesReady(replay[pair]) {
			delete(c.missingReplayUntil, pair)
			continue
		}
		if !c.missingReplay(pair, now) {
			discovered = append(discovered, pair)
		}
		c.missingReplayUntil[pair] = retryAfter
	}
	sortHistoryPairs(discovered)
	return discovered
}

func (c *HistoryCache) logReplayCandidatesFiltered(
	missing []HistoryPair, skipped int, retryAfter time.Time,
) {
	limit := min(10, len(missing))
	labels := make([]string, 0, limit)
	for _, pair := range missing[:limit] {
		labels = append(labels, pair.Venue+":"+pair.SourceSymbol)
	}
	slog.Default().Info(
		"ranking replay candidates filtered",
		"missing_legs", len(missing),
		"skipped_candidates", skipped,
		"retry_after", retryAfter,
		"pairs", labels,
	)
}

func replaySeriesReady(series *LegSeries) bool {
	return series != nil && series.rows > 0
}

func filterReplayReadyCandidates(
	replay map[HistoryPair]*LegSeries, candidates []CandidatePair,
) []CandidatePair {
	result := make([]CandidatePair, 0, len(candidates))
	for _, candidate := range candidates {
		if replaySeriesReady(replay[candidate.First]) &&
			replaySeriesReady(replay[candidate.Second]) {
			result = append(result, candidate)
		}
	}
	return result
}

func pruneMissingRetainUntil(
	retainUntil map[HistoryPair]time.Time,
	missingUntil map[HistoryPair]time.Time,
	now time.Time,
) map[HistoryPair]time.Time {
	if len(retainUntil) == 0 {
		return retainUntil
	}
	for pair := range retainUntil {
		until, ok := missingUntil[pair]
		if ok && until.After(now) {
			delete(retainUntil, pair)
		}
	}
	return retainUntil
}

func (c *HistoryCache) updateReplayTier(
	ctx context.Context,
	previous *HistoryGeneration,
	pairs []HistoryPair,
	now time.Time,
	lookback time.Duration,
) (map[HistoryPair]*LegSeries, error) {
	previousReplay := map[HistoryPair]*LegSeries(nil)
	if previous != nil {
		previousReplay = previous.replay
	}
	var existing, added []HistoryPair
	for _, pair := range pairs {
		if c.missingReplay(pair, now) {
			continue
		}
		if previousReplay[pair] == nil {
			if c.warmFailed(pair, now) {
				continue
			}
			added = append(added, pair)
		} else {
			existing = append(existing, pair)
		}
	}
	result := selectSeries(previousReplay, pairs)
	to := now.Truncate(time.Minute)
	if len(existing) > 0 {
		from := to.Add(-5 * time.Minute)
		if previous != nil && !previous.DataThrough.IsZero() {
			from = previous.DataThrough.Add(-time.Minute)
		}
		next, err := c.streamTier(
			ctx, result, existing, from, to, now.Add(-lookback), false, replayQuoteBlockSize,
		)
		if err != nil {
			return nil, err
		}
		for pair, series := range next {
			result[pair] = series
		}
	}
	if len(added) > 0 {
		next, err := c.streamTier(
			ctx, result, added, now.Add(-lookback), to, now.Add(-lookback), true,
			replayQuoteBlockSize,
		)
		if err != nil {
			if previous != nil {
				c.markWarmFailure(added, now)
			}
			return nil, err
		}
		for pair, series := range next {
			result[pair] = series
			if replaySeriesReady(series) {
				delete(c.warmFailureUntil, pair)
			}
		}
	}
	return selectSeries(result, pairs), nil
}

func (c *HistoryCache) streamTier(
	ctx context.Context,
	previous map[HistoryPair]*LegSeries,
	pairs []HistoryPair,
	from, to, cutoff time.Time,
	warm bool,
	blockSize int,
) (map[HistoryPair]*LegSeries, error) {
	if len(pairs) == 0 || !from.Before(to) {
		return selectSeries(previous, pairs), nil
	}
	if warm {
		result := selectSeries(previous, pairs)
		for _, chunk := range warmQueryChunks(from, to) {
			next, err := c.streamTierChunkWithRetry(
				ctx, result, pairs, chunk[0], chunk[1], cutoff, true, blockSize,
			)
			if err != nil {
				return nil, err
			}
			result = next
		}
		return result, nil
	}
	return c.streamTierChunkWithRetry(
		ctx, previous, pairs, from, to, cutoff, false, blockSize,
	)
}

func (c *HistoryCache) streamTierChunkWithRetry(
	ctx context.Context,
	previous map[HistoryPair]*LegSeries,
	pairs []HistoryPair,
	from, to, cutoff time.Time,
	warm bool,
	blockSize int,
) (map[HistoryPair]*LegSeries, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		result, err := c.streamTierChunk(
			ctx, previous, pairs, from, to, cutoff, warm, blockSize,
		)
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
		if isCapacityError(err) {
			break
		}
		delay := time.Duration(1<<attempt) * 250 * time.Millisecond
		jitter := time.Duration(
			(hashHistoryPairs(pairs)+uint64(attempt*97))%200,
		) * time.Millisecond
		timer := time.NewTimer(delay + jitter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func (c *HistoryCache) streamTierChunk(
	ctx context.Context,
	previous map[HistoryPair]*LegSeries,
	pairs []HistoryPair,
	from, to, cutoff time.Time,
	warm bool,
	blockSize int,
) (map[HistoryPair]*LegSeries, error) {
	builder := newTierBuilder(previous, pairs, cutoff.Unix()/60, blockSize)
	consume := func(value HistoryQuote) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return builder.Add(value)
	}
	var err error
	if warm {
		err = c.source.QueryWarmPairs(ctx, from, to, pairs, consume)
	} else {
		err = c.source.QueryPairs(ctx, from, to, pairs, consume)
	}
	if err != nil {
		return nil, err
	}
	return builder.Freeze(), nil
}

func (c *HistoryCache) SnapshotRange(
	from, to time.Time, pairs []HistoryPair,
) (map[HistoryPair]MinuteSeriesView, *HistoryGeneration, error) {
	generation := c.current.Load()
	if generation == nil || generation.ReplaySlots == 0 {
		return nil, generation, fmt.Errorf("%w: history cache is warming", ErrInsufficient)
	}
	result := make(map[HistoryPair]MinuteSeriesView, len(pairs))
	fromMinute, toMinute := from.Unix()/60, to.Unix()/60
	for _, pair := range pairs {
		if series := generation.replay[pair]; series != nil &&
			series.throughMinute >= fromMinute && series.fromMinute < toMinute {
			result[pair] = MinuteSeriesView{
				series: series, fromMinute: fromMinute, toMinute: toMinute,
			}
		}
	}
	return result, generation, nil
}

func (v MinuteSeriesView) Empty() bool {
	return v.series == nil || v.fromMinute >= v.toMinute
}

func (v MinuteSeriesView) Latest() (MinuteQuote, bool) {
	if v.Empty() {
		return MinuteQuote{}, false
	}
	return v.series.atOrBefore(v.toMinute-1, v.toMinute-v.fromMinute)
}

func (v MinuteSeriesView) AtOrBefore(targetMinute, toleranceMinutes int64) (MinuteQuote, bool) {
	if v.Empty() || targetMinute < v.fromMinute {
		return MinuteQuote{}, false
	}
	if targetMinute >= v.toMinute {
		targetMinute = v.toMinute - 1
	}
	quote, ok := v.series.atOrBefore(targetMinute, toleranceMinutes)
	return quote, ok && quote.Minute >= v.fromMinute
}

func (v MinuteSeriesView) ForEach(yield func(MinuteQuote) bool) {
	if v.Empty() {
		return
	}
	for _, block := range v.series.blocks {
		for _, quote := range block.quotes {
			if quote.Minute < v.fromMinute {
				continue
			}
			if quote.Minute >= v.toMinute || !yield(quote) {
				return
			}
		}
	}
}

func (s *LegSeries) atOrBefore(targetMinute, toleranceMinutes int64) (MinuteQuote, bool) {
	if s == nil || s.rows == 0 {
		return MinuteQuote{}, false
	}
	blockIndex := sort.Search(len(s.blocks), func(index int) bool {
		block := s.blocks[index]
		return block.quotes[len(block.quotes)-1].Minute >= targetMinute
	})
	if blockIndex == len(s.blocks) {
		blockIndex--
	}
	for ; blockIndex >= 0; blockIndex-- {
		quotes := s.blocks[blockIndex].quotes
		index := sort.Search(len(quotes), func(index int) bool {
			return quotes[index].Minute > targetMinute
		}) - 1
		if index >= 0 {
			quote := quotes[index]
			if toleranceMinutes < 0 || targetMinute-quote.Minute <= toleranceMinutes {
				return quote, true
			}
			return MinuteQuote{}, false
		}
	}
	return MinuteQuote{}, false
}

type tierBuilder struct {
	previous     map[HistoryPair]*LegSeries
	allowed      map[HistoryPair]struct{}
	cutoffMinute int64
	blockSize    int
	result       map[HistoryPair]*LegSeries
	seen         map[HistoryPair]struct{}
	current      HistoryPair
	currentSet   bool
	incoming     []MinuteQuote
}

func newTierBuilder(
	previous map[HistoryPair]*LegSeries,
	pairs []HistoryPair,
	cutoffMinute int64,
	blockSize int,
) *tierBuilder {
	allowed := make(map[HistoryPair]struct{}, len(pairs))
	for _, pair := range pairs {
		allowed[pair] = struct{}{}
	}
	return &tierBuilder{
		previous: previous, allowed: allowed, cutoffMinute: cutoffMinute,
		blockSize: blockSize,
		result:    make(map[HistoryPair]*LegSeries, len(pairs)),
		seen:      make(map[HistoryPair]struct{}, len(pairs)),
	}
}

func (b *tierBuilder) Add(value HistoryQuote) error {
	if _, ok := b.allowed[value.Pair]; !ok {
		return nil
	}
	if value.Quote.Minute < b.cutoffMinute ||
		value.Quote.Bid <= 0 || value.Quote.Ask <= 0 ||
		value.Quote.Bid > value.Quote.Ask {
		return nil
	}
	if b.currentSet && value.Pair != b.current {
		b.flush()
	}
	if !b.currentSet {
		b.current = value.Pair
		b.currentSet = true
	}
	b.incoming = append(b.incoming, value.Quote)
	return nil
}

func (b *tierBuilder) flush() {
	if !b.currentSet {
		return
	}
	b.result[b.current] = mergeLegSeries(
		b.previous[b.current], b.incoming, b.cutoffMinute, b.blockSize,
	)
	b.seen[b.current] = struct{}{}
	b.incoming = b.incoming[:0]
	b.currentSet = false
}

func (b *tierBuilder) Freeze() map[HistoryPair]*LegSeries {
	b.flush()
	for pair := range b.allowed {
		if _, ok := b.seen[pair]; ok {
			continue
		}
		if series := trimLegSeries(
			b.previous[pair], b.cutoffMinute, b.blockSize,
		); series != nil {
			b.result[pair] = series
		}
	}
	return b.result
}

func mergeLegSeries(
	existing *LegSeries,
	incoming []MinuteQuote,
	cutoffMinute int64,
	blockSize int,
) *LegSeries {
	incoming = normalizeMinutes(incoming, cutoffMinute)
	if len(incoming) == 0 {
		return trimLegSeries(existing, cutoffMinute, blockSize)
	}
	firstIncoming := incoming[0].Minute
	blocks := make([]*QuoteBlock, 0)
	var overlap []MinuteQuote
	if existing != nil {
		for _, block := range existing.blocks {
			first := block.quotes[0].Minute
			last := block.quotes[len(block.quotes)-1].Minute
			switch {
			case last < cutoffMinute:
				continue
			case first >= cutoffMinute && last < firstIncoming:
				blocks = append(blocks, block)
			default:
				for _, quote := range block.quotes {
					if quote.Minute >= cutoffMinute && quote.Minute >= firstIncoming {
						overlap = append(overlap, quote)
					} else if quote.Minute >= cutoffMinute && quote.Minute < firstIncoming {
						blocks = appendQuoteToBlocks(blocks, quote, blockSize)
					}
				}
			}
		}
	}
	merged := mergeSortedMinutes(overlap, incoming)
	blocks = appendBlocks(blocks, merged, blockSize)
	return newLegSeries(blocks)
}

func trimLegSeries(
	existing *LegSeries, cutoffMinute int64, blockSize int,
) *LegSeries {
	if existing == nil || existing.rows == 0 || existing.throughMinute < cutoffMinute {
		return nil
	}
	blocks := make([]*QuoteBlock, 0, len(existing.blocks))
	for _, block := range existing.blocks {
		first := block.quotes[0].Minute
		last := block.quotes[len(block.quotes)-1].Minute
		switch {
		case last < cutoffMinute:
			continue
		case first >= cutoffMinute:
			blocks = append(blocks, block)
		default:
			var boundary []MinuteQuote
			for _, quote := range block.quotes {
				if quote.Minute >= cutoffMinute {
					boundary = append(boundary, quote)
				}
			}
			blocks = appendBlocks(blocks, boundary, blockSize)
		}
	}
	return newLegSeries(blocks)
}

func appendQuoteToBlocks(
	blocks []*QuoteBlock, quote MinuteQuote, blockSize int,
) []*QuoteBlock {
	if len(blocks) > 0 {
		last := blocks[len(blocks)-1]
		if cap(last.quotes) == blockSize && len(last.quotes) < blockSize {
			copied := make([]MinuteQuote, len(last.quotes), blockSize)
			copy(copied, last.quotes)
			copied = append(copied, quote)
			blocks[len(blocks)-1] = &QuoteBlock{quotes: copied}
			return blocks
		}
	}
	quotes := make([]MinuteQuote, 1, blockSize)
	quotes[0] = quote
	return append(blocks, &QuoteBlock{quotes: quotes})
}

func appendBlocks(
	blocks []*QuoteBlock, quotes []MinuteQuote, blockSize int,
) []*QuoteBlock {
	for len(quotes) > 0 {
		if len(blocks) > 0 &&
			cap(blocks[len(blocks)-1].quotes) == blockSize &&
			len(blocks[len(blocks)-1].quotes) < blockSize {
			space := blockSize - len(blocks[len(blocks)-1].quotes)
			count := min(space, len(quotes))
			copied := make([]MinuteQuote, len(blocks[len(blocks)-1].quotes), blockSize)
			copy(copied, blocks[len(blocks)-1].quotes)
			copied = append(copied, quotes[:count]...)
			blocks[len(blocks)-1] = &QuoteBlock{quotes: copied}
			quotes = quotes[count:]
			continue
		}
		count := min(blockSize, len(quotes))
		blockQuotes := make([]MinuteQuote, count, blockSize)
		copy(blockQuotes, quotes[:count])
		blocks = append(blocks, &QuoteBlock{quotes: blockQuotes})
		quotes = quotes[count:]
	}
	return blocks
}

func newLegSeries(blocks []*QuoteBlock) *LegSeries {
	if len(blocks) == 0 {
		return nil
	}
	rows := 0
	slots := 0
	for _, block := range blocks {
		rows += len(block.quotes)
		slots += cap(block.quotes)
	}
	return &LegSeries{
		blocks: blocks, rows: rows, slots: slots,
		fromMinute:    blocks[0].quotes[0].Minute,
		throughMinute: blocks[len(blocks)-1].quotes[len(blocks[len(blocks)-1].quotes)-1].Minute,
	}
}

func normalizeMinutes(values []MinuteQuote, cutoffMinute int64) []MinuteQuote {
	if len(values) == 0 {
		return nil
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Minute < values[j].Minute })
	result := make([]MinuteQuote, 0, len(values))
	for _, value := range values {
		if value.Minute < cutoffMinute {
			continue
		}
		if len(result) > 0 && result[len(result)-1].Minute == value.Minute {
			result[len(result)-1] = value
			continue
		}
		result = append(result, value)
	}
	return result
}

func mergeSortedMinutes(left, right []MinuteQuote) []MinuteQuote {
	result := make([]MinuteQuote, 0, len(left)+len(right))
	i, j := 0, 0
	for i < len(left) && j < len(right) {
		switch {
		case left[i].Minute < right[j].Minute:
			result = append(result, left[i])
			i++
		case left[i].Minute > right[j].Minute:
			result = append(result, right[j])
			j++
		default:
			result = append(result, right[j])
			i++
			j++
		}
	}
	result = append(result, left[i:]...)
	result = append(result, right[j:]...)
	return result
}

func (c *HistoryCache) selectCandidates(
	rates []funding.Rate,
	tail map[HistoryPair]*LegSeries,
	previous *HistoryGeneration,
	now time.Time,
	replayBudget int,
) ([]CandidatePair, []HistoryPair, map[HistoryPair]time.Time) {
	type scored struct {
		pair      CandidatePair
		symbol    string
		score     float64
		funding   float64
		spread    float64
		liquidity float64
	}
	groups := make(map[string][]funding.Rate)
	for _, rate := range rates {
		if _, ok := recordedRankingVenues[strings.ToLower(rate.Exchange)]; !ok {
			continue
		}
		groups[funding.RankingCanonicalSymbol(rate)] = append(
			groups[funding.RankingCanonicalSymbol(rate)], rate,
		)
	}
	var scoredPairs []scored
	for _, group := range groups {
		for left := 0; left < len(group); left++ {
			for right := left + 1; right < len(group); right++ {
				if !funding.PairableRates(group[left], group[right]) {
					continue
				}
				first, firstOK := historyPairForRate(group[left])
				second, secondOK := historyPairForRate(group[right])
				if !firstOK || !secondOK {
					continue
				}
				firstQuote, firstOK := latestSeriesQuote(tail[first], now)
				secondQuote, secondOK := latestSeriesQuote(tail[second], now)
				if !firstOK || !secondOK {
					continue
				}
				rateDiff := math.Abs(effectiveFundingRate(group[left]) - effectiveFundingRate(group[right]))
				forward := secondQuote.Bid/firstQuote.Ask - 1
				reverse := firstQuote.Bid/secondQuote.Ask - 1
				spread := math.Max(forward, reverse)
				liquidity := math.Min(group[left].Turnover24hUSD, group[right].Turnover24hUSD)
				score := rateDiff + spread
				scoredPairs = append(scoredPairs, scored{
					pair:   orderedCandidatePair(first, second),
					symbol: funding.RankingCanonicalSymbol(group[left]),
					score:  score, funding: rateDiff, spread: spread, liquidity: liquidity,
				})
			}
		}
	}
	sort.SliceStable(scoredPairs, func(i, j int) bool {
		if scoredPairs[i].score != scoredPairs[j].score {
			return scoredPairs[i].score > scoredPairs[j].score
		}
		return candidatePairKey(scoredPairs[i].pair) < candidatePairKey(scoredPairs[j].pair)
	})
	ordered := make([]scored, 0, min(defaultCandidateMax, len(scoredPairs)))
	seenOrder := make(map[CandidatePair]struct{}, len(scoredPairs))
	appendScored := func(value scored) {
		if len(ordered) >= defaultCandidateMax {
			return
		}
		if _, exists := seenOrder[value.pair]; exists {
			return
		}
		seenOrder[value.pair] = struct{}{}
		ordered = append(ordered, value)
	}
	byPair := make(map[CandidatePair]scored, len(scoredPairs))
	for _, value := range scoredPairs {
		byPair[value.pair] = value
	}
	if previous != nil {
		previousCount := 0
		for _, pair := range previous.candidates {
			if previousCount >= 200 {
				break
			}
			if value, ok := byPair[pair]; ok {
				appendScored(value)
				previousCount++
			}
		}
	}
	perSymbol := make(map[string]int)
	for _, value := range scoredPairs {
		if perSymbol[value.symbol] >= 3 {
			continue
		}
		perSymbol[value.symbol]++
		appendScored(value)
	}
	appendTopBy := func(limit int, less func(left, right scored) bool) {
		values := append([]scored(nil), scoredPairs...)
		sort.SliceStable(values, func(i, j int) bool { return less(values[i], values[j]) })
		for index := 0; index < len(values) && index < limit; index++ {
			appendScored(values[index])
		}
	}
	appendTopBy(defaultCandidateTarget, func(left, right scored) bool {
		return left.score > right.score
	})
	appendTopBy(250, func(left, right scored) bool {
		return left.funding > right.funding
	})
	appendTopBy(250, func(left, right scored) bool {
		return left.spread > right.spread
	})
	appendTopBy(100, func(left, right scored) bool {
		return left.liquidity > right.liquidity
	})
	for index, value := range scoredPairs {
		if index%20 == 0 {
			appendScored(value)
		}
	}

	perLegSlots := roundSlots(int(c.retention/time.Minute), replayQuoteBlockSize)
	selectedLegs := make(map[HistoryPair]struct{})
	retainUntil := make(map[HistoryPair]time.Time)
	candidates := make([]CandidatePair, 0, min(defaultCandidateMax, len(ordered)))
	seenCandidates := make(map[CandidatePair]struct{})
	for _, candidate := range ordered {
		if len(candidates) >= defaultCandidateMax {
			break
		}
		if _, ok := seenCandidates[candidate.pair]; ok {
			continue
		}
		if c.missingReplay(candidate.pair.First, now) || c.missingReplay(candidate.pair.Second, now) ||
			c.warmFailed(candidate.pair.First, now) || c.warmFailed(candidate.pair.Second, now) {
			continue
		}
		additional := 0
		if _, ok := selectedLegs[candidate.pair.First]; !ok {
			additional += perLegSlots
		}
		if _, ok := selectedLegs[candidate.pair.Second]; !ok {
			additional += perLegSlots
		}
		if len(selectedLegs)*perLegSlots+additional > replayBudget {
			continue
		}
		selectedLegs[candidate.pair.First] = struct{}{}
		selectedLegs[candidate.pair.Second] = struct{}{}
		retainUntil[candidate.pair.First] = now.Add(candidateRetention)
		retainUntil[candidate.pair.Second] = now.Add(candidateRetention)
		seenCandidates[candidate.pair] = struct{}{}
		candidates = append(candidates, candidate.pair)
	}
	if previous != nil {
		for pair, until := range previous.replayRetainUntil {
			if !until.After(now) {
				continue
			}
			if c.missingReplay(pair, now) || c.warmFailed(pair, now) {
				continue
			}
			if _, ok := selectedLegs[pair]; ok {
				continue
			}
			if (len(selectedLegs)+1)*perLegSlots > replayBudget {
				break
			}
			selectedLegs[pair] = struct{}{}
			retainUntil[pair] = until
		}
	}
	replayPairs := make([]HistoryPair, 0, len(selectedLegs))
	for pair := range selectedLegs {
		replayPairs = append(replayPairs, pair)
	}
	sortHistoryPairs(replayPairs)
	return candidates, replayPairs, retainUntil
}

func latestSeriesQuote(series *LegSeries, now time.Time) (MinuteQuote, bool) {
	if series == nil {
		return MinuteQuote{}, false
	}
	quote, ok := series.atOrBefore(now.Unix()/60, 2)
	return quote, ok
}

func historyUniverse(rates []funding.Rate) []HistoryPair {
	groups := make(map[string][]funding.Rate)
	for _, rate := range rates {
		if _, ok := recordedRankingVenues[strings.ToLower(rate.Exchange)]; !ok {
			continue
		}
		canonical := funding.RankingCanonicalSymbol(rate)
		if canonical != "" {
			groups[canonical] = append(groups[canonical], rate)
		}
	}
	seen := make(map[HistoryPair]struct{})
	for _, group := range groups {
		for left := 0; left < len(group); left++ {
			for right := left + 1; right < len(group); right++ {
				if !funding.PairableRates(group[left], group[right]) {
					continue
				}
				if pair, ok := historyPairForRate(group[left]); ok {
					seen[pair] = struct{}{}
				}
				if pair, ok := historyPairForRate(group[right]); ok {
					seen[pair] = struct{}{}
				}
			}
		}
	}
	result := make([]HistoryPair, 0, len(seen))
	for pair := range seen {
		result = append(result, pair)
	}
	sortHistoryPairs(result)
	return result
}

func historyPairForRate(rate funding.Rate) (HistoryPair, bool) {
	venue := strings.ToLower(strings.TrimSpace(rate.Exchange))
	source := funding.HistorySourceSymbol(rate)
	canonical := funding.RankingCanonicalSymbol(rate)
	if venue == "" || source == "" || canonical == "" {
		return HistoryPair{}, false
	}
	return HistoryPair{
		Venue: venue, SourceSymbol: source, CanonicalSymbol: canonical,
	}, true
}

func orderedCandidatePair(first, second HistoryPair) CandidatePair {
	if historyPairKey(first) > historyPairKey(second) {
		first, second = second, first
	}
	return CandidatePair{First: first, Second: second}
}

func effectiveFundingRate(rate funding.Rate) float64 {
	if rate.NextRate != nil {
		return *rate.NextRate
	}
	return rate.Rate
}

func selectSeries(
	source map[HistoryPair]*LegSeries, pairs []HistoryPair,
) map[HistoryPair]*LegSeries {
	result := make(map[HistoryPair]*LegSeries, len(pairs))
	for _, pair := range pairs {
		if series := source[pair]; series != nil {
			result[pair] = series
		}
	}
	return result
}

func tierSlots(tier map[HistoryPair]*LegSeries) int {
	total := 0
	for _, series := range tier {
		total += series.slots
	}
	return total
}

func tierRows(tier map[HistoryPair]*LegSeries) int {
	total := 0
	for _, series := range tier {
		total += series.rows
	}
	return total
}

func candidateDataThrough(
	replay map[HistoryPair]*LegSeries, candidates []CandidatePair,
) time.Time {
	var minute int64
	seen := make(map[HistoryPair]struct{})
	for _, candidate := range candidates {
		for _, pair := range []HistoryPair{candidate.First, candidate.Second} {
			if _, exists := seen[pair]; exists {
				continue
			}
			seen[pair] = struct{}{}
			series := replay[pair]
			if series == nil || series.rows == 0 {
				return time.Time{}
			}
			if minute == 0 || series.throughMinute < minute {
				minute = series.throughMinute
			}
		}
	}
	if minute == 0 {
		return time.Time{}
	}
	return time.Unix(minute*60, 0).UTC()
}

func estimateTierSlots(
	pairs []HistoryPair, retention time.Duration, blockSize int,
) int {
	return len(pairs) * roundSlots(int(retention/time.Minute), blockSize)
}

func roundSlots(value, block int) int {
	if value <= 0 {
		return 0
	}
	return ((value + block - 1) / block) * block
}

func estimateGenerationBytes(
	tail, replay map[HistoryPair]*LegSeries, candidates []CandidatePair,
) int64 {
	const (
		pairEstimate        = 96
		seriesEstimate      = 64
		blockHeaderEstimate = 32
		candidateEstimate   = 192
	)
	var total int64
	for _, tier := range []map[HistoryPair]*LegSeries{tail, replay} {
		total += int64(len(tier) * pairEstimate)
		for _, series := range tier {
			total += seriesEstimate
			total += int64(series.slots * quoteBytesEstimate)
			total += int64(len(series.blocks) * blockHeaderEstimate)
		}
	}
	total += int64(len(candidates) * candidateEstimate)
	return total
}

func sortHistoryPairs(pairs []HistoryPair) {
	sort.Slice(pairs, func(i, j int) bool {
		return historyPairKey(pairs[i]) < historyPairKey(pairs[j])
	})
}

func historyPairKey(pair HistoryPair) string {
	return pair.Venue + "\x00" + pair.SourceSymbol + "\x00" + pair.CanonicalSymbol
}

func candidatePairKey(pair CandidatePair) string {
	return historyPairKey(pair.First) + "\x01" + historyPairKey(pair.Second)
}

func hashHistoryPairs(pairs []HistoryPair) uint64 {
	hash := fnv.New64a()
	for _, pair := range pairs {
		_, _ = hash.Write([]byte(historyPairKey(pair)))
		_, _ = hash.Write([]byte{0})
	}
	return hash.Sum64()
}

func isCapacityError(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "slot limit") ||
		errors.Is(err, ErrInsufficient))
}
