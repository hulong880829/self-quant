package trader

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"selfquant/backend/internal/trader/exchange"
)

func TestPrepareArbitrageHedgeIntentReplayAndProtection(t *testing.T) {
	orders := newMemoryStore()
	combo := ArbitrageCombination{
		ID: "combo-1", RuntimeState: "maker_open",
	}
	store := &executorFailStore{combo: combo, intentStore: orders}
	execution := ArbitrageExecution{ID: "exec-1", CombinationID: combo.ID}
	intent := Order{
		IdempotencyKey: "arb:exec-1:b:hedge:0", OwnerUsername: "admin",
		TradingAccountID: 2, Exchange: "okx", InstrumentID: 202,
		Side: "sell", OrderType: "limit", Quantity: "1", Price: "100",
		RequestFingerprint: "fp-1", ArbitrageExecutionID: execution.ID,
		ArbitrageLeg: "b", ArbitrageRole: "hedge",
	}
	first, err := store.PrepareArbitrageHedgeIntent(context.Background(), PrepareArbitrageHedgeIntentInput{
		Combination: combo, Execution: execution, Order: intent, ExpectedSequence: 0,
		HedgeSide: "sell", TargetQuantity: "1",
	})
	if err != nil || !first.Created || first.Execution.HedgeSequence != 1 ||
		first.Execution.HedgeOrderID == "" || first.Combination.RuntimeState != "hedging" {
		t.Fatalf("first prepare=%+v err=%v", first, err)
	}
	replay, err := store.PrepareArbitrageHedgeIntent(context.Background(), PrepareArbitrageHedgeIntentInput{
		Combination: combo, Execution: first.Execution, Order: intent, ExpectedSequence: 0,
		HedgeSide: "sell", TargetQuantity: "1",
	})
	if err != nil || replay.Created || replay.Execution.HedgeSequence != 1 ||
		replay.Execution.HedgeOrderID != first.Execution.HedgeOrderID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	started := 0
	for _, eventType := range store.eventTypes {
		if eventType == "hedge_started" {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("hedge_started=%d", started)
	}

	residualStore := &executorFailStore{combo: combo, intentStore: newMemoryStore()}
	residualExec := ArbitrageExecution{ID: "exec-residual", CombinationID: combo.ID}
	residualIntent := Order{
		IdempotencyKey: "arb:exec-residual:b:residual:0", OwnerUsername: "admin",
		TradingAccountID: 2, Exchange: "okx", InstrumentID: 202,
		Side: "sell", OrderType: "limit", Quantity: "1", Price: "100",
		RequestFingerprint: "fp-residual", ArbitrageExecutionID: residualExec.ID,
		ArbitrageLeg: "b", ArbitrageRole: "residual",
	}
	firstResidual, err := residualStore.PrepareArbitrageHedgeIntent(context.Background(), PrepareArbitrageHedgeIntentInput{
		Combination: combo, Execution: residualExec, Order: residualIntent, ExpectedSequence: 0,
		HedgeSide: "sell", TargetQuantity: "1",
	})
	if err != nil || !firstResidual.Created || firstResidual.Order.ArbitrageRole != "residual" {
		t.Fatalf("residual first=%+v err=%v", firstResidual, err)
	}
	replayResidual, err := residualStore.PrepareArbitrageHedgeIntent(context.Background(), PrepareArbitrageHedgeIntentInput{
		Combination: combo, Execution: firstResidual.Execution, Order: residualIntent, ExpectedSequence: 0,
		HedgeSide: "sell", TargetQuantity: "1",
	})
	if err != nil || replayResidual.Created ||
		replayResidual.Order.ID != firstResidual.Order.ID ||
		replayResidual.Order.ArbitrageRole != "residual" {
		t.Fatalf("residual replay=%+v err=%v", replayResidual, err)
	}

	protected := &executorFailStore{
		combo: ArbitrageCombination{
			ID: "combo-2", RuntimeState: "position_uncertain", PositionUncertain: true,
		},
		intentStore: newMemoryStore(),
	}
	protectedExec := ArbitrageExecution{ID: "exec-2"}
	protectedIntent := intent
	protectedIntent.IdempotencyKey = "arb:exec-2:b:hedge:0"
	protectedIntent.ArbitrageExecutionID = protectedExec.ID
	got, err := protected.PrepareArbitrageHedgeIntent(context.Background(), PrepareArbitrageHedgeIntentInput{
		Combination: protected.combo, Execution: protectedExec, Order: protectedIntent,
		ExpectedSequence: 0,
	})
	if err != nil || got.Combination.RuntimeState != "position_uncertain" {
		t.Fatalf("protected runtime=%+v err=%v", got, err)
	}

	advanced := &executorFailStore{
		combo: combo, intentStore: orders,
		execution: ArbitrageExecution{ID: "exec-1", HedgeSequence: 1},
	}
	repaired, err := advanced.PrepareArbitrageHedgeIntent(context.Background(), PrepareArbitrageHedgeIntentInput{
		Combination: combo, Execution: ArbitrageExecution{ID: "exec-1", HedgeSequence: 1},
		Order: intent, ExpectedSequence: 0,
	})
	if err != nil || repaired.Created || repaired.Execution.HedgeSequence != 1 ||
		repaired.Execution.HedgeOrderID != first.Order.ID {
		t.Fatalf("sequence-already-advanced repair=%+v err=%v", repaired, err)
	}
}

func TestPrepareFailureDoesNotPlaceHedgeOrder(t *testing.T) {
	store := &executorFailStore{
		combo:       ArbitrageCombination{ID: "combo-1"},
		intentStore: newMemoryStore(),
		prepareErr:  errors.New("intent persist failed"),
	}
	_, err := store.PrepareArbitrageHedgeIntent(context.Background(), PrepareArbitrageHedgeIntentInput{
		Execution: ArbitrageExecution{ID: "exec-1"},
		Order:     Order{IdempotencyKey: "arb:exec-1:b:hedge:0"},
	})
	if err == nil {
		t.Fatal("expected prepare failure")
	}
}

func TestSubmitPreparedSkipsDuplicateSubmittedEvent(t *testing.T) {
	store := newMemoryStore()
	service := NewService(store, nil, nil, nil, time.Second, nil)
	intent, _, err := store.CreateIntent(context.Background(), Order{
		IdempotencyKey: "hedge-skip-submitted", OwnerUsername: "admin",
		TradingAccountID: 1, Exchange: "okx", Side: "sell", OrderType: "limit",
		Quantity: "1", Price: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = store.AppendEvent(context.Background(), intent.ID, "intent", nil)
	_ = store.AppendEvent(context.Background(), intent.ID, "submitted", nil)
	adapter := &stubAdapter{place: exchange.Result{Status: "filled", FilledQuantity: "1"}}
	if _, err := service.submitPrepared(
		context.Background(), adapter,
		Credentials{TradingAccountID: 1, Exchange: "okx"},
		Instrument{ExchangeSymbol: "BTC-USDT"}, intent, "IOC", false,
	); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	submitted := 0
	for _, eventType := range store.events {
		if eventType == "submitted" {
			submitted++
		}
	}
	if submitted != 1 {
		t.Fatalf("submitted events=%v", store.events)
	}
}

func TestSubmitPreparedDoesNotRetryPostgresDeadlock(t *testing.T) {
	store := newMemoryStore()
	service := NewService(store, nil, nil, nil, time.Second, nil)
	intent, _, err := store.CreateIntent(context.Background(), Order{
		IdempotencyKey: "hedge-deadlock", OwnerUsername: "admin",
		TradingAccountID: 1, Exchange: "okx", Side: "sell", OrderType: "limit",
		Quantity: "1", Price: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &stubAdapter{err: &pgconn.PgError{Code: postgresDeadlockSQLState, Message: "deadlock detected"}}
	_, err = service.submitPrepared(
		context.Background(), adapter,
		Credentials{TradingAccountID: 1, Exchange: "okx"},
		Instrument{ExchangeSymbol: "BTC-USDT"}, intent, "IOC", false,
	)
	if err == nil {
		t.Fatal("expected place error")
	}
	if adapter.calls != 1 {
		t.Fatalf("PlaceOrder calls=%d err=%v", adapter.calls, err)
	}
}

func TestAggregateCarryStubReplacesTerminalHedge(t *testing.T) {
	orders := newMemoryStore()
	combo := ArbitrageCombination{
		ID: "combo-1", OwnerUsername: "admin", CarryBaseQuantity: "3343",
		CircuitOpen: true, RuntimeState: "manual_intervention",
	}
	old, _, err := orders.CreateIntent(context.Background(), Order{
		IdempotencyKey: "arb:exec-1:b:hedge:2", OwnerUsername: "admin",
		Quantity: "3343", Side: "sell", OrderType: "limit",
		ArbitrageExecutionID: "exec-1", ArbitrageLeg: "b", ArbitrageRole: "hedge",
	})
	if err != nil {
		t.Fatal(err)
	}
	old, err = orders.UpdateResult(context.Background(), old.ID, VenueResult{
		Status: "rejected", FilledQuantity: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &executorFailStore{
		combo: combo, intentStore: orders,
		execution: ArbitrageExecution{
			ID: "exec-1", HedgeOrderID: old.ID, HedgeSequence: 3, Status: "reconciling",
		},
	}
	intent := Order{
		IdempotencyKey: "arb:exec-1:b:hedge:3", OwnerUsername: "admin",
		Quantity: "3343", RequestFingerprint: "fp-new",
		ArbitrageExecutionID: "exec-1", ArbitrageLeg: "b", ArbitrageRole: "hedge",
	}
	got, err := store.PrepareArbitrageHedgeIntent(context.Background(), PrepareArbitrageHedgeIntentInput{
		Combination: combo, Execution: ArbitrageExecution{ID: "exec-1"},
		Order: intent, ExpectedSequence: 3, AggregateCarry: true,
		ExpectedCarryQuantity: "3343",
	})
	if err != nil || !got.Created || got.Execution.HedgeSequence != 4 ||
		got.Execution.HedgeOrderID == old.ID {
		t.Fatalf("replace=%+v err=%v", got, err)
	}
	if !got.Combination.CircuitOpen || got.Combination.RuntimeState != "manual_intervention" {
		t.Fatalf("combo=%+v", got.Combination)
	}
	replay, err := store.PrepareArbitrageHedgeIntent(context.Background(), PrepareArbitrageHedgeIntentInput{
		Combination: combo, Execution: got.Execution, Order: intent,
		ExpectedSequence: 3, AggregateCarry: true, ExpectedCarryQuantity: "3343",
	})
	if err != nil || replay.Created || replay.Execution.HedgeSequence != 4 ||
		replay.Execution.HedgeOrderID != got.Execution.HedgeOrderID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

func TestAggregateCarryStubRejectsPendingAndLiveHedge(t *testing.T) {
	t.Run("pending_pointer", func(t *testing.T) {
		orders := newMemoryStore()
		old, _, err := orders.CreateIntent(context.Background(), Order{
			IdempotencyKey: "arb:exec-p:b:hedge:2", OwnerUsername: "admin",
			Quantity: "1", ArbitrageExecutionID: "exec-p",
			ArbitrageLeg: "b", ArbitrageRole: "hedge",
		})
		if err != nil {
			t.Fatal(err)
		}
		store := &executorFailStore{
			combo: ArbitrageCombination{
				ID: "combo-p", OwnerUsername: "admin", CarryBaseQuantity: "1",
			},
			intentStore: orders,
			execution: ArbitrageExecution{
				ID: "exec-p", HedgeOrderID: old.ID, HedgeSequence: 3,
			},
		}
		_, err = store.PrepareArbitrageHedgeIntent(context.Background(), PrepareArbitrageHedgeIntentInput{
			Combination: store.combo, Execution: ArbitrageExecution{ID: "exec-p"},
			Order: Order{
				IdempotencyKey: "arb:exec-p:b:hedge:3", Quantity: "1",
				ArbitrageExecutionID: "exec-p", ArbitrageLeg: "b", ArbitrageRole: "hedge",
			},
			ExpectedSequence: 3, AggregateCarry: true, ExpectedCarryQuantity: "1",
		})
		if !errors.Is(err, ErrArbitrageHedgeAdmissionConflict) {
			t.Fatalf("err=%v", err)
		}
		if store.execution.HedgeSequence != 3 || store.execution.HedgeOrderID != old.ID {
			t.Fatalf("execution=%+v", store.execution)
		}
	})
	t.Run("live_beside_terminal", func(t *testing.T) {
		orders := newMemoryStore()
		old, _, err := orders.CreateIntent(context.Background(), Order{
			IdempotencyKey: "arb:exec-l:b:hedge:2", OwnerUsername: "admin",
			Quantity: "1", ArbitrageExecutionID: "exec-l",
			ArbitrageLeg: "b", ArbitrageRole: "hedge",
		})
		if err != nil {
			t.Fatal(err)
		}
		old, err = orders.UpdateResult(context.Background(), old.ID, VenueResult{
			Status: "rejected", FilledQuantity: "0",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := orders.CreateIntent(context.Background(), Order{
			IdempotencyKey: "arb:exec-l:b:hedge:live", OwnerUsername: "admin",
			Quantity: "1", ArbitrageExecutionID: "exec-l",
			ArbitrageLeg: "b", ArbitrageRole: "hedge",
		}); err != nil {
			t.Fatal(err)
		}
		store := &executorFailStore{
			combo: ArbitrageCombination{
				ID: "combo-l", OwnerUsername: "admin", CarryBaseQuantity: "1",
			},
			intentStore: orders,
			execution: ArbitrageExecution{
				ID: "exec-l", HedgeOrderID: old.ID, HedgeSequence: 3,
			},
		}
		_, err = store.PrepareArbitrageHedgeIntent(context.Background(), PrepareArbitrageHedgeIntentInput{
			Combination: store.combo, Execution: ArbitrageExecution{ID: "exec-l"},
			Order: Order{
				IdempotencyKey: "arb:exec-l:b:hedge:3", Quantity: "1",
				ArbitrageExecutionID: "exec-l", ArbitrageLeg: "b", ArbitrageRole: "hedge",
			},
			ExpectedSequence: 3, AggregateCarry: true, ExpectedCarryQuantity: "1",
		})
		if !errors.Is(err, ErrArbitrageHedgeAdmissionConflict) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestHedgeFastPathStubRejectsTerminalPointer(t *testing.T) {
	orders := newMemoryStore()
	maker, _, err := orders.CreateIntent(context.Background(), Order{
		IdempotencyKey: "arb:exec-f:a:maker:0", OwnerUsername: "admin",
		Quantity: "1", FilledQuantity: "1", ArbitrageExecutionID: "exec-f",
		ArbitrageLeg: "a", ArbitrageRole: "maker",
	})
	if err != nil {
		t.Fatal(err)
	}
	maker, err = orders.UpdateResult(context.Background(), maker.ID, VenueResult{
		Status: "filled", FilledQuantity: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := orders.CreateIntent(context.Background(), Order{
		IdempotencyKey: "arb:exec-f:b:hedge:0", OwnerUsername: "admin",
		Quantity: "1", ArbitrageExecutionID: "exec-f",
		ArbitrageLeg: "b", ArbitrageRole: "hedge",
	})
	if err != nil {
		t.Fatal(err)
	}
	old, err = orders.UpdateResult(context.Background(), old.ID, VenueResult{
		Status: "rejected", FilledQuantity: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &executorFailStore{
		combo: ArbitrageCombination{
			ID: "combo-f", OwnerUsername: "admin", MakerLeg: "a",
		},
		intentStore: orders,
		execution: ArbitrageExecution{
			ID: "exec-f", Status: "hedging", MakerOrderID: maker.ID,
			HedgeOrderID: old.ID, HedgeSequence: 1, LegBFilledQuantity: "0",
		},
	}
	_, err = store.PrepareArbitrageHedgeIntent(context.Background(), PrepareArbitrageHedgeIntentInput{
		Combination: store.combo, Execution: store.execution,
		Order: Order{
			IdempotencyKey: "arb:exec-f:b:hedge:1", Quantity: "1",
			ArbitrageExecutionID: "exec-f", ArbitrageLeg: "b", ArbitrageRole: "hedge",
		},
		ExpectedSequence: 1, FastPathAdmission: true,
		ConfirmedMakerFilled: "1", ConfirmedHedgeFilled: "0",
	})
	if !errors.Is(err, ErrArbitrageHedgeAdmissionConflict) {
		t.Fatalf("err=%v", err)
	}
	if store.execution.HedgeSequence != 1 || store.execution.HedgeOrderID != old.ID {
		t.Fatalf("execution=%+v", store.execution)
	}
}
