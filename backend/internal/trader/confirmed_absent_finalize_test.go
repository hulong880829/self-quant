package trader

import (
	"context"
	"testing"
	"time"

	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

func TestEvaluateConfirmedAbsentZeroFillFinalize(t *testing.T) {
	combo := ArbitrageCombination{
		Status: "running", PositionUncertain: true,
		RuntimeState: "position_uncertain",
		ErrorMessage: arbitrageOrderReconcileError,
	}
	execution := ArbitrageExecution{
		ID: "exec-1", Status: "maker_open",
		LegAFilledQuantity: "0", LegBFilledQuantity: "0",
	}
	ghost := Order{
		ID: "ghost", ArbitrageExecutionID: "exec-1", Status: "rejected",
		FilledQuantity: "0", ErrorCode: errorConfirmedAbsentAfterUncertainSubmit,
	}
	if got := evaluateConfirmedAbsentZeroFillFinalize(
		combo, execution, []Order{ghost}, []ArbitrageExecution{execution},
	); got != confirmedAbsentFinalizeCanceledExec {
		t.Fatalf("got=%s", got)
	}

	backoff := combo
	backoff.PositionUncertain = false
	backoff.RuntimeState = "backoff"
	backoff.ErrorMessage = ""
	if got := evaluateConfirmedAbsentZeroFillFinalize(
		backoff, execution, []Order{ghost}, []ArbitrageExecution{execution},
	); got != confirmedAbsentFinalizeCanceledExec {
		t.Fatalf("backoff got=%s", got)
	}

	openCircuit := combo
	openCircuit.CircuitOpen = true
	if got := evaluateConfirmedAbsentZeroFillFinalize(
		openCircuit, execution, []Order{ghost}, []ArbitrageExecution{execution},
	); got != confirmedAbsentFinalizeSkipped {
		t.Fatalf("circuit open got=%s", got)
	}

	filledLeg := execution
	filledLeg.LegAFilledQuantity = "0.01"
	if got := evaluateConfirmedAbsentZeroFillFinalize(
		combo, filledLeg, []Order{ghost}, []ArbitrageExecution{filledLeg},
	); got != confirmedAbsentFinalizeSkipped {
		t.Fatalf("leg fill got=%s", got)
	}

	filledZero := ghost
	filledZero.ID = "filled-zero"
	filledZero.Status = "filled"
	filledZero.ErrorCode = ""
	if got := evaluateConfirmedAbsentZeroFillFinalize(
		combo, execution, []Order{ghost, filledZero}, []ArbitrageExecution{execution},
	); got != confirmedAbsentFinalizeSkipped {
		t.Fatalf("filled zero got=%s", got)
	}

	otherLive := ArbitrageExecution{ID: "exec-2", Status: "hedge_open"}
	if got := evaluateConfirmedAbsentZeroFillFinalize(
		combo, execution, []Order{ghost}, []ArbitrageExecution{execution, otherLive},
	); got != confirmedAbsentFinalizeCanceledExec {
		t.Fatalf("other live exec got=%s", got)
	}

	otherOpen := Order{
		ID: "live", ArbitrageExecutionID: "exec-2", Status: "open",
		FilledQuantity: "0",
	}
	if got := evaluateConfirmedAbsentZeroFillFinalize(
		combo, execution, []Order{ghost, otherOpen},
		[]ArbitrageExecution{execution, {ID: "exec-2", Status: "completed"}},
	); got != confirmedAbsentFinalizeCanceledExec {
		t.Fatalf("other live order got=%s", got)
	}

	plain := ghost
	plain.ErrorCode = ""
	plain.Status = "canceled"
	if got := evaluateConfirmedAbsentZeroFillFinalize(
		combo, execution, []Order{plain}, []ArbitrageExecution{execution},
	); got != confirmedAbsentFinalizeSkipped {
		t.Fatalf("healthy canceled got=%s", got)
	}
}

func TestSettleCanceledOrderConfirmedAbsentSkipsREST(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	combo := ArbitrageCombination{
		ID: "combo-absent", OwnerUsername: "admin", Status: "running",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 101, Exchange: "hyperliquid",
			ExchangeSymbol: "BTC", ContractType: "perpetual",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 202, Exchange: "okx",
			ExchangeSymbol: "BTC-USDT-SWAP", ContractType: "perpetual",
		},
	}
	store := &executorFailStore{combo: combo}
	adapter := &countingCancelAdapter{allowGet: true}
	catalog := silentRiskCatalog{
		101: Instrument{
			ID: 101, Exchange: "hyperliquid", ContractType: "perpetual",
			ExchangeSymbol: "BTC", QuantityStep: "1", PriceTick: "0.1",
		},
		202: Instrument{
			ID: 202, Exchange: "okx", ContractType: "perpetual",
			ExchangeSymbol: "BTC-USDT-SWAP", QuantityStep: "1", PriceTick: "0.1",
		},
	}
	executor := NewArbitrageExecutor(
		store, orderStore, NewService(orderStore, nil, nil, nil, time.Second, nil),
		catalog, silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"hyperliquid": adapter, "okx": adapter,
		}),
		nil, "", 20*time.Millisecond, 0, 0, 0, time.Second, nil,
	)
	ghost := Order{
		ID: "ghost", OwnerUsername: "admin", Status: "rejected",
		FilledQuantity: "0", ErrorCode: errorConfirmedAbsentAfterUncertainSubmit,
		Exchange: "hyperliquid",
	}
	got, err := executor.settleCanceledOrder(
		context.Background(), Credentials{}, catalog[101], adapter, ghost, orderObservation{},
	)
	if err != nil || got.Status != "rejected" || adapter.getCalls() != 0 {
		t.Fatalf("order=%+v gets=%d err=%v", got, adapter.getCalls(), err)
	}

	plain := ghost
	plain.ErrorCode = ""
	plain.Status = "canceled"
	if _, err := executor.settleCanceledOrder(
		context.Background(), Credentials{}, catalog[101], adapter, plain, orderObservation{},
	); err == nil && adapter.getCalls() == 0 {
		t.Fatal("plain canceled must still query")
	}
	if adapter.getCalls() == 0 {
		t.Fatal("plain canceled must still query")
	}

	invalid := ghost
	invalid.Status = "canceled"
	invalid.FilledQuantity = "abc"
	invalid.ErrorCode = ""
	before := adapter.getCalls()
	_, _ = executor.settleCanceledOrder(
		context.Background(), Credentials{}, catalog[101], adapter, invalid, orderObservation{},
	)
	if adapter.getCalls() == before {
		t.Fatal("illegal filled quantity must still query")
	}
}

