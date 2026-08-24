package ranking

import (
	"sync"
	"testing"
	"time"
)

func TestSnapshotStatusChangesFreshnessWithoutReplacingItems(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	store.ReplaceWithDataThrough(
		Period1h, []Opportunity{{Rank: 1, GlobalSymbol: "BTCUSDT"}},
		now, now.Add(-time.Minute),
	)
	ready := store.View(Period1h)
	store.MarkStale(Period1h)
	stale := store.View(Period1h)
	if stale.Status != SnapshotStale || !stale.Stale || stale.Version == ready.Version {
		t.Fatalf("ready=%+v stale=%+v", ready, stale)
	}
	if stale.CalculatedAt != ready.CalculatedAt || stale.LastSuccessfulAt != ready.LastSuccessfulAt ||
		len(stale.Items) != 1 {
		t.Fatalf("stale transition replaced successful data: %+v", stale)
	}
}

func TestSnapshotConcurrentReadersObservePublishedGenerations(t *testing.T) {
	store := NewSnapshotStore()
	var group sync.WaitGroup
	for reader := 0; reader < 16; reader++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for range 1000 {
				snapshot := store.View(Period1h)
				if snapshot.Generation > 0 && len(snapshot.Items) != 1 {
					t.Errorf("partial snapshot: %+v", snapshot)
					return
				}
			}
		}()
	}
	for generation := 0; generation < 100; generation++ {
		store.Replace(
			Period1h, []Opportunity{{Rank: 1, GlobalSymbol: "BTCUSDT"}},
			time.Now().UTC(),
		)
	}
	group.Wait()
}

func BenchmarkSnapshotViewParallel(b *testing.B) {
	store := NewSnapshotStore()
	items := make([]Opportunity, 1000)
	for index := range items {
		items[index] = Opportunity{Rank: index + 1, GlobalSymbol: "BTCUSDT"}
	}
	store.Replace(Period1h, items, time.Now().UTC())
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			snapshot := store.View(Period1h)
			if len(snapshot.Items) != len(items) {
				b.Fatal("invalid snapshot")
			}
		}
	})
}
