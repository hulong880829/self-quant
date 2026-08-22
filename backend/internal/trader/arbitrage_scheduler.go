package trader

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/marketdata"
)

type arbitrageExecutionRunner interface {
	Execute(context.Context, ArbitrageCombination, ArbitrageExecution, marketdata.BBO, marketdata.BBO)
	CloseCombination(context.Context, ArbitrageCombination) error
}

type ArbitrageScheduler struct {
	store      arbitrageStore
	market     *marketdata.Manager
	runner     arbitrageExecutionRunner
	interval   time.Duration
	lease      time.Duration
	batch      int
	workers    int
	maxAccount int
	maxVenue   int
	dryRun     bool
	logger     *slog.Logger
	mu         sync.Mutex
	runtimes   map[string]*arbitrageRuntime
	workerGate chan struct{}
	accountUse map[int64]int
	venueUse   map[string]int
	triggers   atomic.Uint64
	claims     atomic.Uint64
	staleBBO   atomic.Uint64
	rejected   atomic.Uint64
}

type arbitrageRuntime struct {
	combination ArbitrageCombination
	cancel      context.CancelFunc
	legA        *marketdata.Subscription
	legB        *marketdata.Subscription
	nextRenew   time.Time
	lastPersist time.Time
	lastSignal  string
	resumed     map[string]bool
}

func NewArbitrageScheduler(
	store arbitrageStore,
	market *marketdata.Manager,
	runner arbitrageExecutionRunner,
	interval, lease time.Duration,
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
	return &ArbitrageScheduler{
		store: store, market: market, runner: runner, interval: interval, lease: lease,
		batch: batch, workers: workers, dryRun: dryRun, logger: logger,
		maxAccount: maxAccount, maxVenue: maxVenue,
		runtimes: make(map[string]*arbitrageRuntime), workerGate: make(chan struct{}, workers),
		accountUse: make(map[int64]int), venueUse: make(map[string]int),
	}
}

