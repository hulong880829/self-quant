package aggdata

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func fairTestSnapshot(bids, asks []Level, wallNS uint64) *Snapshot {
	return &Snapshot{
		Profile: "agg", Symbol: "BTCUSDT", Kind: KindBook,
		RingEpoch: 7, RingSequence: wallNS, Generation: wallNS, WallNS: wallNS,
		Ready: true,
		Book: &Book{
			ExchangeTSNS: wallNS - min(wallNS, uint64(1)),
			Base:         "BTC", Quote: "USDT",
			PriceScale: 0, QuantityScale: 0,
			MemberCount: 2, MemberMask: 3, ActiveMask: 3,
			Bids: bids, Asks: asks,
		},
	}
}

func fairTestEngine(t *testing.T, update func(*FairPriceConfig)) *fairPriceEngine {
	t.Helper()
	config := DefaultFairPriceConfig()
	config.DepthK = 2
	if update != nil {
		update(&config)
	}
	engine, err := newFairPriceEngine(config)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestFairPriceSymmetricAndImbalancedBooks(t *testing.T) {
	engine := fairTestEngine(t, func(config *FairPriceConfig) {
		config.LambdaPerBP = 0
	})
	symmetric, _ := engine.compute("agg", "BTCUSDT", fairTestSnapshot(
		[]Level{{Price: 99, Quantity: 10}, {Price: 98, Quantity: 10}},
		[]Level{{Price: 101, Quantity: 10}, {Price: 102, Quantity: 10}}, 1,
	), nil)
	if !symmetric.Ready || symmetric.PriceRaw.Mantissa != 100000 ||
		symmetric.Mid.Mantissa != 100000 || symmetric.CrossBPS != nil {
		t.Fatalf("symmetric=%+v", symmetric)
	}

	imbalanced, _ := engine.compute("agg", "BTCUSDT", fairTestSnapshot(
		[]Level{{Price: 99, Quantity: 30}, {Price: 98, Quantity: 30}},
		[]Level{{Price: 101, Quantity: 10}, {Price: 102, Quantity: 10}}, 2,
	), nil)
	if !imbalanced.Ready || imbalanced.PriceRaw.Mantissa != 100500 ||
		math.Abs(imbalanced.Imbalance-0.75) > 1e-12 {
		t.Fatalf("imbalanced=%+v", imbalanced)
	}
}

func TestFairPriceVirtualUncross(t *testing.T) {
	engine := fairTestEngine(t, nil)
	value, _ := engine.compute("agg", "BTCUSDT", fairTestSnapshot(
		[]Level{{Price: 101, Quantity: 2}, {Price: 99, Quantity: 3}},
		[]Level{{Price: 100, Quantity: 1}, {Price: 102, Quantity: 4}}, 1,
	), nil)
	if !value.Ready || !value.Crossed || value.CrossBPS == nil ||
		value.CrossedQuantity == nil || value.CrossedQuantity.Mantissa != 1 ||
		value.EffectiveBid.Mantissa != 101000 || value.EffectiveAsk.Mantissa != 102000 ||
		!value.Degraded {
		t.Fatalf("value=%+v", value)
	}
	var payload map[string]any
	if err := json.Unmarshal(value.JSON, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["channel"] != "fairprice" || payload["ring_epoch"] != "7" {
		t.Fatalf("payload=%v", payload)
	}
}

func TestFairPriceRejectsExhaustedAndInactiveBooks(t *testing.T) {
	engine := fairTestEngine(t, nil)
	exhausted, _ := engine.compute("agg", "BTCUSDT", fairTestSnapshot(
		[]Level{{Price: 101, Quantity: 1}},
		[]Level{{Price: 100, Quantity: 1}}, 1,
	), nil)
	if exhausted.Ready || exhausted.ResetReason != "uncross_depth_exhausted" {
		t.Fatalf("exhausted=%+v", exhausted)
	}
	inactiveSnapshot := fairTestSnapshot(
		[]Level{{Price: 99, Quantity: 1}},
		[]Level{{Price: 101, Quantity: 1}}, 2,
	)
	inactiveSnapshot.Book.ActiveMask = 0
	inactive, _ := engine.compute("agg", "BTCUSDT", inactiveSnapshot, nil)
	if inactive.Ready || inactive.ResetReason != "no_active_venues" {
		t.Fatalf("inactive=%+v", inactive)
	}
}

func TestFairPriceEWMAUsesWallTimeAndSpreadClamp(t *testing.T) {
	engine := fairTestEngine(t, func(config *FairPriceConfig) {
		config.DepthK = 1
		config.LambdaPerBP = 0
		config.EWMATau = time.Second
	})
	first, state := engine.compute("agg", "BTCUSDT", fairTestSnapshot(
		[]Level{{Price: 99, Quantity: 10}},
		[]Level{{Price: 101, Quantity: 10}}, uint64(time.Second),
	), nil)
	second, _ := engine.compute("agg", "BTCUSDT", fairTestSnapshot(
		[]Level{{Price: 109, Quantity: 100}},
		[]Level{{Price: 111, Quantity: 1}}, uint64(2*time.Second),
	), state)
	if !first.Ready || !second.Ready {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	if second.Price.Mantissa != 109000 || second.PriceRaw.Mantissa <= second.Price.Mantissa {
		t.Fatalf("second=%+v", second)
	}
}

func TestFairPriceImpactAndFixedDecimal(t *testing.T) {
	engine := fairTestEngine(t, func(config *FairPriceConfig) {
		config.DepthK = 1
		config.ImpactNotional = 500
	})
	value, _ := engine.compute("agg", "BTCUSDT", fairTestSnapshot(
		[]Level{{Price: 99, Quantity: 10}},
		[]Level{{Price: 101, Quantity: 2}}, 1,
	), nil)
	if !value.Ready || value.ImpactBid == nil || value.ImpactAsk == nil ||
		!containsReason(value.DegradedReasons, "impact_depth_insufficient") {
		t.Fatalf("value=%+v", value)
	}
	if got := fixedDecimalString(FixedValue{Mantissa: -123, Scale: 5}); got != "-0.00123" {
		t.Fatalf("decimal=%q", got)
	}
}

func TestFairPriceQualityAndNumericGuards(t *testing.T) {
	engine := fairTestEngine(t, func(config *FairPriceConfig) {
		config.DepthK = 1
		config.VenueDominanceRatio = 0.80
	})
	snapshot := fairTestSnapshot(
		[]Level{{Price: 99, Quantity: 10, VenueQuantity: [8]int64{9, 1}}},
		[]Level{{Price: 101, Quantity: 10, VenueQuantity: [8]int64{9, 1}}},
		1,
	)
	snapshot.Book.ActiveMask = 1
	value, _ := engine.compute("agg", "BTCUSDT", snapshot, nil)
	if !value.Ready ||
		!containsReason(value.DegradedReasons, "inactive_venues") ||
		!containsReason(value.DegradedReasons, "single_venue_dominant") {
		t.Fatalf("quality reasons=%v", value.DegradedReasons)
	}

	invalid := fairTestSnapshot(
		[]Level{{Price: 99, Quantity: 0}},
		[]Level{{Price: 101, Quantity: 1}},
		2,
	)
	value, _ = engine.compute("agg", "BTCUSDT", invalid, nil)
	if value.Ready || value.ResetReason != "invalid_level" {
		t.Fatalf("invalid level result=%+v", value)
	}

	overflow := fairTestSnapshot(
		[]Level{{Price: math.MaxInt64 - 1, Quantity: 1}},
		[]Level{{Price: math.MaxInt64, Quantity: 1}},
		3,
	)
	value, _ = engine.compute("agg", "BTCUSDT", overflow, nil)
	if value.Ready || value.ResetReason != "fixed_overflow" {
		t.Fatalf("overflow result=%+v", value)
	}
}

func BenchmarkFairPriceComputeCrossed50(b *testing.B) {
	engine, err := newFairPriceEngine(DefaultFairPriceConfig())
	if err != nil {
		b.Fatal(err)
	}
	bids := make([]Level, maxDepth)
	asks := make([]Level, maxDepth)
	for index := range maxDepth {
		bids[index] = Level{Price: int64(10_100 - index), Quantity: 1}
		asks[index] = Level{Price: int64(10_000 + index), Quantity: 1}
	}
	snapshot := fairTestSnapshot(bids, asks, 1)
	b.ReportAllocs()
	for b.Loop() {
		engine.compute("agg", "BTCUSDT", snapshot, nil)
	}
}

func containsReason(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