func TestRecoverWithoutMarketDataConfirmedAbsentRecoveredSkipsReconciling(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61757", OwnerUsername: "admin",
		Status: "running", ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		PositionUncertain: true, RuntimeState: "position_uncertain",
		ErrorMessage: arbitrageOrderReconcileError,
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 101, Exchange: "hyperliquid",
			ExchangeSymbol: "BTC", ContractType: "perpetual",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 202, Exchange: "okx",
			ExchangeSymbol: "BTC-USDT-SWAP", ContractType: "perpetual",
		},
	}
	execution := ArbitrageExecution{
		ID: "exec-recover", Status: "maker_open", MakerOrderID: "maker-absent",
		LegAFilledQuantity: "0", LegBFilledQuantity: "0",
	}
	store := &executorFailStore{
		combo: combo, execution: execution,
		finalizeResult: confirmedAbsentFinalizeCanceledExec,
	}
	adapter := &countingCancelAdapter{allowGet: true}
	intent := Order{
		ID: "maker-absent", IdempotencyKey: "recover-absent", OwnerUsername: "admin",
		TradingAccountID: 1, Exchange: "hyperliquid", Quantity: "1",
		FilledQuantity: "0", Status: "rejected",
		ErrorCode:            errorConfirmedAbsentAfterUncertainSubmit,
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "maker",
	}
	orderStore.mu.Lock()
	orderStore.orders[intent.ID] = intent
	orderStore.mu.Unlock()
	catalog := silentRiskCatalog{
		101: Instrument{
			ID: 101, Exchange: "hyperliquid", ContractType: "perpetual",
			ExchangeSymbol: "BTC", QuantityStep: "1", PriceTick: "0.1",
		},
		202: Instrument{
			ID: 202, Exchange: "okx", ContractType: "perpetual",
			ExchangeSymbol: "BTC-USDT-SWAP", QuantityStep: "1", PriceTick: "0.1",
		},
	}
	executor := NewArbitrageExecutor(
		store, orderStore, NewService(orderStore, nil, nil, nil, time.Second, nil),
		catalog, silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"hyperliquid": adapter, "okx": adapter,
		}),
		nil, "", 20*time.Millisecond, 0, 0, 0, time.Second, nil,
	)
	if err := executor.RecoverWithoutMarketData(context.Background(), combo, execution); err != nil {
		t.Fatal(err)
	}
	if adapter.getCalls() != 0 {
		t.Fatalf("GetOrder calls=%d", adapter.getCalls())
	}
	if store.finalizeCalls != 1 {
		t.Fatalf("finalizeCalls=%d", store.finalizeCalls)
	}
	if store.execution.Status == "reconciling" || store.combo.RuntimeState == "reconciling" {
		t.Fatalf("execution=%+v combo=%+v", store.execution, store.combo)
	}
}

