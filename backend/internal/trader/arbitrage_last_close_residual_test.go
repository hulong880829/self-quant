package trader

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

func lastCloseResidual604Combo() ArbitrageCombination {
	combo := lastClipCloseCombination("bid")
	combo.RuntimeState = "position_uncertain"
	combo.PositionUncertain = true
	combo.MakerLeg = "a"
	combo.LegABasePosition = "1.4"
	combo.LegBBasePosition = "-2"
	combo.CarryBaseQuantity = "-0.6"
	combo.ErrorMessage = "arbitrage hedge would reverse position"
	return combo
}

func lastCloseResidual604Execution(combo ArbitrageCombination) ArbitrageExecution {
	execution := ArbitrageExecution{
		ID: "exec-604", CombinationID: combo.ID,
		Status: "reconciling", Direction: "bid",
		PositionEffect: "close", ReduceOnly: true, LastCloseClip: true,
		LegAFilledQuantity: "28", LegBFilledQuantity: "0",
		TriggerLegABid: "0.214", TriggerLegAAsk: "0.214",
		TriggerLegBBid: "0.214", TriggerLegBAsk: "0.214",
	}
	return execution
}

func lastCloseResidual604Orders(executionID string, residualQty string) []Order {
	return []Order{
		{
			ID: "maker-604", ArbitrageExecutionID: executionID,
			ArbitrageRole: "maker", ArbitrageLeg: "a", Side: "sell",
			Status: "filled", FilledQuantity: "28", AveragePrice: "0.214",
			ReduceOnly: true,
		},
		{
			ID: "residual-604", ArbitrageExecutionID: executionID,
			ArbitrageRole: "residual", ArbitrageLeg: "b", Side: "buy",
			Status: "filled", FilledQuantity: residualQty, AveragePrice: "0.214",
			ReduceOnly: true,
		},
	}
}

func lastCloseResidual604Store(
	combo ArbitrageCombination,
	execution ArbitrageExecution,
	orders []Order,
) *executorFailStore {
	copied := execution
	return &executorFailStore{
		combo: combo, execution: execution, activeExecution: &copied, orders: orders,
	}
}

func lastCloseResidualRecoveredCount(store *executorFailStore) int {
	count := 0
	for _, eventType := range store.eventTypes {
		if eventType == lastCloseResidualRecoveredEvent {
			count++
		}
	}
	return count
}

type countingRiskCatalog struct {
	silentRiskCatalog
	calls atomic.Int32
}

func (c *countingRiskCatalog) Get(ctx context.Context, id int64) (Instrument, error) {
	c.calls.Add(1)
	if c.silentRiskCatalog == nil {
		return Instrument{}, ErrInstrumentUnavailable
	}
	return c.silentRiskCatalog.Get(ctx, id)
}

type lastCloseRecoverRunner struct {
	inner     *ArbitrageExecutor
	recovered chan error
	executed  atomic.Int32
}

func (r *lastCloseRecoverRunner) Execute(
	ctx context.Context,
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	bboA, bboB marketdata.BBO,
	permit *executionWorkPermit,
) {
	r.executed.Add(1)
	r.inner.Execute(ctx, combination, execution, bboA, bboB, permit)
}

func (r *lastCloseRecoverRunner) RecoverWithoutMarketData(
	ctx context.Context,
	combination ArbitrageCombination,
	execution ArbitrageExecution,
) error {
	err := r.inner.RecoverWithoutMarketData(ctx, combination, execution)
	if r.recovered != nil {
		r.recovered <- err
	}
	return err
}

func (*lastCloseRecoverRunner) ReconcileCircuitOpen(
	context.Context, ArbitrageCombination,
) error {
	return nil
}

func (*lastCloseRecoverRunner) CloseCombination(context.Context, ArbitrageCombination) error {
	return nil
}

