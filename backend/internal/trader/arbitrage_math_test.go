package trader

import (
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

func TestCalculateArbitrageSpreadsAndTriggers(t *testing.T) {
	ask, bid, ok := calculateArbitrageSpreads(
		decimal.RequireFromString("100"),
		decimal.RequireFromString("101"),
		decimal.RequireFromString("99"),
		decimal.RequireFromString("102"),
	)
	if !ok {
		t.Fatal("expected valid BBO")
	}
	if !ask.Equal(decimal.RequireFromString("99.009900990099")) {
		t.Fatalf("ask=%s", ask)
	}
	if !bid.Equal(decimal.RequireFromString("-100")) {
		t.Fatalf("bid=%s", bid)
	}
	if direction := arbitrageTriggered(
		ask, bid, decimal.RequireFromString("12"), decimal.RequireFromString("-8"),
	); direction != "ask" {
		t.Fatalf("direction=%s", direction)
	}
	if direction := arbitrageTriggered(
		decimal.Zero, bid, decimal.RequireFromString("12"), decimal.RequireFromString("-8"),
	); direction != "bid" {
		t.Fatalf("direction=%s", direction)
	}
}

func TestExecutableArbitrageSpreadsMakerAndSimultaneous(t *testing.T) {
	aBid := decimal.RequireFromString("100")
	aAsk := decimal.RequireFromString("101")
	bBid := decimal.RequireFromString("99")
	bAsk := decimal.RequireFromString("102")
	quoteAsk, quoteBid, ok := calculateArbitrageSpreads(aBid, aAsk, bBid, bAsk)
	if !ok {
		t.Fatal("expected valid quote spreads")
	}

	open, close, ok := executableArbitrageSpreads(
		ArbitrageCombination{ExecutionMode: "maker_then_hedge", MakerLeg: "a"},
		aBid, aAsk, bBid, bAsk,
	)
	if !ok || !open.Equal(quoteBid) || !close.Equal(quoteAsk) {
		t.Fatalf("maker A open=%s close=%s want %s/%s", open, close, quoteBid, quoteAsk)
	}

	open, close, ok = executableArbitrageSpreads(
		ArbitrageCombination{ExecutionMode: "maker_then_hedge", MakerLeg: "b"},
		aBid, aAsk, bBid, bAsk,
	)
	if !ok || !open.Equal(quoteAsk) || !close.Equal(quoteBid) {
		t.Fatalf("maker B open=%s close=%s want %s/%s", open, close, quoteAsk, quoteBid)
	}

	open, close, ok = executableArbitrageSpreads(
		ArbitrageCombination{ExecutionMode: "maker_then_hedge"},
		aBid, aAsk, bBid, bAsk,
	)
	if !ok || !open.Equal(quoteAsk) || !close.Equal(quoteBid) {
		t.Fatalf("maker-first default open=%s close=%s want %s/%s", open, close, quoteAsk, quoteBid)
	}

	open, close, ok = executableArbitrageSpreads(
		ArbitrageCombination{ExecutionMode: "simultaneous_market", MakerLeg: "a"},
		aBid, aAsk, bBid, bAsk,
	)
	wantOpen := bBid.Div(aAsk).Sub(decimal.NewFromInt(1)).Mul(tenThousand)
	wantClose := bAsk.Div(aBid).Sub(decimal.NewFromInt(1)).Mul(tenThousand)
	if !ok || !open.Equal(wantOpen) || !close.Equal(wantClose) {
		t.Fatalf("simultaneous open=%s close=%s want %s/%s", open, close, wantOpen, wantClose)
	}
	if open.Equal(quoteAsk) || close.Equal(quoteBid) {
		t.Fatal("simultaneous executable spreads must not reuse quoteAsk/quoteBid")
	}
}

func TestArbitrageLegSides(t *testing.T) {
	a, b, ok := arbitrageLegSides("ask")
	if !ok || a != "buy" || b != "sell" {
		t.Fatalf("ask sides=%s/%s", a, b)
	}
	a, b, ok = arbitrageLegSides("bid")
	if !ok || a != "sell" || b != "buy" {
		t.Fatalf("bid sides=%s/%s", a, b)
	}
}

func TestArbitrageQuantityAndDelta(t *testing.T) {
	qty := baseQuantityForNotional(
		decimal.RequireFromString("500"),
		decimal.RequireFromString("101"),
		decimal.RequireFromString("0.01"),
	)
	if !qty.Equal(decimal.RequireFromString("4.95")) {
		t.Fatalf("qty=%s", qty)
	}
	delta := deltaNotional(
		decimal.RequireFromString("4.95"),
		decimal.RequireFromString("4.94"),
		decimal.RequireFromString("101"),
	)
	if !delta.Equal(decimal.RequireFromString("1.01")) {
		t.Fatalf("delta=%s", delta)
	}
}

func TestNormalizeArbitrageInput(t *testing.T) {
	input, err := normalizeArbitrageInput(CreateArbitrageInput{
		Token: " token ", IdempotencyKey: "request-123",
		LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		AskThresholdBps: "12.00", BidThresholdBps: "-8.00",
		TargetNotional: "10000",
		ExecutionMode:  "MAKER_THEN_HEDGE", MakerLeg: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if input.MakerLeg != "a" ||
		input.AskThresholdBps != "12" || input.BidThresholdBps != "-8" {
		t.Fatalf("normalized=%+v", input)
	}
}

func TestNormalizeArbitrageInputAllowsAnyThresholdSigns(t *testing.T) {
	cases := []struct {
		name             string
		ask, bid         string
		wantAsk, wantBid string
	}{
		{name: "negative ask positive bid", ask: "-70.00", bid: "8.00", wantAsk: "-70", wantBid: "8"},
		{name: "zero thresholds", ask: "0", bid: "0.0", wantAsk: "0", wantBid: "0"},
		{name: "positive ask negative bid", ask: "12.00", bid: "-8.00", wantAsk: "12", wantBid: "-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, err := normalizeArbitrageInput(CreateArbitrageInput{
				Token: "token", IdempotencyKey: "request-123",
				LegATradingAccountID: 1, LegAInstrumentID: 11,
				LegBTradingAccountID: 2, LegBInstrumentID: 22,
				AskThresholdBps: tc.ask, BidThresholdBps: tc.bid,
				TargetNotional: "1000",
				ExecutionMode:  "simultaneous_market",
			})
			if err != nil {
				t.Fatal(err)
			}
			if input.AskThresholdBps != tc.wantAsk || input.BidThresholdBps != tc.wantBid {
				t.Fatalf("ask=%s bid=%s", input.AskThresholdBps, input.BidThresholdBps)
			}
		})
	}
}

