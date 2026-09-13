package trader

import (
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

func sizingInstrument(id int64, exchangeName, contractSize, step string) Instrument {
	return Instrument{
		ID: id, Exchange: exchangeName, ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", ContractSize: contractSize,
		QuantityStep: step, PriceTick: "0.1",
		MinQuantity: step, MinNotional: "1",
		MinQuantityStatus:  exchange.ConstraintKnown,
		MinNotionalStatus:  exchange.ConstraintKnown,
		MaxQuantityStatus:  exchange.ConstraintNotApplicable,
		MarketQuantityStep: step, MarketMinQuantity: step, MarketMinNotional: "1",
		MarketQuantityStepStatus: exchange.ConstraintKnown,
		MarketMinQuantityStatus:  exchange.ConstraintKnown,
		MarketMaxQuantityStatus:  exchange.ConstraintNotApplicable,
		MarketMinNotionalStatus:  exchange.ConstraintKnown,
	}
}

func TestTopOfBookNotionalUsesContractSizeNotRawQtyTimesPrice(t *testing.T) {
	instrument := sizingInstrument(1, "okx", "0.01", "0.01")
	got, ok := topOfBookNotional(instrument, "100", "10")
	if !ok {
		t.Fatal("expected top of book notional")
	}
	// 10 contracts * 0.01 base * 100 price = 10, not raw 10*100=1000.
	want := decimal.RequireFromString("10")
	if !got.Equal(want) {
		t.Fatalf("got=%s want=%s", got, want)
	}
}

func TestRandomNotionalBoundsZeroAnd1999Cents(t *testing.T) {
	combo := ArbitrageCombination{
		ExecutionMode: "maker_then_hedge", MakerLeg: "a", TargetNotional: "10000",
		LegA: ArbitrageLeg{InstrumentID: 1, ExchangeSymbol: "BTCUSDT"},
		LegB: ArbitrageLeg{InstrumentID: 2, ExchangeSymbol: "ETHUSDT"},
	}
	instrumentA := sizingInstrument(1, "binance", "1", "0.001")
	instrumentB := sizingInstrument(2, "okx", "1", "0.001")
	bbo := marketdata.BBO{
		BidPrice: "100", AskPrice: "101", BidQuantity: "1000", AskQuantity: "1000",
	}
	zero, _, ok := sizeArbitrageExecution(
		combo, instrumentA, instrumentB, bbo, bbo, "ask", "open",
		decimal.NewFromInt(10000), decimal.Zero, fixedNotionalRand{decimal.Zero},
	)
	if !ok || !zero.RandomNotional.Equal(decimal.Zero) {
		t.Fatalf("zero random plan=%+v ok=%v", zero, ok)
	}
	maxRandom := decimal.RequireFromString("19.99")
	high, _, ok := sizeArbitrageExecution(
		combo, instrumentA, instrumentB, bbo, bbo, "ask", "open",
		decimal.NewFromInt(10000), decimal.Zero, fixedNotionalRand{maxRandom},
	)
	if !ok || !high.RandomNotional.Equal(maxRandom) || !high.RandomNotional.LessThan(decimal.NewFromInt(20)) {
		t.Fatalf("19.99 random plan=%+v ok=%v", high, ok)
	}
}

func TestMakerSizeCapsAtHedgeTopOfBook(t *testing.T) {
	combo := ArbitrageCombination{
		ExecutionMode: "maker_then_hedge", MakerLeg: "a", TargetNotional: "10000",
		LegA: ArbitrageLeg{InstrumentID: 1}, LegB: ArbitrageLeg{InstrumentID: 2},
	}
	instrument := sizingInstrument(1, "binance", "1", "0.001")
	maker := marketdata.BBO{BidPrice: "100", AskPrice: "100", BidQuantity: "1000", AskQuantity: "1000"}
	hedge := marketdata.BBO{BidPrice: "100", AskPrice: "100", BidQuantity: "0.3", AskQuantity: "0.3"}
	plan, _, ok := sizeArbitrageExecution(
		combo, instrument, instrument, maker, hedge, "ask", "open",
		decimal.NewFromInt(10000), decimal.Zero, fixedNotionalRand{decimal.RequireFromString("19.99")},
	)
	if !ok {
		t.Fatal("expected sized plan")
	}
	hedgeCap := decimal.RequireFromString("30")
	if plan.RequestedNotional.GreaterThan(hedgeCap) || plan.HedgeCapNotional.GreaterThan(hedgeCap) {
		t.Fatalf("plan=%+v hedgeCap=%s", plan, hedgeCap)
	}
}

func TestRequestedNotionalIsMaxOfLegNotionals(t *testing.T) {
	combo := ArbitrageCombination{
		ExecutionMode: "simultaneous_market", TargetNotional: "10000",
		LegA: ArbitrageLeg{InstrumentID: 1}, LegB: ArbitrageLeg{InstrumentID: 2},
	}
	instrument := sizingInstrument(1, "binance", "1", "0.001")
	bboA := marketdata.BBO{BidPrice: "100", AskPrice: "100", BidQuantity: "1000", AskQuantity: "1000"}
	bboB := marketdata.BBO{BidPrice: "110", AskPrice: "110", BidQuantity: "1000", AskQuantity: "1000"}
	plan, _, ok := sizeArbitrageExecution(
		combo, instrument, instrument, bboA, bboB, "ask", "open",
		decimal.NewFromInt(10000), decimal.Zero, fixedNotionalRand{decimal.Zero},
	)
	if !ok {
		t.Fatal("expected sized plan")
	}
	want := decimal.Max(plan.LegANotional, plan.LegBNotional)
	if !plan.RequestedNotional.Equal(want) {
		t.Fatalf("requested=%s want=%s a=%s b=%s", plan.RequestedNotional, want, plan.LegANotional, plan.LegBNotional)
	}
}

func TestOpenSizeAbortsWhenRemainingBelowTwenty(t *testing.T) {
	combo := ArbitrageCombination{
		ExecutionMode: "simultaneous_market", TargetNotional: "10000",
		LegA: ArbitrageLeg{InstrumentID: 1}, LegB: ArbitrageLeg{InstrumentID: 2},
	}
	instrument := sizingInstrument(1, "binance", "1", "0.001")
	bbo := marketdata.BBO{BidPrice: "100", AskPrice: "100", BidQuantity: "1000", AskQuantity: "1000"}
	if _, _, ok := sizeArbitrageExecution(
		combo, instrument, instrument, bbo, bbo, "ask", "open",
		decimal.NewFromInt(19), decimal.Zero, fixedNotionalRand{decimal.Zero},
	); ok {
		t.Fatal("expected remaining < 20 to skip open")
	}
}

func TestCloseSizeUsesActualPositionWhenSmallerThanDynamic(t *testing.T) {
	combo := ArbitrageCombination{
		ExecutionMode: "simultaneous_market", TargetNotional: "10000",
		LegABasePosition: "0.05", LegBBasePosition: "-0.05",
		LegA: ArbitrageLeg{InstrumentID: 1}, LegB: ArbitrageLeg{InstrumentID: 2},
	}
	instrument := sizingInstrument(1, "binance", "1", "0.001")
	bbo := marketdata.BBO{BidPrice: "100", AskPrice: "100", BidQuantity: "1000", AskQuantity: "1000"}
	closeable := decimal.RequireFromString("5")
	plan, _, ok := sizeArbitrageExecution(
		combo, instrument, instrument, bbo, bbo, "bid", "close",
		decimal.Zero, closeable, fixedNotionalRand{decimal.Zero},
	)
	if !ok || !plan.ReduceOnly || plan.Effect != "close" {
		t.Fatalf("plan=%+v ok=%v", plan, ok)
	}
	if plan.RequestedNotional.GreaterThan(closeable) {
		t.Fatalf("requested=%s closeable=%s", plan.RequestedNotional, closeable)
	}
	if plan.TargetBaseQuantity.GreaterThan(decimal.RequireFromString("0.05")) {
		t.Fatalf("base=%s", plan.TargetBaseQuantity)
	}
}

func TestStoredTargetBaseQuantityIsNotRecappedByRequestedNotional(t *testing.T) {
	execution := ArbitrageExecution{TargetBaseQuantity: "0.5", RequestedNotional: "10"}
	price := decimal.NewFromInt(100)
	qty := parsePositiveDecimal(execution.TargetBaseQuantity)
	if !qty.IsPositive() {
		t.Fatal("stored target base is required")
	}
	requestedCap := executionNotional(ArbitrageCombination{OrderNotional: "500"}, execution).Div(price)
	if !qty.GreaterThan(requestedCap) {
		t.Fatalf("qty=%s recap=%s", qty, requestedCap)
	}
	if executionNotional(ArbitrageCombination{OrderNotional: "500"}, execution).Equal(decimal.RequireFromString("500")) {
		t.Fatal("execution notional must not fall back to combination order notional")
	}
}

func TestRemainingDirectionNotionalUsesMaxUsedLeg(t *testing.T) {
	remaining := remainingDirectionNotional(
		decimal.RequireFromString("2"),
		decimal.RequireFromString("-3"),
		decimal.NewFromInt(1000),
		decimal.NewFromInt(100),
		decimal.NewFromInt(100),
	)
	if !remaining.Equal(decimal.NewFromInt(700)) {
		t.Fatalf("remaining=%s", remaining)
	}
	if remainingDirectionNotional(
		decimal.Zero, decimal.Zero, decimal.NewFromInt(19),
		decimal.NewFromInt(100), decimal.NewFromInt(100),
	).GreaterThanOrEqual(decimal.NewFromInt(20)) {
		t.Fatal("remaining below 20 should not allow a new open")
	}
}

func TestCryptoNotionalRandStaysBelowTwenty(t *testing.T) {
	for i := 0; i < 200; i++ {
		value := cryptoNotionalRand{}.RandomNotional()
		if value.IsNegative() || !value.LessThan(decimal.NewFromInt(20)) {
			t.Fatalf("random=%s", value)
		}
	}
}

func lastClipCloseInstrument(id int64, minQuantity, minNotional string) Instrument {
	return Instrument{
		ID: id, Exchange: "bybit", ContractType: "perpetual",
		ExchangeSymbol: "USELESSUSDT", ContractSize: "1",
		QuantityStep: "1", PriceTick: "0.001",
		MinQuantity: minQuantity, MinNotional: minNotional,
		MinQuantityStatus:  exchange.ConstraintKnown,
		MinNotionalStatus:  exchange.ConstraintKnown,
		MaxQuantityStatus:  exchange.ConstraintNotApplicable,
		MarketQuantityStep: "1", MarketMinQuantity: minQuantity,
		MarketMinNotional:        minNotional,
		MarketQuantityStepStatus: exchange.ConstraintKnown,
		MarketMinQuantityStatus:  exchange.ConstraintKnown,
		MarketMaxQuantityStatus:  exchange.ConstraintNotApplicable,
		MarketMinNotionalStatus:  exchange.ConstraintKnown,
	}
}

func lastClipCloseBBO() marketdata.BBO {
	return marketdata.BBO{
		BidPrice: "0.214", AskPrice: "0.214",
		BidQuantity: "100000", AskQuantity: "100000",
	}
}

func lastClipCloseable() decimal.Decimal {
	return decimal.RequireFromString("4.28")
}

func lastClipCloseCombination(direction string) ArbitrageCombination {
	legA, legB := "20", "-20"
	if strings.EqualFold(direction, "ask") {
		legA, legB = "-20", "20"
	}
	return ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61750", OwnerUsername: "admin",
		Status: "closing", RuntimeState: "closing",
		ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		TargetNotional: "10000", OrderNotional: "500",
		LegABasePosition: legA, LegBBasePosition: legB,
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 101,
			Exchange: "bybit", ContractType: "perpetual",
			ExchangeSymbol: "USELESSUSDT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 202,
			Exchange: "okx", ContractType: "perpetual",
			ExchangeSymbol: "USELESSUSDT",
		},
	}
}

