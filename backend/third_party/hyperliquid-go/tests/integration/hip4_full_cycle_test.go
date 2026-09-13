//go:build integration

package integration

import (
	"math"
	"strconv"
	"testing"
	"time"


	"github.com/Simon-Busch/hyperliquid-go/types"
	"github.com/Simon-Busch/hyperliquid-go/trade"
	hl "github.com/Simon-Busch/hyperliquid-go"
)

// hip4CycleUSDC is the USDC we swap into USDH at the start of the cycle.
const hip4CycleUSDC = 15.0

// hip4CycleUSDHPerLeg is the USDH notional spent on each side (YES, NO).
// The venue minimum is $10 USDH, so this is the smallest legal leg.
const hip4CycleUSDHPerLeg = 10.0

// TestHIP4_FullCycle exercises the complete user-described HIP-4 flow on
// one outcome:
//
//	1. Convert $15 USDC -> USDH.
//	2. Find an outcome with live mids on both the YES (sideIdx 0) and NO
//	   (sideIdx 1) sides.
//	3. Buy YES at market for ~$10 USDH.
//	4. Hold 10s.
//	5. Sell the exact filled YES size back to flatten.
//	6. Buy NO at market for ~$10 USDH on the same outcome.
//	7. Hold 10s.
//	8. Sell the exact filled NO size back to flatten.
//
// The scenario is intentionally expensive — two round-trips at venue
// minimum size — and skips cleanly when the budget isn't available so
// it doesn't drain a thin wallet by accident. Cleanups defensively
// flatten any side that survives a fatal in the middle.
func TestHIP4_FullCycle(t *testing.T) {
	c := newClient(t)
	cfg, _ := loadConfig()

	usdcBefore := spotBalance(t, c, cfg.AccountAddr, "USDC")
	usdhBefore := spotBalance(t, c, cfg.AccountAddr, "USDH")
	t.Logf("starting balances: USDC=%.4f USDH=%.4f", usdcBefore, usdhBefore)

	// Step 1: USDC -> USDH. Skip if we cannot raise enough USDH for two
	// $10 legs (a single leg's USDH is returned on its sell, so 15 USDH
	// is enough for both sides sequentially).
	requiredUSDH := hip4CycleUSDC
	if usdhBefore < requiredUSDH {
		if usdcBefore < hip4CycleUSDC {
			t.Skipf("need %.2f USDC OR %.2f USDH to run the cycle; have USDC=%.4f USDH=%.4f",
				hip4CycleUSDC, requiredUSDH, usdcBefore, usdhBefore)
		}
		t.Logf("step 1: converting %.2f USDC -> USDH", hip4CycleUSDC)
		swap, err := c.Trade.Convert.USDCToUSDH(hip4CycleUSDC)
		if err != nil {
			t.Fatalf("Convert.USDCToUSDH(%.2f): %v", hip4CycleUSDC, err)
		}
		if swap.Error != "" {
			t.Fatalf("Convert rejected by venue: %s", swap.Error)
		}
		t.Logf("  swap ack: oid=%d filled=%s @ %s", swap.OID, swap.TotalSz, swap.AvgPx)

		// Give the venue a beat to settle the USDH credit before we
		// start spending it.
		time.Sleep(2 * time.Second)
		usdhAfterSwap := spotBalance(t, c, cfg.AccountAddr, "USDH")
		t.Logf("  USDH after swap: %.4f", usdhAfterSwap)
	} else {
		t.Logf("step 1 skipped: USDH balance %.4f already covers the cycle", usdhBefore)
	}

	// Step 2: find an outcome with live mids on both sides.
	yesCanonical, noCanonical, friendly, yesMid, noMid := findBothSidesOrSkip(t, c)
	t.Logf("step 2: market %s — YES=%s mid=%.6f, NO=%s mid=%.6f",
		friendly, yesCanonical, yesMid, noCanonical, noMid)

	// Run the YES leg, then the NO leg. Each leg is identical except
	// for the canonical name and the mid used for sizing.
	runHIP4Leg(t, c, "YES", yesCanonical, yesMid)

	// Settle window between sides.
	time.Sleep(2 * time.Second)

	runHIP4Leg(t, c, "NO", noCanonical, noMid)

	// Final accounting.
	usdcAfter := spotBalance(t, c, cfg.AccountAddr, "USDC")
	usdhAfter := spotBalance(t, c, cfg.AccountAddr, "USDH")
	t.Logf("final balances: USDC=%.4f USDH=%.4f (delta USDC=%+.4f USDH=%+.4f)",
		usdcAfter, usdhAfter, usdcAfter-usdcBefore, usdhAfter-usdhBefore)
}

