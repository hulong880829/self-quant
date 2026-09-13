package trader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

type circuitOpenQueryAdapter struct {
	mu          sync.Mutex
	results     map[string]exchange.Result
	placeCalls  int
	cancelCalls int
	queryCalls  int
}

func (a *circuitOpenQueryAdapter) GetBBO(
	context.Context, exchange.Instrument,
) (exchange.BBO, error) {
	return exchange.BBO{}, errors.New("unused adapter")
}

func (a *circuitOpenQueryAdapter) PlaceOrder(
	context.Context, exchange.Credentials, exchange.OrderRequest,
) (exchange.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.placeCalls++
	return exchange.Result{}, errors.New("place forbidden")
}

func (a *circuitOpenQueryAdapter) GetOrder(
	_ context.Context, _ exchange.Credentials, request exchange.QueryRequest,
) (exchange.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queryCalls++
	if result, ok := a.results[request.ClientOrderID]; ok {
		return result, nil
	}
	return exchange.Result{}, exchange.ErrOrderNotFound
}

func (a *circuitOpenQueryAdapter) CancelOrder(
	context.Context, exchange.Credentials, exchange.CancelRequest,
) (exchange.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancelCalls++
	return exchange.Result{}, errors.New("cancel forbidden")
}

func (a *circuitOpenQueryAdapter) CancelAndGetOrder(
	ctx context.Context, credentials exchange.Credentials, request exchange.CancelRequest,
) (exchange.Result, error) {
	return a.CancelOrder(ctx, credentials, request)
}

func (a *circuitOpenQueryAdapter) counts() (place, cancel, query int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.placeCalls, a.cancelCalls, a.queryCalls
}

type circuitOpenTestStore struct {
	executorFailStore
	memory      *memoryStore
	muApply     sync.Mutex
	applyCalls  int
	beforeApply func()
	lastReq     circuitOpenExternalReconcileRequest
}

func (s *circuitOpenTestStore) ListArbitrageOrders(
	context.Context, string, string,
) ([]Order, error) {
	s.memory.mu.Lock()
	defer s.memory.mu.Unlock()
	items := make([]Order, 0, len(s.memory.orders))
	for _, order := range s.memory.orders {
		items = append(items, order)
	}
	return items, nil
}

func (s *circuitOpenTestStore) CreateIntent(
	ctx context.Context, order Order,
) (Order, bool, error) {
	return s.memory.CreateIntent(ctx, order)
}

func (s *circuitOpenTestStore) GetByOwner(
	ctx context.Context, owner, id string,
) (Order, error) {
	return s.memory.GetByOwner(ctx, owner, id)
}

func (s *circuitOpenTestStore) ListByOwnerAccount(
	ctx context.Context, owner string, accountID int64, view string, limit int, cursor string,
) ([]Order, string, error) {
	return s.memory.ListByOwnerAccount(ctx, owner, accountID, view, limit, cursor)
}

func (s *circuitOpenTestStore) UpdateResult(
	ctx context.Context, orderID string, result VenueResult,
) (Order, error) {
	return s.memory.UpdateResult(ctx, orderID, result)
}

func (s *circuitOpenTestStore) AppendEvent(
	ctx context.Context, orderID, eventType string, payload map[string]any,
) error {
	return s.memory.AppendEvent(ctx, orderID, eventType, payload)
}

func (s *circuitOpenTestStore) CreateArbitrageIntents(
	context.Context, []Order,
) ([]Order, []bool, error) {
	return nil, nil, nil
}

func (s *circuitOpenTestStore) ApplyStreamUpdate(
	ctx context.Context, id string, _ StreamUpdate,
) (Order, error) {
	return s.GetByOwner(ctx, "", id)
}

