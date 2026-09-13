package trader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

func defaultSizingInstrument() Instrument {
	return Instrument{
		Exchange: "binance", ContractType: "perpetual", ContractSize: "1",
		QuantityStep: "0.001", PriceTick: "0.01",
		MinQuantity: "0.001", MinNotional: "5",
		MinQuantityStatus:  exchange.ConstraintKnown,
		MinNotionalStatus:  exchange.ConstraintKnown,
		MaxQuantityStatus:  exchange.ConstraintNotApplicable,
		MarketQuantityStep: "0.001", MarketMinQuantity: "0.001",
		MarketMinNotional:        "5",
		MarketQuantityStepStatus: exchange.ConstraintKnown,
		MarketMinQuantityStatus:  exchange.ConstraintKnown,
		MarketMaxQuantityStatus:  exchange.ConstraintNotApplicable,
		MarketMinNotionalStatus:  exchange.ConstraintKnown,
	}
}

type anyInstrumentCatalog struct {
	item Instrument
}

func (c anyInstrumentCatalog) List(context.Context, string, string) ([]Instrument, error) {
	return []Instrument{c.item}, nil
}

func (c anyInstrumentCatalog) Get(_ context.Context, id int64) (Instrument, error) {
	item := c.item
	item.ID = id
	return item, nil
}

func newTestArbitrageScheduler(
	store arbitrageStore,
	market *marketdata.Manager,
	runner arbitrageExecutionRunner,
	controlInterval, coalesceWindow, lease time.Duration,
	batch, workers int,
	maxAccount, maxVenue int,
	dryRun bool,
	logger *slog.Logger,
) *ArbitrageScheduler {
	scheduler := NewArbitrageScheduler(
		store, market, runner, controlInterval, coalesceWindow, lease,
		batch, workers, maxAccount, maxVenue, dryRun, logger,
	)
	scheduler.notionalRand = fixedNotionalRand{decimal.Zero}
	scheduler.ConfigureArbitrageInstruments(anyInstrumentCatalog{item: defaultSizingInstrument()})
	return scheduler
}

type dryRunStore struct {
	arbitrageStore
	mu                   sync.Mutex
	leased               bool
	item                 ArbitrageCombination
	eventTypes           []string
	claims               int
	claim                bool
	claimLimit           int
	activeExecution      *ArbitrageExecution
	activeLookups        int
	refreshes            int
	controlProbes        int
	snapshots            int
	renewals             int
	renewResult          *bool
	finalizeCalls        int
	lastFinalizeID       string
	finalizeResult       confirmedAbsentFinalizeResult
	finalizeErr          error
	dustGateCalls        int
	dustGateErr          error
	dustGateLiveOrder    bool
	dustGateUnreconciled bool
	withDustSkip         bool
	withDustErr          error
}

type closeRetryStore struct {
	arbitrageStore
	item  ArbitrageCombination
	calls int
}

func (s *closeRetryStore) RecordArbitrageCloseFailure(
	_ context.Context, _ string, key, message string,
) (ArbitrageCombination, error) {
	s.calls++
	s.item.ErrorMessage = message
	s.item.LastFailureKey = key
	s.item.ConsecutiveFailures++
	s.item.NextRetryAt = time.Now().UTC().Add(time.Minute)
	return s.item, nil
}

type closeRetryRunner struct {
	calls int
	err   error
}

func (*closeRetryRunner) Execute(
	context.Context, ArbitrageCombination, ArbitrageExecution,
	marketdata.BBO, marketdata.BBO, *executionWorkPermit,
) {
}

func (*closeRetryRunner) RecoverWithoutMarketData(
	context.Context, ArbitrageCombination, ArbitrageExecution,
) error {
	return nil
}

func (*closeRetryRunner) ReconcileCircuitOpen(
	context.Context, ArbitrageCombination, ArbitrageExecution,
) error {
	return nil
}

func (r *closeRetryRunner) CloseCombination(
	context.Context, ArbitrageCombination,
) error {
	r.calls++
	return r.err
}

func (s *dryRunStore) LeaseArbitrageCombinations(
	context.Context, int, time.Duration,
) ([]ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leased {
		return nil, nil
	}
	s.leased = true
	return []ArbitrageCombination{s.item}, nil
}

func (s *dryRunStore) UpdateArbitrageMarketSnapshot(
	_ context.Context,
	_ string,
	ask, bid string,
	stale bool,
) (arbitrageMarketSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots++
	s.item.CurrentAskSpreadBps = ask
	s.item.CurrentBidSpreadBps = bid
	s.item.MarketDataStale = stale
	return arbitrageMarketSnapshot{
		CurrentAskSpreadBps: s.item.CurrentAskSpreadBps,
		CurrentBidSpreadBps: s.item.CurrentBidSpreadBps,
		MarketDataStale:     s.item.MarketDataStale,
		RuntimeState:        s.item.RuntimeState,
		Status:              s.item.Status,
		Version:             s.item.Version,
		UpdatedAt:           s.item.UpdatedAt,
	}, nil
}

func (s *dryRunStore) RecordArbitrageCloseFailure(
	_ context.Context, _ string, key, message string,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.item.ErrorMessage = message
	s.item.LastFailureKey = key
	s.item.ConsecutiveFailures++
	s.item.NextRetryAt = time.Now().UTC().Add(2 * time.Second)
	return s.item, nil
}

func (s *dryRunStore) AppendArbitrageEvent(
	_ context.Context,
	_, _, eventType string,
	_ map[string]any,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventTypes = append(s.eventTypes, eventType)
	return nil
}

func (s *dryRunStore) ClaimArbitrageExecution(
	_ context.Context, execution ArbitrageExecution,
) (ArbitrageExecution, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	if s.claimLimit > 0 && s.claims > s.claimLimit {
		return ArbitrageExecution{}, false, nil
	}
	return execution, s.claim, nil
}

func (s *dryRunStore) MarkOneShotWaitingExit(
	_ context.Context, id string, version int64, scheduled time.Time,
) (ArbitrageCombination, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.item.ID != id || s.item.Version != version {
		return s.item, false, nil
	}
	s.item.OneShotPhase = "waiting_exit"
	s.item.TargetReachedAt = time.Now().UTC()
	s.item.ScheduledExitAt = scheduled
	s.item.Version++
	return s.item, true, nil
}

func (s *dryRunStore) MarkOneShotExiting(
	_ context.Context, id string, version int64, _ string, _ map[string]any,
) (ArbitrageCombination, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.item.ID != id || s.item.Version != version || s.item.Status != "running" {
		return s.item, false, nil
	}
	s.item.OneShotPhase = "exiting"
	s.item.Version++
	return s.item, true, nil
}

func (s *dryRunStore) MarkOneShotExited(
	_ context.Context, id string, version int64,
) (ArbitrageCombination, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.item.ID != id || s.item.Version != version ||
		s.item.Status != "running" || s.item.OneShotPhase != "exiting" ||
		!combinationLooksFlat(s.item) {
		return s.item, false, nil
	}
	s.item.OneShotPhase = "exited"
	s.item.Version++
	return s.item, true, nil
}

func (s *dryRunStore) MarkOneShotExitedWithDust(
	_ context.Context, id string, version int64, expectedA, expectedB, expectedCarry string,
) (ArbitrageCombination, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.withDustErr != nil {
		return s.item, false, s.withDustErr
	}
	if s.withDustSkip {
		return s.item, false, nil
	}
	if s.item.ID != id || s.item.Version != version ||
		s.item.Status != "running" || s.item.RunMode != "one_shot" ||
		s.item.OneShotPhase != "exiting" ||
		s.item.PositionUncertain || s.item.CircuitOpen ||
		s.item.RuntimeState == "manual_intervention" {
		return s.item, false, nil
	}
	legA := parseDecimal(s.item.LegABasePosition)
	legB := parseDecimal(s.item.LegBBasePosition)
	carry := parseDecimal(s.item.CarryBaseQuantity)
	if carry.IsZero() || !carry.Equal(legA.Add(legB)) ||
		!legA.Equal(parseDecimal(expectedA)) ||
		!legB.Equal(parseDecimal(expectedB)) ||
		!carry.Equal(parseDecimal(expectedCarry)) {
		return s.item, false, nil
	}
	if (legA.IsZero() == legB.IsZero()) || (!legA.IsZero() && !legB.IsZero()) {
		return s.item, false, nil
	}
	s.item.OneShotPhase = "exited"
	s.item.Version++
	s.eventTypes = append(s.eventTypes, "one_shot_exited_with_dust")
	return s.item, true, nil
}

func (s *dryRunStore) ArbitrageDustOrderGate(
	_ context.Context, _ string, _ time.Time,
) (liveOrder, unreconciled, liveExecution bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dustGateCalls++
	if s.dustGateErr != nil {
		return false, false, false, s.dustGateErr
	}
	if s.activeExecution != nil &&
		!terminalArbitrageExecutionStatus(s.activeExecution.Status) {
		liveExecution = true
	}
	return s.dustGateLiveOrder, s.dustGateUnreconciled, liveExecution, nil
}

func (s *dryRunStore) UpdateArbitrageCombinationRuntime(
	_ context.Context, item ArbitrageCombination,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item.Version++
	s.item = item
	return s.item, nil
}

func (s *dryRunStore) GetActiveArbitrageExecution(
	context.Context, string,
) (ArbitrageExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeLookups++
	if s.activeExecution != nil &&
		!terminalArbitrageExecutionStatus(s.activeExecution.Status) {
		return *s.activeExecution, nil
	}
	return ArbitrageExecution{}, ErrNotFound
}

func (s *dryRunStore) FinalizeConfirmedAbsentZeroFillExecution(
	_ context.Context, executionID string,
) (confirmedAbsentFinalizeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalizeCalls++
	s.lastFinalizeID = executionID
	if s.finalizeErr != nil {
		return confirmedAbsentFinalizeSkipped, s.finalizeErr
	}
	if s.finalizeResult == "" {
		return confirmedAbsentFinalizeSkipped, nil
	}
	return s.finalizeResult, nil
}

func (s *dryRunStore) GetArbitrageCombinationByOwner(
	_ context.Context, _, _ string,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshes++
	return s.item, nil
}

func (s *dryRunStore) GetArbitrageControlSummary(
	_ context.Context,
	_ string,
) (arbitrageControlSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.controlProbes++
	return arbitrageControlSummary{
		Version: s.item.Version, Status: s.item.Status,
		RuntimeState: s.item.RuntimeState,
	}, nil
}

func (s *dryRunStore) RenewArbitrageLease(context.Context, string, time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renewals++
	if s.renewResult != nil {
		return *s.renewResult, nil
	}
	return true, nil
}

func TestArbitrageSchedulerRefreshAppliesUpdatedParametersToNextEvaluation(t *testing.T) {
	store := &dryRunStore{item: ArbitrageCombination{
		ID: "combo-1", OwnerUsername: "admin", Status: "running",
		TargetNotional: "12000", OrderNotional: "750",
		AskThresholdBps: "-7", BidThresholdBps: "4", Version: 2,
	}}
	scheduler := &ArbitrageScheduler{store: store}
	runtime := &arbitrageRuntime{combination: ArbitrageCombination{
		ID: "combo-1", OwnerUsername: "admin", Status: "running",
		TargetNotional: "10000", OrderNotional: "500",
		AskThresholdBps: "12", BidThresholdBps: "-8", Version: 1,
	}}
	stop, refreshed := scheduler.refreshCombination(context.Background(), runtime)
	if stop || !refreshed {
		t.Fatalf("stop=%v refreshed=%v", stop, refreshed)
	}
	if runtime.combination.TargetNotional != "12000" ||
		runtime.combination.OrderNotional != "750" ||
		runtime.combination.AskThresholdBps != "-7" ||
		runtime.combination.BidThresholdBps != "4" {
		t.Fatalf("combination=%+v", runtime.combination)
	}
}

