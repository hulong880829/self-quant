package trader

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/marketdata"
)

const (
	circuitOpenReconcileInterval     = 30 * time.Second
	marketSnapshotPersistInterval    = 5 * time.Second
	closeWaitingSnapshotLogInterval  = 30 * time.Second
	defaultSignalBBOStale            = 2 * time.Second
	arbitrageControlSummaryBatchSize = 250
	arbitrageLeaseBatchSize          = 250
	arbitrageLeaseMergeWindow        = 50 * time.Millisecond
	arbitrageSnapshotMergeWindow     = 150 * time.Millisecond
	arbitrageSnapshotBatchSize       = 200
	metricsApplyRetryDelay           = 15 * time.Second
)

type arbitrageExecutionRunner interface {
	Execute(context.Context, ArbitrageCombination, ArbitrageExecution, marketdata.BBO, marketdata.BBO, *executionWorkPermit)
	RecoverWithoutMarketData(
		context.Context, ArbitrageCombination, ArbitrageExecution,
	) error
	ReconcileCircuitOpen(
		context.Context, ArbitrageCombination, ArbitrageExecution,
	) error
	CloseCombination(context.Context, ArbitrageCombination) error
}

type ArbitrageScheduler struct {
	store                 arbitrageStore
	market                *marketdata.Manager
	runner                arbitrageExecutionRunner
	catalog               instrumentCatalog
	controlInterval       time.Duration
	coalesceWindow        time.Duration
	lease                 time.Duration
	signalBBOStale        time.Duration
	batch                 int
	workers               int
	maxAccount            int
	maxVenue              int
	enabledVenues         map[string]bool
	dryRun                bool
	logger                *slog.Logger
	notionalRand          arbitrageNotionalRand
	mu                    sync.Mutex
	runtimes              map[string]*arbitrageRuntime
	valuations            map[string]ArbitrageValuation
	executionSlots        chan struct{}
	workPermits           *workPermitPool
	slotsInUse            atomic.Int64
	accountUse            map[int64]int
	venueUse              map[string]int
	runtimeWG             sync.WaitGroup
	triggers              atomic.Uint64
	claims                atomic.Uint64
	staleBBO              atomic.Uint64
	rejected              atomic.Uint64
	bboWakeups            atomic.Uint64
	bboEvaluations        atomic.Uint64
	evaluations           atomic.Uint64
	controlWakeups        atomic.Uint64
	staleWakeups          atomic.Uint64
	retryWakeups          atomic.Uint64
	controlProbes         atomic.Uint64
	fullReloads           atomic.Uint64
	snapshotWrites        atomic.Uint64
	signalStaleRejected   atomic.Uint64
	nextRuntimeGeneration atomic.Uint64
	batchersStarted       atomic.Bool
	controlBatcher        *controlSummaryBatcher
	leaseBatcher          *leaseBatcher
	snapshotBatcher       *snapshotBatcher
	lifecycle             arbitrageRuntimeLifecycle
	closeWaitingLogAt     sync.Map
}

type arbitrageRuntime struct {
	combination                   ArbitrageCombination
	ctx                           context.Context
	cancel                        context.CancelFunc
	legA                          *marketdata.Subscription
	legB                          *marketdata.Subscription
	latestA                       marketdata.BBO
	latestB                       marketdata.BBO
	hasA                          bool
	hasB                          bool
	nextRenew                     time.Time
	lastPersist                   time.Time
	lastSignal                    string
	completion                    chan struct{}
	done                          chan struct{}
	initOnce                      sync.Once
	cleanupOnce                   sync.Once
	loopStarted                   atomic.Bool
	executionRunning              atomic.Bool
	recoveryRunning               atomic.Bool
	lastCircuitOpenReconcileAt    time.Time
	lastConfirmedAbsentFinalizeAt time.Time
	lastDustCheckAt               time.Time
	generation                    uint64
	controlProbe                  atomic.Value
	leaseNotify                   chan struct{}
	leaseNotice                   atomic.Value
	renewPending                  bool
	snapshotNotify                chan struct{}
	snapshotNotice                atomic.Value
	snapshotSequence              uint64
	lastAppliedSnapshotSeq        uint64
	snapshotInFlightSeq           uint64
	nextSnapshotAttemptAt         time.Time
	lastSnapshotUpdatedAt         time.Time
}

type arbitrageRuntimeLifecycle interface {
	RegisterCombination(ArbitrageCombination)
	UnregisterCombination(string)
}

func NewArbitrageScheduler(
	store arbitrageStore,
	market *marketdata.Manager,
	runner arbitrageExecutionRunner,
	controlInterval, coalesceWindow, lease time.Duration,
	batch, workers int,
	maxAccount, maxVenue int,
	dryRun bool,
	logger *slog.Logger,
) *ArbitrageScheduler {
	if logger == nil {
		logger = slog.Default()
	}
	if maxAccount <= 0 {
		maxAccount = 4
	}
	if maxVenue <= 0 {
		maxVenue = 16
	}
	if controlInterval <= 0 {
		controlInterval = time.Second
	}
	if coalesceWindow <= 0 {
		coalesceWindow = 50 * time.Millisecond
	}
	if lease <= 0 {
		lease = 30 * time.Second
	}
	if batch <= 0 {
		batch = 50
	}
	if workers <= 0 {
		workers = 16
	}
	scheduler := &ArbitrageScheduler{
		store: store, market: market, runner: runner,
		controlInterval: controlInterval, coalesceWindow: coalesceWindow, lease: lease,
		signalBBOStale: defaultSignalBBOStale,
		batch:          batch, workers: workers, dryRun: dryRun, logger: logger,
		maxAccount: maxAccount, maxVenue: maxVenue,
		runtimes:       make(map[string]*arbitrageRuntime),
		valuations:     make(map[string]ArbitrageValuation),
		executionSlots: make(chan struct{}, workers),
		workPermits:    newWorkPermitPool(workers),
		accountUse:     make(map[int64]int), venueUse: make(map[string]int),
	}
	scheduler.controlBatcher = newControlSummaryBatcher(scheduler)
	scheduler.leaseBatcher = newLeaseBatcher(scheduler)
	scheduler.snapshotBatcher = newSnapshotBatcher(scheduler)
	return scheduler
}

func (s *ArbitrageScheduler) ConfigureRuntimeLifecycle(lifecycle arbitrageRuntimeLifecycle) {
	s.lifecycle = lifecycle
}

func (s *ArbitrageScheduler) StartBatchers(ctx context.Context) {
	if s == nil || !s.batchersStarted.CompareAndSwap(false, true) {
		return
	}
	if s.controlBatcher != nil {
		go s.controlBatcher.Run(ctx)
	}
	if s.leaseBatcher != nil {
		go s.leaseBatcher.Run(ctx)
	}
	if s.snapshotBatcher != nil {
		go s.snapshotBatcher.Run(ctx)
	}
}

func (s *ArbitrageScheduler) ConfigureArbitrageInstruments(
	catalog instrumentCatalog,
) {
	s.catalog = catalog
}

func (s *ArbitrageScheduler) ConfigureArbitrageExchanges(enabled map[string]bool) {
	s.enabledVenues = copyExchangeSet(enabled)
}

func (s *ArbitrageScheduler) ConfigureSignalBBOStale(d time.Duration) {
	if d > 0 {
		s.signalBBOStale = d
	}
}

func (s *ArbitrageScheduler) signalBBOTooOld(bboA, bboB marketdata.BBO) bool {
	freshNow := time.Now().UTC()
	return bboA.Stale(freshNow, s.signalBBOStale) || bboB.Stale(freshNow, s.signalBBOStale)
}

func (s *ArbitrageScheduler) rejectStaleSignal(bboA, bboB marketdata.BBO) bool {
	if !s.signalBBOTooOld(bboA, bboB) {
		return false
	}
	s.signalStaleRejected.Add(1)
	return true
}

func (s *ArbitrageScheduler) Run(ctx context.Context) {
	s.StartBatchers(ctx)
	ticker := time.NewTicker(s.controlInterval)
	defer ticker.Stop()
	defer s.closeAll()
	s.acquire(ctx, true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.acquire(ctx, true)
		}
	}
}

// runOnce remains a synchronous entry point for deterministic scheduler tests.
// Production uses Run, which starts one event loop per leased combination.
func (s *ArbitrageScheduler) runOnce(ctx context.Context) {
	s.acquire(ctx, false)
	s.mu.Lock()
	runtimes := make([]*arbitrageRuntime, 0, len(s.runtimes))
	for _, runtime := range s.runtimes {
		runtimes = append(runtimes, runtime)
	}
	s.mu.Unlock()
	for _, runtime := range runtimes {
		if s.renewLease(ctx, runtime) {
			continue
		}
		s.evaluate(ctx, runtime, "synchronous")
	}
}

func (s *ArbitrageScheduler) acquire(ctx context.Context, startLoop bool) {
	if s.store == nil || s.market == nil {
		return
	}
	items, err := s.store.LeaseArbitrageCombinations(ctx, s.batch, s.lease)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Error("lease arbitrage combinations failed", "error", err)
		}
		return
	}
	for _, item := range items {
		s.mu.Lock()
		_, exists := s.runtimes[item.ID]
		s.mu.Unlock()
		if exists {
			continue
		}
		runtime, err := s.subscribe(ctx, item)
		if err != nil {
			s.logger.Error("subscribe arbitrage BBO failed", "combination_id", item.ID, "error", err)
			continue
		}
		s.mu.Lock()
		if _, exists = s.runtimes[item.ID]; exists {
			s.mu.Unlock()
			s.finishRuntime(runtime)
			continue
		}
		s.runtimes[item.ID] = runtime
		s.mu.Unlock()
		s.registerRuntimeLifecycle(item)
		s.logger.Info("arbitrage combination leased", "combination_id", item.ID)
		if startLoop {
			runtime.loopStarted.Store(true)
			s.runtimeWG.Add(1)
			go func() {
				defer s.runtimeWG.Done()
				s.runCombination(ctx, runtime)
			}()
		}
	}
}

