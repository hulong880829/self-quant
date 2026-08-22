package polymarket

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSnapshotStoreConcurrentReadersAndWriters(t *testing.T) {
	store := NewSnapshotStore()
	market := Market{
		ID: "market-1", Asset: "BTC", Period: "5m", Active: true,
		WindowStart: time.Now().UTC().Add(-time.Minute),
		WindowEnd:   time.Now().UTC().Add(4 * time.Minute),
	}
	store.ReplaceMarkets([]Market{market})

	var group sync.WaitGroup
	for writer := 0; writer < 4; writer++ {
		group.Add(1)
		go func(writer int) {
			defer group.Done()
			for index := 0; index < 500; index++ {
				store.Update(Snapshot{
					Market: market, ChainlinkPrice: fmt.Sprintf("%d", writer*500+index),
					SourceUpdated: time.Now(),
				})
			}
		}(writer)
	}
	for reader := 0; reader < 12; reader++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := 0; index < 1000; index++ {
				_, _ = store.Get(market.ID)
				items, _ := store.ListMarkets("BTC", "5m", true)
				if len(items) != 1 {
					t.Errorf("market count=%d", len(items))
					return
				}
			}
		}()
	}
	group.Wait()
}

func TestReplaceMarketsPreservesOpenUnderConcurrentPriceUpdate(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	market := Market{
		ID: "market-open", Asset: "BTC", Period: "5m", Active: true,
		WindowStart: now.Add(-time.Minute),
		WindowEnd:   now.Add(4 * time.Minute),
	}
	store.ReplaceMarkets([]Market{market})

	var group sync.WaitGroup
	stop := make(chan struct{})
	group.Add(2)
	go func() {
		defer group.Done()
		for index := 0; index < 2000; index++ {
			select {
			case <-stop:
				return
			default:
			}
			store.UpdatePrice(Snapshot{
				Market: market, OpenPrice: "65000.12",
				ChainlinkPrice: "65001.00",
				SourceUpdated:  time.Now(),
			}, nil)
		}
	}()
	go func() {
		defer group.Done()
		for index := 0; index < 2000; index++ {
			select {
			case <-stop:
				return
			default:
			}
			store.ReplaceMarkets([]Market{market})
		}
	}()
	group.Wait()
	close(stop)

	snapshot, ok := store.Get(market.ID)
	if !ok {
		t.Fatal("snapshot missing")
	}
	if snapshot.OpenPrice != "65000.12" {
		t.Fatalf("open=%q want preserved under concurrent replace", snapshot.OpenPrice)
	}
}

func TestSnapshotStoreCommitCASWithReplaceMarkets(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	initial := Market{
		ID: "market-cas", Asset: "BTC", Period: "5m", Active: true,
		WindowStart: now.Add(-time.Minute),
		WindowEnd:   now.Add(4 * time.Minute),
		Title:       "initial",
	}
	updated := initial
	updated.Title = "updated"
	store.ReplaceMarkets([]Market{initial})

	var group sync.WaitGroup
	stop := make(chan struct{})
	group.Add(2)
	go func() {
		defer group.Done()
		for index := 0; index < 2000; index++ {
			select {
			case <-stop:
				return
			default:
			}
			store.UpdatePrice(Snapshot{
				Market: updated, OpenPrice: "64000.00",
				ChainlinkPrice: "64001.00",
				SourceUpdated:  time.Now(),
			}, nil)
		}
	}()
	go func() {
		defer group.Done()
		for index := 0; index < 2000; index++ {
			select {
			case <-stop:
				return
			default:
			}
			store.ReplaceMarkets([]Market{updated})
		}
	}()
	group.Wait()
	close(stop)

	snapshot, ok := store.Get(updated.ID)
	if !ok {
		t.Fatal("snapshot missing")
	}
	if snapshot.OpenPrice != "64000.00" {
		t.Fatalf("open=%q", snapshot.OpenPrice)
	}
	if snapshot.Market.Title != "updated" {
		t.Fatalf("market title=%q", snapshot.Market.Title)
	}
	markets, _ := store.ListMarkets("", "", false)
	if len(markets) != 1 || markets[0].Title != "updated" {
		t.Fatalf("markets=%+v", markets)
	}
}

