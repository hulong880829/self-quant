package ranking

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"selfquant/backend/internal/funding"
)

type Engine struct {
	history        historySnapshotProvider
	fundingHistory FundingHistoryStore
	snapshots      *SnapshotStore
	staleAfter     time.Duration
}

type historySnapshotProvider interface {
	SnapshotRange(time.Time, time.Time, []HistoryPair) (
		map[HistoryPair]MinuteSeriesView, *HistoryGeneration, error,
	)
}

type FundingHistoryStore interface {
	ListSettledHistoryRange(
		context.Context,
		time.Time,
		time.Time,
		[]funding.HistoryKey,
	) (map[funding.HistoryKey][]funding.HistoryPoint, error)
}

type ComputedPeriod struct {
	Period      Period
	Items       []Opportunity
	DataThrough time.Time
	Rejections  map[string]int
}

type Computation struct {
	Periods []ComputedPeriod
	Errors  map[Period]error
}

func NewEngine(
	history historySnapshotProvider,
	fundingHistory FundingHistoryStore,
	snapshots *SnapshotStore,
	staleAfter time.Duration,
) *Engine {
	return &Engine{
		history: history, fundingHistory: fundingHistory,
		snapshots: snapshots, staleAfter: staleAfter,
	}
}

func (e *Engine) Refresh(
	ctx context.Context,
	periods []Period,
	rates []funding.Rate,
	now time.Time,
) error {
	computation, err := e.Compute(ctx, periods, rates, now)
	if err != nil {
		for _, period := range periods {
			e.snapshots.MarkStale(period)
		}
		return err
	}
	e.Publish(computation.Periods, now)
	var periodErrors []error
	for period, periodErr := range computation.Errors {
		e.snapshots.MarkStale(period)
		periodErrors = append(periodErrors, fmt.Errorf("%s: %w", period, periodErr))
	}
	return errors.Join(periodErrors...)
}

func (e *Engine) Compute(
	ctx context.Context,
	periods []Period,
	rates []funding.Rate,
	now time.Time,
) (Computation, error) {
	if len(periods) == 0 {
		return Computation{}, nil
	}
	for _, period := range periods {
		if period.Lookback() <= 0 {
			return Computation{}, ErrInvalidPeriod
		}
	}
	rateGroups := groupRates(rates)
	pairs := historyUniverse(rates)
	quoteIndex, dataThrough, err := e.loadIndex(
		now.Add(-replayLookback), now, pairs,
	)
	if err != nil {
		return Computation{}, fmt.Errorf("load opportunity history cache: %w", err)
	}
	if dataThrough.IsZero() || !quoteIndex.hasCrossVenueCoverage(dataThrough) {
		return Computation{}, fmt.Errorf("%w: no fresh cross-venue perpetual BBO", ErrInsufficient)
	}
	fundingHistory := make(map[funding.HistoryKey][]funding.HistoryPoint)
	if e.fundingHistory != nil {
		var err error
		fundingHistory, err = e.fundingHistory.ListSettledHistoryRange(
			ctx, now.Add(-replayLookback), now, rateHistoryKeys(rates),
		)
		if err != nil {
			return Computation{}, fmt.Errorf("load settled funding history: %w", err)
		}
	}
	results := make([]ComputedPeriod, 0, len(periods))
	periodErrors := make(map[Period]error)
	for _, period := range periods {
		items, rejections, rankErr := e.rankPeriod(
			period, rateGroups, quoteIndex, fundingHistory, now, dataThrough,
		)
		if rankErr != nil {
			periodErrors[period] = rankErr
			continue
		}
		results = append(results, ComputedPeriod{
			Period: period, Items: items, DataThrough: dataThrough,
			Rejections: rejections,
		})
	}
	return Computation{Periods: results, Errors: periodErrors}, nil
}

func (e *Engine) Publish(results []ComputedPeriod, now time.Time) {
	for _, result := range results {
		e.snapshots.ReplaceComputed(
			result.Period, result.Items, result.Rejections, now, result.DataThrough,
		)
	}
}

type loadedHistory struct {
	minutes    map[HistoryPair]MinuteSeriesView
	candidates map[CandidatePair]struct{}
}

func (e *Engine) loadIndex(
	from, to time.Time, pairs []HistoryPair,
) (*loadedHistory, time.Time, error) {
	if e.history == nil {
		return nil, time.Time{}, fmt.Errorf("%w: history cache is unavailable", ErrInsufficient)
	}
	index, generation, err := e.history.SnapshotRange(from, to, pairs)
	if err != nil {
		return nil, time.Time{}, err
	}
	candidates := make(map[CandidatePair]struct{}, len(generation.candidates))
	for _, candidate := range generation.candidates {
		candidates[candidate] = struct{}{}
	}
	return &loadedHistory{minutes: index, candidates: candidates}, generation.DataThrough, nil
}

