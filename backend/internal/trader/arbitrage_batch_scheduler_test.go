package trader

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type countingControlBatchStore struct {
	dryRunStore
	mu      sync.Mutex
	batches []int
	found   map[string]arbitrageControlSummary
	missing map[string]struct{}
	err     error
}

func (s *countingControlBatchStore) ListArbitrageControlSummaries(
	_ context.Context, ids []string,
) (arbitrageControlSummaryBatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, len(ids))
	if s.err != nil {
		return arbitrageControlSummaryBatch{}, s.err
	}
	result := arbitrageControlSummaryBatch{
		Found: make(map[string]arbitrageControlSummary),
	}
	for _, id := range ids {
		if _, gone := s.missing[id]; gone {
			result.Missing = append(result.Missing, id)
			continue
		}
		if summary, ok := s.found[id]; ok {
			result.Found[id] = summary
			continue
		}
		result.Found[id] = arbitrageControlSummary{
			Version: 1, Status: "running", RuntimeState: "monitoring",
		}
	}
	return result, nil
}

func TestControlSummaryBatcherChunksThousandRuntimes(t *testing.T) {
	store := &countingControlBatchStore{
		found: make(map[string]arbitrageControlSummary),
	}
	scheduler := NewArbitrageScheduler(
		store, nil, nil, time.Second, time.Millisecond, time.Second,
		10, 1, 4, 16, true, nil,
	)
	for i := 0; i < 1000; i++ {
		id := uuid.NewString()
		scheduler.runtimes[id] = &arbitrageRuntime{
			combination: ArbitrageCombination{ID: id, Status: "running"},
			generation:  1,
		}
	}
	scheduler.controlBatcher.flush(context.Background())
	if len(store.batches) != 4 {
		t.Fatalf("batches=%v", store.batches)
	}
	total := 0
	for _, size := range store.batches {
		if size > arbitrageControlSummaryBatchSize {
			t.Fatalf("batch size %d exceeds %d", size, arbitrageControlSummaryBatchSize)
		}
		total += size
	}
	if total != 1000 {
		t.Fatalf("total=%d batches=%v", total, store.batches)
	}
}

func TestControlSummaryFlushQueryFailedDoesNotMarkMissing(t *testing.T) {
	id := uuid.NewString()
	store := &countingControlBatchStore{err: errors.New("query failed")}
	scheduler := NewArbitrageScheduler(
		store, nil, nil, time.Second, time.Millisecond, time.Second,
		10, 1, 4, 16, true, nil,
	)
	runtime := &arbitrageRuntime{
		combination: ArbitrageCombination{ID: id, Status: "running"},
		generation:  1,
	}
	scheduler.runtimes[id] = runtime
	scheduler.controlBatcher.flush(context.Background())
	probe, ok := runtime.controlProbe.Load().(controlProbeSnapshot)
	if !ok || !probe.ready || !probe.queryFailed || probe.missing {
		t.Fatalf("probe=%+v ok=%v", probe, ok)
	}
}

func TestHandleControlStopsWhenProbeMissing(t *testing.T) {
	scheduler := &ArbitrageScheduler{store: &dryRunStore{item: ArbitrageCombination{
		ID: "combo-1", Status: "running",
	}}}
	runtime := &arbitrageRuntime{combination: ArbitrageCombination{
		ID: "combo-1", Status: "running", RuntimeState: "monitoring", Version: 1,
	}}
	runtime.controlProbe.Store(controlProbeSnapshot{ready: true, missing: true})
	stop, refreshed := scheduler.handleControl(context.Background(), runtime)
	if !stop || refreshed {
		t.Fatalf("stop=%v refreshed=%v", stop, refreshed)
	}
}

