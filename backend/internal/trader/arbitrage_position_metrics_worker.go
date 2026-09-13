package trader

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/marketdata"
)

type arbitrageBBOSource interface {
	Latest(marketdata.Key) (marketdata.BBO, error)
}

type arbitrageMetricsPersistJob struct {
	item ArbitrageCombination
	midA string
	midB string
	now  time.Time
}

type arbitrageFillAverageCompensator interface {
	Compensate(context.Context, []string)
}

type metricsMember struct {
	combo     ArbitrageCombination
	nextDueAt time.Time
	pinned    bool
}

type ArbitragePositionMetricsWorker struct {
	store       arbitragePositionMetricsStore
	market      arbitrageBBOSource
	compensator arbitrageFillAverageCompensator
	interval    time.Duration
	logger      *slog.Logger
	mu          sync.Mutex
	members     map[string]*metricsMember
	lastPersist map[string]time.Time
	inFlight    map[string]struct{}
	exitState   map[string]oneShotExitState
}

func NewArbitragePositionMetricsWorker(
	store arbitragePositionMetricsStore,
	market arbitrageBBOSource,
	logger *slog.Logger,
) *ArbitragePositionMetricsWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &ArbitragePositionMetricsWorker{
		store: store, market: market,
		interval:    arbitragePositionMetricsInterval,
		logger:      logger,
		members:     make(map[string]*metricsMember),
		lastPersist: make(map[string]time.Time),
		inFlight:    make(map[string]struct{}),
		exitState:   make(map[string]oneShotExitState),
	}
}

func (w *ArbitragePositionMetricsWorker) ConfigureFillAverageCompensator(
	compensator arbitrageFillAverageCompensator,
) {
	w.compensator = compensator
}

func (w *ArbitragePositionMetricsWorker) Run(ctx context.Context) {
	syncTicker := time.NewTicker(w.interval)
	defer syncTicker.Stop()
	dueTicker := time.NewTicker(time.Second)
	defer dueTicker.Stop()
	sampleTicker := time.NewTicker(oneShotExitSampleInterval)
	defer sampleTicker.Stop()
	w.syncMembers(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-syncTicker.C:
			w.syncMembers(ctx)
		case <-dueTicker.C:
			w.refreshDue(ctx, time.Now().UTC())
		case <-sampleTicker.C:
			w.sampleExits(ctx, time.Now().UTC())
		}
	}
}

func (w *ArbitragePositionMetricsWorker) refreshAsync(ctx context.Context) {
	w.syncMembers(ctx)
	w.refreshDue(ctx, time.Now().UTC())
}

func (w *ArbitragePositionMetricsWorker) refreshAt(ctx context.Context, now time.Time) {
	w.syncMembers(ctx)
	w.refreshDue(ctx, now)
}

func (w *ArbitragePositionMetricsWorker) RegisterCombination(item ArbitrageCombination) {
	if w == nil || strings.ToLower(strings.TrimSpace(item.Status)) != "running" || item.ID == "" {
		return
	}
	now := time.Now().UTC()
	w.mu.Lock()
	defer w.mu.Unlock()
	if existing, ok := w.members[item.ID]; ok {
		existing.pinned = true
		existing.combo = item
		return
	}
	w.members[item.ID] = &metricsMember{
		combo:     item,
		nextDueAt: metricsDueAtOnRegister(item.CreatedAt, item.MetricsCalculatedAt, now),
		pinned:    true,
	}
}

func (w *ArbitragePositionMetricsWorker) UnregisterCombination(id string) {
	if w == nil || id == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.removeMemberLocked(id)
}