func TestNormalizeArbitrageInputOneShotEntryDirection(t *testing.T) {
	base := CreateArbitrageInput{
		Token: "token", IdempotencyKey: "request-123",
		LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional:     "1000",
		ExecutionMode:      "maker_then_hedge",
		RunMode:            "one_shot",
		ExitPolicy:         "annualized",
		ExitAnnualizedRate: "0.15",
	}

	empty, err := normalizeArbitrageInput(base)
	if err != nil {
		t.Fatal(err)
	}
	if empty.EntryDirection != "ask" {
		t.Fatalf("empty direction=%q", empty.EntryDirection)
	}

	askInput := base
	askInput.EntryDirection = "ASK"
	ask, err := normalizeArbitrageInput(askInput)
	if err != nil {
		t.Fatal(err)
	}
	if ask.EntryDirection != "ask" {
		t.Fatalf("ask direction=%q", ask.EntryDirection)
	}
	if arbitrageFingerprint(ArbitrageCombination{
		LegA:            ArbitrageLeg{TradingAccountID: 1, InstrumentID: 11},
		LegB:            ArbitrageLeg{TradingAccountID: 2, InstrumentID: 22},
		AskThresholdBps: empty.AskThresholdBps, BidThresholdBps: empty.BidThresholdBps,
		TargetNotional: empty.TargetNotional, ExecutionMode: empty.ExecutionMode,
		MakerLeg: empty.MakerLeg, RunMode: empty.RunMode, EntryDirection: empty.EntryDirection,
		ExitPolicy: empty.ExitPolicy, ExitAnnualizedRate: empty.ExitAnnualizedRate,
	}) != arbitrageFingerprint(ArbitrageCombination{
		LegA:            ArbitrageLeg{TradingAccountID: 1, InstrumentID: 11},
		LegB:            ArbitrageLeg{TradingAccountID: 2, InstrumentID: 22},
		AskThresholdBps: ask.AskThresholdBps, BidThresholdBps: ask.BidThresholdBps,
		TargetNotional: ask.TargetNotional, ExecutionMode: ask.ExecutionMode,
		MakerLeg: ask.MakerLeg, RunMode: ask.RunMode, EntryDirection: ask.EntryDirection,
		ExitPolicy: ask.ExitPolicy, ExitAnnualizedRate: ask.ExitAnnualizedRate,
	}) {
		t.Fatal("empty and ask fingerprints must match after normalize")
	}

	bidInput := base
	bidInput.EntryDirection = "bid"
	if _, err := normalizeArbitrageInput(bidInput); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bid err=%v", err)
	}

	spread := base
	spread.RunMode = "spread"
	spread.EntryDirection = "bid"
	normalizedSpread, err := normalizeArbitrageInput(spread)
	if err != nil {
		t.Fatal(err)
	}
	if normalizedSpread.EntryDirection != "" {
		t.Fatalf("spread direction=%q", normalizedSpread.EntryDirection)
	}
}

func TestNormalizeArbitrageInputOneShotIgnoresThresholds(t *testing.T) {
	base := CreateArbitrageInput{
		Token: "token", IdempotencyKey: "request-123",
		LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		TargetNotional:     "1000",
		ExecutionMode:      "maker_then_hedge",
		RunMode:            "one_shot",
		ExitPolicy:         "annualized",
		ExitAnnualizedRate: "0.15",
	}
	cases := []struct {
		name     string
		ask, bid string
	}{
		{name: "empty", ask: "", bid: ""},
		{name: "whitespace", ask: "  ", bid: "\t"},
		{name: "numeric leftover", ask: "12", bid: "-8"},
		{name: "illegal strings", ask: "abc", bid: "12x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := base
			input.AskThresholdBps = tc.ask
			input.BidThresholdBps = tc.bid
			got, err := normalizeArbitrageInput(input)
			if err != nil {
				t.Fatal(err)
			}
			if got.AskThresholdBps != "0" || got.BidThresholdBps != "0" {
				t.Fatalf("ask=%q bid=%q", got.AskThresholdBps, got.BidThresholdBps)
			}
		})
	}
}

func TestNormalizeArbitrageInputOneShotHoldTime(t *testing.T) {
	base := CreateArbitrageInput{
		Token: "token", IdempotencyKey: "request-123",
		LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		TargetNotional: "1000",
		ExecutionMode:  "maker_then_hedge",
		RunMode:        "one_shot",
		ExitPolicy:     "time",
	}
	for _, seconds := range []int{3600, 14400, 28800, 86400, 604800} {
		input := base
		input.ExitAfterSeconds = seconds
		got, err := normalizeArbitrageInput(input)
		if err != nil {
			t.Fatalf("seconds=%d err=%v", seconds, err)
		}
		if got.ExitAfterSeconds != seconds || got.ExitAnnualizedRate != "" {
			t.Fatalf("seconds=%d got=%+v", seconds, got)
		}
	}
	input := base
	input.ExitAfterSeconds = 123
	if _, err := normalizeArbitrageInput(input); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err=%v", err)
	}
}