func TestHandleControlContinuesWhenProbeQueryFailed(t *testing.T) {
	item := ArbitrageCombination{
		ID: "combo-1", Status: "running", RuntimeState: "monitoring", Version: 1,
	}
	store := &dryRunStore{item: item}
	scheduler := &ArbitrageScheduler{store: store}
	runtime := &arbitrageRuntime{combination: item}
	runtime.controlProbe.Store(controlProbeSnapshot{ready: true, queryFailed: true})
	stop, refreshed := scheduler.refreshCombinationPeriodic(context.Background(), runtime)
	if stop || refreshed {
		t.Fatalf("stop=%v refreshed=%v", stop, refreshed)
	}
	if store.controlProbes != 0 || store.refreshes != 0 {
		t.Fatalf("probes=%d refreshes=%d", store.controlProbes, store.refreshes)
	}
}

func TestControlSummaryDeliveryIgnoresFinishedRuntime(t *testing.T) {
	scheduler := NewArbitrageScheduler(
		&countingControlBatchStore{err: errors.New("query failed")},
		nil, nil, time.Second, time.Millisecond, time.Second,
		10, 1, 4, 16, true, nil,
	)
	runtime := &arbitrageRuntime{
		combination: ArbitrageCombination{ID: uuid.NewString()},
		generation:  1,
	}
	scheduler.runtimes[runtime.combination.ID] = runtime
	scheduler.finishRuntime(runtime)
	scheduler.storeControlProbe(runtime.combination.ID, runtime, 1, controlProbeSnapshot{
		ready: true, missing: true,
	})
	if _, ok := runtime.controlProbe.Load().(controlProbeSnapshot); ok {
		t.Fatal("probe stored on finished runtime")
	}
}

func TestHandleControlReloadsWhenProbeFieldsChange(t *testing.T) {
	item := ArbitrageCombination{
		ID: "combo-1", OwnerUsername: "admin", Status: "running",
		RuntimeState: "position_uncertain", Version: 8,
	}
	store := &dryRunStore{item: item}
	scheduler := &ArbitrageScheduler{store: store}
	runtime := &arbitrageRuntime{combination: ArbitrageCombination{
		ID: "combo-1", OwnerUsername: "admin", Status: "running",
		RuntimeState: "monitoring", Version: 7,
	}}
	runtime.controlProbe.Store(controlProbeSnapshot{
		ready: true,
		summary: arbitrageControlSummary{
			Version: 8, Status: "running", RuntimeState: "position_uncertain",
		},
	})
	stop, refreshed := scheduler.refreshCombinationPeriodic(context.Background(), runtime)
	if stop || !refreshed || runtime.combination.RuntimeState != "position_uncertain" {
		t.Fatalf("stop=%v refreshed=%v item=%+v", stop, refreshed, runtime.combination)
	}
	if store.controlProbes != 0 {
		t.Fatalf("periodic probe should not call single-row summary: %d", store.controlProbes)
	}
}

func TestLeaseBatcherPartialLossStopsOnlyLostRuntime(t *testing.T) {
	keep := uuid.NewString()
	lost := uuid.NewString()
	store := &leaseBatchFakeStore{renewed: map[string]bool{keep: true}}
	scheduler := NewArbitrageScheduler(
		store, nil, nil, time.Second, time.Millisecond, 3*time.Second,
		10, 1, 4, 16, true, nil,
	)
	scheduler.batchersStarted.Store(true)
	keepRuntime := &arbitrageRuntime{
		combination: ArbitrageCombination{ID: keep, Status: "running"},
		generation:  2,
		nextRenew:   time.Now().UTC().Add(-time.Second),
	}
	lostRuntime := &arbitrageRuntime{
		combination: ArbitrageCombination{ID: lost, Status: "running"},
		generation:  3,
		nextRenew:   time.Now().UTC().Add(-time.Second),
	}
	scheduler.ensureRuntime(keepRuntime)
	scheduler.ensureRuntime(lostRuntime)
	scheduler.runtimes[keep] = keepRuntime
	scheduler.runtimes[lost] = lostRuntime
	scheduler.leaseBatcher.Submit(keepRuntime)
	scheduler.leaseBatcher.Submit(lostRuntime)
	scheduler.leaseBatcher.flush(context.Background())
	if scheduler.renewLease(context.Background(), keepRuntime) {
		t.Fatal("kept lease should not stop")
	}
	if !scheduler.renewLease(context.Background(), lostRuntime) {
		t.Fatal("lost lease should stop")
	}
}

