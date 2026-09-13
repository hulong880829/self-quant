package trader

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

func hedgeFastInstrument(id int64, venue, symbol string) Instrument {
	return Instrument{
		ID: id, Exchange: venue, ContractType: "perpetual",
		ExchangeSymbol: symbol, BaseAsset: "BTC", QuoteAsset: "USDT",
		ContractSize: "1", PriceTick: "0.1", QuantityStep: "1",
		MinQuantityStatus:  exchange.ConstraintNotApplicable,
		MaxQuantityStatus:  exchange.ConstraintNotApplicable,
		MinNotionalStatus:  exchange.ConstraintNotApplicable,
		MarketQuantityStep: "1", MarketQuantityStepStatus: exchange.ConstraintKnown,
		MarketMinQuantityStatus: exchange.ConstraintNotApplicable,
		MarketMaxQuantityStatus: exchange.ConstraintNotApplicable,
		MarketMinNotionalStatus: exchange.ConstraintNotApplicable,
	}
}

func hedgeFastCombo(maker, hedge Instrument) ArbitrageCombination {
	return ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61760", OwnerUsername: "admin",
		Status: "running", RuntimeState: "monitoring",
		ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		OrderNotional: "100", CarryBaseQuantity: "0",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: maker.ID,
			ProductName: "ARBITRAGE", Exchange: maker.Exchange,
			ContractType: "perpetual", ExchangeSymbol: maker.ExchangeSymbol,
			BaseAsset: "BTC", QuoteAsset: "USDT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: hedge.ID,
			ProductName: "ARBITRAGE", Exchange: hedge.Exchange,
			ContractType: "perpetual", ExchangeSymbol: hedge.ExchangeSymbol,
			BaseAsset: "BTC", QuoteAsset: "USDT",
		},
	}
}

func hedgeFastMarket(t *testing.T, maker, hedge Instrument) *marketdata.Manager {
	t.Helper()
	keyA, err := marketdata.NewKey(maker.Exchange, "perpetual", maker.ExchangeSymbol)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := marketdata.NewKey(hedge.Exchange, "perpetual", hedge.ExchangeSymbol)
	if err != nil {
		t.Fatal(err)
	}
	connectionA := &schedulerConnection{reads: make(chan []byte, 1), done: make(chan struct{})}
	connectionB := &schedulerConnection{reads: make(chan []byte, 1), done: make(chan struct{})}
	parser := func(key marketdata.Key, _ []byte, received time.Time) (marketdata.BBO, bool, error) {
		return marketdata.BBO{
			Key: key, BidPrice: "100", AskPrice: "100.1",
			VenueTimestamp: received, ReceiveTimestamp: received,
		}, true, nil
	}
	market, err := marketdata.New(marketdata.Options{
		Connector: &schedulerConnector{connections: map[marketdata.Key]*schedulerConnection{
			keyA: connectionA, keyB: connectionB,
		}},
		Parsers: map[string]marketdata.Parser{
			maker.Exchange: parser, hedge.Exchange: parser,
		},
		StaleAfter: time.Second, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { market.Close() })
	if _, err := market.Subscribe(context.Background(), keyA); err != nil {
		t.Fatal(err)
	}
	if _, err := market.Subscribe(context.Background(), keyB); err != nil {
		t.Fatal(err)
	}
	connectionA.reads <- []byte("bbo")
	connectionB.reads <- []byte("bbo")
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

type hedgeFastHarness struct {
	store     *makerHedgeStore
	base      *executorFailStore
	orders    *postOnlyOrderStore
	executor  *ArbitrageExecutor
	maker     *stubAdapter
	hedge     *stubAdapter
	combo     ArbitrageCombination
	execution ArbitrageExecution
}

func newHedgeFastHarness(t *testing.T, hedgeResults []exchange.Result, hedgeErrs []error) *hedgeFastHarness {
	t.Helper()
	makerInst := hedgeFastInstrument(101, "bybit", "BTCUSDT")
	hedgeInst := hedgeFastInstrument(202, "okx", "BTC-USDT-SWAP")
	combo := hedgeFastCombo(makerInst, hedgeInst)
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	base := &executorFailStore{combo: combo, intentStore: orderStore}
	store := &makerHedgeStore{executorFailStore: base, orderStore: orderStore.memoryStore}
	maker := &stubAdapter{place: exchange.Result{
		VenueOrderID: "maker-1", Status: "filled",
		FilledQuantity: "1", AveragePrice: "100",
	}}
	hedge := &stubAdapter{
		place: exchange.Result{
			VenueOrderID: "hedge-1", Status: "filled",
			FilledQuantity: "1", AveragePrice: "100",
		},
		placeResults: hedgeResults,
		placeErrors:  hedgeErrs,
	}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": maker, "okx": hedge,
		}),
		hedgeFastMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	return &hedgeFastHarness{
		store: store, base: base, orders: orderStore, executor: executor,
		maker: maker, hedge: hedge, combo: combo,
		execution: ArbitrageExecution{
			ID: "hedge-fast-exec", CombinationID: combo.ID,
			Direction: "ask", RequestedNotional: "100",
			TriggerLegABid: "100", TriggerLegAAsk: "100.1",
			TriggerLegBBid: "100", TriggerLegBAsk: "100.1",
		},
	}
}

