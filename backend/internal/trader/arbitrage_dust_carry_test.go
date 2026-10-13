package trader

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

type scriptedCarryStore struct {
	*executorFailStore
	carries []string
}

func (s *scriptedCarryStore) RecomputeArbitrageBasePositions(
	_ context.Context, _ string,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recomputeCalls++
	if len(s.carries) > 0 {
		idx := s.recomputeCalls - 1
		if idx >= len(s.carries) {
			idx = len(s.carries) - 1
		}
		s.combo.CarryBaseQuantity = s.carries[idx]
	}
	return s.combo, nil
}

func (s *scriptedCarryStore) RecomputeArbitrageBasePositionsForExecution(
	ctx context.Context, id string,
) (ArbitrageCombination, error) {
	combo, err := s.RecomputeArbitrageBasePositions(ctx, id)
	if err != nil {
		return combo, err
	}
	s.syncPairedExecutionFills(ctx, id)
	return combo, nil
}

func dustCarryInstrument(id int64, venue, symbol, minNotional string) Instrument {
	return Instrument{
		ID: id, Exchange: venue, ContractType: "perpetual",
		ExchangeSymbol: symbol, BaseAsset: "BTC", QuoteAsset: "USDT",
		ContractSize: "1", PriceTick: "0.1", QuantityStep: "1",
		MinQuantity: "1", MinNotional: minNotional,
		MinQuantityStatus:  exchange.ConstraintKnown,
		MaxQuantityStatus:  exchange.ConstraintNotApplicable,
		MinNotionalStatus:  exchange.ConstraintKnown,
		MarketQuantityStep: "1", MarketMinQuantity: "1",
		MarketMinNotional:        minNotional,
		MarketQuantityStepStatus: exchange.ConstraintKnown,
		MarketMinQuantityStatus:  exchange.ConstraintKnown,
		MarketMaxQuantityStatus:  exchange.ConstraintNotApplicable,
		MarketMinNotionalStatus:  exchange.ConstraintKnown,
	}
}