func TestSnapshotSubscriptionDropsOldUpdateForSlowConsumer(t *testing.T) {
	store := NewSnapshotStore()
	updates, cancel := store.Subscribe("market-1")
	defer cancel()
	market := Market{ID: "market-1"}
	for index := 0; index < 10; index++ {
		store.Update(Snapshot{Market: market, ChainlinkPrice: fmt.Sprintf("%d", index)})
	}
	update := <-updates
	if update.Snapshot.ChainlinkPrice != "9" {
		t.Fatalf("latest update=%q", update.Snapshot.ChainlinkPrice)
	}
}

func TestPatchQuotesSkipsUnchangedAndPreservesSeries(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	market := Market{
		ID: "market-q", Asset: "BTC", Period: "5m", Active: true,
		WindowStart: now.Add(-time.Minute),
		WindowEnd:   now.Add(4 * time.Minute),
	}
	store.ReplaceMarkets([]Market{market})
	series := make([]PricePoint, 0, 50)
	for index := 0; index < 50; index++ {
		series = append(series, PricePoint{
			Timestamp:      now.Add(time.Duration(index) * time.Second),
			ChainlinkPrice: fmt.Sprintf("%d", 65000+index),
		})
	}
	store.Update(Snapshot{
		Market: market, ChainlinkPrice: "65000", Series: series, UpBid: "0.40",
	})

	before, _ := store.Get(market.ID)
	if store.PatchQuotes(market.ID, func(snapshot *Snapshot) bool {
		if snapshot.UpBid == "0.40" {
			return false
		}
		snapshot.UpBid = "0.40"
		return true
	}) {
		t.Fatal("expected unchanged patch to return false")
	}

	changed := store.PatchQuotes(market.ID, func(snapshot *Snapshot) bool {
		snapshot.UpBid = "0.55"
		snapshot.UpAsk = "0.56"
		snapshot.SourceUpdated = now
		snapshot.Stale = false
		return true
	})
	if !changed {
		t.Fatal("expected quote change")
	}
	after, ok := store.Get(market.ID)
	if !ok {
		t.Fatal("missing snapshot")
	}
	if after.UpBid != "0.55" || after.UpAsk != "0.56" {
		t.Fatalf("quotes=%q/%q", after.UpBid, after.UpAsk)
	}
	if len(after.Series) != len(before.Series) {
		t.Fatalf("series length changed: %d -> %d", len(before.Series), len(after.Series))
	}
	after.Series[0].ChainlinkPrice = "mutated"
	recheck, _ := store.Get(market.ID)
	if recheck.Series[0].ChainlinkPrice == "mutated" {
		t.Fatal("Get must defensive-copy Series")
	}
}

func TestPatchQuotesConcurrentWithPriceUpdate(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	market := Market{
		ID: "market-cq", Asset: "ETH", Period: "15m", Active: true,
		WindowStart: now.Add(-time.Minute),
		WindowEnd:   now.Add(10 * time.Minute),
	}
	store.ReplaceMarkets([]Market{market})
	store.Update(Snapshot{Market: market, Series: []PricePoint{{
		Timestamp: now, ChainlinkPrice: "3000",
	}}})

	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		for index := 0; index < 1000; index++ {
			store.PatchQuotes(market.ID, func(snapshot *Snapshot) bool {
				snapshot.UpBid = fmt.Sprintf("0.%03d", index%1000)
				snapshot.SourceUpdated = time.Now().UTC()
				return true
			})
		}
	}()
	go func() {
		defer group.Done()
		for index := 0; index < 1000; index++ {
			store.UpdatePrice(Snapshot{
				Market: market, OpenPrice: "3000",
				ChainlinkPrice: fmt.Sprintf("%d", 3000+index),
				Series: []PricePoint{{
					Timestamp: now, OpenPrice: "3000",
					ChainlinkPrice: fmt.Sprintf("%d", 3000+index),
				}},
				SourceUpdated: time.Now(),
			}, nil)
		}
	}()
	group.Wait()
	snapshot, ok := store.Get(market.ID)
	if !ok {
		t.Fatal("missing snapshot")
	}
	if snapshot.OpenPrice != "3000" {
		t.Fatalf("open=%q", snapshot.OpenPrice)
	}
}

