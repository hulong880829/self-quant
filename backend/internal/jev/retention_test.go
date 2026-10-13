package jev

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

type recordingRetentionStore struct {
	calls      []string
	stopped    int64
	positions  int64
	decisions  int64
	equity     int64
	snapshots  int64
	failOn     string
	alwaysFull bool
}

func (s *recordingRetentionStore) DeleteExpiredStoppedRuns(context.Context, time.Time, int) (int64, error) {
	return s.hit("stopped", s.stopped)
}
func (s *recordingRetentionStore) ArchiveAndDeleteClosedPositions(context.Context, time.Time, int) (int64, error) {
	return s.hit("positions", s.positions)
}
func (s *recordingRetentionStore) DeleteExpiredTerminalDecisions(context.Context, time.Time, int) (int64, error) {
	return s.hit("decisions", s.decisions)
}
func (s *recordingRetentionStore) DeleteExpiredEquityPoints(context.Context, time.Time, int) (int64, error) {
	return s.hit("equity", s.equity)
}
func (s *recordingRetentionStore) DeleteExpiredSnapshotsKeepingLatest(context.Context, time.Time, int) (int64, error) {
	return s.hit("snapshots", s.snapshots)
}

func (s *recordingRetentionStore) hit(name string, n int64) (int64, error) {
	s.calls = append(s.calls, name)
	if s.failOn == name {
		return 0, errors.New(name + " failed")
	}
	if s.alwaysFull {
		return int64(cleanupBatchSize), nil
	}
	return n, nil
}

type busyCycleLock struct{}

func (busyCycleLock) AcquireCycleLock(context.Context) (CycleLease, bool, error) {
	return nil, false, nil
}

type heldFakeLock struct {
	released bool
}

func (h *heldFakeLock) AcquireCycleLock(context.Context) (CycleLease, bool, error) {
	return &fakeLease{lock: h}, true, nil
}

type fakeLease struct{ lock *heldFakeLock }

func (l *fakeLease) Release() error {
	l.lock.released = true
	return nil
}

func TestRetentionSkipsPositionsWhenLockBusyButContinues(t *testing.T) {
	store := &recordingRetentionStore{decisions: 1, equity: 1, snapshots: 1, positions: 99}
	worker := NewRetentionWorker(store, busyCycleLock{}, slog.Default())
	worker.now = func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) }
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if containsCall(store.calls, "positions") {
		t.Fatalf("lock busy must skip positions, calls=%v", store.calls)
	}
	if !containsCall(store.calls, "decisions") || !containsCall(store.calls, "equity") || !containsCall(store.calls, "snapshots") {
		t.Fatalf("other cleanup must continue, calls=%v", store.calls)
	}
}

func TestRetentionBatchCap(t *testing.T) {
	store := &recordingRetentionStore{alwaysFull: true}
	lock := &heldFakeLock{}
	worker := NewRetentionWorker(store, lock, slog.Default())
	worker.now = func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) }
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, call := range store.calls {
		counts[call]++
	}
	for _, name := range []string{"stopped", "positions", "decisions", "equity", "snapshots"} {
		if counts[name] != cleanupMaxBatches {
			t.Fatalf("%s batches=%d want=%d calls=%v", name, counts[name], cleanupMaxBatches, store.calls)
		}
	}
	if !lock.released {
		t.Fatal("cycle lock must be released")
	}
}

func TestRetentionFailureDoesNotDependOnEngine(t *testing.T) {
	store := &recordingRetentionStore{failOn: "equity"}
	worker := NewRetentionWorker(store, &heldFakeLock{}, slog.Default())
	worker.now = func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) }
	if err := worker.RunOnce(context.Background()); err == nil {
		t.Fatal("expected equity failure")
	}
}

func TestRetentionLoopContinuesAfterError(t *testing.T) {
	store := &recordingRetentionStore{failOn: "stopped"}
	worker := NewRetentionWorker(store, busyCycleLock{}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		worker.RunLoop(ctx)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retention loop did not stop")
	}
}

func containsCall(calls []string, name string) bool {
	for _, call := range calls {
		if call == name {
			return true
		}
	}
	return false
}
