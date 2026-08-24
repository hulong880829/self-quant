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
}

type SnapshotStore struct {
	snapshot atomic.Pointer[Snapshot]
}

func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{}
}

func (s *SnapshotStore) Replace(rates []Rate, total int, now time.Time) {
	copied := cloneRates(rates)
	s.snapshot.Store(&Snapshot{
		Rates: copied, Total: total,
		Version:   now.UTC().Format("20060102T150405.000000000"),
		UpdatedAt: now.UTC(),
	})
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