func (h *hedgeFastHarness) run(t *testing.T) {
	t.Helper()
	bbo := marketdata.BBO{BidPrice: "100", AskPrice: "100.1"}
	h.executor.Execute(context.Background(), h.combo, h.execution, bbo, bbo, nil)
}

func hasEvent(types []string, want string) bool {
	for _, eventType := range types {
		if eventType == want {
			return true
		}
	}
	return false
}

func TestHedgeFastPathPreparesOnceWithoutRefreshBeforePlace(t *testing.T) {
	h := newHedgeFastHarness(t, nil, nil)
	h.run(t)
	h.base.mu.Lock()
	defer h.base.mu.Unlock()
	if h.base.execution.Status != "completed" || h.base.failures != 0 {
		t.Fatalf("execution=%+v failures=%d", h.base.execution, h.base.failures)
	}
	if h.base.prepareCalls != 1 || !h.base.lastPrepare.FastPathAdmission {
		t.Fatalf("prepareCalls=%d last=%+v", h.base.prepareCalls, h.base.lastPrepare)
	}
	if len(h.hedge.requests) != 1 || h.hedge.requests[0].TimeInForce != "IOC" ||
		h.hedge.requests[0].OrderType != "limit" {
		t.Fatalf("hedge requests=%+v", h.hedge.requests)
	}
	if !hasEvent(h.base.eventTypes, "maker_terminal") {
		t.Fatalf("events=%v", h.base.eventTypes)
	}
}

func TestHedgeFastPathPartialFillHedgesActualQuantity(t *testing.T) {
	h := newHedgeFastHarness(t, nil, nil)
	h.maker.place = exchange.Result{
		VenueOrderID: "maker-1", Status: "canceled",
		FilledQuantity: "1", AveragePrice: "100",
	}
	h.run(t)
	h.base.mu.Lock()
	defer h.base.mu.Unlock()
	if h.base.execution.LegAFilledQuantity != "1" {
		t.Fatalf("maker fill=%s", h.base.execution.LegAFilledQuantity)
	}
	if len(h.hedge.requests) != 1 || h.hedge.requests[0].Quantity != "1" {
		t.Fatalf("hedge requests=%+v", h.hedge.requests)
	}
}

func TestHedgeFastPathMemoryAdmissionFallsBackToOriginalCarry(t *testing.T) {
	makerInst := hedgeFastInstrument(101, "bybit", "BTCUSDT")
	hedgeInst := hedgeFastInstrument(202, "okx", "BTC-USDT-SWAP")
	combo := hedgeFastCombo(makerInst, hedgeInst)
	combo.CarryBaseQuantity = "1"
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	store := &executorFailStore{combo: combo, intentStore: orderStore}
	hedge := &stubAdapter{place: exchange.Result{
		VenueOrderID: "hedge-1", Status: "filled",
		FilledQuantity: "1", AveragePrice: "100",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"okx": hedge}),
		hedgeFastMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	executor.ConfigureHedgeEmergencyAfter(20 * time.Millisecond)
	execution := ArbitrageExecution{
		ID: "memory-miss", CombinationID: combo.ID, Direction: "ask",
		Status: "maker_open", LegAFilledQuantity: "1",
	}
	timing := hedgeTiming{
		dispatchStartedAt: time.Now(),
		emergencyAt:       time.Now().Add(20 * time.Millisecond),
	}
	_, err := executor.hedgeMakerCarryWithDeadline(
		context.Background(), &combo, &execution, combo.LegB, hedgeInst,
		Credentials{TradingAccountID: 2}, hedge, nil, "sell", timing,
	)
	if err != nil && !errors.Is(err, ErrRiskLimit) {
		t.Fatal(err)
	}
	if store.lastPrepare.FastPathAdmission {
		t.Fatal("memory miss must not use FastPathAdmission")
	}
	if len(hedge.requests) != 1 {
		t.Fatalf("requests=%+v", hedge.requests)
	}
}

