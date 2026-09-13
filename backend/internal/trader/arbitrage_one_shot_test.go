package trader

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/marketdata"
)

func TestMaybeMarkOneShotWaitingExitRequiresSettledState(t *testing.T) {
	store := &dryRunStore{item: ArbitrageCombination{
		ID: "combo-one-shot", Status: "running", RunMode: "one_shot",
		OneShotPhase: "building_target", TargetNotional: "10000",
		LegABasePosition: "1", LegBBasePosition: "-1",
	}}
	scheduler := &ArbitrageScheduler{store: store}
	runtime := &arbitrageRuntime{combination: store.item}
	remaining := decimal.NewFromInt(10)
	scheduler.maybeMarkOneShotWaitingExit(context.Background(), runtime, remaining)
	if runtime.combination.OneShotPhase == "waiting_exit" {
		t.Fatal("uncertain/unreconciled combo entered waiting_exit")
	}

	runtime.combination.LastPositionReconciledAt = time.Now().UTC()
	store.item = runtime.combination
	runtime.executionRunning.Store(true)
	scheduler.maybeMarkOneShotWaitingExit(context.Background(), runtime, remaining)
	if runtime.combination.OneShotPhase == "waiting_exit" {
		t.Fatal("live execution entered waiting_exit")
	}

	runtime.executionRunning.Store(false)
	store.activeExecution = &ArbitrageExecution{ID: "live"}
	scheduler.maybeMarkOneShotWaitingExit(context.Background(), runtime, remaining)
	if runtime.combination.OneShotPhase == "waiting_exit" {
		t.Fatal("active execution entered waiting_exit")
	}

	store.activeExecution = nil
	runtime.combination.PositionUncertain = true
	store.item = runtime.combination
	scheduler.maybeMarkOneShotWaitingExit(context.Background(), runtime, remaining)
	if runtime.combination.OneShotPhase == "waiting_exit" {
		t.Fatal("uncertain combo entered waiting_exit")
	}

	runtime.combination.PositionUncertain = false
	runtime.combination.CarryBaseQuantity = "0.2"
	store.item = runtime.combination
	scheduler.maybeMarkOneShotWaitingExit(context.Background(), runtime, remaining)
	if runtime.combination.OneShotPhase == "waiting_exit" {
		t.Fatal("unhedged carry entered waiting_exit")
	}

	runtime.combination.CarryBaseQuantity = "0"
	runtime.combination.LegBBasePosition = "0"
	store.item = runtime.combination
	scheduler.maybeMarkOneShotWaitingExit(context.Background(), runtime, remaining)
	if runtime.combination.OneShotPhase == "waiting_exit" {
		t.Fatal("naked single leg entered waiting_exit")
	}

	runtime.combination.LegBBasePosition = "-1"
	store.item = runtime.combination
	scheduler.maybeMarkOneShotWaitingExit(context.Background(), runtime, remaining)
	if runtime.combination.OneShotPhase != "waiting_exit" {
		t.Fatalf("settled combo stayed %q", runtime.combination.OneShotPhase)
	}
}

func TestMaybeMarkOneShotWaitingExitAcceptsDustCarry(t *testing.T) {
	item := ArbitrageCombination{
		ID: "combo-dust", Status: "running", RunMode: "one_shot",
		OneShotPhase: "building_target", TargetNotional: "20",
		LegABasePosition: "20000", LegBBasePosition: "-19487", CarryBaseQuantity: "513",
		LastPositionReconciledAt: time.Now().UTC(),
		LegA: ArbitrageLeg{
			InstrumentID: 1, ContractType: "perpetual", Exchange: "okx",
		},
		LegB: ArbitrageLeg{
			InstrumentID: 2, ContractType: "perpetual", Exchange: "okx",
		},
	}
	store := &dryRunStore{item: item}
	instrument := oneShotDustTestInstrument("1", "1", "5")
	scheduler := &ArbitrageScheduler{
		store:   store,
		catalog: anyInstrumentCatalog{item: instrument},
	}
	runtime := &arbitrageRuntime{combination: item}
	runtime.latestA = marketdata.BBO{BidPrice: "0.001", AskPrice: "0.001"}
	runtime.latestB = runtime.latestA
	scheduler.maybeMarkOneShotWaitingExit(context.Background(), runtime, decimal.Zero)
	if runtime.combination.OneShotPhase != "waiting_exit" {
		t.Fatalf("dust carry stayed %q", runtime.combination.OneShotPhase)
	}
	if runtime.combination.CarryBaseQuantity != "513" ||
		runtime.combination.LegABasePosition != "20000" ||
		runtime.combination.LegBBasePosition != "-19487" {
		t.Fatalf("positions mutated: %+v", runtime.combination)
	}
	found := false
	for _, eventType := range store.eventTypes {
		if eventType == "one_shot_target_reached_with_dust" {
			found = true
		}
	}
	if !found {
		t.Fatalf("events=%v", store.eventTypes)
	}
}

func TestMaybeMarkOneShotWaitingExitCircuitAndLiveOrderRetry(t *testing.T) {
	item := ArbitrageCombination{
		ID: "combo-retry", Status: "running", RunMode: "one_shot",
		OneShotPhase: "building_target", TargetNotional: "20",
		LegABasePosition: "1", LegBBasePosition: "-1",
		LastPositionReconciledAt: time.Now().UTC(), CircuitOpen: true,
	}
	store := &dryRunStore{item: item}
	scheduler := &ArbitrageScheduler{store: store}
	runtime := &arbitrageRuntime{combination: item}
	scheduler.maybeMarkOneShotWaitingExit(context.Background(), runtime, decimal.Zero)
	if runtime.combination.OneShotPhase == "waiting_exit" {
		t.Fatal("circuit open entered waiting_exit")
	}
	runtime.combination.CircuitOpen = false
	store.item = runtime.combination
	store.dustGateLiveOrder = true
	scheduler.maybeMarkOneShotWaitingExit(context.Background(), runtime, decimal.Zero)
	if runtime.combination.OneShotPhase == "waiting_exit" {
		t.Fatal("live order entered waiting_exit")
	}
}