func dustCarryCombo(
	maker, hedge Instrument, direction, runtime, carry string,
) ArbitrageCombination {
	legA, legB := "200", "-161"
	if strings.EqualFold(direction, "bid") {
		legA, legB = "161", "-200"
	}
	return ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61771", OwnerUsername: "admin",
		Status: "running", RuntimeState: runtime,
		ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		OrderNotional: "90", CarryBaseQuantity: carry,
		LegABasePosition: legA, LegBBasePosition: legB,
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

func dustCarryMarket(t *testing.T, maker, hedge Instrument) *marketdata.Manager {
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
			Key: key, BidPrice: "1", AskPrice: "1",
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

func dustEventReason(store *executorFailStore) string {
	for _, payload := range store.eventPayloads {
		reason, _ := payload["reason"].(string)
		if reason != "" {
			return reason
		}
	}
	return ""
}

func TestAllowBelowMinNotional(t *testing.T) {
	perp := dustCarryInstrument(1, "bybit", "BTCUSDT", "50")
	spot := perp
	spot.ContractType = "spot"
	lastClip := ArbitrageExecution{
		PositionEffect: "close", ReduceOnly: true, LastCloseClip: true,
	}
	if !allowBelowMinNotional(lastClip, perp) {
		t.Fatal("lastClip perp close should skip minNotional")
	}
	if allowBelowMinNotional(lastClip, spot) {
		t.Fatal("spot must not skip minNotional")
	}
	if allowBelowMinNotional(
		ArbitrageExecution{PositionEffect: "close", ReduceOnly: true}, perp,
	) {
		t.Fatal("ordinary close must not skip minNotional")
	}
	closing := ArbitrageCombination{Status: "closing"}
	running := ArbitrageCombination{Status: "running"}
	exiting := ArbitrageCombination{
		Status: "running", RunMode: "one_shot", OneShotPhase: "exiting",
	}
	waiting := ArbitrageCombination{
		Status: "running", RunMode: "one_shot", OneShotPhase: "waiting_exit",
	}
	if !allowBelowMinNotionalCombo(closing, lastClip, perp) {
		t.Fatal("closing lastClip should skip")
	}
	if !allowBelowMinNotionalCombo(exiting, lastClip, perp) {
		t.Fatal("one-shot exiting lastClip should skip")
	}
	if allowBelowMinNotionalCombo(running, lastClip, perp) {
		t.Fatal("running lastClip must not skip")
	}
	if allowBelowMinNotionalCombo(waiting, lastClip, perp) {
		t.Fatal("waiting_exit lastClip must not skip")
	}
	if allowBelowMinNotionalCombo(exiting, ArbitrageExecution{
		PositionEffect: "close", ReduceOnly: true,
	}, perp) {
		t.Fatal("exiting without lastClip must not skip")
	}
	if allowBelowMinNotionalCombo(exiting, ArbitrageExecution{
		PositionEffect: "close", LastCloseClip: true,
	}, perp) {
		t.Fatal("exiting without reduceOnly must not skip")
	}
	if allowBelowMinNotionalCombo(exiting, ArbitrageExecution{
		PositionEffect: "open", ReduceOnly: true, LastCloseClip: true,
	}, perp) {
		t.Fatal("open lastClip must not skip")
	}
	if allowBelowMinNotionalCombo(exiting, lastClip, spot) {
		t.Fatal("spot lastClip must not skip")
	}
	qty, ok, err := executableHedgeQuantity(
		decimal.NewFromInt(10), decimal.RequireFromString("0.2435"), perp, true,
	)
	if err != nil || !ok || !qty.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("skip minNotional qty=%s ok=%v err=%v", qty, ok, err)
	}
	perp.MinQuantity = "50"
	_, ok, err = executableHedgeQuantity(
		decimal.NewFromInt(10), decimal.RequireFromString("0.2435"), perp, true,
	)
	if err != nil || ok {
		t.Fatalf("minQty must still block skip minNotional ok=%v err=%v", ok, err)
	}
}

func TestExecutableHedgeQuantityMinNotionalStatus(t *testing.T) {
	instrument := dustCarryInstrument(1, "okx", "BTC-USDT-SWAP", "50")
	target := decimal.NewFromInt(20)
	price := decimal.NewFromInt(1)
	instrument.MinNotionalStatus = exchange.ConstraintUnknown
	if _, _, err := executableHedgeQuantity(target, price, instrument, false); !errors.Is(
		err, ErrInstrumentUnavailable,
	) {
		t.Fatal("unknown minNotional must stay unavailable")
	}
	instrument.MinNotionalStatus = exchange.ConstraintNotApplicable
	qty, ok, err := executableHedgeQuantity(target, price, instrument, false)
	if err != nil || !ok || !qty.Equal(target) {
		t.Fatalf("notApplicable qty=%s ok=%v err=%v", qty, ok, err)
	}
	instrument.MinNotionalStatus = exchange.ConstraintKnown
	qty, ok, err = executableHedgeQuantity(target, price, instrument, false)
	if err != nil || ok {
		t.Fatalf("below known minNotional qty=%s ok=%v err=%v", qty, ok, err)
	}
	qty, ok, err = executableHedgeQuantity(target, price, instrument, true)
	if err != nil || !ok || !qty.Equal(target) {
		t.Fatalf("skip minNotional qty=%s ok=%v err=%v", qty, ok, err)
	}
}

func TestRunningCloseBelowMinNotionalDefersDust(t *testing.T) {
	instrument := dustCarryInstrument(202, "okx", "BTC-USDT-SWAP", "50")
	combo := dustCarryCombo(
		dustCarryInstrument(101, "bybit", "BTCUSDT", "50"), instrument,
		"bid", "monitoring", "-20",
	)
	combo.LegB = ArbitrageLeg{
		TradingAccountID: 2, InstrumentID: 202,
		Exchange: "okx", ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
	}
	store := &executorFailStore{combo: combo}
	market := dustCarryMarket(
		t, dustCarryInstrument(101, "bybit", "BTCUSDT", "50"), instrument,
	)
	executor := NewArbitrageExecutor(
		store, nil, nil, nil, nil, nil, market, "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "running-close-dust", CombinationID: combo.ID,
		Direction: "bid", PositionEffect: "close", ReduceOnly: true,
	}
	hedged, err := executor.hedgeCarry(
		context.Background(), &combo, &execution, combo.LegB, instrument,
		Credentials{}, unusedVenueAdapter{}, nil, time.Time{},
	)
	if hedged || err != nil || store.combo.RuntimeState != "hedge_deferred_dust" {
		t.Fatalf("hedged=%v err=%v combo=%+v", hedged, err, store.combo)
	}
	if dustEventReason(store) != "below_min_notional" {
		t.Fatalf("events=%v payloads=%v", store.eventTypes, store.eventPayloads)
	}
}

func TestHedgeQuantityDustReasonClassification(t *testing.T) {
	step := dustCarryInstrument(1, "okx", "BTC-USDT-SWAP", "50")
	if got := hedgeQuantityDustReason(
		decimal.RequireFromString("0.4"), decimal.NewFromInt(1), step,
	); got != dustReasonStepRoundsToZero {
		t.Fatalf("step reason=%s", got)
	}
	minQty := dustCarryInstrument(1, "okx", "BTC-USDT-SWAP", "50")
	minQty.MinQuantity = "10"
	if got := hedgeQuantityDustReason(
		decimal.NewFromInt(5), decimal.NewFromInt(10), minQty,
	); got != dustReasonBelowMinQuantity {
		t.Fatalf("min qty reason=%s", got)
	}
	if got := hedgeQuantityDustReason(
		decimal.NewFromInt(39), decimal.NewFromInt(1), step,
	); got != dustReasonBelowMinNotional {
		t.Fatalf("min notional reason=%s", got)
	}
	unknown := step
	unknown.MinNotionalStatus = exchange.ConstraintUnknown
	if got := hedgeQuantityDustReason(
		decimal.NewFromInt(20), decimal.NewFromInt(1), unknown,
	); got != "" {
		t.Fatalf("unknown reason=%s", got)
	}
}

func TestEmergencyHedgeOrdinaryBelowMinNotionalDoesNotPlace(t *testing.T) {
	maker := dustCarryInstrument(101, "bybit", "BTCUSDT", "50")
	hedge := dustCarryInstrument(202, "okx", "BTC-USDT-SWAP", "50")
	combo := dustCarryCombo(maker, hedge, "ask", "hedging", "20")
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	store := &executorFailStore{combo: combo, intentStore: orderStore}
	adapter := &stubAdapter{}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{maker.ID: maker, hedge.ID: hedge},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"okx": adapter}),
		dustCarryMarket(t, maker, hedge), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "emergency-min-notional", CombinationID: combo.ID, Direction: "ask",
	}
	filled, err := executor.submitEmergencyHedge(
		context.Background(), combo, &execution, combo.LegB, hedge,
		Credentials{TradingAccountID: 2}, adapter, nil, "sell",
		decimal.NewFromInt(20), Order{}, time.Now(), "deadline",
	)
	if err != nil || !filled.IsZero() || len(adapter.requests) != 0 {
		t.Fatalf("filled=%s err=%v requests=%+v", filled, err, adapter.requests)
	}
	execution.LastCloseClip = true
	execution.PositionEffect = "close"
	execution.ReduceOnly = true
	combo.Status = "closing"
	adapter.place = exchange.Result{
		VenueOrderID: "em-1", Status: "filled", FilledQuantity: "20", AveragePrice: "1",
	}
	filled, err = executor.submitEmergencyHedge(
		context.Background(), combo, &execution, combo.LegB, hedge,
		Credentials{TradingAccountID: 2}, adapter, nil, "sell",
		decimal.NewFromInt(20), Order{}, time.Now(), "deadline",
	)
	if err != nil || !filled.Equal(decimal.NewFromInt(20)) || len(adapter.requests) != 1 {
		t.Fatalf("lastClip filled=%s err=%v requests=%+v", filled, err, adapter.requests)
	}
}

func TestDustOpenAggregatesCarryIntoHedge(t *testing.T) {
	for _, runtime := range []string{"hedge_deferred_dust", "monitoring", "repricing"} {
		t.Run(runtime, func(t *testing.T) {
			testDustAggregatesCarry(
				t, "ask", "open", false, "39", "129", "129", "0", runtime, "monitoring",
			)
		})
	}
}

func TestDustCloseAggregatesCarryIntoHedge(t *testing.T) {
	testDustAggregatesCarry(
		t, "bid", "close", true, "-39", "-129", "129", "0", "monitoring", "monitoring",
	)
}

func TestDustPartialHedgeKeepsComboCarry(t *testing.T) {
	testDustAggregatesCarry(
		t, "bid", "close", true, "-39", "-129", "100", "-29", "hedge_deferred_dust", "hedge_deferred_dust",
	)
}