func TestHedgeFastPathRemainingZeroDoesNotHedgeCarry(t *testing.T) {
	h := newHedgeFastHarness(t, nil, nil)
	execution := h.execution
	execution.MakerOrderID = "maker-already"
	execution.Status = "maker_open"
	execution.LegAFilledQuantity = "1"
	execution.LegBFilledQuantity = "1"
	hedged, err := h.executor.hedgeMakerCarryWithDeadline(
		context.Background(), &h.combo, &execution, h.combo.LegB,
		hedgeFastInstrument(202, "okx", "BTC-USDT-SWAP"),
		Credentials{TradingAccountID: 2}, h.hedge, nil, "sell", hedgeTiming{},
	)
	if err != nil || hedged {
		t.Fatalf("hedged=%v err=%v", hedged, err)
	}
	if len(h.hedge.requests) != 0 {
		t.Fatalf("requests=%+v", h.hedge.requests)
	}
}

func TestHedgeFastPathZeroMakerFillDoesNotOriginalCarry(t *testing.T) {
	makerInst := hedgeFastInstrument(101, "bybit", "BTCUSDT")
	hedgeInst := hedgeFastInstrument(202, "okx", "BTC-USDT-SWAP")
	combo := hedgeFastCombo(makerInst, hedgeInst)
	combo.CarryBaseQuantity = "1"
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	store := &executorFailStore{combo: combo, intentStore: orderStore}
	hedge := &stubAdapter{place: exchange.Result{
		VenueOrderID: "hedge-1", Status: "filled",
		FilledQuantity: "1", AveragePrice: "100",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"okx": hedge}),
		hedgeFastMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "zero-fill", CombinationID: combo.ID, Direction: "ask",
		Status: "maker_open", MakerOrderID: "maker-already",
	}
	hedged, err := executor.hedgeMakerCarryWithDeadline(
		context.Background(), &combo, &execution, combo.LegB, hedgeInst,
		Credentials{TradingAccountID: 2}, hedge, nil, "sell", hedgeTiming{},
	)
	if hedged || err != nil {
		t.Fatalf("hedged=%v err=%v", hedged, err)
	}
	if len(hedge.requests) != 0 {
		t.Fatalf("remaining 0 must not original carry: %+v", hedge.requests)
	}
}

func TestHedgeFastPathAdmissionConflictDoesNotFallback(t *testing.T) {
	h := newHedgeFastHarness(t, nil, nil)
	h.base.activeExecution = &ArbitrageExecution{ID: "other-active"}
	h.run(t)
	h.base.mu.Lock()
	defer h.base.mu.Unlock()
	if len(h.hedge.requests) != 0 {
		t.Fatalf("conflict must not place hedge: %+v", h.hedge.requests)
	}
	if h.base.execution.Status != "reconciling" {
		t.Fatalf("execution=%+v", h.base.execution)
	}
	if !hasEvent(h.base.eventTypes, "maker_terminal") {
		t.Fatalf("events=%v", h.base.eventTypes)
	}
}

func TestHedgeFastPathFillMismatchDoesNotInsertOrFallback(t *testing.T) {
	h := newHedgeFastHarness(t, nil, nil)
	h.base.fastPathMakerFill = "9"
	h.run(t)
	h.base.mu.Lock()
	defer h.base.mu.Unlock()
	if h.base.prepareCalls < 1 || h.base.lastPrepare.FastPathAdmission != true {
		t.Fatalf("prepareCalls=%d last=%+v", h.base.prepareCalls, h.base.lastPrepare)
	}
	if len(h.hedge.requests) != 0 {
		t.Fatalf("mismatch must not place: %+v", h.hedge.requests)
	}
	if h.base.execution.HedgeSequence != 0 {
		t.Fatalf("sequence=%d", h.base.execution.HedgeSequence)
	}
	if h.base.execution.Status != "reconciling" {
		t.Fatalf("execution=%+v", h.base.execution)
	}
}