func (s *ArbitrageScheduler) subscribe(
	parent context.Context,
	item ArbitrageCombination,
) (*arbitrageRuntime, error) {
	ctx, cancel := context.WithCancel(parent)
	keyA, err := marketdata.NewKey(item.LegA.Exchange, item.LegA.ContractType, item.LegA.ExchangeSymbol)
	if err != nil {
		cancel()
		return nil, err
	}
	keyB, err := marketdata.NewKey(item.LegB.Exchange, item.LegB.ContractType, item.LegB.ExchangeSymbol)
	if err != nil {
		cancel()
		return nil, err
	}
	subA, err := s.market.Subscribe(ctx, keyA)
	if err != nil {
		cancel()
		return nil, err
	}
	subB, err := s.market.Subscribe(ctx, keyB)
	if err != nil {
		_ = subA.Close()
		cancel()
		return nil, err
	}
	runtime := &arbitrageRuntime{
		combination: item, ctx: ctx, cancel: cancel, legA: subA, legB: subB,
		nextRenew:             time.Now().UTC().Add(s.lease / 3),
		generation:            s.nextRuntimeGeneration.Add(1),
		lastSnapshotUpdatedAt: item.UpdatedAt,
	}
	s.ensureRuntime(runtime)
	return runtime, nil
}

func (s *ArbitrageScheduler) ensureRuntime(runtime *arbitrageRuntime) {
	runtime.initOnce.Do(func() {
		if runtime.completion == nil {
			runtime.completion = make(chan struct{}, 1)
		}
		if runtime.done == nil {
			runtime.done = make(chan struct{})
		}
		if runtime.leaseNotify == nil {
			runtime.leaseNotify = make(chan struct{}, 1)
		}
		if runtime.snapshotNotify == nil {
			runtime.snapshotNotify = make(chan struct{}, 1)
		}
	})
}

func (s *ArbitrageScheduler) runCombination(
	schedulerCtx context.Context,
	runtime *arbitrageRuntime,
) {
	s.ensureRuntime(runtime)
	defer s.finishRuntime(runtime)

	controlTicker := time.NewTicker(s.controlInterval)
	defer controlTicker.Stop()
	renewAfter := s.lease / 3
	if renewAfter <= 0 {
		renewAfter = time.Second
	}
	renewTimer := time.NewTimer(renewAfter)
	defer renewTimer.Stop()
	coalesceTimer := time.NewTimer(time.Hour)
	if !coalesceTimer.Stop() {
		<-coalesceTimer.C
	}
	defer coalesceTimer.Stop()
	var coalesceC <-chan time.Time
	retryTimer := time.NewTimer(time.Hour)
	if !retryTimer.Stop() {
		<-retryTimer.C
	}
	defer retryTimer.Stop()
	var retryC <-chan time.Time

	resetRetry := func() {
		stopAndDrainTimer(retryTimer)
		retryC = nil
		delay, ok := retryTimerDelay(runtime.combination.NextRetryAt, time.Now().UTC())
		if ok {
			retryTimer.Reset(delay)
			retryC = retryTimer.C
		}
	}
	resetRetry()

	legAUpdates := runtime.legA.Updates()
	legBUpdates := runtime.legB.Updates()
	for {
		select {
		case <-schedulerCtx.Done():
			s.waitForExecution(runtime)
			return
		case <-runtime.ctx.Done():
			s.waitForExecution(runtime)
			return
		case bbo, ok := <-legAUpdates:
			if !ok {
				legAUpdates = nil
				runtime.cancel()
				continue
			}
			runtime.latestA, runtime.hasA = bbo, true
			s.bboWakeups.Add(1)
			if coalesceC == nil {
				coalesceTimer.Reset(s.coalesceWindow)
				coalesceC = coalesceTimer.C
			}
		case bbo, ok := <-legBUpdates:
			if !ok {
				legBUpdates = nil
				runtime.cancel()
				continue
			}
			runtime.latestB, runtime.hasB = bbo, true
			s.bboWakeups.Add(1)
			if coalesceC == nil {
				coalesceTimer.Reset(s.coalesceWindow)
				coalesceC = coalesceTimer.C
			}
		case <-coalesceC:
			coalesceC = nil
			if runtime.hasA && runtime.hasB &&
				s.evaluate(schedulerCtx, runtime, "bbo_event") {
				return
			}
		case <-controlTicker.C:
			s.controlWakeups.Add(1)
			stop, refreshed := s.handleControl(schedulerCtx, runtime)
			if refreshed {
				resetRetry()
			}
			if stop {
				return
			}
		case <-renewTimer.C:
			if s.renewLease(schedulerCtx, runtime) {
				runtime.cancel()
				s.waitForExecution(runtime)
				return
			}
			delay := time.Until(runtime.nextRenew)
			if delay <= 0 {
				if runtime.renewPending {
					delay = arbitrageLeaseMergeWindow
				} else {
					delay = renewAfter
				}
			}
			renewTimer.Reset(delay)
		case <-runtime.leaseNotify:
			if s.renewLease(schedulerCtx, runtime) {
				runtime.cancel()
				s.waitForExecution(runtime)
				return
			}
			delay := time.Until(runtime.nextRenew)
			if delay <= 0 {
				if runtime.renewPending {
					delay = arbitrageLeaseMergeWindow
				} else {
					delay = renewAfter
				}
			}
			stopAndDrainTimer(renewTimer)
			renewTimer.Reset(delay)
		case <-runtime.snapshotNotify:
			if s.applySnapshotReceipt(schedulerCtx, runtime) {
				s.waitForExecution(runtime)
				return
			}
		case <-retryC:
			retryC = nil
			s.retryWakeups.Add(1)
			stop, _ := s.refreshCombination(schedulerCtx, runtime)
			if stop {
				return
			}
			resetRetry()
			if s.evaluate(schedulerCtx, runtime, "next_retry") {
				return
			}
		case <-runtime.completion:
			stop, refreshed := s.refreshCombination(schedulerCtx, runtime)
			if stop {
				return
			}
			if !refreshed {
				continue
			}
			resetRetry()
			s.maybePromoteOneShotWaitingExit(schedulerCtx, runtime, true)
			if arbitrageFlattening(runtime.combination) &&
				s.evaluate(schedulerCtx, runtime, "execution_completion") {
				return
			}
		}
	}
}

func stopAndDrainTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func retryTimerDelay(at, now time.Time) (time.Duration, bool) {
	if at.IsZero() || at.Equal(time.Unix(0, 0).UTC()) || at.Year() >= 9999 ||
		!at.After(now) {
		return 0, false
	}
	return at.Sub(now), true
}

func retryReady(at, now time.Time) bool {
	return !at.IsZero() && !at.Equal(time.Unix(0, 0).UTC()) &&
		at.Year() < 9999 && !at.After(now)
}

func (s *ArbitrageScheduler) waitForExecution(runtime *arbitrageRuntime) {
	for runtime.executionRunning.Load() {
		<-runtime.completion
	}
}

func (s *ArbitrageScheduler) renewLease(
	ctx context.Context,
	runtime *arbitrageRuntime,
) bool {
	now := time.Now().UTC()
	if notice := takeLeaseNotice(runtime); notice != nil {
		if notice.generation != runtime.generation {
			return false
		}
		runtime.renewPending = false
		if notice.failed || !notice.renewed {
			return true
		}
		runtime.nextRenew = now.Add(s.lease / 3)
		return false
	}
	if now.Before(runtime.nextRenew) {
		return false
	}
	if runtime.renewPending {
		return false
	}
	if s.leaseBatcher != nil && s.batchersStarted.Load() {
		if _, ok := s.store.(arbitrageLeaseBatchStore); ok {
			runtime.renewPending = true
			s.leaseBatcher.Submit(runtime)
			return false
		}
	}
	renewed, err := s.store.RenewArbitrageLease(ctx, runtime.combination.ID, s.lease)
	if err != nil || !renewed {
		return true
	}
	runtime.nextRenew = now.Add(s.lease / 3)
	return false
}

func takeLeaseNotice(runtime *arbitrageRuntime) *leaseRenewNotice {
	if runtime == nil {
		return nil
	}
	value := runtime.leaseNotice.Swap((*leaseRenewNotice)(nil))
	notice, _ := value.(*leaseRenewNotice)
	return notice
}

func (s *ArbitrageScheduler) handleControl(
	ctx context.Context,
	runtime *arbitrageRuntime,
) (stop, refreshed bool) {
	stop, refreshed = s.refreshCombinationPeriodic(ctx, runtime)
	if stop {
		return stop, refreshed
	}
	if arbitrageFlattening(runtime.combination) {
		return s.evaluate(ctx, runtime, "control_flatten"), refreshed
	}
	if runtime.combination.Status != "running" {
		return true, refreshed
	}
	s.reconcileCircuitOpen(ctx, runtime)
	s.maybeFinalizeConfirmedAbsent(ctx, runtime)
	if runtime.combination.RuntimeState == "backoff" &&
		retryReady(runtime.combination.NextRetryAt, time.Now().UTC()) {
		return s.evaluate(ctx, runtime, "control_retry"), refreshed
	}
	bboA, errA := runtime.legA.Latest()
	bboB, errB := runtime.legB.Latest()
	if errA != nil || errB != nil {
		s.staleWakeups.Add(1)
		s.handleMarketUnavailable(ctx, runtime)
		return false, refreshed
	}
	if runtime.combination.MarketDataStale {
		return s.evaluate(ctx, runtime, "stale_recovered"), refreshed
	}
	runtime.latestA, runtime.latestB = bboA, bboB
	runtime.hasA, runtime.hasB = true, true
	s.resumeActive(ctx, runtime, bboA, bboB)
	s.maybePromoteOneShotWaitingExit(ctx, runtime, false)
	return false, refreshed
}