func TestArbitrageSchedulerRefreshSkipsFullLoadWhenControlSummaryUnchanged(t *testing.T) {
	item := ArbitrageCombination{
		ID: "combo-1", OwnerUsername: "admin", Status: "running",
		RuntimeState: "monitoring", Version: 7,
	}
	store := &dryRunStore{item: item}
	scheduler := &ArbitrageScheduler{store: store}
	runtime := &arbitrageRuntime{combination: item}

	stop, refreshed := scheduler.refreshCombination(context.Background(), runtime)
	if stop || refreshed {
		t.Fatalf("stop=%v refreshed=%v", stop, refreshed)
	}
	if store.controlProbes != 1 || store.refreshes != 0 {
		t.Fatalf("control probes=%d full reads=%d", store.controlProbes, store.refreshes)
	}
	stats := scheduler.Stats()
	if stats.ControlProbes != 1 || stats.FullReloads != 0 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestArbitrageSchedulerRefreshReloadsOnRuntimeStateChange(t *testing.T) {
	item := ArbitrageCombination{
		ID: "combo-1", OwnerUsername: "admin", Status: "running",
		RuntimeState: "position_uncertain", Version: 7,
	}
	store := &dryRunStore{item: item}
	scheduler := &ArbitrageScheduler{store: store}
	runtime := &arbitrageRuntime{combination: item}
	runtime.combination.RuntimeState = "monitoring"

	stop, refreshed := scheduler.refreshCombination(context.Background(), runtime)
	if stop || !refreshed || runtime.combination.RuntimeState != "position_uncertain" {
		t.Fatalf("stop=%v refreshed=%v item=%+v", stop, refreshed, runtime.combination)
	}
}

func TestArbitrageMarketSnapshotDoesNotMaskConcurrentVersionChange(t *testing.T) {
	item := ArbitrageCombination{
		Version: 7, Status: "running", RuntimeState: "monitoring",
		CurrentAskSpreadBps: "1", CurrentBidSpreadBps: "2",
	}
	requiresReload := applyArbitrageMarketSnapshot(&item, arbitrageMarketSnapshot{
		Version: 8, Status: "running", RuntimeState: "monitoring",
		CurrentAskSpreadBps: "3", CurrentBidSpreadBps: "4",
	})
	if !requiresReload {
		t.Fatal("concurrent version change must force a full reload")
	}
	if item.Version != 7 {
		t.Fatalf("snapshot masked concurrent version change: version=%d", item.Version)
	}
	if item.CurrentAskSpreadBps != "3" || item.CurrentBidSpreadBps != "4" {
		t.Fatalf("snapshot fields were not merged: %+v", item)
	}
}

type schedulerConnection struct {
	reads chan []byte
	done  chan struct{}
	once  sync.Once
}

func (c *schedulerConnection) Read() ([]byte, error) {
	select {
	case payload := <-c.reads:
		return payload, nil
	case <-c.done:
		return nil, io.EOF
	}
}

func (c *schedulerConnection) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

type schedulerConnector struct {
	mu          sync.Mutex
	connections map[marketdata.Key]*schedulerConnection
	sequences   map[marketdata.Key][]*schedulerConnection
	connects    map[marketdata.Key]int
}

type schedulerRunner struct {
	executed      chan ArbitrageExecution
	executeHook   func()
	recovered     chan ArbitrageExecution
	recoverHook   func()
	reconciled    chan ArbitrageExecution
	reconcileErr  error
	reconcileHook func()
	closed        chan ArbitrageCombination
	closeErr      error
}

type blockingSchedulerRunner struct {
	started      chan struct{}
	release      chan struct{}
	canceled     chan struct{}
	ignoreCancel bool
}

type blockingRecoveryRunner struct {
	started chan struct{}
	release chan struct{}
	closed  chan ArbitrageCombination
}

type activeResumeStore struct {
	arbitrageStore
	execution ArbitrageExecution
}

func (s activeResumeStore) GetActiveArbitrageExecution(
	context.Context,
	string,
) (ArbitrageExecution, error) {
	return s.execution, nil
}

func (r *schedulerRunner) Execute(
	_ context.Context,
	_ ArbitrageCombination,
	execution ArbitrageExecution,
	_, _ marketdata.BBO,
	_ *executionWorkPermit,
) {
	if r.executeHook != nil {
		r.executeHook()
	}
	r.executed <- execution
}

func (r *blockingSchedulerRunner) Execute(
	ctx context.Context,
	_ ArbitrageCombination,
	_ ArbitrageExecution,
	_, _ marketdata.BBO,
	_ *executionWorkPermit,
) {
	close(r.started)
	if r.ignoreCancel {
		<-r.release
		return
	}
	select {
	case <-r.release:
	case <-ctx.Done():
		close(r.canceled)
	}
}

func (*blockingSchedulerRunner) RecoverWithoutMarketData(
	context.Context, ArbitrageCombination, ArbitrageExecution,
) error {
	return nil
}

func (*blockingSchedulerRunner) ReconcileCircuitOpen(
	context.Context, ArbitrageCombination, ArbitrageExecution,
) error {
	return nil
}

func (*blockingSchedulerRunner) CloseCombination(
	context.Context, ArbitrageCombination,
) error {
	return nil
}

func (*blockingRecoveryRunner) Execute(
	context.Context, ArbitrageCombination, ArbitrageExecution,
	marketdata.BBO, marketdata.BBO, *executionWorkPermit,
) {
}

func (r *blockingRecoveryRunner) RecoverWithoutMarketData(
	context.Context, ArbitrageCombination, ArbitrageExecution,
) error {
	close(r.started)
	<-r.release
	return nil
}

func (*blockingRecoveryRunner) ReconcileCircuitOpen(
	context.Context, ArbitrageCombination, ArbitrageExecution,
) error {
	return nil
}

func (r *blockingRecoveryRunner) CloseCombination(
	_ context.Context, item ArbitrageCombination,
) error {
	r.closed <- item
	return nil
}

func (r *schedulerRunner) RecoverWithoutMarketData(
	_ context.Context, _ ArbitrageCombination, execution ArbitrageExecution,
) error {
	if r.recoverHook != nil {
		r.recoverHook()
	}
	if r.recovered != nil {
		r.recovered <- execution
	}
	return nil
}

func (r *schedulerRunner) ReconcileCircuitOpen(
	_ context.Context, _ ArbitrageCombination, execution ArbitrageExecution,
) error {
	if r.reconcileHook != nil {
		r.reconcileHook()
	}
	if r.reconciled != nil {
		r.reconciled <- execution
	}
	return r.reconcileErr
}

func (r *schedulerRunner) CloseCombination(
	_ context.Context,
	item ArbitrageCombination,
) error {
	if r.closed != nil {
		r.closed <- item
	}
	return r.closeErr
}

func TestArbitrageSchedulerCloseBackoffBlocksEveryEntryPoint(t *testing.T) {
	item := ArbitrageCombination{
		ID: "combo-close", Status: "closing", RuntimeState: "closing",
		NextRetryAt: time.Now().UTC().Add(time.Minute),
	}
	store := &closeRetryStore{item: item}
	runner := &closeRetryRunner{err: ErrCloseOrderUncertain}
	scheduler := &ArbitrageScheduler{
		store: store, runner: runner, logger: slog.Default(),
	}
	runtime := &arbitrageRuntime{combination: item}

	if scheduler.closeCombination(context.Background(), runtime) ||
		scheduler.evaluate(context.Background(), runtime, "bbo_event") {
		t.Fatal("backoff should not stop runtime")
	}
	if runner.calls != 0 || store.calls != 0 {
		t.Fatalf("runner=%d store=%d", runner.calls, store.calls)
	}

	runtime.combination.NextRetryAt = time.Now().UTC().Add(-time.Second)
	if scheduler.closeCombination(context.Background(), runtime) {
		t.Fatal("failed close should remain scheduled")
	}
	if runner.calls != 1 || store.calls != 1 ||
		runtime.combination.LastFailureKey != "close_order_uncertain" {
		t.Fatalf(
			"runner=%d store=%d combination=%+v",
			runner.calls, store.calls, runtime.combination,
		)
	}
	if scheduler.evaluate(context.Background(), runtime, "bbo_event") {
		t.Fatal("backoff BBO should not stop runtime")
	}
	if runner.calls != 1 {
		t.Fatalf("BBO bypassed backoff: calls=%d", runner.calls)
	}
}

func TestArbitrageSchedulerCloseWaitingSnapshotDoesNotRecordFailure(t *testing.T) {
	item := ArbitrageCombination{
		ID: "combo-close", Status: "closing", RuntimeState: "closing",
		LegABasePosition: "0", LegBBasePosition: "0", CarryBaseQuantity: "0",
	}
	store := &closeRetryStore{item: item}
	runner := &closeRetryRunner{err: ErrArbitrageCloseWaitingSnapshot}
	scheduler := &ArbitrageScheduler{
		store: store, runner: runner, logger: slog.Default(),
	}
	runtime := &arbitrageRuntime{combination: item}
	if scheduler.closeCombination(context.Background(), runtime) {
		t.Fatal("waiting audit should keep runtime")
	}
	if runner.calls != 1 || store.calls != 0 {
		t.Fatalf("runner=%d store=%d", runner.calls, store.calls)
	}
}

func TestArbitrageSchedulerRetriesActiveExecutionAfterRunnerReturns(t *testing.T) {
	execution := ArbitrageExecution{ID: "execution-1", CombinationID: "combo-1", Status: "reconciling"}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 2)}
	scheduler := newTestArbitrageScheduler(
		activeResumeStore{execution: execution},
		nil,
		runner,
		time.Second,
		time.Millisecond,
		time.Second,
		1,
		1,
		1,
		2,
		false,
		nil,
	)
	runtime := &arbitrageRuntime{combination: ArbitrageCombination{
		ID:   "combo-1",
		LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "binance"},
		LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "okx"},
	}}
	if !scheduler.resumeActive(
		context.Background(), runtime, marketdata.BBO{}, marketdata.BBO{},
	) {
		t.Fatal("expected first active execution resume")
	}
	select {
	case <-runner.executed:
	case <-time.After(time.Second):
		t.Fatal("first execution was not run")
	}
	waitForExecutionRunner(t, runtime)
	if !scheduler.resumeActive(
		context.Background(), runtime, marketdata.BBO{}, marketdata.BBO{},
	) {
		t.Fatal("expected second active execution resume")
	}
	select {
	case <-runner.executed:
	case <-time.After(time.Second):
		t.Fatal("active execution was not retried")
	}
	waitForExecutionRunner(t, runtime)
}

func TestArbitrageSchedulerBlockedStateDoesNotResumeActiveExecution(t *testing.T) {
	tests := []struct {
		name        string
		combination ArbitrageCombination
	}{
		{
			name:        "circuit open",
			combination: ArbitrageCombination{CircuitOpen: true},
		},
		{
			name:        "position uncertain flag",
			combination: ArbitrageCombination{PositionUncertain: true},
		},
		{
			name: "manual intervention state",
			combination: ArbitrageCombination{
				RuntimeState: "manual_intervention",
			},
		},
		{
			name: "position uncertain state",
			combination: ArbitrageCombination{
				RuntimeState: "position_uncertain",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			execution := ArbitrageExecution{
				ID: "execution-1", CombinationID: "combo-1", Status: "reconciling",
			}
			runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
			scheduler := newTestArbitrageScheduler(
				activeResumeStore{execution: execution}, nil, runner,
				time.Second, time.Millisecond, time.Second, 1, 1, 1, 2, false, nil,
			)
			test.combination.ID = "combo-1"
			test.combination.LegA = ArbitrageLeg{
				TradingAccountID: 1, Exchange: "binance",
			}
			test.combination.LegB = ArbitrageLeg{
				TradingAccountID: 2, Exchange: "okx",
			}
			runtime := &arbitrageRuntime{combination: test.combination}

			if !scheduler.resumeActive(
				context.Background(), runtime, marketdata.BBO{}, marketdata.BBO{},
			) {
				t.Fatal("blocked state should prevent active execution and new claims")
			}
			select {
			case resumed := <-runner.executed:
				t.Fatalf("blocked active execution resumed: %+v", resumed)
			default:
			}
			if runtime.executionRunning.Load() {
				t.Fatal("blocked runtime was marked execution-running")
			}
		})
	}
}

func TestArbitrageSchedulerActiveExecutionHonorsBackoff(t *testing.T) {
	execution := ArbitrageExecution{ID: "execution-1", CombinationID: "combo-1", Status: "reconciling"}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		activeResumeStore{execution: execution},
		nil,
		runner,
		time.Second,
		time.Millisecond,
		time.Second,
		1,
		1,
		1,
		2,
		false,
		nil,
	)
	runtime := &arbitrageRuntime{combination: ArbitrageCombination{
		ID: "combo-1", NextRetryAt: time.Now().UTC().Add(time.Minute),
		LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "binance"},
		LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "okx"},
	}}
	if !scheduler.resumeActive(
		context.Background(), runtime, marketdata.BBO{}, marketdata.BBO{},
	) {
		t.Fatal("active execution should block new claims during backoff")
	}
	select {
	case execution := <-runner.executed:
		t.Fatalf("execution resumed during backoff: %+v", execution)
	default:
	}
	if runtime.executionRunning.Load() {
		t.Fatal("runtime marked running during backoff")
	}
}

func TestArbitrageSchedulerRecoversActiveOrdersWithoutFreshBBO(t *testing.T) {
	execution := ArbitrageExecution{
		ID: "execution-1", CombinationID: "combo-1", Status: "reconciling",
	}
	runner := &schedulerRunner{recovered: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		activeResumeStore{execution: execution}, nil, runner,
		time.Second, time.Millisecond, time.Second, 1, 1, 1, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: ArbitrageCombination{
		ID: "combo-1", CircuitOpen: true, RuntimeState: "manual_intervention",
		LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "binance"},
		LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "okx"},
	}}
	if !scheduler.resumeWithoutMarketData(context.Background(), runtime) {
		t.Fatal("active execution was not claimed for stale-market recovery")
	}
	select {
	case recovered := <-runner.recovered:
		if recovered.ID != execution.ID {
			t.Fatalf("execution=%+v", recovered)
		}
	case <-time.After(time.Second):
		t.Fatal("stale-market recovery did not run")
	}
	waitForExecutionRunner(t, runtime)
}

func TestArbitrageSchedulerResumeWithoutMarketDataSkipsWhenRecoveryRunning(t *testing.T) {
	execution := ArbitrageExecution{
		ID: "execution-1", CombinationID: "combo-1", Status: "reconciling",
	}
	runner := &schedulerRunner{recovered: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		activeResumeStore{execution: execution}, nil, runner,
		time.Second, time.Millisecond, time.Second, 1, 1, 1, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: ArbitrageCombination{
		ID: "combo-1", CircuitOpen: true, RuntimeState: "manual_intervention",
		LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "binance"},
		LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "okx"},
	}}
	runtime.recoveryRunning.Store(true)
	if !scheduler.resumeWithoutMarketData(context.Background(), runtime) {
		t.Fatal("busy recovery should report the runtime as occupied")
	}
	select {
	case <-runner.recovered:
		t.Fatal("started a second recovery task")
	case <-time.After(50 * time.Millisecond):
	}
}

func waitForExecutionRunner(t *testing.T, runtime *arbitrageRuntime) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for runtime.executionRunning.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if runtime.executionRunning.Load() {
		t.Fatal("execution runner did not release runtime")
	}
}

func (c *schedulerConnector) Connect(
	_ context.Context,
	key marketdata.Key,
) (marketdata.Connection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sequence := c.sequences[key]; len(sequence) > 0 {
		if c.connects == nil {
			c.connects = make(map[marketdata.Key]int)
		}
		index := c.connects[key]
		c.connects[key]++
		if index >= len(sequence) {
			index = len(sequence) - 1
		}
		return sequence[index], nil
	}
	connection := c.connections[key]
	if connection == nil {
		return nil, fmt.Errorf("missing connection for %v", key)
	}
	return connection, nil
}