func TestHedgeEmergencyIOCFillBeforeDeadlineSkipsMarket(t *testing.T) {
	h := newHedgeFastHarness(t, nil, nil)
	h.executor.ConfigureHedgeEmergencyAfter(time.Hour)
	h.run(t)
	if len(h.hedge.requests) != 1 || h.hedge.requests[0].OrderType != "limit" {
		t.Fatalf("requests=%+v", h.hedge.requests)
	}
	if hasEvent(h.base.eventTypes, "hedge_emergency_triggered") {
		t.Fatalf("events=%v", h.base.eventTypes)
	}
}

func TestHedgeEmergencyCanceledIOCPlacesOneMarket(t *testing.T) {
	h := newHedgeFastHarness(t, []exchange.Result{
		{Status: "canceled", FilledQuantity: "0"},
		{VenueOrderID: "mkt-1", Status: "filled", FilledQuantity: "1", AveragePrice: "100"},
	}, nil)
	h.executor.ConfigureHedgeEmergencyAfter(20 * time.Millisecond)
	h.run(t)
	if len(h.hedge.requests) != 2 {
		t.Fatalf("requests=%+v", h.hedge.requests)
	}
	if h.hedge.requests[0].OrderType != "limit" || h.hedge.requests[0].TimeInForce != "IOC" {
		t.Fatalf("first=%+v", h.hedge.requests[0])
	}
	if h.hedge.requests[1].OrderType != "market" || h.hedge.requests[1].TimeInForce != "" {
		t.Fatalf("emergency=%+v", h.hedge.requests[1])
	}
	if h.hedge.requests[1].Quantity != "1" {
		t.Fatalf("emergency qty=%s", h.hedge.requests[1].Quantity)
	}
	if !hasEvent(h.base.eventTypes, "hedge_emergency_triggered") ||
		!hasEvent(h.base.eventTypes, "hedge_emergency_completed") {
		t.Fatalf("events=%v", h.base.eventTypes)
	}
}

func TestHedgeEmergencyPartialIOCUsesRemaining(t *testing.T) {
	h := newHedgeFastHarness(t, []exchange.Result{
		{Status: "canceled", FilledQuantity: "0"},
		{VenueOrderID: "mkt-1", Status: "filled", FilledQuantity: "1", AveragePrice: "100"},
	}, nil)
	h.maker.place.FilledQuantity = "1"
	h.executor.ConfigureHedgeEmergencyAfter(20 * time.Millisecond)
	h.run(t)
	if len(h.hedge.requests) < 2 || h.hedge.requests[1].Quantity != "1" {
		t.Fatalf("requests=%+v", h.hedge.requests)
	}
}

func TestHedgeEmergencyUncertainIOCDoesNotPlaceMarket(t *testing.T) {
	h := newHedgeFastHarness(t, []exchange.Result{
		{Status: "unknown", FilledQuantity: "0"},
	}, []error{exchange.ErrUncertain})
	h.hedge.place = exchange.Result{Status: "unknown", FilledQuantity: "0"}
	h.hedge.err = exchange.ErrUncertain
	h.hedge.query = exchange.Result{Status: "unknown", FilledQuantity: "0"}
	h.hedge.queryErr = exchange.ErrUncertain
	h.executor.ConfigureHedgeEmergencyAfter(20 * time.Millisecond)
	h.run(t)
	market := 0
	for _, req := range h.hedge.requests {
		if req.OrderType == "market" {
			market++
		}
	}
	if market != 0 {
		t.Fatalf("requests=%+v", h.hedge.requests)
	}
	h.base.mu.Lock()
	defer h.base.mu.Unlock()
	if h.base.execution.Status != "reconciling" {
		t.Fatalf("execution=%+v", h.base.execution)
	}
	if !hasEvent(h.base.eventTypes, "maker_terminal") {
		t.Fatalf("events=%v", h.base.eventTypes)
	}
}

