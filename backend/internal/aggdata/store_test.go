package aggdata

import (
	"math"
	"sync"
	"testing"
	"time"
)

func TestCurrentHourSampleRingRetainsFullHourAtGatewayRate(t *testing.T) {
	ring := newSampleRing(currentHourSampleCapacity)
	hour := time.Date(2026, 8, 15, 6, 0, 0, 0, time.UTC)
	const interval = 50 * time.Millisecond

	for index := range currentHourSampleCapacity {
		ring.add(LiveSample{
			WallNS:   uint64(hour.Add(time.Duration(index) * interval).UnixNano()),
			GatedBPS: float64(index),
		})
	}

	samples := ring.snapshot()
	if len(samples) != currentHourSampleCapacity {
		t.Fatalf("samples=%d want=%d", len(samples), currentHourSampleCapacity)
	}
	if got := samples[0].WallNS; got != uint64(hour.UnixNano()) {
		t.Fatalf("first sample=%d want=%d", got, hour.UnixNano())
	}
	if got := samples[len(samples)-1].WallNS; got != uint64(hour.Add(time.Hour-interval).UnixNano()) {
		t.Fatalf("last sample=%d want=%d", got, hour.Add(time.Hour-interval).UnixNano())
	}
}

func TestCurrentHourSampleRingResetsAtHourBoundary(t *testing.T) {
	ring := newSampleRing(currentHourSampleCapacity)
	hour := time.Date(2026, 8, 15, 6, 0, 0, 0, time.UTC)
	ring.add(LiveSample{WallNS: uint64(hour.UnixNano()), GatedBPS: 1})
	ring.add(LiveSample{WallNS: uint64(hour.Add(time.Hour).UnixNano()), GatedBPS: 2})

	samples := ring.snapshot()
	if len(samples) != 1 || samples[0].GatedBPS != 2 {
		t.Fatalf("unexpected next-hour samples: %+v", samples)
	}
}

func TestStoreLearnsAmbiguousTopicSequentially(t *testing.T) {
	store := NewStore()
	first := "/sq.agg_first_binance.btc_usdt.aggbbo.2"
	second := "/sq.agg_second_binance.btc_usdt.aggbbo.2"
	store.Reconcile(&catalogSnapshot{Markets: []catalogMarket{
		{
			Identity: Identity{Profile: "agg_first_binance", Symbol: "BTC_USDT"},
			Segments: map[Kind]string{KindBBO: first},
		},
		{
			Identity: Identity{Profile: "agg_second_binance", Symbol: "BTC_USDT"},
			Segments: map[Kind]string{KindBBO: second},
		},
	}})
	store.BeginLearning(second)
	frame, err := DecodeGatewayFrame(gatewayFrameForTest(KindBBO, 1, 8, validGatewayBBOPayload()))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(frame); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !store.SegmentMapped(second) {
		t.Fatal("learning segment was not mapped")
	}
	market, err := store.Lookup("BTC_USDT")
	if err == nil || market != nil {
		t.Fatal("ambiguous public symbol lookup unexpectedly succeeded")
	}
	market, err = store.LookupProfile("agg_second_binance", "btc-usdt")
	if err != nil || market == nil ||
		market.catalog.Profile != "agg_second_binance" {
		t.Fatalf("profile lookup failed: market=%+v err=%v", market, err)
	}
}

func TestSignedSpreadAllowsCrossedMarket(t *testing.T) {
	value, err := signedSpreadBPS(102, 100)
	if err != nil {
		t.Fatal(err)
	}
	if value >= 0 {
		t.Fatalf("expected negative spread, got %f", value)
	}
}

func TestSignedSpreadPreservesFractionalBPS(t *testing.T) {
	value, err := signedSpreadBPS(652_054, 652_138)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(value-1.288) > 0.001 {
		t.Fatalf("unexpected fractional spread: %.6f", value)
	}
}

