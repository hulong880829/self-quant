package trader

import (
	"testing"

	"github.com/shopspring/decimal"
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
		TargetNotional: "10000", OrderNotional: "500",
		ExecutionMode: "MAKER_THEN_HEDGE", MakerLeg: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if input.MakerLeg != "a" || input.MaxDeltaNotional != "500" ||
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
				TargetNotional: "1000", OrderNotional: "100", MaxDeltaNotional: "10",
				ExecutionMode: "simultaneous_market",
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
		name      string
		position  string
		direction string
		wantOK    bool
		effect    string
		reduce    bool
		size      string
	}{
		{name: "zero ask opens", position: "0", direction: "ask", wantOK: true, effect: "open", size: "3000"},
		{name: "zero bid opens", position: "0", direction: "bid", wantOK: true, effect: "open", size: "3000"},
		{name: "ask clips remaining", position: "8000", direction: "ask", wantOK: true, effect: "open", size: "2000"},
		{name: "ask at cap blocked", position: "10000", direction: "ask", wantOK: false},
		{name: "bid closes positive", position: "2500", direction: "bid", wantOK: true, effect: "close", reduce: true, size: "2500"},
		{name: "ask closes negative", position: "-1800", direction: "ask", wantOK: true, effect: "close", reduce: true, size: "1800"},
		{name: "bid clips remaining short", position: "-8000", direction: "bid", wantOK: true, effect: "open", size: "2000"},
		{name: "bid at cap blocked", position: "-10000", direction: "bid", wantOK: false},
		{name: "bid does not cross zero", position: "500", direction: "bid", wantOK: true, effect: "close", reduce: true, size: "500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, ok := planArbitragePosition(
				decimal.RequireFromString(tc.position), target, order, tc.direction,
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

func TestProtectedIOCPrice(t *testing.T) {
	bbo := marketdata.BBO{BidPrice: "99.9", AskPrice: "100.1"}
	tick := decimal.RequireFromString("0.1")
	if got := protectedIOCPrice(bbo, "buy", tick, 3); got != "100.4" {
		t.Fatalf("buy=%s", got)
	}
	if got := protectedIOCPrice(bbo, "sell", tick, 3); got != "99.6" {
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