func (s *ArbitrageScheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	defer s.closeAll()
	for {
		s.runOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *ArbitrageScheduler) runOnce(ctx context.Context) {
	s.acquire(ctx)
	s.mu.Lock()
	runtimes := make([]*arbitrageRuntime, 0, len(s.runtimes))
	for _, runtime := range s.runtimes {
		runtimes = append(runtimes, runtime)
	}
	s.mu.Unlock()
	for _, runtime := range runtimes {
		s.process(ctx, runtime)
	}
}

func (s *ArbitrageScheduler) acquire(ctx context.Context) {
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
		s.runtimes[item.ID] = runtime
		s.mu.Unlock()
		s.logger.Info("arbitrage combination leased", "combination_id", item.ID)
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
	return &arbitrageRuntime{
		combination: item, cancel: cancel, legA: subA, legB: subB,
		nextRenew: time.Now().UTC().Add(s.lease / 3),
		resumed:   make(map[string]bool),
	}, nil
}

func (s *ArbitrageScheduler) process(ctx context.Context, runtime *arbitrageRuntime) {
	now := time.Now().UTC()
	if !now.Before(runtime.nextRenew) {
		renewed, err := s.store.RenewArbitrageLease(ctx, runtime.combination.ID, s.lease)
		if err != nil || !renewed {
			s.remove(runtime.combination.ID)
			return
		}
		if refreshed, err := s.store.GetArbitrageCombinationByOwner(
			ctx, runtime.combination.OwnerUsername, runtime.combination.ID,
		); err == nil {
			runtime.combination = refreshed
		}
		runtime.nextRenew = now.Add(s.lease / 3)
	}
	if runtime.combination.Status == "closing" {
		if s.runner != nil {
			if err := s.runner.CloseCombination(ctx, runtime.combination); err != nil {
				s.logger.Error("close arbitrage combination failed",
					"combination_id", runtime.combination.ID, "error", err)
				return
			}
		}
		s.remove(runtime.combination.ID)
		return
	}
	bboA, errA := runtime.legA.Latest()
	bboB, errB := runtime.legB.Latest()
	if errA != nil || errB != nil {
		s.staleBBO.Add(1)
		if !runtime.combination.MarketDataStale || now.Sub(runtime.lastPersist) >= time.Second {
			runtime.combination.MarketDataStale = true
			if updated, err := s.store.UpdateArbitrageMarketSnapshot(
				ctx, runtime.combination.ID,
				runtime.combination.CurrentAskSpreadBps,
				runtime.combination.CurrentBidSpreadBps, true,
			); err == nil {
				runtime.combination = updated
				runtime.lastPersist = now
			}
		}
		return
	}
	aBid, e1 := decimal.NewFromString(bboA.BidPrice)
	aAsk, e2 := decimal.NewFromString(bboA.AskPrice)
	bBid, e3 := decimal.NewFromString(bboB.BidPrice)
	bAsk, e4 := decimal.NewFromString(bboB.AskPrice)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return
	}
	ask, bid, ok := calculateArbitrageSpreads(aBid, aAsk, bBid, bAsk)
	if !ok {
		return
	}
	runtime.combination.CurrentAskSpreadBps = ask.String()
	runtime.combination.CurrentBidSpreadBps = bid.String()
	runtime.combination.MarketDataStale = false
	if now.Sub(runtime.lastPersist) >= time.Second {
		if updated, err := s.store.UpdateArbitrageMarketSnapshot(
			ctx, runtime.combination.ID, ask.String(), bid.String(), false,
		); err == nil {
			runtime.combination = updated
			runtime.lastPersist = now
		}
	}
	if s.resumeActive(ctx, runtime, bboA, bboB) {
		return
	}
	askThreshold, _ := decimal.NewFromString(runtime.combination.AskThresholdBps)
	bidThreshold, _ := decimal.NewFromString(runtime.combination.BidThresholdBps)
	direction := arbitrageTriggered(ask, bid, askThreshold, bidThreshold)
	if direction == "" {
		runtime.lastSignal = ""
		return
	}
	s.triggers.Add(1)
	if s.dryRun {
		if runtime.lastSignal != direction {
			_ = s.store.AppendArbitrageEvent(ctx, runtime.combination.ID, "", "dry_run_trigger", map[string]any{
				"direction": direction, "askSpreadBps": ask.String(), "bidSpreadBps": bid.String(),
			})
			s.logger.Info("arbitrage dry-run trigger",
				"combination_id", runtime.combination.ID, "direction", direction,
				"ask_spread_bps", ask.String(), "bid_spread_bps", bid.String())
		}
		runtime.lastSignal = direction
		return
	}
	execution, claimed, err := s.store.ClaimArbitrageExecution(ctx, ArbitrageExecution{
		ID: uuid.NewString(), CombinationID: runtime.combination.ID,
		Direction: direction, Status: "claimed",
		TriggerAskSpread: ask.String(), TriggerBidSpread: bid.String(),
		TriggerLegABid: aBid.String(), TriggerLegAAsk: aAsk.String(),
		TriggerLegBBid: bBid.String(), TriggerLegBAsk: bAsk.String(),
		TargetBaseQuantity: "0", LegAFilledQuantity: "0", LegBFilledQuantity: "0",
		DeltaNotional: "0",
	})
	if err != nil || !claimed || s.runner == nil {
		return
	}
	s.claims.Add(1)
	s.logger.Info("arbitrage trigger claimed",
		"combination_id", runtime.combination.ID, "execution_id", execution.ID,
		"direction", direction, "ask_spread_bps", ask.String(),
		"bid_spread_bps", bid.String(),
		"leg_a_bbo_age_ms", now.Sub(bboA.ReceiveTimestamp).Milliseconds(),
		"leg_b_bbo_age_ms", now.Sub(bboB.ReceiveTimestamp).Milliseconds(),
	)
	select {
	case s.workerGate <- struct{}{}:
		if !s.acquireExecutionCapacity(runtime.combination) {
			<-s.workerGate
			execution.Status = "failed"
			execution.ErrorMessage = "arbitrage account or venue concurrency limit reached"
			s.rejected.Add(1)
			_, _ = s.store.UpdateArbitrageExecution(ctx, execution)
			return
		}
		go func() {
			defer func() { <-s.workerGate }()
			defer s.releaseExecutionCapacity(runtime.combination)
			s.runner.Execute(ctx, runtime.combination, execution, bboA, bboB)
		}()
	default:
		execution.Status = "failed"
		execution.ErrorMessage = "arbitrage worker limit reached"
		_, _ = s.store.UpdateArbitrageExecution(ctx, execution)
	}
}

