package trader

import (
	"context"
	"errors"
	"testing"
	"time"

	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

func TestBinanceMaker2011CancelFilledHedges(t *testing.T) {
	store, execution, maker, hedge := runBinanceMaker2011Cancel(
		t,
		exchange.Result{Status: "filled", VenueOrderID: "maker-1", FilledQuantity: "1", AveragePrice: "100"},
		nil,
	)
	if execution.Status != "completed" || store.combo.RuntimeState == "repricing" {
		t.Fatalf("execution=%+v combo=%+v", execution, store.combo)
	}
	if store.failures != 0 {
		t.Fatalf("failures=%d", store.failures)
	}
	for _, eventType := range store.eventTypes {
		if eventType == "venue_rejected" {
			t.Fatalf("events=%v", store.eventTypes)
		}
	}
	if maker.cancelCalls() < 1 || hedge.calls < 1 {
		t.Fatalf("cancels=%d hedgePlaces=%d", maker.cancelCalls(), hedge.calls)
	}
}

func TestBinanceMaker2011CancelZeroFillReprices(t *testing.T) {
	store, execution, maker, hedge := runBinanceMaker2011Cancel(
		t,
		exchange.Result{Status: "canceled", VenueOrderID: "maker-1", FilledQuantity: "0"},
		nil,
	)
	if execution.Status != "completed" || store.combo.RuntimeState != "repricing" {
		t.Fatalf("execution=%+v combo=%+v", execution, store.combo)
	}
	if store.failures != 0 || hedge.calls != 0 || maker.cancelCalls() < 1 {
		t.Fatalf("failures=%d hedgePlaces=%d cancels=%d", store.failures, hedge.calls, maker.cancelCalls())
	}
	hasRequote := false
	for _, eventType := range store.eventTypes {
		hasRequote = hasRequote || eventType == "maker_requote"
		if eventType == "venue_rejected" {
			t.Fatalf("events=%v", store.eventTypes)
		}
	}
	if !hasRequote {
		t.Fatalf("events=%v", store.eventTypes)
	}
}

func TestBinanceMaker2011CancelUncertainReconciles(t *testing.T) {
	store, execution, _, hedge := runBinanceMaker2011Cancel(
		t,
		exchange.Result{Status: "unknown", ErrorCode: "-2011"},
		exchange.ErrUncertain,
	)
	if execution.Status != "reconciling" || store.combo.RuntimeState != "reconciling" {
		t.Fatalf("execution=%+v combo=%+v", execution, store.combo)
	}
	if hedge.calls != 0 {
		t.Fatalf("hedgePlaces=%d", hedge.calls)
	}
}

func runBinanceMaker2011Cancel(
	t *testing.T,
	cancel exchange.Result,
	cancelErr error,
) (*executorFailStore, ArbitrageExecution, *countingCancelAdapter, *stubAdapter) {
	t.Helper()
	market := newBinance2011TestMarket(t)
	makerInstrument := Instrument{
		ID: 101, Exchange: "binance", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		ContractSize: "1", PriceTick: "0.1", QuantityStep: "1",
		MinQuantityStatus: exchange.ConstraintNotApplicable,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotionalStatus: exchange.ConstraintNotApplicable,
	}
	hedgeInstrument := Instrument{
		ID: 202, Exchange: "okx", ContractType: "perpetual",
		ExchangeSymbol: "BTC-USDT-SWAP", BaseAsset: "BTC", QuoteAsset: "USDT",
		ContractSize: "1", PriceTick: "0.1", QuantityStep: "1",
		MinQuantityStatus: exchange.ConstraintNotApplicable,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotionalStatus: exchange.ConstraintNotApplicable,
	}
	combination := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61748", OwnerUsername: "admin",
		Status: "running", RuntimeState: "monitoring",
		ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		OrderNotional: "100", CarryBaseQuantity: "0",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: makerInstrument.ID,
			ProductName: "ARBITRAGE", Exchange: "binance",
			ContractType: "perpetual", ExchangeSymbol: makerInstrument.ExchangeSymbol,
			BaseAsset: "BTC", QuoteAsset: "USDT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: hedgeInstrument.ID,
			ProductName: "ARBITRAGE", Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: hedgeInstrument.ExchangeSymbol,
			BaseAsset: "BTC", QuoteAsset: "USDT",
		},
	}
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	baseStore := &executorFailStore{combo: combination, intentStore: orderStore}
	store := &makerHedgeStore{executorFailStore: baseStore, orderStore: orderStore.memoryStore}
	maker := &countingCancelAdapter{
		place:    exchange.Result{Status: "open", VenueOrderID: "maker-1", FilledQuantity: "0"},
		result:   cancel,
		err:      cancelErr,
		allowGet: true,
		get:      exchange.Result{Status: "open", VenueOrderID: "maker-1", FilledQuantity: "0"},
	}
	hedge := &stubAdapter{place: exchange.Result{
		VenueOrderID: "hedge-1", Status: "filled",
		FilledQuantity: "1", AveragePrice: "102",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{
			makerInstrument.ID: makerInstrument, hedgeInstrument.ID: hedgeInstrument,
		},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"binance": maker, "okx": hedge,
		}),
		market, "", time.Millisecond, 1, 0, 1, time.Second, nil,
	)
	executor.streamAudit = time.Hour
	execution := ArbitrageExecution{
		ID: "binance-2011", CombinationID: combination.ID,
		Direction: "ask", RequestedNotional: "100",
		TriggerLegABid: "100", TriggerLegAAsk: "100.1",
		TriggerLegBBid: "100", TriggerLegBAsk: "100.1",
	}
	err := executor.executeMakerThenHedge(
		context.Background(), combination, &execution,
		marketdata.BBO{BidPrice: "100", AskPrice: "100.1"},
		marketdata.BBO{BidPrice: "100", AskPrice: "100.1"},
		nil,
	)
	if cancelErr != nil {
		if !errors.Is(err, ErrArbitrageReconciling) {
			t.Fatalf("err=%v", err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	return baseStore, execution, maker, hedge
}

func newBinance2011TestMarket(t *testing.T) *marketdata.Manager {
	t.Helper()
	keyA, err := marketdata.NewKey("binance", "perpetual", "BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := marketdata.NewKey("okx", "perpetual", "BTC-USDT-SWAP")
	if err != nil {
		t.Fatal(err)
	}
	connectionA := &schedulerConnection{reads: make(chan []byte, 1), done: make(chan struct{})}
	connectionB := &schedulerConnection{reads: make(chan []byte, 1), done: make(chan struct{})}
	parser := func(
		key marketdata.Key, _ []byte, received time.Time,
	) (marketdata.BBO, bool, error) {
		return marketdata.BBO{
			Key: key, BidPrice: "102", AskPrice: "102.1",
			VenueTimestamp: received, ReceiveTimestamp: received,
		}, true, nil
	}
	market, err := marketdata.New(marketdata.Options{
		Connector: &schedulerConnector{connections: map[marketdata.Key]*schedulerConnection{
			keyA: connectionA, keyB: connectionB,
		}},
		Parsers: map[string]marketdata.Parser{
			"binance": parser, "okx": parser,
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