func testDustAggregatesCarry(
	t *testing.T,
	direction, effect string,
	reduceOnly bool,
	startCarry, aggregatedCarry, hedgeFill, finalCarry, runtime, wantState string,
) {
	t.Helper()
	makerInst := dustCarryInstrument(101, "bybit", "BTCUSDT", "50")
	hedgeInst := dustCarryInstrument(202, "okx", "BTC-USDT-SWAP", "50")
	combo := dustCarryCombo(makerInst, hedgeInst, direction, runtime, startCarry)
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	base := &executorFailStore{combo: combo, intentStore: orderStore}
	store := &scriptedCarryStore{
		executorFailStore: base,
		carries:           []string{startCarry, aggregatedCarry, finalCarry, finalCarry},
	}
	maker := &stubAdapter{place: exchange.Result{
		VenueOrderID: "maker-1", Status: "filled",
		FilledQuantity: "90", AveragePrice: "1",
	}}
	hedge := &stubAdapter{place: exchange.Result{
		VenueOrderID: "hedge-1", Status: "filled",
		FilledQuantity: hedgeFill, AveragePrice: "1",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": maker, "okx": hedge,
		}),
		dustCarryMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "dust-agg", CombinationID: combo.ID, Direction: direction,
		PositionEffect: effect, ReduceOnly: reduceOnly,
		TargetBaseQuantity: "90", RequestedNotional: "90",
		TriggerLegABid: "1", TriggerLegAAsk: "1",
		TriggerLegBBid: "1", TriggerLegBAsk: "1",
	}
	bbo := marketdata.BBO{BidPrice: "1", AskPrice: "1"}
	err := executor.executeMakerThenHedge(
		context.Background(), combo, &execution, bbo, bbo, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(maker.requests) != 1 || maker.requests[0].Quantity != "90" {
		t.Fatalf("maker requests=%+v", maker.requests)
	}
	if len(hedge.requests) != 1 || hedge.requests[0].Quantity != parseDecimal(aggregatedCarry).Abs().String() {
		t.Fatalf("hedge requests=%+v", hedge.requests)
	}
	if !base.lastPrepare.AggregateCarry ||
		!parseDecimal(base.lastPrepare.ExpectedCarryQuantity).Equal(parseDecimal(aggregatedCarry)) {
		t.Fatalf("prepare=%+v", base.lastPrepare)
	}
	if base.lastPrepare.FastPathAdmission {
		t.Fatal("dust aggregate must not use fast path residual")
	}
	if base.recomputeCalls != 4 {
		t.Fatalf("recomputeCalls=%d", base.recomputeCalls)
	}
	if store.combo.RuntimeState != wantState ||
		!parseDecimal(store.combo.CarryBaseQuantity).Equal(parseDecimal(finalCarry)) {
		t.Fatalf("combo=%+v wantState=%s final=%s", store.combo, wantState, finalCarry)
	}
	if wantState == "hedge_deferred_dust" && dustEventReason(base) != "below_min_notional" {
		t.Fatalf("events=%v payloads=%v", base.eventTypes, base.eventPayloads)
	}
}

func TestDustOppositeCarryDoesNotMerge(t *testing.T) {
	testDustOneCloseAbsorbsOppositeCarry(t)
}

func TestDustPriceMoveHedgesCarryBeforeMaker(t *testing.T) {
	makerInst := dustCarryInstrument(101, "bybit", "BTCUSDT", "20")
	hedgeInst := dustCarryInstrument(202, "okx", "BTC-USDT-SWAP", "20")
	combo := dustCarryCombo(makerInst, hedgeInst, "ask", "hedge_deferred_dust", "39")
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	base := &executorFailStore{combo: combo, intentStore: orderStore}
	store := &scriptedCarryStore{
		executorFailStore: base,
		carries:           []string{"39", "39", "0", "0"},
	}
	maker := &stubAdapter{place: exchange.Result{
		VenueOrderID: "maker-1", Status: "filled",
		FilledQuantity: "90", AveragePrice: "1",
	}}
	hedge := &stubAdapter{place: exchange.Result{
		VenueOrderID: "hedge-1", Status: "filled",
		FilledQuantity: "39", AveragePrice: "1",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": maker, "okx": hedge,
		}),
		dustCarryMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "dust-prehedge", CombinationID: combo.ID, Direction: "ask",
		TargetBaseQuantity: "90", RequestedNotional: "90",
		TriggerLegABid: "1", TriggerLegAAsk: "1",
		TriggerLegBBid: "1", TriggerLegBAsk: "1",
	}
	bbo := marketdata.BBO{BidPrice: "1", AskPrice: "1"}
	if err := executor.executeMakerThenHedge(
		context.Background(), combo, &execution, bbo, bbo, nil,
	); err != nil {
		t.Fatal(err)
	}
	if len(maker.requests) != 0 {
		t.Fatalf("executable leftover carry must not start maker: %+v", maker.requests)
	}
	if len(hedge.requests) != 1 || hedge.requests[0].Quantity != "39" {
		t.Fatalf("hedge requests=%+v", hedge.requests)
	}
	if !base.lastPrepare.AggregateCarry {
		t.Fatalf("prepare=%+v", base.lastPrepare)
	}
}

func TestDustCloseMergedHedgeOverCloseableSkipsMaker(t *testing.T) {
	makerInst := dustCarryInstrument(101, "bybit", "BTCUSDT", "50")
	hedgeInst := dustCarryInstrument(202, "okx", "BTC-USDT-SWAP", "50")
	combo := dustCarryCombo(makerInst, hedgeInst, "bid", "hedge_deferred_dust", "-39")
	combo.LegABasePosition = "50"
	combo.LegBBasePosition = "-50"
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	store := &executorFailStore{combo: combo, intentStore: orderStore}
	maker := &stubAdapter{place: exchange.Result{
		VenueOrderID: "maker-1", Status: "filled",
		FilledQuantity: "90", AveragePrice: "1",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": maker, "okx": unusedVenueAdapter{},
		}),
		dustCarryMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "dust-closeable", CombinationID: combo.ID, Direction: "bid",
		PositionEffect: "close", ReduceOnly: true,
		TargetBaseQuantity: "90", RequestedNotional: "90",
		TriggerLegABid: "1", TriggerLegAAsk: "1",
		TriggerLegBBid: "1", TriggerLegBAsk: "1",
	}
	err := executor.executeMakerThenHedge(
		context.Background(), combo, &execution,
		marketdata.BBO{BidPrice: "1", AskPrice: "1"},
		marketdata.BBO{BidPrice: "1", AskPrice: "1"},
		nil,
	)
	if !errors.Is(err, ErrRiskLimit) || len(maker.requests) != 0 {
		t.Fatalf("err=%v maker=%+v", err, maker.requests)
	}
}

func TestExpectedCarryAfterMaker(t *testing.T) {
	got := expectedCarryAfterMaker(
		decimal.NewFromInt(-39), decimal.NewFromInt(90), "sell",
	)
	if !got.Equal(decimal.NewFromInt(-129)) {
		t.Fatalf("got=%s", got)
	}
	got = expectedCarryAfterMaker(
		decimal.NewFromInt(39), decimal.NewFromInt(90), "buy",
	)
	if !got.Equal(decimal.NewFromInt(129)) {
		t.Fatalf("got=%s", got)
	}
}