func TestRecoverWithoutMarketDataConfirmedAbsentSkippedDoesNotCancel(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61758", OwnerUsername: "admin",
		Status: "running",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 101, Exchange: "hyperliquid",
			ExchangeSymbol: "BTC", ContractType: "perpetual",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 202, Exchange: "okx",
			ExchangeSymbol: "BTC-USDT-SWAP", ContractType: "perpetual",
		},
	}
	execution := ArbitrageExecution{
		ID: "exec-skip", Status: "maker_open", MakerOrderID: "maker-skip",
	}
	active := execution
	store := &executorFailStore{
		combo: combo, execution: execution, activeExecution: &active,
		finalizeResult: confirmedAbsentFinalizeSkipped,
	}
	adapter := &countingCancelAdapter{allowGet: true}
	intent := Order{
		ID: "maker-skip", IdempotencyKey: "recover-skip", OwnerUsername: "admin",
		TradingAccountID: 1, Exchange: "hyperliquid", Quantity: "1",
		FilledQuantity: "0", Status: "rejected",
		ErrorCode:            errorConfirmedAbsentAfterUncertainSubmit,
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "maker",
	}
	orderStore.mu.Lock()
	orderStore.orders[intent.ID] = intent
	orderStore.mu.Unlock()
	catalog := silentRiskCatalog{
		101: Instrument{
			ID: 101, Exchange: "hyperliquid", ContractType: "perpetual",
			ExchangeSymbol: "BTC", QuantityStep: "1", PriceTick: "0.1",
		},
		202: Instrument{
			ID: 202, Exchange: "okx", ContractType: "perpetual",
			ExchangeSymbol: "BTC-USDT-SWAP", QuantityStep: "1", PriceTick: "0.1",
		},
	}
	executor := NewArbitrageExecutor(
		store, orderStore, NewService(orderStore, nil, nil, nil, time.Second, nil),
		catalog, silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"hyperliquid": adapter, "okx": adapter,
		}),
		nil, "", 20*time.Millisecond, 0, 0, 0, time.Second, nil,
	)
	if err := executor.RecoverWithoutMarketData(context.Background(), combo, execution); err != nil {
		t.Fatal(err)
	}
	if adapter.getCalls() != 0 {
		t.Fatalf("GetOrder calls=%d", adapter.getCalls())
	}
	if store.execution.Status != "reconciling" {
		t.Fatalf("skipped must fall back to reconciling, status=%s", store.execution.Status)
	}
	if store.execution.Status == "canceled" || store.execution.Status == "completed" {
		t.Fatal("skipped must not mark execution canceled or completed")
	}
}

func TestArbitrageSchedulerMonitoringFinalizesConfirmedAbsent(t *testing.T) {
	item := ArbitrageCombination{
		ID: "combo-1", Status: "running", RuntimeState: "monitoring",
		LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "binance"},
		LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "okx"},
	}
	store := &dryRunStore{
		item: item,
		activeExecution: &ArbitrageExecution{
			ID: "exec-1", CombinationID: item.ID, Status: "maker_open",
		},
	}
	scheduler := NewArbitrageScheduler(
		store, nil, nil, time.Second, time.Millisecond, time.Second,
		1, 1, 1, 2, false, discardLogger(),
	)
	runtime := &arbitrageRuntime{
		combination: item,
		legA:        &marketdata.Subscription{},
		legB:        &marketdata.Subscription{},
	}
	scheduler.handleControl(context.Background(), runtime)
	if store.finalizeCalls != 1 || store.lastFinalizeID != "exec-1" {
		t.Fatalf("finalizeCalls=%d id=%s", store.finalizeCalls, store.lastFinalizeID)
	}
}

