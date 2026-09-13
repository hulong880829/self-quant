package trader

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

func lastClipClosingHandoffCombo() ArbitrageCombination {
	combo := lastClipCloseCombination("bid")
	combo.LegABasePosition = "10"
	combo.LegBBasePosition = "-10"
	combo.LegA.Exchange = "gate"
	combo.LegA.ExchangeSymbol = "USELESS_USDT"
	combo.LegB.Exchange = "bybit"
	combo.LegB.ExchangeSymbol = "USELESSUSDT"
	return combo
}

func lastClipHandoffExecution(combo ArbitrageCombination) ArbitrageExecution {
	return ArbitrageExecution{
		ID: "last-clip-handoff", CombinationID: combo.ID,
		Status: "hedging", Direction: "bid", PositionEffect: "close",
		ReduceOnly: true, LastCloseClip: true,
		TargetBaseQuantity: "10", RequestedNotional: "2.435",
		MakerOrderID: "maker-1", HedgeOrderID: "hedge-1",
		LegAFilledQuantity: "10",
	}
}

func lastClipHandoffOrders(executionID string, hedgeStatus, hedgeFill, hedgeMessage string) []Order {
	now := time.Now().UTC()
	return []Order{
		{
			ID: "maker-1", ArbitrageExecutionID: executionID, ArbitrageRole: "maker",
			Status: "filled", FilledQuantity: "10", UpdatedAt: now.Add(-time.Second),
		},
		{
			ID: "hedge-1", ArbitrageExecutionID: executionID, ArbitrageRole: "hedge",
			Status: hedgeStatus, FilledQuantity: hedgeFill, ErrorMessage: hedgeMessage,
			UpdatedAt: now,
		},
	}
}

func TestLastClipHedgeMinNotionalErrorMatching(t *testing.T) {
	if !lastClipHedgeMinNotionalError(ErrOrderBelowMinimum) {
		t.Fatal("ErrOrderBelowMinimum")
	}
	if !lastClipHedgeMinNotionalError(fmtInvalidQuantity("quantity below venue minimum")) {
		t.Fatal("invalid quantity below venue minimum")
	}
	if lastClipHedgeMinNotionalError(fmtInvalidQuantity("quantity does not match venue step")) {
		t.Fatal("step mismatch must not handoff")
	}
	if lastClipHedgeMinNotionalError(fmt.Errorf("%w: generic", ErrVenueRejected)) {
		t.Fatal("bare venue rejected must not handoff")
	}
	if !lastClipHedgeMinNotionalError(errors.New("Order must have minimum value of $10")) {
		t.Fatal("hyperliquid minimum value")
	}
	if lastClipHedgeMinNotionalError(ErrVenueUncertain) {
		t.Fatal("uncertain must not handoff")
	}
}

func fmtInvalidQuantity(message string) error {
	return fmt.Errorf("%w: %s", exchange.ErrInvalidQuantity, message)
}

func TestLastClipHedgeMinNotionalRejectedFillMath(t *testing.T) {
	combo := lastClipClosingHandoffCombo()
	execution := lastClipHandoffExecution(combo)
	cause := errors.New("Order must have minimum value of $10")
	if !lastClipHedgeMinNotionalRejected(combo, execution, cause, lastClipHandoffOrders(execution.ID, "rejected", "0", "minimum value")) {
		t.Fatal("expected handoff")
	}
	covered := lastClipHandoffOrders(execution.ID, "rejected", "0", "minimum value")
	covered = append(covered, Order{
		ID: "hedge-0", ArbitrageExecutionID: execution.ID, ArbitrageRole: "hedge",
		Status: "filled", FilledQuantity: "10", UpdatedAt: time.Now().UTC().Add(-time.Minute),
	})
	if lastClipHedgeMinNotionalRejected(combo, execution, cause, covered) {
		t.Fatal("unhedgedBase <= 0 must not handoff")
	}
	pending := lastClipHandoffOrders(execution.ID, "open", "0", "minimum value")
	if lastClipHedgeMinNotionalRejected(combo, execution, cause, pending) {
		t.Fatal("live hedge must not handoff")
	}
	exiting := combo
	exiting.Status = "running"
	exiting.RunMode = "one_shot"
	exiting.OneShotPhase = "exiting"
	if lastClipHedgeMinNotionalRejected(
		exiting, execution, cause, lastClipHandoffOrders(execution.ID, "rejected", "0", "minimum value"),
	) {
		t.Fatal("one-shot exiting must not handoff")
	}
}

