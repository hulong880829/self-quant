package funding

import (
	"sync"
	"testing"
	"time"
)

func TestSnapshotStoreCopiesData(t *testing.T) {
	store := NewSnapshotStore()
	source := []Rate{{
		Exchange:       "binance",
		ExchangeSymbol: "BTCUSDT",
		BaseAsset:      "BTC",
		QuoteAsset:     "USDT",
		History:        []HistoryPoint{{Rate: 0.001}},
	}}
	store.Replace(source, 1, time.Unix(100, 0))
	source[0].ExchangeSymbol = "changed"
	source[0].History[0].Rate = 1

	snapshot := store.Get()
	if snapshot.Rates[0].ExchangeSymbol != "BTCUSDT" ||
		snapshot.Rates[0].History[0].Rate != 0.001 {
		t.Fatalf("store retained caller-owned data: %+v", snapshot)
	}
	snapshot.Rates[0].History[0].Rate = 2
	if store.Get().Rates[0].History[0].Rate != 0.001 {
		t.Fatal("read returned store-owned data")
	}
}

func TestSnapshotStoreConcurrentAccess(t *testing.T) {
	store := NewSnapshotStore()
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for iteration := 0; iteration < 100; iteration++ {
				store.Replace([]Rate{{ExchangeSymbol: "BTCUSDT"}}, 1, time.Now())
				_ = store.Get()
			}
		}()
	}
	group.Wait()
}
