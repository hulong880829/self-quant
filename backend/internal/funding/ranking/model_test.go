package ranking

import (
	"math"
	"testing"
	"time"
)

func TestAnalyzeBuildsConditionalFirstPassageDistribution(t *testing.T) {
	now := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	points := syntheticPairPoints(now, 24*time.Hour, 28)
	result, err := Analyze(Period1h, points, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Paths) != simulationPaths {
		t.Fatalf("paths=%d", len(result.Paths))
	}
	if result.FirstPassageProbability < 0 || result.FirstPassageProbability > 1 {
		t.Fatalf("probability=%f", result.FirstPassageProbability)
	}
	if result.ExpectedExitMinutes <= 0 || result.ExpectedExitMinutes > 60 {
		t.Fatalf("exit=%f", result.ExpectedExitMinutes)
	}
	if math.Abs(result.CurrentMidSpreadBPS-result.TargetSpreadBPS) < 1 {
		t.Fatalf("current=%f target=%f", result.CurrentMidSpreadBPS, result.TargetSpreadBPS)
	}
}

func TestAnalyzeRejectsInsufficientHistory(t *testing.T) {
	_, err := Analyze(Period1h, syntheticPairPoints(time.Now().UTC(), time.Hour, 20), 1)
	if err == nil {
		t.Fatal("expected insufficient data error")
	}
}

func syntheticPairPoints(now time.Time, duration time.Duration, finalShock float64) []PairPoint {
	count := int(duration / time.Minute)
	result := make([]PairPoint, 0, count)
	for index := 0; index < count; index++ {
		ts := now.Add(-time.Duration(count-1-index) * time.Minute)
		spreadBPS := 5*math.Sin(float64(index)/18) + 2*math.Sin(float64(index)/5)
		if index == count-1 {
			spreadBPS += finalShock
		}
		longMid := 100.0
		shortMid := longMid * (1 + spreadBPS/10_000)
		result = append(result, PairPoint{
			TS: ts,
			Long: Quote{
				TS: ts, Symbol: "BTCUSDT", Venue: "binance",
				Bid: longMid - 0.005, Ask: longMid + 0.005,
			},
			Short: Quote{
				TS: ts, Symbol: "BTCUSDT", Venue: "okx",
				Bid: shortMid - 0.005, Ask: shortMid + 0.005,
			},
		})
	}
	return result
}