func TestArbitrageSchedulerDryRunRecordsTriggerWithoutClaimingExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keyA, _ := marketdata.NewKey("binance", "perpetual", "BTCUSDT")
	keyB, _ := marketdata.NewKey("okx", "perpetual", "BTC-USDT-SWAP")
	connectionA := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connectionB := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connector := &schedulerConnector{connections: map[marketdata.Key]*schedulerConnection{
		keyA: connectionA, keyB: connectionB,
	}}
	parser := func(key marketdata.Key, payload []byte, received time.Time) (marketdata.BBO, bool, error) {
		parts := strings.Split(string(payload), ",")
		if len(parts) != 2 {
			return marketdata.BBO{}, false, fmt.Errorf("bad fixture")
		}
		if _, err := strconv.ParseFloat(parts[0], 64); err != nil {
			return marketdata.BBO{}, false, err
		}
		return marketdata.BBO{
			Key: key, BidPrice: parts[0], AskPrice: parts[1],
			BidQuantity: "10000", AskQuantity: "10000",
			VenueTimestamp: received, ReceiveTimestamp: received,
		}, true, nil
	}
	market, err := marketdata.New(marketdata.Options{
		Connector: connector,
		Parsers: map[string]marketdata.Parser{
			"binance": parser, "okx": parser,
		},
		StaleAfter: time.Second, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer market.Close()
	store := &dryRunStore{item: ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61731", OwnerUsername: "admin",
		Status: "running", AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "10000", OrderNotional: "500", PositionNotional: "0",
		MarketDataStale: true,
		LegA: ArbitrageLeg{
			TradingAccountID: 1, Exchange: "binance",
			ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
		},
	}}
	scheduler := newTestArbitrageScheduler(
		store, market, nil, time.Millisecond, time.Millisecond, time.Second,
		10, 2, 2, 2, true, nil,
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("100,101")
	connectionB.reads <- []byte("101,102")
	deadline := time.Now().Add(time.Second)
	for {
		if _, errA := market.Latest(keyA); errA == nil {
			if _, errB := market.Latest(keyB); errB == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("BBO fixtures were not consumed")
		}
		time.Sleep(time.Millisecond)
	}
	scheduler.runOnce(ctx)
	scheduler.closeAll()

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claims != 0 {
		t.Fatalf("dry-run claimed %d executions", store.claims)
	}
	if len(store.eventTypes) != 1 || store.eventTypes[0] != "dry_run_trigger" {
		t.Fatalf("events=%v", store.eventTypes)
	}
}

func TestArbitrageSchedulerLiveModeExecutesSameAccountSpotPerpetualTrigger(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keyA, _ := marketdata.NewKey("gate", "spot", "BEAT_USDT")
	keyB, _ := marketdata.NewKey("gate", "perpetual", "BEAT_USDT")
	connectionA := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connectionB := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connector := &schedulerConnector{connections: map[marketdata.Key]*schedulerConnection{
		keyA: connectionA, keyB: connectionB,
	}}
	parser := func(key marketdata.Key, payload []byte, received time.Time) (marketdata.BBO, bool, error) {
		parts := strings.Split(string(payload), ",")
		return marketdata.BBO{
			Key: key, BidPrice: parts[0], AskPrice: parts[1],
			BidQuantity: "10000", AskQuantity: "10000",
			VenueTimestamp: received, ReceiveTimestamp: received,
		}, true, nil
	}
	market, err := marketdata.New(marketdata.Options{
		Connector: connector,
		Parsers: map[string]marketdata.Parser{
			"gate": parser,
		},
		StaleAfter: time.Second, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer market.Close()
	store := &dryRunStore{claim: true, item: ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61732", OwnerUsername: "admin",
		Status: "running", AskThresholdBps: "10", BidThresholdBps: "-10",
		TargetNotional: "10000", OrderNotional: "500", PositionNotional: "0",
		MarketDataStale: true,
		LegA: ArbitrageLeg{
			TradingAccountID: 1, Exchange: "gate",
			ContractType: "spot", ExchangeSymbol: "BEAT_USDT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 1, Exchange: "gate",
			ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT",
		},
	}}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Millisecond, time.Second,
		10, 2, 1, 1, false, nil,
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")
	deadline := time.Now().Add(time.Second)
	for {
		if _, errA := market.Latest(keyA); errA == nil {
			if _, errB := market.Latest(keyB); errB == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("BBO fixtures were not consumed")
		}
		time.Sleep(time.Millisecond)
	}
	scheduler.runOnce(ctx)
	defer scheduler.closeAll()

	select {
	case execution := <-runner.executed:
		if execution.Direction != "ask" || execution.PositionEffect != "open" ||
			execution.ReduceOnly || parsePositiveDecimal(execution.RequestedNotional).IsZero() {
			t.Fatalf("execution=%+v", execution)
		}
	case <-time.After(time.Second):
		t.Fatal("live trigger was not executed")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claims != 1 {
		t.Fatalf("claims=%d", store.claims)
	}
}

func TestArbitrageSchedulerWaitsForBothFreshLegsBeforeTrigger(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keyA, _ := marketdata.NewKey("gate", "perpetual", "BEAT_USDT")
	keyB, _ := marketdata.NewKey("okx", "perpetual", "BEAT-USDT-SWAP")
	connectionA := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connectionB := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connector := &schedulerConnector{connections: map[marketdata.Key]*schedulerConnection{
		keyA: connectionA, keyB: connectionB,
	}}
	parser := func(key marketdata.Key, payload []byte, received time.Time) (marketdata.BBO, bool, error) {
		parts := strings.Split(string(payload), ",")
		if len(parts) != 2 {
			return marketdata.BBO{}, false, fmt.Errorf("bad BBO fixture")
		}
		return marketdata.BBO{
			Key: key, BidPrice: parts[0], AskPrice: parts[1],
			BidQuantity: "10000", AskQuantity: "10000",
			VenueTimestamp: received, ReceiveTimestamp: received,
		}, true, nil
	}
	market, err := marketdata.New(marketdata.Options{
		Connector: connector,
		Parsers: map[string]marketdata.Parser{
			"gate": parser, "okx": parser,
		},
		StaleAfter: time.Second, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer market.Close()
	store := &dryRunStore{claim: true, item: ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61739", OwnerUsername: "admin",
		Status: "running", AskThresholdBps: "10", BidThresholdBps: "-10",
		TargetNotional: "10000", OrderNotional: "500", PositionNotional: "0",
		MarketDataStale: true,
		LegA: ArbitrageLeg{
			TradingAccountID: 1, Exchange: "gate",
			ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: "BEAT-USDT-SWAP",
		},
	}}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	scheduler.runOnce(ctx)

	connectionA.reads <- []byte("100,100.1")
	waitForFreshBBO(t, market, keyA)
	scheduler.runOnce(ctx)
	store.mu.Lock()
	if store.claims != 0 || !store.item.MarketDataStale {
		t.Fatalf("one-leg state: claims=%d stale=%t", store.claims, store.item.MarketDataStale)
	}
	store.mu.Unlock()
	select {
	case <-runner.executed:
		t.Fatal("one fresh BBO leg triggered execution")
	default:
	}

	connectionB.reads <- []byte("100.3,100.4")
	waitForFreshBBO(t, market, keyB)
	scheduler.runOnce(ctx)
	defer scheduler.closeAll()
	select {
	case <-runner.executed:
	case <-time.After(time.Second):
		t.Fatal("two fresh BBO legs did not trigger execution")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claims != 1 {
		t.Fatalf("two-leg claims=%d", store.claims)
	}
}

func TestArbitrageSchedulerBlocksImmediatelyDuringLegReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keyA, _ := marketdata.NewKey("gate", "perpetual", "BEAT_USDT")
	keyB, _ := marketdata.NewKey("okx", "perpetual", "BEAT-USDT-SWAP")
	firstA := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	secondA := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connectionB := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connector := &schedulerConnector{
		connections: map[marketdata.Key]*schedulerConnection{keyB: connectionB},
		sequences:   map[marketdata.Key][]*schedulerConnection{keyA: {firstA, secondA}},
	}
	parser := func(key marketdata.Key, payload []byte, received time.Time) (marketdata.BBO, bool, error) {
		parts := strings.Split(string(payload), ",")
		if len(parts) != 2 {
			return marketdata.BBO{}, false, fmt.Errorf("bad BBO fixture")
		}
		return marketdata.BBO{
			Key: key, BidPrice: parts[0], AskPrice: parts[1],
			BidQuantity: "10000", AskQuantity: "10000",
			VenueTimestamp: received, ReceiveTimestamp: received,
		}, true, nil
	}
	market, err := marketdata.New(marketdata.Options{
		Connector: connector,
		Parsers: map[string]marketdata.Parser{
			"gate": parser, "okx": parser,
		},
		StaleAfter: time.Hour, ReconnectInitial: time.Millisecond, ReconnectMax: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer market.Close()
	store := &dryRunStore{claim: false, item: ArbitrageCombination{
		ID: "967a504d-5ef8-42a9-ae64-c110f9738515", OwnerUsername: "admin",
		Status: "running", AskThresholdBps: "10", BidThresholdBps: "-10",
		TargetNotional: "10000", OrderNotional: "500", PositionNotional: "0",
		MarketDataStale: true,
		LegA: ArbitrageLeg{
			TradingAccountID: 1, Exchange: "gate",
			ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: "BEAT-USDT-SWAP",
		},
	}}
	scheduler := newTestArbitrageScheduler(
		store, market, nil, time.Millisecond, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	scheduler.runOnce(ctx)

	firstA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")
	waitForFreshBBO(t, market, keyA)
	waitForFreshBBO(t, market, keyB)
	scheduler.runOnce(ctx)
	store.mu.Lock()
	if store.claims != 1 {
		t.Fatalf("initial claims=%d", store.claims)
	}
	store.mu.Unlock()

	if err := firstA.Close(); err != nil {
		t.Fatal(err)
	}
	waitForMissingBBO(t, market, keyA)
	scheduler.runOnce(ctx)
	store.mu.Lock()
	if store.claims != 1 || !store.item.MarketDataStale {
		t.Fatalf("reconnecting state: claims=%d stale=%t", store.claims, store.item.MarketDataStale)
	}
	store.mu.Unlock()

	secondA.reads <- []byte("100,100.1")
	waitForFreshBBO(t, market, keyA)
	scheduler.runOnce(ctx)
	defer scheduler.closeAll()
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claims != 2 {
		t.Fatalf("recovered claims=%d", store.claims)
	}
}

func waitForFreshBBO(t *testing.T, market *marketdata.Manager, key marketdata.Key) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := market.Latest(key); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("BBO %v was not consumed", key)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForMissingBBO(t *testing.T, market *marketdata.Manager, key marketdata.Key) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := market.Latest(key); errors.Is(err, marketdata.ErrNoValue) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("BBO %v remained available after disconnect", key)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestArbitrageCapacityCountsSameAccountAndVenueOnce(t *testing.T) {
	scheduler := newTestArbitrageScheduler(
		nil, nil, nil, time.Second, time.Millisecond, time.Second,
		1, 1, 1, 1, false, nil,
	)
	item := ArbitrageCombination{
		LegA: ArbitrageLeg{TradingAccountID: 7, Exchange: "gate"},
		LegB: ArbitrageLeg{TradingAccountID: 7, Exchange: "gate"},
	}
	if !scheduler.acquireExecutionCapacity(item) {
		t.Fatal("first same-account execution capacity was rejected")
	}
	if scheduler.accountUse[7] != 1 || scheduler.venueUse["gate"] != 1 {
		t.Fatalf("capacity counts: accounts=%v venues=%v", scheduler.accountUse, scheduler.venueUse)
	}
	if scheduler.acquireExecutionCapacity(item) {
		t.Fatal("second same-account execution exceeded configured capacity")
	}
	scheduler.releaseExecutionCapacity(item)
	if len(scheduler.accountUse) != 0 || len(scheduler.venueUse) != 0 {
		t.Fatalf("capacity was not released: accounts=%v venues=%v", scheduler.accountUse, scheduler.venueUse)
	}
}

func TestArbitrageSchedulerDoesNotTriggerWithStaleLeg(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keyA, _ := marketdata.NewKey("bybit", "perpetual", "COTIUSDT")
	keyB, _ := marketdata.NewKey("bitget", "perpetual", "COTIUSDT")
	connectionA := &schedulerConnection{reads: make(chan []byte, 1), done: make(chan struct{})}
	connectionB := &schedulerConnection{reads: make(chan []byte, 1), done: make(chan struct{})}
	connector := &schedulerConnector{connections: map[marketdata.Key]*schedulerConnection{
		keyA: connectionA, keyB: connectionB,
	}}
	market, err := marketdata.New(marketdata.Options{
		Connector: connector,
		Parsers: map[string]marketdata.Parser{
			"bybit": func(key marketdata.Key, payload []byte, received time.Time) (marketdata.BBO, bool, error) {
				return marketdata.BBO{
					Key: key, BidPrice: "100", AskPrice: "100", BidQuantity: "10000", AskQuantity: "10000",
					ReceiveTimestamp: received,
				}, true, nil
			},
			"bitget": testStaleParser,
		},
		StaleAfter: time.Millisecond, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer market.Close()
	store := &dryRunStore{claim: true, item: ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61733", OwnerUsername: "admin",
		Status: "running", AskThresholdBps: "-100", BidThresholdBps: "100",
		TargetNotional: "10000", OrderNotional: "500", PositionNotional: "0",
		LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "bybit", ContractType: "perpetual", ExchangeSymbol: "COTIUSDT"},
		LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "bitget", ContractType: "perpetual", ExchangeSymbol: "COTIUSDT"},
	}}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("fresh")
	connectionB.reads <- []byte("stale")
	deadline := time.Now().Add(time.Second)
	for market.Stats().Updates < 2 {
		if time.Now().After(deadline) {
			t.Fatal("BBO fixtures were not consumed")
		}
		time.Sleep(time.Millisecond)
	}
	scheduler.runOnce(ctx)
	scheduler.closeAll()

	select {
	case <-runner.executed:
		t.Fatal("stale BBO triggered execution")
	default:
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claims != 0 {
		t.Fatalf("stale BBO claims=%d", store.claims)
	}
}

func testStaleParser(key marketdata.Key, _ []byte, received time.Time) (marketdata.BBO, bool, error) {
	return marketdata.BBO{
		Key: key, BidPrice: "101", AskPrice: "101", BidQuantity: "10000", AskQuantity: "10000",
		ReceiveTimestamp: received.Add(-time.Second),
	}, true, nil
}

func TestArbitrageSchedulerSkipsAskAtPositiveCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keyA, _ := marketdata.NewKey("bybit", "perpetual", "COTIUSDT")
	keyB, _ := marketdata.NewKey("bitget", "perpetual", "COTIUSDT")
	connectionA := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connectionB := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connector := &schedulerConnector{connections: map[marketdata.Key]*schedulerConnection{
		keyA: connectionA, keyB: connectionB,
	}}
	parser := func(key marketdata.Key, payload []byte, received time.Time) (marketdata.BBO, bool, error) {
		parts := strings.Split(string(payload), ",")
		return marketdata.BBO{
			Key: key, BidPrice: parts[0], AskPrice: parts[1],
			BidQuantity: "10000", AskQuantity: "10000",
			VenueTimestamp: received, ReceiveTimestamp: received,
		}, true, nil
	}
	market, err := marketdata.New(marketdata.Options{
		Connector:  connector,
		Parsers:    map[string]marketdata.Parser{"bybit": parser, "bitget": parser},
		StaleAfter: time.Second, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer market.Close()
	store := &dryRunStore{claim: true, item: ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61736", OwnerUsername: "admin",
		Status: "running", AskThresholdBps: "10", BidThresholdBps: "-10",
		TargetNotional: "10000", OrderNotional: "500", PositionNotional: "10000",
		LegABasePosition: "100", LegBBasePosition: "-100",
		LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "bybit", ContractType: "perpetual", ExchangeSymbol: "COTIUSDT"},
		LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "bitget", ContractType: "perpetual", ExchangeSymbol: "COTIUSDT"},
	}}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")
	deadline := time.Now().Add(time.Second)
	for {
		if _, errA := market.Latest(keyA); errA == nil {
			if _, errB := market.Latest(keyB); errB == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("BBO fixtures were not consumed")
		}
		time.Sleep(time.Millisecond)
	}
	scheduler.runOnce(ctx)
	scheduler.closeAll()
	select {
	case <-runner.executed:
		t.Fatal("capped ask position triggered execution")
	default:
	}
	if store.claims != 0 {
		t.Fatalf("capped claims=%d", store.claims)
	}
}

func TestArbitrageSchedulerOverTargetOpenDoesNotMaskClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	market, connectionA, connectionB := newEventSchedulerMarket(t)
	item := eventSchedulerCombination()
	item.TargetNotional = "500"
	item.OrderNotional = "20"
	item.LegABasePosition = "6.2"
	item.LegBBasePosition = "-6.2"
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("99,101")
	keyA, _ := marketdata.NewKey("bybit", "perpetual", "COTIUSDT")
	keyB, _ := marketdata.NewKey("bitget", "perpetual", "COTIUSDT")
	waitForFreshBBO(t, market, keyA)
	waitForFreshBBO(t, market, keyB)
	scheduler.runOnce(ctx)
	defer scheduler.closeAll()
	select {
	case execution := <-runner.executed:
		if execution.Direction != "bid" || execution.PositionEffect != "close" ||
			!execution.ReduceOnly || parsePositiveDecimal(execution.RequestedNotional).IsZero() {
			t.Fatalf("execution=%+v", execution)
		}
	case <-time.After(time.Second):
		t.Fatal("over-target close was not executed")
	}
}

func TestArbitrageSchedulerClosingCreatesSequentialReduceOnlyCloses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	market, connectionA, connectionB := newEventSchedulerMarket(t)
	item := eventSchedulerCombination()
	item.Status = "closing"
	item.RuntimeState = "closing"
	item.RunMode = "one_shot"
	item.OneShotPhase = "waiting_exit"
	item.LegABasePosition = "5"
	item.LegBBasePosition = "-5"
	item.PositionNotional = "500"
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 2)}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("99,101")
	keyA, _ := marketdata.NewKey("bybit", "perpetual", "COTIUSDT")
	keyB, _ := marketdata.NewKey("bitget", "perpetual", "COTIUSDT")
	waitForFreshBBO(t, market, keyA)
	waitForFreshBBO(t, market, keyB)
	scheduler.runOnce(ctx)
	select {
	case execution := <-runner.executed:
		if execution.PositionEffect != "close" || !execution.ReduceOnly {
			t.Fatalf("first close=%+v", execution)
		}
	case <-time.After(time.Second):
		t.Fatal("first closing flatten was not executed")
	}
	time.Sleep(50 * time.Millisecond)
	scheduler.runOnce(ctx)
	select {
	case execution := <-runner.executed:
		if execution.PositionEffect != "close" || !execution.ReduceOnly {
			t.Fatalf("second close=%+v", execution)
		}
	case <-time.After(time.Second):
		t.Fatal("second closing flatten was not executed")
	}
	scheduler.closeAll()
	if store.claims < 2 {
		t.Fatalf("expected continuous close claims, got %d", store.claims)
	}
}

func TestArbitrageSchedulerCompletionContinuesClosingDrain(t *testing.T) {
	market, connectionA, connectionB := newEventSchedulerMarket(t)
	item := eventSchedulerCombination()
	item.Status = "closing"
	item.RuntimeState = "closing"
	item.ExecutionMode = "simultaneous_market"
	item.LegABasePosition = "5"
	item.LegBBasePosition = "-5"
	store := &dryRunStore{claim: true, claimLimit: 2, item: item}
	runner := &schedulerRunner{
		executed: make(chan ArbitrageExecution, 2),
		executeHook: func() {
			store.mu.Lock()
			store.item.LegABasePosition = "4"
			store.item.LegBBasePosition = "-4"
			store.item.Version++
			store.mu.Unlock()
		},
	}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Hour, time.Millisecond, time.Hour,
		10, 2, 2, 2, false, nil,
	)
	runSchedulerForTest(t, scheduler)
	waitForSchedulerCondition(t, func() bool {
		return scheduler.Stats().ActiveCombinations == 1
	}, "combination runtime was not started")
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("99,101")

	for index := 0; index < 2; index++ {
		select {
		case execution := <-runner.executed:
			if execution.PositionEffect != "close" || !execution.ReduceOnly {
				t.Fatalf("close %d=%+v", index+1, execution)
			}
		case <-time.After(time.Second):
			t.Fatalf("close %d was not executed", index+1)
		}
	}
	if store.claims < 2 {
		t.Fatalf("execution completion did not continue drain: claims=%d", store.claims)
	}
}