func TestNormalizeArbitrageInputSpreadRejectsEmptyOrIllegalThresholds(t *testing.T) {
	base := CreateArbitrageInput{
		Token: "token", IdempotencyKey: "request-123",
		LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		TargetNotional: "1000",
		ExecutionMode:  "maker_then_hedge",
		RunMode:        "spread",
	}
	cases := []struct {
		name     string
		ask, bid string
	}{
		{name: "empty", ask: "", bid: ""},
		{name: "empty ask", ask: "", bid: "-8"},
		{name: "illegal", ask: "12x", bid: "-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := base
			input.AskThresholdBps = tc.ask
			input.BidThresholdBps = tc.bid
			if _, err := normalizeArbitrageInput(input); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestSameArbitrageRequestOneShotIgnoresStoredThresholds(t *testing.T) {
	oldRow := ArbitrageCombination{
		OwnerUsername:   "admin",
		LegA:            ArbitrageLeg{TradingAccountID: 1, InstrumentID: 11},
		LegB:            ArbitrageLeg{TradingAccountID: 2, InstrumentID: 22},
		AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "1000", ExecutionMode: "maker_then_hedge",
		MakerLeg: "a", RunMode: "one_shot", EntryDirection: "ask",
		ExitPolicy: "annualized", ExitAnnualizedRate: "0.15",
	}
	normalized := oldRow
	normalized.AskThresholdBps = "0"
	normalized.BidThresholdBps = "0"
	if !sameArbitrageRequest(oldRow, normalized) {
		t.Fatal("one-shot 12/-8 and 0/0 should match")
	}
	if arbitrageFingerprint(oldRow) != arbitrageFingerprint(normalized) {
		t.Fatal("one-shot fingerprints must use 0/0")
	}
}

func TestArbitrageFingerprintSpreadThresholdsDiffer(t *testing.T) {
	left := ArbitrageCombination{
		LegA:            ArbitrageLeg{TradingAccountID: 1, InstrumentID: 11},
		LegB:            ArbitrageLeg{TradingAccountID: 2, InstrumentID: 22},
		AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "1000", ExecutionMode: "maker_then_hedge",
		MakerLeg: "a", RunMode: "spread",
	}
	right := left
	right.AskThresholdBps = "10"
	if arbitrageFingerprint(left) == arbitrageFingerprint(right) {
		t.Fatal("spread fingerprints must change with thresholds")
	}
	if sameArbitrageRequest(left, right) {
		t.Fatal("spread requests with different thresholds must not match")
	}
}

func TestArbitrageTriggeredWithArbitraryThresholdSigns(t *testing.T) {
	if direction := arbitrageTriggered(
		decimal.RequireFromString("-10"),
		decimal.RequireFromString("-5"),
		decimal.RequireFromString("-70"),
		decimal.RequireFromString("8"),
	); direction != "ask" {
		t.Fatalf("negative ask threshold direction=%s", direction)
	}
	if direction := arbitrageTriggered(
		decimal.RequireFromString("-80"),
		decimal.RequireFromString("5"),
		decimal.RequireFromString("-70"),
		decimal.RequireFromString("8"),
	); direction != "bid" {
		t.Fatalf("positive bid threshold direction=%s", direction)
	}
	if direction := arbitrageTriggered(
		decimal.RequireFromString("-80"),
		decimal.RequireFromString("10"),
		decimal.RequireFromString("-70"),
		decimal.RequireFromString("8"),
	); direction != "" {
		t.Fatalf("untriggered direction=%s", direction)
	}
	if direction := arbitrageTriggered(
		decimal.Zero,
		decimal.RequireFromString("1"),
		decimal.Zero,
		decimal.Zero,
	); direction != "ask" {
		t.Fatalf("zero thresholds prefer ask direction=%s", direction)
	}
}

func TestPlanArbitragePosition(t *testing.T) {
	target := decimal.RequireFromString("10000")
	order := decimal.RequireFromString("3000")
	cases := []struct {
		name, venue, direction string
		wantOK                 bool
		effect                 string
		reduce                 bool
		size                   string
	}{
		{name: "zero ask opens", venue: "0", direction: "ask", wantOK: true, effect: "open", size: "3000"},
		{name: "zero bid does nothing", venue: "0", direction: "bid", wantOK: false},
		{name: "ask clips remaining", venue: "8000", direction: "ask", wantOK: true, effect: "open", size: "2000"},
		{name: "ask at cap blocked", venue: "10000", direction: "ask", wantOK: false},
		{name: "bid closes baseline and combination", venue: "9000", direction: "bid", wantOK: true, effect: "close", reduce: true, size: "3000"},
		{name: "bid limited by venue", venue: "500", direction: "bid", wantOK: true, effect: "close", reduce: true, size: "500"},
		{name: "bid never opens reverse", venue: "-8000", direction: "bid", wantOK: false},
		{name: "reverse baseline ask opens through", venue: "-1800", direction: "ask", wantOK: true, effect: "open", size: "3000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, ok := planArbitragePosition(
				decimal.RequireFromString(tc.venue),
				target, order, tc.direction,
			)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v plan=%+v", ok, plan)
			}
			if !ok {
				return
			}
			if plan.Effect != tc.effect || plan.ReduceOnly != tc.reduce || plan.RequestedNotional.String() != tc.size {
				t.Fatalf("plan=%+v", plan)
			}
		})
	}
}

func TestPlanArbitragePositionOverTargetAllowsCloseOnly(t *testing.T) {
	target := decimal.RequireFromString("500")
	order := decimal.RequireFromString("20")
	venue := decimal.RequireFromString("620")
	if _, ok := planArbitragePosition(venue, target, order, "ask"); ok {
		t.Fatal("over-target ask must not open")
	}
	plan, ok := planArbitragePosition(venue, target, order, "bid")
	if !ok || plan.Effect != "close" || !plan.ReduceOnly ||
		plan.RequestedNotional.String() != "20" {
		t.Fatalf("plan=%+v ok=%v", plan, ok)
	}
}

func TestSelectArbitragePositionDirection(t *testing.T) {
	open := decimal.RequireFromString("20")
	closeSpread := decimal.RequireFromString("-20")
	openTh := decimal.RequireFromString("12")
	closeTh := decimal.RequireFromString("-8")
	target := decimal.RequireFromString("500")
	if got := selectArbitragePositionDirection(
		open, closeSpread, openTh, closeTh, decimal.RequireFromString("400"), target,
	); got != "ask" {
		t.Fatalf("under target both signals=%s", got)
	}
	if got := selectArbitragePositionDirection(
		open, closeSpread, openTh, closeTh, decimal.RequireFromString("500"), target,
	); got != "bid" {
		t.Fatalf("at target both signals=%s", got)
	}
	if got := selectArbitragePositionDirection(
		open, closeSpread, openTh, closeTh, decimal.RequireFromString("620"), target,
	); got != "bid" {
		t.Fatalf("over target both signals=%s", got)
	}
	if got := selectArbitragePositionDirection(
		open, decimal.RequireFromString("1"), openTh, closeTh,
		decimal.RequireFromString("620"), target,
	); got != "" {
		t.Fatalf("over target open only=%s", got)
	}
}

func TestArbitrageCloseableBaseAndCommonStep(t *testing.T) {
	combination := ArbitrageCombination{
		LegAVenueBaselineBasePosition: "10",
		LegBVenueBaselineBasePosition: "-7",
		VenueBaselineCapturedAt:       time.Now().UTC(),
		LegABasePosition:              "-2",
		LegBBasePosition:              "1",
	}
	if got := arbitrageCloseableBase(combination); !got.Equal(decimal.NewFromInt(6)) {
		t.Fatalf("closeable=%s", got)
	}
	if got := commonQuantityStep(
		decimal.RequireFromString("0.2"),
		decimal.RequireFromString("0.3"),
	); !got.Equal(decimal.RequireFromString("0.6")) {
		t.Fatalf("common step=%s", got)
	}
	combination.LegBBasePosition = "8"
	if got := arbitrageCloseableBase(combination); !got.IsZero() {
		t.Fatalf("mixed direction closeable=%s", got)
	}
	combination.VenueBaselineCapturedAt = time.Time{}
	combination.LegABasePosition = "-20"
	combination.LegBBasePosition = "20"
	if got := arbitrageCloseableBase(combination); !got.Equal(decimal.NewFromInt(20)) {
		t.Fatalf("short closeable=%s", got)
	}
}

