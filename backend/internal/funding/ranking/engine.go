package ranking

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"time"

	"selfquant/backend/internal/funding"
)

type Engine struct {
	history    HistoryStore
	snapshots  *SnapshotStore
	staleAfter time.Duration
}

func NewEngine(history HistoryStore, snapshots *SnapshotStore, staleAfter time.Duration) *Engine {
	return &Engine{history: history, snapshots: snapshots, staleAfter: staleAfter}
}

func (e *Engine) Refresh(
	ctx context.Context,
	periods []Period,
	rates []funding.Rate,
	now time.Time,
) error {
	if len(periods) == 0 {
		return nil
	}
	for _, period := range periods {
		if period.Lookback() <= 0 {
			return ErrInvalidPeriod
		}
	}
	symbols, venues := rateDimensions(rates)
	rateGroups := groupRates(rates)
	var refreshErrors []error
	for _, period := range periods {
		quoteIndex, dataThrough, err := e.loadIndex(
			ctx, now.Add(-period.Lookback()), now, symbols, venues,
		)
		if err != nil {
			e.snapshots.MarkStale(period)
			refreshErrors = append(refreshErrors, fmt.Errorf("%s: %w", period, err))
			continue
		}
		if !quoteIndex.hasCrossVenueCoverage(now) {
			e.snapshots.MarkStale(period)
			refreshErrors = append(
				refreshErrors,
				fmt.Errorf("%s: %w: no fresh cross-venue perpetual BBO", period, ErrInsufficient),
			)
			continue
		}
		items := e.rankPeriod(period, rateGroups, quoteIndex, now)
		e.snapshots.ReplaceWithDataThrough(period, items, now, dataThrough)
	}
	return errors.Join(refreshErrors...)
}

type historySnapshotProvider interface {
	SnapshotRange(time.Time, time.Time, []string, []string) (
		map[string]map[string][]MinuteQuote, *HistoryGeneration, error,
	)
}

type loadedHistory struct {
	quotes  map[string]map[string][]Quote
	minutes map[string]map[string][]MinuteQuote
}

func (e *Engine) loadIndex(
	ctx context.Context, from, to time.Time, symbols, venues []string,
) (*loadedHistory, time.Time, error) {
	if cache, ok := e.history.(historySnapshotProvider); ok {
		index, generation, err := cache.SnapshotRange(from, to, symbols, venues)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("load opportunity history cache: %w", err)
		}
		return &loadedHistory{minutes: index}, generation.DataThrough, nil
	}
	quotes, err := e.history.Query(ctx, from, to, symbols, venues)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("load opportunity history: %w", err)
	}
	var dataThrough time.Time
	for _, quote := range quotes {
		if quote.TS.After(dataThrough) {
			dataThrough = quote.TS
		}
	}
	return &loadedHistory{quotes: indexQuotes(quotes)}, dataThrough, nil
}