func TestHedgeEmergencyCreatedFalseDoesNotResubmit(t *testing.T) {
	h := newHedgeFastHarness(t, nil, nil)
	intent := h.executor.orderIntent(
		h.combo, h.execution, h.combo.LegB,
		hedgeFastInstrument(202, "okx", "BTC-USDT-SWAP"),
		"sell", "limit", "1", "100", "hedge", 0,
	)
	stored, created, err := h.orders.CreateIntent(context.Background(), intent)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	h.hedge.query = exchange.Result{
		VenueOrderID: "hedge-1", Status: "filled",
		FilledQuantity: "1", AveragePrice: "100",
	}
	h.run(t)
	if h.hedge.calls != 0 {
		t.Fatalf("PlaceOrder calls=%d requests=%+v", h.hedge.calls, h.hedge.requests)
	}
	if stored.ID == "" {
		t.Fatal("expected reused intent")
	}
}

func TestHedgeEmergencyCloseReduceOnlyOpenDoesNot(t *testing.T) {
	h := newHedgeFastHarness(t, []exchange.Result{
		{Status: "canceled", FilledQuantity: "0"},
		{VenueOrderID: "mkt-1", Status: "filled", FilledQuantity: "1", AveragePrice: "100"},
	}, nil)
	h.execution.PositionEffect = "close"
	h.execution.ReduceOnly = true
	h.combo.Status = "closing"
	h.combo.RuntimeState = "closing"
	h.combo.LegABasePosition = "-1"
	h.combo.LegBBasePosition = "1"
	h.base.combo = h.combo
	h.orders.mu.Lock()
	h.orders.orders["owned-a"] = Order{
		ID: "owned-a", ArbitrageLeg: "a", Side: "sell",
		Status: "filled", FilledQuantity: "1",
	}
	h.orders.orders["owned-b"] = Order{
		ID: "owned-b", ArbitrageLeg: "b", Side: "buy",
		Status: "filled", FilledQuantity: "1",
	}
	h.orders.mu.Unlock()
	h.executor.ConfigureHedgeEmergencyAfter(20 * time.Millisecond)
	h.run(t)
	if len(h.hedge.requests) < 2 || !h.hedge.requests[1].ReduceOnly {
		t.Fatalf("close emergency=%+v", h.hedge.requests)
	}

	open := newHedgeFastHarness(t, []exchange.Result{
		{Status: "canceled", FilledQuantity: "0"},
		{VenueOrderID: "mkt-1", Status: "filled", FilledQuantity: "1", AveragePrice: "100"},
	}, nil)
	open.executor.ConfigureHedgeEmergencyAfter(20 * time.Millisecond)
	open.run(t)
	if len(open.hedge.requests) < 2 || open.hedge.requests[1].ReduceOnly {
		t.Fatalf("open emergency=%+v", open.hedge.requests)
	}
}

func TestHedgeEmergencyLockWaitStillSubmitsPersistedIOC(t *testing.T) {
	h := newHedgeFastHarness(t, []exchange.Result{
		{Status: "canceled", FilledQuantity: "0"},
		{VenueOrderID: "mkt-1", Status: "filled", FilledQuantity: "1", AveragePrice: "100"},
	}, nil)
	h.executor.ConfigureHedgeEmergencyAfter(20 * time.Millisecond)
	lock := h.executor.service.accountLock(2)
	lock.Lock()
	go func() {
		time.Sleep(40 * time.Millisecond)
		lock.Unlock()
	}()
	h.run(t)
	if len(h.hedge.requests) != 2 {
		t.Fatalf("requests=%+v", h.hedge.requests)
	}
	if h.hedge.requests[0].OrderType != "limit" || h.hedge.requests[0].TimeInForce != "IOC" {
		t.Fatalf("lock wait must still submit IOC: %+v", h.hedge.requests[0])
	}
	if h.hedge.requests[1].OrderType != "market" {
		t.Fatalf("emergency after IOC: %+v", h.hedge.requests)
	}
}

