package exchange

import (
	"errors"
	"testing"
)

func TestValidateVenueOrderRulesReduceOnlySkipsMinNotional(t *testing.T) {
	instrument := Instrument{
		QuantityStep:      "1",
		PriceTick:         "0.001",
		MinQuantity:       "1",
		MinNotional:       "5",
		MinQuantityStatus: ConstraintKnown,
		MinNotionalStatus: ConstraintKnown,
	}
	request := OrderRequest{
		Instrument: instrument,
		OrderType:  "limit",
		Quantity:   "20",
		Price:      "0.214",
		ReduceOnly: true,
	}
	if err := validateVenueOrderRules(request); err != nil {
		t.Fatalf("reduce-only below minNotional: %v", err)
	}
	request.ReduceOnly = false
	if err := validateVenueOrderRules(request); err == nil || !errors.Is(err, ErrInvalidQuantity) {
		t.Fatalf("open below minNotional err=%v", err)
	}
	request.ReduceOnly = true
	request.Quantity = "20.5"
	if err := validateVenueOrderRules(request); err == nil || !errors.Is(err, ErrInvalidQuantity) {
		t.Fatalf("quantity step err=%v", err)
	}
	request.Quantity = "20"
	request.Price = "0.2145"
	if err := validateVenueOrderRules(request); err == nil || !errors.Is(err, ErrRejected) {
		t.Fatalf("price tick err=%v", err)
	}
}