func BenchmarkSnapshotStoreRead(b *testing.B) {
	store := NewSnapshotStore()
	market := Market{
		ID: "market-1", Asset: "BTC", Period: "5m", Active: true,
		WindowStart: time.Now().UTC().Add(-time.Minute),
		WindowEnd:   time.Now().UTC().Add(4 * time.Minute),
	}
	store.ReplaceMarkets([]Market{market})
	store.Update(Snapshot{
		Market: market,
		Series: []PricePoint{{Timestamp: time.Now(), ChainlinkPrice: "65000"}},
	})
	b.ReportAllocs()
	b.RunParallel(func(parallel *testing.PB) {
		for parallel.Next() {
			if _, ok := store.Get("market-1"); !ok {
				b.Fatal("snapshot missing")
			}
		}
	})
}

func BenchmarkSnapshotStoreUpdateQuotes(b *testing.B) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	markets := make([]Market, 0, 50)
	for index := 0; index < 50; index++ {
		markets = append(markets, Market{
			ID: fmt.Sprintf("m-%d", index), Asset: "BTC", Period: "5m", Active: true,
			WindowStart: now.Add(-time.Minute),
			WindowEnd:   now.Add(4 * time.Minute),
		})
	}
	store.ReplaceMarkets(markets)
	series := make([]PricePoint, 300)
	for index := range series {
		series[index] = PricePoint{
			Timestamp:      now.Add(time.Duration(index) * time.Second),
			ChainlinkPrice: "65000",
		}
	}
	for _, market := range markets {
		store.Update(Snapshot{Market: market, Series: series, UpBid: "0.4"})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		marketID := markets[index%len(markets)].ID
		store.PatchQuotes(marketID, func(snapshot *Snapshot) bool {
			snapshot.UpBid = fmt.Sprintf("0.%04d", index%10000)
			snapshot.SourceUpdated = now
			return true
		})
	}
}

func TestPatchQuoteBatchUpdatesManyMarketsOnce(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	markets := []Market{
		{ID: "a", Asset: "BTC", Period: "5m", Active: true,
			WindowStart: now.Add(-time.Minute), WindowEnd: now.Add(4 * time.Minute)},
		{ID: "b", Asset: "ETH", Period: "5m", Active: true,
			WindowStart: now.Add(-time.Minute), WindowEnd: now.Add(4 * time.Minute)},
	}
	store.ReplaceMarkets(markets)
	store.Update(Snapshot{
		Market: markets[0], Series: []PricePoint{{Timestamp: now, ChainlinkPrice: "1"}},
		UpBid: "0.1",
	})
	store.Update(Snapshot{
		Market: markets[1], Series: []PricePoint{{Timestamp: now, ChainlinkPrice: "2"}},
		DownAsk: "0.2",
	})
	eventsA, cancelA := store.Subscribe("a")
	defer cancelA()
	eventsB, cancelB := store.Subscribe("b")
	defer cancelB()
	changed := store.PatchQuoteBatch([]QuotePatch{
		{MarketID: "a", UpBid: "0.55", HasUp: true, SourceUpdated: now},
		{MarketID: "b", DownAsk: "0.66", HasDown: true, SourceUpdated: now},
		{MarketID: "a", UpBid: "0.55", HasUp: true, SourceUpdated: now}, // unchanged
	})
	if changed != 2 {
		t.Fatalf("changed=%d", changed)
	}
	snapA, _ := store.Get("a")
	snapB, _ := store.Get("b")
	if snapA.UpBid != "0.55" || snapB.DownAsk != "0.66" {
		t.Fatalf("a=%q b=%q", snapA.UpBid, snapB.DownAsk)
	}
	if len(snapA.Series) != 1 || len(snapB.Series) != 1 {
		t.Fatal("series should be preserved")
	}
	eventA := <-eventsA
	eventB := <-eventsB
	if eventA.Kind != SnapshotEventQuotes || eventA.Snapshot.Series != nil {
		t.Fatalf("eventA=%+v", eventA)
	}
	if eventB.Snapshot.DownAsk != "0.66" {
		t.Fatalf("eventB=%+v", eventB)
	}
}