func TestCarryBeforeExecutionFromFills(t *testing.T) {
	t.Parallel()
	refreshed := decimal.NewFromInt(89)
	maker := decimal.NewFromInt(90)
	hedge := decimal.NewFromInt(40)
	got, err := carryBeforeExecutionFromFills(refreshed, maker, hedge, "ask")
	if err != nil || !got.Equal(decimal.NewFromInt(39)) {
		t.Fatalf("ask both legs got=%s err=%v", got, err)
	}
	got, err = carryBeforeExecutionFromFills(refreshed, maker, decimal.Zero, "ask")
	if err != nil || !got.Equal(decimal.NewFromInt(-1)) {
		t.Fatalf("ask maker only got=%s err=%v", got, err)
	}
	got, err = carryBeforeExecutionFromFills(
		decimal.NewFromInt(-13369), decimal.NewFromInt(17765), decimal.Zero, "bid",
	)
	if err != nil || !got.Equal(decimal.NewFromInt(4396)) {
		t.Fatalf("bid maker only got=%s err=%v", got, err)
	}
	got, err = carryBeforeExecutionFromFills(
		decimal.NewFromInt(-29), decimal.NewFromInt(90), decimal.NewFromInt(100), "bid",
	)
	if err != nil || !got.Equal(decimal.NewFromInt(-39)) {
		t.Fatalf("bid both legs got=%s err=%v", got, err)
	}
	if _, err := carryBeforeExecutionFromFills(refreshed, maker, hedge, "sideways"); !errors.Is(
		err, ErrInvalidArgument,
	) {
		t.Fatalf("unknown direction err=%v", err)
	}
}

