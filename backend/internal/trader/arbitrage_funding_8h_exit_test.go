package trader

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

type stubFunding8hStore struct {
	block       chan struct{}
	listCalls   atomic.Int64
	markCalls   atomic.Int64
	rows        []funding8hExitRow
	listErr     error
	applied     bool
	appliedOnce bool
	markErr     error
	lastReason  string
	lastDetails map[string]any
}

func (s *stubFunding8hStore) ListOneShotFunding8hExitRows(
	ctx context.Context, _, _ time.Time,
) ([]funding8hExitRow, error) {
	s.listCalls.Add(1)
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.rows, nil
}

func (s *stubFunding8hStore) MarkOneShotExiting(
	_ context.Context, id string, version int64, reason string, details map[string]any,
) (ArbitrageCombination, bool, error) {
	s.markCalls.Add(1)
	s.lastReason = reason
	s.lastDetails = details
	if s.markErr != nil {
		return ArbitrageCombination{}, false, s.markErr
	}
	if !s.applied || s.appliedOnce {
		return ArbitrageCombination{ID: id, Version: version}, false, nil
	}
	s.appliedOnce = true
	return ArbitrageCombination{
		ID: id, Version: version + 1, Status: "running",
		RunMode: "one_shot", OneShotPhase: "exiting",
	}, true, nil
}

func TestFunding8hExitWorkerSkipsOverlappingTicks(t *testing.T) {
	store := &stubFunding8hStore{block: make(chan struct{})}
	worker := newTestFunding8hWorker(store)
	ctx := context.Background()
	worker.tick(ctx)
	worker.tick(ctx)
	waitAtomic(t, &store.listCalls, 1)
	close(store.block)
	waitInFlightClear(t, worker)
	if store.listCalls.Load() != 1 {
		t.Fatalf("listCalls=%d", store.listCalls.Load())
	}
}

func TestFunding8hExitWorkerQueryTimeoutDoesNotMark(t *testing.T) {
	store := &stubFunding8hStore{block: make(chan struct{})}
	worker := newTestFunding8hWorker(store)
	worker.queryTimeout = 20 * time.Millisecond
	worker.runOnce(context.Background())
	if store.listCalls.Load() != 1 || store.markCalls.Load() != 0 {
		t.Fatalf("list=%d mark=%d", store.listCalls.Load(), store.markCalls.Load())
	}
}

func TestFunding8hExitWorkerListErrorDoesNotMark(t *testing.T) {
	store := &stubFunding8hStore{listErr: errors.New("query failed")}
	worker := newTestFunding8hWorker(store)
	worker.runOnce(context.Background())
	if store.markCalls.Load() != 0 {
		t.Fatalf("markCalls=%d", store.markCalls.Load())
	}
}

func TestFunding8hExitWorkerDoesNotRetryLostCAS(t *testing.T) {
	end := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	store := &stubFunding8hStore{rows: triggeringFunding8hRows("combo-1", end), applied: false}
	worker := newTestFunding8hWorker(store)
	worker.now = func() time.Time { return end }
	worker.runOnce(context.Background())
	if store.markCalls.Load() != 1 || store.lastReason != "funding_8h_floor" {
		t.Fatalf("markCalls=%d reason=%s", store.markCalls.Load(), store.lastReason)
	}
	if store.lastDetails["configuredFloor"] != "0.05" {
		t.Fatalf("details=%v", store.lastDetails)
	}
	if store.lastDetails["targetReachedAt"] != end.Add(-8*time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("targetReachedAt=%v", store.lastDetails["targetReachedAt"])
	}
}

func TestFunding8hExitWorkerSkipsIncompleteThenMarksAfterCompleteTrigger(t *testing.T) {
	store := &stubFunding8hStore{
		rows:    funding8hIncidentRows(false),
		applied: true,
	}
	worker := newTestFunding8hWorker(store)
	worker.now = func() time.Time { return funding8hIncidentEnd }
	worker.runOnce(context.Background())
	if store.listCalls.Load() != 1 || store.markCalls.Load() != 0 {
		t.Fatalf("incomplete round list=%d mark=%d", store.listCalls.Load(), store.markCalls.Load())
	}

	triggerEnd := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	store.rows = triggeringFunding8hRows("combo-1", triggerEnd)
	worker.now = func() time.Time { return triggerEnd }
	worker.runOnce(context.Background())
	if store.listCalls.Load() != 2 || store.markCalls.Load() != 1 ||
		store.lastReason != "funding_8h_floor" {
		t.Fatalf("complete round list=%d mark=%d reason=%s",
			store.listCalls.Load(), store.markCalls.Load(), store.lastReason)
	}
}