func TestPatchChainlinkPriceSkipsUnchangedAndSamplesOncePerSecond(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	market := Market{
		ID: "btc", Asset: "BTC", Period: "5m", Active: true,
		WindowStart: now.Add(-time.Minute), WindowEnd: now.Add(4 * time.Minute),
	}
	store.ReplaceMarkets([]Market{market})
	store.Update(Snapshot{
		Market: market, ChainlinkPrice: "100", SourceUpdated: now,
		Series: []PricePoint{{Timestamp: now, ChainlinkPrice: "100"}},
	})
	changed, _, delta := store.PatchChainlinkPrice("btc", func(snapshot *Snapshot) (bool, []PricePoint) {
		if snapshot.ChainlinkPrice == "100" && snapshot.SourceUpdated.Equal(now) && !snapshot.Stale {
			return false, nil
		}
		return true, nil
	})
	if changed || delta != nil {
		t.Fatal("expected skip")
	}
	changed, _, delta = store.PatchChainlinkPrice("btc", func(snapshot *Snapshot) (bool, []PricePoint) {
		snapshot.ChainlinkPrice = "101"
		snapshot.SourceUpdated = now.Add(200 * time.Millisecond)
		snapshot.Stale = false
		return true, nil
	})
	if !changed || len(delta) != 0 {
		t.Fatalf("scalar update changed=%v delta=%d", changed, len(delta))
	}
	snap, _ := store.Get("btc")
	if snap.ChainlinkPrice != "101" || len(snap.Series) != 1 {
		t.Fatalf("snap=%+v", snap)
	}
	sampleAt := now.Add(time.Second)
	changed, _, delta = store.PatchChainlinkPrice("btc", func(snapshot *Snapshot) (bool, []PricePoint) {
		point := PricePoint{Timestamp: sampleAt, ChainlinkPrice: "102"}
		series := append([]PricePoint(nil), snapshot.Series...)
		series = append(series, point)
		snapshot.Series = series
		snapshot.ChainlinkPrice = "102"
		snapshot.SourceUpdated = sampleAt
		return true, []PricePoint{point}
	})
	if !changed || len(delta) != 1 {
		t.Fatalf("sample changed=%v delta=%d", changed, len(delta))
	}
	snap, _ = store.Get("btc")
	if len(snap.Series) != 2 || snap.Series[1].ChainlinkPrice != "102" {
		t.Fatalf("series=%+v", snap.Series)
	}
}

func BenchmarkSnapshotStorePatchQuoteBatch(b *testing.B) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	markets := make([]Market, 0, 50)
	for index := 0; index < 50; index++ {
		markets = append(markets, Market{
			ID: fmt.Sprintf("m-%d", index), Asset: "BTC", Period: "5m", Active: true,
			WindowStart: now.Add(-time.Minute), WindowEnd: now.Add(4 * time.Minute),
		})
	}
	store.ReplaceMarkets(markets)
	series := make([]PricePoint, 1200)
	for index := range series {
		series[index] = PricePoint{Timestamp: now, ChainlinkPrice: "1"}
	}
	for _, market := range markets {
		store.Update(Snapshot{Market: market, Series: series, UpBid: "0.4"})
	}
	patches := make([]QuotePatch, len(markets))
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		for i, market := range markets {
			patches[i] = QuotePatch{
				MarketID: market.ID, UpBid: fmt.Sprintf("0.%04d", index%10000),
				HasUp: true, SourceUpdated: now,
			}
		}
		store.PatchQuoteBatch(patches)
	}
}

func BenchmarkSnapshotStorePatchChainlinkPrice(b *testing.B) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	markets := make([]Market, 0, 50)
	for index := 0; index < 50; index++ {
		markets = append(markets, Market{
			ID: fmt.Sprintf("m-%d", index), Asset: "BTC", Period: "5m", Active: true,
			WindowStart: now.Add(-time.Minute), WindowEnd: now.Add(4 * time.Minute),
		})
	}
	store.ReplaceMarkets(markets)
	series := make([]PricePoint, 1200)
	for index := range series {
		series[index] = PricePoint{Timestamp: now, ChainlinkPrice: "1"}
	}
	for _, market := range markets {
		store.Update(Snapshot{Market: market, Series: series, ChainlinkPrice: "1"})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		marketID := markets[index%len(markets)].ID
		store.PatchChainlinkPrice(marketID, func(snapshot *Snapshot) (bool, []PricePoint) {
			snapshot.ChainlinkPrice = fmt.Sprintf("%d", index)
			snapshot.SourceUpdated = now
			return true, nil
		})
	}
}

