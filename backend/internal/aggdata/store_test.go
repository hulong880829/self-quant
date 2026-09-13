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

func TestStoreLearnsSpotAndPerpSameVenuesSequentially(t *testing.T) {
	store := NewStore()
	spot := "/selfquant.mds.agg_spot_usdt_binance-bitget-bybit-gate-okx.btcusdt.aggbbo.2"
	perp := "/selfquant.mds.agg_perp_usdt_binance-bitget-bybit-gate-okx.btcusdt.aggbbo.2"
	store.Reconcile(&catalogSnapshot{Markets: []catalogMarket{
		{
			Identity: Identity{
				Profile: "agg_spot_usdt_binance-bitget-bybit-gate-okx",
				Symbol:  "BTCUSDT",
			},
			Segments: map[Kind]string{KindBBO: spot},
		},
		{
			Identity: Identity{
				Profile: "agg_perp_usdt_binance-bitget-bybit-gate-okx",
				Symbol:  "BTCUSDT",
			},
			Segments: map[Kind]string{KindBBO: perp},
		},
	}})

	store.BeginLearning(perp)
	perpFrame, err := DecodeGatewayFrame(
		gatewayFrameForTest(KindBBO, 1, 6, validGatewayBBOPayload()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(perpFrame); err != nil {
		t.Fatalf("Apply perp: %v", err)
	}
	if !store.SegmentMapped(perp) {
		t.Fatal("perp segment was not mapped")
	}
	if store.SegmentMapped(spot) {
		t.Fatal("spot segment mapped before it was learned")
	}
	perpMarket, err := store.LookupProfile(
		"agg_perp_usdt_binance-bitget-bybit-gate-okx", "BTCUSDT",
	)
	if err != nil || perpMarket == nil {
		t.Fatalf("perp profile lookup failed: %v", err)
	}
	if snapshot := perpMarket.bbo.value.Load(); snapshot == nil || !snapshot.Ready {
		t.Fatalf("perp BBO was not bound: %+v", snapshot)
	}
	spotMarket, err := store.LookupProfile(
		"agg_spot_usdt_binance-bitget-bybit-gate-okx", "BTCUSDT",
	)
	if err != nil || spotMarket == nil {
		t.Fatalf("spot catalog lookup failed: %v", err)
	}
	if snapshot := spotMarket.bbo.value.Load(); snapshot != nil && snapshot.Ready {
		t.Fatal("spot BBO was bound from the perp topic")
	}

	store.BeginLearning(spot)
	spotFrame, err := DecodeGatewayFrame(
		gatewayFrameForTest(KindBBO, 1, 0, validGatewayBBOPayload()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(spotFrame); err != nil {
		t.Fatalf("Apply spot: %v", err)
	}
	if !store.SegmentMapped(spot) {
		t.Fatal("spot segment was not mapped")
	}
	if snapshot := spotMarket.bbo.value.Load(); snapshot == nil || !snapshot.Ready {
		t.Fatalf("spot BBO was not bound: %+v", snapshot)
	}
	if market, err := store.Lookup("BTCUSDT"); err == nil || market != nil {
		t.Fatal("ambiguous public symbol lookup unexpectedly succeeded")
	}
}

func TestStoreReconcilePreservesSpotAndPerpTopicMappings(t *testing.T) {
	store := NewStore()
	spot := "/selfquant.mds.agg_spot_usdt_binance-bitget-bybit-gate-okx.btcusdt.aggbbo.2"
	perp := "/selfquant.mds.agg_perp_usdt_binance-bitget-bybit-gate-okx.btcusdt.aggbbo.2"
	catalog := &catalogSnapshot{Markets: []catalogMarket{
		{
			Identity: Identity{
				Profile: "agg_spot_usdt_binance-bitget-bybit-gate-okx",
				Symbol:  "BTCUSDT",
			},
			Segments: map[Kind]string{KindBBO: spot},
		},
		{
			Identity: Identity{
				Profile: "agg_perp_usdt_binance-bitget-bybit-gate-okx",
				Symbol:  "BTCUSDT",
			},
			Segments: map[Kind]string{KindBBO: perp},
		},
	}}
	store.Reconcile(catalog)

	decode := func(topic uint16) GatewayFrame {
		frame, err := DecodeGatewayFrame(
			gatewayFrameForTest(KindBBO, 1, topic, validGatewayBBOPayload()),
		)
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}
	perpFrame := decode(6)
	store.BeginLearning(perp)
	if err := store.Apply(perpFrame); err != nil {
		t.Fatal(err)
	}
	spotFrame := decode(7)
	store.BeginLearning(spot)
	if err := store.Apply(spotFrame); err != nil {
		t.Fatal(err)
	}

	store.Reconcile(catalog)
	if !store.SegmentMapped(spot) || !store.SegmentMapped(perp) {
		t.Fatal("unchanged topic mappings were not preserved across reconcile")
	}
	spotFrame.RingSequence++
	spotFrame.Generation++
	spotFrame.WallNS++
	perpFrame.RingSequence++
	perpFrame.Generation++
	perpFrame.WallNS++
	if err := store.Apply(spotFrame); err != nil {
		t.Fatalf("Apply spot after reconcile: %v", err)
	}
	if err := store.Apply(perpFrame); err != nil {
		t.Fatalf("Apply perp after reconcile: %v", err)
	}
	spotMarket, _ := store.LookupProfile(
		"agg_spot_usdt_binance-bitget-bybit-gate-okx", "BTCUSDT",
	)
	perpMarket, _ := store.LookupProfile(
		"agg_perp_usdt_binance-bitget-bybit-gate-okx", "BTCUSDT",
	)
	if got := spotMarket.bbo.value.Load(); got == nil ||
		got.RingSequence != spotFrame.RingSequence {
		t.Fatalf("spot snapshot did not advance after reconcile: %+v", got)
	}
	if got := perpMarket.bbo.value.Load(); got == nil ||
		got.RingSequence != perpFrame.RingSequence {
		t.Fatalf("perp snapshot did not advance after reconcile: %+v", got)
	}
}

func TestStoreReconcileKeepsAmbiguousFairPricesAdvancing(t *testing.T) {
	config := DefaultFairPriceConfig()
	config.DepthK = 1
	store, err := NewStoreWithFairPrice(config)
	if err != nil {
		t.Fatal(err)
	}
	spot := "/selfquant.mds.agg_spot_usdt_binance-bitget-bybit-gate-okx.btcusdt.aggorderbook.2"
	perp := "/selfquant.mds.agg_perp_usdt_binance-bitget-bybit-gate-okx.btcusdt.aggorderbook.2"
	catalog := &catalogSnapshot{Markets: []catalogMarket{
		{
			Identity: Identity{
				Profile: "agg_spot_usdt_binance-bitget-bybit-gate-okx",
				Symbol:  "BTCUSDT",
			},
			Segments: map[Kind]string{KindBook: spot},
		},
		{
			Identity: Identity{
				Profile: "agg_perp_usdt_binance-bitget-bybit-gate-okx",
				Symbol:  "BTCUSDT",
			},
			Segments: map[Kind]string{KindBook: perp},
		},
	}}
	store.Reconcile(catalog)
	book := fairTestSnapshot(
		[]Level{{Price: 10_000, Quantity: 10}},
		[]Level{{Price: 10_100, Quantity: 10}},
		1,
	).Book
	book.Base, book.Quote = "BTC", "USDT"
	book.VenueSlotIDs[0] = 1
	spotFrame := GatewayFrame{
		Kind: KindBook, TopicID: 10, RingEpoch: 1, RingSequence: 1,
		Generation: 1, WallNS: 1, Book: book,
	}
	perpBook := *book
	perpFrame := GatewayFrame{
		Kind: KindBook, TopicID: 11, RingEpoch: 2, RingSequence: 1,
		Generation: 1, WallNS: 1, Book: &perpBook,
	}
	store.BeginLearning(spot)
	if err := store.Apply(spotFrame); err != nil {
		t.Fatal(err)
	}
	store.BeginLearning(perp)
	if err := store.Apply(perpFrame); err != nil {
		t.Fatal(err)
	}

	store.Reconcile(catalog)
	spotFrame.RingSequence, spotFrame.Generation, spotFrame.WallNS = 2, 2, 2
	perpFrame.RingSequence, perpFrame.Generation, perpFrame.WallNS = 2, 2, 2
	if err := store.Apply(spotFrame); err != nil {
		t.Fatalf("Apply spot after reconcile: %v", err)
	}
	if err := store.Apply(perpFrame); err != nil {
		t.Fatalf("Apply perp after reconcile: %v", err)
	}
	spotMarket, _ := store.LookupProfile(
		"agg_spot_usdt_binance-bitget-bybit-gate-okx", "BTCUSDT",
	)
	perpMarket, _ := store.LookupProfile(
		"agg_perp_usdt_binance-bitget-bybit-gate-okx", "BTCUSDT",
	)
	if fair := spotMarket.fair.Load(); fair == nil || !fair.Ready ||
		fair.RingSequence != 2 {
		t.Fatalf("spot fair price did not advance: %+v", fair)
	}
	if fair := perpMarket.fair.Load(); fair == nil || !fair.Ready ||
		fair.RingSequence != 2 {
		t.Fatalf("perp fair price did not advance: %+v", fair)
	}
}

func TestStoreReconcileInvalidatesChangedSegment(t *testing.T) {
	store := NewStore()
	oldSegment := "/sq.agg_binance.btcusdt.aggbbo.2"
	newSegment := "/sq.agg_binance_v2.btcusdt.aggbbo.2"
	catalog := &catalogSnapshot{Markets: []catalogMarket{{
		Identity: Identity{Profile: "agg_binance", Symbol: "BTCUSDT"},
		Segments: map[Kind]string{KindBBO: oldSegment},
	}}}
	store.Reconcile(catalog)
	store.BeginLearning(oldSegment)
	frame, err := DecodeGatewayFrame(
		gatewayFrameForTest(KindBBO, 1, 8, validGatewayBBOPayload()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(frame); err != nil {
		t.Fatal(err)
	}

	store.Reconcile(&catalogSnapshot{Markets: []catalogMarket{{
		Identity: Identity{Profile: "agg_binance", Symbol: "BTCUSDT"},
		Segments: map[Kind]string{KindBBO: newSegment},
	}}})
	if store.SegmentMapped(oldSegment) || store.SegmentMapped(newSegment) {
		t.Fatal("changed segment inherited an old topic mapping")
	}
	market, err := store.Lookup("BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := market.bbo.value.Load(); snapshot != nil {
		t.Fatalf("changed segment inherited an old snapshot: %+v", snapshot)
	}
}

func TestStoreSkipsAmbiguousFrameWithoutError(t *testing.T) {
	store := NewStore()
	spot := "/selfquant.mds.agg_spot_usdt_binance-bitget-bybit-gate-okx.btcusdt.aggbbo.2"
	perp := "/selfquant.mds.agg_perp_usdt_binance-bitget-bybit-gate-okx.btcusdt.aggbbo.2"
	eth := "/selfquant.mds.agg_perp_usdt_binance-bitget-bybit-gate-okx.ethusdt.aggbbo.2"
	store.Reconcile(&catalogSnapshot{Markets: []catalogMarket{
		{
			Identity: Identity{
				Profile: "agg_spot_usdt_binance-bitget-bybit-gate-okx",
				Symbol:  "BTCUSDT",
			},
			Segments: map[Kind]string{KindBBO: spot},
		},
		{
			Identity: Identity{
				Profile: "agg_perp_usdt_binance-bitget-bybit-gate-okx",
				Symbol:  "BTCUSDT",
			},
			Segments: map[Kind]string{KindBBO: perp},
		},
	}})

	frame, err := DecodeGatewayFrame(
		gatewayFrameForTest(KindBBO, 1, 6, validGatewayBBOPayload()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Apply(frame); err != nil {
		t.Fatalf("unlearned ambiguous Apply: %v", err)
	}
	if store.SegmentMapped(spot) || store.SegmentMapped(perp) {
		t.Fatal("ambiguous frame mapped a segment without learning")
	}

	store.BeginLearning(eth)
	if err := store.Apply(frame); err != nil {
		t.Fatalf("mismatched learning Apply: %v", err)
	}
	if store.SegmentMapped(spot) || store.SegmentMapped(perp) {
		t.Fatal("frame mapped while learning a different segment")
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

func TestStoreReadinessRequiresFreshSnapshots(t *testing.T) {
	store := NewStore()
	store.SetStaleAfter(15 * time.Second)
	segment := "/sq.agg_binance.btcusdt.aggbbo.2"
	store.Reconcile(&catalogSnapshot{Markets: []catalogMarket{{
		Identity: Identity{Profile: "agg_binance", Symbol: "BTCUSDT"},
		Segments: map[Kind]string{KindBBO: segment},
	}}})
	market, err := store.Lookup("BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	market.bbo.value.Store(&Snapshot{
		Kind: KindBBO, Ready: true, TopicID: 1,
		WallNS: uint64(now.Add(-16 * time.Second).UnixNano()),
	})
	if store.ReadyAt(now) {
		t.Fatal("stale snapshot was reported ready")
	}
	market.bbo.value.Store(&Snapshot{
		Kind: KindBBO, Ready: true, TopicID: 1,
		WallNS: uint64(now.Add(-time.Second).UnixNano()),
	})
	if !store.ReadyAt(now) {
		t.Fatal("fresh snapshot was not reported ready")
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