func (e *Engine) rankPeriod(
	period Period,
	groups map[string][]funding.Rate,
	quotes *loadedHistory,
	now time.Time,
) []Opportunity {
	items := make([]Opportunity, 0)
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
				forwardPoints := quotes.paired(
					symbol, firstRate.Exchange, secondRate.Exchange,
				)
				var candidates []Opportunity
				if item, ok := e.scoreDirection(
					period, firstRate, secondRate,
					forwardPoints,
					now,
				); ok {
					candidates = append(candidates, item)
				}
				if item, ok := e.scoreDirection(
					period, secondRate, firstRate,
					reversePairPoints(forwardPoints),
					now,
				); ok {
					candidates = append(candidates, item)
				}
				if len(candidates) == 0 {
					continue
				}
				sort.Slice(candidates, func(i, j int) bool {
					return candidates[i].CombinedExpectedAnnualized >
						candidates[j].CombinedExpectedAnnualized
				})
				items = append(items, candidates[0])
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].CombinedExpectedAnnualized > items[j].CombinedExpectedAnnualized
	})
	for index := range items {
		items[index].Rank = index + 1
	}
	return items
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
	points []PairPoint,
	now time.Time,
) (Opportunity, bool) {
	expectedBuckets := int(period.Lookback() / time.Minute)
	coverage := math.Min(1, float64(len(points))/float64(max(1, expectedBuckets)))
	if coverage < minimumCoverage(period) || len(points) == 0 {
		return Opportunity{}, false
	}
	latest := points[len(points)-1]
	if now.Sub(latest.TS) > 2*time.Minute {
		return Opportunity{}, false
	}
	result, err := Analyze(
		period, points,
		stableSeed(longRate.GlobalSymbol, longRate.Exchange, shortRate.Exchange, string(period)),
	)
	if err != nil {
		return Opportunity{}, false
	}
	totalReturns := make([]float64, 0, len(result.Paths))
	spreadReturns := make([]float64, 0, len(result.Paths))
	fundingReturns := make([]float64, 0, len(result.Paths))
	profitable := 0
	for _, path := range result.Paths {
		exitAt := now.Add(time.Duration(path.ExitMinutes * float64(time.Minute)))
		fundingReturn := projectedFunding(shortRate, now, exitAt) -
			projectedFunding(longRate, now, exitAt)
		total := path.SpreadReturn + fundingReturn
		spreadReturns = append(spreadReturns, path.SpreadReturn)
		fundingReturns = append(fundingReturns, fundingReturn)
		totalReturns = append(totalReturns, total)
		if total > 0 {
			profitable++
		}
	}
	if len(totalReturns) == 0 {
		return Opportunity{}, false
	}
	sort.Float64s(totalReturns)
	factor := 365 * 24 / period.Horizon().Hours()
	expectedTotal := mean(totalReturns)
	expectedSpread := mean(spreadReturns)
	expectedFunding := mean(fundingReturns)
	confidence := modelConfidence(period, coverage, result)
	updatedAt := latest.TS
	if longRate.SourceUpdatedAt.Before(updatedAt) {
		updatedAt = longRate.SourceUpdatedAt
	}
	if shortRate.SourceUpdatedAt.Before(updatedAt) {
		updatedAt = shortRate.SourceUpdatedAt
	}
	stale := updatedAt.IsZero() || now.Sub(updatedAt) > e.staleAfter
	modelState := "ready"
	if confidence < 0.7 {
		modelState = "low_confidence"
	}
	return Opportunity{
		GlobalSymbol: longRate.GlobalSymbol, BaseAsset: longRate.BaseAsset,
		QuoteAsset: longRate.QuoteAsset, Period: period,
		Long: legFromRate(longRate), Short: legFromRate(shortRate),
		CurrentMidSpreadBPS:        result.CurrentMidSpreadBPS,
		CurrentExecutableSpreadBPS: result.CurrentExecutableSpreadBPS,
		TargetSpreadBPS:            result.TargetSpreadBPS, PeriodExpectedReturn: expectedTotal,
		FundingExpectedAnnualized:  expectedFunding * factor,
		SpreadExpectedAnnualized:   expectedSpread * factor,
		CombinedExpectedAnnualized: expectedTotal * factor,
		FirstPassageProbability:    result.FirstPassageProbability,
		ProfitProbability:          float64(profitable) / float64(len(totalReturns)),
		ExpectedExitMinutes:        result.ExpectedExitMinutes,
		P5Return:                   totalReturns[max(0, int(math.Floor(0.05*float64(len(totalReturns)-1))))],
		MinPositionNotionalUSD:     math.Min(longRate.PositionNotionalUSD, shortRate.PositionNotionalUSD),
		MinTurnover24hUSD:          math.Min(longRate.Turnover24hUSD, shortRate.Turnover24hUSD),
		Coverage:                   coverage, Confidence: confidence, ModelState: modelState,
		UpdatedAt: updatedAt, Stale: stale,
	}, true
}

func projectedFunding(rate funding.Rate, from, to time.Time) float64 {
	if rate.IntervalHours <= 0 || !from.Before(to) {
		return 0
	}
	interval := time.Duration(rate.IntervalHours * float64(time.Hour))
	next := rate.FundingTime
	if next.IsZero() {
		next = from.Add(interval)
	}
	for next.Before(from) {
		next = next.Add(interval)
	}
	current := rate.Rate
	if rate.NextRate != nil {
		current = *rate.NextRate
	}
	var total float64
	for settlement, count := next, 0; !settlement.After(to) && count < 48; settlement, count = settlement.Add(interval), count+1 {
		weight := math.Pow(0.65, float64(count))
		total += weight*current + (1-weight)*rate.Rate
	}
	return total
}

func pairedQuotes(longQuotes, shortQuotes []Quote) []PairPoint {
	shortByTime := make(map[int64]Quote, len(shortQuotes))
	for _, quote := range shortQuotes {
		shortByTime[quote.TS.Unix()/60] = quote
	}
	result := make([]PairPoint, 0, min(len(longQuotes), len(shortQuotes)))
	for _, longQuote := range longQuotes {
		shortQuote, ok := shortByTime[longQuote.TS.Unix()/60]
		if !ok {
			continue
		}
		result = append(result, PairPoint{TS: longQuote.TS, Long: longQuote, Short: shortQuote})
	}
	return result
}

func (h *loadedHistory) paired(symbol, longVenue, shortVenue string) []PairPoint {
	if h.minutes != nil {
		return pairedMinuteQuotes(
			h.minutes[symbol][longVenue], h.minutes[symbol][shortVenue],
		)
	}
	return pairedQuotes(h.quotes[symbol][longVenue], h.quotes[symbol][shortVenue])
}

