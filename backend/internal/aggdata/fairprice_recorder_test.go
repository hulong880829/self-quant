package aggdata

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestEnqueueLatestReplacesPendingBatch(t *testing.T) {
	queue := make(chan fairPriceBatch, 1)
	first := fairPriceBatch{observedAt: time.Unix(1, 0)}
	second := fairPriceBatch{observedAt: time.Unix(2, 0)}
	if enqueueLatest(queue, first) {
		t.Fatal("empty queue reported a drop")
	}
	if !enqueueLatest(queue, second) {
		t.Fatal("full queue did not report a drop")
	}
	if got := <-queue; !got.observedAt.Equal(second.observedAt) {
		t.Fatalf("queue retained stale batch: %v", got.observedAt)
	}
}

func TestFairPriceRecorderStopsWhileDatabaseUnavailable(t *testing.T) {
	recorder, err := NewFairPriceRecorder(
		NewStore(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		FairPriceRecorderConfig{
			DatabaseURL:      "postgres://127.0.0.1:1/unavailable?sslmode=disable",
			SampleInterval:   10 * time.Millisecond,
			Retention:        time.Hour,
			CleanupInterval:  time.Minute,
			DeleteBatch:      10,
			DeleteMaxBatches: 2,
			OperationTimeout: 20 * time.Millisecond,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		recorder.Run(ctx)
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recorder did not stop after context cancellation")
	}
}
