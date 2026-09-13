package trade

import (
	"math"
	"testing"

	"github.com/Simon-Busch/hyperliquid-go/info"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

// TestPriceToWireOutcomeDecimals verifies that an outcome asset with
// szDecimals=0 produces up to 6 decimal price formatting (capped at 5
// significant figures, which is the binding constraint for prices < 1).
// szDecimals=0 is empirically confirmed: the exchange rejected size=16.4
// with "Order has invalid size" but accepted size=16.
func TestPriceToWireOutcomeDecimals(t *testing.T) {
	const outcomeAssetBase = 100000000
	outcomeAsset := outcomeAssetBase + 40 // outcome 4, YES (the live mainnet market)
	infoC := info.NewForTest(
		nil,
		map[string]int{"#40": outcomeAsset},
		map[string]string{"#40": "#40"},
		map[int]int{outcomeAsset: 0},
	)

	cases := []struct {
		price float64
		want  string
	}{
		// 5 sig figs cap is the binding constraint for prices in (0, 1).
		// Values pulled from a real mainnet snapshot of asset #40.
		{0.66782, "0.66782"},
		{0.6689, "0.6689"},
		{0.5, "0.5"},
		{0.001, "0.001"}, // minimum
		{0.999, "0.999"}, // maximum (exchange bounds)
		// 5 sf of 0.7501234 → 0.75012
		{0.7501234, "0.75012"},
	}
	for _, tc := range cases {
		got, err := PriceToWire(tc.price, outcomeAsset, infoC, types.AssetClassOutcome)
		if err != nil {
			t.Errorf("PriceToWire(%f) error: %v", tc.price, err)
			continue
		}
		if got != tc.want {
			t.Errorf("PriceToWire(%f, outcome) = %q, want %q", tc.price, got, tc.want)
		}
	}
}

// TestFormatPriceToTickSize verifies the function applies the two Hyperliquid
// price constraints in order: (1) 5 significant figures, (2) max decimal
// places = MAX_DECIMALS - szDecimals.
func TestFormatPriceToTickSize(t *testing.T) {
	cases := []struct {
		name       string
		price      float64
		szDecimals int
		class      types.AssetClass
		want       float64
	}{
		// Perp/builder-perp: maxPriceDecimals = 6 - szDecimals
		// 5 sf of 50000.12345 → 50000; allowed=4 → 50000
		{"perp 50000.12345 sz2", 50000.12345, 2, types.AssetClassPerp, 50000},
		// 5 sf of 2500.123 → 2500.1; allowed=4 → 2500.1
		{"perp 2500.123 sz2", 2500.123, 2, types.AssetClassPerp, 2500.1},
		// 5 sf of 100.001 → 100.00; allowed=4 → 100
		{"perp 100.001 sz2", 100.001, 2, types.AssetClassPerp, 100},
		// Builder perp must behave identically to perp (the HIP-3 fix invariant)
		{"builder perp 50000.123 sz2", 50000.123, 2, types.AssetClassBuilderPerp, 50000},
		{"builder perp 100.001 sz2", 100.001, 2, types.AssetClassBuilderPerp, 100},
		// Spot: maxPriceDecimals = 8 - szDecimals
		// Previously zeroed-out due to broken sig-fig branch — now correct.
		// 5 sf of 0.123456789 → 0.12346; allowed=6 → 0.12346
		{"spot 0.123456789 sz2", 0.123456789, 2, types.AssetClassSpot, 0.12346},
		// 5 sf of 1.23456 → 1.2346; allowed=6 → 1.2346
		{"spot 1.23456 sz2", 1.23456, 2, types.AssetClassSpot, 1.2346},
		// Outcome (HIP-4): MaxPriceDecimals = 6, szDecimals = 0 (integer
		// contracts). 5 sig figs is the binding cap for prices in (0, 1).
		// Values pulled from the live mainnet binary BTC market.
		{"outcome 0.66782 sz0", 0.66782, 0, types.AssetClassOutcome, 0.66782},
		{"outcome 0.6689 sz0", 0.6689, 0, types.AssetClassOutcome, 0.6689},
		{"outcome 0.001 min sz0", 0.001, 0, types.AssetClassOutcome, 0.001},
		{"outcome 0.5 sz0", 0.5, 0, types.AssetClassOutcome, 0.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatPriceToTickSize(tc.price, tc.szDecimals, tc.class)
			// Compare with epsilon since we're dealing with floats
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("formatPriceToTickSize(%f, sz=%d, class=%v) = %v, want %v",
					tc.price, tc.szDecimals, tc.class, got, tc.want)
			}
		})
	}
}

// TestPriceToWireHIP3BugFix — the actual production-code regression test
// for the HIP-3 fix. With szDecimals=2, a builder-perp asset should now
// allow 4 price decimals (6-2), not 6 (8-2). Verifies via observable
// wire output, not enum lookup.
func TestPriceToWireHIP3BugFix(t *testing.T) {
	hip3Asset := 110005 // dex 1, idx 5; previously misclassified as spot
	infoC := info.NewForTest(nil, nil, nil, map[int]int{hip3Asset: 2})
	// 0.123456: with old (buggy) spot rules, allowed=6 → "0.12346"
	//           with new builder-perp rules, allowed=4 → "0.1235" (5 sf cap)
	got, err := PriceToWire(0.123456, hip3Asset, infoC, types.AssetClassBuilderPerp)
	if err != nil {
		t.Fatalf("PriceToWire error: %v", err)
	}
	// 5 sf of 0.123456 → 0.12346; allowed=4 → "0.1235"
	if got != "0.1235" {
		t.Errorf("HIP-3 builder perp PriceToWire(0.123456) = %q, want %q "+
			"(if got %q, the HIP-3 fix is broken — that was the old buggy spot behavior)",
			got, "0.1235", "0.12346")
	}
}

// TestPriceToWireBackwardsCompatPerpSpot ensures the signature change didn't
// regress perp/spot wire formatting.
func TestPriceToWireBackwardsCompatPerpSpot(t *testing.T) {
	// Perp BTC: szDecimals=5 → maxDecimals(perp)=6 → allowed=1
	perpAsset := 0
	infoC := info.NewForTest(nil, nil, nil, map[int]int{perpAsset: 5})
	got, err := PriceToWire(50000.123, perpAsset, infoC, types.AssetClassPerp)
	if err != nil {
		t.Fatalf("perp PriceToWire error: %v", err)
	}
	// 5 sig figs of 50000.123 → 50000; allowed=1 → "50000"
	if got != "50000" {
		t.Errorf("perp PriceToWire = %q, want %q", got, "50000")
	}

	// Spot: szDecimals=2 → maxDecimals(spot)=8 → allowed=6
	spotAsset := 10000
	info2 := info.NewForTest(nil, nil, nil, map[int]int{spotAsset: 2})
	got, err = PriceToWire(0.12345678, spotAsset, info2, types.AssetClassSpot)
	if err != nil {
		t.Fatalf("spot PriceToWire error: %v", err)
	}
	// 5 sig figs of 0.12345678 → 0.12346; allowed=6 → "0.12346"
	if got != "0.12346" {
		t.Errorf("spot PriceToWire = %q, want %q", got, "0.12346")
	}
}