func pairedMinuteQuotes(longQuotes, shortQuotes []MinuteQuote) []PairPoint {
	result := make([]PairPoint, 0, min(len(longQuotes), len(shortQuotes)))
	longIndex, shortIndex := 0, 0
	for longIndex < len(longQuotes) && shortIndex < len(shortQuotes) {
		longQuote, shortQuote := longQuotes[longIndex], shortQuotes[shortIndex]
		switch {
		case longQuote.Minute < shortQuote.Minute:
			longIndex++
		case longQuote.Minute > shortQuote.Minute:
			shortIndex++
		default:
			result = append(result, PairPoint{
				TS: time.Unix(longQuote.Minute*60, 0).UTC(),
				Long: Quote{
					TS:  time.Unix(longQuote.Minute*60, 0).UTC(),
					Bid: longQuote.Bid, Ask: longQuote.Ask,
				},
				Short: Quote{
					TS:  time.Unix(shortQuote.Minute*60, 0).UTC(),
					Bid: shortQuote.Bid, Ask: shortQuote.Ask,
				},
			})
			longIndex++
			shortIndex++
		}
	}
	return result
}

func reversePairPoints(points []PairPoint) []PairPoint {
	result := make([]PairPoint, len(points))
	for index, point := range points {
		result[index] = PairPoint{TS: point.TS, Long: point.Short, Short: point.Long}
	}
	return result
}

func rateDimensions(rates []funding.Rate) ([]string, []string) {
	symbols := make([]string, 0, len(rates))
	venues := make([]string, 0, len(rates))
	for _, rate := range rates {
		if rate.GlobalSymbol != "" && rate.Exchange != "" {
			symbols = append(symbols, rate.GlobalSymbol)
			venues = append(venues, rate.Exchange)
		}
	}
	return uniqueSorted(symbols), uniqueSorted(venues)
}

func groupRates(rates []funding.Rate) map[string][]funding.Rate {
	groups := make(map[string][]funding.Rate)
	seen := make(map[string]bool)
	for _, rate := range rates {
		if rate.GlobalSymbol == "" || rate.Exchange == "" || rate.IntervalHours <= 0 {
			continue
		}
		key := rate.GlobalSymbol + "\x00" + rate.Exchange
		if seen[key] {
			continue
		}
		seen[key] = true
		groups[rate.GlobalSymbol] = append(groups[rate.GlobalSymbol], rate)
	}
	for symbol := range groups {
		sort.Slice(groups[symbol], func(i, j int) bool {
			return groups[symbol][i].Exchange < groups[symbol][j].Exchange
		})
	}
	return groups
}

func indexQuotes(quotes []Quote) map[string]map[string][]Quote {
	result := make(map[string]map[string][]Quote)
	for _, quote := range quotes {
		if result[quote.Symbol] == nil {
			result[quote.Symbol] = make(map[string][]Quote)
		}
		result[quote.Symbol][quote.Venue] = append(result[quote.Symbol][quote.Venue], quote)
	}
	return result
}

func hasCrossVenueCoverage(quotes []Quote, now time.Time) bool {
	venuesBySymbol := make(map[string]map[string]bool)
	for _, quote := range quotes {
		if now.Sub(quote.TS) > 2*time.Minute {
			continue
		}
		if venuesBySymbol[quote.Symbol] == nil {
			venuesBySymbol[quote.Symbol] = make(map[string]bool)
		}
		venuesBySymbol[quote.Symbol][quote.Venue] = true
	}
	for _, venues := range venuesBySymbol {
		if len(venues) >= 2 {
			return true
		}
	}
	return false
}

func hasCrossVenueCoverageIndex(index map[string]map[string][]Quote, now time.Time) bool {
	for _, byVenue := range index {
		fresh := 0
		for _, quotes := range byVenue {
			if len(quotes) > 0 && now.Sub(quotes[len(quotes)-1].TS) <= 2*time.Minute {
				fresh++
			}
		}
		if fresh >= 2 {
			return true
		}
	}
	return false
}

func (h *loadedHistory) hasCrossVenueCoverage(now time.Time) bool {
	if h.minutes == nil {
		return hasCrossVenueCoverageIndex(h.quotes, now)
	}
	nowMinute := now.Unix() / 60
	for _, byVenue := range h.minutes {
		fresh := 0
		for _, quotes := range byVenue {
			if len(quotes) > 0 && nowMinute-quotes[len(quotes)-1].Minute <= 2 {
				fresh++
			}
		}
		if fresh >= 2 {
			return true
		}
	}
	return false
}

func minimumCoverage(period Period) float64 {
	if period == Period24h {
		return 0.8
	}
	return 0.7
}

func modelConfidence(period Period, coverage float64, result ModelResult) float64 {
	residualFactor := math.Min(1, float64(result.ResidualCount)/500)
	stability := math.Min(1, math.Max(0, (0.9999-result.Phi)/0.02))
	confidence := coverage * residualFactor * stability
	if period == Period24h {
		confidence *= 0.8
	}
	return math.Min(1, math.Max(0, confidence))
}

func stableSeed(values ...string) int64 {
	hash := fnv.New64a()
	for _, value := range values {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return int64(hash.Sum64())
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