func TestStoreIgnoresRepeatedLatestGatewayImage(t *testing.T) {
	store := NewStore()
	segment := "/sq.agg_binance.btcusdt.aggbbo.2"
	store.Reconcile(&catalogSnapshot{Markets: []catalogMarket{{
		Identity: Identity{Profile: "agg_binance", Symbol: "BTCUSDT"},
		Segments: map[Kind]string{KindBBO: segment},
	}}})
	store.BeginLearning(segment)
	frame, err := DecodeGatewayFrame(
		gatewayFrameForTest(KindBBO, 1, 8, validGatewayBBOPayload()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(frame); err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(frame); err != nil {
		t.Fatalf("repeated latest image must be idempotent: %v", err)
	}
}

func TestStorePublishesFairPriceAndAcceptsNewEpoch(t *testing.T) {
	config := DefaultFairPriceConfig()
	config.DepthK = 1
	store, err := NewStoreWithFairPrice(config)
	if err != nil {
		t.Fatal(err)
	}
	segment := "/sq.agg_binance.btcusdt.aggorderbook.2"
	store.Reconcile(&catalogSnapshot{Markets: []catalogMarket{{
		Identity: Identity{Profile: "agg_binance", Symbol: "BTCUSDT"},
		Segments: map[Kind]string{KindBook: segment},
	}}})
	store.BeginLearning(segment)
	snapshot := fairTestSnapshot(
		[]Level{{Price: 10_000, Quantity: 10}},
		[]Level{{Price: 10_100, Quantity: 10}},
		1_000_000_000,
	)
	book := snapshot.Book
	book.Base, book.Quote = "BTC", "USDT"
	book.VenueSlotIDs[0] = 1
	frame := GatewayFrame{
		Kind: KindBook, Format: 2, TopicID: 5,
		RingEpoch: 100, RingSequence: 900, Generation: 1,
		WallNS: 1_000_000_000, Book: book,
	}
	if err := store.Apply(frame); err != nil {
		t.Fatal(err)
	}
	market, err := store.Lookup("BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	fair := market.fair.Load()
	if fair == nil || !fair.Ready || fair.RingEpoch != 100 || fair.RingSequence != 900 {
		t.Fatalf("unexpected fair snapshot: %+v", fair)
	}

	frame.RingEpoch = 101
	frame.RingSequence = 1
	frame.Generation = 2
	frame.WallNS = 2_000_000_000
	if err := store.Apply(frame); err != nil {
		t.Fatalf("new epoch with lower sequence was rejected: %v", err)
	}
	fair = market.fair.Load()
	if fair.RingEpoch != 101 || fair.RingSequence != 1 {
		t.Fatalf("fair identity did not advance epoch: %+v", fair)
	}
}

func TestStoreFairPriceResetRecoveryAndConcurrentReconcile(t *testing.T) {
	config := DefaultFairPriceConfig()
	config.DepthK = 1
	store, err := NewStoreWithFairPrice(config)
	if err != nil {
		t.Fatal(err)
	}
	segment := "/sq.agg_binance.btcusdt.aggorderbook.2"
	catalog := &catalogSnapshot{Markets: []catalogMarket{{
		Identity: Identity{Profile: "agg_binance", Symbol: "BTCUSDT"},
		Segments: map[Kind]string{KindBook: segment},
	}}}
	store.Reconcile(catalog)
	store.BeginLearning(segment)
	book := fairTestSnapshot(
		[]Level{{Price: 99, Quantity: 10}},
		[]Level{{Price: 101, Quantity: 10}},
		1,
	).Book
	frame := GatewayFrame{
		Kind: KindBook, TopicID: 6, RingEpoch: 1, RingSequence: 1,
		Generation: 1, WallNS: 1, Book: book,
	}
	if err := store.Apply(frame); err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(GatewayFrame{
		Kind: KindReset, TopicID: 6, RingEpoch: 1, RingSequence: 2, Generation: 2,
	}); err != nil {
		t.Fatal(err)
	}
	market, err := store.Lookup("BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if fair := market.fair.Load(); fair == nil || fair.Ready ||
		fair.ResetReason != "orderbook_reset" {
		t.Fatalf("unexpected reset state: %+v", fair)
	}
	frame.RingSequence, frame.Generation, frame.WallNS = 3, 3, 3
	if err := store.Apply(frame); err != nil {
		t.Fatal(err)
	}
	market, _ = store.Lookup("BTCUSDT")
	if fair := market.fair.Load(); fair == nil || !fair.Ready {
		t.Fatalf("fair price did not recover: %+v", fair)
	}

	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		for range 100 {
			store.Reconcile(catalog)
		}
	}()
	for sequence := uint64(4); sequence < 104; sequence++ {
		frame.RingEpoch = sequence
		frame.RingSequence = 1
		frame.Generation = sequence
		frame.WallNS = sequence
		if err := store.Apply(frame); err != nil {
			t.Errorf("concurrent Apply: %v", err)
			break
		}
	}
	wait.Wait()
}