func TestLastClipCloseSizesBelowVenueMinNotional(t *testing.T) {
	instrumentA := lastClipCloseInstrument(101, "1", "5")
	instrumentB := lastClipCloseInstrument(202, "1", "5")
	bbo := lastClipCloseBBO()
	closeable := lastClipCloseable()
	for _, direction := range []string{"bid", "ask"} {
		plan, reason, ok := sizeArbitrageExecution(
			lastClipCloseCombination(direction), instrumentA, instrumentB, bbo, bbo,
			direction, "close", decimal.Zero, closeable, fixedNotionalRand{decimal.Zero},
		)
		if !ok || !plan.LastClip || !plan.ReduceOnly || plan.Effect != "close" {
			t.Fatalf("direction=%s plan=%+v ok=%v reason=%s", direction, plan, ok, reason)
		}
		if !plan.TargetBaseQuantity.Equal(decimal.NewFromInt(20)) {
			t.Fatalf("direction=%s qty=%s", direction, plan.TargetBaseQuantity)
		}
	}
	if _, _, ok := sizeArbitrageExecution(
		lastClipCloseCombination("ask"), instrumentA, instrumentB, bbo, bbo,
		"ask", "open", lastClipCloseable(), decimal.Zero, fixedNotionalRand{decimal.Zero},
	); ok {
		t.Fatal("open remaining below 20 should stay rejected")
	}
}