func waitLastCloseRecovered(t *testing.T, recovered <-chan error) {
	t.Helper()
	select {
	case err := <-recovered:
		if err != nil {
			t.Fatalf("recover err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for last-close recover")
	}
}

func TestAllocateLastCloseClipResidualFills(t *testing.T) {
	combo := lastCloseResidual604Combo()
	execution := lastCloseResidual604Execution(combo)

	t.Run("604_allocates_28_28", func(t *testing.T) {
		alloc := allocateLastCloseClipResidualFills(
			lastCloseResidual604Orders(execution.ID, "28"), combo, execution,
		)
		if !alloc.matched() || !alloc.FillA.Equal(decimal.NewFromInt(28)) ||
			!alloc.FillB.Equal(decimal.NewFromInt(28)) {
			t.Fatalf("alloc=%+v", alloc)
		}
	})
	t.Run("excess_residual_caps_to_gap", func(t *testing.T) {
		alloc := allocateLastCloseClipResidualFills(
			lastCloseResidual604Orders(execution.ID, "40"), combo, execution,
		)
		if !alloc.matched() || !alloc.AllocatedResidual.Equal(decimal.NewFromInt(28)) ||
			!alloc.ResidualFill.Equal(decimal.NewFromInt(40)) {
			t.Fatalf("alloc=%+v", alloc)
		}
	})
	t.Run("maker_leg_or_wrong_side_residual_ignored", func(t *testing.T) {
		orders := []Order{
			{
				ID: "maker", ArbitrageExecutionID: execution.ID,
				ArbitrageRole: "maker", ArbitrageLeg: "a", Side: "sell",
				Status: "filled", FilledQuantity: "28",
			},
			{
				ID: "maker-residual", ArbitrageExecutionID: execution.ID,
				ArbitrageRole: "residual", ArbitrageLeg: "a", Side: "sell",
				Status: "filled", FilledQuantity: "28",
			},
			{
				ID: "wrong-side", ArbitrageExecutionID: execution.ID,
				ArbitrageRole: "residual", ArbitrageLeg: "b", Side: "sell",
				Status: "filled", FilledQuantity: "28",
			},
			{
				ID: "other-exec", ArbitrageExecutionID: "other",
				ArbitrageRole: "residual", ArbitrageLeg: "b", Side: "buy",
				Status: "filled", FilledQuantity: "28",
			},
		}
		alloc := allocateLastCloseClipResidualFills(orders, combo, execution)
		if alloc.ResidualFill.IsPositive() || alloc.matched() || alloc.residualShort() {
			t.Fatalf("alloc=%+v", alloc)
		}
	})
	t.Run("ordinary_open_excludes_residual", func(t *testing.T) {
		openCombo := combo
		openCombo.Status = "running"
		openCombo.PositionUncertain = false
		openExec := execution
		openExec.LastCloseClip = false
		openExec.PositionEffect = "open"
		openExec.ReduceOnly = false
		if lastCloseClipClosingEligible(openCombo, openExec) {
			t.Fatal("ordinary open must not be last-close-closing")
		}
	})
}

func TestAggregateSimultaneousMarketLastCloseFills(t *testing.T) {
	executionID := "exec-market"
	orders := []Order{
		{
			ArbitrageExecutionID: executionID, ArbitrageRole: "market", ArbitrageLeg: "a",
			Status: "filled", FilledQuantity: "20",
		},
		{
			ArbitrageExecutionID: executionID, ArbitrageRole: "market", ArbitrageLeg: "a",
			Status: "canceled", FilledQuantity: "17",
		},
		{
			ArbitrageExecutionID: executionID, ArbitrageRole: "market", ArbitrageLeg: "b",
			Status: "filled", FilledQuantity: "37",
		},
		{
			ArbitrageExecutionID: "other", ArbitrageRole: "market", ArbitrageLeg: "a",
			Status: "filled", FilledQuantity: "100",
		},
		{
			ArbitrageExecutionID: executionID, ArbitrageRole: "maker", ArbitrageLeg: "a",
			Status: "filled", FilledQuantity: "9",
		},
	}
	fillA, fillB, found := aggregateSimultaneousMarketLastCloseFills(executionID, orders)
	if !found || !fillA.Equal(decimal.NewFromInt(37)) || !fillB.Equal(decimal.NewFromInt(37)) {
		t.Fatalf("a=%s b=%s found=%v", fillA, fillB, found)
	}
	againA, againB, againFound := aggregateSimultaneousMarketLastCloseFills(executionID, orders)
	if !againFound || !againA.Equal(fillA) || !againB.Equal(fillB) {
		t.Fatalf("repeat a=%s b=%s", againA, againB)
	}
	_, _, missing := aggregateSimultaneousMarketLastCloseFills("missing", orders)
	if missing {
		t.Fatal("other execution must not count")
	}
}

func TestApplyLastCloseClipKeepsSimultaneousMarketFills(t *testing.T) {
	combo := lastCloseResidual604Combo()
	combo.ExecutionMode = "simultaneous_market"
	execution := lastCloseResidual604Execution(combo)
	execution.LegAFilledQuantity = "37"
	execution.LegBFilledQuantity = "37"
	orders := []Order{
		{
			ArbitrageExecutionID: execution.ID, ArbitrageRole: "market", ArbitrageLeg: "a",
			Status: "filled", FilledQuantity: "37",
		},
		{
			ArbitrageExecutionID: execution.ID, ArbitrageRole: "market", ArbitrageLeg: "b",
			Status: "expired", FilledQuantity: "37",
		},
		{
			ArbitrageExecutionID: "other", ArbitrageRole: "market", ArbitrageLeg: "a",
			Status: "filled", FilledQuantity: "5",
		},
	}
	fillA, fillB := applyLastCloseClipAllocatedFills(combo, &execution, orders)
	if !fillA.Equal(decimal.NewFromInt(37)) || !fillB.Equal(decimal.NewFromInt(37)) ||
		execution.LegAFilledQuantity != "37" || execution.LegBFilledQuantity != "37" {
		t.Fatalf("fills=%s/%s execution=%s/%s", fillA, fillB, execution.LegAFilledQuantity, execution.LegBFilledQuantity)
	}
	applyLastCloseClipAllocatedFills(combo, &execution, orders)
	if execution.LegAFilledQuantity != "37" || execution.LegBFilledQuantity != "37" {
		t.Fatalf("repeat execution=%s/%s", execution.LegAFilledQuantity, execution.LegBFilledQuantity)
	}

	makerCombo := combo
	makerCombo.ExecutionMode = "maker_then_hedge"
	makerExec := lastCloseResidual604Execution(makerCombo)
	makerOrders := lastCloseResidual604Orders(makerExec.ID, "28")
	makerA, makerB := applyLastCloseClipAllocatedFills(makerCombo, &makerExec, makerOrders)
	if !makerA.Equal(decimal.NewFromInt(28)) || !makerB.Equal(decimal.NewFromInt(28)) {
		t.Fatalf("maker fills=%s/%s", makerA, makerB)
	}
}

func TestLastCloseRecoveryMark(t *testing.T) {
	execution := lastCloseResidual604Execution(lastCloseResidual604Combo())
	orders := lastCloseResidual604Orders(execution.ID, "28")
	mark := lastCloseRecoveryMark(orders, execution)
	if !mark.Equal(parseDecimal("0.214")) {
		t.Fatalf("mark=%s", mark)
	}
	blank := execution
	blank.TriggerLegABid, blank.TriggerLegAAsk = "", ""
	blank.TriggerLegBBid, blank.TriggerLegBAsk = "", ""
	emptyOrders := lastCloseResidual604Orders(execution.ID, "28")
	for i := range emptyOrders {
		emptyOrders[i].AveragePrice = ""
	}
	if lastCloseRecoveryMark(emptyOrders, blank).IsPositive() {
		t.Fatal("expected no mark")
	}
}

func TestRecoverLastCloseClipClosing604ToCompleted(t *testing.T) {
	combo := lastCloseResidual604Combo()
	execution := lastCloseResidual604Execution(combo)
	store := lastCloseResidual604Store(combo, execution, lastCloseResidual604Orders(execution.ID, "28"))
	catalog := &countingRiskCatalog{}
	adapter := &stubAdapter{}
	executor := NewArbitrageExecutor(
		store, nil, nil, catalog, nil, exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": adapter, "okx": adapter,
		}), nil, "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	if err := executor.RecoverWithoutMarketData(context.Background(), combo, execution); err != nil {
		t.Fatal(err)
	}
	if store.execution.Status != "completed" {
		t.Fatalf("execution=%+v", store.execution)
	}
	if !parseDecimal(store.execution.LegAFilledQuantity).Equal(decimal.NewFromInt(28)) ||
		!parseDecimal(store.execution.LegBFilledQuantity).Equal(decimal.NewFromInt(28)) {
		t.Fatalf("fills=%+v", store.execution)
	}
	if !store.combo.PositionUncertain || store.combo.LegABasePosition != "1.4" ||
		store.combo.LegBBasePosition != "-2" {
		t.Fatalf("combo ledger changed: %+v", store.combo)
	}
	if _, err := store.GetActiveArbitrageExecution(context.Background(), combo.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("active after complete err=%v", err)
	}
	if lastCloseResidualRecoveredCount(store) != 1 {
		t.Fatalf("events=%v", store.eventTypes)
	}
	if adapter.calls != 0 || catalog.calls.Load() != 0 {
		t.Fatalf("place=%d catalog=%d", adapter.calls, catalog.calls.Load())
	}
	for _, eventType := range store.eventTypes {
		if eventType == lastClipDustHandoffEvent || eventType == "maker_accepted" {
			t.Fatalf("events=%v", store.eventTypes)
		}
	}

	if err := executor.RecoverWithoutMarketData(context.Background(), combo, lastCloseResidual604Execution(combo)); err != nil {
		t.Fatal(err)
	}
	executor.Execute(
		context.Background(), combo, lastCloseResidual604Execution(combo),
		lastClipCloseBBO(), lastClipCloseBBO(), nil,
	)
	if lastCloseResidualRecoveredCount(store) != 1 {
		t.Fatalf("duplicate events=%v", store.eventTypes)
	}
	if adapter.calls != 0 {
		t.Fatalf("restart placed orders: %+v", adapter.requests)
	}
}

func TestRecoverLastCloseClipClosingResidualShortFailsSafe(t *testing.T) {
	combo := lastCloseResidual604Combo()
	execution := lastCloseResidual604Execution(combo)
	store := lastCloseResidual604Store(combo, execution, lastCloseResidual604Orders(execution.ID, "10"))
	adapter := &stubAdapter{}
	executor := NewArbitrageExecutor(
		store, nil, nil, nil, nil, exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": adapter, "okx": adapter,
		}), nil, "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	if err := executor.RecoverWithoutMarketData(context.Background(), combo, execution); err != nil {
		t.Fatal(err)
	}
	if store.execution.Status != "reconciling" ||
		!store.combo.PositionUncertain ||
		store.combo.RuntimeState != "position_uncertain" ||
		store.combo.ErrorMessage != ErrArbitrageUnsafeHedge.Error() ||
		store.combo.CircuitOpen {
		t.Fatalf("execution=%+v combo=%+v", store.execution, store.combo)
	}
	if adapter.calls != 0 {
		t.Fatalf("placed=%+v", adapter.requests)
	}
	found := false
	for _, eventType := range store.eventTypes {
		if eventType == lastCloseRecoveryFillMismatchEvent {
			found = true
		}
		if eventType == lastCloseResidualRecoveredEvent || eventType == "maker_accepted" ||
			eventType == "last_close_clip_unbalanced" {
			t.Fatalf("events=%v", store.eventTypes)
		}
	}
	if !found {
		t.Fatalf("events=%v", store.eventTypes)
	}
	if err := executor.RecoverWithoutMarketData(context.Background(), combo, execution); err != nil {
		t.Fatal(err)
	}
	if countEvents(store.eventTypes, lastCloseRecoveryFillMismatchEvent) != 1 || adapter.calls != 0 {
		t.Fatalf("repeat events=%v place=%d", store.eventTypes, adapter.calls)
	}
}

