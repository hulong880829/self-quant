package types

import "testing"

func TestClassifyAsset(t *testing.T) {
	cases := []struct {
		name  string
		asset int
		want  AssetClass
	}{
		// Default perp range: 0..9_999
		{"perp BTC", 0, AssetClassPerp},
		{"perp ETH", 1, AssetClassPerp},
		{"perp upper boundary", 9999, AssetClassPerp},

		// Spot range: 10_000..99_999
		{"spot lower boundary", 10000, AssetClassSpot},
		{"spot PURR/USDC", 10000, AssetClassSpot},
		{"spot mid range", 50000, AssetClassSpot},
		{"spot upper boundary", 99999, AssetClassSpot},

		// Builder perp range: 100_000..99_999_999 (HIP-3)
		{"builder perp lower boundary", 100000, AssetClassBuilderPerp},
		// dex 1, idx 0 → 100000 + 1*10000 + 0 = 110000
		{"builder perp dex1 idx0", 110000, AssetClassBuilderPerp},
		// dex 1, idx 5 → 110005 (the previously-misclassified case)
		{"builder perp dex1 idx5", 110005, AssetClassBuilderPerp},
		{"builder perp upper boundary", 99999999, AssetClassBuilderPerp},

		// Outcome range: 100_000_000+ (HIP-4)
		{"outcome lower boundary", 100000000, AssetClassOutcome},
		// outcome 123, side 0 → 100_000_000 + 1230 = 100_001_230
		{"outcome 123 yes", 100001230, AssetClassOutcome},
		// outcome 123, side 1 → 100_000_000 + 1231 = 100_001_231
		{"outcome 123 no", 100001231, AssetClassOutcome},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyAsset(tc.asset)
			if got != tc.want {
				t.Errorf("ClassifyAsset(%d) = %v, want %v", tc.asset, got, tc.want)
			}
		})
	}
}

func TestMaxPriceDecimals(t *testing.T) {
	cases := []struct {
		class AssetClass
		want  int
	}{
		{AssetClassPerp, 6},
		{AssetClassBuilderPerp, 6},
		{AssetClassSpot, 8},
		{AssetClassOutcome, 6}, // empirically: 5 decimal max prices with szDecimals=1
	}

	for _, tc := range cases {
		got := tc.class.MaxPriceDecimals()
		if got != tc.want {
			t.Errorf("(%v).MaxPriceDecimals() = %d, want %d", tc.class, got, tc.want)
		}
	}
}

// TestHIP3MisclassificationFix is the regression test for the pre-existing bug
// where `isSpot := asset >= 10000` treated HIP-3 builder perps (>=100_000) as
// spot, giving them spot's 8-decimal max instead of perp's 6-decimal max.
func TestHIP3MisclassificationFix(t *testing.T) {
	hip3Asset := 110005 // dex 1, idx 5
	class := ClassifyAsset(hip3Asset)
	if class == AssetClassSpot {
		t.Fatalf("HIP-3 builder perp asset %d misclassified as spot", hip3Asset)
	}
	if class != AssetClassBuilderPerp {
		t.Fatalf("HIP-3 builder perp asset %d classified as %v, want AssetClassBuilderPerp", hip3Asset, class)
	}
	if got := class.MaxPriceDecimals(); got != 6 {
		t.Errorf("HIP-3 builder perp MaxPriceDecimals = %d, want 6 (was incorrectly 8 before fix)", got)
	}
}

func TestIsSpotLike(t *testing.T) {
	if !AssetClassSpot.IsSpotLike() {
		t.Error("AssetClassSpot.IsSpotLike() = false, want true")
	}
	for _, c := range []AssetClass{AssetClassPerp, AssetClassBuilderPerp, AssetClassOutcome} {
		if c.IsSpotLike() {
			t.Errorf("(%v).IsSpotLike() = true, want false", c)
		}
	}
}