func TestArbitrageOwnedAndVenueCloseDirectionsUseDifferentPositions(t *testing.T) {
	combination := ArbitrageCombination{
		Status:                        "closing",
		LegABasePosition:              "5",
		LegBBasePosition:              "-5",
		LegAVenueBaselineBasePosition: "-100",
		LegBVenueBaselineBasePosition: "100",
		VenueBaselineCapturedAt:       time.Now().UTC(),
	}
	if got := arbitrageOwnedCloseableBase(combination); !got.Equal(decimal.NewFromInt(5)) {
		t.Fatalf("owned closeable=%s", got)
	}
	if direction, ok := arbitrageOwnedCloseDirection(combination); !ok || direction != "bid" {
		t.Fatalf("owned direction=%q ok=%v", direction, ok)
	}
	if direction, ok := arbitrageVenueReduceDirection(combination); !ok || direction != "ask" {
		t.Fatalf("venue direction=%q ok=%v", direction, ok)
	}
	if got := arbitrageCloseableBase(combination); !got.Equal(decimal.NewFromInt(95)) {
		t.Fatalf("venue closeable=%s", got)
	}

	combination.LegBBasePosition = "5"
	if _, ok := arbitrageOwnedCloseDirection(combination); ok {
		t.Fatal("same-direction owned legs must not have a close direction")
	}
}

func TestArbitrageFlatteningIncludesOneShotExiting(t *testing.T) {
	combination := ArbitrageCombination{
		Status: "running", RunMode: "one_shot", OneShotPhase: "exiting",
	}
	if !arbitrageFlattening(combination) {
		t.Fatal("one-shot exiting must be a flattening state")
	}
	combination.OneShotPhase = "exited"
	if arbitrageFlattening(combination) {
		t.Fatal("one-shot exited must not be a flattening state")
	}
}

func TestArbitrageExecutableBaseUsesActualOrderRules(t *testing.T) {
	instrument := func(id int64) Instrument {
		return Instrument{
			ID: id, Exchange: "binance", ContractType: "perpetual",
			QuantityStep: "0.1", PriceTick: "0.1",
			MinQuantity: "0.1", MinNotional: "1",
			MinQuantityStatus:  exchange.ConstraintKnown,
			MaxQuantityStatus:  exchange.ConstraintNotApplicable,
			MinNotionalStatus:  exchange.ConstraintKnown,
			MarketQuantityStep: "0.1", MarketMinQuantity: "0.1",
			MarketMinNotional:        "200",
			MarketQuantityStepStatus: exchange.ConstraintKnown,
			MarketMinQuantityStatus:  exchange.ConstraintKnown,
			MarketMaxQuantityStatus:  exchange.ConstraintNotApplicable,
			MarketMinNotionalStatus:  exchange.ConstraintKnown,
		}
	}
	combination := ArbitrageCombination{
		ExecutionMode: "simultaneous_market",
		LegA:          ArbitrageLeg{InstrumentID: 1, ContractType: "perpetual"},
		LegB:          ArbitrageLeg{InstrumentID: 2, ContractType: "perpetual"},
	}
	bbo := marketdata.BBO{BidPrice: "99.9", AskPrice: "100"}
	if _, _, ok := arbitrageExecutableBase(
		combination, instrument(1), instrument(2), bbo, bbo,
		"ask", "open", decimal.NewFromInt(100), false,
	); ok {
		t.Fatal("market minimum notional should block preflight")
	}
	combination.ExecutionMode = "maker_then_hedge"
	if got, _, ok := arbitrageExecutableBase(
		combination, instrument(1), instrument(2), bbo, bbo,
		"ask", "open", decimal.NewFromInt(100), false,
	); !ok || !got.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("limit preflight quantity=%s ok=%v", got, ok)
	}
}

func TestArbitrageExecutableBaseCloseSkipsMinNotional(t *testing.T) {
	instrument := lastClipCloseInstrument(1, "1", "5")
	combination := ArbitrageCombination{
		ExecutionMode:    "maker_then_hedge",
		MakerLeg:         "a",
		LegABasePosition: "20",
		LegBBasePosition: "-20",
		LegA:             ArbitrageLeg{InstrumentID: 1, ContractType: "perpetual"},
		LegB:             ArbitrageLeg{InstrumentID: 2, ContractType: "perpetual"},
	}
	bbo := lastClipCloseBBO()
	got, _, ok := arbitrageExecutableBase(
		combination, instrument, lastClipCloseInstrument(2, "1", "5"), bbo, bbo,
		"bid", "close", lastClipCloseable(), true,
	)
	if !ok || !got.Equal(decimal.NewFromInt(20)) {
		t.Fatalf("close skip quantity=%s ok=%v", got, ok)
	}
	if _, _, ok := arbitrageExecutableBase(
		combination, instrument, lastClipCloseInstrument(2, "1", "5"), bbo, bbo,
		"bid", "close", lastClipCloseable(), false,
	); ok {
		t.Fatal("closing without lastClip should honor minNotional")
	}
	if _, _, ok := arbitrageExecutableBase(
		combination, instrument, lastClipCloseInstrument(2, "1", "5"), bbo, bbo,
		"ask", "open", lastClipCloseable(), false,
	); ok {
		t.Fatal("open below minNotional should stay rejected")
	}
	if _, _, ok := arbitrageExecutableBase(
		combination, lastClipCloseInstrument(1, "50", "5"), lastClipCloseInstrument(2, "50", "5"),
		bbo, bbo, "bid", "close", lastClipCloseable(), true,
	); ok {
		t.Fatal("close should still honor minQty")
	}
}

func TestArbitragePositionNotionalsUseEachLegMid(t *testing.T) {
	capturedAt := time.Now().UTC()
	combination := ArbitrageCombination{
		LegAVenueBaselineBasePosition: "2",
		LegBVenueBaselineBasePosition: "-1",
		VenueBaselineCapturedAt:       capturedAt,
		LegABasePosition:              "3",
		LegBBasePosition:              "-4",
	}
	venue, combo, captured := arbitragePositionNotionals(
		combination,
		decimal.RequireFromString("100"),
		decimal.RequireFromString("110"),
	)
	if !captured || venue.String() != "500" || combo.String() != "300" {
		t.Fatalf("venue=%s combo=%s captured=%v", venue, combo, captured)
	}

	combination.LegAVenueBaselineBasePosition = "-2"
	combination.LegBVenueBaselineBasePosition = "2"
	combination.LegABasePosition = "0"
	combination.LegBBasePosition = "0"
	venue, combo, captured = arbitragePositionNotionals(
		combination,
		decimal.RequireFromString("100"),
		decimal.RequireFromString("110"),
	)
	if !captured || venue.String() != "-200" || !combo.IsZero() {
		t.Fatalf("reverse venue=%s combo=%s captured=%v", venue, combo, captured)
	}
}