func TestLastClipRequiresClosingStatus(t *testing.T) {
	instrumentA := lastClipCloseInstrument(101, "1", "5")
	instrumentB := lastClipCloseInstrument(202, "1", "5")
	bbo := lastClipCloseBBO()
	combo := lastClipCloseCombination("bid")
	combo.Status = "running"
	combo.LegABasePosition = "200"
	combo.LegBBasePosition = "-200"
	plan, reason, ok := sizeArbitrageExecution(
		combo, instrumentA, instrumentB, bbo, bbo,
		"bid", "close", decimal.Zero, decimal.NewFromInt(100),
		fixedNotionalRand{decimal.Zero},
	)
	if !ok || plan.LastClip {
		t.Fatalf("running close must not lastClip ok=%v plan=%+v reason=%s", ok, plan, reason)
	}
}

func TestLastClipAllowsOneShotExiting(t *testing.T) {
	instrumentA := lastClipCloseInstrument(101, "1", "5")
	instrumentB := lastClipCloseInstrument(202, "1", "5")
	bbo := lastClipCloseBBO()
	combo := lastClipCloseCombination("bid")
	combo.Status = "running"
	combo.RuntimeState = "monitoring"
	combo.RunMode = "one_shot"
	combo.OneShotPhase = "exiting"
	plan, reason, ok := sizeArbitrageExecution(
		combo, instrumentA, instrumentB, bbo, bbo,
		"bid", "close", decimal.Zero, lastClipCloseable(),
		fixedNotionalRand{decimal.Zero},
	)
	if !ok || !plan.LastClip || !plan.ReduceOnly {
		t.Fatalf("one-shot exiting last clip ok=%v plan=%+v reason=%s", ok, plan, reason)
	}
}

