package ranking

import (
	"math"
	"sort"
	"time"

	"selfquant/backend/internal/funding"
)

const (
	replayLookback      = 7 * 24 * time.Hour
	roundTripCostBPS    = 12.0
	roundTripCostReturn = roundTripCostBPS / 10_000
	endpointTolerance   = int64(2)
)

type ReplayInput struct {
	Period        Period
	LongRate      funding.Rate
	ShortRate     funding.Rate
	LongQuotes    MinuteSeriesView
	ShortQuotes   MinuteSeriesView
	LongFunding   []funding.HistoryPoint
	ShortFunding  []funding.HistoryPoint
	DataThrough   time.Time
	MinTurnover24 float64
}

type ReplayResult struct {
	CurrentMidSpreadBPS        float64
	CurrentExecutableSpreadBPS float64
	PeriodExpectedReturn       float64
	ProjectedFundingCarry      float64
	ConditionalSpreadReturn    float64
	FundingExpectedAnnualized  float64
	SpreadExpectedAnnualized   float64
	CombinedExpectedAnnualized float64
	ProfitProbability          float64
	P5Return                   float64
	Coverage                   float64
	Confidence                 float64
	Score                      float64
	SampleCount                int
	ExpectedPaybackMinutes     float64
	PaybackStatus              PaybackStatus
}

type replayWindow struct {
	entrySpread  float64
	spreadReturn float64
}

func Replay(input ReplayInput) (ReplayResult, error) {
	horizon := input.Period.Horizon()
	if horizon <= 0 {
		return ReplayResult{}, ErrInvalidPeriod
	}
	if input.DataThrough.IsZero() || input.LongQuotes.Empty() || input.ShortQuotes.Empty() {
		return ReplayResult{}, ErrInsufficient
	}
	end := input.DataThrough.UTC().Truncate(time.Minute)
	endMinute := end.Unix() / 60
	longLatest, longOK := input.LongQuotes.AtOrBefore(endMinute, endpointTolerance)
	shortLatest, shortOK := input.ShortQuotes.AtOrBefore(endMinute, endpointTolerance)
	if !longOK || !shortOK || !validMinuteQuote(longLatest) || !validMinuteQuote(shortLatest) {
		return ReplayResult{}, ErrInsufficient
	}
	maxWindows := int(replayLookback / horizon)
	if maxWindows <= 0 {
		return ReplayResult{}, ErrInvalidPeriod
	}
	windows := make([]replayWindow, 0, maxWindows)
	for index := 0; index < maxWindows; index++ {
		windowEnd := end.Add(-time.Duration(index) * horizon)
		windowStart := windowEnd.Add(-horizon)
		startMinute := windowStart.Unix() / 60
		stopMinute := windowEnd.Unix() / 60
		longEntry, longEntryOK := input.LongQuotes.AtOrBefore(startMinute, endpointTolerance)
		shortEntry, shortEntryOK := input.ShortQuotes.AtOrBefore(startMinute, endpointTolerance)
		longExit, longExitOK := input.LongQuotes.AtOrBefore(stopMinute, endpointTolerance)
		shortExit, shortExitOK := input.ShortQuotes.AtOrBefore(stopMinute, endpointTolerance)
		if !longEntryOK || !shortEntryOK || !longExitOK || !shortExitOK ||
			!validMinuteQuote(longEntry) || !validMinuteQuote(shortEntry) ||
			!validMinuteQuote(longExit) || !validMinuteQuote(shortExit) {
			continue
		}
		spreadReturn := longExit.Bid/longEntry.Ask - 1 +
			1 - shortExit.Ask/shortEntry.Bid
		entrySpread := shortEntry.Bid/longEntry.Ask - 1
		if !finite(spreadReturn) || !finite(entrySpread) {
			continue
		}
		windows = append(windows, replayWindow{
			entrySpread: entrySpread, spreadReturn: spreadReturn,
		})
	}
	coverage := float64(len(windows)) / float64(maxWindows)
	if len(windows) < minimumReplaySamplesForPeriod(input.Period) ||
		coverage < minimumCoverage(input.Period) {
		return ReplayResult{}, ErrInsufficient
	}
	longCarry, longCarryOK := projectFundingCarry(
		input.LongRate, input.LongFunding, end, end.Add(horizon),
	)
	shortCarry, shortCarryOK := projectFundingCarry(
		input.ShortRate, input.ShortFunding, end, end.Add(horizon),
	)
	if !longCarryOK || !shortCarryOK {
		return ReplayResult{}, ErrInsufficient
	}
	projectedCarry := shortCarry - longCarry
	currentSpread := shortLatest.Bid/longLatest.Ask - 1
	sort.SliceStable(windows, func(i, j int) bool {
		left := math.Abs(windows[i].entrySpread - currentSpread)
		right := math.Abs(windows[j].entrySpread - currentSpread)
		if left != right {
			return left < right
		}
		return windows[i].entrySpread < windows[j].entrySpread
	})
	conditionalCount := max(3, int(math.Ceil(float64(len(windows))*0.20)))
	if conditionalCount > len(windows) {
		return ReplayResult{}, ErrInsufficient
	}
	selected := windows[:conditionalCount]
	netReturns := make([]float64, 0, conditionalCount)
	spreadReturns := make([]float64, 0, conditionalCount)
	profitable := 0
	for _, window := range selected {
		netReturn := projectedCarry + window.spreadReturn - roundTripCostReturn
		if !finite(netReturn) {
			continue
		}
		netReturns = append(netReturns, netReturn)
		spreadReturns = append(spreadReturns, window.spreadReturn)
		if netReturn > 0 {
			profitable++
		}
	}
	if len(netReturns) < 3 {
		return ReplayResult{}, ErrInsufficient
	}
	sort.Float64s(netReturns)
	meanSpread := mean(spreadReturns)
	meanNet := mean(netReturns)
	cyclesPerYear := 365 * 24 / horizon.Hours()
	confidence := coverage * math.Min(1, float64(len(netReturns))/float64(conditionalCount))
	p5 := percentileSorted(netReturns, 0.05)
	downsidePenalty := math.Max(0, -p5) * cyclesPerYear
	liquidityPenalty := 0.0
	if input.MinTurnover24 > 0 {
		liquidityPenalty = 1 / math.Sqrt(input.MinTurnover24)
	}
	combinedAnnualized := meanNet * cyclesPerYear
	score := combinedAnnualized*confidence - downsidePenalty - liquidityPenalty
	meanGross := projectedCarry + meanSpread
	paybackStatus := PaybackNever
	var paybackMinutes float64
	if meanGross > roundTripCostReturn {
		paybackStatus = PaybackReady
		paybackMinutes = horizon.Minutes() * roundTripCostReturn / meanGross
	}
	return ReplayResult{
		CurrentMidSpreadBPS: (((shortLatest.Bid+shortLatest.Ask)/2)/
			((longLatest.Bid+longLatest.Ask)/2) - 1) * 10_000,
		CurrentExecutableSpreadBPS: currentSpread * 10_000,
		PeriodExpectedReturn:       meanNet,
		ProjectedFundingCarry:      projectedCarry,
		ConditionalSpreadReturn:    meanSpread,
		FundingExpectedAnnualized:  projectedCarry * cyclesPerYear,
		SpreadExpectedAnnualized:   (meanSpread - roundTripCostReturn) * cyclesPerYear,
		CombinedExpectedAnnualized: combinedAnnualized,
		ProfitProbability:          float64(profitable) / float64(len(netReturns)),
		P5Return:                   p5,
		Coverage:                   coverage,
		Confidence:                 confidence,
		Score:                      score,
		SampleCount:                len(netReturns),
		ExpectedPaybackMinutes:     paybackMinutes,
		PaybackStatus:              paybackStatus,
	}, nil
}

