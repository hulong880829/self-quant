package funding

import (
	"sync"
	"time"
)

type Snapshot struct {
	Rates     []Rate
	Total     int
	Version   string
	UpdatedAt time.Time
}

type SnapshotStore struct {
	mu       sync.RWMutex
	snapshot Snapshot
}

func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{}
}

func (s *SnapshotStore) Replace(rates []Rate, total int, now time.Time) {
	copied := cloneRates(rates)
	s.mu.Lock()
	s.snapshot = Snapshot{
		Rates: copied, Total: total,
		Version:   now.UTC().Format("20060102T150405.000000000"),
		UpdatedAt: now.UTC(),
	}
	s.mu.Unlock()
}

func (s *SnapshotStore) Get() Snapshot {
	s.mu.RLock()
	snapshot := s.snapshot
	snapshot.Rates = cloneRates(snapshot.Rates)
	s.mu.RUnlock()
	return snapshot
}

func cloneRates(rates []Rate) []Rate {
	result := make([]Rate, len(rates))
	copy(result, rates)
	for index := range result {
		result[index].History = append([]HistoryPoint(nil), rates[index].History...)
	}
	return result
}