func TestArbitrageSchedulerFlattenUsesOwnedDirectionAndVenueNetGate(t *testing.T) {
	item := eventSchedulerCombination()
	item.Status = "closing"
	item.RuntimeState = "closing"
	item.ExecutionMode = "simultaneous_market"
	item.LegABasePosition = "5"
	item.LegBBasePosition = "-5"
	item.LegAVenueBaselineBasePosition = "-100"
	item.LegBVenueBaselineBasePosition = "100"
	item.VenueBaselineCapturedAt = time.Now().UTC()
	store := &dryRunStore{claim: true, item: item}
	scheduler := newTestArbitrageScheduler(
		store, nil, &schedulerRunner{executed: make(chan ArbitrageExecution, 1)},
		time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := lastClipCloseBBO()
	bbo.ReceiveTimestamp = time.Now().UTC()
	px := decimal.RequireFromString("0.214")

	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("manual intervention must keep runtime active")
	}
	if runtime.combination.RuntimeState != "manual_intervention" ||
		store.claims != 0 || len(store.eventTypes) != 1 {
		t.Fatalf("combination=%+v claims=%d events=%v",
			runtime.combination, store.claims, store.eventTypes)
	}
	scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	)
	if store.claims != 0 || len(store.eventTypes) != 1 {
		t.Fatalf("manual intervention repeated work: claims=%d events=%v",
			store.claims, store.eventTypes)
	}
}

func ownedFlattenCombo(legA, legB string) ArbitrageCombination {
	item := lastClipCloseCombination("bid")
	item.LegABasePosition = legA
	item.LegBBasePosition = legB
	item.CarryBaseQuantity = parseDecimal(legA).Add(parseDecimal(legB)).String()
	item.VenueBaselineCapturedAt = time.Now().UTC()
	item.LegAVenueBaselineBasePosition = "0"
	item.LegBVenueBaselineBasePosition = "0"
	item.LegAVenueBasePosition = legA
	item.LegBVenueBasePosition = legB
	item.LegAPositionDifference = "0"
	item.LegBPositionDifference = "0"
	item.LastPositionReconciledAt = time.Now().UTC()
	return item
}

func flattenCloseBBO() marketdata.BBO {
	bbo := lastClipCloseBBO()
	bbo.ReceiveTimestamp = time.Now().UTC()
	return bbo
}

func circuitTinyOwnedCombo() ArbitrageCombination {
	item := ownedFlattenCombo("0", "-10")
	item.CircuitOpen = true
	item.RuntimeState = "closing"
	return item
}

func dryRunActiveLookups(store *dryRunStore) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.activeLookups
}

func TestArbitrageSchedulerFlattenTinyUnpairedClosesWithoutClaim(t *testing.T) {
	item := ownedFlattenCombo("0", "-10")
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{closed: make(chan ArbitrageCombination, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if !scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("tiny unpaired should close combination")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d events=%v", store.claims, store.eventTypes)
	}
	select {
	case <-runner.closed:
	case <-time.After(time.Second):
		t.Fatal("tiny unpaired did not call CloseCombination")
	}
}

func TestArbitrageSchedulerFlattenMinusFivePlusTenTinyClosesWithoutClaim(t *testing.T) {
	item := ownedFlattenCombo("-5", "10")
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{closed: make(chan ArbitrageCombination, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if !scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("paired tiny leftover should close combination")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d events=%v", store.claims, store.eventTypes)
	}
	select {
	case <-runner.closed:
	case <-time.After(time.Second):
		t.Fatal("paired tiny leftover did not call CloseCombination")
	}
}

func TestArbitrageSchedulerFlattenPairedTinyUnequalClosesWithoutClaim(t *testing.T) {
	item := ownedFlattenCombo("0.5", "-0.57")
	item.CarryBaseQuantity = "-0.07"
	item.LegBPositionDifference = "3e-16"
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{
		executed: make(chan ArbitrageExecution, 1),
		closed:   make(chan ArbitrageCombination, 1),
	}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	bbo.BidPrice = "25"
	bbo.AskPrice = "25"
	px := decimal.RequireFromString("25")
	if !scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("paired-but-unequal tiny should close combination")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d events=%v", store.claims, store.eventTypes)
	}
	select {
	case <-runner.closed:
	case <-time.After(time.Second):
		t.Fatal("paired-but-unequal tiny did not call CloseCombination")
	}
	select {
	case execution := <-runner.executed:
		t.Fatalf("claimed sized execution=%+v", execution)
	default:
	}
}

func TestArbitrageSchedulerFlattenPairedTinyCloseFailedKeyStillCloses(t *testing.T) {
	item := ownedFlattenCombo("0.5", "-0.57")
	item.CarryBaseQuantity = "-0.07"
	item.LegBPositionDifference = "3e-16"
	item.LastFailureKey = "close_failed"
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{closed: make(chan ArbitrageCombination, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	bbo.BidPrice = "25"
	bbo.AskPrice = "25"
	px := decimal.RequireFromString("25")
	if !scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("close_failed must not block a valid paired tiny")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d", store.claims)
	}
	select {
	case <-runner.closed:
	case <-time.After(time.Second):
		t.Fatal("close_failed paired tiny did not call CloseCombination")
	}
}

func TestArbitrageSchedulerFlattenPairedTinyResumesActiveExecution(t *testing.T) {
	item := ownedFlattenCombo("0.5", "-0.57")
	item.CarryBaseQuantity = "-0.07"
	item.LegBPositionDifference = "3e-16"
	execution := ArbitrageExecution{
		ID: "existing-paired-tiny", CombinationID: item.ID,
		Status: "hedging", PositionEffect: "close", ReduceOnly: true,
	}
	store := &dryRunStore{claim: true, item: item, activeExecution: &execution}
	runner := &schedulerRunner{
		executed: make(chan ArbitrageExecution, 1),
		closed:   make(chan ArbitrageCombination, 1),
	}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	bbo.BidPrice = "25"
	bbo.AskPrice = "25"
	px := decimal.RequireFromString("25")
	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("active execution should keep runtime")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d", store.claims)
	}
	select {
	case resumed := <-runner.executed:
		if resumed.ID != execution.ID {
			t.Fatalf("resumed=%+v", resumed)
		}
	case <-time.After(time.Second):
		t.Fatal("paired tiny did not resume active execution")
	}
	select {
	case closed := <-runner.closed:
		t.Fatalf("closed while active execution=%+v", closed)
	default:
	}
}

func TestArbitrageSchedulerFlattenNonTinyPairedStillClaimsSized(t *testing.T) {
	item := ownedFlattenCombo("100", "-100")
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	bbo.BidPrice = "1"
	bbo.AskPrice = "1"
	px := decimal.RequireFromString("1")
	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("non-tiny paired leftover should claim sized execution")
	}
	if store.claims != 1 {
		t.Fatalf("claims=%d events=%v", store.claims, store.eventTypes)
	}
	select {
	case execution := <-runner.executed:
		if execution.PositionEffect != "close" || !execution.ReduceOnly {
			t.Fatalf("execution=%+v", execution)
		}
	case <-time.After(time.Second):
		t.Fatal("non-tiny paired leftover did not start sized execution")
	}
}

func TestArbitrageSchedulerFlattenSameSignTinyClosesBothLegs(t *testing.T) {
	item := ownedFlattenCombo("5", "10")
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{closed: make(chan ArbitrageCombination, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if !scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("same-sign tiny should close combination")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d", store.claims)
	}
	select {
	case <-runner.closed:
	case <-time.After(time.Second):
		t.Fatal("same-sign tiny did not call CloseCombination")
	}
}

func TestArbitrageSchedulerFlattenRecoversUnpairedTinyManualReason(t *testing.T) {
	item := ownedFlattenCombo("0", "-10")
	item.RuntimeState = "manual_intervention"
	item.ErrorMessage = arbitrageUnpairedLegsManualReason
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{closed: make(chan ArbitrageCombination, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if !scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("recoverable unpaired tiny should close combination")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d", store.claims)
	}
	select {
	case <-runner.closed:
	case <-time.After(time.Second):
		t.Fatal("recovered unpaired tiny did not close")
	}
}

func TestArbitrageSchedulerFlattenDoesNotRecoverOtherManualReason(t *testing.T) {
	item := ownedFlattenCombo("0", "-10")
	item.RuntimeState = "manual_intervention"
	item.ErrorMessage = "venue net position cannot reduce the combination-owned position"
	store := &dryRunStore{claim: true, item: item}
	scheduler := newTestArbitrageScheduler(
		store, nil, &schedulerRunner{executed: make(chan ArbitrageExecution, 1)},
		time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("other manual intervention must stay blocked")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d", store.claims)
	}
}

func TestArbitrageSchedulerFlattenPairedCloseStillClaimsSized(t *testing.T) {
	item := lastClipCloseCombination("bid")
	item.VenueBaselineCapturedAt = time.Now().UTC()
	item.LegAVenueBaselineBasePosition = "0"
	item.LegBVenueBaselineBasePosition = "0"
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("paired flatten should keep runtime")
	}
	if store.claims != 1 {
		t.Fatalf("claims=%d events=%v", store.claims, store.eventTypes)
	}
}

func TestArbitrageSchedulerFlattenLargeUnpairedStaysManual(t *testing.T) {
	item := ownedFlattenCombo("0", "-10")
	store := &dryRunStore{claim: true, item: item}
	scheduler := newTestArbitrageScheduler(
		store, nil, &schedulerRunner{executed: make(chan ArbitrageExecution, 1)},
		time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	bbo.BidPrice = "2.1"
	bbo.AskPrice = "2.1"
	px := decimal.RequireFromString("2.1")
	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("large unpaired residual should stay scheduled")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d", store.claims)
	}
	if runtime.combination.RuntimeState != "manual_intervention" ||
		runtime.combination.ErrorMessage != arbitrageUnpairedLegsManualReason {
		t.Fatalf("combination=%+v", runtime.combination)
	}
}

func TestArbitrageSchedulerFlattenDifferenceStaysManual(t *testing.T) {
	item := ownedFlattenCombo("0", "-10")
	item.LegBPositionDifference = "1"
	store := &dryRunStore{claim: true, item: item}
	scheduler := newTestArbitrageScheduler(
		store, nil, &schedulerRunner{executed: make(chan ArbitrageExecution, 1)},
		time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("difference residual should stay scheduled")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d", store.claims)
	}
	if runtime.combination.RuntimeState != "manual_intervention" {
		t.Fatalf("combination=%+v", runtime.combination)
	}
}

func TestArbitrageSchedulerClosingCircuitTinyOwnedCloses(t *testing.T) {
	item := circuitTinyOwnedCombo()
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{
		executed: make(chan ArbitrageExecution, 1),
		closed:   make(chan ArbitrageCombination, 1),
	}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if !scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("circuit tiny owned should close combination")
	}
	if store.claims != 0 || dryRunActiveLookups(store) < 1 {
		t.Fatalf("claims=%d lookups=%d", store.claims, dryRunActiveLookups(store))
	}
	select {
	case <-runner.closed:
	case <-time.After(time.Second):
		t.Fatal("circuit tiny owned did not call CloseCombination")
	}
	select {
	case execution := <-runner.executed:
		t.Fatalf("claimed sized execution=%+v", execution)
	default:
	}
}