func (s *circuitOpenTestStore) ApplyCircuitOpenExternalReconcile(
	_ context.Context,
	request circuitOpenExternalReconcileRequest,
) (ArbitrageCombination, bool, error) {
	if s.beforeApply != nil {
		s.beforeApply()
	}
	s.muApply.Lock()
	s.applyCalls++
	s.lastReq = request
	s.muApply.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.combo.Version != request.ExpectedVersion ||
		!s.combo.CircuitOpen || s.combo.Status != "running" {
		return s.combo, false, nil
	}
	s.memory.mu.Lock()
	orderSumA, orderSumB := decimal.Zero, decimal.Zero
	for _, order := range s.memory.orders {
		if !terminalStatus(order.Status) || order.ReconcileFailures > 0 {
			s.memory.mu.Unlock()
			return s.combo, false, nil
		}
		delta := signedFilledBase(order)
		if order.ArbitrageLeg == "b" {
			orderSumB = orderSumB.Add(delta)
		} else {
			orderSumA = orderSumA.Add(delta)
		}
	}
	s.memory.mu.Unlock()
	baselineA := parseDecimal(s.combo.LegAVenueBaselineBasePosition)
	baselineB := parseDecimal(s.combo.LegBVenueBaselineBasePosition)
	adjustmentA := request.VenueA.Sub(baselineA).Sub(orderSumA)
	adjustmentB := request.VenueB.Sub(baselineB).Sub(orderSumB)
	localA := orderSumA.Add(adjustmentA)
	localB := orderSumB.Add(adjustmentB)
	if !circuitOpenLegsNotSameDirection(localA, localB) {
		return applyCircuitOpenVenueSnapshot(&s.combo, request.VenueA, request.VenueB), false, nil
	}
	if !circuitOpenCarryDust(
		localA.Add(localB), request.InstrumentA, request.InstrumentB,
		request.MarkA, request.MarkB,
	) {
		return applyCircuitOpenVenueSnapshot(&s.combo, request.VenueA, request.VenueB), false, nil
	}
	updated := s.combo
	updated.LegABasePosition = localA.String()
	updated.LegBBasePosition = localB.String()
	_, comboNotional, _ := arbitragePositionNotionals(updated, request.MarkA, request.MarkB)
	s.combo.LegAReconciliationAdjustment = adjustmentA.String()
	s.combo.LegBReconciliationAdjustment = adjustmentB.String()
	s.combo.LegABasePosition = localA.String()
	s.combo.LegBBasePosition = localB.String()
	s.combo.CarryBaseQuantity = localA.Add(localB).String()
	s.combo.PositionNotional = comboNotional.String()
	s.combo.LegAPositionDifference = "0"
	s.combo.LegBPositionDifference = "0"
	s.combo.CircuitOpen = false
	s.combo.PositionUncertain = false
	s.combo.RuntimeState = "monitoring"
	s.combo.ErrorMessage = ""
	s.combo.LastFailureKey = ""
	s.combo.Version++
	s.execution.Status = "canceled"
	s.execution.ErrorMessage = "externally_reconciled"
	return s.combo, true, nil
}

func applyCircuitOpenVenueSnapshot(
	combo *ArbitrageCombination,
	venueA, venueB decimal.Decimal,
) ArbitrageCombination {
	expectedA, expectedB := arbitrageExpectedVenuePositions(
		*combo,
		parseDecimal(combo.LegABasePosition),
		parseDecimal(combo.LegBBasePosition),
	)
	combo.LegAVenueBasePosition = venueA.String()
	combo.LegBVenueBasePosition = venueB.String()
	combo.LegAPositionDifference = venueA.Sub(expectedA).String()
	combo.LegBPositionDifference = venueB.Sub(expectedB).String()
	combo.LastPositionReconciledAt = time.Now().UTC()
	return *combo
}

func circuitOpenTestInstrument(id int64, venue, symbol string) Instrument {
	return Instrument{
		ID: id, Exchange: venue, ContractType: "perpetual", ExchangeSymbol: symbol,
		BaseAsset: "XMR", QuoteAsset: "USDT", ContractSize: "1",
		QuantityStep: "0.001", MinQuantity: "0.001",
		MinQuantityStatus: exchange.ConstraintKnown,
		MinNotional:       "0.1", MinNotionalStatus: exchange.ConstraintKnown,
	}
}

func circuitOpenReadyCombo() ArbitrageCombination {
	now := time.Now().UTC()
	return ArbitrageCombination{
		ID: "combo-circuit", OwnerUsername: "admin", Status: "running",
		RuntimeState: "manual_intervention", CircuitOpen: true, Version: 3,
		TargetNotional: "1000", OrderNotional: "100",
		ErrorMessage:                  "venue result uncertain: timeout",
		LastFailureKey:                "venue result uncertain: timeout",
		LegAVenueBaselineBasePosition: "0",
		LegBVenueBaselineBasePosition: "0",
		VenueBaselineCapturedAt:       now,
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 101, Exchange: "binance",
			ContractType: "perpetual", ExchangeSymbol: "XMRUSDT",
			BaseAsset: "XMR", QuoteAsset: "USDT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 202, Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: "XMR-USDT-SWAP",
			BaseAsset: "XMR", QuoteAsset: "USDT",
		},
	}
}