func TestLastCloseStandaloneCarryIsResidualAndCompletesPairedZero(t *testing.T) {
	makerInst := dustCarryInstrument(101, "bybit", "BTCUSDT", "50")
	hedgeInst := dustCarryInstrument(202, "okx", "BTC-USDT-SWAP", "50")
	combo := dustCarryCombo(makerInst, hedgeInst, "bid", "closing", "4501")
	combo.Status = "running"
	combo.RunMode = "one_shot"
	combo.OneShotPhase = "exiting"
	combo.LegABasePosition = "0"
	combo.LegBBasePosition = "4501"
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	execution := ArbitrageExecution{
		ID: "last-close-carry", CombinationID: combo.ID, Direction: "bid",
		Status: "claimed", PositionEffect: "close", ReduceOnly: true,
		LastCloseClip: true, TargetBaseQuantity: "10401", RequestedNotional: "10401",
		TriggerLegABid: "1", TriggerLegAAsk: "1",
		TriggerLegBBid: "1", TriggerLegBAsk: "1",
	}
	base := &executorFailStore{
		combo: combo, intentStore: orderStore, execution: execution,
	}
	base.activeExecution = &base.execution
	store := &scriptedCarryStore{
		executorFailStore: base,
		carries:           []string{"4501", "4501", "0", "0"},
	}
	maker := &stubAdapter{place: exchange.Result{
		VenueOrderID: "maker-1", Status: "filled",
		FilledQuantity: "10401", AveragePrice: "1",
	}}
	hedge := &stubAdapter{place: exchange.Result{
		VenueOrderID: "hedge-1", Status: "filled",
		FilledQuantity: "4501", AveragePrice: "1",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": maker, "okx": hedge,
		}),
		dustCarryMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	err := executor.executeMakerThenHedge(
		context.Background(), combo, &base.execution,
		marketdata.BBO{BidPrice: "1", AskPrice: "1"},
		marketdata.BBO{BidPrice: "1", AskPrice: "1"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(maker.requests) != 0 {
		t.Fatalf("maker requests=%+v", maker.requests)
	}
	if len(hedge.requests) != 1 || hedge.requests[0].Quantity != "4501" {
		t.Fatalf("hedge requests=%+v", hedge.requests)
	}
	if base.lastPrepare.Order.ArbitrageRole != "residual" || !base.lastPrepare.AggregateCarry {
		t.Fatalf("prepare=%+v", base.lastPrepare)
	}
	if base.execution.Status != "completed" ||
		base.execution.Status == "failed" ||
		base.execution.Status == "reconciling" {
		t.Fatalf("execution=%+v", base.execution)
	}
	if parseDecimal(base.execution.LegAFilledQuantity).IsPositive() ||
		parseDecimal(base.execution.LegBFilledQuantity).IsPositive() {
		t.Fatalf("paired fills=%+v", base.execution)
	}
	orders, err := store.ListArbitrageOrders(context.Background(), combo.OwnerUsername, combo.ID)
	if err != nil {
		t.Fatal(err)
	}
	residual := 0
	for _, order := range orders {
		if order.ArbitrageRole == "residual" {
			residual++
			if order.FilledQuantity != "4501" {
				t.Fatalf("residual=%+v", order)
			}
		}
		if order.ArbitrageRole == "maker" {
			t.Fatalf("unexpected maker=%+v", order)
		}
	}
	if residual != 1 {
		t.Fatalf("residual orders=%d orders=%+v", residual, orders)
	}
	for _, eventType := range base.eventTypes {
		if eventType == "last_close_clip_unbalanced" {
			t.Fatalf("events=%v", base.eventTypes)
		}
	}

	base.combo.LegABasePosition = "10401"
	base.combo.LegBBasePosition = "-10401"
	base.combo.CarryBaseQuantity = "0"
	store.carries = []string{"4501", "4501", "0", "0", "0", "0", "0", "0"}
	paired := ArbitrageExecution{
		ID: "last-close-paired", CombinationID: combo.ID, Direction: "bid",
		Status: "claimed", PositionEffect: "close", ReduceOnly: true,
		LastCloseClip: true, TargetBaseQuantity: "10401", RequestedNotional: "10401",
		TriggerLegABid: "1", TriggerLegAAsk: "1",
		TriggerLegBBid: "1", TriggerLegBAsk: "1",
	}
	base.execution = paired
	base.activeExecution = &base.execution
	hedge.place = exchange.Result{
		VenueOrderID: "hedge-2", Status: "filled",
		FilledQuantity: "10401", AveragePrice: "1",
	}
	if err := executor.executeMakerThenHedge(
		context.Background(), base.combo, &base.execution,
		marketdata.BBO{BidPrice: "1", AskPrice: "1"},
		marketdata.BBO{BidPrice: "1", AskPrice: "1"},
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if len(maker.requests) != 1 || maker.requests[0].Quantity != "10401" {
		t.Fatalf("maker requests=%+v", maker.requests)
	}
	if len(hedge.requests) != 2 || hedge.requests[1].Quantity != "10401" {
		t.Fatalf("hedge requests=%+v", hedge.requests)
	}
	if base.lastPrepare.Order.ArbitrageRole != "hedge" {
		t.Fatalf("paired hedge role=%q", base.lastPrepare.Order.ArbitrageRole)
	}
	if base.execution.Status != "completed" {
		t.Fatalf("paired execution=%+v", base.execution)
	}
	if !parseDecimal(base.execution.LegAFilledQuantity).Equal(decimal.NewFromInt(10401)) ||
		!parseDecimal(base.execution.LegBFilledQuantity).Equal(decimal.NewFromInt(10401)) {
		t.Fatalf("paired fills=%+v", base.execution)
	}
	orders, err = store.ListArbitrageOrders(context.Background(), combo.OwnerUsername, combo.ID)
	if err != nil {
		t.Fatal(err)
	}
	var residualFill, makerFill, hedgeFill decimal.Decimal
	for _, order := range orders {
		filled := parseDecimal(order.FilledQuantity)
		switch order.ArbitrageRole {
		case "residual":
			residualFill = residualFill.Add(filled)
		case "maker":
			makerFill = makerFill.Add(filled)
		case "hedge":
			hedgeFill = hedgeFill.Add(filled)
		}
	}
	if !residualFill.Equal(decimal.NewFromInt(4501)) ||
		!makerFill.Equal(decimal.NewFromInt(10401)) ||
		!hedgeFill.Equal(decimal.NewFromInt(10401)) {
		t.Fatalf("ledger residual=%s maker=%s hedge=%s orders=%+v",
			residualFill, makerFill, hedgeFill, orders)
	}
	unbalanced := 0
	for _, eventType := range base.eventTypes {
		if eventType == "last_close_clip_unbalanced" {
			unbalanced++
		}
	}
	if unbalanced != 0 {
		t.Fatalf("events=%v", base.eventTypes)
	}
}

func dustHasEvent(store *executorFailStore, name string) bool {
	for _, eventType := range store.eventTypes {
		if eventType == name {
			return true
		}
	}
	return false
}

func seedDustOrder(
	t *testing.T, store *postOnlyOrderStore, order Order, status, filled string,
) Order {
	t.Helper()
	if order.OwnerUsername == "" {
		order.OwnerUsername = "admin"
	}
	stored, created, err := store.CreateIntent(context.Background(), order)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if status == "" || status == "pending" {
		return stored
	}
	stored, err = store.UpdateResult(context.Background(), stored.ID, VenueResult{
		Status: status, FilledQuantity: filled, AveragePrice: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestStandaloneCarryPlan(t *testing.T) {
	makerInst := dustCarryInstrument(101, "bybit", "BTCUSDT", "50")
	hedgeInst := dustCarryInstrument(202, "okx", "BTC-USDT-SWAP", "50")
	bbo := marketdata.BBO{BidPrice: "1", AskPrice: "1"}
	executor := &ArbitrageExecutor{
		spotSnapshots: make(map[string]map[int64]portfolio.Snapshot),
	}
	closeExec := ArbitrageExecution{
		ID: "plan", Direction: "ask", PositionEffect: "close", ReduceOnly: true,
	}

	full := dustCarryCombo(makerInst, hedgeInst, "ask", "monitoring", "100")
	full.LegABasePosition = "0"
	full.LegBBasePosition = "100"
	action, err := executor.standaloneCarryPlan(
		context.Background(), full, closeExec, full.LegB, hedgeInst,
		Credentials{TradingAccountID: 2}, bbo,
	)
	if err != nil || action != standaloneCarryExecute {
		t.Fatalf("full size action=%d err=%v", action, err)
	}

	partial := dustCarryCombo(makerInst, hedgeInst, "ask", "monitoring", "100")
	partial.LegABasePosition = "80"
	partial.LegBBasePosition = "20"
	action, err = executor.standaloneCarryPlan(
		context.Background(), partial, closeExec, partial.LegB, hedgeInst,
		Credentials{TradingAccountID: 2}, bbo,
	)
	if err != nil || action != standaloneCarryDeferToMaker {
		t.Fatalf("available < carry action=%d err=%v", action, err)
	}

	tiny := dustCarryCombo(makerInst, hedgeInst, "ask", "monitoring", "39")
	action, err = executor.standaloneCarryPlan(
		context.Background(), tiny, closeExec, tiny.LegB, hedgeInst,
		Credentials{TradingAccountID: 2}, bbo,
	)
	if err != nil || action != standaloneCarryDeferToMaker {
		t.Fatalf("below min notional action=%d err=%v", action, err)
	}

	unknown := hedgeInst
	unknown.MinNotionalStatus = exchange.ConstraintUnknown
	_, err = executor.standaloneCarryPlan(
		context.Background(), full, closeExec, full.LegB, unknown,
		Credentials{TradingAccountID: 2}, bbo,
	)
	if !errors.Is(err, ErrInstrumentUnavailable) {
		t.Fatalf("unknown constraint err=%v", err)
	}

	noStep := hedgeInst
	noStep.QuantityStep = "0"
	_, err = executor.standaloneCarryPlan(
		context.Background(), full, closeExec, full.LegB, noStep,
		Credentials{TradingAccountID: 2}, bbo,
	)
	if !errors.Is(err, ErrInstrumentUnavailable) {
		t.Fatalf("step unavailable err=%v", err)
	}

	spot := hedgeInst
	spot.ContractType = "spot"
	spotCombo := full
	spotCombo.LegB.ContractType = "spot"
	executor.spotSnapshots[closeExec.ID] = map[int64]portfolio.Snapshot{
		2: {SpotBalances: map[string]string{"BTC": "10"}},
	}
	action, err = executor.standaloneCarryPlan(
		context.Background(), spotCombo, closeExec, spotCombo.LegB, spot,
		Credentials{TradingAccountID: 2}, bbo,
	)
	if err != nil || action != standaloneCarryDeferToMaker {
		t.Fatalf("spot shortfall action=%d err=%v", action, err)
	}

	executor.spotSnapshots = make(map[string]map[int64]portfolio.Snapshot)
	_, err = executor.standaloneCarryPlan(
		context.Background(), spotCombo, closeExec, spotCombo.LegB, spot,
		Credentials{TradingAccountID: 2}, bbo,
	)
	if !errors.Is(err, ErrVenueUnavailable) {
		t.Fatalf("snapshot failure err=%v", err)
	}
}

func TestLeftoverCloseDustCompletableGuards(t *testing.T) {
	combo := ArbitrageCombination{}
	execution := ArbitrageExecution{PositionEffect: "close", ReduceOnly: true, ID: "dust"}
	maker := Order{Status: "filled", FilledQuantity: "40"}
	before := decimal.NewFromInt(100)
	shrunk := decimal.NewFromInt(60)
	owned := decimal.NewFromInt(200)
	if !leftoverCloseDustCompletable(combo, execution, maker, nil, before, shrunk, owned) {
		t.Fatal("expected leftover complete")
	}
	if leftoverCloseDustCompletable(combo, execution, maker, nil, before, before, owned) {
		t.Fatal("equal carry must fail")
	}
	if leftoverCloseDustCompletable(
		combo, execution, maker,
		[]Order{{
			ArbitrageExecutionID: "dust", ArbitrageRole: "hedge", Status: "open",
		}},
		before, shrunk, owned,
	) {
		t.Fatal("live hedge must not complete")
	}
	openExec := execution
	openExec.PositionEffect = "open"
	if leftoverCloseDustCompletable(combo, openExec, maker, nil, before, shrunk, owned) {
		t.Fatal("open must not complete leftover")
	}
}

func testDustOneCloseAbsorbsOppositeCarry(t *testing.T) {
	t.Helper()
	makerInst := dustCarryInstrument(101, "bybit", "BTCUSDT", "50")
	hedgeInst := dustCarryInstrument(202, "okx", "BTC-USDT-SWAP", "50")
	combo := dustCarryCombo(makerInst, hedgeInst, "bid", "monitoring", "4396")
	combo.LegABasePosition = "431618"
	combo.LegBBasePosition = "-427222"
	combo.OrderNotional = "17765"
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	base := &executorFailStore{combo: combo, intentStore: orderStore}
	store := &scriptedCarryStore{
		executorFailStore: base,
		carries:           []string{"4396", "-13369", "0", "0"},
	}
	maker := &stubAdapter{place: exchange.Result{
		VenueOrderID: "maker-1", Status: "filled",
		FilledQuantity: "17765", AveragePrice: "1",
	}}
	hedge := &stubAdapter{place: exchange.Result{
		VenueOrderID: "hedge-1", Status: "filled",
		FilledQuantity: "13369", AveragePrice: "1",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": maker, "okx": hedge,
		}),
		dustCarryMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "dust-one-close", CombinationID: combo.ID, Direction: "bid",
		PositionEffect: "close", ReduceOnly: true,
		TargetBaseQuantity: "17765", RequestedNotional: "17765",
		TriggerLegABid: "1", TriggerLegAAsk: "1",
		TriggerLegBBid: "1", TriggerLegBAsk: "1",
	}
	bbo := marketdata.BBO{BidPrice: "1", AskPrice: "1"}
	if err := executor.executeMakerThenHedge(
		context.Background(), combo, &execution, bbo, bbo, nil,
	); err != nil {
		t.Fatal(err)
	}
	if len(maker.requests) != 1 || maker.requests[0].Quantity != "17765" {
		t.Fatalf("maker requests=%+v", maker.requests)
	}
	if len(hedge.requests) != 1 ||
		hedge.requests[0].Side != "buy" ||
		hedge.requests[0].Quantity != "13369" ||
		!hedge.requests[0].ReduceOnly {
		t.Fatalf("hedge requests=%+v", hedge.requests)
	}
	if !base.lastPrepare.AggregateCarry || base.lastPrepare.FastPathAdmission ||
		!parseDecimal(base.lastPrepare.ExpectedCarryQuantity).Equal(decimal.NewFromInt(-13369)) {
		t.Fatalf("prepare=%+v", base.lastPrepare)
	}
	if execution.Status != "completed" {
		t.Fatalf("execution=%+v", execution)
	}
}

func TestDustOrdinaryFillUsesFastPathNoExtraRefresh(t *testing.T) {
	makerInst := dustCarryInstrument(101, "bybit", "BTCUSDT", "50")
	hedgeInst := dustCarryInstrument(202, "okx", "BTC-USDT-SWAP", "50")
	combo := dustCarryCombo(makerInst, hedgeInst, "ask", "monitoring", "0")
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	base := &executorFailStore{combo: combo, intentStore: orderStore}
	store := &scriptedCarryStore{
		executorFailStore: base,
		carries:           []string{"0", "0", "0"},
	}
	maker := &stubAdapter{place: exchange.Result{
		VenueOrderID: "maker-1", Status: "filled",
		FilledQuantity: "90", AveragePrice: "1",
	}}
	hedge := &stubAdapter{place: exchange.Result{
		VenueOrderID: "hedge-1", Status: "filled",
		FilledQuantity: "90", AveragePrice: "1",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": maker, "okx": hedge,
		}),
		dustCarryMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "dust-fast", CombinationID: combo.ID, Direction: "ask",
		TargetBaseQuantity: "90", RequestedNotional: "90",
		TriggerLegABid: "1", TriggerLegAAsk: "1",
		TriggerLegBBid: "1", TriggerLegBAsk: "1",
	}
	bbo := marketdata.BBO{BidPrice: "1", AskPrice: "1"}
	if err := executor.executeMakerThenHedge(
		context.Background(), combo, &execution, bbo, bbo, nil,
	); err != nil {
		t.Fatal(err)
	}
	if len(hedge.requests) != 1 || hedge.requests[0].Quantity != "90" {
		t.Fatalf("hedge requests=%+v", hedge.requests)
	}
	if !base.lastPrepare.FastPathAdmission || base.lastPrepare.AggregateCarry {
		t.Fatalf("prepare=%+v", base.lastPrepare)
	}
	if base.recomputeCalls != 3 {
		t.Fatalf("ordinary fill must not refresh before hedge: recomputeCalls=%d", base.recomputeCalls)
	}
}

func TestDustMakerFilledNoHedgeAggregatesWithoutDustState(t *testing.T) {
	makerInst := dustCarryInstrument(101, "bybit", "BTCUSDT", "50")
	hedgeInst := dustCarryInstrument(202, "okx", "BTC-USDT-SWAP", "50")
	combo := dustCarryCombo(makerInst, hedgeInst, "ask", "monitoring", "129")
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	base := &executorFailStore{combo: combo, intentStore: orderStore}
	store := &scriptedCarryStore{
		executorFailStore: base,
		carries:           []string{"129", "129", "0", "0"},
	}
	makerOrder := seedDustOrder(t, orderStore, Order{
		IdempotencyKey: "arb:dust-recover:a:maker:0", OwnerUsername: "admin",
		TradingAccountID: 1, Exchange: "bybit", InstrumentID: makerInst.ID,
		Side: "buy", OrderType: "limit", Quantity: "90", Price: "1",
		ArbitrageExecutionID: "dust-recover", ArbitrageLeg: "a", ArbitrageRole: "maker",
	}, "filled", "90")
	maker := unusedVenueAdapter{}
	hedge := &stubAdapter{place: exchange.Result{
		VenueOrderID: "hedge-1", Status: "filled",
		FilledQuantity: "129", AveragePrice: "1",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": maker, "okx": hedge,
		}),
		dustCarryMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "dust-recover", CombinationID: combo.ID, Direction: "ask",
		Status: "maker_open", MakerOrderID: makerOrder.ID,
		LegAFilledQuantity: "90", TargetBaseQuantity: "90", RequestedNotional: "90",
		TriggerLegABid: "1", TriggerLegAAsk: "1",
		TriggerLegBBid: "1", TriggerLegBAsk: "1",
	}
	bbo := marketdata.BBO{BidPrice: "1", AskPrice: "1"}
	if err := executor.executeMakerThenHedge(
		context.Background(), combo, &execution, bbo, bbo, nil,
	); err != nil {
		t.Fatal(err)
	}
	if len(hedge.requests) != 1 || hedge.requests[0].Quantity != "129" {
		t.Fatalf("hedge requests=%+v", hedge.requests)
	}
	if !base.lastPrepare.AggregateCarry || base.lastPrepare.FastPathAdmission {
		t.Fatalf("prepare=%+v", base.lastPrepare)
	}
}

func TestDustRestartPartialHedgeReconstructsCarry(t *testing.T) {
	makerInst := dustCarryInstrument(101, "bybit", "BTCUSDT", "50")
	hedgeInst := dustCarryInstrument(202, "okx", "BTC-USDT-SWAP", "50")
	combo := dustCarryCombo(makerInst, hedgeInst, "ask", "monitoring", "89")
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	base := &executorFailStore{combo: combo, intentStore: orderStore}
	store := &scriptedCarryStore{
		executorFailStore: base,
		carries:           []string{"89", "89", "89", "0", "0"},
	}
	makerOrder := seedDustOrder(t, orderStore, Order{
		IdempotencyKey: "arb:dust-restart:a:maker:0", OwnerUsername: "admin",
		TradingAccountID: 1, Exchange: "bybit", InstrumentID: makerInst.ID,
		Side: "buy", OrderType: "limit", Quantity: "90", Price: "1",
		ArbitrageExecutionID: "dust-restart", ArbitrageLeg: "a", ArbitrageRole: "maker",
	}, "filled", "90")
	hedgeOrder := seedDustOrder(t, orderStore, Order{
		IdempotencyKey: "arb:dust-restart:b:hedge:0", OwnerUsername: "admin",
		TradingAccountID: 2, Exchange: "okx", InstrumentID: hedgeInst.ID,
		Side: "sell", OrderType: "limit", Quantity: "90", Price: "1",
		ArbitrageExecutionID: "dust-restart", ArbitrageLeg: "b", ArbitrageRole: "hedge",
	}, "canceled", "40")
	hedge := &stubAdapter{place: exchange.Result{
		VenueOrderID: "hedge-2", Status: "filled",
		FilledQuantity: "89", AveragePrice: "1",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": unusedVenueAdapter{}, "okx": hedge,
		}),
		dustCarryMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "dust-restart", CombinationID: combo.ID, Direction: "ask",
		Status: "reconciling", MakerOrderID: makerOrder.ID, HedgeOrderID: hedgeOrder.ID,
		HedgeSequence: 0, LegAFilledQuantity: "90", LegBFilledQuantity: "40",
		TargetBaseQuantity: "90", RequestedNotional: "90",
		TriggerLegABid: "1", TriggerLegAAsk: "1",
		TriggerLegBBid: "1", TriggerLegBAsk: "1",
	}
	bbo := marketdata.BBO{BidPrice: "1", AskPrice: "1"}
	if err := executor.executeMakerThenHedge(
		context.Background(), combo, &execution, bbo, bbo, nil,
	); err != nil {
		t.Fatal(err)
	}
	if len(hedge.requests) != 1 || hedge.requests[0].Quantity != "89" {
		t.Fatalf("restart must aggregate current carry, not remaining 50: %+v", hedge.requests)
	}
	if !base.lastPrepare.AggregateCarry || base.lastPrepare.FastPathAdmission {
		t.Fatalf("prepare=%+v", base.lastPrepare)
	}
}

func TestDustLiveHedgeDoesNotCompleteAsDust(t *testing.T) {
	makerInst := dustCarryInstrument(101, "bybit", "BTCUSDT", "50")
	hedgeInst := dustCarryInstrument(202, "okx", "BTC-USDT-SWAP", "50")
	combo := dustCarryCombo(makerInst, hedgeInst, "ask", "monitoring", "129")
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	store := &executorFailStore{combo: combo, intentStore: orderStore}
	makerOrder := seedDustOrder(t, orderStore, Order{
		IdempotencyKey: "arb:dust-live:a:maker:0", OwnerUsername: "admin",
		TradingAccountID: 1, Exchange: "bybit", InstrumentID: makerInst.ID,
		Side: "buy", OrderType: "limit", Quantity: "90", Price: "1",
		ArbitrageExecutionID: "dust-live", ArbitrageLeg: "a", ArbitrageRole: "maker",
	}, "filled", "90")
	hedgeOrder := seedDustOrder(t, orderStore, Order{
		IdempotencyKey: "arb:dust-live:b:hedge:0", OwnerUsername: "admin",
		TradingAccountID: 2, Exchange: "okx", InstrumentID: hedgeInst.ID,
		Side: "sell", OrderType: "limit", Quantity: "90", Price: "1",
		ArbitrageExecutionID: "dust-live", ArbitrageLeg: "b", ArbitrageRole: "hedge",
	}, "open", "0")
	hedge := &stubAdapter{query: exchange.Result{Status: "open", FilledQuantity: "0"}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": unusedVenueAdapter{}, "okx": hedge,
		}),
		dustCarryMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, 20*time.Millisecond, nil,
	)
	execution := ArbitrageExecution{
		ID: "dust-live", CombinationID: combo.ID, Direction: "ask",
		Status: "hedging", MakerOrderID: makerOrder.ID, HedgeOrderID: hedgeOrder.ID,
		LegAFilledQuantity: "90", TargetBaseQuantity: "90",
		TriggerLegABid: "1", TriggerLegAAsk: "1",
		TriggerLegBBid: "1", TriggerLegBAsk: "1",
	}
	err := executor.executeMakerThenHedge(
		context.Background(), combo, &execution,
		marketdata.BBO{BidPrice: "1", AskPrice: "1"},
		marketdata.BBO{BidPrice: "1", AskPrice: "1"},
		nil,
	)
	if !errors.Is(err, ErrVenueUncertain) {
		t.Fatalf("err=%v", err)
	}
	if execution.Status == "completed" {
		t.Fatalf("live hedge must not complete: %+v", execution)
	}
	if len(hedge.requests) != 0 {
		t.Fatalf("placed replacement: %+v", hedge.requests)
	}
}