type lastClipHandoffStore struct {
	*executorFailStore
	mu             sync.Mutex
	failNextFailed bool
	failedAttempts int
	closeKeys      []string
}

func (s *lastClipHandoffStore) RecordArbitrageCloseFailure(
	ctx context.Context, id, key, message string,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	s.closeKeys = append(s.closeKeys, key)
	s.mu.Unlock()
	return s.executorFailStore.RecordArbitrageCloseFailure(ctx, id, key, message)
}

func (s *lastClipHandoffStore) UpdateArbitrageExecution(
	ctx context.Context, execution ArbitrageExecution,
) (ArbitrageExecution, error) {
	s.mu.Lock()
	fail := s.failNextFailed && execution.Status == "failed"
	if fail {
		s.failedAttempts++
		s.failNextFailed = false
		s.mu.Unlock()
		return ArbitrageExecution{}, errors.New("db unavailable")
	}
	s.mu.Unlock()
	return s.executorFailStore.UpdateArbitrageExecution(ctx, execution)
}

func newLastClipHandoffExecutor(
	t *testing.T,
	combo ArbitrageCombination,
	store *lastClipHandoffStore,
	hedgeErr error,
	hedgeMessage string,
	iocRetries int,
) (*ArbitrageExecutor, marketdata.BBO, *stubAdapter) {
	t.Helper()
	makerInstrument := oneShotExitingLastClipInstrument(101, "gate", "USELESS_USDT")
	hedgeInstrument := oneShotExitingLastClipInstrument(202, "bybit", "USELESSUSDT")
	market := newFixedPriceMarket(t, "bybit", "USELESSUSDT", "0.2435", "0.2435")
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	store.intentStore = orderStore
	gate := &stubAdapter{place: exchange.Result{
		VenueOrderID: "maker-1", Status: "filled",
		FilledQuantity: "10", AveragePrice: "0.2435",
	}}
	bybit := &stubAdapter{
		err: hedgeErr,
		place: exchange.Result{
			Status: "rejected", ErrorCode: "10001", ErrorMessage: hedgeMessage,
		},
	}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{101: makerInstrument, 202: hedgeInstrument},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"gate": gate, "bybit": bybit,
		}),
		market, "", time.Millisecond, 0, 0, iocRetries, time.Second, nil,
	)
	bbo := marketdata.BBO{
		BidPrice: "0.2435", AskPrice: "0.2435",
		BidQuantity: "100000", AskQuantity: "100000",
	}
	return executor, bbo, bybit
}

func lastClipHandoffStartExecution(combo ArbitrageCombination) ArbitrageExecution {
	return ArbitrageExecution{
		ID: "last-clip-handoff", CombinationID: combo.ID,
		Direction: "bid", PositionEffect: "close", ReduceOnly: true,
		LastCloseClip: true, TargetBaseQuantity: "10", RequestedNotional: "2.435",
	}
}

func TestLastClipHedgeMinNotionalHandoffFailsExecution(t *testing.T) {
	combo := lastClipClosingHandoffCombo()
	store := &lastClipHandoffStore{executorFailStore: &executorFailStore{combo: combo}}
	executor, bbo, bybit := newLastClipHandoffExecutor(
		t, combo, store, exchange.ErrRejected,
		"Order must have minimum value of $10", 2,
	)
	executor.Execute(context.Background(), combo, lastClipHandoffStartExecution(combo), bbo, bbo, nil)
	if len(bybit.requests) != 1 {
		t.Fatalf("hedge requests=%d", len(bybit.requests))
	}
	store.executorFailStore.mu.Lock()
	execution := store.execution
	key := store.combo.LastFailureKey
	events := append([]string(nil), store.eventTypes...)
	store.executorFailStore.mu.Unlock()
	store.mu.Lock()
	keys := append([]string(nil), store.closeKeys...)
	store.mu.Unlock()
	if execution.Status != "failed" {
		t.Fatalf("execution=%+v", execution)
	}
	if execution.LegAFilledQuantity != "10" {
		t.Fatalf("maker fill=%s", execution.LegAFilledQuantity)
	}
	if key != lastClipHedgeMinNotionalFailureKey || len(keys) == 0 || keys[0] != lastClipHedgeMinNotionalFailureKey {
		t.Fatalf("keys=%v last=%s", keys, key)
	}
	for _, item := range keys {
		if item == "close_recovery" {
			t.Fatalf("close_recovery keys=%v", keys)
		}
	}
	found := false
	for _, eventType := range events {
		if eventType == lastClipDustHandoffEvent {
			found = true
		}
	}
	if !found {
		t.Fatalf("events=%v", events)
	}
}