func TestHedgeEmergencyRejectedMarketDoesNotFabricateFill(t *testing.T) {
	h := newHedgeFastHarness(t, []exchange.Result{
		{Status: "canceled", FilledQuantity: "0"},
		{Status: "rejected", ErrorCode: "x", FilledQuantity: "0"},
	}, []error{nil, exchange.ErrRejected})
	h.executor.ConfigureHedgeEmergencyAfter(20 * time.Millisecond)
	h.run(t)
	h.base.mu.Lock()
	defer h.base.mu.Unlock()
	if parseDecimal(h.base.execution.LegBFilledQuantity).IsPositive() &&
		h.base.execution.Status == "completed" {
		t.Fatalf("rejected market must not complete as filled: %+v", h.base.execution)
	}
}

func TestRecoverWithoutMarketDataHasNoEmergency(t *testing.T) {
	h := newHedgeFastHarness(t, nil, nil)
	h.executor.ConfigureHedgeEmergencyAfter(20 * time.Millisecond)
	execution := h.execution
	execution.Status = "reconciling"
	if err := h.executor.RecoverWithoutMarketData(context.Background(), h.combo, execution); err != nil &&
		!errors.Is(err, ErrNotFound) {
		// Recover may no-op without orders; must not place emergency market.
	}
	for _, req := range h.hedge.requests {
		if req.OrderType == "market" {
			t.Fatalf("recover must not emergency: %+v", h.hedge.requests)
		}
	}
}

func TestMakerHedgeRemainingUsesExecutionFills(t *testing.T) {
	combo := ArbitrageCombination{MakerLeg: "a"}
	execution := ArbitrageExecution{LegAFilledQuantity: "2", LegBFilledQuantity: "0.5"}
	maker, hedge, remaining := makerHedgeRemaining(combo, execution)
	if !maker.Equal(decimal.RequireFromString("2")) ||
		!hedge.Equal(decimal.RequireFromString("0.5")) ||
		!remaining.Equal(decimal.RequireFromString("1.5")) {
		t.Fatalf("maker=%s hedge=%s remaining=%s", maker, hedge, remaining)
	}
	combo.MakerLeg = "b"
	maker, hedge, remaining = makerHedgeRemaining(combo, execution)
	if !remaining.Equal(decimal.RequireFromString("0")) {
		t.Fatalf("b-maker remaining=%s", remaining)
	}
}

func TestHedgeCreatedReusedConcurrentAtMostOnePlace(t *testing.T) {
	h := newHedgeFastHarness(t, nil, nil)
	instrument := hedgeFastInstrument(202, "okx", "BTC-USDT-SWAP")
	execution := h.execution
	execution.Status = "maker_open"
	execution.MakerOrderID = "maker-1"
	intent := h.executor.orderIntent(
		h.combo, execution, h.combo.LegB, instrument,
		"sell", "limit", "1", "100", "hedge", 0,
	)
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.base.PrepareArbitrageHedgeIntent(context.Background(), PrepareArbitrageHedgeIntentInput{
				Combination: h.combo, Execution: execution, Order: intent,
				ExpectedSequence: 0, HedgeSide: "sell", TargetQuantity: "1",
			})
			if err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	created := 0
	h.orders.mu.Lock()
	for _, order := range h.orders.orders {
		if order.ArbitrageRole == "hedge" {
			created++
		}
	}
	h.orders.mu.Unlock()
	if created != 1 {
		t.Fatalf("hedge orders=%d", created)
	}
}

func TestHedgeAdmissionConflictHelper(t *testing.T) {
	if !hedgeAdmissionConflict(ErrArbitrageHedgeAdmissionConflict) ||
		!hedgeAdmissionConflict(ErrArbitrageHedgeSequence) ||
		hedgeAdmissionConflict(ErrMarketDataStale) {
		t.Fatal("conflict classification")
	}
}

func TestConfigureHedgeEmergencyAfterZeroDisables(t *testing.T) {
	executor := &ArbitrageExecutor{}
	executor.ConfigureHedgeEmergencyAfter(2 * time.Second)
	if executor.hedgeEmergencyAfter != 2*time.Second {
		t.Fatalf("got %s", executor.hedgeEmergencyAfter)
	}
	executor.ConfigureHedgeEmergencyAfter(0)
	if executor.hedgeEmergencyAfter != 0 {
		t.Fatal("zero must disable")
	}
}