func (s *ArbitrageScheduler) reconcileCircuitOpen(
	ctx context.Context,
	runtime *arbitrageRuntime,
) {
	if s == nil || s.runner == nil || s.store == nil || runtime == nil {
		return
	}
	if !runtime.combination.CircuitOpen || runtime.combination.Status != "running" {
		return
	}
	now := time.Now().UTC()
	if !runtime.lastCircuitOpenReconcileAt.IsZero() &&
		now.Sub(runtime.lastCircuitOpenReconcileAt) < circuitOpenReconcileInterval {
		return
	}
	if runtime.executionRunning.Load() {
		return
	}
	execution, err := s.store.GetActiveArbitrageExecution(ctx, runtime.combination.ID)
	if err != nil {
		return
	}
	res, ok := s.tryAcquireExecutionResources(
		runtime, workPermitUrgent, false, false,
	)
	if !ok {
		return
	}
	runtime.lastCircuitOpenReconcileAt = now
	snapshot := runtime.combination
	s.ensureRuntime(runtime)
	go func(item ArbitrageCombination, execution ArbitrageExecution, res *executionResources) {
		defer s.notifyExecutionComplete(runtime)
		defer s.releaseExecutionResources(runtime, item, res)
		if recErr := s.runner.ReconcileCircuitOpen(ctx, item, execution); recErr != nil &&
			s.logger != nil {
			s.logger.Info(
				"circuit open readonly reconcile failed",
				"combination_id", item.ID,
				"execution_id", execution.ID,
				"error", sanitizeError(recErr),
			)
		}
	}(snapshot, execution, res)
}

func (s *ArbitrageScheduler) maybeFinalizeConfirmedAbsent(
	ctx context.Context,
	runtime *arbitrageRuntime,
) {
	if s == nil || s.store == nil || runtime == nil {
		return
	}
	if runtime.executionRunning.Load() {
		return
	}
	if !confirmedAbsentFinalizeEligible(runtime.combination) {
		return
	}
	now := time.Now().UTC()
	if !runtime.lastConfirmedAbsentFinalizeAt.IsZero() &&
		now.Sub(runtime.lastConfirmedAbsentFinalizeAt) < circuitOpenReconcileInterval {
		return
	}
	execution, err := s.store.GetActiveArbitrageExecution(ctx, runtime.combination.ID)
	runtime.lastConfirmedAbsentFinalizeAt = now
	if err != nil {
		return
	}
	result, finErr := s.store.FinalizeConfirmedAbsentZeroFillExecution(ctx, execution.ID)
	if finErr != nil {
		if s.logger != nil {
			s.logger.Info(
				"confirmed absent finalize failed",
				"combination_id", runtime.combination.ID,
				"execution_id", execution.ID,
				"error", sanitizeError(finErr),
			)
		}
		return
	}
	if result == confirmedAbsentFinalizeRecovered ||
		result == confirmedAbsentFinalizeCanceledExec {
		s.refreshCombination(ctx, runtime)
	}
}

func (s *ArbitrageScheduler) refreshCombination(
	ctx context.Context,
	runtime *arbitrageRuntime,
) (stop, refreshed bool) {
	return s.refreshCombinationMode(ctx, runtime, false)
}

func (s *ArbitrageScheduler) refreshCombinationPeriodic(
	ctx context.Context,
	runtime *arbitrageRuntime,
) (stop, refreshed bool) {
	return s.refreshCombinationMode(ctx, runtime, true)
}

func (s *ArbitrageScheduler) refreshCombinationMode(
	ctx context.Context,
	runtime *arbitrageRuntime,
	periodic bool,
) (stop, refreshed bool) {
	if periodic {
		if probe, ok := runtime.controlProbe.Load().(controlProbeSnapshot); ok && probe.ready {
			s.controlProbes.Add(1)
			if probe.queryFailed {
				return false, false
			}
			if probe.missing {
				return true, false
			}
			if probe.summary.Status != "running" && probe.summary.Status != "closing" {
				return true, false
			}
			if probe.summary.Version == runtime.combination.Version &&
				probe.summary.Status == runtime.combination.Status &&
				probe.summary.RuntimeState == runtime.combination.RuntimeState {
				return false, false
			}
			return s.reloadCombination(ctx, runtime)
		}
	}
	if controls, ok := s.store.(arbitrageControlStore); ok {
		s.controlProbes.Add(1)
		summary, err := controls.GetArbitrageControlSummary(ctx, runtime.combination.ID)
		if err != nil {
			return errors.Is(err, ErrNotFound), false
		}
		if summary.Status != "running" && summary.Status != "closing" {
			return true, false
		}
		if summary.Version == runtime.combination.Version &&
			summary.Status == runtime.combination.Status &&
			summary.RuntimeState == runtime.combination.RuntimeState {
			return false, false
		}
	}
	return s.reloadCombination(ctx, runtime)
}

func (s *ArbitrageScheduler) reloadCombination(
	ctx context.Context,
	runtime *arbitrageRuntime,
) (stop, refreshed bool) {
	s.fullReloads.Add(1)
	item, err := s.store.GetArbitrageCombinationByOwner(
		ctx, runtime.combination.OwnerUsername, runtime.combination.ID,
	)
	if err != nil {
		return errors.Is(err, ErrNotFound), false
	}
	runtime.combination = item
	return item.Status != "running" && item.Status != "closing", true
}

func (s *ArbitrageScheduler) closeCombination(
	ctx context.Context,
	runtime *arbitrageRuntime,
) bool {
	if runtime.combination.Status == "closing" {
		if !combinationLooksFlat(runtime.combination) {
			if !s.closeFlattenTinyOwnedPrecheck(ctx, runtime.combination, runtime.latestA, runtime.latestB) {
				return false
			}
		}
		if runtime.recoveryRunning.Load() {
			return false
		}
		if runtime.combination.NextRetryAt.After(time.Now().UTC()) {
			return false
		}
		if s.runner != nil {
			if err := s.runner.CloseCombination(ctx, runtime.combination); err != nil {
				if ctx.Err() != nil {
					return false
				}
				if errors.Is(err, ErrArbitrageCloseWaitingSnapshot) {
					s.logCloseWaitingSnapshot(runtime.combination.ID)
					return false
				}
				failureKey := "close_internal"
				switch {
				case errors.Is(err, ErrCloseOrderOpen):
					failureKey = "close_order_open"
				case errors.Is(err, ErrCloseOrderUncertain):
					failureKey = "close_order_uncertain"
				}
				if runtime.combination.LastFailureKey == lastClipHedgeMinNotionalFailureKey {
					failureKey = lastClipHedgeMinNotionalFailureKey
				}
				if failures, ok := s.store.(arbitrageCloseFailureStore); ok {
					if updated, recordErr := failures.RecordArbitrageCloseFailure(
						ctx, runtime.combination.ID, failureKey, sanitizeError(err),
					); recordErr == nil {
						runtime.combination = updated
					} else {
						s.logger.Error("record arbitrage close failure failed",
							"combination_id", runtime.combination.ID, "error", recordErr)
					}
				}
				s.logger.Error("close arbitrage combination failed",
					"combination_id", runtime.combination.ID,
					"failure_key", failureKey, "error", err)
				return false
			}
		}
		return true
	}
	return false
}

