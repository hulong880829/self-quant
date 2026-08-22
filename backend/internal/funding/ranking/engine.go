package ranking

import (
	"context"
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
	maxLookback := time.Duration(0)
	for _, period := range periods {
		if period.Lookback() <= 0 {
			return ErrInvalidPeriod
		}
		maxLookback = max(maxLookback, period.Lookback())
	}
	symbols, venues := rateDimensions(rates)
	quotes, err := e.history.Query(ctx, now.Add(-maxLookback), now, symbols, venues)
	if err != nil {
		for _, period := range periods {
			e.snapshots.MarkStale(period)
		}
		return fmt.Errorf("load opportunity history: %w", err)
	}
	if !hasCrossVenueCoverage(quotes, now) {
		for _, period := range periods {
			e.snapshots.MarkStale(period)
		}
		return fmt.Errorf("%w: no fresh cross-venue perpetual BBO", ErrInsufficient)
	}
	quoteIndex := indexQuotes(quotes)
	rateGroups := groupRates(rates)
	for _, period := range periods {
		items := e.rankPeriod(period, rateGroups, quoteIndex, now)
		e.snapshots.Replace(period, items, now)
	}
	return nil
}

func (e *Engine) rankPeriod(
	period Period,
	groups map[string][]funding.Rate,
	quotes map[string]map[string][]Quote,
	now time.Time,
) []Opportunity {
	items := make([]Opportunity, 0)
	symbols := make([]string, 0, len(groups))
	for symbol := range groups {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)
	for _, symbol := range symbols {
		rates := groups[symbol]
		for first := 0; first < len(rates); first++ {
			for second := first + 1; second < len(rates); second++ {
				firstRate, secondRate := rates[first], rates[second]
				var candidates []Opportunity
				if item, ok := e.scoreDirection(
					period, firstRate, secondRate,
					pairedQuotes(quotes[symbol][firstRate.Exchange], quotes[symbol][secondRate.Exchange]),
					now,
				); ok {
					candidates = append(candidates, item)
				}
				if item, ok := e.scoreDirection(
					period, secondRate, firstRate,
					pairedQuotes(quotes[symbol][secondRate.Exchange], quotes[symbol][firstRate.Exchange]),
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
