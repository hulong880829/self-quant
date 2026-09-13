package ranking

import (
	"math"
	"testing"
	"time"

	"selfquant/backend/internal/funding"
)

func TestConditionalReplayCombinesProjectedCarrySpreadAndCostOnce(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	points := intervalReplayPoints(now, 24*time.Hour, 8, 0.002)
	longHistory := fundingHistory(now, 0.0001)
	shortHistory := fundingHistory(now, 0.0005)
	input := replayInputFromPoints(Period24h, points, longHistory, shortHistory, now)
	result, err := Replay(input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Coverage != 1 || result.SampleCount < 3 {
		t.Fatalf("samples=%d coverage=%f", result.SampleCount, result.Coverage)
	}
	want := result.FundingExpectedAnnualized + result.SpreadExpectedAnnualized
	if math.Abs(result.CombinedExpectedAnnualized-want) > 1e-12 {
		t.Fatalf("annualized decomposition=%+v", result)
	}
	if result.ProjectedFundingCarry <= 0 ||
		result.PeriodExpectedReturn != result.ProjectedFundingCarry+
			result.ConditionalSpreadReturn-0.0012 {
		t.Fatalf("net return decomposition=%+v", result)
	}
	if !finite(result.Score) || result.Confidence <= 0 || result.Confidence > 1 {
		t.Fatalf("score/confidence=%+v", result)
	}
}

func TestConditionalReplayEightHourRequiresTwelveAndSeventyPercentCoverage(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	history := fundingHistory(now, 0.0001)
	insufficient := intervalReplayPoints(now, 8*time.Hour, 13, 0.001)
	if _, err := Replay(replayInputFromPoints(
		Period8h, insufficient, history, history, now,
	)); err == nil {
		t.Fatal("12 windows are below 70% of the complete 21-window lookback")
	}
	sufficient := intervalReplayPoints(now, 8*time.Hour, 16, 0.001)
	result, err := Replay(replayInputFromPoints(
		Period8h, sufficient, history, history, now,
	))
	if err != nil {
		t.Fatal(err)
	}
	if result.Coverage < 0.7 {
		t.Fatalf("coverage=%f", result.Coverage)
	}
}

func TestConditionalReplayAllowsTwoMinuteEndpointTolerance(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	points := intervalReplayPoints(now, 24*time.Hour, 8, 0.001)
	for index := range points {
		points[index].TS = points[index].TS.Add(-2 * time.Minute)
		points[index].Long.TS = points[index].TS
		points[index].Short.TS = points[index].TS
	}
	history := fundingHistory(now, 0.0001)
	if _, err := Replay(replayInputFromPoints(
		Period24h, points, history, history, now,
	)); err != nil {
		t.Fatal(err)
	}
}

func TestConditionalReplayRejectsThreeMinuteEndpointGap(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	points := intervalReplayPoints(now, 24*time.Hour, 8, 0.001)
	for index := range points {
		points[index].TS = points[index].TS.Add(-3 * time.Minute)
	}
	history := fundingHistory(now, 0.0001)
	if _, err := Replay(replayInputFromPoints(
		Period24h, points, history, history, now,
	)); err == nil {
		t.Fatal("three-minute endpoint gap must be rejected")
	}
}

func TestConditionalReplayAllNegativeReturnsNeverPaysBack(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	points := intervalReplayPoints(now, 24*time.Hour, 8, 0)
	history := fundingHistory(now, 0.0001)
	input := replayInputFromPoints(Period24h, points, history, history, now)
	input.ShortRate.Rate = input.LongRate.Rate
	result, err := Replay(input)
	if err != nil {
		t.Fatal(err)
	}
	if result.PeriodExpectedReturn >= 0 || result.ProfitProbability != 0 ||
		result.PaybackStatus != PaybackNever {
		t.Fatalf("negative result=%+v", result)
	}
}

func TestProjectedCarryUsesActualOneHourAndEightHourSchedules(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	history := []funding.HistoryPoint{
		{Rate: 0.0001, SettledAt: now.Add(-3 * time.Hour)},
		{Rate: 0.0001, SettledAt: now.Add(-2 * time.Hour)},
		{Rate: 0.0001, SettledAt: now.Add(-time.Hour)},
	}
	oneHour, ok := projectFundingCarry(funding.Rate{
		Rate: 0.0001, IntervalHours: 1, FundingTime: now.Add(time.Hour),
	}, history, now, now.Add(8*time.Hour))
	if !ok {
		t.Fatal("one-hour carry rejected")
	}
	eightHour, ok := projectFundingCarry(funding.Rate{
		Rate: 0.0001, IntervalHours: 8, FundingTime: now.Add(time.Hour),
	}, history, now, now.Add(8*time.Hour))
	if !ok || oneHour <= eightHour {
		t.Fatalf("one-hour=%f eight-hour=%f", oneHour, eightHour)
	}
}

func TestConditionalReplayRejectsMissingFundingHistoryOrSchedule(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	points := intervalReplayPoints(now, 24*time.Hour, 8, 0.001)
	input := replayInputFromPoints(Period24h, points, nil, nil, now)
	if _, err := Replay(input); err == nil {
		t.Fatal("missing robust funding history must be rejected")
	}
	input.LongFunding = fundingHistory(now, 0.0001)
	input.ShortFunding = fundingHistory(now, 0.0001)
	input.LongRate.FundingTime = time.Time{}
	if _, err := Replay(input); err == nil {
		t.Fatal("missing settlement schedule must be rejected")
	}
}

func TestProjectFundingCarryUsesNextRateThenDecaysToRobustCenter(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	next := 0.001
	rate := funding.Rate{
		Rate: 0.0001, NextRate: &next, IntervalHours: 8,
		FundingTime: now.Add(time.Hour),
	}
	carry, ok := projectFundingCarry(
		rate, fundingHistory(now, 0.0001), now, now.Add(24*time.Hour),
	)
	if !ok || carry <= next || carry >= 3*next {
		t.Fatalf("carry=%f ok=%v", carry, ok)
	}
}

func replayInputFromPoints(
	period Period,
	points []PairPoint,
	longFunding, shortFunding []funding.HistoryPoint,
	dataThrough time.Time,
) ReplayInput {
	longQuotes := make([]MinuteQuote, 0, len(points))
	shortQuotes := make([]MinuteQuote, 0, len(points))
	for _, point := range points {
		longQuotes = append(longQuotes, MinuteQuote{
			Minute: point.TS.Unix() / 60, Bid: point.Long.Bid, Ask: point.Long.Ask,
		})
		shortQuotes = append(shortQuotes, MinuteQuote{
			Minute: point.TS.Unix() / 60, Bid: point.Short.Bid, Ask: point.Short.Ask,
		})
	}
	fromMinute := dataThrough.Add(-replayLookback-time.Minute).Unix() / 60
	toMinute := dataThrough.Add(time.Minute).Unix() / 60
	longSeries := newLegSeries(appendBlocks(
		nil, normalizeMinutes(longQuotes, fromMinute), replayQuoteBlockSize,
	))
	shortSeries := newLegSeries(appendBlocks(
		nil, normalizeMinutes(shortQuotes, fromMinute), replayQuoteBlockSize,
	))
	return ReplayInput{
		Period: period,
		LongRate: funding.Rate{
			Rate: 0.0001, IntervalHours: 8, FundingTime: dataThrough.Add(time.Hour),
		},
		ShortRate: funding.Rate{
			Rate: 0.0005, IntervalHours: 8, FundingTime: dataThrough.Add(time.Hour),
		},
		LongQuotes: MinuteSeriesView{
			series: longSeries, fromMinute: fromMinute, toMinute: toMinute,
		},
		ShortQuotes: MinuteSeriesView{
			series: shortSeries, fromMinute: fromMinute, toMinute: toMinute,
		},
		LongFunding: longFunding, ShortFunding: shortFunding,
		DataThrough: dataThrough, MinTurnover24: 10_000_000,
	}
}

func fundingHistory(now time.Time, rate float64) []funding.HistoryPoint {
	result := make([]funding.HistoryPoint, 0, 21)
	for index := 21; index > 0; index-- {
		result = append(result, funding.HistoryPoint{
			Rate: rate, SettledAt: now.Add(-time.Duration(index) * 8 * time.Hour),
		})
	}
	return result
}

func intervalReplayPoints(
	now time.Time, step time.Duration, count int, decline float64,
) []PairPoint {
	result := make([]PairPoint, 0, count)
	for index := 0; index < count; index++ {
		ts := now.Add(-time.Duration(count-1-index) * step)
		short := 110 * math.Pow(1-decline, float64(index))
		result = append(result, PairPoint{
			TS: ts,
			Long: Quote{
				TS: ts, Symbol: "BTCUSDT", Venue: "binance", Bid: 100, Ask: 100,
			},
			Short: Quote{
				TS: ts, Symbol: "BTCUSDT", Venue: "okx", Bid: short, Ask: short,
			},
		})
	}
	return result
}