func TestLeaseBatcherIgnoresStaleGeneration(t *testing.T) {
	id := uuid.NewString()
	store := &leaseBatchFakeStore{renewed: map[string]bool{id: true}}
	scheduler := NewArbitrageScheduler(
		store, nil, nil, time.Second, time.Millisecond, 3*time.Second,
		10, 1, 4, 16, true, nil,
	)
	oldRuntime := &arbitrageRuntime{
		combination: ArbitrageCombination{ID: id}, generation: 1,
		nextRenew: time.Now().UTC().Add(-time.Second),
	}
	newRuntime := &arbitrageRuntime{
		combination: ArbitrageCombination{ID: id}, generation: 2,
		nextRenew: time.Now().UTC().Add(-time.Second),
	}
	scheduler.ensureRuntime(oldRuntime)
	scheduler.ensureRuntime(newRuntime)
	scheduler.runtimes[id] = newRuntime
	scheduler.leaseBatcher.Submit(oldRuntime)
	scheduler.leaseBatcher.flush(context.Background())
	if takeLeaseNotice(newRuntime) != nil {
		t.Fatal("old generation result delivered to new runtime")
	}
}

type leaseBatchFakeStore struct {
	dryRunStore
	renewed map[string]bool
	err     error
}

func (s *leaseBatchFakeStore) RenewArbitrageLeases(
	context.Context, []string, time.Duration,
) (map[string]bool, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.renewed, nil
}

func TestSnapshotReceiptIgnoresOlderSequence(t *testing.T) {
	scheduler := &ArbitrageScheduler{}
	runtime := &arbitrageRuntime{
		combination:            ArbitrageCombination{ID: "combo-1", Version: 1, Status: "running", RuntimeState: "monitoring"},
		generation:             1,
		lastAppliedSnapshotSeq: 11,
		lastSnapshotUpdatedAt:  time.Unix(11, 0).UTC(),
		snapshotInFlightSeq:    10,
	}
	runtime.snapshotNotice.Store(&snapshotFlushNotice{
		generation: 1, seq: 10, applied: true,
		item: arbitrageMarketSnapshotBatchItem{UpdatedAt: time.Unix(10, 0).UTC(), AskSpread: "1"},
	})
	if scheduler.applySnapshotReceipt(context.Background(), runtime) {
		t.Fatal("stale receipt should not stop")
	}
	if runtime.lastSnapshotUpdatedAt.Unix() != 11 || runtime.lastAppliedSnapshotSeq != 11 {
		t.Fatalf("cursor rewound: %+v", runtime)
	}
	if runtime.snapshotInFlightSeq != 0 {
		t.Fatal("matching in-flight seq should clear")
	}
}

func TestSnapshotSkippedVersionReloadsCombination(t *testing.T) {
	item := ArbitrageCombination{
		ID: "combo-1", OwnerUsername: "admin", Status: "running",
		RuntimeState: "backoff", Version: 9, UpdatedAt: time.Unix(20, 0).UTC(),
	}
	store := &dryRunStore{item: item}
	scheduler := &ArbitrageScheduler{store: store}
	runtime := &arbitrageRuntime{
		combination: ArbitrageCombination{
			ID: "combo-1", OwnerUsername: "admin", Status: "running",
			RuntimeState: "monitoring", Version: 7,
		},
		generation: 1,
	}
	runtime.snapshotNotice.Store(&snapshotFlushNotice{
		generation: 1, seq: 3, skipped: true,
		item: arbitrageMarketSnapshotBatchItem{
			Version: 9, Status: "running", RuntimeState: "backoff",
			UpdatedAt: time.Unix(20, 0).UTC(),
		},
	})
	if scheduler.applySnapshotReceipt(context.Background(), runtime) {
		t.Fatal("reload should not stop running combination")
	}
	if runtime.combination.RuntimeState != "backoff" || runtime.combination.Version != 9 {
		t.Fatalf("did not full reload: %+v", runtime.combination)
	}
	if store.refreshes != 1 {
		t.Fatalf("refreshes=%d", store.refreshes)
	}
}

