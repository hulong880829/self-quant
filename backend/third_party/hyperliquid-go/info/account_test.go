package info

import (
	"math"
	"testing"

	"github.com/Simon-Busch/hyperliquid-go/types"
)

func TestAssetAndAssetID(t *testing.T) {
	c := NewForTest(
		nil,
		map[string]int{"BTC": 0, "ETH": 1, "PURR/USDC": 10000},
		map[string]string{"BTC": "BTC", "ETH": "ETH", "PURR/USDC": "PURR/USDC"},
		map[int]int{0: 5, 1: 4, 10000: 2},
	)

	if got := c.AssetID("ETH"); got != 1 {
		t.Errorf("AssetID(ETH) = %d, want 1", got)
	}
	if got := c.AssetID("UNKNOWN"); got != 0 {
		// Unknown coins resolve through nameToCoin → "" → coinToAsset[""] = 0
		t.Errorf("AssetID(UNKNOWN) = %d, want 0", got)
	}

	// BTC: perp, szDecimals=5 → MinSize=1e-5, MaxPriceDecimals=6, so
	// allowed price decimals = 6-5 = 1 → TickSize = 0.1.
	btc, err := c.Asset("BTC")
	if err != nil {
		t.Fatalf("Asset(BTC) err: %v", err)
	}
	if btc.ID != 0 || btc.SzDecimals != 5 || btc.Class != types.AssetClassPerp {
		t.Errorf("Asset(BTC) = %+v", btc)
	}
	if math.Abs(btc.MinSize-1e-5) > 1e-12 {
		t.Errorf("BTC MinSize = %v, want 1e-5", btc.MinSize)
	}
	if math.Abs(btc.TickSize-0.1) > 1e-12 {
		t.Errorf("BTC TickSize = %v, want 0.1", btc.TickSize)
	}

	// PURR/USDC: spot, szDecimals=2 → MinSize=0.01, MaxPriceDecimals(spot)=8
	// so allowed = 8-2 = 6 → TickSize = 1e-6.
	purr, err := c.Asset("PURR/USDC")
	if err != nil {
		t.Fatalf("Asset(PURR/USDC) err: %v", err)
	}
	if purr.Class != types.AssetClassSpot {
		t.Errorf("PURR/USDC class = %v", purr.Class)
	}
	if math.Abs(purr.MinSize-0.01) > 1e-12 {
		t.Errorf("PURR/USDC MinSize = %v, want 0.01", purr.MinSize)
	}
	if math.Abs(purr.TickSize-1e-6) > 1e-12 {
		t.Errorf("PURR/USDC TickSize = %v, want 1e-6", purr.TickSize)
	}
}
