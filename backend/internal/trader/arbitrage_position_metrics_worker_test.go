package trader

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"selfquant/backend/internal/trader/marketdata"
)

type stubPositionMetricsStore struct {
	items         []ArbitrageCombination
	listErr       error
	calls         atomic.Int64
	closeCalls    atomic.Int64
	lastID        string
	lastMidA      string
	lastMidB      string
	lastCloseID   string
	lastCloseVer  int64
	persistResult arbitragePositionMetricsPersistResult
	persistErr    error
	closeApplied  bool
	closeErr      error
}

func (s *stubPositionMetricsStore) ListRunningArbitrageCombinationsForMetrics(
	context.Context,
) ([]ArbitrageCombination, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.items, nil
}

func (s *stubPositionMetricsStore) PersistRunningArbitragePositionMetrics(
	_ context.Context, id, midA, midB string, _ time.Time,
) (arbitragePositionMetricsPersistResult, error) {
	s.calls.Add(1)
	s.lastID, s.lastMidA, s.lastMidB = id, midA, midB
	if s.persistErr != nil {
		return arbitragePositionMetricsPersistResult{}, s.persistErr
	}
	result := s.persistResult
	if result.Combination.ID == "" {
		result.Combination = ArbitrageCombination{ID: id}
	}
	return result, nil
}

func (s *stubPositionMetricsStore) MarkOneShotExiting(
	_ context.Context, id string, version int64, _ string, _ map[string]any,
) (ArbitrageCombination, bool, error) {
	s.closeCalls.Add(1)
	s.lastCloseID = id
	s.lastCloseVer = version
	if s.closeErr != nil {
		return ArbitrageCombination{}, false, s.closeErr
	}
	if !s.closeApplied {
		return ArbitrageCombination{ID: id, Version: version}, false, nil
	}
	return ArbitrageCombination{
		ID: id, Version: version + 1, Status: "running",
		RunMode: "one_shot", OneShotPhase: "exiting",
	}, true, nil
}

type stubBBOSource struct {
	values map[marketdata.Key]marketdata.BBO
	now    func() time.Time
}

func (s stubBBOSource) Latest(key marketdata.Key) (marketdata.BBO, error) {
	value, ok := s.values[key]
	if !ok {
		return marketdata.BBO{}, marketdata.ErrNoValue
	}
	if s.now != nil {
		value.ReceiveTimestamp = s.now()
	}
	return value, nil
}