func TestLastClipCloseStillHonorsMinQtyAndStep(t *testing.T) {
	bbo := lastClipCloseBBO()
	combo := lastClipCloseCombination("bid")
	if _, _, ok := sizeArbitrageExecution(
		combo, lastClipCloseInstrument(101, "50", "5"), lastClipCloseInstrument(202, "50", "5"),
		bbo, bbo, "bid", "close", decimal.Zero, lastClipCloseable(),
		fixedNotionalRand{decimal.Zero},
	); ok {
		t.Fatal("minQty should block close lastClip")
	}
	okx := lastClipCloseInstrument(101, "1", "5")
	okx.Exchange = "okx"
	okx.ContractSize = "3"
	if _, _, ok := sizeArbitrageExecution(
		combo, okx, okx, bbo, bbo, "bid", "close",
		decimal.Zero, lastClipCloseable(), fixedNotionalRand{decimal.Zero},
	); ok {
		t.Fatal("venue quantity conversion should block close lastClip")
	}
}

func TestPrepareArbitrageOrderCloseSkipsMinNotional(t *testing.T) {
	instrument := lastClipCloseInstrument(101, "1", "5")
	closeExec := ArbitrageExecution{
		PositionEffect: "close", ReduceOnly: true, LastCloseClip: true,
	}
	for _, orderType := range []string{"limit", "market"} {
		price := "0.214"
		if orderType == "market" {
			price = ""
		}
		qty, preparedPrice, err := prepareArbitrageOrder(
			instrument, orderType, "20", price, "0.214", closeExec,
		)
		if err != nil || qty != "20" {
			t.Fatalf("orderType=%s qty=%s price=%s err=%v", orderType, qty, preparedPrice, err)
		}
		if _, _, err := prepareArbitrageOrder(
			instrument, orderType, "20", price, "0.214",
			ArbitrageExecution{PositionEffect: "open"},
		); err == nil || !errors.Is(err, ErrOrderBelowMinimum) {
			t.Fatalf("open orderType=%s err=%v", orderType, err)
		}
	}
	if _, _, err := prepareArbitrageOrder(
		instrument, "limit", "20.5", "0.214", "0.214", closeExec,
	); err == nil {
		t.Fatal("step mismatch should still reject close")
	}
	if _, _, err := prepareArbitrageOrder(
		lastClipCloseInstrument(101, "50", "5"), "limit", "20", "0.214", "0.214", closeExec,
	); err == nil || !errors.Is(err, ErrOrderBelowMinimum) {
		t.Fatal("minQty should still reject close")
	}
	if _, _, err := prepareArbitrageOrder(
		instrument, "limit", "20", "0.214", "0.214",
		ArbitrageExecution{PositionEffect: "close", ReduceOnly: true},
	); err == nil || !errors.Is(err, ErrOrderBelowMinimum) {
		t.Fatal("ordinary close must not skip minNotional")
	}
	spot := instrument
	spot.ContractType = "spot"
	if _, _, err := prepareArbitrageOrder(
		spot, "limit", "20", "0.214", "0.214", closeExec,
	); err == nil || !errors.Is(err, ErrOrderBelowMinimum) {
		t.Fatal("spot lastClip must not skip minNotional")
	}
}