func TestCapSpotReduceQuantity(t *testing.T) {
	step := decimal.RequireFromString("0.1")
	if got := capSpotReduceQuantity(
		decimal.RequireFromString("3"),
		decimal.RequireFromString("1.25"),
		"sell", step,
	); got.String() != "1.2" {
		t.Fatalf("sell cap=%s", got)
	}
	if got := capSpotReduceQuantity(
		decimal.RequireFromString("2"),
		decimal.RequireFromString("-0.65"),
		"buy", step,
	); got.String() != "0.6" {
		t.Fatalf("buy cap=%s", got)
	}
	if got := capSpotReduceQuantity(
		decimal.RequireFromString("2"),
		decimal.RequireFromString("-1"),
		"sell", step,
	); !got.IsZero() {
		t.Fatalf("wrong-direction cap=%s", got)
	}
}

func TestSignedPositionDelta(t *testing.T) {
	delta := signedPositionDelta(
		"ask",
		decimal.RequireFromString("2"),
		decimal.RequireFromString("1.5"),
		decimal.RequireFromString("100"),
	)
	if !delta.Equal(decimal.RequireFromString("150")) {
		t.Fatalf("ask delta=%s", delta)
	}
	delta = signedPositionDelta(
		"bid",
		decimal.RequireFromString("2"),
		decimal.RequireFromString("1.5"),
		decimal.RequireFromString("100"),
	)
	if !delta.Equal(decimal.RequireFromString("-150")) {
		t.Fatalf("bid delta=%s", delta)
	}
}

func TestMakerQuantityWithinCarryBudget(t *testing.T) {
	step := decimal.RequireFromString("1")
	capacity := decimal.RequireFromString("100")
	if got := makerQuantityWithinCarryBudget(
		decimal.RequireFromString("100"),
		decimal.RequireFromString("12"),
		"buy",
		capacity,
		step,
	); !got.Equal(decimal.RequireFromString("88")) {
		t.Fatalf("same-direction carry quantity=%s", got)
	}
	if got := makerQuantityWithinCarryBudget(
		decimal.RequireFromString("100"),
		decimal.RequireFromString("12"),
		"sell",
		capacity,
		step,
	); !got.Equal(decimal.RequireFromString("100")) {
		t.Fatalf("opposite-direction carry quantity=%s", got)
	}
}

func TestOpportunityStillValidByDirection(t *testing.T) {
	combination := ArbitrageCombination{
		AskThresholdBps: "90",
		BidThresholdBps: "-90",
	}
	legA := marketdata.BBO{BidPrice: "100", AskPrice: "101"}
	legB := marketdata.BBO{BidPrice: "99", AskPrice: "102"}
	if !opportunityStillValid(combination, legA, legB, "ask") {
		t.Fatal("expected ask opportunity")
	}
	if !opportunityStillValid(combination, legA, legB, "bid") {
		t.Fatal("expected bid opportunity")
	}
	combination.AskThresholdBps = "200"
	if opportunityStillValid(combination, legA, legB, "ask") {
		t.Fatal("unexpected ask opportunity")
	}
}

func TestOpportunityStillValidOneShotIgnoresThresholds(t *testing.T) {
	combination := ArbitrageCombination{
		RunMode:         "one_shot",
		AskThresholdBps: "1000",
		BidThresholdBps: "-1000",
	}
	legA := marketdata.BBO{BidPrice: "100", AskPrice: "101"}
	legB := marketdata.BBO{BidPrice: "99", AskPrice: "102"}
	if !opportunityStillValid(combination, legA, legB, "ask") {
		t.Fatal("one-shot ask should stay valid")
	}
	if !opportunityStillValid(combination, legA, legB, "bid") {
		t.Fatal("one-shot bid should stay valid")
	}
}

func TestOpportunityStillValidMakerACloseFollowsQuoteAsk(t *testing.T) {
	combination := ArbitrageCombination{
		ExecutionMode:   "maker_then_hedge",
		MakerLeg:        "a",
		AskThresholdBps: "1000",
		BidThresholdBps: "-120",
	}
	legA := marketdata.BBO{BidPrice: "100", AskPrice: "101"}
	legB := marketdata.BBO{BidPrice: "99", AskPrice: "99.5"}
	quoteAsk, quoteBid, ok := calculateArbitrageSpreads(
		decimal.RequireFromString("100"),
		decimal.RequireFromString("101"),
		decimal.RequireFromString("99"),
		decimal.RequireFromString("99.5"),
	)
	if !ok {
		t.Fatal("expected valid quote spreads")
	}
	if quoteAsk.GreaterThan(decimal.RequireFromString("-120")) {
		t.Fatalf("quoteAsk=%s should hit bid threshold", quoteAsk)
	}
	if quoteBid.LessThanOrEqual(decimal.RequireFromString("-120")) {
		t.Fatalf("quoteBid=%s should miss bid threshold", quoteBid)
	}
	if !opportunityStillValid(combination, legA, legB, "bid") {
		t.Fatal("maker A close should follow quoteAsk")
	}
	combination.MakerLeg = "b"
	if opportunityStillValid(combination, legA, legB, "bid") {
		t.Fatal("maker B close should follow quoteBid and miss")
	}
}

func TestProtectedIOCPrice(t *testing.T) {
	bbo := marketdata.BBO{BidPrice: "99.9", AskPrice: "100.1"}
	tick := decimal.RequireFromString("0.1")
	if got := protectedIOCPrice(bbo, "buy", tick, 10); got != "100.3" {
		t.Fatalf("buy=%s", got)
	}
	if got := protectedIOCPrice(bbo, "sell", tick, 10); got != "99.8" {
		t.Fatalf("sell=%s", got)
	}
}

func TestMakerRepricesAtTwoTicks(t *testing.T) {
	tick := decimal.RequireFromString("0.1")
	if makerShouldReprice(decimal.RequireFromString("0.19"), tick, 2) {
		t.Fatal("must not reprice below two ticks")
	}
	if !makerShouldReprice(decimal.RequireFromString("0.2"), tick, 2) {
		t.Fatal("must reprice at two ticks")
	}
	if got := passiveMakerPrice("100.19", "buy", tick); got != "100.1" {
		t.Fatalf("passive buy=%s", got)
	}
	if got := passiveMakerPrice("100.11", "sell", tick); got != "100.2" {
		t.Fatalf("passive sell=%s", got)
	}
}

func TestExecutionHasExposureIncludesUnconfirmedSubmittedIntent(t *testing.T) {
	if !executionHasExposure(ArbitrageExecution{
		Status: "maker_open", MakerOrderID: "order-1",
	}) {
		t.Fatal("submitted maker intent must be treated as potential exposure")
	}
	if executionHasExposure(ArbitrageExecution{
		Status: "failed", MakerOrderID: "order-1",
	}) {
		t.Fatal("definitively failed intent must not be treated as exposure")
	}
	if !executionHasExposure(ArbitrageExecution{
		Status: "reconciling", HedgeOrderID: "order-2",
	}) {
		t.Fatal("unknown hedge intent must be treated as potential exposure")
	}
}