func (w *ArbitragePositionMetricsWorker) syncMembers(ctx context.Context) {
	items, err := w.store.ListRunningArbitrageCombinationsForMetrics(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.logger.Error("list running arbitrage combinations for metrics failed", "error", err)
		}
		return
	}
	seen := make(map[string]struct{}, len(items))
	now := time.Now().UTC()
	w.mu.Lock()
	for _, item := range items {
		if ctx.Err() != nil {
			w.mu.Unlock()
			return
		}
		if strings.ToLower(strings.TrimSpace(item.Status)) != "running" {
			continue
		}
		seen[item.ID] = struct{}{}
		if existing, ok := w.members[item.ID]; ok {
			existing.combo = item
			continue
		}
		w.members[item.ID] = &metricsMember{
			combo:     item,
			nextDueAt: metricsDueAtOnRegister(item.CreatedAt, item.MetricsCalculatedAt, now),
		}
	}
	for id, member := range w.members {
		if _, ok := seen[id]; ok {
			continue
		}
		if member.pinned {
			continue
		}
		w.removeMemberLocked(id)
	}
	w.mu.Unlock()
	w.pruneExitStates(seen, items)
}

func (w *ArbitragePositionMetricsWorker) refreshDue(ctx context.Context, now time.Time) {
	w.persistDue(ctx, w.collectDue(now))
}

func (w *ArbitragePositionMetricsWorker) collectDue(now time.Time) []arbitrageMetricsPersistJob {
	w.mu.Lock()
	due := make([]arbitrageMetricsPersistJob, 0)
	for id, member := range w.members {
		if _, active := w.inFlight[id]; active {
			continue
		}
		if !member.nextDueAt.IsZero() && member.nextDueAt.After(now) {
			continue
		}
		w.inFlight[id] = struct{}{}
		item := member.combo
		item.ID = id
		due = append(due, arbitrageMetricsPersistJob{item: item, now: now})
	}
	w.mu.Unlock()
	for index := range due {
		midA, midB, _ := combinationBBOMids(w.market, due[index].item)
		due[index].midA, due[index].midB = midA, midB
	}
	return due
}

func (w *ArbitragePositionMetricsWorker) persistDue(
	ctx context.Context,
	due []arbitrageMetricsPersistJob,
) {
	if w.compensator != nil && len(due) > 0 {
		ids := make([]string, 0, len(due))
		for _, job := range due {
			ids = append(ids, job.item.ID)
		}
		w.compensator.Compensate(ctx, ids)
	}
	for index, job := range due {
		if ctx.Err() != nil {
			w.releaseJobs(due[index:])
			return
		}
		result, persistErr := w.store.PersistRunningArbitragePositionMetrics(
			ctx, job.item.ID, job.midA, job.midB, job.now,
		)
		w.applyPersistResult(job.item.ID, job.now, result, persistErr)
		if persistErr != nil && ctx.Err() == nil {
			w.logger.Error("persist arbitrage position metrics failed",
				"combination_id", job.item.ID, "error", persistErr)
			continue
		}
		if persistErr == nil && result.Applied {
			w.publishExitBasis(job.item.ID, result)
		}
	}
}

func (w *ArbitragePositionMetricsWorker) applyPersistResult(
	id string,
	now time.Time,
	result arbitragePositionMetricsPersistResult,
	persistErr error,
) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.inFlight, id)
	member, ok := w.members[id]
	if persistErr != nil {
		if ok {
			member.nextDueAt = now.Add(metricsApplyRetryDelay)
		}
		return
	}
	reason := result.Reason
	if reason == "" {
		if result.Applied {
			reason = MetricsApplied
		} else {
			reason = MetricsNotReady
		}
	}
	if !ok {
		return
	}
	if !result.Combination.CreatedAt.IsZero() {
		member.combo.CreatedAt = result.Combination.CreatedAt
	}
	if !result.Combination.MetricsCalculatedAt.IsZero() {
		member.combo.MetricsCalculatedAt = result.Combination.MetricsCalculatedAt
	}
	switch reason {
	case MetricsApplied:
		w.lastPersist[id] = now
		member.nextDueAt = nextMetricsPhaseAfter(member.combo.CreatedAt, now)
	case MetricsActiveWork, MetricsVersionConflict:
		member.nextDueAt = now.Add(metricsApplyRetryDelay)
	case MetricsAlreadyCurrent, MetricsNotReady:
		w.lastPersist[id] = now
		member.nextDueAt = nextMetricsPhaseAfter(member.combo.CreatedAt, now)
	case MetricsInactive:
		w.removeMemberLocked(id)
	default:
		w.lastPersist[id] = now
		member.nextDueAt = nextMetricsPhaseAfter(member.combo.CreatedAt, now)
	}
}

