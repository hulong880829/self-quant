//go:build integration

package integration

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"
	"time"


	"github.com/Simon-Busch/hyperliquid-go/types"
	"github.com/Simon-Busch/hyperliquid-go/trade"
	hl "github.com/Simon-Busch/hyperliquid-go"
)

// hip4MaxNotional caps the test order value at the user's HIP-4 budget.
// HIP-4 markets settle in USDH (separate from the perp USDC wallet);
// keep the test well under the documented balance.
const hip4MaxNotional = 5.0

// hip4VenueMinNotional is Hyperliquid's enforced minimum order value
// for HIP-4 outcome markets. Below this the venue rejects orders with
// "Order must have minimum value of 10 USDH". Tests that need to
// actually place an order require the budget to be at least this much.
const hip4VenueMinNotional = 10.0

// TestHIP4_YesFlow exercises the end-to-end HIP-4 trading lifecycle on a
// single outcome: find a live YES-side market, take a long via market
// IOC, hold for 10 seconds, then sell the exact filled size back to
// flatten.
//
// HIP-4 contracts may not surface in UserState.AssetPositions the way
// perps do, so the test logs every state field that *could* hold the
// position (AssetPositions by canonical and friendly name, raw spot
// balances, raw user-state JSON keys) so it's obvious where the SDK
// needs to be extended if a key field is missing.
func TestHIP4_YesFlow(t *testing.T) {
	c := newClient(t)

	canonical, friendly, midPx := requireYesOutcomeOrSkip(t, c)
	t.Logf("HIP-4 YES market: friendly=%q canonical=%q mid=%.6f", friendly, canonical, midPx)

	meta, err := c.Info.Asset(canonical)
	if err != nil {
		t.Fatalf("Info.Asset(%s): %v", canonical, err)
	}
	if meta.MinSize <= 0 {
		t.Fatalf("HIP-4 outcome %s reports MinSize=%v (expected > 0)", canonical, meta.MinSize)
	}
	t.Logf("asset meta: id=%d szDecimals=%d minSize=%v tickSize=%v class=%v",
		meta.ID, meta.SzDecimals, meta.MinSize, meta.TickSize, meta.Class)

	// Hyperliquid enforces a $10 USDH minimum order value on HIP-4
	// markets. Pick the smallest size that clears the venue minimum,
	// then guard the test against blowing past the test budget — if the
	// venue minimum already exceeds the budget at this price, skip
	// with a clear message rather than fail.
	target := hip4VenueMinNotional / midPx
	steps := math.Ceil(target / meta.MinSize)
	if steps < 1 {
		steps = 1
	}
	size := steps * meta.MinSize
	notional := size * midPx
	if notional > hip4MaxNotional {
		t.Skipf("HIP-4 venue minimum %.2f USDH exceeds test budget %.2f at price %.4f on %s — top up the USDH wallet or pick a lower-priced outcome",
			notional, hip4MaxNotional, midPx, canonical)
	}
	t.Logf("placing market BUY size=%v contracts (~$%.2f USDH at %.6f)",
		size, size*midPx, midPx)

	cfg, _ := loadConfig()

	res, err := c.Trade.PlaceMarket(canonical, types.Buy, size, trade.WithSlippage(0.05))
	if err != nil {
		t.Fatalf("PlaceMarket buy: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("PlaceMarket rejected by venue: %s", res.Error)
	}
	t.Logf("order ack: oid=%d status=%s avgPx=%s totalSz=%s",
		res.OID, res.Status, res.AvgPx, res.TotalSz)

	filledSize, err := strconv.ParseFloat(res.TotalSz, 64)
	if err != nil || filledSize <= 0 {
		t.Skipf("buy did not fill (totalSz=%q); cannot exercise the hold-and-close flow", res.TotalSz)
	}

	closed := false
	t.Cleanup(func() {
		if closed {
			return
		}
		if _, err := c.Trade.PlaceMarket(canonical, types.Sell, filledSize, trade.WithSlippage(0.10)); err != nil {
			t.Logf("cleanup sell %v contracts: %v (best-effort)", filledSize, err)
		}
	})

	// Diagnostic snapshot: where does the HIP-4 position actually live
	// in the API response? Log every plausible field after the buy.
	logHIP4StateSnapshot(t, c, cfg.AccountAddr, canonical, friendly)

	// 10-second hold. Re-snapshot once mid-hold so we can see whether
	// the position becomes visible after propagation lag.
	t.Logf("holding 10s with %v contracts...", filledSize)
	time.Sleep(5 * time.Second)
	logHIP4StateSnapshot(t, c, cfg.AccountAddr, canonical, friendly)
	time.Sleep(5 * time.Second)

	// Close by selling the exact filled size at market.
	closeRes, err := c.Trade.PlaceMarket(canonical, types.Sell, filledSize, trade.WithSlippage(0.10))
	if err != nil {
		t.Fatalf("PlaceMarket sell (close): %v", err)
	}
	if closeRes.Error != "" {
		t.Fatalf("close sell rejected by venue: %s", closeRes.Error)
	}
	closed = true
	t.Logf("close ack: oid=%d status=%s avgPx=%s totalSz=%s",
		closeRes.OID, closeRes.Status, closeRes.AvgPx, closeRes.TotalSz)

	closedSize, _ := strconv.ParseFloat(closeRes.TotalSz, 64)
	if closedSize < filledSize {
		t.Logf("close did not fully flatten: bought=%v sold=%v (residual=%v on book)",
			filledSize, closedSize, filledSize-closedSize)
	}

	logHIP4StateSnapshot(t, c, cfg.AccountAddr, canonical, friendly)
}

// logHIP4StateSnapshot dumps every state shape that could possibly hold
// a HIP-4 position to the test log. Designed so the next run reveals
// exactly which field the SDK should be reading from.
func logHIP4StateSnapshot(t *testing.T, c *hl.Client, addr, canonical, friendly string) {
	t.Helper()

	// 1. Info.Position by canonical name (what awaitPosition uses).
	if pos, err := c.Info.Position(addr, canonical); err == nil && pos != nil {
		t.Logf("  Info.Position(canonical=%q) → szi=%s entry=%s",
			canonical, pos.Szi, deref(pos.EntryPx))
	} else {
		t.Logf("  Info.Position(canonical=%q) → nil (err=%v)", canonical, err)
	}

	// 2. Info.Position by friendly name (HL may key positions by either).
	if pos, err := c.Info.Position(addr, friendly); err == nil && pos != nil {
		t.Logf("  Info.Position(friendly=%q) → szi=%s entry=%s",
			friendly, pos.Szi, deref(pos.EntryPx))
	} else {
		t.Logf("  Info.Position(friendly=%q) → nil (err=%v)", friendly, err)
	}

	// 3. Full UserState — walk every AssetPosition.Coin to see what HL
	//    actually returns for HIP-4 entries.
	if state, err := c.Info.UserState(addr); err == nil {
		t.Logf("  UserState.AssetPositions: %d entries", len(state.AssetPositions))
		for i, ap := range state.AssetPositions {
			t.Logf("    [%d] coin=%q szi=%s entry=%s",
				i, ap.Position.Coin, ap.Position.Szi, deref(ap.Position.EntryPx))
		}
	} else {
		t.Logf("  UserState err: %v", err)
	}

	// 4. SpotBalances — HIP-4 holdings might be encoded as spot tokens.
	if spot, err := c.Info.SpotBalances(addr); err == nil {
		nonzero := 0
		for _, b := range spot.Balances {
			if v, _ := strconv.ParseFloat(b.Total, 64); v > 0 {
				t.Logf("    spot %q (token=%d): total=%s hold=%s entry=%s",
					b.Coin, b.Token, b.Total, b.Hold, b.EntryNtl)
				nonzero++
			}
		}
		t.Logf("  SpotBalances: %d nonzero (of %d total)", nonzero, len(spot.Balances))
	} else {
		t.Logf("  SpotBalances err: %v", err)
	}

	// 5. As a final fallback, fetch raw user-state JSON to see if there
	//    is an undocumented field (e.g. outcomePositions) we are not
	//    unmarshalling.
	if rawState, err := c.Info.UserState(addr); err == nil {
		if blob, jerr := json.Marshal(rawState); jerr == nil {
			t.Logf("  raw UserState (truncated): %s", truncateLog(blob, 600))
		}
	}
}

// deref returns the dereferenced value or "" for nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