func TestCloseFlattenTinyOwnedPositions(t *testing.T) {
	now := time.Now().UTC()
	bbo := marketdata.BBO{
		BidPrice: "1", AskPrice: "1", ReceiveTimestamp: now,
	}
	step := tinyOwnedTestInstrument("0.001")
	base := ArbitrageCombination{
		Status:                        "closing",
		LegA:                          ArbitrageLeg{ContractType: "perpetual"},
		LegB:                          ArbitrageLeg{ContractType: "perpetual"},
		VenueBaselineCapturedAt:       now,
		LastPositionReconciledAt:      now,
		LegAPositionDifference:        "0",
		LegBPositionDifference:        "0",
		LegAVenueBaselineBasePosition: "0",
		LegBVenueBaselineBasePosition: "0",
	}
	in := func(combo ArbitrageCombination, a, b marketdata.BBO) closeFlattenTinyOwnedInput {
		return closeFlattenTinyOwnedInput{
			Combination:    combo,
			InstrumentA:    step,
			InstrumentB:    step,
			BBOA:           a,
			BBOB:           b,
			Now:            now,
			SignalBBOStale: defaultSignalBBOStale,
		}
	}
	combo := base
	combo.LegABasePosition = "0"
	combo.LegBBasePosition = "-10"
	combo.LegAVenueBasePosition = "0"
	combo.LegBVenueBasePosition = "-10"
	if !closeFlattenTinyOwnedPositions(in(combo, bbo, bbo)) {
		t.Fatal("0/-10 under 20U should be tiny")
	}

	combo = base
	combo.LegABasePosition = "5"
	combo.LegBBasePosition = "10"
	combo.LegAVenueBasePosition = "5"
	combo.LegBVenueBasePosition = "10"
	if !closeFlattenTinyOwnedPositions(in(combo, bbo, bbo)) {
		t.Fatal("+5/+10 under 20U should be tiny")
	}

	combo = base
	combo.LegAVenueBasePosition = "0.1"
	combo.LegBVenueBasePosition = "-0.15"
	if closeFlattenTinyOwnedPositions(in(combo, bbo, bbo)) {
		t.Fatal("0/0 owned should not be tiny flatten")
	}

	combo = base
	combo.LegABasePosition = "0"
	combo.LegBBasePosition = "-10"
	combo.LegAVenueBasePosition = "0"
	combo.LegBVenueBasePosition = "-10"
	atThreshold := bbo
	atThreshold.BidPrice = "2"
	atThreshold.AskPrice = "2"
	if closeFlattenTinyOwnedPositions(in(combo, atThreshold, atThreshold)) {
		t.Fatal("exactly 20U should not be tiny")
	}
	above := bbo
	above.BidPrice = "2.1"
	above.AskPrice = "2.1"
	if closeFlattenTinyOwnedPositions(in(combo, above, above)) {
		t.Fatal("leftover above 20U should not be tiny")
	}

	diff := combo
	diff.LegBPositionDifference = "1"
	if closeFlattenTinyOwnedPositions(in(diff, bbo, bbo)) {
		t.Fatal("difference should reject")
	}
	missingBaseline := combo
	missingBaseline.VenueBaselineCapturedAt = time.Time{}
	if closeFlattenTinyOwnedPositions(in(missingBaseline, bbo, bbo)) {
		t.Fatal("missing baseline should reject")
	}
	staleSnap := combo
	staleSnap.LastPositionReconciledAt = now.Add(-3 * time.Minute)
	if closeFlattenTinyOwnedPositions(in(staleSnap, bbo, bbo)) {
		t.Fatal("stale snapshot should reject")
	}
	shortVenue := combo
	shortVenue.LegBVenueBasePosition = "-5"
	if closeFlattenTinyOwnedPositions(in(shortVenue, bbo, bbo)) {
		t.Fatal("insufficient venue delta should reject")
	}
	opposite := combo
	opposite.LegBVenueBasePosition = "10"
	if closeFlattenTinyOwnedPositions(in(opposite, bbo, bbo)) {
		t.Fatal("opposite venue delta should reject")
	}

	missingStep := in(combo, bbo, bbo)
	missingStep.InstrumentA = Instrument{}
	missingStep.InstrumentB = Instrument{}
	if closeFlattenTinyOwnedPositions(missingStep) {
		t.Fatal("missing instrument step should reject tiny")
	}

	lag := in(combo, bbo, bbo)
	lag.RequireOrders = true
	lag.LatestOrderAt = now.Add(time.Second)
	if closeFlattenTinyOwnedPositions(lag) {
		t.Fatal("snapshot lagging orders should reject tiny")
	}
	if !closeFlattenTinyOwnedWaitingSnapshot(lag) {
		t.Fatal("snapshot lagging orders should wait")
	}
	staleBBO := bbo
	staleBBO.ReceiveTimestamp = now.Add(-time.Minute)
	waitBBO := in(combo, bbo, staleBBO)
	if !closeFlattenTinyOwnedWaitingSnapshot(waitBBO) {
		t.Fatal("stale BBO should wait")
	}
	if closeFlattenBothPerpetual("perpetual", "spot") ||
		!closeFlattenBothPerpetual("perpetual", "perpetual") {
		t.Fatal("expected both-perpetual gate")
	}
	if closeFlattenSnapshotFresh(time.Time{}, now) ||
		closeFlattenSnapshotFresh(time.Unix(0, 0).UTC(), now) ||
		closeFlattenSnapshotFresh(now.Add(-3*time.Minute), now) ||
		!closeFlattenSnapshotFresh(now.Add(-time.Minute), now) {
		t.Fatal("unexpected snapshot freshness")
	}
}

func tinyOwnedTestInstrument(step string) Instrument {
	return Instrument{
		QuantityStep:             step,
		MarketQuantityStep:       step,
		MarketQuantityStepStatus: exchange.ConstraintKnown,
	}
}

func TestPositionDifferenceWithinMarketStep(t *testing.T) {
	step := decimal.RequireFromString("0.01")
	if !positionDifferenceWithinMarketStep(decimal.RequireFromString("3e-16"), step) {
		t.Fatal("3e-16 should be within 0.01 step")
	}
	if !positionDifferenceWithinMarketStep(decimal.RequireFromString("0.009999"), step) {
		t.Fatal("below one step should be within")
	}
	if positionDifferenceWithinMarketStep(decimal.RequireFromString("0.01"), step) {
		t.Fatal("exactly one step is material")
	}
	if positionDifferenceWithinMarketStep(decimal.RequireFromString("0.02"), step) {
		t.Fatal("two steps are material")
	}
	if positionDifferenceWithinMarketStep(decimal.RequireFromString("3e-16"), decimal.Zero) {
		t.Fatal("non-positive step must fail closed")
	}
}