func TestDustClosePartialPositiveCarryCompletesLeftover(t *testing.T) {
	makerInst := dustCarryInstrument(101, "bybit", "BTCUSDT", "20")
	hedgeInst := dustCarryInstrument(202, "okx", "BTC-USDT-SWAP", "20")
	combo := dustCarryCombo(makerInst, hedgeInst, "bid", "monitoring", "100")
	combo.LegABasePosition = "200"
	combo.LegBBasePosition = "-100"
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	base := &executorFailStore{combo: combo, intentStore: orderStore}
	store := &scriptedCarryStore{
		executorFailStore: base,
		carries:           []string{"100", "60", "60"},
	}
	maker := &stubAdapter{place: exchange.Result{
		VenueOrderID: "maker-1", Status: "filled",
		FilledQuantity: "40", AveragePrice: "1",
	}}
	hedge := &stubAdapter{}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": maker, "okx": hedge,
		}),
		dustCarryMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "dust-leftover", CombinationID: combo.ID, Direction: "bid",
		PositionEffect: "close", ReduceOnly: true,
		TargetBaseQuantity: "40", RequestedNotional: "40",
		TriggerLegABid: "1", TriggerLegAAsk: "1",
		TriggerLegBBid: "1", TriggerLegBAsk: "1",
	}
	if err := executor.executeMakerThenHedge(
		context.Background(), combo, &execution,
		marketdata.BBO{BidPrice: "1", AskPrice: "1"},
		marketdata.BBO{BidPrice: "1", AskPrice: "1"},
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if len(hedge.requests) != 0 {
		t.Fatalf("must not expand short hedge: %+v", hedge.requests)
	}
	if execution.Status != "completed" {
		t.Fatalf("execution=%+v", execution)
	}
	if store.combo.RuntimeState != "hedge_deferred_dust" {
		t.Fatalf("combo=%+v", store.combo)
	}
	if !dustHasEvent(base, "hedge_deferred_dust") {
		t.Fatalf("events=%v", base.eventTypes)
	}
}