func newCircuitOpenExecutor(
	store *circuitOpenTestStore,
	adapterA, adapterB *circuitOpenQueryAdapter,
	snapshots map[string]portfolio.Snapshot,
) *ArbitrageExecutor {
	catalog := silentRiskCatalog{
		101: circuitOpenTestInstrument(101, "binance", "XMRUSDT"),
		202: circuitOpenTestInstrument(202, "okx", "XMR-USDT-SWAP"),
	}
	registry := exchange.NewTestRegistry(map[string]exchange.Adapter{
		"binance": adapterA, "okx": adapterB,
	})
	service := NewService(store, catalog, nil, registry, time.Second, discardLogger())
	executor := NewArbitrageExecutor(
		store, store, service, catalog, silentRiskCredentials{}, registry,
		nil, "", time.Millisecond, 0, 0, 0, time.Second, discardLogger(),
	)
	executor.ConfigurePortfolios(positionAuditPortfolios{snapshots: snapshots})
	return executor
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestArbitrageExecutorReconcileCircuitOpenUnlocksBalancedPerps(t *testing.T) {
	assertCircuitOpenUnlocksBalancedPerps(t, circuitOpenReadyCombo())
}

func TestArbitrageExecutorReconcileCircuitOpenUnlocksRiskLimitEmptyMessage(t *testing.T) {
	combo := circuitOpenReadyCombo()
	combo.LastFailureKey = ErrRiskLimit.Error()
	combo.ErrorMessage = ""
	assertCircuitOpenUnlocksBalancedPerps(t, combo)
}

func assertCircuitOpenUnlocksBalancedPerps(t *testing.T, combo ArbitrageCombination) {
	t.Helper()
	memory := newMemoryStore()
	orderA := Order{
		ID: "order-a", ClientOrderID: "cloid-a", OwnerUsername: "admin",
		Status: "canceled", Quantity: "0.01", FilledQuantity: "0.01", Side: "buy",
		ArbitrageExecutionID: "exec-1", ArbitrageLeg: "a",
	}
	orderB := Order{
		ID: "order-b", ClientOrderID: "cloid-b", OwnerUsername: "admin",
		Status: "filled", Quantity: "0.01", FilledQuantity: "0.01", Side: "sell",
		ArbitrageExecutionID: "exec-1", ArbitrageLeg: "b",
	}
	memory.orders[orderA.ID] = orderA
	memory.orders[orderB.ID] = orderB
	store := &circuitOpenTestStore{
		executorFailStore: executorFailStore{
			combo: combo,
			execution: ArbitrageExecution{
				ID: "exec-1", CombinationID: combo.ID, Status: "reconciling",
			},
		},
		memory: memory,
	}
	adapterA := &circuitOpenQueryAdapter{results: map[string]exchange.Result{
		"cloid-a": {Status: "filled", FilledQuantity: "0.01", AveragePrice: "100"},
	}}
	adapterB := &circuitOpenQueryAdapter{results: map[string]exchange.Result{
		"cloid-b": {Status: "filled", FilledQuantity: "0.01", AveragePrice: "100"},
	}}
	executor := newCircuitOpenExecutor(store, adapterA, adapterB, map[string]portfolio.Snapshot{
		"binance": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "binance", WireSymbol: "XMRUSDT",
			SignedContractSize: "0.01", MarkPrice: "100",
		}}},
		"okx": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "okx", WireSymbol: "XMR-USDT-SWAP",
			SignedContractSize: "-0.01", MarkPrice: "100",
		}}},
	})
	if err := executor.ReconcileCircuitOpen(
		context.Background(), combo, store.execution,
	); err != nil {
		t.Fatal(err)
	}
	placeA, cancelA, queryA := adapterA.counts()
	placeB, cancelB, queryB := adapterB.counts()
	if placeA != 0 || placeB != 0 || cancelA != 0 || cancelB != 0 {
		t.Fatalf("place/cancel a=%d/%d b=%d/%d", placeA, cancelA, placeB, cancelB)
	}
	if queryA != 0 || queryB != 0 {
		t.Fatalf("query a=%d b=%d", queryA, queryB)
	}
	if store.applyCalls != 1 {
		t.Fatalf("applyCalls=%d", store.applyCalls)
	}
	if store.combo.CircuitOpen || store.combo.RuntimeState != "monitoring" {
		t.Fatalf("combo=%+v", store.combo)
	}
	if store.combo.LegAPositionDifference != "0" || store.combo.LegBPositionDifference != "0" {
		t.Fatalf("unlock diffs=%+v", store.combo)
	}
	if store.execution.Status != "canceled" ||
		store.execution.ErrorMessage != "externally_reconciled" {
		t.Fatalf("execution=%+v", store.execution)
	}
}

