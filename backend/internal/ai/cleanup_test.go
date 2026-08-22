package ai

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fakeExpiredStore struct {
	counts []int64
	calls  int
}

func (f *fakeExpiredStore) DeleteExpired(context.Context, int) (int64, error) {
	result := f.counts[f.calls]
	f.calls++
	return result, nil
}

func TestCleanupWorkerUsesBoundedBatches(t *testing.T) {
	store := &fakeExpiredStore{counts: []int64{500, 500, 500}}
	worker := NewCleanupWorker(
		store, time.Hour, 500, 2,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	worker.cleanup(context.Background())
	if store.calls != 2 {
		t.Fatalf("calls=%d", store.calls)
	}
}

func TestCleanupWorkerStopsAfterPartialBatch(t *testing.T) {
	store := &fakeExpiredStore{counts: []int64{12}}
	worker := NewCleanupWorker(
		store, time.Hour, 500, 10,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	worker.cleanup(context.Background())
	if store.calls != 1 {
		t.Fatalf("calls=%d", store.calls)
	}
}