func TestMaybeMarkOneShotTimeExitingFlipsOnce(t *testing.T) {
	store := &dryRunStore{item: ArbitrageCombination{
		ID: "combo-time", Status: "running", RunMode: "one_shot",
		OneShotPhase: "waiting_exit", Version: 3,
		ScheduledExitAt: time.Now().UTC().Add(-time.Second),
	}}
	scheduler := &ArbitrageScheduler{store: store}
	runtime := &arbitrageRuntime{combination: store.item}
	scheduler.maybeMarkOneShotTimeExiting(context.Background(), runtime, time.Now().UTC())
	if runtime.combination.Status != "running" ||
		runtime.combination.OneShotPhase != "exiting" ||
		runtime.combination.Version != 4 {
		t.Fatalf("combination=%+v", runtime.combination)
	}
	scheduler.maybeMarkOneShotTimeExiting(context.Background(), runtime, time.Now().UTC())
	if runtime.combination.Version != 4 {
		t.Fatalf("exiting applied twice: %+v", runtime.combination)
	}
}

func TestMaybeMarkOneShotWaitingExitSchedulesSevenDays(t *testing.T) {
	store := &dryRunStore{item: ArbitrageCombination{
		ID: "combo-7d", Status: "running", RunMode: "one_shot",
		OneShotPhase: "building_target", ExitPolicy: "time",
		ExitAfterSeconds: 604800, TargetNotional: "10000",
		LegABasePosition: "1", LegBBasePosition: "-1",
		LastPositionReconciledAt: time.Now().UTC(),
	}}
	scheduler := &ArbitrageScheduler{store: store}
	runtime := &arbitrageRuntime{combination: store.item}
	before := time.Now().UTC()
	scheduler.maybeMarkOneShotWaitingExit(context.Background(), runtime, decimal.Zero)
	if runtime.combination.OneShotPhase != "waiting_exit" {
		t.Fatalf("phase=%s", runtime.combination.OneShotPhase)
	}
	delta := runtime.combination.ScheduledExitAt.Sub(runtime.combination.TargetReachedAt)
	if delta < 7*24*time.Hour-2*time.Second || delta > 7*24*time.Hour+2*time.Second {
		t.Fatalf("scheduled delta=%s", delta)
	}
	if runtime.combination.ScheduledExitAt.Before(before.Add(7 * 24 * time.Hour).Add(-2 * time.Second)) {
		t.Fatalf("scheduled=%s", runtime.combination.ScheduledExitAt)
	}
}

func TestMaybeMarkOneShotTimeExitingSkipsBeforeDeadline(t *testing.T) {
	store := &dryRunStore{item: ArbitrageCombination{
		ID: "combo-time", Status: "running", RunMode: "one_shot",
		OneShotPhase: "waiting_exit", Version: 3,
		ScheduledExitAt: time.Now().UTC().Add(time.Hour),
	}}
	scheduler := &ArbitrageScheduler{store: store}
	runtime := &arbitrageRuntime{combination: store.item}
	scheduler.maybeMarkOneShotTimeExiting(context.Background(), runtime, time.Now().UTC())
	if runtime.combination.Status != "running" || runtime.combination.Version != 3 {
		t.Fatalf("closed early: %+v", runtime.combination)
	}
}

func TestCircuitOpenNotionalWithinTargetDropsOrderBuffer(t *testing.T) {
	if !circuitOpenNotionalWithinTarget(decimal.NewFromInt(1000), decimal.NewFromInt(1000)) {
		t.Fatal("equal target should be within")
	}
	if circuitOpenNotionalWithinTarget(decimal.NewFromInt(1001), decimal.NewFromInt(1000)) {
		t.Fatal("over target should fail")
	}
}

func TestExecutionNotionalIgnoresCombinationOrderNotional(t *testing.T) {
	combo := ArbitrageCombination{OrderNotional: "500"}
	got := executionNotional(combo, ArbitrageExecution{RequestedNotional: "37.5"})
	if !got.Equal(decimal.RequireFromString("37.5")) {
		t.Fatalf("got=%s", got)
	}
	if executionNotional(combo, ArbitrageExecution{}).IsPositive() {
		t.Fatal("empty execution notional must not fall back to combination")
	}
}

func TestWaitingExitDoesNotClaimOpen(t *testing.T) {
	store := &dryRunStore{claim: true, item: ArbitrageCombination{
		ID: "combo-wait", Status: "running", RunMode: "one_shot",
		OneShotPhase: "waiting_exit", EntryDirection: "ask",
		TargetNotional: "10000", ExecutionMode: "simultaneous_market",
		LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "binance", ContractType: "perpetual"},
		LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "okx", ContractType: "perpetual"},
	}}
	scheduler := &ArbitrageScheduler{store: store, notionalRand: fixedNotionalRand{decimal.Zero}}
	runtime := &arbitrageRuntime{combination: store.item}
	if scheduler.evaluate(context.Background(), runtime, "bbo_event") {
		t.Fatal("waiting_exit evaluate should not stop")
	}
	if store.claims != 0 {
		t.Fatalf("waiting_exit claimed open execution: %d", store.claims)
	}
}
