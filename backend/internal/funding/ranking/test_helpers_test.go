package ranking

import (
	"math"
	"time"
)

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