func metricsTestKeys(t *testing.T) (marketdata.Key, marketdata.Key) {
	t.Helper()
	keyA, err := marketdata.NewKey("binance", "perpetual", "BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := marketdata.NewKey("okx", "perpetual", "BTC-USDT-SWAP")
	if err != nil {
		t.Fatal(err)
	}
	return keyA, keyB
}

func metricsTestCombo() ArbitrageCombination {
	return ArbitrageCombination{
		ID:               "combo-1",
		Status:           "running",
		CreatedAt:        time.Now().UTC().Add(-arbitragePositionMetricsInterval - time.Second),
		LegABasePosition: "1",
		LegBBasePosition: "-1",
		LegA: ArbitrageLeg{
			Exchange: "binance", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
		},
		LegB: ArbitrageLeg{
			Exchange: "okx", ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
		},
	}
}

func TestPositionMetricsWorkerDoesNotPersistEverySample(t *testing.T) {
	keyA, keyB := metricsTestKeys(t)
	now := time.Now().UTC()
	current := now
	store := &stubPositionMetricsStore{items: []ArbitrageCombination{metricsTestCombo()}}
	worker := NewArbitragePositionMetricsWorker(store, stubBBOSource{
		now: func() time.Time { return current },
		values: map[marketdata.Key]marketdata.BBO{
			keyA: {BidPrice: "100", AskPrice: "102"},
			keyB: {BidPrice: "101", AskPrice: "103"},
		},
	}, slog.Default())
	worker.refreshAt(context.Background(), current)
	if store.calls.Load() != 1 {
		t.Fatalf("first collect should persist: calls=%d", store.calls.Load())
	}
	current = now.Add(5 * time.Second)
	worker.refreshAt(context.Background(), current)
	if store.calls.Load() != 1 {
		t.Fatalf("duplicate persist within 10m: calls=%d", store.calls.Load())
	}
	if _, ok := worker.lastPersistAt("combo-1"); !ok {
		t.Fatal("expected lastPersist clock for running combination")
	}
}

func TestPositionMetricsWorkerPersistsRunningAfterInterval(t *testing.T) {
	keyA, keyB := metricsTestKeys(t)
	now := time.Now().UTC()
	store := &stubPositionMetricsStore{items: []ArbitrageCombination{metricsTestCombo()}}
	current := now
	worker := NewArbitragePositionMetricsWorker(store, stubBBOSource{
		now: func() time.Time { return current },
		values: map[marketdata.Key]marketdata.BBO{
			keyA: {BidPrice: "100", AskPrice: "102"},
			keyB: {BidPrice: "101", AskPrice: "103"},
		},
	}, slog.Default())
	worker.refreshAt(context.Background(), current)
	if store.calls.Load() != 1 || store.lastID != "combo-1" {
		t.Fatalf("first persist calls=%d id=%s", store.calls.Load(), store.lastID)
	}
	current = now.Add(arbitragePositionMetricsInterval)
	worker.refreshAt(context.Background(), current)
	if store.calls.Load() != 2 || store.lastID != "combo-1" {
		t.Fatalf("persist calls=%d id=%s", store.calls.Load(), store.lastID)
	}
	if store.lastMidA != "101" || store.lastMidB != "102" {
		t.Fatalf("mids=%s/%s", store.lastMidA, store.lastMidB)
	}
}

func TestPositionMetricsWorkerRefreshOnlyPersistsMetrics(t *testing.T) {
	keyA, keyB := metricsTestKeys(t)
	combo := metricsTestCombo()
	combo.RunMode = "one_shot"
	combo.OneShotPhase = "waiting_exit"
	combo.ExitPolicy = "annualized"
	combo.ExitAnnualizedRate = "0.1"
	combo.CombinedPositionAnnualized = "0.2"
	store := &stubPositionMetricsStore{items: []ArbitrageCombination{combo}}
	worker := NewArbitragePositionMetricsWorker(store, stubBBOSource{
		now: time.Now,
		values: map[marketdata.Key]marketdata.BBO{
			keyA: {BidPrice: "100", AskPrice: "102"},
			keyB: {BidPrice: "101", AskPrice: "103"},
		},
	}, slog.Default())
	worker.refreshAt(context.Background(), time.Now().UTC())
	if store.calls.Load() != 1 {
		t.Fatalf("worker should persist once, calls=%d", store.calls.Load())
	}
}

func TestPositionMetricsWorkerDoesNotPersistClosing(t *testing.T) {
	keyA, keyB := metricsTestKeys(t)
	now := time.Now().UTC()
	combo := metricsTestCombo()
	combo.Status = "closing"
	store := &stubPositionMetricsStore{items: []ArbitrageCombination{combo}}
	worker := NewArbitragePositionMetricsWorker(store, stubBBOSource{
		now: func() time.Time { return now },
		values: map[marketdata.Key]marketdata.BBO{
			keyA: {BidPrice: "100", AskPrice: "102"},
			keyB: {BidPrice: "101", AskPrice: "103"},
		},
	}, slog.Default())
	worker.refreshAt(context.Background(), now)
	worker.refreshAt(context.Background(), now.Add(arbitragePositionMetricsInterval))
	if store.calls.Load() != 0 {
		t.Fatalf("closing must not persist: calls=%d id=%s", store.calls.Load(), store.lastID)
	}
}

func TestPositionMetricsWorkerPrunesMissingCombinations(t *testing.T) {
	now := time.Now().UTC()
	store := &stubPositionMetricsStore{items: []ArbitrageCombination{metricsTestCombo()}}
	worker := NewArbitragePositionMetricsWorker(store, stubBBOSource{}, slog.Default())
	worker.refreshAt(context.Background(), now)
	if _, ok := worker.lastPersistAt("combo-1"); !ok {
		t.Fatal("expected lastPersist for listed combination")
	}
	store.items = nil
	worker.refreshAt(context.Background(), now.Add(5*time.Second))
	if _, ok := worker.lastPersistAt("combo-1"); ok {
		t.Fatal("lastPersist leaked after combination left the list")
	}
}

func TestPositionMetricsWorkerPassesEmptyMidsWithoutBBO(t *testing.T) {
	now := time.Now().UTC()
	store := &stubPositionMetricsStore{
		items: []ArbitrageCombination{metricsTestCombo()},
	}
	worker := NewArbitragePositionMetricsWorker(store, stubBBOSource{}, slog.Default())
	worker.refreshAt(context.Background(), now)
	if store.calls.Load() != 1 {
		t.Fatalf("first persist calls=%d", store.calls.Load())
	}
	worker.refreshAt(context.Background(), now.Add(arbitragePositionMetricsInterval))
	if store.calls.Load() != 2 {
		t.Fatalf("persist calls=%d", store.calls.Load())
	}
	if store.lastMidA != "" || store.lastMidB != "" {
		t.Fatalf("expected empty mids, got %s/%s", store.lastMidA, store.lastMidB)
	}
}

func TestPositionMetricsWorkerCompensatesBeforePersist(t *testing.T) {
	keyA, keyB := metricsTestKeys(t)
	now := time.Now().UTC()
	store := &stubPositionMetricsStore{items: []ArbitrageCombination{metricsTestCombo()}}
	compensator := &recordingFillAverageCompensator{store: store}
	worker := NewArbitragePositionMetricsWorker(store, stubBBOSource{
		values: map[marketdata.Key]marketdata.BBO{
			keyA: {BidPrice: "100", AskPrice: "102"},
			keyB: {BidPrice: "101", AskPrice: "103"},
		},
	}, slog.Default())
	worker.ConfigureFillAverageCompensator(compensator)
	worker.refreshAt(context.Background(), now)
	if !compensator.called {
		t.Fatal("expected fill-average compensate before persist")
	}
	if len(compensator.ids) != 1 || compensator.ids[0] != "combo-1" {
		t.Fatalf("ids=%v", compensator.ids)
	}
	if store.calls.Load() != 1 {
		t.Fatalf("persist calls=%d", store.calls.Load())
	}
	if compensator.persistDuringCompensate {
		t.Fatal("persist ran inside compensate")
	}
}

func TestPositionMetricsWorkerMemberSyncDoesNotBulkPersist(t *testing.T) {
	now := time.Now().UTC()
	fresh := metricsTestCombo()
	fresh.ID = "combo-fresh"
	fresh.CreatedAt = now
	due := metricsTestCombo()
	due.ID = "combo-due"
	store := &stubPositionMetricsStore{items: []ArbitrageCombination{fresh, due}}
	worker := NewArbitragePositionMetricsWorker(store, stubBBOSource{}, slog.Default())
	worker.refreshAt(context.Background(), now)
	if store.calls.Load() != 1 || store.lastID != "combo-due" {
		t.Fatalf("only overdue member should persist: calls=%d id=%s", store.calls.Load(), store.lastID)
	}
}

func TestPositionMetricsWorkerRegisterCatchupWithoutList(t *testing.T) {
	now := time.Now().UTC()
	store := &stubPositionMetricsStore{}
	worker := NewArbitragePositionMetricsWorker(store, stubBBOSource{}, slog.Default())
	worker.RegisterCombination(metricsTestCombo())
	worker.refreshDue(context.Background(), now)
	if store.calls.Load() != 1 {
		t.Fatalf("registered catch-up should persist: calls=%d", store.calls.Load())
	}
}

func TestPositionMetricsWorkerKeepsPinnedWhenAbsentFromList(t *testing.T) {
	store := &stubPositionMetricsStore{}
	worker := NewArbitragePositionMetricsWorker(store, stubBBOSource{}, slog.Default())
	combo := metricsTestCombo()
	worker.RegisterCombination(combo)
	worker.syncMembers(context.Background())
	worker.mu.Lock()
	member, ok := worker.members[combo.ID]
	worker.mu.Unlock()
	if !ok || member == nil || !member.pinned {
		t.Fatal("pinned member must survive empty metrics list")
	}
	if store.calls.Load() != 0 {
		t.Fatalf("member sync must not persist: %d", store.calls.Load())
	}
}

func TestPositionMetricsWorkerSQLErrorRetriesAfter15s(t *testing.T) {
	now := time.Now().UTC()
	store := &stubPositionMetricsStore{
		items:      []ArbitrageCombination{metricsTestCombo()},
		persistErr: errors.New("persist sql"),
	}
	worker := NewArbitragePositionMetricsWorker(store, stubBBOSource{}, slog.Default())
	worker.refreshAt(context.Background(), now)
	if store.calls.Load() != 1 {
		t.Fatalf("first persist calls=%d", store.calls.Load())
	}
	worker.refreshAt(context.Background(), now.Add(time.Second))
	if store.calls.Load() != 1 {
		t.Fatalf("sql error must not busy-loop: calls=%d", store.calls.Load())
	}
	worker.refreshAt(context.Background(), now.Add(metricsApplyRetryDelay))
	if store.calls.Load() != 2 {
		t.Fatalf("sql error should retry after 15s: calls=%d", store.calls.Load())
	}
}

func TestClassifyMetricsApplyMissReasons(t *testing.T) {
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	calculated := now.Add(-time.Minute)
	if metricsApplyMissReason("closing", &calculated, now, false, false) != MetricsInactive {
		t.Fatal("closing should be inactive")
	}
	if metricsApplyMissReason("running", &calculated, now, true, false) != MetricsActiveWork {
		t.Fatal("active execution")
	}
	if metricsApplyMissReason("running", &calculated, now, false, true) != MetricsActiveWork {
		t.Fatal("active filled order")
	}
	current := now
	if metricsApplyMissReason("running", &current, now, false, false) != MetricsAlreadyCurrent {
		t.Fatal("already current")
	}
	if metricsApplyMissReason("running", &calculated, now, false, false) != MetricsNotReady {
		t.Fatal("default not ready")
	}
}

type recordingFillAverageCompensator struct {
	store                   *stubPositionMetricsStore
	called                  bool
	persistDuringCompensate bool
	ids                     []string
}

func (c *recordingFillAverageCompensator) Compensate(_ context.Context, ids []string) {
	c.called = true
	c.ids = append([]string{}, ids...)
	if c.store.calls.Load() != 0 {
		c.persistDuringCompensate = true
	}
}
