package exchange

import (
	"errors"
	"testing"
)

func TestOKXQuantityRoundTripUsesContractMultiplier(t *testing.T) {
	instrument := Instrument{
		Exchange: "okx", ContractType: "perpetual", ContractSize: "0.01",
		Metadata: map[string]any{"ctMult": "10"},
	}
	wire, err := ToVenueQuantity(instrument, "1.2")
	if err != nil || wire != "12" {
		t.Fatalf("wire=%s err=%v", wire, err)
	}
	base, err := FromVenueQuantity(instrument, wire)
	if err != nil || base != "1.2" {
		t.Fatalf("base=%s err=%v", base, err)
	}
	step, err := BaseQuantityStep(instrument, "1")
	if err != nil || step != "0.1" {
		t.Fatalf("step=%s err=%v", step, err)
	}
}

func TestGateQuantityRoundTripAndRejectsFractionalContracts(t *testing.T) {
	instrument := Instrument{
		Exchange: "gate", ContractType: "perpetual", ContractSize: "0.001",
	}
	wire, err := ToVenueQuantity(instrument, "0.005")
	if err != nil || wire != "5" {
		t.Fatalf("wire=%s err=%v", wire, err)
	}
	base, err := FromVenueQuantity(instrument, wire)
	if err != nil || base != "0.005" {
		t.Fatalf("base=%s err=%v", base, err)
	}
	if _, err := ToVenueQuantity(instrument, "0.0005"); err == nil {
		t.Fatal("expected fractional contract quantity to be rejected")
	}
}

func TestOKXQuantityAllowsFractionalContractsAtLotStep(t *testing.T) {
	instrument := Instrument{
		Exchange: "okx", ContractType: "perpetual", ContractSize: "10",
		QuantityStep: "1",
	}
	wire, err := ToVenueQuantity(instrument, "154")
	if err != nil || wire != "15.4" {
		t.Fatalf("wire=%s err=%v", wire, err)
	}
	base, err := FromVenueQuantity(instrument, wire)
	if err != nil || base != "154" {
		t.Fatalf("base=%s err=%v", base, err)
	}
	if _, err := ToVenueQuantity(instrument, "154.5"); !errors.Is(err, ErrInvalidQuantity) {
		t.Fatalf("expected invalid quantity, got %v", err)
	}
}

func TestBaseQuantityPassThroughForNativeBaseVenues(t *testing.T) {
	for _, venue := range []string{"binance", "bybit", "bitget"} {
		instrument := Instrument{Exchange: venue, ContractType: "perpetual", ContractSize: "100"}
		wire, err := ToVenueQuantity(instrument, "0.123")
		if err != nil || wire != "0.123" {
			t.Fatalf("%s wire=%s err=%v", venue, wire, err)
		}
	}
}