func TestArbitrageSchedulerBackoffFinalizesConfirmedAbsent(t *testing.T) {
	item := ArbitrageCombination{
		ID: "combo-1", Status: "running", RuntimeState: "backoff",
		NextRetryAt: time.Now().UTC().Add(time.Hour),
		LegA:        ArbitrageLeg{TradingAccountID: 1, Exchange: "binance"},
		LegB:        ArbitrageLeg{TradingAccountID: 2, Exchange: "okx"},
	}
	store := &dryRunStore{
		item: item,
		activeExecution: &ArbitrageExecution{
			ID: "exec-1", CombinationID: item.ID, Status: "maker_open",
		},
	}
	scheduler := NewArbitrageScheduler(
		store, nil, nil, time.Second, time.Millisecond, time.Second,
		1, 1, 1, 2, false, discardLogger(),
	)
	runtime := &arbitrageRuntime{
		combination: item,
		legA:        &marketdata.Subscription{},
		legB:        &marketdata.Subscription{},
	}
	scheduler.handleControl(context.Background(), runtime)
	if store.finalizeCalls != 1 || store.lastFinalizeID != "exec-1" {
		t.Fatalf("finalizeCalls=%d id=%s", store.finalizeCalls, store.lastFinalizeID)
	}
}

func TestArbitrageSchedulerCircuitOpenDoesNotFinalizeConfirmedAbsent(t *testing.T) {
	item := ArbitrageCombination{
		ID: "combo-1", Status: "running", CircuitOpen: true, RuntimeState: "monitoring",
		LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "binance"},
		LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "okx"},
	}
	store := &dryRunStore{
		item: item,
		activeExecution: &ArbitrageExecution{
			ID: "exec-1", CombinationID: item.ID, Status: "maker_open",
		},
	}
	scheduler := NewArbitrageScheduler(
		store, nil, nil, time.Second, time.Millisecond, time.Second,
		1, 1, 1, 2, false, discardLogger(),
	)
	runtime := &arbitrageRuntime{
		combination: item,
		legA:        &marketdata.Subscription{},
		legB:        &marketdata.Subscription{},
	}
	scheduler.handleControl(context.Background(), runtime)
	if store.finalizeCalls != 0 {
		t.Fatalf("finalizeCalls=%d", store.finalizeCalls)
	}
}

func TestArbitrageSchedulerConfirmedAbsentFinalizeThrottleAndRunningGuard(t *testing.T) {
	item := ArbitrageCombination{
		ID: "combo-1", Status: "running", PositionUncertain: true,
		RuntimeState: "position_uncertain", ErrorMessage: arbitrageOrderReconcileError,
		LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "binance"},
		LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "okx"},
	}
	execution := ArbitrageExecution{
		ID: "exec-1", CombinationID: item.ID, Status: "maker_open",
	}
	store := &dryRunStore{item: item, activeExecution: &execution}
	scheduler := NewArbitrageScheduler(
		store, nil, nil, time.Second, time.Millisecond, time.Second,
		1, 1, 1, 2, false, discardLogger(),
	)
	runtime := &arbitrageRuntime{
		combination: item,
		legA:        &marketdata.Subscription{},
		legB:        &marketdata.Subscription{},
	}
	scheduler.handleControl(context.Background(), runtime)
	scheduler.handleControl(context.Background(), runtime)
	if store.finalizeCalls != 1 || store.lastFinalizeID != execution.ID {
		t.Fatalf("finalizeCalls=%d id=%s", store.finalizeCalls, store.lastFinalizeID)
	}

	blocked := &arbitrageRuntime{
		combination: item,
		legA:        &marketdata.Subscription{},
		legB:        &marketdata.Subscription{},
	}
	blocked.executionRunning.Store(true)
	blockedStore := &dryRunStore{item: item, activeExecution: &execution}
	blockedScheduler := NewArbitrageScheduler(
		blockedStore, nil, nil, time.Second, time.Millisecond, time.Second,
		1, 1, 1, 2, false, discardLogger(),
	)
	blockedScheduler.handleControl(context.Background(), blocked)
	if blockedStore.finalizeCalls != 0 {
		t.Fatalf("executionRunning finalizeCalls=%d", blockedStore.finalizeCalls)
	}
}
