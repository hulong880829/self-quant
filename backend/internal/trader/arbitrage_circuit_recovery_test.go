package trader

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

func TestExplicitVenueRejectSkipsOrderQuery(t *testing.T) {
	combo := circuitOpenReadyCombo()
	memory := newMemoryStore()
	memory.orders["pending-1"] = Order{
		ID: "pending-1", ClientOrderID: "pending-1", Status: "pending", FilledQuantity: "0",
	}
	memory.orders["timeout-1"] = Order{
		ID: "timeout-1", ClientOrderID: "timeout-1", Status: "rejected",
		FilledQuantity: "0", ErrorCode: "timeout",
	}
	store := &circuitOpenTestStore{
		executorFailStore: executorFailStore{combo: combo},
		memory:            memory,
	}
	adapter := &circuitOpenQueryAdapter{results: map[string]exchange.Result{
		"pending-1": {Status: "filled", FilledQuantity: "1", AveragePrice: "1"},
		"timeout-1": {Status: "rejected", FilledQuantity: "0"},
	}}
	executor := newCircuitOpenExecutor(store, adapter, adapter, map[string]portfolio.Snapshot{})
	instrument := circuitOpenTestInstrument(101, "bybit", "SANDUSDT")
	credentials := Credentials{Exchange: "bybit", TradingAccountID: 1, APIKey: "k", APISecret: "s"}
	rejected := Order{
		ID: "reject-1", ClientOrderID: "reject-1", Status: "rejected",
		FilledQuantity: "0", ErrorCode: "110007", Exchange: "bybit", ArbitrageRole: "hedge",
	}
	if _, err := executor.settleCanceledOrder(
		context.Background(), credentials, instrument, adapter, rejected, orderObservation{},
	); err != nil {
		t.Fatal(err)
	}
	if _, _, queries := adapter.counts(); queries != 0 {
		t.Fatalf("explicit reject queried %d", queries)
	}
	pending := Order{
		ID: "pending-1", ClientOrderID: "pending-1", Status: "pending", FilledQuantity: "0",
	}
	if _, err := executor.settleCanceledOrder(
		context.Background(), credentials, instrument, adapter, pending, orderObservation{},
	); err != nil {
		t.Fatal(err)
	}
	timeoutOrder := Order{
		ID: "timeout-1", ClientOrderID: "timeout-1", Status: "rejected",
		FilledQuantity: "0", ErrorCode: "timeout",
	}
	_, _ = executor.settleCanceledOrder(
		context.Background(), credentials, instrument, adapter, timeoutOrder, orderObservation{},
	)
	if _, _, queries := adapter.counts(); queries < 2 {
		t.Fatalf("ambiguous orders queried %d", queries)
	}
}

func TestCircuitOpenManualHotPathSkipsExecutionRead(t *testing.T) {
	for _, phase := range []string{"", "building", "building_target", "waiting_exit"} {
		item := ArbitrageCombination{
			ID: "combo-hot", Status: "running", CircuitOpen: true,
			RuntimeState: "manual_intervention", OneShotPhase: phase,
			LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "binance"},
			LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "okx"},
		}
		store := &dryRunStore{
			item: item,
			activeExecution: &ArbitrageExecution{
				ID: "exec-hot", Status: "hedging", CombinationID: item.ID,
			},
		}
		runner := &schedulerRunner{recovered: make(chan ArbitrageExecution, 1)}
		scheduler := NewArbitrageScheduler(
			store, nil, runner, time.Second, time.Millisecond, time.Second,
			1, 1, 1, 2, false, discardLogger(),
		)
		runtime := &arbitrageRuntime{combination: item}
		scheduler.resumeLockedSafe(
			context.Background(), runtime, marketdata.BBO{}, marketdata.BBO{}, true,
		)
		if store.activeLookups != 0 {
			t.Fatalf("phase %q lookups=%d", phase, store.activeLookups)
		}
		select {
		case got := <-runner.recovered:
			t.Fatalf("phase %q recovered %+v", phase, got)
		default:
		}
	}

	closing := ArbitrageCombination{
		ID: "combo-close", Status: "closing", CircuitOpen: true,
		RuntimeState: "manual_intervention",
		LegA:         ArbitrageLeg{TradingAccountID: 1, Exchange: "binance"},
		LegB:         ArbitrageLeg{TradingAccountID: 2, Exchange: "okx"},
	}
	store := &dryRunStore{item: closing}
	runner := &schedulerRunner{recovered: make(chan ArbitrageExecution, 1)}
	scheduler := NewArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		1, 1, 1, 2, false, discardLogger(),
	)
	scheduler.resumeLockedSafe(
		context.Background(), &arbitrageRuntime{combination: closing},
		marketdata.BBO{}, marketdata.BBO{}, false,
	)
	if store.activeLookups == 0 {
		t.Fatal("closing was suppressed")
	}

	exiting := closing
	exiting.ID = "combo-exit"
	exiting.Status = "running"
	exiting.RunMode = "one_shot"
	exiting.OneShotPhase = "exiting"
	exitStore := &dryRunStore{item: exiting}
	exitScheduler := NewArbitrageScheduler(
		exitStore, nil, runner, time.Second, time.Millisecond, time.Second,
		1, 1, 1, 2, false, discardLogger(),
	)
	exitScheduler.resumeLockedSafe(
		context.Background(), &arbitrageRuntime{combination: exiting},
		marketdata.BBO{}, marketdata.BBO{}, false,
	)
	if exitStore.activeLookups == 0 {
		t.Fatal("one-shot exiting was suppressed")
	}
}

