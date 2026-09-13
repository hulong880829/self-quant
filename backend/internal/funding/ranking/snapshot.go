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

func (s *SnapshotStore) Restore(snapshot Snapshot) error {
	slot := s.slot(snapshot.Period)
	if slot == nil {
		return ErrInvalidPeriod
	}
	if err := validatePersistedSnapshot(snapshot, CurrentModelVersion); err != nil {
		return err
	}
	copied := snapshot
	copied.Items = cloneOpportunities(snapshot.Items)
	copied.RejectionSummary = cloneRejections(snapshot.RejectionSummary)
	copied.Status = SnapshotStale
	copied.Stale = true
	advanceGeneration(&s.generation, copied.Generation)
	freshness := s.freshness.Add(1)
	copied.FreshnessGeneration = freshness
	copied.Version = fmt.Sprintf("%d-%d", copied.Generation, freshness)
	for {
		current := slot.Load()
		if current != nil && current.Generation > copied.Generation {
			return nil
		}
		if slot.CompareAndSwap(current, &copied) {
			return nil
		}
	}
}

func (s *SnapshotStore) Replace(period Period, items []Opportunity, now time.Time) {
	s.ReplaceWithDataThrough(period, items, now, now)
}

func (s *SnapshotStore) ReplaceWithDataThrough(
	period Period, items []Opportunity, now, dataThrough time.Time,
) {
	s.ReplaceComputed(period, items, nil, now, dataThrough)
}

func (s *SnapshotStore) ReplaceComputed(
	period Period,
	items []Opportunity,
	rejections map[string]int,
	now, dataThrough time.Time,
) {
	slot := s.slot(period)
	if slot == nil {
		return
	}
	copied := cloneOpportunities(items)
	generation := s.generation.Add(1)
	freshness := s.freshness.Add(1)
	now = now.UTC()
	slot.Store(&Snapshot{
		Period: period, Items: copied, Generation: generation, FreshnessGeneration: freshness,
		Version: fmt.Sprintf("%d-%d", generation, freshness), Status: SnapshotReady,
		CalculatedAt: now, LastSuccessfulAt: now, DataThrough: dataThrough.UTC(),
		Stale: false, RejectionSummary: cloneRejections(rejections),
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
	if slot == nil {
		return
	}
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
	slot := s.slot(period)
	if slot == nil {
		return &Snapshot{Period: period, Status: SnapshotUnavailable, Stale: true}
	}
	if snapshot := slot.Load(); snapshot != nil {
		return snapshot
	}
	return &Snapshot{Period: period, Status: SnapshotUnavailable, Stale: true}
}

func (s *SnapshotStore) Get(period Period) Snapshot {
	return *s.View(period)
}

func (s *SnapshotStore) slot(period Period) *atomic.Pointer[Snapshot] {
	return s.slots[period]
}

func cloneOpportunities(items []Opportunity) []Opportunity {
	if len(items) > 100 {
		items = items[:100]
	}
	result := make([]Opportunity, len(items))
	copy(result, items)
	return result
}

func cloneRejections(values map[string]int) map[string]int {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]int, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func advanceGeneration(counter *atomic.Uint64, target uint64) {
	for {
		current := counter.Load()
		if current >= target || counter.CompareAndSwap(current, target) {
			return
		}
	}
}