func TestArbitrageExecutorReconcileCircuitOpenUnlocksOverTarget(t *testing.T) {
	combo := circuitOpenReadyCombo()
	combo.TargetNotional = "500"
	combo.OrderNotional = "20"
	combo.ErrorMessage = "Margin is insufficient"
	combo.LegAReconciliationAdjustment = "0"
	combo.LegBReconciliationAdjustment = "-38"
	combo.PositionNotional = "493.86725"
	memory := newMemoryStore()
	memory.orders["order-a"] = Order{
		ID: "order-a", ClientOrderID: "cloid-a", OwnerUsername: "admin",
		Status: "filled", Quantity: "1340", FilledQuantity: "1340", Side: "buy",
		ArbitrageExecutionID: "exec-1", ArbitrageLeg: "a",
	}
	memory.orders["order-b"] = Order{
		ID: "order-b", ClientOrderID: "cloid-b", OwnerUsername: "admin",
		Status: "filled", Quantity: "1287", FilledQuantity: "1287", Side: "sell",
		ArbitrageExecutionID: "exec-1", ArbitrageLeg: "b",
	}
	store := &circuitOpenTestStore{
		executorFailStore: executorFailStore{
			combo: combo,
			execution: ArbitrageExecution{
				ID: "exec-1", CombinationID: combo.ID, Status: "reconciling",
			},
		},
		memory: memory,
	}
	adapterA := &circuitOpenQueryAdapter{results: map[string]exchange.Result{
		"cloid-a": {Status: "filled", FilledQuantity: "1340", AveragePrice: "0.463"},
	}}
	adapterB := &circuitOpenQueryAdapter{results: map[string]exchange.Result{
		"cloid-b": {Status: "filled", FilledQuantity: "1287", AveragePrice: "0.463"},
	}}
	executor := newCircuitOpenExecutor(store, adapterA, adapterB, map[string]portfolio.Snapshot{
		"binance": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "binance", WireSymbol: "XMRUSDT",
			SignedContractSize: "1340", MarkPrice: "0.463",
		}}},
		"okx": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "okx", WireSymbol: "XMR-USDT-SWAP",
			SignedContractSize: "-1340", MarkPrice: "0.463",
		}}},
	})
	if err := executor.ReconcileCircuitOpen(
		context.Background(), combo, store.execution,
	); err != nil {
		t.Fatal(err)
	}
	if store.combo.CircuitOpen || store.combo.RuntimeState != "monitoring" {
		t.Fatalf("combo=%+v", store.combo)
	}
	if store.combo.TargetNotional != "500" {
		t.Fatalf("target mutated=%s", store.combo.TargetNotional)
	}
	if store.combo.LegAReconciliationAdjustment != "0" ||
		store.combo.LegBReconciliationAdjustment != "-53" {
		t.Fatalf("adjustments=%+v", store.combo)
	}
	if store.combo.LegABasePosition != "1340" || store.combo.LegBBasePosition != "-1340" {
		t.Fatalf("positions=%+v", store.combo)
	}
	if store.combo.CarryBaseQuantity != "0" {
		t.Fatalf("carry=%s", store.combo.CarryBaseQuantity)
	}
	wantNotional := decimal.RequireFromString("1340").Mul(decimal.RequireFromString("0.463"))
	if parseDecimal(store.combo.PositionNotional).Cmp(wantNotional) != 0 {
		t.Fatalf("position_notional=%s want=%s", store.combo.PositionNotional, wantNotional)
	}
	if store.execution.Status != "canceled" ||
		store.execution.ErrorMessage != "externally_reconciled" {
		t.Fatalf("execution=%+v", store.execution)
	}
}