func (s *ArbitrageScheduler) evaluate(
	ctx context.Context,
	runtime *arbitrageRuntime,
	source string,
) bool {
	s.evaluations.Add(1)
	if source == "bbo_event" {
		s.bboEvaluations.Add(1)
	}
	if runtime.combination.Status != "running" && runtime.combination.Status != "closing" {
		return true
	}
	if runtime.combination.RuntimeState == "manual_intervention" ||
		runtime.combination.PositionUncertain {
		if !s.canRecoverUnpairedTinyClose(ctx, runtime) {
			if runtime.legA != nil && runtime.legB != nil {
				bboA, errA := runtime.legA.Latest()
				bboB, errB := runtime.legB.Latest()
				if errA == nil && errB == nil {
					s.resumeActiveForIntervention(ctx, runtime, bboA, bboB)
				}
			}
			return false
		}
	}
	if strings.EqualFold(runtime.combination.Status, "closing") &&
		combinationLooksFlat(runtime.combination) {
		return s.finalizeFlattening(ctx, runtime)
	}
	if strings.EqualFold(runtime.combination.Status, "running") &&
		strings.EqualFold(runtime.combination.RunMode, "one_shot") &&
		runtime.combination.OneShotPhase == "exited" {
		return false
	}
	if runtime.legA == nil || runtime.legB == nil {
		return false
	}
	now := time.Now().UTC()
	bboA, errA := runtime.legA.Latest()
	bboB, errB := runtime.legB.Latest()
	if errA != nil || errB != nil {
		s.handleMarketUnavailable(ctx, runtime)
		return false
	}
	runtime.latestA, runtime.latestB = bboA, bboB
	runtime.hasA, runtime.hasB = true, true
	aBid, e1 := decimal.NewFromString(bboA.BidPrice)
	aAsk, e2 := decimal.NewFromString(bboA.AskPrice)
	bBid, e3 := decimal.NewFromString(bboB.BidPrice)
	bAsk, e4 := decimal.NewFromString(bboB.AskPrice)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return false
	}
	ask, bid, ok := executableArbitrageSpreads(
		runtime.combination, aBid, aAsk, bBid, bAsk,
	)
	if !ok {
		s.clearArbitrageValuation(runtime.combination.ID)
		return false
	}
	s.storeArbitrageValuation(
		runtime.combination.ID, aBid, aAsk, bBid, bAsk,
		bboA.ReceiveTimestamp, bboB.ReceiveTimestamp,
	)
	runtime.combination.CurrentAskSpreadBps = ask.String()
	runtime.combination.CurrentBidSpreadBps = bid.String()
	wasStale := runtime.combination.MarketDataStale
	runtime.combination.MarketDataStale = false
	if s.persistMarketSnapshot(ctx, runtime, ask.String(), bid.String(), false, wasStale) {
		return true
	}
	if arbitrageFlattening(runtime.combination) {
		return s.flattenOrFinalizeClosingFrom(
			ctx, runtime, bboA, bboB, aBid, aAsk, bBid, bAsk, source,
		)
	}
	s.maybeMarkOneShotTimeExiting(ctx, runtime, now)
	if arbitrageFlattening(runtime.combination) {
		return s.flattenOrFinalizeClosingFrom(
			ctx, runtime, bboA, bboB, aBid, aAsk, bBid, bAsk, source,
		)
	}
	if s.resumeActive(ctx, runtime, bboA, bboB) {
		return false
	}
	if !s.combinationEnabled(runtime.combination) ||
		runtime.combination.CircuitOpen ||
		runtime.combination.RuntimeState == "manual_intervention" {
		return false
	}
	if runtime.combination.PositionUncertain {
		return false
	}
	if !runtime.combination.NextRetryAt.IsZero() &&
		!runtime.combination.NextRetryAt.Equal(time.Unix(0, 0).UTC()) &&
		now.Before(runtime.combination.NextRetryAt) {
		return false
	}
	if s.rejectStaleSignal(bboA, bboB) {
		return false
	}
	askThreshold, _ := decimal.NewFromString(runtime.combination.AskThresholdBps)
	bidThreshold, _ := decimal.NewFromString(runtime.combination.BidThresholdBps)
	legAMid := aBid.Add(aAsk).Div(decimal.NewFromInt(2))
	legBMid := bBid.Add(bAsk).Div(decimal.NewFromInt(2))
	venueNotional, _, _ :=
		arbitragePositionNotionals(runtime.combination, legAMid, legBMid)
	target, _ := decimal.NewFromString(runtime.combination.TargetNotional)
	remaining := remainingDirectionNotional(
		parseDecimal(runtime.combination.LegABasePosition),
		parseDecimal(runtime.combination.LegBBasePosition),
		target, legAMid, legBMid,
	)
	if strings.EqualFold(runtime.combination.RunMode, "one_shot") &&
		runtime.combination.OneShotPhase == "waiting_exit" {
		return false
	}
	direction := ""
	effect := "open"
	if strings.EqualFold(runtime.combination.RunMode, "one_shot") {
		if remaining.LessThan(decimal.NewFromInt(arbitrageMinOpenNotional)) {
			return false
		}
		direction = runtime.combination.EntryDirection
	} else {
		direction = selectArbitragePositionDirection(
			ask, bid, askThreshold, bidThreshold, venueNotional, target,
		)
		if strings.EqualFold(direction, "bid") {
			effect = "close"
		}
	}
	if direction == "" {
		runtime.lastSignal = ""
		return false
	}
	return s.claimSizedExecution(
		ctx, runtime, bboA, bboB, aBid, aAsk, bBid, bAsk,
		ask, bid, direction, effect, remaining, venueNotional,
	)
}

func (s *ArbitrageScheduler) flattenOrFinalizeClosing(
	ctx context.Context,
	runtime *arbitrageRuntime,
	bboA, bboB marketdata.BBO,
	aBid, aAsk, bBid, bAsk decimal.Decimal,
) bool {
	return s.flattenOrFinalizeClosingFrom(
		ctx, runtime, bboA, bboB, aBid, aAsk, bBid, bAsk, "control",
	)
}

func (s *ArbitrageScheduler) flattenOrFinalizeClosingFrom(
	ctx context.Context,
	runtime *arbitrageRuntime,
	bboA, bboB marketdata.BBO,
	aBid, aAsk, bBid, bAsk decimal.Decimal,
	source string,
) bool {
	runtime.latestA, runtime.latestB = bboA, bboB
	unpairedTiny := strings.EqualFold(runtime.combination.Status, "closing") &&
		runtime.combination.RuntimeState == "manual_intervention" &&
		runtime.combination.ErrorMessage == arbitrageUnpairedLegsManualReason &&
		!runtime.combination.PositionUncertain
	if runtime.combination.PositionUncertain ||
		(runtime.combination.RuntimeState == "manual_intervention" && !unpairedTiny) {
		s.resumeActiveForIntervention(ctx, runtime, bboA, bboB)
		return false
	}
	direction, ownedOK := arbitrageOwnedCloseDirection(runtime.combination)
	tinyOwnedClose := strings.EqualFold(runtime.combination.Status, "closing") &&
		s.closeFlattenTinyOwnedPrecheck(ctx, runtime.combination, bboA, bboB)
	tinyOwnedCircuitClose := tinyOwnedClose && runtime.combination.CircuitOpen
	allowInterventionResume := unpairedTiny || tinyOwnedCircuitClose
	if allowInterventionResume &&
		s.resumeActiveForIntervention(ctx, runtime, bboA, bboB) {
		return false
	}
	if !runtime.combination.NextRetryAt.IsZero() &&
		!runtime.combination.NextRetryAt.Equal(time.Unix(0, 0).UTC()) &&
		runtime.combination.NextRetryAt.After(time.Now().UTC()) {
		return false
	}
	if !allowInterventionResume && s.resumeActive(ctx, runtime, bboA, bboB) {
		return false
	}
	if combinationLooksFlat(runtime.combination) {
		return s.finalizeFlattening(ctx, runtime)
	}
	if s.rejectStaleSignal(bboA, bboB) {
		return false
	}
	if tinyOwnedClose {
		return s.closeCombination(ctx, runtime)
	}
	venueDirection, venueOK := arbitrageVenueReduceDirection(runtime.combination)
	if ownedOK {
		if !venueOK || direction != venueDirection {
			s.markFlattenManualIntervention(
				ctx, runtime, "venue net position cannot reduce the combination-owned position",
			)
			return false
		}
		closeableBase := decimal.Min(
			arbitrageOwnedCloseableBase(runtime.combination),
			arbitrageCloseableBase(runtime.combination),
		)
		if !closeableBase.IsPositive() {
			s.markFlattenManualIntervention(
				ctx, runtime, "no venue-reducible combination-owned paired position",
			)
			return false
		}
		ask, _ := decimal.NewFromString(runtime.combination.CurrentAskSpreadBps)
		bid, _ := decimal.NewFromString(runtime.combination.CurrentBidSpreadBps)
		return s.claimSizedExecution(
			ctx, runtime, bboA, bboB, aBid, aAsk, bBid, bAsk,
			ask, bid, direction, "close", decimal.Zero, decimal.Zero,
		)
	}
	if oneShotRunningExiting(runtime.combination) {
		return s.handleOneShotExitingLeftover(ctx, runtime, bboA, bboB, source)
	}
	s.markFlattenManualIntervention(
		ctx, runtime, arbitrageUnpairedLegsManualReason,
	)
	return false
}

func (s *ArbitrageScheduler) finalizeFlattening(
	ctx context.Context,
	runtime *arbitrageRuntime,
) bool {
	if !combinationLooksFlat(runtime.combination) {
		return false
	}
	if strings.EqualFold(runtime.combination.Status, "closing") {
		return s.closeCombination(ctx, runtime)
	}
	if !strings.EqualFold(runtime.combination.RunMode, "one_shot") ||
		runtime.combination.OneShotPhase != "exiting" {
		return false
	}
	store, ok := s.store.(arbitrageOneShotStore)
	if !ok {
		return false
	}
	updated, applied, err := store.MarkOneShotExited(
		ctx, runtime.combination.ID, runtime.combination.Version,
	)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Error("mark one-shot exited failed",
				"combination_id", runtime.combination.ID, "error", err)
		}
		return false
	}
	if applied {
		runtime.combination = updated
	}
	return false
}

func (s *ArbitrageScheduler) markFlattenManualIntervention(
	ctx context.Context,
	runtime *arbitrageRuntime,
	reason string,
) {
	if runtime.combination.RuntimeState == "manual_intervention" ||
		runtime.combination.PositionUncertain {
		return
	}
	item := runtime.combination
	item.RuntimeState = "manual_intervention"
	item.ErrorMessage = reason
	updated, err := s.store.UpdateArbitrageCombinationRuntime(ctx, item)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Error("mark flatten manual intervention failed",
				"combination_id", item.ID, "error", err)
		}
		return
	}
	runtime.combination = updated
	_ = s.store.AppendArbitrageEvent(
		ctx, item.ID, "", "flatten_manual_intervention",
		map[string]any{"reason": reason},
	)
}

func (s *ArbitrageScheduler) closeFlattenTinyOwnedPrecheck(
	ctx context.Context,
	combination ArbitrageCombination,
	bboA, bboB marketdata.BBO,
) bool {
	if s == nil || s.catalog == nil {
		return false
	}
	instrumentA, errA := s.catalog.Get(ctx, combination.LegA.InstrumentID)
	instrumentB, errB := s.catalog.Get(ctx, combination.LegB.InstrumentID)
	if errA != nil || errB != nil {
		return false
	}
	stale := s.signalBBOStale
	if stale <= 0 {
		stale = defaultSignalBBOStale
	}
	return closeFlattenTinyOwnedPositions(closeFlattenTinyOwnedInput{
		Combination:    combination,
		InstrumentA:    instrumentA,
		InstrumentB:    instrumentB,
		BBOA:           bboA,
		BBOB:           bboB,
		Now:            time.Now().UTC(),
		RequireOrders:  false,
		SignalBBOStale: stale,
	})
}