func TestCloseFlattenTinyOwnedStepAwareDifference(t *testing.T) {
	now := time.Now().UTC()
	bbo := marketdata.BBO{BidPrice: "25", AskPrice: "25", ReceiveTimestamp: now}
	combo := ArbitrageCombination{
		Status:                        "closing",
		LegA:                          ArbitrageLeg{ContractType: "perpetual"},
		LegB:                          ArbitrageLeg{ContractType: "perpetual"},
		VenueBaselineCapturedAt:       now,
		LastPositionReconciledAt:      now,
		LegABasePosition:              "0.5",
		LegBBasePosition:              "-0.57",
		CarryBaseQuantity:             "-0.07",
		LegAVenueBasePosition:         "0.5",
		LegBVenueBasePosition:         "-0.57",
		LegAVenueBaselineBasePosition: "0",
		LegBVenueBaselineBasePosition: "0",
		LegAPositionDifference:        "0",
		LegBPositionDifference:        "3e-16",
	}
	known := tinyOwnedTestInstrument("0.01")
	fallback := Instrument{QuantityStep: "0.01"}
	gate := Instrument{
		Exchange: "gate", ContractSize: "10", QuantityStep: "0.01",
	}
	in := func(instrumentA, instrumentB Instrument) closeFlattenTinyOwnedInput {
		return closeFlattenTinyOwnedInput{
			Combination:    combo,
			InstrumentA:    instrumentA,
			InstrumentB:    instrumentB,
			BBOA:           bbo,
			BBOB:           bbo,
			Now:            now,
			SignalBBOStale: defaultSignalBBOStale,
		}
	}
	if !closeFlattenTinyOwnedPositions(in(known, known)) {
		t.Fatal("3e-16 vs 0.01 market step should be tiny")
	}
	if combo.LegBPositionDifference != "3e-16" {
		t.Fatalf("difference mutated: %s", combo.LegBPositionDifference)
	}
	if !closeFlattenTinyOwnedPositions(in(fallback, fallback)) {
		t.Fatal("missing market step should fall back to QuantityStep")
	}
	if !closeFlattenTinyOwnedPositions(in(gate, known)) {
		t.Fatal("gate contract size must not replace catalog QuantityStep")
	}

	material := combo
	material.LegBPositionDifference = "0.01"
	if closeFlattenTinyOwnedPositions(closeFlattenTinyOwnedInput{
		Combination: material, InstrumentA: known, InstrumentB: known,
		BBOA: bbo, BBOB: bbo, Now: now, SignalBBOStale: defaultSignalBBOStale,
	}) {
		t.Fatal("one step difference must reject tiny")
	}
	two := combo
	two.LegBPositionDifference = "0.02"
	if closeFlattenTinyOwnedPositions(closeFlattenTinyOwnedInput{
		Combination: two, InstrumentA: known, InstrumentB: known,
		BBOA: bbo, BBOB: bbo, Now: now, SignalBBOStale: defaultSignalBBOStale,
	}) {
		t.Fatal("two step difference must reject tiny")
	}
	if closeFlattenTinyOwnedPositions(in(Instrument{}, Instrument{})) {
		t.Fatal("missing market and QuantityStep must reject tiny")
	}

	gateMaterial := combo
	gateMaterial.LegAPositionDifference = "1"
	if closeFlattenTinyOwnedPositions(closeFlattenTinyOwnedInput{
		Combination: gateMaterial, InstrumentA: gate, InstrumentB: known,
		BBOA: bbo, BBOB: bbo, Now: now, SignalBBOStale: defaultSignalBBOStale,
	}) {
		t.Fatal("gate difference of 1 base must use QuantityStep 0.01, not contract size 10")
	}
}

func TestLastCloseClipMustMatchFills(t *testing.T) {
	combo := lastClipCloseCombination("bid")
	execution := ArbitrageExecution{
		LastCloseClip: true, PositionEffect: "close", ReduceOnly: true,
	}
	if !lastCloseClipMustMatchFills(combo, execution) {
		t.Fatal("paired last-clip close must match fills")
	}
	combo.Status = "running"
	combo.RunMode = ""
	combo.OneShotPhase = ""
	if lastCloseClipMustMatchFills(combo, execution) {
		t.Fatal("non-flattening last-clip must not require fill match")
	}
}

func TestCloseFlattenQuantityUsesMarketStep(t *testing.T) {
	instrument := Instrument{
		ContractType:             "perpetual",
		MarketQuantityStep:       "0.001",
		MarketQuantityStepStatus: exchange.ConstraintKnown,
		MarketMinQuantity:        "10",
		MarketMinQuantityStatus:  exchange.ConstraintKnown,
		MarketMinNotional:        "50",
		MarketMinNotionalStatus:  exchange.ConstraintKnown,
		MinQuantity:              "10",
		MinQuantityStatus:        exchange.ConstraintKnown,
		MinNotional:              "50",
		MinNotionalStatus:        exchange.ConstraintKnown,
	}
	quantity := closeFlattenQuantity(
		instrument, decimal.RequireFromString("0.0015"),
	)
	if quantity.String() != "0.001" {
		t.Fatalf("quantity=%s", quantity)
	}
	fallback := closeFlattenQuantity(
		Instrument{QuantityStep: "0.01"},
		decimal.RequireFromString("0.025"),
	)
	if fallback.String() != "0.02" {
		t.Fatalf("quantity-step fallback=%s", fallback)
	}
	if closeFlattenSide(decimal.RequireFromString("0.001")) != "sell" ||
		closeFlattenSide(decimal.RequireFromString("-0.002")) != "buy" {
		t.Fatal("unexpected flatten sides")
	}
	okx := exchange.Instrument{
		Exchange: "okx", ContractType: "perpetual", ContractSize: "0.001",
		QuantityStep: "0.001",
	}
	if _, err := exchange.ToVenueQuantity(okx, "0.0005"); err == nil {
		t.Fatal("expected fractional okx contract to fail preflight")
	}
	wire, err := exchange.ToVenueQuantity(okx, "0.002")
	if err != nil || wire != "2" {
		t.Fatalf("okx wire=%s err=%v", wire, err)
	}
}