func TestLastClipHedgeMinNotionalHandoffKeepsActiveWhenFailedUpdateFails(t *testing.T) {
	combo := lastClipClosingHandoffCombo()
	store := &lastClipHandoffStore{executorFailStore: &executorFailStore{combo: combo}, failNextFailed: true}
	executor, bbo, bybit := newLastClipHandoffExecutor(
		t, combo, store, exchange.ErrRejected,
		"Order must have minimum value of $10", 2,
	)
	executor.Execute(context.Background(), combo, lastClipHandoffStartExecution(combo), bbo, bbo, nil)
	if len(bybit.requests) != 1 {
		t.Fatalf("first hedge requests=%d", len(bybit.requests))
	}
	store.executorFailStore.mu.Lock()
	execution := store.execution
	key := store.combo.LastFailureKey
	store.executorFailStore.mu.Unlock()
	store.mu.Lock()
	keys := append([]string(nil), store.closeKeys...)
	failedAttempts := store.failedAttempts
	store.mu.Unlock()
	if terminalArbitrageExecutionStatus(execution.Status) {
		t.Fatalf("execution should stay active %+v", execution)
	}
	if key != lastClipHedgeMinNotionalFailureKey || failedAttempts != 1 {
		t.Fatalf("key=%s attempts=%d keys=%v", key, failedAttempts, keys)
	}
	for _, item := range keys {
		if item == "close_recovery" {
			t.Fatalf("close_recovery keys=%v", keys)
		}
	}
	store.failNextFailed = false
	store.executorFailStore.mu.Lock()
	retryCombo := store.combo
	retryExecution := store.execution
	store.executorFailStore.mu.Unlock()
	executor.Execute(context.Background(), retryCombo, retryExecution, bbo, bbo, nil)
	if len(bybit.requests) != 1 {
		t.Fatalf("retry must not place another hedge requests=%d", len(bybit.requests))
	}
	store.executorFailStore.mu.Lock()
	retry := store.execution
	store.executorFailStore.mu.Unlock()
	if retry.Status != "failed" {
		t.Fatalf("retry execution=%+v", retry)
	}
}

func TestLastClipHedgeGenericRejectDoesNotHandoff(t *testing.T) {
	combo := lastClipClosingHandoffCombo()
	store := &lastClipHandoffStore{executorFailStore: &executorFailStore{combo: combo}}
	executor, bbo, _ := newLastClipHandoffExecutor(t, combo, store, exchange.ErrRejected, "insufficient margin", 1)
	executor.Execute(context.Background(), combo, lastClipHandoffStartExecution(combo), bbo, bbo, nil)
	store.executorFailStore.mu.Lock()
	execution := store.execution
	key := store.combo.LastFailureKey
	store.executorFailStore.mu.Unlock()
	if execution.Status == "failed" || key == lastClipHedgeMinNotionalFailureKey {
		t.Fatalf("execution=%+v key=%s", execution, key)
	}
}

func TestLastClipHedgeInvalidQuantityStepDoesNotHandoff(t *testing.T) {
	combo := lastClipClosingHandoffCombo()
	store := &lastClipHandoffStore{executorFailStore: &executorFailStore{combo: combo}}
	executor, bbo, _ := newLastClipHandoffExecutor(
		t, combo, store, exchange.ErrInvalidQuantity, "quantity does not match venue step", 1,
	)
	executor.Execute(context.Background(), combo, lastClipHandoffStartExecution(combo), bbo, bbo, nil)
	store.executorFailStore.mu.Lock()
	key := store.combo.LastFailureKey
	status := store.execution.Status
	store.executorFailStore.mu.Unlock()
	if key == lastClipHedgeMinNotionalFailureKey || status == "failed" {
		t.Fatalf("status=%s key=%s", status, key)
	}
}

func TestRecoverWithoutMarketDataLastClipHandoff(t *testing.T) {
	combo := lastClipClosingHandoffCombo()
	execution := lastClipHandoffExecution(combo)
	store := &executorFailStore{
		combo: combo, execution: execution,
		orders: lastClipHandoffOrders(execution.ID, "rejected", "0", "minimum notional"),
	}
	executor := NewArbitrageExecutor(
		store, nil, nil, nil, nil, nil, nil, "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	if err := executor.RecoverWithoutMarketData(context.Background(), combo, execution); err != nil {
		t.Fatal(err)
	}
	if store.execution.Status != "failed" || store.combo.LastFailureKey != lastClipHedgeMinNotionalFailureKey {
		t.Fatalf("execution=%+v combo=%+v", store.execution, store.combo)
	}
}