func TestSnapshotSkippedUpdatedAtOnlySyncsCursor(t *testing.T) {
	scheduler := &ArbitrageScheduler{}
	updated := time.Unix(30, 0).UTC()
	runtime := &arbitrageRuntime{
		combination: ArbitrageCombination{
			ID: "combo-1", Status: "running", RuntimeState: "monitoring", Version: 4,
		},
		generation: 1,
	}
	runtime.snapshotNotice.Store(&snapshotFlushNotice{
		generation: 1, seq: 4, skipped: true,
		item: arbitrageMarketSnapshotBatchItem{
			Version: 4, Status: "running", RuntimeState: "monitoring",
			UpdatedAt: updated,
		},
	})
	if scheduler.applySnapshotReceipt(context.Background(), runtime) {
		t.Fatal("updated_at skip should not stop")
	}
	if !runtime.lastSnapshotUpdatedAt.Equal(updated) {
		t.Fatalf("cursor=%s", runtime.lastSnapshotUpdatedAt)
	}
	if runtime.combination.Version != 4 {
		t.Fatalf("patched version: %+v", runtime.combination)
	}
}

func TestSnapshotSQLFailureThrottlesRetry(t *testing.T) {
	store := &snapshotBatchFakeStore{}
	scheduler := NewArbitrageScheduler(
		store, nil, nil, time.Second, time.Millisecond, time.Second,
		10, 1, 4, 16, true, nil,
	)
	scheduler.batchersStarted.Store(true)
	runtime := &arbitrageRuntime{
		combination: ArbitrageCombination{
			ID: "combo-1", Status: "running", RuntimeState: "monitoring", Version: 1,
			UpdatedAt: time.Unix(1, 0).UTC(),
		},
		generation:  1,
		lastPersist: time.Now().UTC().Add(-marketSnapshotPersistInterval),
	}
	store.item = runtime.combination
	scheduler.runtimes[runtime.combination.ID] = runtime
	scheduler.ensureRuntime(runtime)
	if scheduler.persistMarketSnapshot(context.Background(), runtime, "1", "-1", false, false) {
		t.Fatal("submit should not stop")
	}
	store.err = fmt.Errorf("flush failed")
	scheduler.snapshotBatcher.flush(context.Background())
	if scheduler.applySnapshotReceipt(context.Background(), runtime) {
		t.Fatal("failed flush should not stop")
	}
	if runtime.nextSnapshotAttemptAt.IsZero() {
		t.Fatal("expected retry throttle")
	}
	if scheduler.persistMarketSnapshot(context.Background(), runtime, "2", "-2", false, false) {
		t.Fatal("throttled persist should not stop")
	}
	if runtime.snapshotInFlightSeq != 0 {
		t.Fatal("throttled submit should not start another in-flight")
	}
	beforeUrgent := runtime.lastPersist
	if scheduler.persistMarketSnapshot(context.Background(), runtime, "3", "-2", true, true) {
		t.Fatal("urgent persist should not stop")
	}
	if !runtime.lastPersist.After(beforeUrgent) {
		t.Fatal("urgent persist must ignore nextSnapshotAttemptAt")
	}
	if !runtime.nextSnapshotAttemptAt.IsZero() {
		t.Fatal("urgent success should clear retry throttle")
	}
}

type snapshotBatchFakeStore struct {
	dryRunStore
	err error
}

func (s *snapshotBatchFakeStore) BatchUpdateArbitrageMarketSnapshots(
	_ context.Context, writes []arbitrageMarketSnapshotWrite,
) ([]arbitrageMarketSnapshotBatchItem, error) {
	if s.err != nil {
		return nil, s.err
	}
	items := make([]arbitrageMarketSnapshotBatchItem, 0, len(writes))
	for _, write := range writes {
		items = append(items, arbitrageMarketSnapshotBatchItem{
			CombinationID: write.CombinationID, Applied: true,
			Version: write.ExpectedVersion, Status: "running", RuntimeState: "monitoring",
			UpdatedAt: time.Now().UTC(), AskSpread: write.AskSpread, BidSpread: write.BidSpread,
			Sequence: write.Sequence,
		})
	}
	return items, nil
}
