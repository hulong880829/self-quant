package trader

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestNormalizeUpdateArbitrageInputPresenceRules(t *testing.T) {
	empty := ""
	if _, err := normalizeUpdateArbitrageInput(UpdateArbitrageInput{
		Token: "token", CombinationID: "11111111-1111-1111-1111-111111111111",
		ExitAnnualizedRate: &empty,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty rate err=%v", err)
	}
	if _, err := normalizeUpdateArbitrageInput(UpdateArbitrageInput{
		Token: "token", CombinationID: "11111111-1111-1111-1111-111111111111",
		EarlyExitFunding8hAnnualizedFloor: &empty,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty floor err=%v", err)
	}
	seconds := 123
	if _, err := normalizeUpdateArbitrageInput(UpdateArbitrageInput{
		Token: "token", CombinationID: "11111111-1111-1111-1111-111111111111",
		ExitAfterSeconds: &seconds,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("illegal period err=%v", err)
	}
	okSeconds := 86400
	rate := "0.15"
	got, err := normalizeUpdateArbitrageInput(UpdateArbitrageInput{
		Token: "token", CombinationID: "11111111-1111-1111-1111-111111111111",
		ExitAfterSeconds:   &okSeconds,
		ExitAnnualizedRate: &rate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ExitAfterSeconds == nil || *got.ExitAfterSeconds != 86400 ||
		got.ExitAnnualizedRate == nil || *got.ExitAnnualizedRate != "0.15" {
		t.Fatalf("got=%+v", got)
	}
}

func TestOneShotExitRequestClearsAfterVersionChange(t *testing.T) {
	runtime := &arbitrageRuntime{
		combination: ArbitrageCombination{
			ID: "combo-1", Status: "running", RunMode: "one_shot",
			OneShotPhase: "building_target", Version: 4,
		},
	}
	runtime.requestOneShotExit("annualized")
	if !runtime.oneShotShouldStopOpen() {
		t.Fatal("matching version should stop opening")
	}
	runtime.combination.Version = 5
	scheduler := &ArbitrageScheduler{}
	scheduler.refreshExitRequest(runtime)
	if runtime.exitRequest.Requested {
		t.Fatal("stale exit request must clear")
	}
	if runtime.oneShotShouldStopOpen() {
		t.Fatal("new version must allow building unless still requested")
	}
	runtime.requestOneShotExit("time")
	if runtime.exitRequest.Version != 5 || !runtime.oneShotShouldStopOpen() {
		t.Fatalf("new request=%+v", runtime.exitRequest)
	}
	runtime.combination.OneShotPhase = "exiting"
	scheduler.refreshExitRequest(runtime)
	if !runtime.exitRequest.Requested {
		t.Fatal("exiting must keep the request")
	}
}

func TestMaybeMarkOneShotTimeExitingFromBuildingTarget(t *testing.T) {
	store := &dryRunStore{item: ArbitrageCombination{
		ID: "combo-time", Status: "running", RunMode: "one_shot",
		OneShotPhase: "building_target", Version: 3,
		ScheduledExitAt: time.Now().UTC().Add(-time.Second),
	}}
	scheduler := &ArbitrageScheduler{store: store}
	runtime := &arbitrageRuntime{combination: store.item}
	scheduler.maybeMarkOneShotTimeExiting(context.Background(), runtime, time.Now().UTC())
	if runtime.combination.OneShotPhase != "exiting" || runtime.combination.Version != 4 {
		t.Fatalf("combination=%+v request=%+v", runtime.combination, runtime.exitRequest)
	}
	if !runtime.oneShotShouldStopOpen() {
		t.Fatal("exiting should stop opening")
	}
}

func TestExecutableInventoryClosePnLUsesBidAsk(t *testing.T) {
	combo := ArbitrageCombination{
		ExecutionMode: "simultaneous_market",
		LegA:          ArbitrageLeg{TradingAccountID: 1, ContractType: "perpetual", Exchange: "binance"},
		LegB:          ArbitrageLeg{TradingAccountID: 2, ContractType: "perpetual", Exchange: "okx"},
	}
	taker := "0.0004"
	accounts := map[int64]accountFeeSnapshot{
		1: {exchange: "binance", contractTaker: &taker},
		2: {exchange: "okx", contractTaker: &taker},
	}
	quotes := arbitrageMetricsQuotes{
		BidA: "99", AskA: "101", BidB: "100", AskB: "102",
		ReceivedA: time.Now().UTC(), ReceivedB: time.Now().UTC(),
	}
	legA := arbitrageLegCost{position: decimal.NewFromInt(1), average: decimal.NewFromInt(100)}
	legB := arbitrageLegCost{position: decimal.NewFromInt(-1), average: decimal.NewFromInt(101)}
	pnl, fee, ok := executableInventoryClosePnL(legA, legB, quotes, accounts, combo)
	if !ok {
		t.Fatal("expected executable close pnl")
	}
	if !pnl.Equal(decimal.NewFromInt(-2)) {
		t.Fatalf("pnl=%s", pnl)
	}
	if fee.IsZero() {
		t.Fatalf("fee=%s", fee)
	}
}

func TestMetricsQuotesFreshRequiresReceiveTimestamp(t *testing.T) {
	now := time.Now().UTC()
	quotes := arbitrageMetricsQuotes{
		BidA: "100", AskA: "101", BidB: "100", AskB: "101",
	}
	if quotes.fresh(now) {
		t.Fatal("zero receive timestamp is stale")
	}
	quotes.ReceivedA = now
	quotes.ReceivedB = now
	if !quotes.fresh(now) {
		t.Fatal("fresh quotes")
	}
}
