package trade

import (
	"testing"

	"github.com/Simon-Busch/hyperliquid-go/types"
)

// applyOpts runs every supplied PlaceOpt against a fresh OrderSpec and
// returns the resulting value. Mirrors how the placement constructors
// assemble a spec.
func applyOpts(opts ...PlaceOpt) types.OrderSpec {
	var s types.OrderSpec
	for _, o := range opts {
		o(&s)
	}
	return s
}

func TestWithTakeProfitStopLossBracket(t *testing.T) {
	s := applyOpts(WithTakeProfit(110), WithStopLoss(90))
	if s.TakeProfit != 110 || s.StopLoss != 90 {
		t.Errorf("WithTakeProfit/WithStopLoss got %+v", s)
	}
	s = applyOpts(WithBracket(120, 80))
	if s.TakeProfit != 120 || s.StopLoss != 80 {
		t.Errorf("WithBracket got %+v", s)
	}
}

func TestWithReduceOnlyAndCloid(t *testing.T) {
	s := applyOpts(WithReduceOnly(), WithCloid("0xabc"))
	if !s.ReduceOnly {
		t.Errorf("WithReduceOnly did not set field")
	}
	if s.Cloid != "0xabc" {
		t.Errorf("WithCloid got %q", s.Cloid)
	}
}

func TestWithBuilderAndSlippage(t *testing.T) {
	s := applyOpts(WithBuilder("0xbuilder", 25), WithSlippage(0.07))
	if s.BuilderAddr != "0xbuilder" || s.BuilderFeeBps != 25 {
		t.Errorf("WithBuilder got %+v", s)
	}
	if s.Slippage != 0.07 {
		t.Errorf("WithSlippage got %v", s.Slippage)
	}
}

func TestWithSizeAndWithLimit(t *testing.T) {
	s := applyOpts(WithSize(0.25), WithLimit(101.5))
	if s.OverrideSize != 0.25 {
		t.Errorf("WithSize got %v", s.OverrideSize)
	}
	if s.LimitPrice != 101.5 {
		t.Errorf("WithLimit got %v", s.LimitPrice)
	}
}

func TestAsMarketAndAsLimit(t *testing.T) {
	s := applyOpts(AsMarket())
	if !s.IsMarket {
		t.Errorf("AsMarket did not set IsMarket")
	}
	s = applyOpts(AsLimit(123))
	if s.IsMarket {
		t.Errorf("AsLimit must clear IsMarket")
	}
	if s.Price != 123 {
		t.Errorf("AsLimit price = %v, want 123", s.Price)
	}
}

func TestBracketLegOpts(t *testing.T) {
	s := applyOpts(
		WithTPSize(0.5),
		WithSLSize(0.25),
		WithTPCloid("0xtp"),
		WithSLCloid("0xsl"),
	)
	if s.TPSize != 0.5 || s.SLSize != 0.25 {
		t.Errorf("bracket sizes got %+v", s)
	}
	if s.TPCloid != "0xtp" || s.SLCloid != "0xsl" {
		t.Errorf("bracket cloids got %+v", s)
	}
}

func TestSkipValidationOpt(t *testing.T) {
	s := applyOpts(SkipValidation())
	if !s.SkipValidate {
		t.Errorf("SkipValidation did not set SkipValidate")
	}
}

func TestOptionMatrix_MethodCompatibility(t *testing.T) {
	// Sanity matrix: feed validateOptionCompatibility every (method, option)
	// pair from the spec and assert the rejection set.
	type row struct {
		name   string
		spec   types.OrderSpec
		reject bool
	}
	rows := []row{
		{"alo+slippage", types.OrderSpec{Method: "alo", Slippage: 0.01}, true},
		{"ioc+slippage", types.OrderSpec{Method: "ioc", Slippage: 0.01}, true},
		{"gtc+slippage", types.OrderSpec{Method: "gtc", Slippage: 0.01}, true},
		{"trigger+slippage", types.OrderSpec{Method: "trigger", Slippage: 0.01}, true},
		{"market+slippage", types.OrderSpec{Method: "market", Slippage: 0.01}, false},
		{"close+slippage", types.OrderSpec{Method: "close", Slippage: 0.01}, false},

		{"alo+overrideSize", types.OrderSpec{Method: "alo", OverrideSize: 1}, true},
		{"gtc+overrideSize", types.OrderSpec{Method: "gtc", OverrideSize: 1}, true},
		{"close+overrideSize", types.OrderSpec{Method: "close", OverrideSize: 1}, false},
		{"modify+overrideSize", types.OrderSpec{Method: "modify", OverrideSize: 1}, false},

		{"alo+limitPrice", types.OrderSpec{Method: "alo", LimitPrice: 1}, true},
		{"close+limitPrice", types.OrderSpec{Method: "close", LimitPrice: 1}, false},
		{"modify+limitPrice", types.OrderSpec{Method: "modify", LimitPrice: 1}, false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			err := validateOptionCompatibility(&r.spec)
			if r.reject && err == nil {
				t.Errorf("%s: expected unsupported_option, got nil", r.name)
			}
			if !r.reject && err != nil {
				t.Errorf("%s: expected nil, got %v", r.name, err)
			}
		})
	}
}