func TestArbitrageSchedulerClosingCircuitTinyOwnedResumesActiveExecution(t *testing.T) {
	item := circuitTinyOwnedCombo()
	execution := ArbitrageExecution{
		ID: "existing-circuit-flatten", CombinationID: item.ID,
		Status: "hedging", PositionEffect: "close", ReduceOnly: true,
	}
	store := &dryRunStore{claim: true, item: item, activeExecution: &execution}
	runner := &schedulerRunner{
		executed: make(chan ArbitrageExecution, 1),
		closed:   make(chan ArbitrageCombination, 1),
	}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("active execution should keep runtime")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d", store.claims)
	}
	select {
	case resumed := <-runner.executed:
		if resumed.ID != execution.ID {
			t.Fatalf("resumed=%+v", resumed)
		}
	case <-time.After(time.Second):
		t.Fatal("circuit tiny owned did not resume active execution")
	}
	select {
	case closed := <-runner.closed:
		t.Fatalf("closed while active execution=%+v", closed)
	default:
	}
}

func TestArbitrageSchedulerClosingCircuitLargeOwnedStaysBlocked(t *testing.T) {
	item := circuitTinyOwnedCombo()
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{
		executed: make(chan ArbitrageExecution, 1),
		closed:   make(chan ArbitrageCombination, 1),
	}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	bbo.BidPrice = "2.1"
	bbo.AskPrice = "2.1"
	px := decimal.RequireFromString("2.1")
	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("large circuit residual should stay blocked")
	}
	if store.claims != 0 || runtime.combination.CircuitOpen != true ||
		runtime.combination.RuntimeState != "closing" {
		t.Fatalf("claims=%d combo=%+v", store.claims, runtime.combination)
	}
	select {
	case closed := <-runner.closed:
		t.Fatalf("closed large circuit residual=%+v", closed)
	default:
	}
}

func TestArbitrageSchedulerClosingCircuitUnsafeTinyDoesNotBypass(t *testing.T) {
	px := decimal.RequireFromString("0.214")
	fresh := flattenCloseBBO()
	staleB := fresh
	staleB.ReceiveTimestamp = time.Now().UTC().Add(-time.Minute)
	tests := []struct {
		name string
		item ArbitrageCombination
		bboA marketdata.BBO
		bboB marketdata.BBO
	}{
		{
			name: "position uncertain",
			item: func() ArbitrageCombination {
				item := circuitTinyOwnedCombo()
				item.PositionUncertain = true
				return item
			}(),
			bboA: fresh, bboB: fresh,
		},
		{
			name: "nonzero difference",
			item: func() ArbitrageCombination {
				item := circuitTinyOwnedCombo()
				item.LegBPositionDifference = "1"
				return item
			}(),
			bboA: fresh, bboB: fresh,
		},
		{
			name: "opposite venue",
			item: func() ArbitrageCombination {
				item := circuitTinyOwnedCombo()
				item.LegBVenueBasePosition = "10"
				return item
			}(),
			bboA: fresh, bboB: fresh,
		},
		{
			name: "short venue",
			item: func() ArbitrageCombination {
				item := circuitTinyOwnedCombo()
				item.LegBVenueBasePosition = "-5"
				return item
			}(),
			bboA: fresh, bboB: fresh,
		},
		{
			name: "missing baseline",
			item: func() ArbitrageCombination {
				item := circuitTinyOwnedCombo()
				item.VenueBaselineCapturedAt = time.Time{}
				return item
			}(),
			bboA: fresh, bboB: fresh,
		},
		{
			name: "stale snapshot",
			item: func() ArbitrageCombination {
				item := circuitTinyOwnedCombo()
				item.LastPositionReconciledAt = time.Now().UTC().Add(-3 * time.Minute)
				return item
			}(),
			bboA: fresh, bboB: fresh,
		},
		{
			name: "stale nonzero bbo",
			item: circuitTinyOwnedCombo(),
			bboA: fresh, bboB: staleB,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &dryRunStore{claim: true, item: test.item}
			runner := &schedulerRunner{closed: make(chan ArbitrageCombination, 1)}
			scheduler := newTestArbitrageScheduler(
				store, nil, runner, time.Second, time.Millisecond, time.Second,
				10, 2, 2, 2, false, nil,
			)
			runtime := &arbitrageRuntime{combination: test.item}
			if scheduler.flattenOrFinalizeClosing(
				context.Background(), runtime, test.bboA, test.bboB, px, px, px, px,
			) {
				t.Fatal("unsafe circuit residual bypassed")
			}
			if store.claims != 0 {
				t.Fatalf("claims=%d", store.claims)
			}
			select {
			case closed := <-runner.closed:
				t.Fatalf("closed=%+v", closed)
			default:
			}
		})
	}
}

func TestArbitrageSchedulerClosingCircuitPairedTinyCloses(t *testing.T) {
	item := ownedFlattenCombo("5", "-5")
	item.CircuitOpen = true
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{
		executed: make(chan ArbitrageExecution, 1),
		closed:   make(chan ArbitrageCombination, 1),
	}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if !scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("circuit paired tiny should close combination")
	}
	if store.claims != 0 || !runtime.combination.CircuitOpen {
		t.Fatalf("claims=%d combo=%+v", store.claims, runtime.combination)
	}
	select {
	case <-runner.closed:
	case <-time.After(time.Second):
		t.Fatal("circuit paired tiny did not call CloseCombination")
	}
	select {
	case execution := <-runner.executed:
		t.Fatalf("claimed sized execution=%+v", execution)
	default:
	}
}

func TestArbitrageSchedulerClosingBackoffSkipsActiveLookup(t *testing.T) {
	item := ownedFlattenCombo("5", "-5")
	item.NextRetryAt = time.Now().UTC().Add(time.Minute)
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{
		executed: make(chan ArbitrageExecution, 1),
		closed:   make(chan ArbitrageCombination, 1),
	}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("backoff should skip flatten work")
	}
	if store.claims != 0 || dryRunActiveLookups(store) != 0 {
		t.Fatalf("claims=%d lookups=%d", store.claims, dryRunActiveLookups(store))
	}
	select {
	case closed := <-runner.closed:
		t.Fatalf("closed during backoff=%+v", closed)
	default:
	}
}

func TestArbitrageSchedulerEvaluateRecoversUnpairedTinyClose(t *testing.T) {
	item := ownedFlattenCombo("0", "-10")
	item.RuntimeState = "manual_intervention"
	item.ErrorMessage = arbitrageUnpairedLegsManualReason
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{closed: make(chan ArbitrageCombination, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	marketA := newFixedPriceMarket(t, "bybit", "USELESSUSDT", "0.214", "0.214")
	marketB := newFixedPriceMarket(t, "okx", "USELESSUSDT", "0.214", "0.214")
	keyA, err := marketdata.NewKey("bybit", "perpetual", "USELESSUSDT")
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := marketdata.NewKey("okx", "perpetual", "USELESSUSDT")
	if err != nil {
		t.Fatal(err)
	}
	subA, err := marketA.Subscribe(context.Background(), keyA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subA.Close() })
	subB, err := marketB.Subscribe(context.Background(), keyB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subB.Close() })
	deadline := time.Now().Add(time.Second)
	for {
		_, errA := subA.Latest()
		_, errB := subB.Latest()
		if errA == nil && errB == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("leg subscriptions were not ready")
		}
		time.Sleep(time.Millisecond)
	}
	runtime := &arbitrageRuntime{combination: item, legA: subA, legB: subB}
	if !scheduler.evaluate(context.Background(), runtime, "bbo_event") {
		t.Fatal("recovered unpaired tiny should close combination")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d", store.claims)
	}
	select {
	case <-runner.closed:
	case <-time.After(time.Second):
		t.Fatal("evaluate did not close combination")
	}
}