func funding8hIncidentRows(includeA0600 bool) []funding8hExitRow {
	combo := funding8hIncidentCombo(includeA0600)
	rows := make([]funding8hExitRow, 0, len(combo.LegA)+len(combo.LegB))
	for _, point := range combo.LegA {
		rows = append(rows, funding8hScanRow(
			combo.ID, "a", point.At, point.Rate.String(), point.IntervalHours.String(), combo.Floor,
			*combo.TargetReachedAt,
		))
	}
	for _, point := range combo.LegB {
		rows = append(rows, funding8hScanRow(
			combo.ID, "b", point.At, point.Rate.String(), point.IntervalHours.String(), combo.Floor,
			*combo.TargetReachedAt,
		))
	}
	return rows
}

func TestFunding8hExitWorkerMarksAfterDelayedPostTargetIngest(t *testing.T) {
	end := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	start := end.Add(-8 * time.Hour)
	target := start.Add(6 * time.Hour)
	incomplete := append(
		funding8hHourlyRows("combo-1", "a", start, end, "0.0001", "1", "0.05", target),
		funding8hScanRow("combo-1", "b", start, "0", "8", "0.05", target),
		funding8hScanRow("combo-1", "b", start.Add(4*time.Hour), "0", "8", "0.05", target),
	)
	store := &stubFunding8hStore{rows: incomplete, applied: true}
	worker := newTestFunding8hWorker(store)
	worker.now = func() time.Time { return end }
	worker.runOnce(context.Background())
	if store.markCalls.Load() != 0 {
		t.Fatalf("pre-ingest mark=%d", store.markCalls.Load())
	}

	store.rows = append(append([]funding8hExitRow{}, incomplete...),
		funding8hScanRow("combo-1", "b", end, "0", "8", "0.05", target),
	)
	worker.runOnce(context.Background())
	if store.markCalls.Load() != 1 || store.lastReason != "funding_8h_floor" {
		t.Fatalf("post-ingest list=%d mark=%d reason=%s",
			store.listCalls.Load(), store.markCalls.Load(), store.lastReason)
	}
}

func TestFunding8hExitWorkerDoesNotRemarkAfterApplied(t *testing.T) {
	end := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	store := &stubFunding8hStore{rows: triggeringFunding8hRows("combo-1", end), applied: true}
	worker := newTestFunding8hWorker(store)
	worker.now = func() time.Time { return end }
	worker.runOnce(context.Background())
	worker.runOnce(context.Background())
	if store.markCalls.Load() != 2 {
		t.Fatalf("markCalls=%d", store.markCalls.Load())
	}
	if !store.appliedOnce {
		t.Fatal("first round must apply")
	}
}

func TestFunding8hExitWorkerSkipsDuplicateKeys(t *testing.T) {
	end := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	rows := triggeringFunding8hRows("combo-1", end)
	store := &stubFunding8hStore{rows: append(rows, rows[0]), applied: true}
	worker := newTestFunding8hWorker(store)
	worker.now = func() time.Time { return end }
	worker.runOnce(context.Background())
	if store.markCalls.Load() != 0 {
		t.Fatalf("duplicate rows must not flatten, markCalls=%d", store.markCalls.Load())
	}
}

func newTestFunding8hWorker(store *stubFunding8hStore) *ArbitrageFunding8hExitWorker {
	worker := NewArbitrageFunding8hExitWorker(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	worker.queryTimeout = time.Second
	return worker
}

func waitAtomic(t *testing.T, value *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if value.Load() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("got %d want %d", value.Load(), want)
}

func waitInFlightClear(t *testing.T, worker *ArbitrageFunding8hExitWorker) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !worker.inFlight.Load() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("inFlight stayed set")
}

func triggeringFunding8hRows(id string, end time.Time) []funding8hExitRow {
	start := end.Add(-8 * time.Hour)
	rateA := "0.0001"
	rateB := "0"
	hours := "8"
	floor := "0.05"
	return []funding8hExitRow{
		funding8hScanRow(id, "a", start, "1", hours, floor, start),
		funding8hScanRow(id, "a", end, rateA, hours, floor, start),
		funding8hScanRow(id, "b", start, "1", hours, floor, start),
		funding8hScanRow(id, "b", end, rateB, hours, floor, start),
	}
}

func funding8hHourlyRows(
	id, leg string, start, end time.Time, rate, hours, floor string, target time.Time,
) []funding8hExitRow {
	rows := []funding8hExitRow{funding8hScanRow(id, leg, start, "0", hours, floor, target)}
	for at := start.Add(time.Hour); !at.After(end); at = at.Add(time.Hour) {
		rows = append(rows, funding8hScanRow(id, leg, at, rate, hours, floor, target))
	}
	return rows
}

func funding8hScanRow(
	id, leg string, at time.Time, rate, hours, floor string, target time.Time,
) funding8hExitRow {
	atCopy := at
	rateCopy := rate
	hoursCopy := hours
	targetCopy := target.UTC()
	return funding8hExitRow{
		CombinationID: id, Version: 4, EntryDirection: "ask", Floor: floor,
		Leg: leg, ContractType: "perpetual", FundingTime: &atCopy,
		FundingRate: &rateCopy, IntervalHours: &hoursCopy,
		TargetReachedAt: &targetCopy,
	}
}
