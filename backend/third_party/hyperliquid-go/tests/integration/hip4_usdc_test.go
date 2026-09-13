//go:build integration

package integration

import (
	"math"
	"strconv"
	"testing"

	hl "github.com/Simon-Busch/hyperliquid-go"
	"github.com/Simon-Busch/hyperliquid-go/trade"
	"github.com/Simon-Busch/hyperliquid-go/types"
)

// TestHIP4_BuyOutcomeWithUSDC proves the SDK can buy a HIP-4 outcome
// market that is collateralized and settled in USDC rather than USDH.
//
// HIP-4 shipped quoting in USDH; the venue has since migrated outcome
// markets to USDC as USDH is sunset. This test asserts the new world:
//
//  1. The chosen outcome reports QuoteToken == "USDC" (so the order pays
//     in USDC, not USDH).
//  2. Funding is checked against the USDC spot balance — NOT USDH.
//  3. A real market BUY clears the venue with no USDH dependency, and is
//     flattened on cleanup.
//
// Skips cleanly when the environment exposes no USDC-quoted outcome with
// a live book, or when the account lacks the small USDC budget.
func TestHIP4_BuyOutcomeWithUSDC(t *testing.T) {
	c := newClient(t)

	canonical, friendly, midPx := requireQuoteTokenOutcomeOrSkip(t, c, "USDC")
	t.Logf("USDC-quoted HIP-4 market: friendly=%q canonical=%q mid=%.6f", friendly, canonical, midPx)

	// Defense-in-depth: re-read the outcome's quote token from meta and
	// assert it before spending anything. This is the core guarantee —
	// the market settles in USDC, not USDH.
	if qt := quoteTokenFor(t, c, canonical); qt != "USDC" {
		t.Fatalf("expected QuoteToken=USDC for %s, got %q", canonical, qt)
	}

	// Funds must be in USDC. If this market still leaned on USDH the
	// order would need USDH headroom instead; requiring USDC here is what
	// makes "buy with USDC instead of USDH" a real assertion.
	skipIfSpotTokenBelow(t, c, "USDC", hip4VenueMinNotional+1)

	meta, err := c.Info.Asset(canonical)
	if err != nil {
		t.Fatalf("Info.Asset(%s): %v", canonical, err)
	}
	if meta.MinSize <= 0 {
		t.Fatalf("outcome %s reports MinSize=%v (expected > 0)", canonical, meta.MinSize)
	}

	// Smallest size clearing the venue's $10 minimum order value, guarded
	// against blowing past the per-test budget.
	target := hip4VenueMinNotional / midPx
	steps := math.Ceil(target / meta.MinSize)
	if steps < 1 {
		steps = 1
	}
	size := steps * meta.MinSize
	notional := size * midPx
	if notional > hip4MaxNotional {
		t.Skipf("venue minimum %.2f USDC exceeds test budget %.2f at price %.4f on %s — pick a lower-priced USDC outcome or raise the budget",
			notional, hip4MaxNotional, midPx, canonical)
	}

	cfg, _ := loadConfig()
	usdcBefore := spotBalance(t, c, cfg.AccountAddr, "USDC")
	t.Logf("placing market BUY size=%v contracts (~$%.2f USDC at %.6f); USDC before=%.4f",
		size, notional, midPx, usdcBefore)

	res, err := c.Trade.PlaceMarket(canonical, types.Buy, size, trade.WithSlippage(0.05))
	if err != nil {
		t.Fatalf("PlaceMarket buy (USDC outcome): %v", err)
	}
	if res.Error != "" {
		t.Fatalf("PlaceMarket rejected by venue: %s", res.Error)
	}
	t.Logf("buy ack: oid=%d status=%s avgPx=%s totalSz=%s", res.OID, res.Status, res.AvgPx, res.TotalSz)

	filledSize, err := strconv.ParseFloat(res.TotalSz, 64)
	if err != nil || filledSize <= 0 {
		t.Skipf("buy did not fill (totalSz=%q); cannot assert USDC was spent", res.TotalSz)
	}

	// Flatten on cleanup so the test leaves no residual exposure.
	t.Cleanup(func() {
		if _, err := c.Trade.PlaceMarket(canonical, types.Sell, filledSize, trade.WithSlippage(0.10)); err != nil {
			t.Logf("cleanup sell %v contracts: %v (best-effort)", filledSize, err)
		}
	})

	usdcAfter := spotBalance(t, c, cfg.AccountAddr, "USDC")
	t.Logf("USDC after buy=%.4f (delta=%+.4f)", usdcAfter, usdcAfter-usdcBefore)
	if usdcAfter >= usdcBefore {
		t.Errorf("expected USDC balance to drop after buying a USDC-quoted outcome; before=%.6f after=%.6f", usdcBefore, usdcAfter)
	}
}

// quoteTokenFor resolves the canonical "#<enc>" outcome name back to its
// QuoteToken via OutcomeMeta. enc = 10*outcome + sideIdx, so the outcome
// id is enc/10. Returns "" when the outcome can't be resolved.
func quoteTokenFor(t *testing.T, c *hl.Client, canonical string) string {
	t.Helper()
	if len(canonical) < 2 || canonical[0] != '#' {
		t.Fatalf("quoteTokenFor: expected canonical \"#<enc>\", got %q", canonical)
	}
	enc, err := strconv.Atoi(canonical[1:])
	if err != nil {
		t.Fatalf("quoteTokenFor: parse %q: %v", canonical, err)
	}
	outcomeID := enc / 10
	meta, err := c.Info.OutcomeMeta()
	if err != nil {
		t.Fatalf("quoteTokenFor: OutcomeMeta: %v", err)
	}
	for _, oc := range meta.Outcomes {
		if oc.Outcome == outcomeID {
			return oc.QuoteToken
		}
	}
	return ""
}