func (e *Engine) rankPeriod(
	period Period,
	groups map[string][]funding.Rate,
	quotes *loadedHistory,
	fundingHistory map[funding.HistoryKey][]funding.HistoryPoint,
	now time.Time,
	dataThrough time.Time,
) ([]Opportunity, map[string]int, error) {
	top := make(opportunityMinHeap, 0, 100)
	heap.Init(&top)
	attempted := 0
	scored := 0
	symbols := make([]string, 0, len(groups))
	for symbol := range groups {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)
	for _, symbol := range symbols {
		rates := e.eligibleRates(groups[symbol], now)
		for first := 0; first < len(rates); first++ {
			for second := first + 1; second < len(rates); second++ {
				firstRate, secondRate := rates[first], rates[second]
				if !funding.PairableRates(firstRate, secondRate) {
					continue
				}
				firstView, secondView, ok := quotes.views(firstRate, secondRate)
				if !ok {
					continue
				}
				attempted += 2
				if item, ok := e.scoreDirection(
					period, firstRate, secondRate,
					firstView, secondView,
					fundingHistory,
					now,
					dataThrough,
				); ok {
					pushOpportunity(&top, item)
					scored++
				}
				if item, ok := e.scoreDirection(
					period, secondRate, firstRate,
					secondView, firstView,
					fundingHistory,
					now,
					dataThrough,
				); ok {
					pushOpportunity(&top, item)
					scored++
				}
			}
		}
	}
	items := append([]Opportunity(nil), top...)
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		return opportunityKey(items[i]) < opportunityKey(items[j])
	})
	for index := range items {
		items[index].Rank = index + 1
	}
	if attempted > 0 && scored == 0 {
		return nil, map[string]int{"replay_rejected": attempted}, ErrInsufficient
	}
	rejections := make(map[string]int)
	if rejected := attempted - scored; rejected > 0 {
		rejections["replay_rejected"] = rejected
	}
	return items, rejections, nil
}

func (e *Engine) eligibleRates(rates []funding.Rate, now time.Time) []funding.Rate {
	result := make([]funding.Rate, 0, len(rates))
	for _, rate := range rates {
		if rate.PositionNotionalUSD <= 0 || rate.Turnover24hUSD <= 0 ||
			rate.SourceUpdatedAt.IsZero() || now.Sub(rate.SourceUpdatedAt) > e.staleAfter {
			continue
		}
		result = append(result, rate)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Turnover24hUSD != result[j].Turnover24hUSD {
			return result[i].Turnover24hUSD > result[j].Turnover24hUSD
		}
		return result[i].Exchange < result[j].Exchange
	})
	return result
}

func (e *Engine) scoreDirection(
	period Period,
	longRate funding.Rate,
	shortRate funding.Rate,
	longQuotes MinuteSeriesView,
	shortQuotes MinuteSeriesView,
	fundingHistory map[funding.HistoryKey][]funding.HistoryPoint,
	now time.Time,
	dataThrough time.Time,
) (Opportunity, bool) {
	if longQuotes.Empty() || shortQuotes.Empty() {
		return Opportunity{}, false
	}
	result, err := Replay(ReplayInput{
		Period: period, LongRate: longRate, ShortRate: shortRate,
		LongQuotes: longQuotes, ShortQuotes: shortQuotes,
		LongFunding: fundingHistory[funding.HistoryKey{
			Exchange: longRate.Exchange, ExchangeSymbol: longRate.ExchangeSymbol,
		}],
		ShortFunding: fundingHistory[funding.HistoryKey{
			Exchange: shortRate.Exchange, ExchangeSymbol: shortRate.ExchangeSymbol,
		}],
		DataThrough:   dataThrough,
		MinTurnover24: math.Min(longRate.Turnover24hUSD, shortRate.Turnover24hUSD),
	})
	if err != nil {
		return Opportunity{}, false
	}
	updatedAt := dataThrough
	if longRate.SourceUpdatedAt.Before(updatedAt) {
		updatedAt = longRate.SourceUpdatedAt
	}
	if shortRate.SourceUpdatedAt.Before(updatedAt) {
		updatedAt = shortRate.SourceUpdatedAt
	}
	stale := updatedAt.IsZero() || now.Sub(updatedAt) > e.staleAfter
	return Opportunity{
		GlobalSymbol: funding.RankingCanonicalSymbol(longRate), BaseAsset: longRate.BaseAsset,
		QuoteAsset: "USDT", Period: period,
		Long: legFromRate(longRate), Short: legFromRate(shortRate),
		CurrentMidSpreadBPS:        result.CurrentMidSpreadBPS,
		CurrentExecutableSpreadBPS: result.CurrentExecutableSpreadBPS,
		PeriodExpectedReturn:       result.PeriodExpectedReturn,
		FundingExpectedAnnualized:  result.FundingExpectedAnnualized,
		SpreadExpectedAnnualized:   result.SpreadExpectedAnnualized,
		CombinedExpectedAnnualized: result.CombinedExpectedAnnualized,
		ProfitProbability:          result.ProfitProbability,
		P5Return:                   result.P5Return,
		MinPositionNotionalUSD:     math.Min(longRate.PositionNotionalUSD, shortRate.PositionNotionalUSD),
		MinTurnover24hUSD:          math.Min(longRate.Turnover24hUSD, shortRate.Turnover24hUSD),
		Coverage:                   result.Coverage, Confidence: result.Confidence, Score: result.Score,
		ModelState: CurrentModelVersion, SampleCount: result.SampleCount,
		ExpectedPaybackMinutes: result.ExpectedPaybackMinutes,
		PaybackStatus:          result.PaybackStatus,
		UpdatedAt:              updatedAt, Stale: stale,
	}, true
}

