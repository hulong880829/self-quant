package funding

import (
	"sync/atomic"
	"time"
)

type Snapshot struct {
	Rates     []Rate
	Total     int
	Version   string
	UpdatedAt time.Time
	exact     map[string]int
	fallback  map[string]int
}

type SnapshotStore struct {
	snapshot atomic.Pointer[Snapshot]
}

func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{}
}

func (s *SnapshotStore) Replace(rates []Rate, total int, now time.Time) {
	copied := cloneRates(rates)
	s.snapshot.Store(indexedSnapshot(copied, total, now.UTC()))
}

func indexedSnapshot(rates []Rate, total int, now time.Time) *Snapshot {
	exact := make(map[string]int, len(rates))
	fallback := make(map[string]int, len(rates))
	for index, rate := range rates {
		exactKey := ExactRateKey(rate.Exchange, rate.ExchangeSymbol)
		if _, exists := exact[exactKey]; !exists {
			exact[exactKey] = index
		}
		fallbackKey := FallbackRateKey(rate.Exchange, rate.BaseAsset, rate.QuoteAsset)
		if _, exists := fallback[fallbackKey]; !exists {
			fallback[fallbackKey] = index
		}
	}
	return &Snapshot{
		Rates: rates, Total: total,
		Version:   now.Format("20060102T150405.000000000"),
		UpdatedAt: now,
		exact:     exact,
		fallback:  fallback,
	}
}

func (s *SnapshotStore) Get() Snapshot {
	snapshot := *s.View()
	snapshot.Rates = cloneRates(snapshot.Rates)
	return snapshot
}

func (s *SnapshotStore) View() *Snapshot {
	if snapshot := s.snapshot.Load(); snapshot != nil {
		return snapshot
	}
	return &Snapshot{}
}

func cloneRates(rates []Rate) []Rate {
	result := make([]Rate, len(rates))
	copy(result, rates)
	for index := range result {
		result[index].History = append([]HistoryPoint(nil), rates[index].History...)
	}
	return result
}
