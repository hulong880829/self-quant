//go:build integration

package integration

import (
	"strconv"
	"testing"
)

// TestHIP3_MoveToFromDex round-trips 1 USDC between the default perp
// wallet and the HIP-3 dex via Trade.Transfer.MoveToDex /
// Trade.Transfer.MoveFromDex. The dex-pinned UserState is queried
// before and after to log the shift.
func TestHIP3_MoveToFromDex(t *testing.T) {
	dex, skip := pickHIP3Dex(t)
	if skip {
		t.Skip("HL_HIP3_DEX not set; skipping HIP-3 suite")
	}
	cfg, _ := loadConfig()
	if cfg.SkipTransfer {
		t.Skip("HL_SKIP_TRANSFER=true; skipping HIP-3 transfer scenario")
	}

	c := newClient(t)
	skipIfNoBalance(t, c)

	beforeDef, err := c.Info.UserState(cfg.AccountAddr)
	if err != nil {
		t.Fatalf("UserState (default) before: %v", err)
	}
	beforeDex, err := c.Info.UserState(cfg.AccountAddr, dex)
	if err != nil {
		t.Logf("UserState (dex=%q) before: %v (acceptable: dex may not have a wallet yet)", dex, err)
	}

	if _, err := c.Trade.Transfer.MoveToDex(dex, "USDC", 1.0); err != nil {
		t.Fatalf("Transfer.MoveToDex(%q, USDC, 1.0): %v", dex, err)
	}

	if _, err := c.Trade.Transfer.MoveFromDex(dex, "USDC", 1.0); err != nil {
		t.Fatalf("Transfer.MoveFromDex(%q, USDC, 1.0): %v", dex, err)
	}

	afterDef, err := c.Info.UserState(cfg.AccountAddr)
	if err != nil {
		t.Fatalf("UserState (default) after: %v", err)
	}
	a, _ := strconv.ParseFloat(beforeDef.MarginSummary.AccountValue, 64)
	b, _ := strconv.ParseFloat(afterDef.MarginSummary.AccountValue, 64)
	t.Logf("HIP-3 MoveTo/From round-trip: default accountValue %v -> %v", a, b)
	if beforeDex != nil {
		afterDex, err := c.Info.UserState(cfg.AccountAddr, dex)
		if err == nil && afterDex != nil {
			t.Logf("HIP-3 dex %q accountValue: before=%s after=%s",
				dex, beforeDex.MarginSummary.AccountValue, afterDex.MarginSummary.AccountValue)
		}
	}
}