func (s *ArbitrageScheduler) canRecoverUnpairedTinyClose(
	ctx context.Context,
	runtime *arbitrageRuntime,
) bool {
	if runtime == nil || runtime.combination.PositionUncertain {
		return false
	}
	if !strings.EqualFold(runtime.combination.Status, "closing") ||
		runtime.combination.RuntimeState != "manual_intervention" ||
		runtime.combination.ErrorMessage != arbitrageUnpairedLegsManualReason {
		return false
	}
	if _, ownedOK := arbitrageOwnedCloseDirection(runtime.combination); ownedOK {
		return false
	}
	if runtime.legA == nil || runtime.legB == nil {
		return false
	}
	bboA, errA := runtime.legA.Latest()
	bboB, errB := runtime.legB.Latest()
	if errA != nil || errB != nil {
		return false
	}
	return s.closeFlattenTinyOwnedPrecheck(ctx, runtime.combination, bboA, bboB)
}

func (s *ArbitrageScheduler) logCloseWaitingSnapshot(combinationID string) {
	if s == nil || s.logger == nil || combinationID == "" {
		return
	}
	now := time.Now()
	if prev, ok := s.closeWaitingLogAt.Load(combinationID); ok {
		if logged, ok := prev.(time.Time); ok && now.Sub(logged) < closeWaitingSnapshotLogInterval {
			return
		}
	}
	s.closeWaitingLogAt.Store(combinationID, now)
	s.logger.Info("arbitrage close waiting for position snapshot",
		"combination_id", combinationID)
}

func (s *ArbitrageScheduler) claimSizedExecution(
	ctx context.Context,
	runtime *arbitrageRuntime,
	bboA, bboB marketdata.BBO,
	aBid, aAsk, bBid, bAsk, ask, bid decimal.Decimal,
	direction, effect string,
	remaining, venueNotional decimal.Decimal,
) bool {
	if s.catalog == nil {
		return false
	}
	instrumentA, instrumentAErr := s.catalog.Get(ctx, runtime.combination.LegA.InstrumentID)
	instrumentB, instrumentBErr := s.catalog.Get(ctx, runtime.combination.LegB.InstrumentID)
	if instrumentAErr != nil || instrumentBErr != nil {
		return false
	}
	aSide, bSide, ok := arbitrageLegSides(direction)
	if !ok {
		return false
	}
	closeableBase := arbitrageCloseableBase(runtime.combination)
	if arbitrageFlattening(runtime.combination) {
		closeableBase = decimal.Min(
			closeableBase,
			arbitrageOwnedCloseableBase(runtime.combination),
		)
	}
	closeable := decimal.Zero
	if closeableBase.IsPositive() {
		closeable = decimal.Max(
			closeableBase.Mul(parsePositiveDecimal(bboPriceForSide(bboA, aSide))),
			closeableBase.Mul(parsePositiveDecimal(bboPriceForSide(bboB, bSide))),
		)
	}
	plan, reason, ok := sizeArbitrageExecution(
		runtime.combination, instrumentA, instrumentB, bboA, bboB,
		direction, effect, remaining, closeable, s.sizeRand(),
	)
	if !ok {
		if strings.EqualFold(effect, "close") && closeable.IsZero() {
			if arbitrageFlattening(runtime.combination) {
				if combinationLooksFlat(runtime.combination) {
					return s.finalizeFlattening(ctx, runtime)
				}
				s.markFlattenManualIntervention(
					ctx, runtime, "no venue-reducible combination-owned paired position",
				)
				return false
			}
			return s.closeCombination(ctx, runtime)
		}
		if strings.EqualFold(effect, "close") && closeable.IsPositive() &&
			arbitrageFlattening(runtime.combination) {
			if failures, ok := s.store.(arbitrageCloseFailureStore); ok {
				if updated, recordErr := failures.RecordArbitrageCloseFailure(
					ctx, runtime.combination.ID, "close_size_unexecutable", reason,
				); recordErr == nil {
					runtime.combination = updated
				} else {
					s.logger.Error("record arbitrage close failure failed",
						"combination_id", runtime.combination.ID, "error", recordErr)
				}
			}
			_ = s.store.AppendArbitrageEvent(
				ctx, runtime.combination.ID, "", "close_size_unexecutable",
				map[string]any{"reason": reason},
			)
			s.logger.Error("close size is unexecutable",
				"combination_id", runtime.combination.ID, "reason", reason)
		}
		return false
	}
	s.triggers.Add(1)
	if s.dryRun {
		if runtime.lastSignal != direction {
			_ = s.store.AppendArbitrageEvent(ctx, runtime.combination.ID, "", "dry_run_trigger", map[string]any{
				"direction": direction, "askSpreadBps": ask.String(), "bidSpreadBps": bid.String(),
				"positionEffect": plan.Effect, "requestedNotional": plan.RequestedNotional.String(),
				"randomNotional":   plan.RandomNotional.String(),
				"hedgeCapNotional": plan.HedgeCapNotional.String(),
				"legANotional":     plan.LegANotional.String(),
				"legBNotional":     plan.LegBNotional.String(),
			})
			s.logger.Info("arbitrage dry-run trigger",
				"combination_id", runtime.combination.ID, "direction", direction,
				"ask_spread_bps", ask.String(), "bid_spread_bps", bid.String())
		}
		runtime.lastSignal = direction
		return false
	}
	res, ok := s.tryAcquireExecutionResources(
		runtime, workPermitNewExecution, true, true,
	)
	if !ok {
		return false
	}
	if s.rejectStaleSignal(bboA, bboB) {
		s.releaseExecutionResources(runtime, runtime.combination, res)
		return false
	}
	execution, claimed, err := s.store.ClaimArbitrageExecution(ctx, ArbitrageExecution{
		ID: uuid.NewString(), CombinationID: runtime.combination.ID,
		Direction: direction, Status: "claimed",
		TriggerAskSpread: ask.String(), TriggerBidSpread: bid.String(),
		TriggerLegABid: aBid.String(), TriggerLegAAsk: aAsk.String(),
		TriggerLegBBid: bBid.String(), TriggerLegBAsk: bAsk.String(),
		TargetBaseQuantity: plan.TargetBaseQuantity.String(),
		RequestedNotional:  plan.RequestedNotional.String(),
		PositionEffect:     plan.Effect, ReduceOnly: plan.ReduceOnly,
		LastCloseClip:      plan.LastClip,
		LegAFilledQuantity: "0", LegBFilledQuantity: "0",
		DeltaNotional: "0",
	})
	if err != nil || !claimed || s.runner == nil {
		s.releaseExecutionResources(runtime, runtime.combination, res)
		return false
	}
	s.claims.Add(1)
	_ = s.store.AppendArbitrageEvent(ctx, runtime.combination.ID, execution.ID, "sized", map[string]any{
		"randomNotional":    plan.RandomNotional.String(),
		"hedgeCapNotional":  plan.HedgeCapNotional.String(),
		"legANotional":      plan.LegANotional.String(),
		"legBNotional":      plan.LegBNotional.String(),
		"requestedNotional": plan.RequestedNotional.String(),
	})
	now := time.Now().UTC()
	s.logger.Info("arbitrage trigger claimed",
		"combination_id", runtime.combination.ID, "execution_id", execution.ID,
		"direction", direction, "ask_spread_bps", ask.String(),
		"bid_spread_bps", bid.String(),
		"leg_a_bbo_age_ms", now.Sub(bboA.ReceiveTimestamp).Milliseconds(),
		"leg_b_bbo_age_ms", now.Sub(bboB.ReceiveTimestamp).Milliseconds(),
	)
	snapshot := runtime.combination
	s.ensureRuntime(runtime)
	go func(item ArbitrageCombination, res *executionResources) {
		defer s.notifyExecutionComplete(runtime)
		defer s.releaseExecutionResources(runtime, item, res)
		s.runner.Execute(ctx, item, execution, bboA, bboB, res.permit)
	}(snapshot, res)
	return false
}

func (s *ArbitrageScheduler) sizeRand() arbitrageNotionalRand {
	if s != nil && s.notionalRand != nil {
		return s.notionalRand
	}
	return cryptoNotionalRand{}
}

func (s *ArbitrageScheduler) maybeMarkOneShotTimeExiting(
	ctx context.Context,
	runtime *arbitrageRuntime,
	now time.Time,
) {
	if !strings.EqualFold(runtime.combination.RunMode, "one_shot") ||
		runtime.combination.OneShotPhase != "waiting_exit" ||
		runtime.combination.Status != "running" {
		return
	}
	if runtime.combination.ScheduledExitAt.IsZero() ||
		runtime.combination.ScheduledExitAt.Equal(time.Unix(0, 0).UTC()) ||
		now.Before(runtime.combination.ScheduledExitAt) {
		return
	}
	store, ok := s.store.(arbitrageOneShotStore)
	if !ok {
		return
	}
	updated, applied, err := store.MarkOneShotExiting(
		ctx, runtime.combination.ID, runtime.combination.Version, "time", nil,
	)
	if err == nil && applied {
		runtime.combination = updated
	}
}