func TestLastClipCloseUsesFullRemainingWithoutBBOQuantity(t *testing.T) {
	instrumentA := lastClipCloseInstrument(101, "0.1", "5")
	instrumentA.QuantityStep = "0.1"
	instrumentA.MarketQuantityStep = "0.1"
	instrumentB := lastClipCloseInstrument(202, "0.1", "5")
	instrumentB.QuantityStep = "0.1"
	instrumentB.MarketQuantityStep = "0.1"
	combo := lastClipCloseCombination("bid")
	combo.LegABasePosition = "2.2"
	combo.LegBBasePosition = "-2.2"
	price := decimal.RequireFromString("13.8").Div(decimal.RequireFromString("2.2"))
	bbo := marketdata.BBO{BidPrice: price.String(), AskPrice: price.String()}
	closeable := decimal.RequireFromString("13.8")
	plan, reason, ok := sizeArbitrageExecution(
		combo, instrumentA, instrumentB, bbo, bbo,
		"bid", "close", decimal.Zero, closeable,
		fixedNotionalRand{decimal.RequireFromString("19.99")},
	)
	if !ok || !plan.LastClip || !plan.TargetBaseQuantity.Equal(decimal.RequireFromString("2.2")) {
		t.Fatalf("plan=%+v ok=%v reason=%s", plan, ok, reason)
	}
	if !plan.RandomNotional.IsZero() || !plan.HedgeCapNotional.IsZero() {
		t.Fatalf("lastClip must skip random and hedgeCap plan=%+v", plan)
	}
}

func TestNonLastClipRequiresBBOQuantity(t *testing.T) {
	instrumentA := lastClipCloseInstrument(101, "1", "5")
	instrumentB := lastClipCloseInstrument(202, "1", "5")
	bbo := lastClipCloseBBO()
	bbo.BidQuantity = ""
	bbo.AskQuantity = ""
	openCombo := lastClipCloseCombination("ask")
	openCombo.Status = "running"
	openCombo.RuntimeState = "monitoring"
	if _, reason, ok := sizeArbitrageExecution(
		openCombo, instrumentA, instrumentB, bbo, bbo,
		"ask", "open", decimal.NewFromInt(10000), decimal.Zero, fixedNotionalRand{decimal.Zero},
	); ok || reason != "missing bbo quantity" {
		t.Fatalf("open ok=%v reason=%s", ok, reason)
	}
	closeCombo := lastClipCloseCombination("bid")
	closeCombo.Status = "running"
	closeCombo.RuntimeState = "monitoring"
	if _, reason, ok := sizeArbitrageExecution(
		closeCombo, instrumentA, instrumentB, bbo, bbo,
		"bid", "close", decimal.Zero, decimal.NewFromInt(100), fixedNotionalRand{decimal.Zero},
	); ok || reason != "missing bbo quantity" {
		t.Fatalf("ordinary close ok=%v reason=%s", ok, reason)
	}
	if _, reason, ok := sizeArbitrageExecution(
		lastClipCloseCombination("bid"), instrumentA, instrumentB, bbo, bbo,
		"bid", "close", decimal.Zero, lastClipCloseable(), fixedNotionalRand{decimal.Zero},
	); !ok {
		t.Fatalf("lastClip with empty bbo quantity should size reason=%s", reason)
	}
}