// runHIP4Leg buys ~$10 USDH worth of the given outcome side at market,
// holds for 10 seconds, then sells the exact filled size back. The
// cleanup is defensive: if a fatal fires after the buy but before the
// sell, we still try to flatten.
func runHIP4Leg(t *testing.T, c *hl.Client, label, canonical string, midPx float64) {
	t.Helper()

	size := sizeForUSDHNotional(t, c, canonical, hip4CycleUSDHPerLeg, midPx)
	t.Logf("step %s.buy: %v contracts at ~%.6f (~$%.2f notional)",
		label, size, midPx, size*midPx)

	buy, err := c.Trade.PlaceMarket(canonical, types.Buy, size, trade.WithSlippage(0.05))
	if err != nil {
		t.Fatalf("PlaceMarket buy %s: %v", label, err)
	}
	if buy.Error != "" {
		t.Fatalf("buy %s rejected by venue: %s", label, buy.Error)
	}
	filled, _ := strconv.ParseFloat(buy.TotalSz, 64)
	if filled <= 0 {
		t.Fatalf("buy %s did not fill (totalSz=%q)", label, buy.TotalSz)
	}
	t.Logf("  ack: oid=%d filled=%v avgPx=%s", buy.OID, filled, buy.AvgPx)

	flattened := false
	t.Cleanup(func() {
		if flattened {
			return
		}
		if _, err := c.Trade.PlaceMarket(canonical, types.Sell, filled, trade.WithSlippage(0.10)); err != nil {
			t.Logf("cleanup sell %s %v: %v (best-effort)", label, filled, err)
		}
	})

	t.Logf("step %s.hold: 10s with %v contracts", label, filled)
	time.Sleep(10 * time.Second)

	t.Logf("step %s.sell: closing %v contracts at market", label, filled)
	sell, err := c.Trade.PlaceMarket(canonical, types.Sell, filled, trade.WithSlippage(0.10))
	if err != nil {
		t.Fatalf("PlaceMarket sell %s: %v", label, err)
	}
	if sell.Error != "" {
		t.Fatalf("sell %s rejected by venue: %s", label, sell.Error)
	}
	flattened = true
	sold, _ := strconv.ParseFloat(sell.TotalSz, 64)
	t.Logf("  ack: oid=%d sold=%v avgPx=%s", sell.OID, sold, sell.AvgPx)

	if sold < filled {
		t.Logf("  partial close: bought=%v sold=%v residual=%v contracts",
			filled, sold, filled-sold)
	}
}

// findBothSidesOrSkip walks Info.OutcomeMeta looking for the first
// outcome that has a live mid on BOTH SideSpecs[0] (YES) and
// SideSpecs[1] (NO). Skips cleanly when no such outcome is registered
// on the target environment.
func findBothSidesOrSkip(t *testing.T, c *hl.Client) (yesCanonical, noCanonical, friendly string, yesMid, noMid float64) {
	t.Helper()
	meta, err := c.Info.OutcomeMeta()
	if err != nil {
		t.Skipf("OutcomeMeta failed: %v", err)
	}
	if meta == nil || len(meta.Outcomes) == 0 {
		t.Skip("no HIP-4 outcomes available on this environment")
	}
	cfg, _ := loadConfig()
	for _, oc := range meta.Outcomes {
		if len(oc.SideSpecs) < 2 {
			continue
		}
		yc := "#" + strconv.Itoa(10*oc.Outcome+0)
		nc := "#" + strconv.Itoa(10*oc.Outcome+1)
		// Honour HL_HIP4_OUTCOME when set: must match the outcome's
		// canonical YES name or its question text.
		if cfg.HIP4Outcome != "" {
			if cfg.HIP4Outcome != yc && cfg.HIP4Outcome != oc.Name {
				continue
			}
		}
		ym, errY := c.Info.Mid(yc)
		nm, errN := c.Info.Mid(nc)
		if errY != nil || errN != nil {
			continue
		}
		if ym <= 0 || ym >= 1 || nm <= 0 || nm >= 1 {
			continue
		}
		return yc, nc, oc.Name, ym, nm
	}
	t.Skip("no HIP-4 outcome with live YES and NO mids found")
	return "", "", "", 0, 0
}

// sizeForUSDHNotional rounds the contract count UP to clear the
// venue's $10 USDH minimum at the supplied mid. HIP-4 contracts are
// integer-quantised (MinSize == 1 today, but the helper reads the
// step from asset metadata so it stays correct if HL changes that).
func sizeForUSDHNotional(t *testing.T, c *hl.Client, coin string, usdhNotional, midPx float64) float64 {
	t.Helper()
	meta, err := c.Info.Asset(coin)
	if err != nil {
		t.Fatalf("Asset(%s): %v", coin, err)
	}
	if meta.MinSize <= 0 {
		t.Fatalf("Asset(%s) has non-positive MinSize %v", coin, meta.MinSize)
	}
	target := usdhNotional / midPx
	steps := math.Ceil(target / meta.MinSize)
	if steps < 1 {
		steps = 1
	}
	return steps * meta.MinSize
}