func TestHedgeEmergencyUncertainMarketForbidsSecond(t *testing.T) {
	h := newHedgeFastHarness(t, []exchange.Result{
		{Status: "canceled", FilledQuantity: "0"},
		{Status: "unknown", FilledQuantity: "0"},
	}, []error{nil, exchange.ErrUncertain})
	h.hedge.place = exchange.Result{Status: "canceled", FilledQuantity: "0"}
	h.hedge.query = exchange.Result{Status: "unknown", FilledQuantity: "0"}
	h.hedge.queryErr = exchange.ErrUncertain
	h.executor.ConfigureHedgeEmergencyAfter(20 * time.Millisecond)
	h.run(t)
	market := 0
	for _, req := range h.hedge.requests {
		if req.OrderType == "market" {
			market++
		}
	}
	if market > 1 {
		t.Fatalf("second market: %+v", h.hedge.requests)
	}
}

func containsMarket(requests []exchange.OrderRequest) bool {
	for _, req := range requests {
		if req.OrderType == "market" {
			return true
		}
	}
	return false
}

func TestHedgeEmergencyRateLimitDoesNotPlaceMarket(t *testing.T) {
	h := newHedgeFastHarness(t, []exchange.Result{
		{Status: "unknown", FilledQuantity: "0"},
	}, []error{exchange.ErrRateLimited})
	h.hedge.place = exchange.Result{Status: "unknown", FilledQuantity: "0"}
	h.hedge.err = exchange.ErrRateLimited
	h.executor.ConfigureHedgeEmergencyAfter(20 * time.Millisecond)
	h.run(t)
	if containsMarket(h.hedge.requests) {
		t.Fatalf("rate limit must not emergency: %+v", h.hedge.requests)
	}
}

func TestHedgeCarryDoesNotPlaceEmergencyMarket(t *testing.T) {
	makerInst := hedgeFastInstrument(101, "bybit", "BTCUSDT")
	hedgeInst := hedgeFastInstrument(202, "okx", "BTC-USDT-SWAP")
	combo := hedgeFastCombo(makerInst, hedgeInst)
	combo.CarryBaseQuantity = "1"
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	store := &executorFailStore{combo: combo, intentStore: orderStore}
	hedge := &stubAdapter{place: exchange.Result{
		Status: "canceled", FilledQuantity: "0",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"okx": hedge}),
		hedgeFastMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	executor.ConfigureHedgeEmergencyAfter(20 * time.Millisecond)
	execution := ArbitrageExecution{
		ID: "carry-no-emergency", CombinationID: combo.ID, Direction: "ask",
		Status: "hedging",
	}
	_, err := executor.hedgeCarry(
		context.Background(), &combo, &execution, combo.LegB, hedgeInst,
		Credentials{TradingAccountID: 2}, hedge, nil, time.Now(),
	)
	if err != nil && !errors.Is(err, ErrRiskLimit) {
		t.Fatal(err)
	}
	if containsMarket(hedge.requests) {
		t.Fatalf("hedgeCarry must not emergency: %+v", hedge.requests)
	}
}

func TestHedgeEmergencySkippedWhenDisabled(t *testing.T) {
	h := newHedgeFastHarness(t, []exchange.Result{
		{Status: "canceled", FilledQuantity: "0"},
		{Status: "canceled", FilledQuantity: "0"},
		{Status: "canceled", FilledQuantity: "0"},
	}, nil)
	h.run(t)
	if containsMarket(h.hedge.requests) {
		t.Fatalf("disabled emergency placed market: %+v", h.hedge.requests)
	}
}