func TestArbitrageExecutorReconcileCircuitOpenRecomputesOrderSum(t *testing.T) {
	combo := circuitOpenReadyCombo()
	memory := newMemoryStore()
	orderA := Order{
		ID: "order-a", ClientOrderID: "cloid-a", OwnerUsername: "admin",
		Status: "filled", Quantity: "0.03", FilledQuantity: "0.026", Side: "buy",
		ArbitrageExecutionID: "exec-1", ArbitrageLeg: "a",
	}
	orderB := Order{
		ID: "order-b", ClientOrderID: "cloid-b", OwnerUsername: "admin",
		Status: "filled", Quantity: "0.029", FilledQuantity: "0.029", Side: "sell",
		ArbitrageExecutionID: "exec-1", ArbitrageLeg: "b",
	}
	memory.orders[orderA.ID] = orderA
	memory.orders[orderB.ID] = orderB
	store := &circuitOpenTestStore{
		executorFailStore: executorFailStore{
			combo:     combo,
			execution: ArbitrageExecution{ID: "exec-1", Status: "reconciling"},
		},
		memory: memory,
	}
	store.beforeApply = func() {
		store.memory.mu.Lock()
		updated := store.memory.orders["order-a"]
		updated.FilledQuantity = "0.029"
		store.memory.orders["order-a"] = updated
		store.memory.mu.Unlock()
	}
	adapterA := &circuitOpenQueryAdapter{results: map[string]exchange.Result{
		"cloid-a": {Status: "filled", FilledQuantity: "0.026", AveragePrice: "100"},
	}}
	adapterB := &circuitOpenQueryAdapter{results: map[string]exchange.Result{
		"cloid-b": {Status: "filled", FilledQuantity: "0.029", AveragePrice: "100"},
	}}
	executor := newCircuitOpenExecutor(store, adapterA, adapterB, map[string]portfolio.Snapshot{
		"binance": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "binance", WireSymbol: "XMRUSDT",
			SignedContractSize: "0.029", MarkPrice: "100",
		}}},
		"okx": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "okx", WireSymbol: "XMR-USDT-SWAP",
			SignedContractSize: "-0.029", MarkPrice: "100",
		}}},
	})
	if err := executor.ReconcileCircuitOpen(
		context.Background(), combo, store.execution,
	); err != nil {
		t.Fatal(err)
	}
	if store.applyCalls != 1 {
		t.Fatalf("applyCalls=%d", store.applyCalls)
	}
	if parseDecimal(store.combo.LegAReconciliationAdjustment).IsPositive() {
		t.Fatalf("adjustment double-counted: %+v", store.combo)
	}
	if store.combo.LegABasePosition != "0.029" || store.combo.LegBBasePosition != "-0.029" {
		t.Fatalf("positions=%+v", store.combo)
	}
}

func TestArbitrageExecutorReconcileCircuitOpenSkipsTerminalOrderQueries(t *testing.T) {
	combo := circuitOpenReadyCombo()
	memory := newMemoryStore()
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("filled-a-%d", i)
		memory.orders[id] = Order{
			ID: id, ClientOrderID: id, OwnerUsername: "admin",
			Status: "filled", Quantity: "0.001", FilledQuantity: "0.001", Side: "buy",
			ArbitrageExecutionID: "exec-1", ArbitrageLeg: "a",
		}
		id = fmt.Sprintf("filled-b-%d", i)
		memory.orders[id] = Order{
			ID: id, ClientOrderID: id, OwnerUsername: "admin",
			Status: "filled", Quantity: "0.001", FilledQuantity: "0.001", Side: "sell",
			ArbitrageExecutionID: "exec-1", ArbitrageLeg: "b",
		}
	}
	store := &circuitOpenTestStore{
		executorFailStore: executorFailStore{
			combo:     combo,
			execution: ArbitrageExecution{ID: "exec-1", CombinationID: combo.ID, Status: "reconciling"},
		},
		memory: memory,
	}
	adapterA := &circuitOpenQueryAdapter{results: map[string]exchange.Result{}}
	adapterB := &circuitOpenQueryAdapter{results: map[string]exchange.Result{}}
	executor := newCircuitOpenExecutor(store, adapterA, adapterB, map[string]portfolio.Snapshot{
		"binance": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "binance", WireSymbol: "XMRUSDT",
			SignedContractSize: "0.008", MarkPrice: "100",
		}}},
		"okx": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "okx", WireSymbol: "XMR-USDT-SWAP",
			SignedContractSize: "-0.008", MarkPrice: "100",
		}}},
	})
	if err := executor.ReconcileCircuitOpen(
		context.Background(), combo, store.execution,
	); err != nil {
		t.Fatal(err)
	}
	_, _, queryA := adapterA.counts()
	_, _, queryB := adapterB.counts()
	if queryA != 0 || queryB != 0 {
		t.Fatalf("terminal orders queried a=%d b=%d", queryA, queryB)
	}
	if store.applyCalls != 1 {
		t.Fatalf("applyCalls=%d", store.applyCalls)
	}
}

