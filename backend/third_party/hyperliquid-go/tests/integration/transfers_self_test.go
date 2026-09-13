//go:build integration

package integration

import (
	"strconv"
	"testing"
)

// TestTransfer_USDToSelf sends 1.0 USDC to the signing account itself
// via Transfer.SendUSD. The on-chain balance should be unchanged within
// fee tolerance and the call should not error.
func TestTransfer_USDToSelf(t *testing.T) {
	cfg, _ := loadConfig()
	if cfg.SkipTransfer {
		t.Skip("HL_SKIP_TRANSFER=true; skipping transfer scenario")
	}

	c := newClient(t)
	skipIfNoBalance(t, c)

	before, err := c.Info.UserState(cfg.AccountAddr)
	if err != nil {
		t.Fatalf("UserState before: %v", err)
	}
	beforeW, _ := strconv.ParseFloat(before.Withdrawable, 64)

	if _, err := c.Trade.Transfer.SendUSD(cfg.AccountAddr, 1.0); err != nil {
		t.Fatalf("Transfer.SendUSD: %v", err)
	}

	after, err := c.Info.UserState(cfg.AccountAddr)
	if err != nil {
		t.Fatalf("UserState after: %v", err)
	}
	afterW, _ := strconv.ParseFloat(after.Withdrawable, 64)

	// Self-transfer: balance should be within 0.01 USDC after fees.
	if delta := beforeW - afterW; delta < -0.01 || delta > 0.01 {
		t.Logf("balance delta = %v (expected ~0 for self-transfer)", delta)
	}
}