func TestDustEqualCarryLeftoverMustFail(t *testing.T) {
	makerInst := dustCarryInstrument(101, "bybit", "BTCUSDT", "20")
	hedgeInst := dustCarryInstrument(202, "okx", "BTC-USDT-SWAP", "20")
	combo := dustCarryCombo(makerInst, hedgeInst, "bid", "monitoring", "100")
	combo.LegABasePosition = "200"
	combo.LegBBasePosition = "-100"
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	base := &executorFailStore{combo: combo, intentStore: orderStore}
	store := &scriptedCarryStore{
		executorFailStore: base,
		carries:           []string{"100", "100"},
	}
	maker := &stubAdapter{place: exchange.Result{
		VenueOrderID: "maker-1", Status: "filled",
		FilledQuantity: "40", AveragePrice: "1",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": maker, "okx": unusedVenueAdapter{},
		}),
		dustCarryMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "dust-equal", CombinationID: combo.ID, Direction: "bid",
		PositionEffect: "close", ReduceOnly: true,
		TargetBaseQuantity: "40", RequestedNotional: "40",
		TriggerLegABid: "1", TriggerLegAAsk: "1",
		TriggerLegBBid: "1", TriggerLegBAsk: "1",
	}
	err := executor.executeMakerThenHedge(
		context.Background(), combo, &execution,
		marketdata.BBO{BidPrice: "1", AskPrice: "1"},
		marketdata.BBO{BidPrice: "1", AskPrice: "1"},
		nil,
	)
	if !errors.Is(err, ErrArbitrageUnsafeHedge) {
		t.Fatalf("err=%v", err)
	}
	if execution.Status == "completed" {
		t.Fatalf("equal carry must not complete: %+v", execution)
	}
}