func TestArbitrageExecutorReconcileCircuitOpenQueriesOnlyLiveOrders(t *testing.T) {
	combo := circuitOpenReadyCombo()
	memory := newMemoryStore()
	memory.orders["order-a"] = Order{
		ID: "order-a", ClientOrderID: "cloid-a", OwnerUsername: "admin",
		Status: "pending", Quantity: "0.01", FilledQuantity: "0", Side: "buy",
		ArbitrageExecutionID: "exec-1", ArbitrageLeg: "a",
	}
	memory.orders["order-b"] = Order{
		ID: "order-b", ClientOrderID: "cloid-b", OwnerUsername: "admin",
		Status: "filled", Quantity: "0.01", FilledQuantity: "0.01", Side: "sell",
		ArbitrageExecutionID: "exec-1", ArbitrageLeg: "b",
	}
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("hist-a-%d", i)
		memory.orders[id] = Order{
			ID: id, ClientOrderID: id, OwnerUsername: "admin",
			Status: "canceled", Quantity: "0.01", FilledQuantity: "0", Side: "buy",
			ArbitrageExecutionID: "exec-1", ArbitrageLeg: "a",
		}
		id = fmt.Sprintf("hist-b-%d", i)
		memory.orders[id] = Order{
			ID: id, ClientOrderID: id, OwnerUsername: "admin",
			Status: "canceled", Quantity: "0.01", FilledQuantity: "0", Side: "sell",
			ArbitrageExecutionID: "exec-1", ArbitrageLeg: "b",
		}
	}
	store := &circuitOpenTestStore{
		executorFailStore: executorFailStore{
			combo:     combo,
			execution: ArbitrageExecution{ID: "exec-1", CombinationID: combo.ID, Status: "reconciling"},
		},
		memory: memory,
	}
	adapterA := &circuitOpenQueryAdapter{results: map[string]exchange.Result{
		"cloid-a": {Status: "filled", FilledQuantity: "0.01", AveragePrice: "100"},
	}}
	adapterB := &circuitOpenQueryAdapter{results: map[string]exchange.Result{}}
	executor := newCircuitOpenExecutor(store, adapterA, adapterB, map[string]portfolio.Snapshot{
		"binance": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "binance", WireSymbol: "XMRUSDT",
			SignedContractSize: "0.01", MarkPrice: "100",
		}}},
		"okx": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "okx", WireSymbol: "XMR-USDT-SWAP",
			SignedContractSize: "-0.01", MarkPrice: "100",
		}}},
	})
	if err := executor.ReconcileCircuitOpen(
		context.Background(), combo, store.execution,
	); err != nil {
		t.Fatal(err)
	}
	_, _, queryA := adapterA.counts()
	_, _, queryB := adapterB.counts()
	if queryA != 1 || queryB != 0 {
		t.Fatalf("query a=%d b=%d", queryA, queryB)
	}
	if store.applyCalls != 1 {
		t.Fatalf("applyCalls=%d", store.applyCalls)
	}
}