func TestArbitrageSchedulerAlreadyClosedDoesNotRetrigger(t *testing.T) {
	item := ownedFlattenCombo("0", "-10")
	item.Status = "closed"
	item.RuntimeState = "monitoring"
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{closed: make(chan ArbitrageCombination, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	if !scheduler.evaluate(context.Background(), runtime, "control") {
		t.Fatal("closed combination should stop runtime")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d", store.claims)
	}
	select {
	case <-runner.closed:
		t.Fatal("closed combination must not call CloseCombination")
	default:
	}
}

type oneShotExitStore struct {
	*dryRunStore
}

func (s *oneShotExitStore) MarkOneShotExited(
	context.Context, string, int64,
) (ArbitrageCombination, bool, error) {
	return s.item, false, nil
}

func TestArbitrageSchedulerOneShotExitedNotAppliedKeepsRunning(t *testing.T) {
	item := ownedFlattenCombo("0", "0")
	item.Status = "running"
	item.RunMode = "one_shot"
	item.OneShotPhase = "exiting"
	item.CarryBaseQuantity = "0"
	store := &oneShotExitStore{dryRunStore: &dryRunStore{item: item}}
	scheduler := newTestArbitrageScheduler(
		store, nil, &schedulerRunner{executed: make(chan ArbitrageExecution, 1)},
		time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	if scheduler.finalizeFlattening(context.Background(), runtime) {
		t.Fatal("one-shot exit not applied should keep runtime")
	}
	if runtime.combination.Status != "running" ||
		runtime.combination.OneShotPhase != "exiting" {
		t.Fatalf("combination=%+v", runtime.combination)
	}
}

func TestArbitrageSchedulerManualInterventionOnlyResumesActiveExecution(t *testing.T) {
	item := eventSchedulerCombination()
	item.Status = "running"
	item.RunMode = "one_shot"
	item.OneShotPhase = "exiting"
	item.RuntimeState = "manual_intervention"
	execution := ArbitrageExecution{
		ID: "existing-flatten", CombinationID: item.ID,
		Status: "reconciling", PositionEffect: "close", ReduceOnly: true,
	}
	store := &dryRunStore{item: item, activeExecution: &execution}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := lastClipCloseBBO()
	if !scheduler.resumeActiveForIntervention(
		context.Background(), runtime, bbo, bbo,
	) {
		t.Fatal("existing execution was not resumed")
	}
	select {
	case resumed := <-runner.executed:
		if resumed.ID != execution.ID {
			t.Fatalf("resumed=%+v", resumed)
		}
	case <-time.After(time.Second):
		t.Fatal("manual intervention did not resume existing execution")
	}
	if store.claims != 0 {
		t.Fatalf("manual intervention claimed new executions: %d", store.claims)
	}
}

func TestArbitrageSchedulerManualInterventionDoesNotResumeFailedLastClose(t *testing.T) {
	item := eventSchedulerCombination()
	item.Status = "running"
	item.RunMode = "one_shot"
	item.OneShotPhase = "exiting"
	item.RuntimeState = "manual_intervention"
	item.CircuitOpen = true
	execution := ArbitrageExecution{
		ID: "failed-last-close", CombinationID: item.ID,
		Status: "failed", PositionEffect: "close", ReduceOnly: true, LastCloseClip: true,
	}
	store := &dryRunStore{item: item, activeExecution: &execution, eventTypes: []string{"last_close_clip_unbalanced"}}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 2)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := lastClipCloseBBO()
	for i := 0; i < 2; i++ {
		if scheduler.resumeActiveForIntervention(
			context.Background(), runtime, bbo, bbo,
		) {
			t.Fatalf("failed last-close resumed on pass %d", i+1)
		}
	}
	select {
	case resumed := <-runner.executed:
		t.Fatalf("resumed=%+v", resumed)
	default:
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d", store.claims)
	}
	unbalanced := 0
	for _, eventType := range store.eventTypes {
		if eventType == "last_close_clip_unbalanced" {
			unbalanced++
		}
		if eventType == "maker_accepted" {
			t.Fatalf("events=%v", store.eventTypes)
		}
	}
	if unbalanced != 1 {
		t.Fatalf("events=%v", store.eventTypes)
	}
}

func TestArbitrageSchedulerOneShotExitingFinalizesOnlyExactFlat(t *testing.T) {
	item := eventSchedulerCombination()
	item.Status = "running"
	item.RunMode = "one_shot"
	item.OneShotPhase = "exiting"
	item.LegABasePosition = "0"
	item.LegBBasePosition = "0"
	item.CarryBaseQuantity = "0"
	store := &dryRunStore{item: item}
	scheduler := &ArbitrageScheduler{store: store, logger: slog.Default()}
	runtime := &arbitrageRuntime{combination: item}
	bbo := lastClipCloseBBO()

	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo,
		decimal.NewFromInt(1), decimal.NewFromInt(1),
		decimal.NewFromInt(1), decimal.NewFromInt(1),
	) {
		t.Fatal("one-shot exited must remain running")
	}
	if runtime.combination.Status != "running" ||
		runtime.combination.OneShotPhase != "exited" {
		t.Fatalf("combination=%+v", runtime.combination)
	}
}

func TestArbitrageSchedulerOneShotExitingDoesNotTinyFlatten(t *testing.T) {
	item := ownedFlattenCombo("0", "-10")
	item.Status = "running"
	item.RunMode = "one_shot"
	item.OneShotPhase = "exiting"
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{closed: make(chan ArbitrageCombination, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("one-shot exiting leftover must not close combination")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d", store.claims)
	}
	select {
	case <-runner.closed:
		t.Fatal("one-shot exiting must not call CloseCombination")
	default:
	}
}

func TestArbitrageSchedulerOneShotOpensWithoutSpreadThreshold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	market, connectionA, connectionB := newEventSchedulerMarket(t)
	item := eventSchedulerCombination()
	item.RunMode = "one_shot"
	item.EntryDirection = "ask"
	item.AskThresholdBps = "1000"
	item.BidThresholdBps = "-1000"
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("99,101")
	keyA, _ := marketdata.NewKey("bybit", "perpetual", "COTIUSDT")
	keyB, _ := marketdata.NewKey("bitget", "perpetual", "COTIUSDT")
	waitForFreshBBO(t, market, keyA)
	waitForFreshBBO(t, market, keyB)
	scheduler.runOnce(ctx)
	defer scheduler.closeAll()
	select {
	case execution := <-runner.executed:
		if execution.Direction != "ask" || execution.PositionEffect != "open" ||
			execution.ReduceOnly || parsePositiveDecimal(execution.RequestedNotional).IsZero() {
			t.Fatalf("execution=%+v", execution)
		}
	case <-time.After(time.Second):
		t.Fatal("one-shot open was not claimed")
	}
}

func TestArbitrageSchedulerClaimsBaselineOnlyBidWithExecutableBase(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	market, connectionA, connectionB := newEventSchedulerMarket(t)
	item := eventSchedulerCombination()
	item.LegA.InstrumentID = 101
	item.LegB.InstrumentID = 202
	item.ExecutionMode = "simultaneous_market"
	item.AskThresholdBps = "1000"
	item.BidThresholdBps = "-50"
	item.OrderNotional = "500"
	item.VenueBaselineCapturedAt = time.Now().UTC()
	item.LegAVenueBaselineBasePosition = "100"
	item.LegBVenueBaselineBasePosition = "-100"
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	instrument := func(id int64) Instrument {
		return Instrument{
			ID: id, Exchange: "binance", ContractType: "perpetual",
			QuantityStep: "1", PriceTick: "0.1",
			MinQuantity: "1", MinNotional: "1",
			MinQuantityStatus:  exchange.ConstraintKnown,
			MaxQuantityStatus:  exchange.ConstraintNotApplicable,
			MinNotionalStatus:  exchange.ConstraintKnown,
			MarketQuantityStep: "1", MarketMinQuantity: "1",
			MarketMinNotional:        "1",
			MarketQuantityStepStatus: exchange.ConstraintKnown,
			MarketMinQuantityStatus:  exchange.ConstraintKnown,
			MarketMaxQuantityStatus:  exchange.ConstraintNotApplicable,
			MarketMinNotionalStatus:  exchange.ConstraintKnown,
		}
	}
	scheduler.ConfigureArbitrageInstruments(
		silentRiskCatalog{101: instrument(101), 202: instrument(202)},
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("99,99.1")
	keyA, _ := marketdata.NewKey("bybit", "perpetual", "COTIUSDT")
	keyB, _ := marketdata.NewKey("bitget", "perpetual", "COTIUSDT")
	waitForFreshBBO(t, market, keyA)
	waitForFreshBBO(t, market, keyB)
	scheduler.runOnce(ctx)
	defer scheduler.closeAll()
	select {
	case execution := <-runner.executed:
		if execution.Direction != "bid" || execution.PositionEffect != "close" ||
			!execution.ReduceOnly || parsePositiveDecimal(execution.TargetBaseQuantity).IsZero() {
			t.Fatalf("execution=%+v", execution)
		}
	case <-time.After(time.Second):
		t.Fatal("baseline-only bid was not executed")
	}
}

func TestArbitrageSchedulerMakerAClosesOnQuoteAskNotQuoteBid(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	market, connectionA, connectionB := newEventSchedulerMarket(t)
	item := eventSchedulerCombination()
	item.LegA.InstrumentID = 101
	item.LegB.InstrumentID = 202
	item.ExecutionMode = "maker_then_hedge"
	item.MakerLeg = "a"
	item.AskThresholdBps = "1000"
	item.BidThresholdBps = "-120"
	item.OrderNotional = "500"
	item.VenueBaselineCapturedAt = time.Now().UTC()
	item.LegAVenueBaselineBasePosition = "100"
	item.LegBVenueBaselineBasePosition = "-100"
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	instrument := func(id int64) Instrument {
		return Instrument{
			ID: id, Exchange: "binance", ContractType: "perpetual",
			QuantityStep: "1", PriceTick: "0.1",
			MinQuantity: "1", MinNotional: "1",
			MinQuantityStatus:  exchange.ConstraintKnown,
			MaxQuantityStatus:  exchange.ConstraintNotApplicable,
			MinNotionalStatus:  exchange.ConstraintKnown,
			MarketQuantityStep: "1", MarketMinQuantity: "1",
			MarketMinNotional:        "1",
			MarketQuantityStepStatus: exchange.ConstraintKnown,
			MarketMinQuantityStatus:  exchange.ConstraintKnown,
			MarketMaxQuantityStatus:  exchange.ConstraintNotApplicable,
			MarketMinNotionalStatus:  exchange.ConstraintKnown,
		}
	}
	scheduler.ConfigureArbitrageInstruments(
		silentRiskCatalog{101: instrument(101), 202: instrument(202)},
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("100,101")
	connectionB.reads <- []byte("99,99.5")
	keyA, _ := marketdata.NewKey("bybit", "perpetual", "COTIUSDT")
	keyB, _ := marketdata.NewKey("bitget", "perpetual", "COTIUSDT")
	waitForFreshBBO(t, market, keyA)
	waitForFreshBBO(t, market, keyB)
	quoteAsk, quoteBid, ok := calculateArbitrageSpreads(
		decimal.RequireFromString("100"),
		decimal.RequireFromString("101"),
		decimal.RequireFromString("99"),
		decimal.RequireFromString("99.5"),
	)
	if !ok {
		t.Fatal("expected valid quote spreads")
	}
	if quoteAsk.GreaterThan(decimal.RequireFromString("-120")) {
		t.Fatalf("quoteAsk=%s should hit bid threshold", quoteAsk)
	}
	if quoteBid.LessThanOrEqual(decimal.RequireFromString("-120")) {
		t.Fatalf("quoteBid=%s should miss bid threshold", quoteBid)
	}
	scheduler.runOnce(ctx)
	defer scheduler.closeAll()
	select {
	case execution := <-runner.executed:
		if execution.Direction != "bid" || execution.PositionEffect != "close" ||
			!execution.ReduceOnly {
			t.Fatalf("execution=%+v", execution)
		}
		if execution.TriggerAskSpread != quoteBid.String() ||
			execution.TriggerBidSpread != quoteAsk.String() {
			t.Fatalf("trigger spreads=%s/%s want executable %s/%s",
				execution.TriggerAskSpread, execution.TriggerBidSpread,
				quoteBid, quoteAsk)
		}
	case <-time.After(time.Second):
		t.Fatal("maker A bid close was not executed on quoteAsk")
	}
}

func TestArbitrageSchedulerValuationCache(t *testing.T) {
	scheduler := newTestArbitrageScheduler(
		nil, nil, nil,
		time.Second, time.Second, time.Second,
		1, 1, 1, 1, true, nil,
	)
	updatedAt := time.Now().UTC()
	scheduler.storeArbitrageValuation(
		"combo",
		decimal.RequireFromString("99"),
		decimal.RequireFromString("101"),
		decimal.RequireFromString("1"),
		decimal.RequireFromString("1.02"),
		updatedAt, updatedAt,
	)
	value, ok := scheduler.ArbitrageValuation("combo")
	if !ok || value.LegAMid != "100" || value.LegBMid != "1.01" {
		t.Fatalf("valuation=%+v ok=%v", value, ok)
	}
	scheduler.clearArbitrageValuation("combo")
	if _, ok := scheduler.ArbitrageValuation("combo"); ok {
		t.Fatal("valuation cache was not cleared")
	}
}

func TestArbitrageSchedulerSkipsDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keyA, _ := marketdata.NewKey("bybit", "perpetual", "COTIUSDT")
	keyB, _ := marketdata.NewKey("bitget", "perpetual", "COTIUSDT")
	connectionA := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connectionB := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connector := &schedulerConnector{connections: map[marketdata.Key]*schedulerConnection{
		keyA: connectionA, keyB: connectionB,
	}}
	parser := func(key marketdata.Key, payload []byte, received time.Time) (marketdata.BBO, bool, error) {
		parts := strings.Split(string(payload), ",")
		return marketdata.BBO{
			Key: key, BidPrice: parts[0], AskPrice: parts[1],
			BidQuantity: "10000", AskQuantity: "10000",
			VenueTimestamp: received, ReceiveTimestamp: received,
		}, true, nil
	}
	market, err := marketdata.New(marketdata.Options{
		Connector:  connector,
		Parsers:    map[string]marketdata.Parser{"bybit": parser, "bitget": parser},
		StaleAfter: time.Second, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer market.Close()
	store := &dryRunStore{claim: true, item: ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61737", OwnerUsername: "admin",
		Status: "running", AskThresholdBps: "10", BidThresholdBps: "-10",
		TargetNotional: "10000", OrderNotional: "500", PositionNotional: "0",
		NextRetryAt: time.Now().UTC().Add(time.Hour),
		LegA:        ArbitrageLeg{TradingAccountID: 1, Exchange: "bybit", ContractType: "perpetual", ExchangeSymbol: "COTIUSDT"},
		LegB:        ArbitrageLeg{TradingAccountID: 2, Exchange: "bitget", ContractType: "perpetual", ExchangeSymbol: "COTIUSDT"},
	}}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")
	deadline := time.Now().Add(time.Second)
	for {
		if _, errA := market.Latest(keyA); errA == nil {
			if _, errB := market.Latest(keyB); errB == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("BBO fixtures were not consumed")
		}
		time.Sleep(time.Millisecond)
	}
	scheduler.runOnce(ctx)
	scheduler.closeAll()
	if store.claims != 0 {
		t.Fatalf("backoff claims=%d", store.claims)
	}
}

func TestEventDrivenSchedulerCoalescesBurstUpdates(t *testing.T) {
	market, connectionA, connectionB := newEventSchedulerMarket(t)
	store := &dryRunStore{item: eventSchedulerCombination()}
	scheduler := newTestArbitrageScheduler(
		store, market, nil, time.Hour, 50*time.Millisecond, time.Hour,
		10, 2, 2, 2, true, nil,
	)
	runSchedulerForTest(t, scheduler)
	waitForSchedulerCondition(t, func() bool {
		return scheduler.Stats().ActiveCombinations == 1
	}, "combination runtime was not started")

	for index := 0; index < 16; index++ {
		connectionA.reads <- []byte("100,100.1")
		connectionB.reads <- []byte("100.3,100.4")
	}
	waitForSchedulerCondition(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.eventTypes) == 1
	}, "coalesced BBO burst did not evaluate")
	time.Sleep(100 * time.Millisecond)

	stats := scheduler.Stats()
	if stats.BBOWakeups <= stats.BBOEvaluations {
		t.Fatalf("wakeups=%d evaluations=%d", stats.BBOWakeups, stats.BBOEvaluations)
	}
	if stats.BBOEvaluations != 1 {
		t.Fatalf("burst evaluations=%d, want 1", stats.BBOEvaluations)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.snapshots != 1 {
		t.Fatalf("snapshot writes=%d, want 1", store.snapshots)
	}
}

func TestControlTickDoesNotRecomputePositionMetrics(t *testing.T) {
	market, connectionA, connectionB := newEventSchedulerMarket(t)
	store := &dryRunStore{item: eventSchedulerCombination()}
	scheduler := newTestArbitrageScheduler(
		store, market, nil, 20*time.Millisecond, 5*time.Millisecond, time.Hour,
		10, 2, 2, 2, true, nil,
	)
	runSchedulerForTest(t, scheduler)
	waitForSchedulerCondition(t, func() bool {
		return scheduler.Stats().ActiveCombinations == 1
	}, "combination runtime was not started")
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")
	waitForSchedulerCondition(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.snapshots >= 1
	}, "control tick did not write a market snapshot")
	time.Sleep(80 * time.Millisecond)
	if scheduler.Stats().ActiveCombinations != 1 {
		t.Fatal("scheduler stopped after control ticks without metrics recompute")
	}
}

func TestEventDrivenSchedulerUsesExactRetryTimer(t *testing.T) {
	market, connectionA, connectionB := newEventSchedulerMarket(t)
	item := eventSchedulerCombination()
	item.NextRetryAt = time.Now().UTC().Add(150 * time.Millisecond)
	store := &dryRunStore{item: item, claim: true, claimLimit: 1}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Hour, 20*time.Millisecond, time.Hour,
		10, 2, 2, 2, false, nil,
	)
	startedAt := time.Now()
	runSchedulerForTest(t, scheduler)
	waitForSchedulerCondition(t, func() bool {
		return scheduler.Stats().ActiveCombinations == 1
	}, "combination runtime was not started")
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")

	select {
	case <-runner.executed:
		if time.Since(startedAt) < 100*time.Millisecond {
			t.Fatal("execution resumed before next_retry_at")
		}
	case <-time.After(time.Second):
		t.Fatal("exact next_retry_at timer did not resume execution")
	}
	if scheduler.Stats().RetryWakeups == 0 {
		t.Fatal("retry wakeup was not recorded")
	}
}

func TestEventDrivenSchedulerRefreshesImmediatelyAfterExecution(t *testing.T) {
	market, connectionA, connectionB := newEventSchedulerMarket(t)
	store := &dryRunStore{
		item: eventSchedulerCombination(), claim: true, claimLimit: 1,
	}
	runner := &schedulerRunner{
		executed: make(chan ArbitrageExecution, 1),
		executeHook: func() {
			store.mu.Lock()
			store.item.Version++
			store.mu.Unlock()
		},
	}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Hour, 20*time.Millisecond, time.Hour,
		10, 2, 2, 2, false, nil,
	)
	runSchedulerForTest(t, scheduler)
	waitForSchedulerCondition(t, func() bool {
		return scheduler.Stats().ActiveCombinations == 1
	}, "combination runtime was not started")
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")

	select {
	case <-runner.executed:
	case <-time.After(time.Second):
		t.Fatal("execution did not run")
	}
	waitForSchedulerCondition(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.refreshes > 0
	}, "execution completion did not refresh combination state")
}

func TestControlTickRetriesActiveExecutionWithoutNewBBO(t *testing.T) {
	market, connectionA, connectionB := newEventSchedulerMarket(t)
	execution := ArbitrageExecution{
		ID: "execution-1", CombinationID: eventSchedulerCombination().ID,
		Status: "reconciling",
	}
	store := &dryRunStore{
		item: eventSchedulerCombination(), activeExecution: &execution,
	}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 2)}
	var runMu sync.Mutex
	runCount := 0
	runner.executeHook = func() {
		runMu.Lock()
		defer runMu.Unlock()
		runCount++
		if runCount == 2 {
			store.mu.Lock()
			store.activeExecution = nil
			store.mu.Unlock()
		}
	}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, 100*time.Millisecond, 20*time.Millisecond, time.Hour,
		10, 2, 2, 2, false, nil,
	)
	runSchedulerForTest(t, scheduler)
	waitForSchedulerCondition(t, func() bool {
		return scheduler.Stats().ActiveCombinations == 1
	}, "combination runtime was not started")
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")

	for index := 0; index < 2; index++ {
		select {
		case resumed := <-runner.executed:
			if resumed.ID != execution.ID {
				t.Fatalf("resumed=%+v", resumed)
			}
		case <-time.After(time.Second):
			t.Fatal("active execution recovery depended on a new BBO")
		}
	}
}

func TestEventDrivenSchedulerRecoversWithoutBBOOnControlTick(t *testing.T) {
	market, _, _ := newEventSchedulerMarket(t)
	execution := ArbitrageExecution{
		ID: "execution-1", CombinationID: eventSchedulerCombination().ID,
		Status: "reconciling",
	}
	store := &dryRunStore{
		item: eventSchedulerCombination(), activeExecution: &execution,
	}
	runner := &schedulerRunner{recovered: make(chan ArbitrageExecution, 1)}
	runner.recoverHook = func() {
		store.mu.Lock()
		store.activeExecution = nil
		store.mu.Unlock()
	}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, 20*time.Millisecond, 20*time.Millisecond, time.Hour,
		10, 2, 2, 2, false, nil,
	)
	runSchedulerForTest(t, scheduler)

	select {
	case recovered := <-runner.recovered:
		if recovered.ID != execution.ID {
			t.Fatalf("recovered=%+v", recovered)
		}
	case <-time.After(time.Second):
		t.Fatal("control tick did not recover active execution without BBO")
	}
	stats := scheduler.Stats()
	if stats.ControlWakeups == 0 || stats.StaleWakeups == 0 {
		t.Fatalf("control=%d stale=%d", stats.ControlWakeups, stats.StaleWakeups)
	}
	time.Sleep(60 * time.Millisecond)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.snapshots != 1 {
		t.Fatalf("stale snapshot writes=%d, want 1 within the snapshot interval", store.snapshots)
	}
}