func (s *ArbitrageScheduler) maybeMarkOneShotWaitingExit(
	ctx context.Context,
	runtime *arbitrageRuntime,
	remaining decimal.Decimal,
) {
	if !strings.EqualFold(runtime.combination.RunMode, "one_shot") ||
		runtime.combination.OneShotPhase != "building_target" ||
		runtime.combination.Status != "running" {
		return
	}
	if remaining.GreaterThanOrEqual(decimal.NewFromInt(arbitrageMinOpenNotional)) {
		return
	}
	if runtime.combination.PositionUncertain || runtime.executionRunning.Load() ||
		runtime.combination.CircuitOpen ||
		runtime.combination.RuntimeState == "manual_intervention" {
		return
	}
	if runtime.combination.LastPositionReconciledAt.IsZero() ||
		runtime.combination.LastPositionReconciledAt.Equal(time.Unix(0, 0).UTC()) {
		return
	}
	store, ok := s.store.(arbitrageOneShotStore)
	if !ok {
		return
	}
	legA := parseDecimal(runtime.combination.LegABasePosition)
	legB := parseDecimal(runtime.combination.LegBBasePosition)
	carry := parseDecimal(runtime.combination.CarryBaseQuantity)
	if carry.IsZero() {
		liveOrder, unreconciled, liveExecution, gateErr := store.ArbitrageDustOrderGate(
			ctx, runtime.combination.ID, runtime.combination.LastPositionReconciledAt,
		)
		if gateErr != nil || liveOrder || unreconciled || liveExecution {
			return
		}
		if legA.IsZero() || legB.IsZero() || legA.Sign() == legB.Sign() {
			return
		}
		s.applyOneShotWaitingExit(ctx, runtime, store, false)
		return
	}
	if s.evaluateRuntimeOneShotDust(
		ctx, runtime, remaining, runtime.latestA, runtime.latestB, "building_target",
	) != oneShotDustAccepted {
		return
	}
	s.applyOneShotWaitingExit(ctx, runtime, store, true)
}

func (s *ArbitrageScheduler) applyOneShotWaitingExit(
	ctx context.Context,
	runtime *arbitrageRuntime,
	store arbitrageOneShotStore,
	withDust bool,
) {
	var scheduled time.Time
	if runtime.combination.ExitPolicy == "time" && runtime.combination.ExitAfterSeconds > 0 {
		scheduled = time.Now().UTC().Add(time.Duration(runtime.combination.ExitAfterSeconds) * time.Second)
	}
	updated, applied, err := store.MarkOneShotWaitingExit(
		ctx, runtime.combination.ID, runtime.combination.Version, scheduled,
	)
	if err != nil || !applied {
		return
	}
	runtime.combination = updated
	if withDust {
		_ = s.store.AppendArbitrageEvent(
			ctx, runtime.combination.ID, "", "one_shot_target_reached_with_dust",
			map[string]any{
				"legABasePosition":  updated.LegABasePosition,
				"legBBasePosition":  updated.LegBBasePosition,
				"carryBaseQuantity": updated.CarryBaseQuantity,
			},
		)
	}
}

func (s *ArbitrageScheduler) oneShotDustRetryInterval() time.Duration {
	interval := s.controlInterval
	if interval < time.Second {
		interval = time.Second
	}
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	return interval
}

func (s *ArbitrageScheduler) maybePromoteOneShotWaitingExit(
	ctx context.Context,
	runtime *arbitrageRuntime,
	force bool,
) {
	if runtime == nil || runtime.legA == nil || runtime.legB == nil {
		return
	}
	if !strings.EqualFold(runtime.combination.RunMode, "one_shot") ||
		runtime.combination.OneShotPhase != "building_target" ||
		runtime.combination.Status != "running" {
		return
	}
	bboA, errA := runtime.legA.Latest()
	bboB, errB := runtime.legB.Latest()
	if errA != nil || errB != nil {
		return
	}
	runtime.latestA, runtime.latestB = bboA, bboB
	runtime.hasA, runtime.hasB = true, true
	aBid, e1 := decimal.NewFromString(bboA.BidPrice)
	aAsk, e2 := decimal.NewFromString(bboA.AskPrice)
	bBid, e3 := decimal.NewFromString(bboB.BidPrice)
	bAsk, e4 := decimal.NewFromString(bboB.AskPrice)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return
	}
	legAMid := aBid.Add(aAsk).Div(decimal.NewFromInt(2))
	legBMid := bBid.Add(bAsk).Div(decimal.NewFromInt(2))
	target, _ := decimal.NewFromString(runtime.combination.TargetNotional)
	remaining := remainingDirectionNotional(
		parseDecimal(runtime.combination.LegABasePosition),
		parseDecimal(runtime.combination.LegBBasePosition),
		target, legAMid, legBMid,
	)
	if remaining.GreaterThanOrEqual(decimal.NewFromInt(arbitrageMinOpenNotional)) {
		return
	}
	now := time.Now()
	if !force && !runtime.lastDustCheckAt.IsZero() &&
		now.Sub(runtime.lastDustCheckAt) < s.oneShotDustRetryInterval() {
		return
	}
	runtime.lastDustCheckAt = now
	s.maybeMarkOneShotWaitingExit(ctx, runtime, remaining)
}

func (s *ArbitrageScheduler) evaluateRuntimeOneShotDust(
	ctx context.Context,
	runtime *arbitrageRuntime,
	remaining decimal.Decimal,
	bboA, bboB marketdata.BBO,
	phase string,
) oneShotDustVerdict {
	store, ok := s.store.(arbitrageOneShotStore)
	if !ok {
		return oneShotDustRetry
	}
	liveOrder, unreconciled, liveExecution, gateErr := store.ArbitrageDustOrderGate(
		ctx, runtime.combination.ID, runtime.combination.LastPositionReconciledAt,
	)
	var instrumentA, instrumentB Instrument
	var instrumentErr error
	if s.catalog == nil {
		instrumentErr = ErrInstrumentUnavailable
	} else {
		var errA, errB error
		instrumentA, errA = s.catalog.Get(ctx, runtime.combination.LegA.InstrumentID)
		instrumentB, errB = s.catalog.Get(ctx, runtime.combination.LegB.InstrumentID)
		if errA != nil {
			instrumentErr = errA
		} else if errB != nil {
			instrumentErr = errB
		}
	}
	priceA, priceB := carryClosePrices(
		parseDecimal(runtime.combination.CarryBaseQuantity), bboA, bboB,
	)
	if phase == "exiting" {
		legA := parseDecimal(runtime.combination.LegABasePosition)
		legB := parseDecimal(runtime.combination.LegBBasePosition)
		if !legA.IsZero() {
			priceA = closeFlattenOwnedExecutablePrice(legA, bboA)
			priceB = decimal.Zero
		} else if !legB.IsZero() {
			priceB = closeFlattenOwnedExecutablePrice(legB, bboB)
			priceA = decimal.Zero
		}
	}
	return evaluateOneShotDust(oneShotDustInput{
		Combination:      runtime.combination,
		Remaining:        remaining,
		Phase:            phase,
		InstrumentA:      instrumentA,
		InstrumentB:      instrumentB,
		InstrumentErr:    instrumentErr,
		PriceA:           priceA,
		PriceB:           priceB,
		LiveOrder:        liveOrder,
		Unreconciled:     unreconciled,
		LiveExecution:    liveExecution,
		GateErr:          gateErr,
		ExecutionRunning: runtime.executionRunning.Load(),
	})
}

func (s *ArbitrageScheduler) handleOneShotExitingLeftover(
	ctx context.Context,
	runtime *arbitrageRuntime,
	bboA, bboB marketdata.BBO,
	source string,
) bool {
	_, residualNotional, single := oneShotLeftoverResidual(runtime.combination, bboA, bboB)
	if source == "bbo_event" {
		if single && residualNotional.GreaterThanOrEqual(decimal.NewFromInt(arbitrageMinOpenNotional)) {
			s.markFlattenManualIntervention(
				ctx, runtime, arbitrageUnpairedLegsManualReason,
			)
		}
		return false
	}
	verdict := s.evaluateRuntimeOneShotDust(
		ctx, runtime, decimal.Zero, bboA, bboB, "exiting",
	)
	switch verdict {
	case oneShotDustAccepted:
		s.markOneShotExitedWithDust(ctx, runtime)
	case oneShotDustUnsafe:
		s.markFlattenManualIntervention(
			ctx, runtime, arbitrageUnpairedLegsManualReason,
		)
	}
	return false
}

func (s *ArbitrageScheduler) markOneShotExitedWithDust(
	ctx context.Context,
	runtime *arbitrageRuntime,
) {
	store, ok := s.store.(arbitrageOneShotStore)
	if !ok {
		return
	}
	updated, applied, err := store.MarkOneShotExitedWithDust(
		ctx,
		runtime.combination.ID,
		runtime.combination.Version,
		runtime.combination.LegABasePosition,
		runtime.combination.LegBBasePosition,
		runtime.combination.CarryBaseQuantity,
	)
	if err != nil {
		if ctx.Err() == nil && s.logger != nil {
			s.logger.Error("mark one-shot exited with dust failed",
				"combination_id", runtime.combination.ID, "error", err)
		}
		return
	}
	if applied {
		runtime.combination = updated
	}
}

func (s *ArbitrageScheduler) combinationEnabled(item ArbitrageCombination) bool {
	return exchangeEnabled(s.enabledVenues, item.LegA.Exchange) &&
		exchangeEnabled(s.enabledVenues, item.LegB.Exchange)
}