func TestExecuteLastCloseClipClosingDoesNotWarmOrPlaceWhenMatched(t *testing.T) {
	combo := lastCloseResidual604Combo()
	execution := lastCloseResidual604Execution(combo)
	store := lastCloseResidual604Store(combo, execution, lastCloseResidual604Orders(execution.ID, "28"))
	catalog := &countingRiskCatalog{silentRiskCatalog: silentRiskCatalog{
		101: lastClipCloseInstrument(101, "1", "5"),
		202: lastClipCloseInstrument(202, "1", "5"),
	}}
	adapter := &stubAdapter{place: exchange.Result{
		VenueOrderID: "should-not-place", Status: "filled",
		FilledQuantity: "28", AveragePrice: "0.214",
	}}
	executor := NewArbitrageExecutor(
		store, nil, nil, catalog, silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": adapter, "okx": adapter,
		}),
		nil, "", time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	executor.Execute(context.Background(), combo, execution, lastClipCloseBBO(), lastClipCloseBBO(), nil)
	if store.execution.Status != "completed" {
		t.Fatalf("execution=%+v", store.execution)
	}
	if catalog.calls.Load() != 0 || adapter.calls != 0 {
		t.Fatalf("catalog=%d place=%d requests=%+v",
			catalog.calls.Load(), adapter.calls, adapter.requests)
	}
}

func TestRecoverLastCloseClipClosingKeepsReconcilingWhenBlockedOrNoMark(t *testing.T) {
	combo := lastCloseResidual604Combo()
	execution := lastCloseResidual604Execution(combo)
	t.Run("live_order", func(t *testing.T) {
		orders := lastCloseResidual604Orders(execution.ID, "28")
		orders[1].Status = "open"
		store := lastCloseResidual604Store(combo, execution, orders)
		adapter := &stubAdapter{}
		executor := NewArbitrageExecutor(
			store, nil, nil, nil, nil, exchange.NewTestRegistry(map[string]exchange.Adapter{
				"bybit": adapter, "okx": adapter,
			}), nil, "",
			time.Millisecond, 0, 0, 1, time.Second, nil,
		)
		if err := executor.RecoverWithoutMarketData(context.Background(), combo, execution); err != nil {
			t.Fatal(err)
		}
		if store.execution.Status != "reconciling" || adapter.calls != 0 {
			t.Fatalf("execution=%+v place=%d", store.execution, adapter.calls)
		}
	})
	t.Run("reconcile_failures", func(t *testing.T) {
		orders := lastCloseResidual604Orders(execution.ID, "28")
		orders[1].ReconcileFailures = 1
		store := lastCloseResidual604Store(combo, execution, orders)
		adapter := &stubAdapter{}
		executor := NewArbitrageExecutor(
			store, nil, nil, nil, nil, exchange.NewTestRegistry(map[string]exchange.Adapter{
				"bybit": adapter, "okx": adapter,
			}), nil, "",
			time.Millisecond, 0, 0, 1, time.Second, nil,
		)
		if err := executor.RecoverWithoutMarketData(context.Background(), combo, execution); err != nil {
			t.Fatal(err)
		}
		if store.execution.Status != "reconciling" || adapter.calls != 0 {
			t.Fatalf("execution=%+v place=%d", store.execution, adapter.calls)
		}
	})
	t.Run("missing_mark", func(t *testing.T) {
		orders := lastCloseResidual604Orders(execution.ID, "28")
		for i := range orders {
			orders[i].AveragePrice = ""
		}
		blank := execution
		blank.TriggerLegABid, blank.TriggerLegAAsk = "", ""
		blank.TriggerLegBBid, blank.TriggerLegBAsk = "", ""
		store := lastCloseResidual604Store(combo, blank, orders)
		adapter := &stubAdapter{}
		executor := NewArbitrageExecutor(
			store, nil, nil, nil, nil, exchange.NewTestRegistry(map[string]exchange.Adapter{
				"bybit": adapter, "okx": adapter,
			}), nil, "",
			time.Millisecond, 0, 0, 1, time.Second, nil,
		)
		if err := executor.RecoverWithoutMarketData(context.Background(), combo, blank); err != nil {
			t.Fatal(err)
		}
		if store.execution.Status != "reconciling" || adapter.calls != 0 {
			t.Fatalf("execution=%+v place=%d", store.execution, adapter.calls)
		}
		if lastCloseResidualRecoveredCount(store) != 0 {
			t.Fatalf("events=%v", store.eventTypes)
		}
	})
}

func TestRecoverLastCloseClipClosingDoesNotHandoffWhenMatched(t *testing.T) {
	combo := lastCloseResidual604Combo()
	execution := lastCloseResidual604Execution(combo)
	orders := lastCloseResidual604Orders(execution.ID, "28")
	orders = append(orders, Order{
		ID: "hedge-rejected", ArbitrageExecutionID: execution.ID,
		ArbitrageRole: "hedge", ArbitrageLeg: "b", Side: "buy",
		Status: "rejected", FilledQuantity: "0", ReduceOnly: true,
		ErrorMessage: "Order must have minimum value of $10",
	})
	store := lastCloseResidual604Store(combo, execution, orders)
	executor := NewArbitrageExecutor(
		store, nil, nil, nil, nil, nil, nil, "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	if err := executor.RecoverWithoutMarketData(context.Background(), combo, execution); err != nil {
		t.Fatal(err)
	}
	if store.execution.Status != "completed" ||
		store.combo.LastFailureKey == lastClipHedgeMinNotionalFailureKey {
		t.Fatalf("execution=%+v combo=%+v", store.execution, store.combo)
	}
}

func TestLastCloseClipClosingCarryDustDoesNotUnsafeHedge(t *testing.T) {
	combo := lastCloseResidual604Combo()
	execution := lastCloseResidual604Execution(combo)
	store := lastCloseResidual604Store(combo, execution, lastCloseResidual604Orders(execution.ID, "28"))
	adapter := &stubAdapter{}
	executor := NewArbitrageExecutor(
		store, nil, nil, nil, nil, exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": adapter, "okx": adapter,
		}), nil, "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	executor.Execute(context.Background(), combo, execution, lastClipCloseBBO(), lastClipCloseBBO(), nil)
	if store.execution.Status != "completed" || adapter.calls != 0 {
		t.Fatalf("execution=%+v place=%d", store.execution, adapter.calls)
	}
	if !parseDecimal(store.combo.CarryBaseQuantity).Equal(parseDecimal("-0.6")) {
		t.Fatalf("carry=%s", store.combo.CarryBaseQuantity)
	}
}

