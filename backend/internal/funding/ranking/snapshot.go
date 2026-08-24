package ranking

import (
	"fmt"
	"sync/atomic"
	"time"
)

type SnapshotStore struct {
	slots      map[Period]*atomic.Pointer[Snapshot]
	generation atomic.Uint64
	freshness  atomic.Uint64
}

func NewSnapshotStore() *SnapshotStore {
	store := &SnapshotStore{slots: make(map[Period]*atomic.Pointer[Snapshot], len(Periods))}
	for _, period := range Periods {
		store.slots[period] = &atomic.Pointer[Snapshot]{}
	}
	return store
}

func (s *SnapshotStore) Replace(period Period, items []Opportunity, now time.Time) {
	s.ReplaceWithDataThrough(period, items, now, now)
}

func (s *SnapshotStore) ReplaceWithDataThrough(
	period Period, items []Opportunity, now, dataThrough time.Time,
) {
	copied := cloneOpportunities(items)
	generation := s.generation.Add(1)
	freshness := s.freshness.Add(1)
	now = now.UTC()
	s.slot(period).Store(&Snapshot{
		Period: period, Items: copied, Generation: generation, FreshnessGeneration: freshness,
		Version: fmt.Sprintf("%d-%d", generation, freshness), Status: SnapshotReady,
		CalculatedAt: now, LastSuccessfulAt: now, DataThrough: dataThrough.UTC(),
		Stale: false,
	})
}

func (s *SnapshotStore) MarkStale(period Period) {
	s.markStatus(period, SnapshotStale)
}

func (s *SnapshotStore) MarkWarming(period Period) {
	s.markStatus(period, SnapshotWarming)
}

func (s *SnapshotStore) MarkUnavailable(period Period) {
	s.markStatus(period, SnapshotUnavailable)
}

func (s *SnapshotStore) markStatus(period Period, status SnapshotStatus) {
	slot := s.slot(period)
	for {
		current := slot.Load()
		freshness := s.freshness.Add(1)
		next := &Snapshot{
			Period: period, Status: status, Stale: status != SnapshotReady,
			FreshnessGeneration: freshness,
		}
		if current != nil {
			*next = *current
			next.Status = status
			next.Stale = status != SnapshotReady
			next.FreshnessGeneration = freshness
		}
		next.Version = fmt.Sprintf("%d-%d", next.Generation, freshness)
		if slot.CompareAndSwap(current, next) {
			return
		}
	}
}

func (s *SnapshotStore) View(period Period) *Snapshot {
	if snapshot := s.slot(period).Load(); snapshot != nil {
		return snapshot
	}
	return &Snapshot{Period: period, Status: SnapshotUnavailable, Stale: true}
}

func (s *SnapshotStore) Get(period Period) Snapshot {
	return *s.View(period)
}

func (s *SnapshotStore) slot(period Period) *atomic.Pointer[Snapshot] {
	if slot := s.slots[period]; slot != nil {
		return slot
	}
	return s.slots[Period1h]
}

func cloneOpportunities(items []Opportunity) []Opportunity {
	result := make([]Opportunity, len(items))
	copy(result, items)
	return result
}
