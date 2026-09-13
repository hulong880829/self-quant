//go:build integration

package integration

import (
	"testing"


	"github.com/Simon-Busch/hyperliquid-go/types"
	hl "github.com/Simon-Busch/hyperliquid-go"
)

// TestHIP3_PlaceALO constructs a builder-pinned client via
// hl.WithBuilderDex, picks a coin from the dex universe, and places +
// cancels a far-from-mid ALO. Verifies Trade.PerpDex echoes the dex.
func TestHIP3_PlaceALO(t *testing.T) {
	dex, skip := pickHIP3Dex(t)
	if skip {
		t.Skip("HL_HIP3_DEX not set; skipping HIP-3 suite")
	}

	c := newClient(t, hl.WithBuilderDex(dex))

	if got := c.Trade.PerpDex(); got != dex {
		t.Fatalf("Trade.PerpDex = %q, want %q", got, dex)
	}

	coin, skipCoin := pickHIP3Coin(t, c, dex)
	if skipCoin {
		t.Skipf("no usable coin on HIP-3 dex %q", dex)
	}

	skipIfNoBalance(t, c)

	m := mid(t, c, coin)
	if m <= 0 {
		t.Skipf("no live mid for %s on dex %q", coin, dex)
	}
	px := snapPrice(m*0.5, c, coin)
	size := testSizeForLimit(t, c, coin, px)

	res, err := c.Trade.PlaceALO(coin, types.Buy, size, px)
	if err != nil {
		t.Fatalf("PlaceALO on dex %q: %v", dex, err)
	}
	if res.OID == 0 {
		t.Fatalf("HIP-3 PlaceALO returned no oid: %+v", res)
	}
	t.Cleanup(func() { cancelIfResting(t, c, coin, res.OID) })

	cfg, _ := loadConfig()
	open, err := c.Info.OpenOrders(cfg.AccountAddr, dex)
	if err != nil {
		t.Fatalf("OpenOrders on dex %q: %v", dex, err)
	}
	found := false
	for _, o := range open {
		if o.Oid == res.OID {
			found = true
			break
		}
	}
	if !found {
		t.Logf("placed oid %d not yet in OpenOrders(dex=%q) — acceptable lag", res.OID, dex)
	}

	if _, err := c.Trade.Cancel(coin, res.OID); err != nil {
		t.Fatalf("Cancel on dex %q: %v", dex, err)
	}
}