func TestArbitrageExecutorReconcileCircuitOpenSkipsExecutableCarry(t *testing.T) {
	combo := circuitOpenReadyCombo()
	memory := newMemoryStore()
	memory.orders["order-a"] = Order{
		ID: "order-a", ClientOrderID: "cloid-a", Status: "filled",
		Quantity: "0.01", FilledQuantity: "0.01", Side: "buy",
		ArbitrageExecutionID: "exec-1", ArbitrageLeg: "a",
	}
	memory.orders["order-b"] = Order{
		ID: "order-b", ClientOrderID: "cloid-b", Status: "filled",
		Quantity: "0.005", FilledQuantity: "0.005", Side: "sell",
		ArbitrageExecutionID: "exec-1", ArbitrageLeg: "b",
	}
	store := &circuitOpenTestStore{
		executorFailStore: executorFailStore{
			combo: combo, execution: ArbitrageExecution{ID: "exec-1"},
		},
		memory: memory,
	}
	adapterA := &circuitOpenQueryAdapter{results: map[string]exchange.Result{
		"cloid-a": {Status: "filled", FilledQuantity: "0.01"},
	}}
	adapterB := &circuitOpenQueryAdapter{results: map[string]exchange.Result{
		"cloid-b": {Status: "filled", FilledQuantity: "0.005"},
	}}
	executor := newCircuitOpenExecutor(store, adapterA, adapterB, map[string]portfolio.Snapshot{
		"binance": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "binance", WireSymbol: "XMRUSDT",
			SignedContractSize: "0.01", MarkPrice: "100",
		}}},
		"okx": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "okx", WireSymbol: "XMR-USDT-SWAP",
			SignedContractSize: "-0.005", MarkPrice: "100",
		}}},
	})
	catalog := silentRiskCatalog{
		101: circuitOpenTestInstrument(101, "binance", "XMRUSDT"),
		202: func() Instrument {
			item := circuitOpenTestInstrument(202, "okx", "XMR-USDT-SWAP")
			item.QuantityStep = "0.01"
			item.MinQuantity = "0.01"
			return item
		}(),
	}
	executor.catalog = catalog
	if err := executor.ReconcileCircuitOpen(
		context.Background(), combo, store.execution,
	); err != nil {
		t.Fatal(err)
	}
	if !store.combo.CircuitOpen || store.combo.RuntimeState != combo.RuntimeState ||
		store.combo.Version != combo.Version {
		t.Fatalf("combo=%+v", store.combo)
	}
	if store.combo.LegABasePosition != combo.LegABasePosition ||
		store.combo.LegBBasePosition != combo.LegBBasePosition {
		t.Fatalf("ledger changed: %+v", store.combo)
	}
	if store.combo.LegAVenueBasePosition != "0.01" ||
		store.combo.LegBVenueBasePosition != "-0.005" ||
		store.combo.LegAPositionDifference != "0.01" ||
		store.combo.LegBPositionDifference != "-0.005" ||
		store.combo.LastPositionReconciledAt.IsZero() {
		t.Fatalf("snapshot=%+v", store.combo)
	}
}

func TestCircuitOpenApplySkipsSnapshotWhenOrdersUnconfirmed(t *testing.T) {
	combo := circuitOpenReadyCombo()
	memory := newMemoryStore()
	memory.orders["order-a"] = Order{
		ID: "order-a", Status: "pending", Quantity: "0.01", FilledQuantity: "0",
		Side: "buy", ArbitrageExecutionID: "exec-1", ArbitrageLeg: "a",
	}
	store := &circuitOpenTestStore{
		executorFailStore: executorFailStore{
			combo: combo, execution: ArbitrageExecution{ID: "exec-1"},
		},
		memory: memory,
	}
	instrument := circuitOpenTestInstrument(101, "binance", "XMRUSDT")
	updated, ok, err := store.ApplyCircuitOpenExternalReconcile(
		context.Background(), circuitOpenExternalReconcileRequest{
			CombinationID:   combo.ID,
			ExecutionID:     "exec-1",
			ExpectedVersion: combo.Version,
			VenueA:          decimal.RequireFromString("0.01"),
			VenueB:          decimal.RequireFromString("-0.01"),
			MarkA:           decimal.RequireFromString("100"),
			MarkB:           decimal.RequireFromString("100"),
			InstrumentA:     instrument,
			InstrumentB:     instrument,
		},
	)
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !updated.CircuitOpen ||
		updated.LegAVenueBasePosition != combo.LegAVenueBasePosition ||
		updated.LegBVenueBasePosition != combo.LegBVenueBasePosition ||
		!updated.LastPositionReconciledAt.IsZero() {
		t.Fatalf("unconfirmed wrote snapshot: %+v", updated)
	}
}