func TestEventDrivenSchedulerClosesWithoutBBO(t *testing.T) {
	market, _, _ := newEventSchedulerMarket(t)
	item := eventSchedulerCombination()
	item.Status = "closing"
	item.RuntimeState = "closing"
	store := &dryRunStore{item: item}
	runner := &schedulerRunner{closed: make(chan ArbitrageCombination, 1)}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, 20*time.Millisecond, 20*time.Millisecond, time.Hour,
		10, 2, 2, 2, false, nil,
	)
	runSchedulerForTest(t, scheduler)

	select {
	case closed := <-runner.closed:
		if closed.ID != item.ID {
			t.Fatalf("closed=%+v", closed)
		}
	case <-time.After(time.Second):
		t.Fatal("closing combination depended on a BBO update")
	}
	waitForSchedulerCondition(t, func() bool {
		return scheduler.Stats().ActiveCombinations == 0
	}, "closed runtime was not removed")
}

func TestClosingWaitsForStaleRecoveryToFinish(t *testing.T) {
	market, _, _ := newEventSchedulerMarket(t)
	execution := ArbitrageExecution{
		ID: "execution-1", CombinationID: eventSchedulerCombination().ID,
		Status: "reconciling",
	}
	store := &dryRunStore{
		item: eventSchedulerCombination(), activeExecution: &execution,
	}
	runner := &blockingRecoveryRunner{
		started: make(chan struct{}), release: make(chan struct{}),
		closed: make(chan ArbitrageCombination, 1),
	}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, 20*time.Millisecond, 20*time.Millisecond, time.Hour,
		10, 2, 2, 2, false, nil,
	)
	runSchedulerForTest(t, scheduler)

	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("stale recovery did not start")
	}
	store.mu.Lock()
	store.item.Status = "closing"
	store.item.RuntimeState = "closing"
	store.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-runner.closed:
		t.Fatal("closing raced an in-flight stale recovery")
	default:
	}

	close(runner.release)
	select {
	case closed := <-runner.closed:
		if closed.ID != execution.CombinationID {
			t.Fatalf("closed=%+v", closed)
		}
	case <-time.After(time.Second):
		t.Fatal("closing did not resume after stale recovery finished")
	}
}

func TestEventDrivenSchedulerRenewsAndStopsAfterLeaseLoss(t *testing.T) {
	market, _, _ := newEventSchedulerMarket(t)
	renewed := false
	store := &dryRunStore{
		item: eventSchedulerCombination(), renewResult: &renewed,
	}
	scheduler := newTestArbitrageScheduler(
		store, market, nil, time.Hour, 20*time.Millisecond, 60*time.Millisecond,
		10, 2, 2, 2, false, nil,
	)
	runSchedulerForTest(t, scheduler)
	waitForSchedulerCondition(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.renewals > 0
	}, "lease was not renewed at lease/3")
	waitForSchedulerCondition(t, func() bool {
		return scheduler.Stats().ActiveCombinations == 0
	}, "runtime remained active after lease loss")
}

func TestLeaseLossDoesNotCancelInFlightExecution(t *testing.T) {
	market, connectionA, connectionB := newEventSchedulerMarket(t)
	renewed := false
	store := &dryRunStore{
		item: eventSchedulerCombination(), claim: true, claimLimit: 1,
		renewResult: &renewed,
	}
	runner := &blockingSchedulerRunner{
		started: make(chan struct{}), release: make(chan struct{}),
		canceled: make(chan struct{}),
	}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Hour, 20*time.Millisecond, 150*time.Millisecond,
		10, 2, 2, 2, false, nil,
	)
	runSchedulerForTest(t, scheduler)
	waitForSchedulerCondition(t, func() bool {
		return scheduler.Stats().ActiveCombinations == 1
	}, "combination runtime was not started")
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")

	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("execution did not start")
	}
	waitForSchedulerCondition(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.renewals > 0
	}, "lease loss was not observed")
	select {
	case <-runner.canceled:
		t.Fatal("runtime cancellation propagated into in-flight execution")
	default:
	}
	if scheduler.Stats().ActiveCombinations != 1 {
		t.Fatal("runtime was removed before its in-flight execution completed")
	}

	close(runner.release)
	waitForSchedulerCondition(t, func() bool {
		return scheduler.Stats().ActiveCombinations == 0
	}, "runtime was not removed after in-flight execution completed")
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.refreshes != 0 {
		t.Fatalf("lost-lease runtime refreshed after execution: %d", store.refreshes)
	}
}

func TestSchedulerShutdownWaitsForInFlightExecution(t *testing.T) {
	market, connectionA, connectionB := newEventSchedulerMarket(t)
	store := &dryRunStore{item: eventSchedulerCombination(), claim: true, claimLimit: 1}
	runner := &blockingSchedulerRunner{
		started: make(chan struct{}), release: make(chan struct{}),
		canceled: make(chan struct{}), ignoreCancel: true,
	}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Hour, 20*time.Millisecond, time.Hour,
		10, 2, 2, 2, false, nil,
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		scheduler.Run(ctx)
	}()
	var released sync.Once
	release := func() { released.Do(func() { close(runner.release) }) }
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("scheduler did not stop")
		}
	})
	waitForSchedulerCondition(t, func() bool {
		return scheduler.Stats().ActiveCombinations == 1
	}, "combination runtime was not started")
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("execution did not start")
	}

	cancel()
	select {
	case <-done:
		t.Fatal("scheduler stopped before in-flight execution finished")
	case <-time.After(80 * time.Millisecond):
	}
	if scheduler.Stats().ActiveCombinations != 1 {
		t.Fatal("runtime was removed before in-flight execution finished")
	}

	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not drain in-flight execution on shutdown")
	}
}

func TestEventDrivenSchedulerStopsWhenSubscriptionsClose(t *testing.T) {
	market, _, _ := newEventSchedulerMarket(t)
	store := &dryRunStore{item: eventSchedulerCombination()}
	scheduler := newTestArbitrageScheduler(
		store, market, nil, time.Hour, 20*time.Millisecond, time.Hour,
		10, 2, 2, 2, false, nil,
	)
	runSchedulerForTest(t, scheduler)
	waitForSchedulerCondition(t, func() bool {
		return scheduler.Stats().ActiveCombinations == 1
	}, "combination runtime was not started")

	if err := market.Close(); err != nil {
		t.Fatal(err)
	}
	waitForSchedulerCondition(t, func() bool {
		return scheduler.Stats().ActiveCombinations == 0
	}, "runtime did not stop after subscription channels closed")
	before := scheduler.Stats().BBOWakeups
	time.Sleep(50 * time.Millisecond)
	if after := scheduler.Stats().BBOWakeups; after != before {
		t.Fatalf("closed subscriptions caused busy loop: before=%d after=%d", before, after)
	}
}

func TestRetryTimerSkipsSentinels(t *testing.T) {
	now := time.Now().UTC()
	for _, value := range []time.Time{
		{},
		time.Unix(0, 0).UTC(),
		time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		now.Add(-time.Second),
	} {
		if delay, ok := retryTimerDelay(value, now); ok {
			t.Fatalf("sentinel %v scheduled with delay %v", value, delay)
		}
	}
	if delay, ok := retryTimerDelay(now.Add(time.Second), now); !ok || delay != time.Second {
		t.Fatalf("future retry delay=%v scheduled=%t", delay, ok)
	}
}

func newEventSchedulerMarket(
	t *testing.T,
) (*marketdata.Manager, *schedulerConnection, *schedulerConnection) {
	t.Helper()
	keyA, _ := marketdata.NewKey("bybit", "perpetual", "COTIUSDT")
	keyB, _ := marketdata.NewKey("bitget", "perpetual", "COTIUSDT")
	connectionA := &schedulerConnection{reads: make(chan []byte, 64), done: make(chan struct{})}
	connectionB := &schedulerConnection{reads: make(chan []byte, 64), done: make(chan struct{})}
	connector := &schedulerConnector{connections: map[marketdata.Key]*schedulerConnection{
		keyA: connectionA, keyB: connectionB,
	}}
	parser := func(
		key marketdata.Key, payload []byte, received time.Time,
	) (marketdata.BBO, bool, error) {
		parts := strings.Split(string(payload), ",")
		if len(parts) != 2 {
			return marketdata.BBO{}, false, fmt.Errorf("bad BBO fixture")
		}
		return marketdata.BBO{
			Key: key, BidPrice: parts[0], AskPrice: parts[1],
			BidQuantity: "10000", AskQuantity: "10000",
			VenueTimestamp: received, ReceiveTimestamp: received,
		}, true, nil
	}
	market, err := marketdata.New(marketdata.Options{
		Connector: connector,
		Parsers: map[string]marketdata.Parser{
			"bybit": parser, "bitget": parser,
		},
		StaleAfter: time.Second, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = market.Close() })
	return market, connectionA, connectionB
}

func eventSchedulerCombination() ArbitrageCombination {
	return ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61799", OwnerUsername: "admin",
		Status: "running", RuntimeState: "monitoring",
		AskThresholdBps: "10", BidThresholdBps: "-10",
		TargetNotional: "10000", OrderNotional: "500", PositionNotional: "0",
		MarketDataStale: true,
		LegA: ArbitrageLeg{
			TradingAccountID: 1, Exchange: "bybit",
			ContractType: "perpetual", ExchangeSymbol: "COTIUSDT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, Exchange: "bitget",
			ContractType: "perpetual", ExchangeSymbol: "COTIUSDT",
		},
	}
}

func TestArbitrageSchedulerExchangeAllowlistFailsClosedPerVenue(t *testing.T) {
	scheduler := newTestArbitrageScheduler(
		nil, nil, nil, time.Second, 50*time.Millisecond, time.Second,
		1, 1, 1, 1, true, nil,
	)
	item := ArbitrageCombination{
		LegA: ArbitrageLeg{Exchange: "binance"},
		LegB: ArbitrageLeg{Exchange: "hyperliquid"},
	}
	if !scheduler.combinationEnabled(item) {
		t.Fatal("nil allowlist should preserve legacy behavior")
	}
	scheduler.ConfigureArbitrageExchanges(map[string]bool{
		"binance": true, "hyperliquid": true,
	})
	if !scheduler.combinationEnabled(item) {
		t.Fatal("enabled venues were rejected")
	}
	scheduler.ConfigureArbitrageExchanges(map[string]bool{"binance": true})
	if scheduler.combinationEnabled(item) {
		t.Fatal("disabled DEX venue was accepted")
	}
}

func runSchedulerForTest(t *testing.T, scheduler *ArbitrageScheduler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		scheduler.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("scheduler did not stop")
		}
	})
}

func waitForSchedulerCondition(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(message)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestShouldPersistMarketSnapshot(t *testing.T) {
	now := time.Now().UTC()
	if !shouldPersistMarketSnapshot(false, time.Time{}, now) {
		t.Fatal("empty lastPersist should write")
	}
	if shouldPersistMarketSnapshot(false, now, now.Add(time.Second)) {
		t.Fatal("writes inside 5s should be skipped")
	}
	if !shouldPersistMarketSnapshot(false, now, now.Add(marketSnapshotPersistInterval)) {
		t.Fatal("5s interval should write")
	}
	if !shouldPersistMarketSnapshot(true, now, now) {
		t.Fatal("stale edge should write immediately")
	}
}

func TestSchedulerExecutionResourcesSplitAndRelease(t *testing.T) {
	item := ArbitrageCombination{
		ID:   "combo-a",
		LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "binance"},
		LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "okx"},
	}
	other := ArbitrageCombination{
		ID:   "combo-b",
		LegA: ArbitrageLeg{TradingAccountID: 3, Exchange: "bybit"},
		LegB: ArbitrageLeg{TradingAccountID: 4, Exchange: "bitget"},
	}
	scheduler := newTestArbitrageScheduler(
		&dryRunStore{item: item}, nil, nil, time.Second, time.Millisecond, time.Second,
		1, 2, 4, 16, false, nil,
	)
	runtimeA := &arbitrageRuntime{combination: item}
	runtimeB := &arbitrageRuntime{combination: other}
	res, ok := scheduler.tryAcquireExecutionResources(
		runtimeA, workPermitNewExecution, true, true,
	)
	if !ok || res == nil {
		t.Fatal("first execution should acquire slot and permit")
	}
	blocked, blockedOK := scheduler.tryAcquireExecutionResources(
		runtimeA, workPermitNewExecution, true, true,
	)
	if blockedOK || blocked != nil {
		t.Fatal("same combination must not run two executions")
	}
	stats := scheduler.Stats()
	if stats.ExecutionSlotsInUse != 1 || stats.WorkPermitsInUse != 1 {
		t.Fatalf("slots=%d permits=%d", stats.ExecutionSlotsInUse, stats.WorkPermitsInUse)
	}
	res.permit.Yield()
	stats = scheduler.Stats()
	if stats.ExecutionSlotsInUse != 1 || stats.WorkPermitsInUse != 0 {
		t.Fatalf("maker wait should release permit only: slots=%d permits=%d",
			stats.ExecutionSlotsInUse, stats.WorkPermitsInUse)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := res.permit.Acquire(ctx, workPermitUrgent); err != nil {
		t.Fatalf("reacquire urgent: %v", err)
	}
	circuit, circuitOK := scheduler.tryAcquireExecutionResources(
		runtimeB, workPermitUrgent, false, false,
	)
	if !circuitOK {
		t.Fatal("circuit open / recovery must not require a lifecycle slot")
	}
	stats = scheduler.Stats()
	if stats.ExecutionSlotsInUse != 1 || stats.WorkPermitsInUse != 2 {
		t.Fatalf("circuit used a slot: slots=%d permits=%d",
			stats.ExecutionSlotsInUse, stats.WorkPermitsInUse)
	}
	scheduler.releaseExecutionResources(runtimeB, other, circuit)
	if runtimeB.executionRunning.Load() {
		t.Fatal("circuit must release executionRunning")
	}
	scheduler.releaseExecutionResources(runtimeA, item, res)
	stats = scheduler.Stats()
	if stats.ExecutionSlotsInUse != 0 || stats.WorkPermitsInUse != 0 ||
		stats.ActiveAccounts != 0 || stats.ActiveVenues != 0 {
		t.Fatalf("leaked resources: %+v", stats)
	}
}

func TestSchedulerSlotSaturationCountsRejected(t *testing.T) {
	item := ArbitrageCombination{
		ID:   "combo-a",
		LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "binance"},
		LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "okx"},
	}
	other := ArbitrageCombination{
		ID:   "combo-b",
		LegA: ArbitrageLeg{TradingAccountID: 3, Exchange: "bybit"},
		LegB: ArbitrageLeg{TradingAccountID: 4, Exchange: "bitget"},
	}
	scheduler := newTestArbitrageScheduler(
		&dryRunStore{item: item}, nil, nil, time.Second, time.Millisecond, time.Second,
		1, 1, 4, 16, false, nil,
	)
	runtimeA := &arbitrageRuntime{combination: item}
	runtimeB := &arbitrageRuntime{combination: other}
	res, ok := scheduler.tryAcquireExecutionResources(
		runtimeA, workPermitNewExecution, true, true,
	)
	if !ok {
		t.Fatal("first slot should be available")
	}
	saturated, satOK := scheduler.tryAcquireExecutionResources(
		runtimeB, workPermitNewExecution, true, true,
	)
	if satOK || saturated != nil {
		t.Fatal("second lifecycle slot should be rejected without a claim")
	}
	if scheduler.Stats().CapacityRejected == 0 {
		t.Fatal("saturation should count capacity_rejected")
	}
	scheduler.releaseExecutionResources(runtimeA, item, res)
	if scheduler.Stats().ExecutionSlotsInUse != 0 || scheduler.Stats().WorkPermitsInUse != 0 {
		t.Fatalf("leaked after saturation: %+v", scheduler.Stats())
	}
}

