package trader

import (
	"context"
	"testing"
	"time"

	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

func TestAsterHedge5018KeepsExecutionRecovering(t *testing.T) {
	market := newAsterHedge5018Market(t)
	makerInstrument := Instrument{
		ID: 101, Exchange: "bybit", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		ContractSize: "1", PriceTick: "0.1", QuantityStep: "1",
		MinQuantityStatus: exchange.ConstraintNotApplicable,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotionalStatus: exchange.ConstraintNotApplicable,
	}
	hedgeInstrument := Instrument{
		ID: 202, Exchange: "aster", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		ContractSize: "1", PriceTick: "0.1", QuantityStep: "1",
		MinQuantityStatus: exchange.ConstraintNotApplicable,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotionalStatus: exchange.ConstraintNotApplicable,
	}
	combination := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61749", OwnerUsername: "admin",
		Status: "running", RuntimeState: "monitoring",
		ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		OrderNotional: "100", CarryBaseQuantity: "0",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: makerInstrument.ID,
			ProductName: "ARBITRAGE", Exchange: "bybit",
			ContractType: "perpetual", ExchangeSymbol: makerInstrument.ExchangeSymbol,
			BaseAsset: "BTC", QuoteAsset: "USDT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: hedgeInstrument.ID,
			ProductName: "ARBITRAGE", Exchange: "aster",
			ContractType: "perpetual", ExchangeSymbol: hedgeInstrument.ExchangeSymbol,
			BaseAsset: "BTC", QuoteAsset: "USDT",
		},
	}
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	baseStore := &executorFailStore{combo: combination, intentStore: orderStore}
	store := &makerHedgeStore{executorFailStore: baseStore, orderStore: orderStore.memoryStore}
	maker := &stubAdapter{place: exchange.Result{
		VenueOrderID: "maker-1", Status: "filled",
		FilledQuantity: "1", AveragePrice: "100",
	}}
	hedge := &countingCancelAdapter{
		place: exchange.Result{
			Status:         "rejected",
			ErrorCode:      "-5018",
			FilledQuantity: "0",
			ErrorMessage:   "ReduceOnly Order is rejected.",
		},
		placeErr: exchange.ErrRejected,
	}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{
			makerInstrument.ID: makerInstrument, hedgeInstrument.ID: hedgeInstrument,
		},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": maker, "aster": hedge,
		}),
		market, "", time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "aster-5018-hedge", CombinationID: combination.ID,
		Direction: "ask", RequestedNotional: "100",
		TriggerLegABid: "100", TriggerLegAAsk: "100.1",
		TriggerLegBBid: "100", TriggerLegBAsk: "100.1",
	}
	bbo := marketdata.BBO{BidPrice: "100", AskPrice: "100.1"}
	executor.Execute(context.Background(), combination, execution, bbo, bbo, nil)

	baseStore.mu.Lock()
	firstExecution := baseStore.execution
	firstCombo := baseStore.combo
	firstFailures := baseStore.failures
	firstSequence := firstExecution.HedgeSequence
	baseStore.mu.Unlock()
	if firstExecution.Status == "canceled" ||
		firstExecution.Status == "completed" ||
		firstExecution.Status == "failed" {
		t.Fatalf("execution=%+v", firstExecution)
	}
	if firstExecution.Status != "hedging" && firstExecution.Status != "reconciling" {
		t.Fatalf("execution=%+v", firstExecution)
	}
	if firstFailures < 1 || parseDecimal(firstCombo.CarryBaseQuantity).IsZero() {
		t.Fatalf("failures=%d combo=%+v", firstFailures, firstCombo)
	}
	if firstSequence < 1 {
		t.Fatalf("hedge sequence=%d", firstSequence)
	}
	assertRejectedHedgeOrders(t, orderStore, 1)
	if hedge.cancelCalls() != 0 || hedge.getCalls() != 0 {
		t.Fatalf("cancels=%d gets=%d places=%d", hedge.cancelCalls(), hedge.getCalls(), hedge.placeCalls())
	}

	executor.Execute(context.Background(), firstCombo, firstExecution, bbo, bbo, nil)

	baseStore.mu.Lock()
	secondExecution := baseStore.execution
	secondCombo := baseStore.combo
	secondSequence := secondExecution.HedgeSequence
	baseStore.mu.Unlock()
	if secondExecution.Status == "canceled" ||
		secondExecution.Status == "completed" ||
		secondExecution.Status == "failed" {
		t.Fatalf("second execution=%+v", secondExecution)
	}
	if parseDecimal(secondCombo.CarryBaseQuantity).IsZero() {
		t.Fatalf("carry cleared: combo=%+v", secondCombo)
	}
	if secondSequence <= firstSequence {
		t.Fatalf("hedge sequence first=%d second=%d", firstSequence, secondSequence)
	}
	assertRejectedHedgeOrders(t, orderStore, 2)
	if hedge.cancelCalls() != 0 {
		t.Fatalf("cancels=%d after retry", hedge.cancelCalls())
	}
}

func assertRejectedHedgeOrders(t *testing.T, orders *postOnlyOrderStore, want int) {
	t.Helper()
	orders.mu.Lock()
	defer orders.mu.Unlock()
	rejected := 0
	for _, order := range orders.orders {
		if order.ArbitrageRole != "hedge" && order.ArbitrageRole != "residual" {
			continue
		}
		if order.Status != "rejected" ||
			order.ErrorCode != "-5018" ||
			order.FilledQuantity != "0" {
			t.Fatalf("hedge order=%+v", order)
		}
		rejected++
	}
	if rejected != want {
		t.Fatalf("rejected hedge orders=%d want=%d", rejected, want)
	}
}

func newAsterHedge5018Market(t *testing.T) *marketdata.Manager {
	t.Helper()
	keyA, err := marketdata.NewKey("bybit", "perpetual", "BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := marketdata.NewKey("aster", "perpetual", "BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	connectionA := &schedulerConnection{reads: make(chan []byte, 1), done: make(chan struct{})}
	connectionB := &schedulerConnection{reads: make(chan []byte, 1), done: make(chan struct{})}
	parser := func(
		key marketdata.Key, _ []byte, received time.Time,
	) (marketdata.BBO, bool, error) {
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
			"bybit": parser, "aster": parser,
		},
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
