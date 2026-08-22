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

func TestNormalizeArbitrageInputRejectsUnsafeThresholds(t *testing.T) {
	_, err := normalizeArbitrageInput(CreateArbitrageInput{
		Token: "token", IdempotencyKey: "request-123",
		LegATradingAccountID: 1, LegAInstrumentID: 11,
		LegBTradingAccountID: 2, LegBInstrumentID: 22,
		AskThresholdBps: "-1", BidThresholdBps: "1",
		TargetNotional: "1000", OrderNotional: "100", MaxDeltaNotional: "10",
		ExecutionMode: "simultaneous_market",
	})
	if err != ErrInvalidArgument {
		t.Fatalf("err=%v", err)
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