func TestDustRepricingThenAggregatesNetCarry(t *testing.T) {
	makerInst := dustCarryInstrument(101, "bybit", "BTCUSDT", "50")
	hedgeInst := dustCarryInstrument(202, "okx", "BTC-USDT-SWAP", "50")
	combo := dustCarryCombo(makerInst, hedgeInst, "ask", "monitoring", "39")
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	base := &executorFailStore{combo: combo, intentStore: orderStore}
	store := &scriptedCarryStore{
		executorFailStore: base,
		carries:           []string{"39", "39", "39", "39", "129", "0", "0"},
	}
	maker := &stubAdapter{
		placeResults: []exchange.Result{
			{
				Status: "rejected", ErrorCode: "EC_PostOnlyWillTakeLiquidity",
				FilledQuantity: "0",
			},
			{VenueOrderID: "maker-2", Status: "filled", FilledQuantity: "90", AveragePrice: "1"},
		},
		placeErrors: []error{exchange.ErrRejected, nil},
	}
	hedge := &stubAdapter{place: exchange.Result{
		VenueOrderID: "hedge-1", Status: "filled",
		FilledQuantity: "129", AveragePrice: "1",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{makerInst.ID: makerInst, hedgeInst.ID: hedgeInst},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": maker, "okx": hedge,
		}),
		dustCarryMarket(t, makerInst, hedgeInst), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	first := ArbitrageExecution{
		ID: "dust-reprice-1", CombinationID: combo.ID, Direction: "ask",
		TargetBaseQuantity: "90", RequestedNotional: "90",
		TriggerLegABid: "1", TriggerLegAAsk: "1",
		TriggerLegBBid: "1", TriggerLegBAsk: "1",
	}
	bbo := marketdata.BBO{BidPrice: "1", AskPrice: "1"}
	if err := executor.executeMakerThenHedge(
		context.Background(), combo, &first, bbo, bbo, nil,
	); err != nil {
		t.Fatal(err)
	}
	if store.combo.RuntimeState != "repricing" {
		t.Fatalf("runtime=%s", store.combo.RuntimeState)
	}
	if len(hedge.requests) != 0 {
		t.Fatalf("zero fill must not hedge: %+v", hedge.requests)
	}
	base.execution = ArbitrageExecution{}
	base.executions = nil
	second := ArbitrageExecution{
		ID: "dust-reprice-2", CombinationID: combo.ID, Direction: "ask",
		TargetBaseQuantity: "90", RequestedNotional: "90",
		TriggerLegABid: "1", TriggerLegAAsk: "1",
		TriggerLegBBid: "1", TriggerLegBAsk: "1",
	}
	if err := executor.executeMakerThenHedge(
		context.Background(), store.combo, &second, bbo, bbo, nil,
	); err != nil {
		t.Fatal(err)
	}
	if len(hedge.requests) != 1 || hedge.requests[0].Quantity != "129" {
		t.Fatalf("hedge requests=%+v", hedge.requests)
	}
	if !base.lastPrepare.AggregateCarry || base.lastPrepare.FastPathAdmission {
		t.Fatalf("prepare=%+v", base.lastPrepare)
	}
}