type closeSizeUnexecutableStore struct {
	arbitrageStore
	mu     sync.Mutex
	item   ArbitrageCombination
	calls  int
	events []string
}

func (s *closeSizeUnexecutableStore) RecordArbitrageCloseFailure(
	_ context.Context, _ string, key, message string,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.item.ErrorMessage = message
	s.item.LastFailureKey = key
	s.item.RuntimeState = "closing"
	s.item.CircuitOpen = false
	s.item.ConsecutiveFailures++
	s.item.NextRetryAt = time.Now().UTC().Add(2 * time.Second)
	return s.item, nil
}

func (s *closeSizeUnexecutableStore) AppendArbitrageEvent(
	_ context.Context, _, _, eventType string, _ map[string]any,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, eventType)
	return nil
}

func (s *closeSizeUnexecutableStore) GetActiveArbitrageExecution(
	context.Context, string,
) (ArbitrageExecution, error) {
	return ArbitrageExecution{}, ErrNotFound
}

func TestArbitrageSchedulerClosingUnexecutableSizeWritesRetry(t *testing.T) {
	item := lastClipCloseCombination("bid")
	store := &closeSizeUnexecutableStore{item: item}
	scheduler := newTestArbitrageScheduler(
		store, nil, nil, time.Second, time.Millisecond, time.Second,
		1, 1, 1, 1, false, nil,
	)
	scheduler.ConfigureArbitrageInstruments(silentRiskCatalog{
		101: lastClipCloseInstrument(101, "50", "5"),
		202: lastClipCloseInstrument(202, "50", "5"),
	})
	runtime := &arbitrageRuntime{combination: item}
	bbo := lastClipCloseBBO()
	bbo.ReceiveTimestamp = time.Now().UTC()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("unexecutable close should keep runtime")
	}
	if store.calls != 1 ||
		runtime.combination.LastFailureKey != "close_size_unexecutable" ||
		runtime.combination.NextRetryAt.IsZero() ||
		runtime.combination.NextRetryAt.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("calls=%d combo=%+v", store.calls, runtime.combination)
	}
	foundEvent := false
	for _, eventType := range store.events {
		if eventType == "close_size_unexecutable" {
			foundEvent = true
		}
	}
	if !foundEvent {
		t.Fatalf("events=%v", store.events)
	}
	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("retry backoff should skip second record")
	}
	if store.calls != 1 {
		t.Fatalf("second evaluate recorded again: calls=%d", store.calls)
	}
}

func TestArbitrageSchedulerLastClipHandoffTinyCloseAfterRetry(t *testing.T) {
	item := ownedFlattenCombo("1", "-2.2")
	item.LastFailureKey = lastClipHedgeMinNotionalFailureKey
	item.NextRetryAt = time.Now().UTC().Add(-time.Second)
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{closed: make(chan ArbitrageCombination, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if !scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("handoff tiny should close combination")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d", store.claims)
	}
	select {
	case <-runner.closed:
	case <-time.After(time.Second):
		t.Fatal("handoff tiny did not call CloseCombination")
	}
}

func TestArbitrageSchedulerLastClipHandoffWaitsForNextRetryAt(t *testing.T) {
	item := ownedFlattenCombo("1", "-2.2")
	item.LastFailureKey = lastClipHedgeMinNotionalFailureKey
	item.NextRetryAt = time.Now().UTC().Add(time.Minute)
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{
		executed: make(chan ArbitrageExecution, 1),
		closed:   make(chan ArbitrageCombination, 1),
	}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("backoff should skip flatten work")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d", store.claims)
	}
	select {
	case <-runner.closed:
		t.Fatal("backoff must not close")
	case <-runner.executed:
		t.Fatal("backoff must not resume")
	default:
	}
}

func TestArbitrageSchedulerLastClipHandoffResumesActiveBeforeTiny(t *testing.T) {
	item := ownedFlattenCombo("1", "-2.2")
	item.LastFailureKey = lastClipHedgeMinNotionalFailureKey
	store := &dryRunStore{
		claim: true, item: item,
		activeExecution: &ArbitrageExecution{ID: "active-last-clip", Status: "hedging"},
	}
	runner := &schedulerRunner{
		executed: make(chan ArbitrageExecution, 1),
		closed:   make(chan ArbitrageCombination, 1),
	}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("active execution should resume rather than close")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d", store.claims)
	}
	select {
	case execution := <-runner.executed:
		if execution.ID != "active-last-clip" {
			t.Fatalf("execution=%+v", execution)
		}
	case <-time.After(time.Second):
		t.Fatal("expected resumeActive")
	}
	select {
	case <-runner.closed:
		t.Fatal("active execution must not tiny close")
	default:
	}
}

func TestArbitrageSchedulerLastClipHandoffLargeLegStillClaims(t *testing.T) {
	item := ownedFlattenCombo("1", "-100")
	item.LastFailureKey = lastClipHedgeMinNotionalFailureKey
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("large leftover should claim sized close")
	}
	if store.claims != 1 {
		t.Fatalf("claims=%d", store.claims)
	}
}

func TestArbitrageSchedulerLastClipHandoffPreservesKeyOnRetryableCloseError(t *testing.T) {
	item := ownedFlattenCombo("1", "-2.2")
	item.LastFailureKey = lastClipHedgeMinNotionalFailureKey
	item.NextRetryAt = time.Now().UTC().Add(-time.Second)
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{
		closed:   make(chan ArbitrageCombination, 1),
		closeErr: errors.New("close combination db unavailable"),
	}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("retryable close should not report success")
	}
	if runtime.combination.LastFailureKey != lastClipHedgeMinNotionalFailureKey {
		t.Fatalf("key=%s", runtime.combination.LastFailureKey)
	}
	waiting := ownedFlattenCombo("1", "-2.2")
	waiting.LastFailureKey = lastClipHedgeMinNotionalFailureKey
	waiting.NextRetryAt = time.Now().UTC().Add(-time.Second)
	waitingStore := &dryRunStore{claim: true, item: waiting}
	waitingRunner := &schedulerRunner{
		closed:   make(chan ArbitrageCombination, 1),
		closeErr: ErrArbitrageCloseWaitingSnapshot,
	}
	waitingScheduler := newTestArbitrageScheduler(
		waitingStore, nil, waitingRunner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	waitingRuntime := &arbitrageRuntime{combination: waiting}
	if waitingScheduler.flattenOrFinalizeClosing(
		context.Background(), waitingRuntime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("waiting snapshot should not close")
	}
	if waitingRuntime.combination.LastFailureKey != lastClipHedgeMinNotionalFailureKey {
		t.Fatalf("waiting key=%s", waitingRuntime.combination.LastFailureKey)
	}
	if waitingRuntime.combination.ConsecutiveFailures != 0 {
		t.Fatalf("waiting snapshot must not record failure combo=%+v", waitingRuntime.combination)
	}
}

func oneShotExitingLeftover(legA, legB string) ArbitrageCombination {
	item := ownedFlattenCombo(legA, legB)
	item.Status = "running"
	item.RuntimeState = "monitoring"
	item.RunMode = "one_shot"
	item.OneShotPhase = "exiting"
	item.TargetNotional = "20"
	return item
}

func TestArbitrageSchedulerBBOEventDoesNotQueryDustGate(t *testing.T) {
	item := oneShotExitingLeftover("0", "-10")
	store := &dryRunStore{item: item}
	scheduler := newTestArbitrageScheduler(
		store, nil, &schedulerRunner{}, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosingFrom(
		context.Background(), runtime, bbo, bbo, px, px, px, px, "bbo_event",
	) {
		t.Fatal("bbo leftover should keep runtime")
	}
	if store.dustGateCalls != 0 {
		t.Fatalf("bbo queried dust gate %d times", store.dustGateCalls)
	}
	if runtime.combination.OneShotPhase != "exiting" ||
		runtime.combination.RuntimeState == "manual_intervention" {
		t.Fatalf("combination=%+v", runtime.combination)
	}
}

func TestArbitrageSchedulerBBOLargeLeftoverMarksManual(t *testing.T) {
	item := oneShotExitingLeftover("0", "-10")
	store := &dryRunStore{item: item}
	scheduler := newTestArbitrageScheduler(
		store, nil, &schedulerRunner{}, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	bbo.BidPrice = "3"
	bbo.AskPrice = "3"
	px := decimal.NewFromInt(3)
	if scheduler.flattenOrFinalizeClosingFrom(
		context.Background(), runtime, bbo, bbo, px, px, px, px, "bbo_event",
	) {
		t.Fatal("large leftover should keep runtime")
	}
	if store.dustGateCalls != 0 {
		t.Fatalf("bbo queried dust gate %d times", store.dustGateCalls)
	}
	if runtime.combination.RuntimeState != "manual_intervention" {
		t.Fatalf("combination=%+v", runtime.combination)
	}
}

func TestArbitrageSchedulerExitingDustAcceptedExitsWithoutOrder(t *testing.T) {
	item := oneShotExitingLeftover("0", "-10")
	store := &dryRunStore{claim: true, item: item}
	scheduler := newTestArbitrageScheduler(
		store, nil, &schedulerRunner{closed: make(chan ArbitrageCombination, 1)},
		time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosingFrom(
		context.Background(), runtime, bbo, bbo, px, px, px, px, "execution_completion",
	) {
		t.Fatal("dust leftover should keep runtime")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d", store.claims)
	}
	if runtime.combination.OneShotPhase != "exited" {
		t.Fatalf("combination=%+v", runtime.combination)
	}
	if runtime.combination.LegBBasePosition != "-10" {
		t.Fatalf("positions rewritten: %+v", runtime.combination)
	}
}

func TestArbitrageSchedulerDustGateErrorDoesNotEnterManualThenRecovers(t *testing.T) {
	item := oneShotExitingLeftover("0", "-10")
	store := &dryRunStore{item: item, dustGateErr: errors.New("db unavailable")}
	scheduler := newTestArbitrageScheduler(
		store, nil, &schedulerRunner{}, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosingFrom(
		context.Background(), runtime, bbo, bbo, px, px, px, px, "control",
	) {
		t.Fatal("gate error should keep runtime")
	}
	if runtime.combination.RuntimeState == "manual_intervention" ||
		runtime.combination.OneShotPhase != "exiting" {
		t.Fatalf("gate error entered MI: %+v", runtime.combination)
	}
	store.dustGateErr = nil
	if scheduler.flattenOrFinalizeClosingFrom(
		context.Background(), runtime, bbo, bbo, px, px, px, px, "control",
	) {
		t.Fatal("recovery should keep runtime")
	}
	if runtime.combination.OneShotPhase != "exited" {
		t.Fatalf("control did not recover: %+v", runtime.combination)
	}
}

func TestArbitrageSchedulerUnreconciledDustDoesNotEnterManualThenRecovers(t *testing.T) {
	item := oneShotExitingLeftover("0", "-10")
	store := &dryRunStore{item: item, dustGateUnreconciled: true}
	scheduler := newTestArbitrageScheduler(
		store, nil, &schedulerRunner{}, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosingFrom(
		context.Background(), runtime, bbo, bbo, px, px, px, px, "control",
	) {
		t.Fatal("unreconciled should keep runtime")
	}
	if runtime.combination.RuntimeState == "manual_intervention" {
		t.Fatal("unreconciled entered MI")
	}
	store.dustGateUnreconciled = false
	if scheduler.flattenOrFinalizeClosingFrom(
		context.Background(), runtime, bbo, bbo, px, px, px, px, "control",
	) {
		t.Fatal("recovery should keep runtime")
	}
	if runtime.combination.OneShotPhase != "exited" {
		t.Fatalf("control did not recover: %+v", runtime.combination)
	}
}

func TestArbitrageSchedulerDustCASConflictRetriesThenSucceeds(t *testing.T) {
	item := oneShotExitingLeftover("0", "-10")
	store := &dryRunStore{item: item, withDustSkip: true}
	scheduler := newTestArbitrageScheduler(
		store, nil, &schedulerRunner{}, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosingFrom(
		context.Background(), runtime, bbo, bbo, px, px, px, px, "control",
	) {
		t.Fatal("CAS skip should keep runtime")
	}
	if runtime.combination.RuntimeState == "manual_intervention" ||
		runtime.combination.OneShotPhase != "exiting" {
		t.Fatalf("CAS conflict entered MI: %+v", runtime.combination)
	}
	store.withDustSkip = false
	if scheduler.flattenOrFinalizeClosingFrom(
		context.Background(), runtime, bbo, bbo, px, px, px, px, "control",
	) {
		t.Fatal("recovery should keep runtime")
	}
	if runtime.combination.OneShotPhase != "exited" {
		t.Fatalf("control did not recover: %+v", runtime.combination)
	}
}

func TestArbitrageSchedulerLargeUnpairedOneShotIsUnsafeManual(t *testing.T) {
	item := oneShotExitingLeftover("0", "-100")
	store := &dryRunStore{item: item}
	scheduler := newTestArbitrageScheduler(
		store, nil, &schedulerRunner{}, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	bbo.BidPrice = "1"
	bbo.AskPrice = "1"
	px := decimal.NewFromInt(1)
	if scheduler.flattenOrFinalizeClosingFrom(
		context.Background(), runtime, bbo, bbo, px, px, px, px, "control",
	) {
		t.Fatal("unsafe leftover should keep runtime")
	}
	if runtime.combination.RuntimeState != "manual_intervention" {
		t.Fatalf("combination=%+v", runtime.combination)
	}
}

func TestArbitrageSchedulerRunningExitedStopsEvaluate(t *testing.T) {
	item := oneShotExitingLeftover("0", "-10")
	item.OneShotPhase = "exited"
	store := &dryRunStore{claim: true, item: item}
	scheduler := newTestArbitrageScheduler(
		store, nil, &schedulerRunner{closed: make(chan ArbitrageCombination, 1)},
		time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	if scheduler.evaluate(context.Background(), runtime, "bbo_event") {
		t.Fatal("running exited should keep runtime")
	}
	if store.claims != 0 {
		t.Fatalf("claims=%d", store.claims)
	}
}

func TestArbitrageSchedulerClosingExitedTinyFlatten(t *testing.T) {
	item := ownedFlattenCombo("0", "-10")
	item.RunMode = "one_shot"
	item.OneShotPhase = "exited"
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{closed: make(chan ArbitrageCombination, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if !scheduler.flattenOrFinalizeClosingFrom(
		context.Background(), runtime, bbo, bbo, px, px, px, px, "control",
	) {
		t.Fatal("closing exited should tiny flatten")
	}
	select {
	case <-runner.closed:
	case <-time.After(time.Second):
		t.Fatal("closing exited did not call CloseCombination")
	}
}