func TestReplaceMarketsPrunesOrphanSnapshots(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	marketA := Market{
		ID: "market-a", Asset: "BTC", Period: "5m", Active: true,
		WindowStart: now.Add(-time.Minute), WindowEnd: now.Add(4 * time.Minute),
	}
	marketB := Market{
		ID: "market-b", Asset: "BTC", Period: "5m", Active: true,
		WindowStart: now.Add(-time.Minute), WindowEnd: now.Add(4 * time.Minute),
	}
	store.ReplaceMarkets([]Market{marketA, marketB})
	store.Update(Snapshot{
		Market: marketA, OpenPrice: "64000", ChainlinkPrice: "64001",
		Series: []PricePoint{{Timestamp: now, OpenPrice: "64000", ChainlinkPrice: "64001"}},
	})
	store.Update(Snapshot{
		Market: marketB, OpenPrice: "65000", ChainlinkPrice: "65001",
		Series: []PricePoint{{Timestamp: now, OpenPrice: "65000", ChainlinkPrice: "65001"}},
	})

	store.ReplaceMarkets([]Market{marketA})

	if _, ok := store.Get(marketB.ID); ok {
		t.Fatal("orphan snapshot was not pruned")
	}
	snapshot, ok := store.Get(marketA.ID)
	if !ok {
		t.Fatal("active snapshot missing")
	}
	if snapshot.OpenPrice != "64000" {
		t.Fatalf("open=%q want preserved", snapshot.OpenPrice)
	}
	if len(snapshot.Series) != 1 {
		t.Fatalf("series length=%d want preserved", len(snapshot.Series))
	}
}

func TestPruneExpiredRemovesOldSnapshots(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	cutoff := now.Add(-time.Hour)
	oldMarket := Market{
		ID: "old", Asset: "BTC", Period: "5m", Active: false,
		WindowStart: cutoff.Add(-2 * time.Hour), WindowEnd: cutoff.Add(-time.Minute),
	}
	liveMarket := Market{
		ID: "live", Asset: "BTC", Period: "5m", Active: true,
		WindowStart: now.Add(-time.Minute), WindowEnd: now.Add(4 * time.Minute),
	}
	store.ReplaceMarkets([]Market{oldMarket, liveMarket})
	store.Update(Snapshot{Market: oldMarket, ChainlinkPrice: "1"})
	store.Update(Snapshot{Market: liveMarket, ChainlinkPrice: "2"})

	removed := store.PruneExpired(cutoff)
	if len(removed) != 1 || removed[0] != oldMarket.ID {
		t.Fatalf("removed=%v want [%q]", removed, oldMarket.ID)
	}
	if _, ok := store.Get(oldMarket.ID); ok {
		t.Fatal("expired snapshot still present")
	}
	if _, ok := store.Get(liveMarket.ID); !ok {
		t.Fatal("live snapshot missing")
	}
}

func TestTrimSeriesCapsLength(t *testing.T) {
	points := make([]PricePoint, 5000)
	for index := range points {
		points[index] = PricePoint{
			Timestamp: time.Unix(int64(index), 0), ChainlinkPrice: fmt.Sprintf("%d", index),
		}
	}
	trimmed := trimSeries(points, MaxChartPoints*4)
	if len(trimmed) != MaxChartPoints*4 {
		t.Fatalf("len=%d want %d", len(trimmed), MaxChartPoints*4)
	}
	if trimmed[0].ChainlinkPrice != fmt.Sprintf("%d", 5000-MaxChartPoints*4) {
		t.Fatalf("first=%q want oldest retained point", trimmed[0].ChainlinkPrice)
	}
	if trimmed[len(trimmed)-1].ChainlinkPrice != "4999" {
		t.Fatalf("last=%q want newest point", trimmed[len(trimmed)-1].ChainlinkPrice)
	}
}
