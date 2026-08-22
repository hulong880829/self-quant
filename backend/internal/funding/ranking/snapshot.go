package ranking

import (
	"sync"
	"time"
)

type SnapshotStore struct {
	mu        sync.RWMutex
	snapshots map[Period]Snapshot
}

func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{snapshots: make(map[Period]Snapshot)}
}

func (s *SnapshotStore) Replace(period Period, items []Opportunity, now time.Time) {
	copied := cloneOpportunities(items)
	s.mu.Lock()
	s.snapshots[period] = Snapshot{
		Period: period, Items: copied,
		Version: now.UTC().Format("20060102T150405.000000000"), CalculatedAt: now.UTC(),
	}
	s.mu.Unlock()
}

func (s *SnapshotStore) MarkStale(period Period) {
	s.mu.Lock()
	snapshot, ok := s.snapshots[period]
	if ok {
		snapshot.Stale = true
		s.snapshots[period] = snapshot
	}
	s.mu.Unlock()
}

func (s *SnapshotStore) Get(period Period) Snapshot {
	s.mu.RLock()
	snapshot := s.snapshots[period]
	snapshot.Items = cloneOpportunities(snapshot.Items)
	s.mu.RUnlock()
	return snapshot
}

func cloneOpportunities(items []Opportunity) []Opportunity {
	result := make([]Opportunity, len(items))
	copy(result, items)
	return result
}