func TestResumeLockedSafeLastCloseClipClosingOnlyRecovers(t *testing.T) {
	combo := lastCloseResidual604Combo()
	execution := lastCloseResidual604Execution(combo)
	freshBBO := lastClipCloseBBO()
	freshBBO.ReceiveTimestamp = time.Now().UTC()

	t.Run("fresh_and_stale_use_recover_not_execute", func(t *testing.T) {
		for _, bothFresh := range []bool{true, false} {
			store := lastCloseResidual604Store(combo, execution, lastCloseResidual604Orders(execution.ID, "28"))
			runner := &schedulerRunner{
				executed:  make(chan ArbitrageExecution, 1),
				recovered: make(chan ArbitrageExecution, 1),
			}
			scheduler := newTestArbitrageScheduler(
				store, nil, runner,
				time.Second, time.Millisecond, time.Second, 1, 1, 1, 2, false, nil,
			)
			runtime := &arbitrageRuntime{combination: combo}
			scheduler.resumeLockedSafe(context.Background(), runtime, freshBBO, freshBBO, bothFresh)
			select {
			case <-runner.recovered:
			case <-time.After(2 * time.Second):
				t.Fatalf("bothFresh=%v did not recover", bothFresh)
			}
			select {
			case got := <-runner.executed:
				t.Fatalf("bothFresh=%v executed %+v", bothFresh, got)
			default:
			}
		}
	})
	t.Run("ordinary_closing_flattening_still_skips_recover", func(t *testing.T) {
		ordinary := lastClipCloseCombination("bid")
		ordinaryExec := ArbitrageExecution{
			ID: "exec-ordinary-close", CombinationID: ordinary.ID,
			Status: "reconciling", Direction: "bid", PositionEffect: "close",
		}
		store := &executorFailStore{
			combo: ordinary, execution: ordinaryExec, activeExecution: &ordinaryExec,
		}
		runner := &schedulerRunner{
			executed:  make(chan ArbitrageExecution, 1),
			recovered: make(chan ArbitrageExecution, 1),
		}
		scheduler := newTestArbitrageScheduler(
			store, nil, runner,
			time.Second, time.Millisecond, time.Second, 1, 1, 1, 2, false, nil,
		)
		runtime := &arbitrageRuntime{combination: ordinary}
		scheduler.resumeLockedSafe(context.Background(), runtime, freshBBO, freshBBO, false)
		select {
		case got := <-runner.recovered:
			t.Fatalf("ordinary closing recovered %+v", got)
		case <-time.After(50 * time.Millisecond):
		}
		select {
		case got := <-runner.executed:
			t.Fatalf("ordinary closing executed %+v", got)
		default:
		}
	})
	t.Run("fresh_and_stale_real_executor_places_nothing", func(t *testing.T) {
		for _, bothFresh := range []bool{true, false} {
			store := lastCloseResidual604Store(combo, execution, lastCloseResidual604Orders(execution.ID, "28"))
			adapter := &stubAdapter{}
			executor := NewArbitrageExecutor(
				store, nil, nil, nil, nil, exchange.NewTestRegistry(map[string]exchange.Adapter{
					"bybit": adapter, "okx": adapter,
				}), nil, "",
				time.Millisecond, 0, 0, 1, time.Second, nil,
			)
			runner := &lastCloseRecoverRunner{
				inner: executor, recovered: make(chan error, 1),
			}
			scheduler := newTestArbitrageScheduler(
				store, nil, runner,
				time.Second, time.Millisecond, time.Second, 1, 1, 1, 2, false, nil,
			)
			runtime := &arbitrageRuntime{combination: combo}
			scheduler.resumeLockedSafe(context.Background(), runtime, freshBBO, freshBBO, bothFresh)
			waitLastCloseRecovered(t, runner.recovered)
			if runner.executed.Load() != 0 || adapter.calls != 0 {
				t.Fatalf("bothFresh=%v executed=%d place=%d",
					bothFresh, runner.executed.Load(), adapter.calls)
			}
			if store.execution.Status != "completed" {
				t.Fatalf("bothFresh=%v execution=%+v", bothFresh, store.execution)
			}
			scheduler.resumeLockedSafe(context.Background(), runtime, freshBBO, freshBBO, bothFresh)
			select {
			case err := <-runner.recovered:
				t.Fatalf("second resume recovered err=%v", err)
			case <-time.After(50 * time.Millisecond):
			}
			if lastCloseResidualRecoveredCount(store) != 1 || adapter.calls != 0 {
				t.Fatalf("events=%v place=%d", store.eventTypes, adapter.calls)
			}
		}
	})
}

func TestLastCloseClipClosingAuditorUnlocksMatchingDust(t *testing.T) {
	combo := ArbitrageCombination{
		ID: "combo-last-close-dust", OwnerUsername: "admin", Version: 4,
		Status: "closing", RuntimeState: "position_uncertain",
		PositionUncertain: true,
		ErrorMessage:      arbitragePositionAuditErrorPrefix + " prior drift",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 10, Exchange: "gate",
			ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT", BaseAsset: "BEAT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 20, Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: "BEAT-USDT-SWAP", BaseAsset: "BEAT",
		},
		LegABasePosition: "1.4", LegBBasePosition: "-2",
	}
	store := &positionAuditStore{item: combo}
	auditor := NewArbitragePositionAuditor(
		store,
		positionAuditCatalog{items: map[int64]Instrument{
			10: {
				ID: 10, Exchange: "gate", ContractType: "perpetual",
				ExchangeSymbol: "BEAT_USDT", ContractSize: "1", QuantityStep: "0.1",
			},
			20: {
				ID: 20, Exchange: "okx", ContractType: "perpetual",
				ExchangeSymbol: "BEAT-USDT-SWAP", ContractSize: "1", QuantityStep: "0.1",
			},
		}},
		positionAuditCredentials{},
		positionAuditPortfolios{snapshots: map[string]portfolio.Snapshot{
			"gate": {Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "Gate", WireSymbol: "BEAT_USDT",
				SignedContractSize: "1.4",
			}}},
			"okx": {Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "OKX", WireSymbol: "BEAT-USDT-SWAP",
				SignedContractSize: "-2",
			}}},
		}},
		"token", time.Minute, time.Second, 20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if store.item.PositionUncertain || store.item.RuntimeState != "closing" {
		t.Fatalf("matching dust should unlock: %+v", store.item)
	}
}

func TestLastCloseResidualAuditUnlockEligible(t *testing.T) {
	if !lastCloseResidualAuditUnlockEligible(
		"closing", "position_uncertain", ErrArbitrageUnsafeHedge.Error(),
		true, false, true, true,
	) {
		t.Fatal("604 recovered exact-zero should unlock")
	}
	cases := []struct {
		name string
	}{
		{name: "running"},
		{name: "circuit"},
		{name: "nonzero"},
		{name: "no_event"},
		{name: "wrong_error"},
	}
	for _, test := range cases {
		status, runtime, errMsg := "closing", "position_uncertain", ErrArbitrageUnsafeHedge.Error()
		uncertain, circuit, recovered, zero := true, false, true, true
		switch test.name {
		case "running":
			status = "running"
		case "circuit":
			circuit = true
		case "nonzero":
			zero = false
		case "no_event":
			recovered = false
		case "wrong_error":
			errMsg = arbitragePositionAuditErrorPrefix + " drift"
		}
		if lastCloseResidualAuditUnlockEligible(
			status, runtime, errMsg, uncertain, circuit, recovered, zero,
		) {
			t.Fatalf("%s should stay locked", test.name)
		}
	}
}

func lastCloseResidualAuditDustCombo() ArbitrageCombination {
	return ArbitrageCombination{
		ID: "combo-last-close-dust", OwnerUsername: "admin", Version: 4,
		Status: "closing", RuntimeState: "position_uncertain",
		PositionUncertain: true,
		ErrorMessage:      ErrArbitrageUnsafeHedge.Error(),
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 10, Exchange: "gate",
			ContractType: "perpetual", ExchangeSymbol: "BEAT_USDT", BaseAsset: "BEAT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 20, Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: "BEAT-USDT-SWAP", BaseAsset: "BEAT",
		},
		LegABasePosition: "1.4", LegBBasePosition: "-2",
	}
}