func TestHedgeCarryReplacesTerminalRejectedHedge(t *testing.T) {
	makerInst := hedgeFastInstrument(101, "bybit", "BTCUSDT")
	hedgeInst := hedgeFastInstrument(202, "okx", "BTC-USDT-SWAP")
	combo := hedgeFastCombo(makerInst, hedgeInst)
	combo.CarryBaseQuantity = "1"
	combo.CircuitOpen = true
	combo.RuntimeState = "reconciling"
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	old, _, err := orderStore.CreateIntent(context.Background(), Order{
		IdempotencyKey: "arb:carry-replace:b:hedge:2", OwnerUsername: "admin",
		TradingAccountID: 2, Exchange: "okx", InstrumentID: hedgeInst.ID,
		Side: "sell", OrderType: "limit", Quantity: "1", Price: "100",
		ArbitrageExecutionID: "carry-replace", ArbitrageLeg: "b", ArbitrageRole: "hedge",
	})
	if err != nil {
		t.Fatal(err)
	}
	old, err = orderStore.UpdateResult(context.Background(), old.ID, VenueResult{
		Status: "rejected", FilledQuantity: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := &executorFailStore{
		combo: combo, intentStore: orderStore,
		execution: ArbitrageExecution{
			ID: "carry-replace", CombinationID: combo.ID, Direction: "ask",
			Status: "reconciling", HedgeOrderID: old.ID, HedgeSequence: 3,
		},
	}
	store := &scriptedCarryStore{
		executorFailStore: base,
		carries:           []string{"1", "0"},
	}
	hedge := &stubAdapter{place: exchange.Result{
		VenueOrderID: "hedge-2", Status: "filled",
		FilledQuantity: "1", AveragePrice: "100",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"okx": hedge}),
		hedgeFastMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	execution := base.execution
	_, err = executor.hedgeCarry(
		context.Background(), &combo, &execution, combo.LegB, hedgeInst,
		Credentials{TradingAccountID: 2}, hedge, nil, time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if base.prepareCalls != 1 || !base.lastPrepare.AggregateCarry {
		t.Fatalf("prepareCalls=%d last=%+v", base.prepareCalls, base.lastPrepare)
	}
	if base.execution.HedgeSequence != 4 || base.execution.HedgeOrderID == old.ID {
		t.Fatalf("execution=%+v", base.execution)
	}
	if !base.combo.CircuitOpen {
		t.Fatalf("circuit_open cleared: %+v", base.combo)
	}
	if len(hedge.requests) != 1 {
		t.Fatalf("requests=%+v", hedge.requests)
	}
	kept, err := orderStore.GetByOwner(context.Background(), "admin", old.ID)
	if err != nil || kept.Status != "rejected" {
		t.Fatalf("old hedge mutated: %+v err=%v", kept, err)
	}
}

func TestHedgeCarryOpenHedgeDoesNotPrepareSecond(t *testing.T) {
	makerInst := hedgeFastInstrument(101, "bybit", "BTCUSDT")
	hedgeInst := hedgeFastInstrument(202, "okx", "BTC-USDT-SWAP")
	combo := hedgeFastCombo(makerInst, hedgeInst)
	combo.CarryBaseQuantity = "1"
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	old, _, err := orderStore.CreateIntent(context.Background(), Order{
		IdempotencyKey: "arb:carry-open:b:hedge:2", OwnerUsername: "admin",
		TradingAccountID: 2, Exchange: "okx", InstrumentID: hedgeInst.ID,
		Side: "sell", OrderType: "limit", Quantity: "1", Price: "100",
		ArbitrageExecutionID: "carry-open", ArbitrageLeg: "b", ArbitrageRole: "hedge",
	})
	if err != nil {
		t.Fatal(err)
	}
	old, err = orderStore.UpdateResult(context.Background(), old.ID, VenueResult{
		Status: "open", FilledQuantity: "0", VenueOrderID: "open-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &executorFailStore{
		combo: combo, intentStore: orderStore,
		execution: ArbitrageExecution{
			ID: "carry-open", CombinationID: combo.ID, Direction: "ask",
			Status: "hedging", HedgeOrderID: old.ID, HedgeSequence: 3,
		},
	}
	hedge := &stubAdapter{query: exchange.Result{Status: "open", FilledQuantity: "0"}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"okx": hedge}),
		hedgeFastMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, 20*time.Millisecond, nil,
	)
	execution := store.execution
	_, err = executor.hedgeCarry(
		context.Background(), &combo, &execution, combo.LegB, hedgeInst,
		Credentials{TradingAccountID: 2}, hedge, nil, time.Now(),
	)
	if !errors.Is(err, ErrVenueUncertain) {
		t.Fatalf("err=%v", err)
	}
	if store.prepareCalls != 0 {
		t.Fatalf("prepareCalls=%d", store.prepareCalls)
	}
	if len(hedge.requests) != 0 {
		t.Fatalf("placed new hedge: %+v", hedge.requests)
	}
}