func (w *ArbitragePositionMetricsWorker) releaseJobs(
	jobs []arbitrageMetricsPersistJob,
) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, job := range jobs {
		delete(w.inFlight, job.item.ID)
	}
}

func (w *ArbitragePositionMetricsWorker) removeMemberLocked(id string) {
	delete(w.members, id)
	delete(w.lastPersist, id)
	delete(w.inFlight, id)
	delete(w.exitState, id)
}

func (w *ArbitragePositionMetricsWorker) lastPersistAt(id string) (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	at, ok := w.lastPersist[id]
	return at, ok
}

func (w *ArbitragePositionMetricsWorker) exitStateCopy(id string) (oneShotExitState, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	state, ok := w.exitState[id]
	if !ok {
		return oneShotExitState{}, false
	}
	state.hits = append([]bool{}, state.hits...)
	return state, true
}

func (w *ArbitragePositionMetricsWorker) pruneExitStates(
	seen map[string]struct{},
	items []ArbitrageCombination,
) {
	eligible := make(map[string]struct{}, len(items))
	for _, item := range items {
		if oneShotAnnualizedWaitingExit(item) {
			eligible[item.ID] = struct{}{}
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for id := range w.exitState {
		if _, ok := seen[id]; !ok {
			if member, exists := w.members[id]; exists && member.pinned {
				continue
			}
			delete(w.exitState, id)
			continue
		}
		if _, ok := eligible[id]; !ok {
			delete(w.exitState, id)
		}
	}
}

func (w *ArbitragePositionMetricsWorker) publishExitBasis(
	id string,
	result arbitragePositionMetricsPersistResult,
) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !result.Applied {
		return
	}
	if result.ExitBasis == nil {
		delete(w.exitState, id)
		return
	}
	w.exitState[id] = oneShotExitState{basis: *result.ExitBasis}
}

func (w *ArbitragePositionMetricsWorker) sampleExits(ctx context.Context, now time.Time) {
	type snapshot struct {
		id    string
		state oneShotExitState
	}
	w.mu.Lock()
	pending := make([]snapshot, 0, len(w.exitState))
	for id, state := range w.exitState {
		copied := state
		copied.hits = append([]bool{}, state.hits...)
		pending = append(pending, snapshot{id: id, state: copied})
	}
	w.mu.Unlock()
	for _, item := range pending {
		if ctx.Err() != nil {
			return
		}
		w.sampleExit(ctx, item.id, item.state, now)
	}
}

func (w *ArbitragePositionMetricsWorker) sampleExit(
	ctx context.Context,
	id string,
	state oneShotExitState,
	now time.Time,
) {
	if now.Sub(state.basis.CalculatedAt) > oneShotExitBasisMaxAge {
		w.mu.Lock()
		if current, ok := w.exitState[id]; ok &&
			current.basis.Version == state.basis.Version &&
			current.basis.CalculatedAt.Equal(state.basis.CalculatedAt) {
			current.hits = nil
			current.stale = true
			w.exitState[id] = current
		}
		w.mu.Unlock()
		return
	}
	if state.stale {
		return
	}
	if !state.lastSample.IsZero() && now.Sub(state.lastSample) < oneShotExitSampleInterval {
		return
	}
	bboA, okA := latestLegBBO(w.market, state.basis.LegA)
	bboB, okB := latestLegBBO(w.market, state.basis.LegB)
	quote, quoteOK := oneShotExitQuote{}, false
	if okA && okB {
		quote, quoteOK = oneShotExitQuoteFromBBO(bboA, bboB, now)
	}
	var result oneShotExitSampleResult
	if !quoteOK {
		result.fault = true
	} else {
		result = evaluateOneShotExitSample(state.basis, quote, now)
	}
	ready := false
	w.mu.Lock()
	current, ok := w.exitState[id]
	if !ok || current.basis.Version != state.basis.Version ||
		!current.basis.CalculatedAt.Equal(state.basis.CalculatedAt) ||
		current.stale {
		w.mu.Unlock()
		return
	}
	if result.fault {
		current.hits = nil
	} else {
		current.hits = appendExitHit(current.hits, result.hit)
		ready = oneShotExitWindowReady(current.hits)
	}
	current.lastSample = now
	w.exitState[id] = current
	w.mu.Unlock()
	if !ready {
		return
	}
	_, applied, err := w.store.MarkOneShotExiting(
		ctx, id, state.basis.Version, "annualized", nil,
	)
	if err != nil {
		if ctx.Err() == nil {
			w.logger.Error("mark one-shot annualized exiting failed",
				"combination_id", id, "error", err)
		}
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	current, ok = w.exitState[id]
	if !ok {
		return
	}
	if applied || current.basis.Version == state.basis.Version {
		delete(w.exitState, id)
	}
}

func metricsDueAtOnRegister(createdAt, calculatedAt, now time.Time) time.Time {
	if createdAt.IsZero() {
		createdAt = now
	}
	first := createdAt.Add(arbitragePositionMetricsInterval)
	if now.Before(first) {
		return first
	}
	lastPhase := lastMetricsPhaseAtOrBefore(createdAt, now)
	if calculatedAt.IsZero() || calculatedAt.Before(lastPhase) {
		return time.Time{}
	}
	return nextMetricsPhaseAfter(createdAt, now)
}

func lastMetricsPhaseAtOrBefore(createdAt, now time.Time) time.Time {
	if createdAt.IsZero() {
		return now
	}
	if now.Before(createdAt.Add(arbitragePositionMetricsInterval)) {
		return createdAt
	}
	n := now.Sub(createdAt) / arbitragePositionMetricsInterval
	return createdAt.Add(n * arbitragePositionMetricsInterval)
}

func nextMetricsPhaseAfter(createdAt, now time.Time) time.Time {
	if createdAt.IsZero() {
		createdAt = now
	}
	if !now.After(createdAt) {
		return createdAt.Add(arbitragePositionMetricsInterval)
	}
	n := now.Sub(createdAt) / arbitragePositionMetricsInterval
	next := createdAt.Add((n + 1) * arbitragePositionMetricsInterval)
	if !next.After(now) {
		next = next.Add(arbitragePositionMetricsInterval)
	}
	return next
}

func combinationBBOMids(
	market arbitrageBBOSource,
	item ArbitrageCombination,
) (string, string, bool) {
	if market == nil {
		return "", "", false
	}
	midA, okA := legBBOMid(market, item.LegA)
	midB, okB := legBBOMid(market, item.LegB)
	if !okA || !okB {
		return "", "", false
	}
	return midA, midB, true
}

func latestLegBBO(market arbitrageBBOSource, leg ArbitrageLeg) (marketdata.BBO, bool) {
	if market == nil {
		return marketdata.BBO{}, false
	}
	key, err := marketdata.NewKey(leg.Exchange, leg.ContractType, leg.ExchangeSymbol)
	if err != nil {
		return marketdata.BBO{}, false
	}
	bbo, err := market.Latest(key)
	if err != nil {
		return marketdata.BBO{}, false
	}
	return bbo, true
}

func legBBOMid(market arbitrageBBOSource, leg ArbitrageLeg) (string, bool) {
	bbo, ok := latestLegBBO(market, leg)
	if !ok {
		return "", false
	}
	bid, errBid := decimal.NewFromString(bbo.BidPrice)
	ask, errAsk := decimal.NewFromString(bbo.AskPrice)
	if errBid != nil || errAsk != nil || !bid.IsPositive() || !ask.IsPositive() {
		return "", false
	}
	return bid.Add(ask).Div(decimal.NewFromInt(2)).String(), true
}