func (h *loadedHistory) views(
	longRate, shortRate funding.Rate,
) (MinuteSeriesView, MinuteSeriesView, bool) {
	longPair, longOK := historyPairForRate(longRate)
	shortPair, shortOK := historyPairForRate(shortRate)
	if !longOK || !shortOK {
		return MinuteSeriesView{}, MinuteSeriesView{}, false
	}
	if len(h.candidates) > 0 {
		if _, ok := h.candidates[orderedCandidatePair(longPair, shortPair)]; !ok {
			return MinuteSeriesView{}, MinuteSeriesView{}, false
		}
	}
	longView, longExists := h.minutes[longPair]
	shortView, shortExists := h.minutes[shortPair]
	if !longExists || !shortExists || longView.Empty() || shortView.Empty() {
		return MinuteSeriesView{}, MinuteSeriesView{}, false
	}
	return longView, shortView, true
}

func rateHistoryKeys(rates []funding.Rate) []funding.HistoryKey {
	seen := make(map[funding.HistoryKey]struct{}, len(rates))
	result := make([]funding.HistoryKey, 0, len(rates))
	for _, rate := range rates {
		key := funding.HistoryKey{
			Exchange: rate.Exchange, ExchangeSymbol: rate.ExchangeSymbol,
		}
		if key.Exchange == "" || key.ExchangeSymbol == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Exchange != result[j].Exchange {
			return result[i].Exchange < result[j].Exchange
		}
		return result[i].ExchangeSymbol < result[j].ExchangeSymbol
	})
	return result
}

func groupRates(rates []funding.Rate) map[string][]funding.Rate {
	groups := make(map[string][]funding.Rate)
	seen := make(map[string]bool)
	for _, rate := range rates {
		if rate.GlobalSymbol == "" || rate.Exchange == "" || rate.IntervalHours <= 0 {
			continue
		}
		symbol := funding.RankingCanonicalSymbol(rate)
		key := symbol + "\x00" + rate.Exchange
		if seen[key] {
			continue
		}
		seen[key] = true
		groups[symbol] = append(groups[symbol], rate)
	}
	for symbol := range groups {
		sort.Slice(groups[symbol], func(i, j int) bool {
			return groups[symbol][i].Exchange < groups[symbol][j].Exchange
		})
	}
	return groups
}

func (h *loadedHistory) hasCrossVenueCoverage(dataThrough time.Time) bool {
	throughMinute := dataThrough.Unix() / 60
	venuesBySymbol := make(map[string]map[string]struct{})
	for pair, view := range h.minutes {
		quote, ok := view.Latest()
		if !ok || throughMinute-quote.Minute > 2 {
			continue
		}
		if venuesBySymbol[pair.CanonicalSymbol] == nil {
			venuesBySymbol[pair.CanonicalSymbol] = make(map[string]struct{})
		}
		venuesBySymbol[pair.CanonicalSymbol][pair.Venue] = struct{}{}
	}
	for _, venues := range venuesBySymbol {
		if len(venues) >= 2 {
			return true
		}
	}
	return false
}

func minimumCoverage(period Period) float64 {
	if period == Period24h {
		return 5.0 / 7.0
	}
	return 0.7
}

type opportunityMinHeap []Opportunity

func (h opportunityMinHeap) Len() int { return len(h) }

func (h opportunityMinHeap) Less(left, right int) bool {
	if h[left].Score != h[right].Score {
		return h[left].Score < h[right].Score
	}
	return opportunityKey(h[left]) > opportunityKey(h[right])
}

func (h opportunityMinHeap) Swap(left, right int) { h[left], h[right] = h[right], h[left] }

func (h *opportunityMinHeap) Push(value any) {
	*h = append(*h, value.(Opportunity))
}

func (h *opportunityMinHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

func pushOpportunity(target *opportunityMinHeap, value Opportunity) {
	if target.Len() < 100 {
		heap.Push(target, value)
		return
	}
	if value.Score > (*target)[0].Score ||
		(value.Score == (*target)[0].Score &&
			opportunityKey(value) < opportunityKey((*target)[0])) {
		heap.Pop(target)
		heap.Push(target, value)
	}
}

func opportunityKey(value Opportunity) string {
	return value.GlobalSymbol + "\x00" +
		value.Long.Exchange + "\x00" + value.Long.ExchangeSymbol + "\x00" +
		value.Short.Exchange + "\x00" + value.Short.ExchangeSymbol
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var total float64
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}