func TestCircuitReconcileBlockedWhileRecoveryRunning(t *testing.T) {
	execution := ArbitrageExecution{ID: "exec-1", CombinationID: "combo-1", Status: "hedging"}
	item := ArbitrageCombination{
		ID: "combo-1", Status: "running", CircuitOpen: true,
		RuntimeState: "manual_intervention",
		LegA:         ArbitrageLeg{TradingAccountID: 1, Exchange: "binance"},
		LegB:         ArbitrageLeg{TradingAccountID: 2, Exchange: "okx"},
	}
	store := &dryRunStore{item: item, activeExecution: &execution}
	runner := &schedulerRunner{reconciled: make(chan ArbitrageCombination, 1)}
	scheduler := NewArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		1, 1, 1, 2, false, discardLogger(),
	)
	runtime := &arbitrageRuntime{combination: item}
	runtime.recoveryRunning.Store(true)
	scheduler.reconcileCircuitOpen(context.Background(), runtime)
	select {
	case got := <-runner.reconciled:
		t.Fatalf("started while recovery running: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestCircuitQuoteZeroFillClaimsNewExecution(t *testing.T) {
	combo := circuitOpenReadyCombo()
	combo.AskThresholdBps = "1"
	combo.BidThresholdBps = "-100"
	combo.CurrentAskSpreadBps = "20"
	combo.CurrentBidSpreadBps = "0"
	combo.TargetNotional = "1000"
	combo.OrderNotional = "100"
	combo.PositionNotional = "0"
	combo.LegAVenueBaselineBasePosition = "0"
	combo.LegBVenueBaselineBasePosition = "0"
	failed := ArbitrageExecution{
		ID: "failed-quote", CombinationID: combo.ID, Status: "failed",
		Direction: "ask", PositionEffect: "open", MakerOrderID: "maker-1",
	}
	maker := Order{
		ID: "maker-1", ClientOrderID: "maker-1", Status: "rejected",
		FilledQuantity: "0", ErrorCode: "110007", Exchange: "bybit",
		ArbitrageRole: "maker", ArbitrageLeg: "a", ArbitrageExecutionID: failed.ID,
		Side: "buy",
	}
	other := Order{
		ID: "other-110007", Status: "rejected", FilledQuantity: "0",
		ErrorCode: "110007", Exchange: "bybit", ArbitrageRole: "hedge",
		ArbitrageExecutionID: "someone-else", ArbitrageLeg: "b",
	}
	memory := newMemoryStore()
	memory.orders[maker.ID] = maker
	memory.orders[other.ID] = other
	store := &circuitOpenTestStore{
		executorFailStore: executorFailStore{
			combo: combo, execution: failed,
			executions: []ArbitrageExecution{failed},
			orders:     []Order{maker, other},
		},
		memory: memory,
	}
	adapter := &circuitOpenQueryAdapter{}
	catalog := silentRiskCatalog{
		101: circuitOpenTestInstrument(101, "binance", "XMRUSDT"),
		202: circuitOpenTestInstrument(202, "okx", "XMR-USDT-SWAP"),
	}
	registry := exchange.NewTestRegistry(map[string]exchange.Adapter{
		"binance": adapter, "okx": adapter,
	})
	service := NewService(store, catalog, nil, registry, time.Second, discardLogger())
	executor := NewArbitrageExecutor(
		store, store, service, catalog, silentRiskCredentials{}, registry,
		nil, "", time.Millisecond, 0, 0, 0, time.Second, discardLogger(),
	)
	executor.ConfigurePortfolios(positionAuditPortfolios{snapshots: map[string]portfolio.Snapshot{
		"binance": {}, "okx": {},
	}})
	if _, claimed, err := store.ClaimArbitrageExecution(
		context.Background(), ArbitrageExecution{CombinationID: combo.ID},
	); err != nil || claimed {
		t.Fatalf("manual claim claimed=%v err=%v", claimed, err)
	}
	if err := executor.ReconcileCircuitOpen(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.claimCircuitCalls != 1 || store.lastCircuitClaim.FailedExecutionID != failed.ID {
		t.Fatalf("claim=%+v calls=%d", store.lastCircuitClaim, store.claimCircuitCalls)
	}
	if store.lastCircuitClaim.Execution.ID == failed.ID ||
		store.lastCircuitClaim.Execution.PositionEffect != "open" {
		t.Fatalf("new execution=%+v", store.lastCircuitClaim.Execution)
	}
	for _, item := range store.executions {
		if item.ID == failed.ID && item.Status != "failed" {
			t.Fatalf("failed execution rewritten: %+v", item)
		}
	}
	if !store.combo.CircuitOpen || store.combo.RuntimeState != "manual_intervention" {
		t.Fatalf("circuit cleared: %+v", store.combo)
	}
	if store.positionFromBaseCalls != 0 {
		t.Fatalf("position base calls=%d", store.positionFromBaseCalls)
	}
	if _, _, queries := adapter.counts(); queries != 0 {
		t.Fatalf("explicit quote reject queried %d", queries)
	}

	combo.OneShotPhase = "waiting_exit"
	store.combo = combo
	store.claimCircuitCalls = 0
	if err := executor.ReconcileCircuitOpen(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.claimCircuitCalls != 0 {
		t.Fatalf("waiting_exit claimed %d", store.claimCircuitCalls)
	}
}

func TestCircuitHedgeReplacementUsesHistoricalCarry(t *testing.T) {
	combo := circuitOpenReadyCombo()
	combo.ExecutionMode = "maker_then_hedge"
	combo.MakerLeg = "b"
	combo.LegAVenueBaselineBasePosition = "27206"
	combo.LegBVenueBaselineBasePosition = "-27206"
	combo.CarryBaseQuantity = "-246"
	combo.LegA = ArbitrageLeg{
		TradingAccountID: 1, InstrumentID: 101, Exchange: "bybit",
		ContractType: "perpetual", ExchangeSymbol: "SANDUSDT",
		BaseAsset: "SAND", QuoteAsset: "USDT",
	}
	combo.LegB = ArbitrageLeg{
		TradingAccountID: 2, InstrumentID: 202, Exchange: "binance",
		ContractType: "perpetual", ExchangeSymbol: "SANDUSDT",
		BaseAsset: "SAND", QuoteAsset: "USDT",
	}
	execution := ArbitrageExecution{
		ID: "exec-hedge", CombinationID: combo.ID, Status: "hedging",
		Direction: "ask", PositionEffect: "open", HedgeOrderID: "hedge-110007",
		HedgeSequence: 5, LegBFilledQuantity: "246",
	}
	rejected := Order{
		ID: "hedge-110007", ClientOrderID: "old-hedge", Status: "rejected",
		FilledQuantity: "0", ErrorCode: "110007", Exchange: "bybit",
		ArbitrageRole: "hedge", ArbitrageLeg: "a", ArbitrageExecutionID: execution.ID,
		Side: "buy",
	}
	filled := Order{
		ID: "maker-binance", Status: "filled", FilledQuantity: "246", Side: "sell",
		ArbitrageRole: "maker", ArbitrageLeg: "b", ArbitrageExecutionID: execution.ID,
	}
	decoy := Order{
		ID: "decoy-110007", Status: "rejected", FilledQuantity: "0",
		ErrorCode: "110007", Exchange: "bybit", ArbitrageRole: "hedge",
		ArbitrageLeg: "a", ArbitrageExecutionID: "other-exec",
	}
	memory := newMemoryStore()
	for _, order := range []Order{rejected, filled, decoy} {
		memory.orders[order.ID] = order
	}
	store := &circuitOpenTestStore{
		executorFailStore: executorFailStore{
			combo: combo, execution: execution, activeExecution: &execution,
			orders: []Order{rejected, filled, decoy},
		},
		memory: memory,
	}
	bybit := &circuitOpenQueryAdapter{}
	binance := &circuitOpenQueryAdapter{}
	instrument := func(id int64, venue string) Instrument {
		item := circuitOpenTestInstrument(id, venue, "SANDUSDT")
		item.BaseAsset = "SAND"
		item.QuantityStep = "1"
		item.MinQuantity = "1"
		item.MinNotional = "1"
		item.PriceTick = "0.01"
		item.MaxQuantityStatus = exchange.ConstraintNotApplicable
		return item
	}
	catalog := silentRiskCatalog{101: instrument(101, "bybit"), 202: instrument(202, "binance")}
	registry := exchange.NewTestRegistry(map[string]exchange.Adapter{
		"bybit": bybit, "binance": binance,
	})
	service := NewService(store, catalog, nil, registry, time.Second, discardLogger())
	executor := NewArbitrageExecutor(
		store, store, service, catalog, silentRiskCredentials{}, registry,
		newNamedMarket(t, "bybit", "binance", "SANDUSDT", "SANDUSDT", "1", "1"),
		"", time.Millisecond, 0, 0, 0, time.Second, discardLogger(),
	)
	executor.ConfigurePortfolios(positionAuditPortfolios{snapshots: map[string]portfolio.Snapshot{
		"bybit": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "bybit", WireSymbol: "SANDUSDT",
			SignedContractSize: "27206", MarkPrice: "1",
		}}},
		"binance": {Positions: []portfolio.Position{{
			Kind: "cex", Exchange: "binance", WireSymbol: "SANDUSDT",
			SignedContractSize: "-27452", MarkPrice: "1",
		}}},
	}})
	if err := executor.ReconcileCircuitOpen(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.prepareCalls != 1 {
		t.Fatalf("prepareCalls=%d", store.prepareCalls)
	}
	got := store.lastPrepare
	if !got.AggregateCarry || got.FastPathAdmission {
		t.Fatalf("admission=%+v", got)
	}
	if got.ExpectedSequence != 5 || got.ExpectedCarryQuantity != "-246" {
		t.Fatalf("sequence=%d carry=%s", got.ExpectedSequence, got.ExpectedCarryQuantity)
	}
	if got.Order.Side != "buy" || got.Order.ArbitrageLeg != "a" ||
		!parseDecimal(got.Order.Quantity).Equal(decimal.NewFromInt(246)) {
		t.Fatalf("order=%+v", got.Order)
	}
	if got.Order.ClientOrderID == "" || got.Order.ClientOrderID == rejected.ClientOrderID {
		t.Fatalf("client id=%s", got.Order.ClientOrderID)
	}
	if got.Execution.ID != execution.ID {
		t.Fatalf("execution=%s", got.Execution.ID)
	}
	bybitPlace, _, bybitQuery := bybit.counts()
	binancePlace, _, _ := binance.counts()
	if bybitPlace != 1 || binancePlace != 0 || bybitQuery != 0 {
		t.Fatalf("bybit place/query=%d/%d binance place=%d", bybitPlace, bybitQuery, binancePlace)
	}
	if !store.combo.CircuitOpen {
		t.Fatal("circuit cleared before hedge completion")
	}

	store.prepareCalls = 0
	store.execution.HedgeOrderID = "maker-binance"
	store.activeExecution = &store.execution
	if err := executor.ReconcileCircuitOpen(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.prepareCalls != 0 {
		t.Fatalf("decoy 110007 prepared %d", store.prepareCalls)
	}
}

func TestClosedCombinationDoesNotEnterCircuitRecovery(t *testing.T) {
	combo := circuitOpenReadyCombo()
	combo.ID = "a8adae03-773e-446f-b9aa-eeccd6892358"
	combo.Status = "closed"
	combo.OneShotPhase = "exited"
	combo.CircuitOpen = false
	combo.LegABasePosition = "0"
	combo.LegBBasePosition = "0"
	adapter := &circuitOpenQueryAdapter{}
	store := &circuitOpenTestStore{executorFailStore: executorFailStore{combo: combo}}
	executor := newCircuitOpenExecutor(store, adapter, adapter, map[string]portfolio.Snapshot{})
	if err := executor.ReconcileCircuitOpen(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	place, _, query := adapter.counts()
	if place != 0 || query != 0 || store.prepareCalls != 0 || store.claimCircuitCalls != 0 {
		t.Fatalf("place=%d query=%d prepare=%d claim=%d", place, query, store.prepareCalls, store.claimCircuitCalls)
	}
	if store.combo.Status != "closed" || store.combo.OneShotPhase != "exited" {
		t.Fatalf("combo=%+v", store.combo)
	}
}

func TestCircuitRecoveryCloseRejectsStaleSnapshot(t *testing.T) {
	combo := circuitOpenReadyCombo()
	store := &circuitOpenTestStore{
		executorFailStore: executorFailStore{
			combo: combo,
			execution: ArbitrageExecution{
				ID: "exec-1", CombinationID: combo.ID, Status: "hedging",
			},
		},
	}
	updated, ok, err := store.CompleteCircuitOpenRecovery(context.Background(), circuitOpenRecoveryCloseRequest{
		CombinationID: combo.ID, ExecutionID: "exec-1", ExpectedVersion: combo.Version,
		Kind: circuitCloseFilled, LegAFilled: "1", LegBFilled: "1",
		ObservedAt: time.Now().UTC().Add(-time.Minute), RecoveryStartedAt: time.Now().UTC().Add(-2 * time.Minute),
	})
	if err != nil || ok || !updated.CircuitOpen || updated.RuntimeState != "manual_intervention" {
		t.Fatalf("ok=%v err=%v combo=%+v", ok, err, updated)
	}
	if store.execution.Status == "completed" {
		t.Fatalf("execution completed: %+v", store.execution)
	}
}

func newNamedMarket(
	t *testing.T, venueA, venueB, symbolA, symbolB, bid, ask string,
) *marketdata.Manager {
	t.Helper()
	keyA, err := marketdata.NewKey(venueA, "perpetual", symbolA)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := marketdata.NewKey(venueB, "perpetual", symbolB)
	if err != nil {
		t.Fatal(err)
	}
	connA := &schedulerConnection{reads: make(chan []byte, 1), done: make(chan struct{})}
	connB := &schedulerConnection{reads: make(chan []byte, 1), done: make(chan struct{})}
	parser := func(item marketdata.Key, _ []byte, received time.Time) (marketdata.BBO, bool, error) {
		return marketdata.BBO{
			Key: item, BidPrice: bid, AskPrice: ask,
			VenueTimestamp: received, ReceiveTimestamp: received,
		}, true, nil
	}
	market, err := marketdata.New(marketdata.Options{
		Connector: &schedulerConnector{connections: map[marketdata.Key]*schedulerConnection{
			keyA: connA, keyB: connB,
		}},
		Parsers:    map[string]marketdata.Parser{venueA: parser, venueB: parser},
		StaleAfter: time.Second, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = market.Close() })
	for _, key := range []marketdata.Key{keyA, keyB} {
		subscription, subErr := market.Subscribe(context.Background(), key)
		if subErr != nil {
			t.Fatal(subErr)
		}
		t.Cleanup(func() { subscription.Close() })
	}
	connA.reads <- []byte("bbo")
	connB.reads <- []byte("bbo")
	deadline := time.Now().Add(time.Second)
	for {
		_, errA := market.Latest(keyA)
		_, errB := market.Latest(keyB)
		if errA == nil && errB == nil {
			return market
		}
		if time.Now().After(deadline) {
			t.Fatal("market data was not published")
		}
		time.Sleep(time.Millisecond)
	}
}
