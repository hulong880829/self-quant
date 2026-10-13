package jev

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestRoundTripCostOneHundredUSDTTwelveBPS(t *testing.T) {
	got := RoundTripCost(decimal.NewFromInt(100), decimal.NewFromInt(12))
	want := decimal.RequireFromString("0.12")
	if !got.Equal(want) {
		t.Fatalf("round trip cost=%s want=%s", got, want)
	}
	half := HalfRoundTripCost(decimal.NewFromInt(100), decimal.NewFromInt(12))
	if !half.Equal(decimal.RequireFromString("0.06")) {
		t.Fatalf("half cost=%s", half)
	}
}

func TestRoundTripCostUsesTenThousandDivisor(t *testing.T) {
	got := RoundTripCost(decimal.RequireFromString("250.5"), decimal.RequireFromString("12.5"))
	want := decimal.RequireFromString("250.5").Mul(decimal.RequireFromString("12.5")).Div(decimal.NewFromInt(10000))
	if !got.Equal(want) {
		t.Fatalf("got=%s want=%s", got, want)
	}
}

func TestSharedBaseQtyTakesMinCapacity(t *testing.T) {
	qty, err := SharedBaseQty(decimal.NewFromInt(100), decimal.NewFromInt(50), decimal.NewFromInt(40))
	if err != nil {
		t.Fatal(err)
	}
	if !qty.Equal(decimal.NewFromInt(2)) {
		t.Fatalf("qty=%s", qty)
	}
	longQty := qty
	shortQty := qty.Neg()
	if !longQty.Add(shortQty).IsZero() {
		t.Fatalf("not delta-neutral long=%s short=%s", longQty, shortQty)
	}
}