func lastCloseResidualAuditDustAuditor(store *positionAuditStore) *ArbitragePositionAuditor {
	return NewArbitragePositionAuditor(
		store,
		positionAuditCatalog{items: map[int64]Instrument{
			10: {
				ID: 10, Exchange: "gate", ContractType: "perpetual",
				ExchangeSymbol: "BEAT_USDT", ContractSize: "1", QuantityStep: "0.1",
			},
			20: {
				ID: 20, Exchange: "okx", ContractType: "perpetual",
				ExchangeSymbol: "BEAT-USDT-SWAP", ContractSize: "1", QuantityStep: "0.1",
			},
		}},
		positionAuditCredentials{},
		positionAuditPortfolios{snapshots: map[string]portfolio.Snapshot{
			"gate": {Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "Gate", WireSymbol: "BEAT_USDT",
				SignedContractSize: "1.4",
			}}},
			"okx": {Positions: []portfolio.Position{{
				Kind: "cex", Exchange: "OKX", WireSymbol: "BEAT-USDT-SWAP",
				SignedContractSize: "-2",
			}}},
		}},
		"token", time.Minute, time.Second, 20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

func TestLastCloseResidualAuditorUnlocksUnsafeHedgeAfterRecovered(t *testing.T) {
	store := &positionAuditStore{
		item:                             lastCloseResidualAuditDustCombo(),
		lastCloseResidualRecoveredLatest: true,
	}
	auditor := lastCloseResidualAuditDustAuditor(store)
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if store.item.PositionUncertain || store.item.RuntimeState != "closing" ||
		store.item.ErrorMessage != "" {
		t.Fatalf("604 recovered should unlock: %+v", store.item)
	}
	if len(store.eventTypes) != 1 || store.eventTypes[0] != "position_uncertain_recovered" {
		t.Fatalf("events=%v", store.eventTypes)
	}
	if store.eventPayloads[0]["reason"] != lastCloseResidualAuditedReason ||
		store.eventPayloads[0]["previousError"] != ErrArbitrageUnsafeHedge.Error() {
		t.Fatalf("payload=%v", store.eventPayloads[0])
	}
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if store.item.PositionUncertain || store.item.RuntimeState != "closing" {
		t.Fatalf("second audit mutated lock: %+v", store.item)
	}
	if len(store.eventTypes) != 1 {
		t.Fatalf("duplicate recovered events=%v", store.eventTypes)
	}
}

func TestLastCloseResidualAuditorKeepsUnsafeHedgeWithoutRecovered(t *testing.T) {
	store := &positionAuditStore{item: lastCloseResidualAuditDustCombo()}
	auditor := lastCloseResidualAuditDustAuditor(store)
	if err := auditor.audit(context.Background(), store.item, nil); err != nil {
		t.Fatal(err)
	}
	if !store.item.PositionUncertain || store.item.ErrorMessage != ErrArbitrageUnsafeHedge.Error() {
		t.Fatalf("missing recovered event must stay locked: %+v", store.item)
	}
	for _, eventType := range store.eventTypes {
		if eventType == "position_uncertain_recovered" {
			t.Fatalf("events=%v", store.eventTypes)
		}
	}
}

type recoveredRouteStore struct {
	dryRunStore
	lookups int
}

func (s *recoveredRouteStore) LastCloseResidualRecovered(context.Context, string) (bool, error) {
	return true, nil
}

func (s *recoveredRouteStore) GetLastCloseResidualFlattenExecution(
	context.Context, string,
) (ArbitrageExecution, bool, error) {
	s.lookups++
	return ArbitrageExecution{}, false, nil
}

func (s *recoveredRouteStore) ClaimLastCloseResidualFlattenExecution(
	context.Context, LastCloseResidualFlattenClaim,
) (ArbitrageExecution, bool, error) {
	s.lookups++
	return ArbitrageExecution{}, false, nil
}

func (s *recoveredRouteStore) CloseLastCloseResidualWithDust(
	context.Context, LastCloseResidualDustClose,
) (ArbitrageCombination, bool, error) {
	return s.item, false, nil
}

func (s *recoveredRouteStore) ListArbitrageExecutions(
	context.Context, string, int,
) ([]ArbitrageExecution, error) {
	return nil, nil
}

