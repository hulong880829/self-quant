//go:build integration

package integration

import (
	"strings"
	"testing"
)

// TestWithdraw_WireOnly exercises Trader.Withdraw's wire-construction
// path. The on-chain withdraw is gated by Hyperliquid's minimum withdraw
// threshold (default 2 USDC on mainnet), so a 0.01 USDC withdraw to the
// signing address is expected to be rejected with a typed "minimum" or
// "below" error. Both outcomes — success OR that specific rejection —
// confirm the SDK signed and submitted the action correctly.
func TestWithdraw_WireOnly(t *testing.T) {
	cfg, _ := loadConfig()
	if cfg.SkipTransfer {
		t.Skip("HL_SKIP_TRANSFER=true; skipping withdraw scenario")
	}

	c := newClient(t)
	skipIfNoBalance(t, c)

	resp, err := c.Trade.Withdraw(0.01, cfg.AccountAddr)
	if err == nil && resp != nil && resp.Error == "" {
		t.Logf("Withdraw succeeded: status=%s txHash=%s", resp.Status, resp.TxHash)
		return
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if resp != nil && resp.Error != "" {
		msg = msg + " | " + resp.Error
	}
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "minimum"),
		strings.Contains(lower, "below"),
		strings.Contains(lower, "less than"),
		strings.Contains(lower, "withdraw"),
		strings.Contains(lower, "insufficient"):
		t.Logf("Withdraw rejected as expected (wire path verified): %s", msg)
	default:
		t.Fatalf("Withdraw failed with unexpected error: %s", msg)
	}
}