type ArbitrageSchedulerStats struct {
	ActiveCombinations uint64
	ActiveAccounts     uint64
	ActiveVenues       uint64
	Triggers           uint64
	Claims             uint64
	StaleBBOReads      uint64
	CapacityRejected   uint64
}

func (s *ArbitrageScheduler) Stats() ArbitrageSchedulerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ArbitrageSchedulerStats{
		ActiveCombinations: uint64(len(s.runtimes)),
		ActiveAccounts:     uint64(len(s.accountUse)), ActiveVenues: uint64(len(s.venueUse)),
		Triggers: s.triggers.Load(), Claims: s.claims.Load(),
		StaleBBOReads: s.staleBBO.Load(), CapacityRejected: s.rejected.Load(),
	}
}

func (s *ArbitrageScheduler) resumeActive(
	ctx context.Context,
	runtime *arbitrageRuntime,
	bboA, bboB marketdata.BBO,
) bool {
	active := false
	for _, direction := range []string{"ask", "bid"} {
		if runtime.resumed[direction] {
			continue
		}
		execution, err := s.store.GetActiveArbitrageExecution(
			ctx, runtime.combination.ID, direction,
		)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				runtime.resumed[direction] = true
			}
			continue
		}
		active = true
		runtime.resumed[direction] = true
		if s.runner == nil {
			continue
		}
		select {
		case s.workerGate <- struct{}{}:
			if !s.acquireExecutionCapacity(runtime.combination) {
				<-s.workerGate
				runtime.resumed[direction] = false
				continue
			}
			go func(execution ArbitrageExecution) {
				defer func() { <-s.workerGate }()
				defer s.releaseExecutionCapacity(runtime.combination)
				s.runner.Execute(ctx, runtime.combination, execution, bboA, bboB)
			}(execution)
		default:
			runtime.resumed[direction] = false
		}
	}
	return active
}

func (s *ArbitrageScheduler) acquireExecutionCapacity(item ArbitrageCombination) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts := []int64{item.LegA.TradingAccountID, item.LegB.TradingAccountID}
	venues := []string{item.LegA.Exchange, item.LegB.Exchange}
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
	for _, account := range []int64{item.LegA.TradingAccountID, item.LegB.TradingAccountID} {
		if s.accountUse[account] > 1 {
			s.accountUse[account]--
		} else {
			delete(s.accountUse, account)
		}
	}
	for _, venue := range []string{item.LegA.Exchange, item.LegB.Exchange} {
		if s.venueUse[venue] > 1 {
			s.venueUse[venue]--
		} else {
			delete(s.venueUse, venue)
		}
	}
}

func (s *ArbitrageScheduler) remove(id string) {
	s.mu.Lock()
	runtime := s.runtimes[id]
	delete(s.runtimes, id)
	s.mu.Unlock()
	if runtime != nil {
		_ = runtime.legA.Close()
		_ = runtime.legB.Close()
		runtime.cancel()
	}
}

func (s *ArbitrageScheduler) closeAll() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.runtimes))
	for id := range s.runtimes {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.remove(id)
	}
}

func marketDataUnavailable(err error) bool {
	return errors.Is(err, marketdata.ErrNoValue) || errors.Is(err, marketdata.ErrStale)
}