func (s *ArbitrageScheduler) handleMarketUnavailable(
	ctx context.Context,
	runtime *arbitrageRuntime,
) {
	now := time.Now().UTC()
	s.clearArbitrageValuation(runtime.combination.ID)
	s.staleBBO.Add(1)
	force := !runtime.combination.MarketDataStale
	if shouldPersistMarketSnapshot(force, runtime.lastPersist, now) {
		runtime.combination.MarketDataStale = true
		s.persistMarketSnapshot(
			ctx, runtime,
			runtime.combination.CurrentAskSpreadBps,
			runtime.combination.CurrentBidSpreadBps, true, true,
		)
	}
	s.resumeWithoutMarketData(ctx, runtime)
}

func (s *ArbitrageScheduler) notifyExecutionComplete(runtime *arbitrageRuntime) {
	select {
	case runtime.completion <- struct{}{}:
	default:
	}
}

func (s *ArbitrageScheduler) storeArbitrageValuation(
	combinationID string,
	legABid, legAAsk, legBBid, legBAsk decimal.Decimal,
	legAUpdatedAt, legBUpdatedAt time.Time,
) {
	legAMid := legABid.Add(legAAsk).Div(decimal.NewFromInt(2))
	legBMid := legBBid.Add(legBAsk).Div(decimal.NewFromInt(2))
	if !legAMid.IsPositive() || !legBMid.IsPositive() {
		s.clearArbitrageValuation(combinationID)
		return
	}
	if legAUpdatedAt.IsZero() {
		legAUpdatedAt = time.Now().UTC()
	}
	if legBUpdatedAt.IsZero() {
		legBUpdatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	s.valuations[combinationID] = ArbitrageValuation{
		LegAMid: legAMid.String(), LegBMid: legBMid.String(),
		LegAUpdatedAt: legAUpdatedAt, LegBUpdatedAt: legBUpdatedAt,
	}
	s.mu.Unlock()
}

func (s *ArbitrageScheduler) clearArbitrageValuation(combinationID string) {
	s.mu.Lock()
	delete(s.valuations, combinationID)
	s.mu.Unlock()
}

func (s *ArbitrageScheduler) ArbitrageValuation(
	combinationID string,
) (ArbitrageValuation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.valuations[combinationID]
	return value, ok
}

func (s *ArbitrageScheduler) resumeWithoutMarketData(
	ctx context.Context,
	runtime *arbitrageRuntime,
) bool {
	if runtime.executionRunning.Load() || runtime.recoveryRunning.Load() || s.runner == nil {
		return runtime.executionRunning.Load() || runtime.recoveryRunning.Load()
	}
	execution, err := s.store.GetActiveArbitrageExecution(
		ctx, runtime.combination.ID,
	)
	if err != nil {
		return !errors.Is(err, ErrNotFound)
	}
	res, ok := s.tryAcquireExecutionResources(
		runtime, workPermitUrgent, false, false,
	)
	if !ok {
		return true
	}
	runtime.recoveryRunning.Store(true)
	snapshot := runtime.combination
	s.ensureRuntime(runtime)
	go func(item ArbitrageCombination, execution ArbitrageExecution, res *executionResources) {
		defer s.notifyExecutionComplete(runtime)
		defer s.releaseExecutionResources(runtime, item, res)
		defer runtime.recoveryRunning.Store(false)
		if recoverErr := s.runner.RecoverWithoutMarketData(
			ctx, item, execution,
		); recoverErr != nil {
			s.logger.Error(
				"arbitrage stale-market recovery failed",
				"combination_id", item.ID,
				"execution_id", execution.ID,
				"error", sanitizeError(recoverErr),
			)
		}
	}(snapshot, execution, res)
	return true
}

type ArbitrageSchedulerStats struct {
	ActiveCombinations  uint64
	ActiveAccounts      uint64
	ActiveVenues        uint64
	Triggers            uint64
	Claims              uint64
	StaleBBOReads       uint64
	CapacityRejected    uint64
	BBOWakeups          uint64
	BBOEvaluations      uint64
	Evaluations         uint64
	ControlWakeups      uint64
	StaleWakeups        uint64
	RetryWakeups        uint64
	ControlProbes       uint64
	FullReloads         uint64
	SnapshotWrites      uint64
	SignalStaleRejected uint64
	ExecutionSlotsInUse uint64
	WorkPermitsInUse    uint64
}

func (s *ArbitrageScheduler) Stats() ArbitrageSchedulerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ArbitrageSchedulerStats{
		ActiveCombinations: uint64(len(s.runtimes)),
		ActiveAccounts:     uint64(len(s.accountUse)), ActiveVenues: uint64(len(s.venueUse)),
		Triggers: s.triggers.Load(), Claims: s.claims.Load(),
		StaleBBOReads: s.staleBBO.Load(), CapacityRejected: s.rejected.Load(),
		BBOWakeups: s.bboWakeups.Load(), BBOEvaluations: s.bboEvaluations.Load(),
		Evaluations:    s.evaluations.Load(),
		ControlWakeups: s.controlWakeups.Load(), StaleWakeups: s.staleWakeups.Load(),
		RetryWakeups:  s.retryWakeups.Load(),
		ControlProbes: s.controlProbes.Load(), FullReloads: s.fullReloads.Load(),
		SnapshotWrites:      s.snapshotWrites.Load(),
		SignalStaleRejected: s.signalStaleRejected.Load(),
		ExecutionSlotsInUse: uint64(s.slotsInUse.Load()),
		WorkPermitsInUse:    uint64(s.workPermits.InUse()),
	}
}

func applyArbitrageMarketSnapshot(
	item *ArbitrageCombination,
	snapshot arbitrageMarketSnapshot,
) bool {
	previousVersion := item.Version
	item.CurrentAskSpreadBps = snapshot.CurrentAskSpreadBps
	item.CurrentBidSpreadBps = snapshot.CurrentBidSpreadBps
	item.MarketDataStale = snapshot.MarketDataStale
	item.RuntimeState = snapshot.RuntimeState
	item.Status = snapshot.Status
	item.UpdatedAt = snapshot.UpdatedAt
	if snapshot.Version == previousVersion {
		item.Version = snapshot.Version
		return false
	}
	return true
}

func (s *ArbitrageScheduler) resumeActive(
	ctx context.Context,
	runtime *arbitrageRuntime,
	bboA, bboB marketdata.BBO,
) bool {
	return s.resumeActiveWithState(ctx, runtime, bboA, bboB, false)
}

func (s *ArbitrageScheduler) resumeActiveForIntervention(
	ctx context.Context,
	runtime *arbitrageRuntime,
	bboA, bboB marketdata.BBO,
) bool {
	return s.resumeActiveWithState(ctx, runtime, bboA, bboB, true)
}

func (s *ArbitrageScheduler) resumeActiveWithState(
	ctx context.Context,
	runtime *arbitrageRuntime,
	bboA, bboB marketdata.BBO,
	allowIntervention bool,
) bool {
	if runtime.executionRunning.Load() {
		return true
	}
	if !allowIntervention && arbitrageActiveResumeBlocked(runtime.combination) {
		return true
	}
	execution, err := s.store.GetActiveArbitrageExecution(ctx, runtime.combination.ID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false
		}
		return true
	}
	now := time.Now().UTC()
	if !runtime.combination.NextRetryAt.IsZero() &&
		!runtime.combination.NextRetryAt.Equal(time.Unix(0, 0).UTC()) &&
		now.Before(runtime.combination.NextRetryAt) {
		return true
	}
	if s.runner == nil {
		return true
	}
	res, ok := s.tryAcquireExecutionResources(
		runtime, workPermitUrgent, true, false,
	)
	if !ok {
		return true
	}
	snapshot := runtime.combination
	s.ensureRuntime(runtime)
	go func(item ArbitrageCombination, execution ArbitrageExecution, res *executionResources) {
		defer s.notifyExecutionComplete(runtime)
		defer s.releaseExecutionResources(runtime, item, res)
		s.runner.Execute(ctx, item, execution, bboA, bboB, res.permit)
	}(snapshot, execution, res)
	return true
}

func arbitrageActiveResumeBlocked(item ArbitrageCombination) bool {
	return item.CircuitOpen ||
		item.PositionUncertain ||
		item.RuntimeState == "manual_intervention" ||
		item.RuntimeState == "position_uncertain"
}

func shouldPersistMarketSnapshot(force bool, lastPersist, now time.Time) bool {
	return force || lastPersist.IsZero() || now.Sub(lastPersist) >= marketSnapshotPersistInterval
}

func snapshotNeedsUrgent(runtime *arbitrageRuntime, wasStale, stale bool) bool {
	if runtime == nil {
		return false
	}
	if wasStale || stale || runtime.combination.MarketDataStale {
		return true
	}
	if strings.EqualFold(runtime.combination.Status, "closing") {
		return true
	}
	switch runtime.combination.RuntimeState {
	case "backoff", "hedge_deferred_dust", "position_uncertain", "manual_intervention":
		return true
	}
	return runtime.lastPersist.IsZero()
}