func TestArbitrageSchedulerCircuitOpenReconcilesWithoutBBO(t *testing.T) {
	execution := ArbitrageExecution{
		ID: "exec-1", CombinationID: "combo-1", Status: "reconciling",
	}
	item := ArbitrageCombination{
		ID: "combo-1", Status: "running", CircuitOpen: true,
		RuntimeState: "manual_intervention",
		ErrorMessage: "venue result uncertain", LastFailureKey: "venue result uncertain",
		LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "binance"},
		LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "okx"},
	}
	store := &dryRunStore{item: item, activeExecution: &execution}
	runner := &schedulerRunner{
		executed:   make(chan ArbitrageExecution, 1),
		reconciled: make(chan ArbitrageExecution, 1),
		recovered:  make(chan ArbitrageExecution, 1),
	}
	scheduler := NewArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		1, 1, 1, 2, false, discardLogger(),
	)
	runtime := &arbitrageRuntime{
		combination: item,
		legA:        &marketdata.Subscription{},
		legB:        &marketdata.Subscription{},
	}
	stop, _ := scheduler.handleControl(context.Background(), runtime)
	if stop {
		t.Fatal("running combination should keep the runtime")
	}
	select {
	case got := <-runner.reconciled:
		if got.ID != execution.ID {
			t.Fatalf("execution=%+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("expected readonly reconcile without BBO")
	}
	select {
	case <-runner.executed:
		t.Fatal("circuit open must not Execute")
	default:
	}
	waitForExecutionRunner(t, runtime)
}

func TestArbitrageSchedulerCircuitOpenThrottleAppliesOnFailure(t *testing.T) {
	execution := ArbitrageExecution{
		ID: "exec-1", CombinationID: "combo-1", Status: "reconciling",
	}
	item := ArbitrageCombination{
		ID: "combo-1", Status: "running", CircuitOpen: true,
		LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "binance"},
		LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "okx"},
	}
	store := &dryRunStore{item: item, activeExecution: &execution}
	runner := &schedulerRunner{
		reconciled:   make(chan ArbitrageExecution, 2),
		reconcileErr: errors.New("venue timeout"),
	}
	scheduler := NewArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		1, 1, 1, 2, false, discardLogger(),
	)
	runtime := &arbitrageRuntime{
		combination: item,
		legA:        &marketdata.Subscription{},
		legB:        &marketdata.Subscription{},
	}
	scheduler.handleControl(context.Background(), runtime)
	waitForExecutionRunner(t, runtime)
	scheduler.handleControl(context.Background(), runtime)
	waitForExecutionRunner(t, runtime)
	if len(runner.reconciled) != 1 {
		t.Fatalf("reconcile calls=%d", len(runner.reconciled))
	}
}

func TestArbitrageSchedulerCircuitOpenSkipsClosingAndMissingExecution(t *testing.T) {
	runner := &schedulerRunner{reconciled: make(chan ArbitrageExecution, 1)}
	scheduler := NewArbitrageScheduler(
		&dryRunStore{item: ArbitrageCombination{ID: "c", Status: "closing"}},
		nil, runner, time.Second, time.Millisecond, time.Second,
		1, 1, 1, 2, false, discardLogger(),
	)
	runtime := &arbitrageRuntime{
		combination: ArbitrageCombination{
			ID: "c", Status: "closing", CircuitOpen: true,
		},
		legA: &marketdata.Subscription{},
		legB: &marketdata.Subscription{},
	}
	scheduler.handleControl(context.Background(), runtime)
	select {
	case <-runner.reconciled:
		t.Fatal("closing must not auto reconcile")
	default:
	}

	running := ArbitrageCombination{
		ID: "combo-1", Status: "running", CircuitOpen: true,
		LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "binance"},
		LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "okx"},
	}
	scheduler = NewArbitrageScheduler(
		&dryRunStore{item: running},
		nil, runner, time.Second, time.Millisecond, time.Second,
		1, 1, 1, 2, false, discardLogger(),
	)
	runtime = &arbitrageRuntime{
		combination: running,
		legA:        &marketdata.Subscription{},
		legB:        &marketdata.Subscription{},
	}
	scheduler.handleControl(context.Background(), runtime)
	select {
	case <-runner.reconciled:
		t.Fatal("missing execution must not auto reconcile")
	default:
	}
}