func projectFundingCarry(
	rate funding.Rate,
	history []funding.HistoryPoint,
	from, to time.Time,
) (float64, bool) {
	if rate.IntervalHours <= 0 || rate.IntervalHours > 24 ||
		rate.FundingTime.IsZero() || !from.Before(to) ||
		len(history) < 3 {
		return 0, false
	}
	interval := time.Duration(rate.IntervalHours * float64(time.Hour))
	values := make([]float64, 0, len(history))
	var latestSettlement time.Time
	for _, point := range history {
		if point.SettledAt.Before(from.Add(-replayLookback)) ||
			point.SettledAt.After(from) {
			continue
		}
		if finite(point.Rate) {
			values = append(values, point.Rate)
			if point.SettledAt.After(latestSettlement) {
				latestSettlement = point.SettledAt
			}
		}
	}
	if len(values) < 3 || latestSettlement.Before(from.Add(-2*interval)) {
		return 0, false
	}
	sort.Float64s(values)
	lower := percentileSorted(values, 0.05)
	upper := percentileSorted(values, 0.95)
	var total float64
	for _, value := range values {
		total += math.Max(lower, math.Min(upper, value))
	}
	center := total / float64(len(values))
	next := rate.FundingTime.UTC()
	for next.Before(from) {
		next = next.Add(interval)
	}
	firstRate := rate.Rate
	if rate.NextRate != nil && finite(*rate.NextRate) {
		firstRate = *rate.NextRate
	}
	if !finite(firstRate) || !finite(center) {
		return 0, false
	}
	var carry float64
	for settlement, count := next, 0; !settlement.After(to) && count < 48; settlement, count = settlement.Add(interval), count+1 {
		weight := math.Pow(0.65, float64(count))
		carry += weight*firstRate + (1-weight)*center
	}
	return carry, finite(carry)
}

func minimumReplaySamplesForPeriod(period Period) int {
	switch period {
	case Period8h:
		return 12
	case Period24h:
		return 5
	default:
		return math.MaxInt
	}
}

func validMinuteQuote(quote MinuteQuote) bool {
	return quote.Bid > 0 && quote.Ask > 0 && quote.Bid <= quote.Ask &&
		finite(quote.Bid) && finite(quote.Ask)
}

func percentileSorted(values []float64, percentile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	index := int(math.Floor(percentile * float64(len(values)-1)))
	return values[max(0, min(index, len(values)-1))]
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