func (s *ArbitrageScheduler) persistMarketSnapshot(
	ctx context.Context,
	runtime *arbitrageRuntime,
	ask, bid string,
	stale, wasStale bool,
) bool {
	now := time.Now().UTC()
	if !shouldPersistMarketSnapshot(wasStale || stale, runtime.lastPersist, now) {
		return false
	}
	urgent := snapshotNeedsUrgent(runtime, wasStale, stale)
	if !urgent &&
		!runtime.nextSnapshotAttemptAt.IsZero() &&
		now.Before(runtime.nextSnapshotAttemptAt) {
		return false
	}
	if !urgent && runtime.snapshotInFlightSeq != 0 {
		return false
	}
	runtime.snapshotSequence++
	seq := runtime.snapshotSequence
	useBatch := !urgent &&
		s.snapshotBatcher != nil &&
		s.batchersStarted.Load()
	if useBatch {
		if _, ok := s.store.(arbitrageSnapshotBatchStore); !ok {
			useBatch = false
		}
	}
	if useBatch {
		expectedAt := runtime.lastSnapshotUpdatedAt
		if expectedAt.IsZero() {
			expectedAt = runtime.combination.UpdatedAt
		}
		runtime.snapshotInFlightSeq = seq
		s.snapshotBatcher.Submit(snapshotRequest{
			write: arbitrageMarketSnapshotWrite{
				CombinationID:     runtime.combination.ID,
				AskSpread:         ask,
				BidSpread:         bid,
				ExpectedVersion:   runtime.combination.Version,
				ExpectedUpdatedAt: expectedAt,
				Sequence:          seq,
			},
			runtime:    runtime,
			generation: runtime.generation,
		})
		return false
	}
	updated, err := s.store.UpdateArbitrageMarketSnapshot(
		ctx, runtime.combination.ID, ask, bid, stale,
	)
	if err != nil {
		if !urgent {
			runtime.nextSnapshotAttemptAt = now.Add(marketSnapshotPersistInterval)
		}
		return false
	}
	requiresReload := applyArbitrageMarketSnapshot(&runtime.combination, updated)
	s.snapshotWrites.Add(1)
	runtime.lastPersist = now
	runtime.lastSnapshotUpdatedAt = updated.UpdatedAt
	runtime.lastAppliedSnapshotSeq = seq
	runtime.nextSnapshotAttemptAt = time.Time{}
	if requiresReload {
		stop, refreshed := s.refreshCombination(ctx, runtime)
		if stop {
			return true
		}
		if !refreshed {
			return false
		}
	}
	return false
}

func takeSnapshotNotice(runtime *arbitrageRuntime) *snapshotFlushNotice {
	if runtime == nil {
		return nil
	}
	value := runtime.snapshotNotice.Swap((*snapshotFlushNotice)(nil))
	notice, _ := value.(*snapshotFlushNotice)
	return notice
}

func (s *ArbitrageScheduler) applySnapshotReceipt(
	ctx context.Context,
	runtime *arbitrageRuntime,
) bool {
	notice := takeSnapshotNotice(runtime)
	if notice == nil || notice.generation != runtime.generation {
		return false
	}
	now := time.Now().UTC()
	if runtime.snapshotInFlightSeq == notice.seq {
		runtime.snapshotInFlightSeq = 0
	}
	if notice.seq < runtime.lastAppliedSnapshotSeq {
		return false
	}
	if notice.failed {
		runtime.nextSnapshotAttemptAt = now.Add(marketSnapshotPersistInterval)
		return false
	}
	if notice.applied {
		runtime.combination.CurrentAskSpreadBps = notice.item.AskSpread
		runtime.combination.CurrentBidSpreadBps = notice.item.BidSpread
		runtime.combination.UpdatedAt = notice.item.UpdatedAt
		runtime.lastPersist = now
		runtime.lastSnapshotUpdatedAt = notice.item.UpdatedAt
		runtime.lastAppliedSnapshotSeq = notice.seq
		runtime.nextSnapshotAttemptAt = time.Time{}
		s.snapshotWrites.Add(1)
		return false
	}
	runtime.nextSnapshotAttemptAt = now.Add(marketSnapshotPersistInterval)
	if notice.item.Version != runtime.combination.Version ||
		notice.item.Status != runtime.combination.Status ||
		notice.item.RuntimeState != runtime.combination.RuntimeState {
		stop, _ := s.reloadCombination(ctx, runtime)
		if !stop {
			runtime.lastSnapshotUpdatedAt = runtime.combination.UpdatedAt
		}
		return stop
	}
	runtime.lastSnapshotUpdatedAt = notice.item.UpdatedAt
	return false
}

func (s *ArbitrageScheduler) registerRuntimeLifecycle(item ArbitrageCombination) {
	if s == nil || s.lifecycle == nil {
		return
	}
	s.lifecycle.RegisterCombination(item)
}

func (s *ArbitrageScheduler) unregisterRuntimeLifecycle(id string) {
	if s == nil || s.lifecycle == nil || id == "" {
		return
	}
	s.lifecycle.UnregisterCombination(id)
}

type executionResources struct {
	permit   *executionWorkPermit
	takeSlot bool
}

func (s *ArbitrageScheduler) tryAcquireSlot() bool {
	if s == nil || s.executionSlots == nil {
		return false
	}
	select {
	case s.executionSlots <- struct{}{}:
		s.slotsInUse.Add(1)
		return true
	default:
		return false
	}
}

func (s *ArbitrageScheduler) releaseSlot() {
	if s == nil || s.executionSlots == nil {
		return
	}
	<-s.executionSlots
	s.slotsInUse.Add(-1)
}

func (s *ArbitrageScheduler) tryAcquireExecutionResources(
	runtime *arbitrageRuntime,
	class workPermitClass,
	takeSlot bool,
	countRejected bool,
) (*executionResources, bool) {
	if s == nil || runtime == nil || s.workPermits == nil ||
		s.accountUse == nil || s.venueUse == nil {
		return nil, false
	}
	if takeSlot && !s.tryAcquireSlot() {
		if countRejected {
			s.rejected.Add(1)
		}
		return nil, false
	}
	permit := newExecutionWorkPermit(s.workPermits)
	if !permit.TryAcquire(class) {
		if takeSlot {
			s.releaseSlot()
		}
		if countRejected {
			s.rejected.Add(1)
		}
		return nil, false
	}
	if !s.acquireExecutionCapacity(runtime.combination) {
		permit.Close()
		if takeSlot {
			s.releaseSlot()
		}
		if countRejected {
			s.rejected.Add(1)
		}
		return nil, false
	}
	if !runtime.executionRunning.CompareAndSwap(false, true) {
		s.releaseExecutionCapacity(runtime.combination)
		permit.Close()
		if takeSlot {
			s.releaseSlot()
		}
		return nil, false
	}
	return &executionResources{permit: permit, takeSlot: takeSlot}, true
}

func (s *ArbitrageScheduler) releaseExecutionResources(
	runtime *arbitrageRuntime,
	item ArbitrageCombination,
	res *executionResources,
) {
	if runtime != nil {
		runtime.executionRunning.Store(false)
	}
	s.releaseExecutionCapacity(item)
	if res != nil && res.permit != nil {
		res.permit.Close()
	}
	if res != nil && res.takeSlot {
		s.releaseSlot()
	}
}

func (s *ArbitrageScheduler) acquireExecutionCapacity(item ArbitrageCombination) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts := arbitrageAccountIDs(item)
	venues := arbitrageVenues(item)
	for _, account := range accounts {
		if s.accountUse[account] >= s.maxAccount {
			return false
		}
	}
	for _, venue := range venues {
		if s.venueUse[venue] >= s.maxVenue {
			return false
		}
	}
	for _, account := range accounts {
		s.accountUse[account]++
	}
	for _, venue := range venues {
		s.venueUse[venue]++
	}
	return true
}

func (s *ArbitrageScheduler) releaseExecutionCapacity(item ArbitrageCombination) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, account := range arbitrageAccountIDs(item) {
		if s.accountUse[account] > 1 {
			s.accountUse[account]--
		} else {
			delete(s.accountUse, account)
		}
	}
	for _, venue := range arbitrageVenues(item) {
		if s.venueUse[venue] > 1 {
			s.venueUse[venue]--
		} else {
			delete(s.venueUse, venue)
		}
	}
}

func arbitrageAccountIDs(item ArbitrageCombination) []int64 {
	if item.LegA.TradingAccountID == item.LegB.TradingAccountID {
		return []int64{item.LegA.TradingAccountID}
	}
	return []int64{item.LegA.TradingAccountID, item.LegB.TradingAccountID}
}

func arbitrageVenues(item ArbitrageCombination) []string {
	if item.LegA.Exchange == item.LegB.Exchange {
		return []string{item.LegA.Exchange}
	}
	return []string{item.LegA.Exchange, item.LegB.Exchange}
}

func (s *ArbitrageScheduler) finishRuntime(runtime *arbitrageRuntime) {
	if runtime == nil {
		return
	}
	s.ensureRuntime(runtime)
	runtime.cleanupOnce.Do(func() {
		if runtime.cancel != nil {
			runtime.cancel()
		}
		if runtime.legA != nil {
			_ = runtime.legA.Close()
		}
		if runtime.legB != nil {
			_ = runtime.legB.Close()
		}
		removed := false
		s.mu.Lock()
		if current := s.runtimes[runtime.combination.ID]; current == runtime {
			delete(s.runtimes, runtime.combination.ID)
			removed = true
		}
		delete(s.valuations, runtime.combination.ID)
		s.mu.Unlock()
		if removed {
			s.unregisterRuntimeLifecycle(runtime.combination.ID)
			if s.leaseBatcher != nil {
				s.leaseBatcher.Remove(runtime.combination.ID)
			}
			if s.snapshotBatcher != nil {
				s.snapshotBatcher.Remove(runtime.combination.ID)
			}
		}
		close(runtime.done)
	})
}

func (s *ArbitrageScheduler) closeAll() {
	s.mu.Lock()
	runtimes := make([]*arbitrageRuntime, 0, len(s.runtimes))
	for _, runtime := range s.runtimes {
		runtimes = append(runtimes, runtime)
	}
	s.mu.Unlock()
	for _, runtime := range runtimes {
		if runtime.cancel != nil {
			runtime.cancel()
		}
		if !runtime.loopStarted.Load() {
			s.finishRuntime(runtime)
		}
	}
	s.runtimeWG.Wait()
}

func marketDataUnavailable(err error) bool {
	return errors.Is(err, marketdata.ErrNoValue) || errors.Is(err, marketdata.ErrStale)
}