func TestRouteLastCloseResidualFlattenLetsFlatCombinationClose(t *testing.T) {
	item := residualFlattenCombo("0", "0")
	item.CarryBaseQuantity = "0"
	item.PositionUncertain = false
	item.RuntimeState = "closing"
	store := &recoveredRouteStore{dryRunStore: dryRunStore{claim: true, item: item}}
	runner := &schedulerRunner{closed: make(chan ArbitrageCombination, 1)}
	runner.closeHook = func(ArbitrageCombination) {
		store.item.Status = "closed"
	}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("1")
	if !scheduler.flattenOrFinalizeClosingFrom(
		context.Background(), runtime, bbo, bbo, px, px, px, px, "control",
	) {
		t.Fatal("flat recovered combination should use the existing close")
	}
	if store.lookups != 0 {
		t.Fatalf("flat combination looked up residual flatten %d times", store.lookups)
	}
	select {
	case closed := <-runner.closed:
		if closed.LegABasePosition != "0" || closed.LegBBasePosition != "0" ||
			closed.CarryBaseQuantity != "0" {
			t.Fatalf("closed=%+v", closed)
		}
	case <-time.After(time.Second):
		t.Fatal("flat recovered combination did not call CloseCombination")
	}

	open := residualFlattenCombo("170", "-200")
	open.CarryBaseQuantity = "-30"
	open.PositionUncertain = false
	open.RuntimeState = "closing"
	openStore := &recoveredRouteStore{dryRunStore: dryRunStore{claim: true, item: open}}
	openRunner := &schedulerRunner{closed: make(chan ArbitrageCombination, 1)}
	openScheduler := newTestArbitrageScheduler(
		openStore, nil, openRunner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	openRuntime := &arbitrageRuntime{combination: open}
	if openScheduler.flattenOrFinalizeClosingFrom(
		context.Background(), openRuntime, bbo, bbo, px, px, px, px, "control",
	) {
		t.Fatal("open recovered ledger must not take the flat close")
	}
	if openStore.lookups == 0 {
		t.Fatal("open recovered ledger should stay on the residual route")
	}
	select {
	case <-openRunner.closed:
		t.Fatal("open recovered ledger called CloseCombination")
	default:
	}

	tiny := ownedFlattenCombo("1.4", "-2")
	tiny.PositionUncertain = false
	tiny.RuntimeState = "closing"
	tinyStore := &recoveredRouteStore{dryRunStore: dryRunStore{claim: true, item: tiny}}
	started := make(chan struct{}, 1)
	tinyRunner := &residualRouteRunner{
		schedulerRunner: schedulerRunner{closed: make(chan ArbitrageCombination, 1)},
		started:         started,
	}
	tinyScheduler := newTestArbitrageScheduler(
		tinyStore, nil, tinyRunner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	tinyRuntime := &arbitrageRuntime{combination: tiny}
	tinyBBO := flattenCloseBBO()
	tinyPx := decimal.RequireFromString("0.214")
	if tinyScheduler.flattenOrFinalizeClosingFrom(
		context.Background(), tinyRuntime, tinyBBO, tinyBBO, tinyPx, tinyPx, tinyPx, tinyPx, "control",
	) {
		t.Fatal("tracked tiny must not take the flat close")
	}
	select {
	case <-tinyRunner.closed:
		t.Fatal("tracked tiny called CloseCombination")
	default:
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("tracked tiny did not start residual flatten")
	}
	if tinyRunner.runs != 1 || tinyStore.lookups == 0 {
		t.Fatalf("runs=%d lookups=%d", tinyRunner.runs, tinyStore.lookups)
	}
}

type residualRouteRunner struct {
	schedulerRunner
	started chan struct{}
	runs    int
}

func (r *residualRouteRunner) RunLastCloseResidualFlatten(
	context.Context, ArbitrageCombination, marketdata.BBO, marketdata.BBO,
) error {
	r.runs++
	select {
	case r.started <- struct{}{}:
	default:
	}
	return nil
}

func TestArbitrageSchedulerUnlockedLastCloseResidualCallsCloseCombination(t *testing.T) {
	item := ownedFlattenCombo("1.4", "-2")
	item.PositionUncertain = false
	item.RuntimeState = "closing"
	item.ErrorMessage = ""
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{closed: make(chan ArbitrageCombination, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosingFrom(
		context.Background(), runtime, bbo, bbo, px, px, px, px, "control",
	) {
		t.Fatal("non-zero leftover must stay on the closing runtime")
	}
	select {
	case <-runner.closed:
	case <-time.After(time.Second):
		t.Fatal("unlocked last-close leftover did not call CloseCombination")
	}
}

func TestArbitrageSchedulerLockedLastCloseResidualSkipsCloseCombination(t *testing.T) {
	item := ownedFlattenCombo("1.4", "-2")
	item.PositionUncertain = true
	item.RuntimeState = "position_uncertain"
	item.ErrorMessage = ErrArbitrageUnsafeHedge.Error()
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{closed: make(chan ArbitrageCombination, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := flattenCloseBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosingFrom(
		context.Background(), runtime, bbo, bbo, px, px, px, px, "control",
	) {
		t.Fatal("locked last-close leftover must not flatten")
	}
	select {
	case <-runner.closed:
		t.Fatal("locked last-close leftover called CloseCombination")
	default:
	}
}

func TestArbitrageExecutorCloseDoesNotPlaceNakedTinyOrders(t *testing.T) {
	combo := ownedTinyCloseCombo("1.4", "-2")
	store := &closeOrderStore{executorFailStore: &executorFailStore{combo: combo}}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{}
	catalog := flattenTestCatalog()
	for id, item := range catalog {
		item.MarketQuantityStep = "0.1"
		item.QuantityStep = "0.1"
		catalog[id] = item
	}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, catalog, "1", "1")
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if len(adapterA.snapshot()) != 0 || len(adapterB.snapshot()) != 0 {
		t.Fatalf("naked orders a=%+v b=%+v", adapterA.snapshot(), adapterB.snapshot())
	}
	if store.combo.Status != "closing" {
		t.Fatalf("status=%s", store.combo.Status)
	}
}

func TestLastCloseClipLockedResumeEligibleOnlyThisError(t *testing.T) {
	combo := lastCloseResidual604Combo()
	execution := lastCloseResidual604Execution(combo)
	if !lastCloseClipLockedResumeEligible(combo, execution) {
		t.Fatal("reconciling unsafe hedge should be eligible")
	}
	hedging := execution
	hedging.Status = "hedging"
	if !lastCloseClipLockedResumeEligible(combo, hedging) {
		t.Fatal("hedging unsafe hedge should be eligible")
	}
	cases := []struct {
		name   string
		adjust func(*ArbitrageCombination, *ArbitrageExecution)
	}{
		{name: "wrong error", adjust: func(combo *ArbitrageCombination, _ *ArbitrageExecution) {
			combo.ErrorMessage = "some other uncertain hedge"
		}},
		{name: "circuit open", adjust: func(combo *ArbitrageCombination, _ *ArbitrageExecution) {
			combo.CircuitOpen = true
		}},
		{name: "runtime monitoring", adjust: func(combo *ArbitrageCombination, _ *ArbitrageExecution) {
			combo.RuntimeState = "monitoring"
		}},
		{name: "not uncertain", adjust: func(combo *ArbitrageCombination, _ *ArbitrageExecution) {
			combo.PositionUncertain = false
		}},
		{name: "ordinary hedging", adjust: func(_ *ArbitrageCombination, execution *ArbitrageExecution) {
			execution.LastCloseClip = false
			execution.ReduceOnly = false
			execution.PositionEffect = "open"
			execution.Status = "hedging"
		}},
		{name: "completed", adjust: func(_ *ArbitrageCombination, execution *ArbitrageExecution) {
			execution.Status = "completed"
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			item := combo
			exec := execution
			test.adjust(&item, &exec)
			if lastCloseClipLockedResumeEligible(item, exec) {
				t.Fatal("expected ineligible")
			}
		})
	}
}

type residualFlattenStore struct {
	*executorFailStore
	recovered     bool
	conflict      bool
	claimCalls    int
	createdOrders int
}

func (s *residualFlattenStore) LastCloseResidualRecovered(context.Context, string) (bool, error) {
	return s.recovered, nil
}

func (s *residualFlattenStore) GetLastCloseResidualFlattenExecution(
	context.Context, string,
) (ArbitrageExecution, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.execution.ID == lastCloseResidualFlattenExecutionID(s.combo.ID) {
		return s.execution, true, nil
	}
	return ArbitrageExecution{}, false, nil
}

func (s *residualFlattenStore) ClaimLastCloseResidualFlattenExecution(
	_ context.Context, claim LastCloseResidualFlattenClaim,
) (ArbitrageExecution, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimCalls++
	id := lastCloseResidualFlattenExecutionID(s.combo.ID)
	if s.conflict {
		return ArbitrageExecution{}, false, nil
	}
	if s.execution.ID == id {
		return s.execution, false, nil
	}
	execution := ArbitrageExecution{
		ID: id, CombinationID: s.combo.ID, Status: "hedging",
		ReduceOnly: true, PositionEffect: "close",
	}
	s.execution = execution
	copied := execution
	s.activeExecution = &copied
	s.orders = append(s.orders, claim.Orders...)
	s.createdOrders += len(claim.Orders)
	s.combo.Version++
	return execution, true, nil
}

func (s *residualFlattenStore) CloseLastCloseResidualWithDust(
	_ context.Context, close LastCloseResidualDustClose,
) (ArbitrageCombination, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.combo.Status != "closing" || s.combo.PositionUncertain || s.combo.CircuitOpen {
		return s.combo, false, nil
	}
	if !close.LastOrderUpdatedAt.IsZero() &&
		s.combo.LastPositionReconciledAt.Before(close.LastOrderUpdatedAt) {
		return s.combo, false, nil
	}
	for _, order := range s.orders {
		if !terminalStatus(order.Status) {
			return s.combo, false, nil
		}
	}
	if s.activeExecution != nil && !terminalArbitrageExecutionStatus(s.activeExecution.Status) {
		return s.combo, false, nil
	}
	if !close.LegANotional.LessThan(decimal.NewFromInt(20)) ||
		!close.LegBNotional.LessThan(decimal.NewFromInt(20)) ||
		close.LegAReducible || close.LegBReducible {
		return s.combo, false, nil
	}
	for _, eventType := range s.eventTypes {
		if eventType == lastCloseResidualDustEvent {
			return s.combo, true, nil
		}
	}
	legA := s.combo.LegABasePosition
	legB := s.combo.LegBBasePosition
	s.combo.Status = "closed"
	s.combo.RuntimeState = "monitoring"
	s.combo.LegABasePosition = legA
	s.combo.LegBBasePosition = legB
	if s.combo.FinalMetricsStatus != finalMetricsCompleted {
		s.combo.FinalMetricsStatus = finalMetricsPartial
		s.combo.FinalMetricsRetryFinite = false
		s.combo.FinalMetricsLastError = residualDustMetricsSkipped
		s.combo.FinalMetricsCompletedAt = time.Time{}
	}
	s.eventTypes = append(s.eventTypes, lastCloseResidualDustEvent)
	s.eventPayloads = append(s.eventPayloads, close.Payload)
	return s.combo, true, nil
}

func residualFlattenCombo(legA, legB string) ArbitrageCombination {
	now := time.Now().UTC()
	return ArbitrageCombination{
		ID: "combo-residual-flatten", OwnerUsername: "admin", Version: 4,
		Status: "closing", RuntimeState: "closing",
		LegABasePosition: legA, LegBBasePosition: legB,
		LegAVenueBasePosition: legA, LegBVenueBasePosition: legB,
		LegAVenueBaselineBasePosition: "0", LegBVenueBaselineBasePosition: "0",
		VenueBaselineCapturedAt: now, LastPositionReconciledAt: now,
		LegA: ArbitrageLeg{
			TradingAccountID: 55, InstrumentID: 101, Exchange: "bybit",
			ContractType: "perpetual", ExchangeSymbol: "ONEUSDT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 53, InstrumentID: 202, Exchange: "bybit",
			ContractType: "perpetual", ExchangeSymbol: "ONEUSDT",
		},
	}
}

func residualFlattenInstrument(id int64, step string) Instrument {
	return Instrument{
		ID: id, Exchange: "bybit", ContractType: "perpetual",
		ExchangeSymbol: "ONEUSDT", ContractSize: "1",
		QuantityStep: step, MarketQuantityStep: step,
		MarketQuantityStepStatus: exchange.ConstraintKnown,
		PriceTick:                "0.0001",
	}
}

func residualFlattenBBO(price string) marketdata.BBO {
	return marketdata.BBO{
		BidPrice: price, AskPrice: price,
		BidQuantity: "100000", AskQuantity: "100000",
		ReceiveTimestamp: time.Now().UTC(),
	}
}

func newResidualFlattenExecutor(store *residualFlattenStore, step string) *ArbitrageExecutor {
	return NewArbitrageExecutor(
		store, nil, nil, silentRiskCatalog{
			101: residualFlattenInstrument(101, step),
			202: residualFlattenInstrument(202, step),
		}, nil, nil, nil, "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
}

func TestLastCloseResidualFlattenClaimsUnequalReduceOnlyAndStaysClosing(t *testing.T) {
	store := &residualFlattenStore{
		executorFailStore: &executorFailStore{combo: residualFlattenCombo("170", "-200")},
		recovered:         true,
	}
	executor := newResidualFlattenExecutor(store, "100")
	bbo := residualFlattenBBO("0.01")
	if err := executor.RunLastCloseResidualFlatten(context.Background(), store.combo, bbo, bbo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closing" || store.createdOrders != 2 || store.claimCalls != 1 {
		t.Fatalf("status=%s created=%d claims=%d", store.combo.Status, store.createdOrders, store.claimCalls)
	}
	quantities := map[string]string{}
	for _, order := range store.orders {
		if !order.ReduceOnly || !strings.Contains(order.IdempotencyKey, lastCloseResidualFlattenIdempotencyRole) {
			t.Fatalf("order=%+v", order)
		}
		quantities[order.ArbitrageLeg] = order.Quantity
	}
	if quantities["a"] != "100" || quantities["b"] != "200" {
		t.Fatalf("quantities=%v", quantities)
	}
	if err := executor.RunLastCloseResidualFlatten(context.Background(), store.combo, bbo, bbo); err != nil {
		t.Fatal(err)
	}
	if store.createdOrders != 2 || len(store.orders) != 2 || store.combo.Status != "closing" {
		t.Fatalf("repeat created=%d orders=%d status=%s", store.createdOrders, len(store.orders), store.combo.Status)
	}
}

func TestLastCloseResidualFlattenRejectsUnsafeTiny(t *testing.T) {
	bbo := residualFlattenBBO("1")
	cases := []struct {
		name   string
		adjust func(*ArbitrageCombination, *residualFlattenStore)
	}{
		{name: "position uncertain", adjust: func(combo *ArbitrageCombination, _ *residualFlattenStore) {
			combo.PositionUncertain = true
			combo.RuntimeState = "position_uncertain"
		}},
		{name: "manual intervention", adjust: func(combo *ArbitrageCombination, _ *residualFlattenStore) {
			combo.RuntimeState = "manual_intervention"
		}},
		{name: "other active order", adjust: func(combo *ArbitrageCombination, store *residualFlattenStore) {
			store.executions = []ArbitrageExecution{{
				ID: "other-exec", CombinationID: combo.ID, Status: "hedging",
			}}
			store.orders = []Order{{
				ID: "other-order", ArbitrageExecutionID: "other-exec", Status: "open",
			}}
		}},
		{name: "venue direction mismatch", adjust: func(combo *ArbitrageCombination, _ *residualFlattenStore) {
			combo.LegAVenueBaselineBasePosition = "-10"
			combo.LegBVenueBaselineBasePosition = "10"
			combo.LegAVenueBasePosition = "-8.6"
			combo.LegBVenueBasePosition = "8"
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			combo := residualFlattenCombo("1.4", "-2")
			combo.CarryBaseQuantity = "-0.6"
			store := &residualFlattenStore{executorFailStore: &executorFailStore{combo: combo}}
			test.adjust(&store.combo, store)
			executor := newResidualFlattenExecutor(store, "0.1")
			executor.ConfigureFinalMetrics(store)
			if err := executor.RunLastCloseResidualFlatten(
				context.Background(), store.combo, bbo, bbo,
			); err != nil {
				t.Fatal(err)
			}
			if store.claimCalls != 0 || store.createdOrders != 0 || store.closedAfterMetrics != 0 {
				t.Fatalf("claims=%d created=%d closed=%d", store.claimCalls, store.createdOrders, store.closedAfterMetrics)
			}
			if store.combo.Status != "closing" {
				t.Fatalf("status=%s", store.combo.Status)
			}
		})
	}
}

func TestTrackedTinyFlatThenSchedulesFinalMetrics(t *testing.T) {
	combo := residualFlattenCombo("1.4", "-2")
	combo.CarryBaseQuantity = "-0.6"
	combo.RunMode = "one_shot"
	combo.OneShotPhase = "waiting_exit"
	store := &residualFlattenStore{executorFailStore: &executorFailStore{combo: combo}}
	executor := newResidualFlattenExecutor(store, "0.1")
	executor.ConfigureFinalMetrics(store)
	compensator := &countingCompensator{}
	executor.ConfigureFillAverageCompensator(compensator)
	bbo := residualFlattenBBO("1")
	if err := executor.RunLastCloseResidualFlatten(context.Background(), store.combo, bbo, bbo); err != nil {
		t.Fatal(err)
	}
	if store.claimCalls != 1 || store.createdOrders != 2 || store.combo.Status != "closing" {
		t.Fatalf("claims=%d created=%d status=%s", store.claimCalls, store.createdOrders, store.combo.Status)
	}
	store.mu.Lock()
	for index := range store.orders {
		store.orders[index].Status = "filled"
	}
	store.execution.Status = "completed"
	store.activeExecution = nil
	store.combo.LegABasePosition = "0"
	store.combo.LegBBasePosition = "0"
	store.combo.CarryBaseQuantity = "0"
	store.combo.LegAVenueBasePosition = "0"
	store.combo.LegBVenueBasePosition = "0"
	store.mu.Unlock()
	if err := executor.CloseCombination(context.Background(), store.combo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closed" || store.combo.OneShotPhase != "exited" ||
		store.combo.FinalMetricsStatus != finalMetricsPending || !store.combo.FinalMetricsRetryFinite ||
		store.persistFinalCalls != 0 || compensator.calls != 0 {
		t.Fatalf("combo=%+v persist=%d compensate=%d", store.combo, store.persistFinalCalls, compensator.calls)
	}
}

func TestLastCloseResidualFlattenTinyClaimsWithoutRecoveredEvent(t *testing.T) {
	combo := residualFlattenCombo("1.4", "-2")
	combo.CarryBaseQuantity = "-0.6"
	store := &residualFlattenStore{
		executorFailStore: &executorFailStore{combo: combo},
		recovered:         false,
	}
	executor := newResidualFlattenExecutor(store, "0.1")
	bbo := residualFlattenBBO("1")
	if err := executor.RunLastCloseResidualFlatten(context.Background(), store.combo, bbo, bbo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closing" || store.createdOrders != 2 || store.claimCalls != 1 {
		t.Fatalf("status=%s created=%d claims=%d", store.combo.Status, store.createdOrders, store.claimCalls)
	}
	if err := executor.RunLastCloseResidualFlatten(context.Background(), store.combo, bbo, bbo); err != nil {
		t.Fatal(err)
	}
	if store.createdOrders != 2 || store.claimCalls != 1 {
		t.Fatalf("repeat created=%d claims=%d", store.createdOrders, store.claimCalls)
	}
}

func TestResidualDustDoesNotDowngradeCompletedMetrics(t *testing.T) {
	completedAt := time.Now().UTC().Truncate(time.Microsecond)
	combo := residualFlattenCombo("70", "-70")
	combo.FinalMetricsStatus = finalMetricsCompleted
	combo.FinalMetricsCompletedAt = completedAt
	combo.FinalMetricsRetryFinite = false
	store := &residualFlattenStore{executorFailStore: &executorFailStore{combo: combo}}
	executor := newResidualFlattenExecutor(store, "100")
	bbo := residualFlattenBBO("0.01")
	if err := executor.RunLastCloseResidualFlatten(context.Background(), store.combo, bbo, bbo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closed" || store.combo.FinalMetricsStatus != finalMetricsCompleted ||
		!store.combo.FinalMetricsCompletedAt.Equal(completedAt) || store.createdOrders != 0 {
		t.Fatalf("combo=%+v created=%d", store.combo, store.createdOrders)
	}
}

func TestLastCloseResidualFlattenRestartDoesNotCreateSecondPair(t *testing.T) {
	combo := residualFlattenCombo("170", "-200")
	execution := ArbitrageExecution{
		ID: lastCloseResidualFlattenExecutionID(combo.ID), CombinationID: combo.ID,
		Status: "hedging", ReduceOnly: true, PositionEffect: "close",
	}
	copied := execution
	now := time.Now().UTC()
	store := &residualFlattenStore{
		executorFailStore: &executorFailStore{
			combo: combo, execution: execution, activeExecution: &copied,
			orders: []Order{
				{
					ID: "persisted-a", ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a",
					Side: "sell", OrderType: "market", Quantity: "100", Status: "filled",
					ReduceOnly: true, UpdatedAt: now,
					IdempotencyKey: lastCloseResidualFlattenIdempotencyKey(execution.ID, "a"),
				},
				{
					ID: "persisted-b", ArbitrageExecutionID: execution.ID, ArbitrageLeg: "b",
					Side: "buy", OrderType: "market", Quantity: "200", Status: "filled",
					ReduceOnly: true, UpdatedAt: now,
					IdempotencyKey: lastCloseResidualFlattenIdempotencyKey(execution.ID, "b"),
				},
			},
		},
		recovered: true,
	}
	store.combo.LastPositionReconciledAt = now.Add(-time.Second)
	executor := newResidualFlattenExecutor(store, "100")
	bbo := residualFlattenBBO("0.01")
	if err := executor.RunLastCloseResidualFlatten(context.Background(), store.combo, bbo, bbo); err != nil {
		t.Fatal(err)
	}
	if store.claimCalls != 0 || store.createdOrders != 0 || len(store.orders) != 2 {
		t.Fatalf("claims=%d created=%d orders=%d", store.claimCalls, store.createdOrders, len(store.orders))
	}
	if store.combo.Status != "closing" {
		t.Fatalf("stale snapshot closed combo=%+v", store.combo)
	}
	for _, eventType := range store.eventTypes {
		if eventType == lastCloseResidualDustEvent {
			t.Fatal("stale snapshot wrote dust")
		}
	}
}

func TestLastCloseResidualFlattenClaimConflictPlacesNothing(t *testing.T) {
	store := &residualFlattenStore{
		executorFailStore: &executorFailStore{combo: residualFlattenCombo("170", "-200")},
		recovered:         true,
		conflict:          true,
	}
	executor := newResidualFlattenExecutor(store, "100")
	bbo := residualFlattenBBO("0.01")
	if err := executor.RunLastCloseResidualFlatten(context.Background(), store.combo, bbo, bbo); err != nil {
		t.Fatal(err)
	}
	if store.createdOrders != 0 || len(store.orders) != 0 || store.combo.Status != "closing" {
		t.Fatalf("created=%d orders=%d status=%s", store.createdOrders, len(store.orders), store.combo.Status)
	}
}

func TestLastCloseResidualDustClosesAtomicallyWhenUntradeable(t *testing.T) {
	store := &residualFlattenStore{
		executorFailStore: &executorFailStore{combo: residualFlattenCombo("70", "-70")},
		recovered:         true,
	}
	executor := newResidualFlattenExecutor(store, "100")
	bbo := residualFlattenBBO("0.01")
	if err := executor.RunLastCloseResidualFlatten(context.Background(), store.combo, bbo, bbo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closed" || store.combo.LegABasePosition != "70" ||
		store.combo.LegBBasePosition != "-70" || store.createdOrders != 0 ||
		store.combo.FinalMetricsStatus != finalMetricsPartial || store.combo.FinalMetricsRetryFinite {
		t.Fatalf("combo=%+v created=%d", store.combo, store.createdOrders)
	}
	dust := 0
	for _, eventType := range store.eventTypes {
		if eventType == lastCloseResidualDustEvent {
			dust++
		}
	}
	if dust != 1 {
		t.Fatalf("events=%v", store.eventTypes)
	}
	if err := executor.RunLastCloseResidualFlatten(context.Background(), store.combo, bbo, bbo); err != nil {
		t.Fatal(err)
	}
	if dustCount := countEvents(store.eventTypes, lastCloseResidualDustEvent); dustCount != 1 {
		t.Fatalf("repeat events=%v", store.eventTypes)
	}
}

func TestLastCloseResidualLargeLegStaysClosing(t *testing.T) {
	store := &residualFlattenStore{
		executorFailStore: &executorFailStore{combo: residualFlattenCombo("30", "-30")},
		recovered:         true,
	}
	executor := newResidualFlattenExecutor(store, "1")
	bbo := residualFlattenBBO("1")
	if err := executor.RunLastCloseResidualFlatten(context.Background(), store.combo, bbo, bbo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closing" || store.claimCalls != 0 || store.createdOrders != 0 {
		t.Fatalf("status=%s claims=%d created=%d", store.combo.Status, store.claimCalls, store.createdOrders)
	}
	for _, eventType := range store.eventTypes {
		if eventType == lastCloseResidualDustEvent {
			t.Fatal("large leg wrote dust")
		}
	}
}

func countEvents(events []string, want string) int {
	count := 0
	for _, eventType := range events {
		if eventType == want {
			count++
		}
	}
	return count
}