func TestCircuitOpenCarryDustUsesPerLegExecutableQuantity(t *testing.T) {
	legA := Instrument{
		QuantityStep: "0.001", MinQuantity: "0.001",
		MinQuantityStatus: exchange.ConstraintKnown,
		MinNotional:       "0.1", MinNotionalStatus: exchange.ConstraintKnown,
	}
	legB := Instrument{
		QuantityStep: "0.01", MinQuantity: "0.01",
		MinQuantityStatus: exchange.ConstraintKnown,
		MinNotional:       "0.1", MinNotionalStatus: exchange.ConstraintKnown,
	}
	mark := decimal.RequireFromString("100")
	carry := decimal.RequireFromString("0.005")
	if circuitOpenCarryDust(carry, legA, legB, mark, mark) {
		t.Fatal("A-leg executable residual must not be dust")
	}
	tiny := Instrument{
		QuantityStep: "0.01", MinQuantity: "0.01",
		MinQuantityStatus: exchange.ConstraintKnown,
		MinNotional:       "0.1", MinNotionalStatus: exchange.ConstraintKnown,
	}
	if !circuitOpenCarryDust(carry, tiny, tiny, mark, mark) {
		t.Fatal("both ineligible legs should be dust")
	}
	unknown := Instrument{QuantityStep: "0.001"}
	if circuitOpenCarryDust(carry, unknown, tiny, mark, mark) {
		t.Fatal("unknown constraints must not be dust")
	}
	if !circuitOpenCarryDust(decimal.Zero, legA, legB, mark, mark) {
		t.Fatal("zero carry is balanced")
	}
}

func oneShotDustTestInstrument(step, minQty, minNotional string) Instrument {
	return Instrument{
		ContractType:      "perpetual",
		QuantityStep:      step,
		MinQuantity:       minQty,
		MinQuantityStatus: exchange.ConstraintKnown,
		MinNotional:       minNotional,
		MinNotionalStatus: exchange.ConstraintKnown,
	}
}

func oneShotDustTestCombo(phase, legA, legB, carry string) ArbitrageCombination {
	return ArbitrageCombination{
		Status: "running", RunMode: "one_shot", OneShotPhase: phase,
		TargetNotional:   "20",
		LegABasePosition: legA, LegBBasePosition: legB, CarryBaseQuantity: carry,
		LastPositionReconciledAt: time.Now().UTC(),
		LegA:                     ArbitrageLeg{ContractType: "perpetual"},
		LegB:                     ArbitrageLeg{ContractType: "perpetual"},
	}
}

func TestEvaluateOneShotDustIOSTCarryAccepted(t *testing.T) {
	instrument := oneShotDustTestInstrument("1", "1", "5")
	combo := oneShotDustTestCombo("building_target", "20000", "-19487", "513")
	verdict := evaluateOneShotDust(oneShotDustInput{
		Combination: combo, Remaining: decimal.Zero, Phase: "building_target",
		InstrumentA: instrument, InstrumentB: instrument,
		PriceA: decimal.RequireFromString("0.001"),
		PriceB: decimal.RequireFromString("0.001"),
	})
	if verdict != oneShotDustAccepted {
		t.Fatalf("verdict=%s", verdict)
	}
}

func TestEvaluateOneShotDustResidualFiveAcceptedTwentyUnsafe(t *testing.T) {
	instrument := oneShotDustTestInstrument("1", "1", "10")
	combo := oneShotDustTestCombo("exiting", "0", "-5", "-5")
	accepted := evaluateOneShotDust(oneShotDustInput{
		Combination: combo, Phase: "exiting",
		InstrumentA: instrument, InstrumentB: instrument,
		PriceB: decimal.NewFromInt(1),
	})
	if accepted != oneShotDustAccepted {
		t.Fatalf("5U verdict=%s", accepted)
	}
	combo20 := oneShotDustTestCombo("exiting", "0", "-20", "-20")
	if got := evaluateOneShotDust(oneShotDustInput{
		Combination: combo20, Phase: "exiting",
		InstrumentA: instrument, InstrumentB: instrument,
		PriceB: decimal.NewFromInt(1),
	}); got != oneShotDustUnsafe {
		t.Fatalf("20U verdict=%s", got)
	}
	combo50 := oneShotDustTestCombo("exiting", "0", "-50", "-50")
	if got := evaluateOneShotDust(oneShotDustInput{
		Combination: combo50, Phase: "exiting",
		InstrumentA: instrument, InstrumentB: instrument,
		PriceB: decimal.NewFromInt(1),
	}); got != oneShotDustUnsafe {
		t.Fatalf("50U verdict=%s", got)
	}
}

func TestEvaluateOneShotDustUnknownAndMissingPriceRetry(t *testing.T) {
	known := oneShotDustTestInstrument("1", "1", "5")
	unknown := Instrument{QuantityStep: "1", ContractType: "perpetual"}
	combo := oneShotDustTestCombo("exiting", "0", "-5", "-5")
	if got := evaluateOneShotDust(oneShotDustInput{
		Combination: combo, Phase: "exiting",
		InstrumentA: known, InstrumentB: unknown,
		PriceB: decimal.NewFromInt(1),
	}); got != oneShotDustRetry {
		t.Fatalf("unknown verdict=%s", got)
	}
	if got := evaluateOneShotDust(oneShotDustInput{
		Combination: combo, Phase: "exiting",
		InstrumentA: known, InstrumentB: known,
	}); got != oneShotDustRetry {
		t.Fatalf("missing price verdict=%s", got)
	}
}

func TestEvaluateOneShotDustCarryMismatchUnsafeWhenReady(t *testing.T) {
	instrument := oneShotDustTestInstrument("1", "1", "5")
	combo := oneShotDustTestCombo("exiting", "0", "-5", "0")
	if got := evaluateOneShotDust(oneShotDustInput{
		Combination: combo, Phase: "exiting",
		InstrumentA: instrument, InstrumentB: instrument,
		PriceB: decimal.NewFromInt(1),
	}); got != oneShotDustUnsafe {
		t.Fatalf("carry mismatch verdict=%s", got)
	}
}

func TestEvaluateOneShotDustGateFailureAndUnreconciledRetry(t *testing.T) {
	instrument := oneShotDustTestInstrument("1", "1", "5")
	combo := oneShotDustTestCombo("exiting", "0", "-5", "-5")
	if got := evaluateOneShotDust(oneShotDustInput{
		Combination: combo, Phase: "exiting",
		InstrumentA: instrument, InstrumentB: instrument,
		PriceB: decimal.NewFromInt(1), GateErr: errors.New("gate down"),
	}); got != oneShotDustRetry {
		t.Fatalf("gate error verdict=%s", got)
	}
	if got := evaluateOneShotDust(oneShotDustInput{
		Combination: combo, Phase: "exiting",
		InstrumentA: instrument, InstrumentB: instrument,
		PriceB: decimal.NewFromInt(1), Unreconciled: true,
	}); got != oneShotDustRetry {
		t.Fatalf("unreconciled verdict=%s", got)
	}
}
