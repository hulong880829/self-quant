//go:build integration

package integration

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"


	"github.com/Simon-Busch/hyperliquid-go/types"
	"github.com/Simon-Busch/hyperliquid-go/info"
	"github.com/Simon-Busch/hyperliquid-go/trade"
	hl "github.com/Simon-Busch/hyperliquid-go"
)

// hip3CycleUSDC is the USDC notional we move into the builder dex and
// spend on the test position. \$11 sits just above Hyperliquid's \$10
// minimum order value with a small slippage cushion.
const hip3CycleUSDC = 11.0

// TestHIP3_ShortFullCycle exercises the user-described HIP-3 flow on a
// single builder-deployed perpetual:
//
//	1. Build a client pinned to the HIP-3 dex (HL_HIP3_DEX, e.g. "xyz").
//	2. Resolve the coin under test (HL_HIP3_COIN, e.g. "COPPER").
//	3. Ensure the dex-pinned perp wallet has at least \$11 of headroom;
//	   if not, move USDC in from the main perp account.
//	4. SHORT the coin at market for ~\$10 notional.
//	5. Hold 10s.
//	6. Close the short via ClosePosition (auto-direction picks Buy).
//	7. Assert the dex-pinned position is flat.
//
// Skips cleanly when HL_HIP3_DEX is unset, when no coin can be picked,
// or when neither the dex wallet nor the main perp wallet has the
// budget. HIP-3 builder perps are real perps — unlike HIP-4 outcome
// contracts they DO surface in UserState.AssetPositions when queried
// with the dex parameter, so awaitPosition / ClosePosition behave
// the same as on the default perp dex.
func TestHIP3_ShortFullCycle(t *testing.T) {
	dex, skip := pickHIP3Dex(t)
	if skip {
		t.Skip("HL_HIP3_DEX not set; skipping HIP-3 full-cycle scenario")
	}

	// Build a client pinned to the HIP-3 dex. Without this option the
	// place path would route to the default perp dex's namespace and
	// silently look up the wrong asset id.
	c := newClient(t, hl.WithBuilderDex(dex))

	coin, skip := pickHIP3Coin(t, c, dex)
	if skip {
		t.Skipf("no HIP-3 coin available on dex %q; set HL_HIP3_COIN or check dex universe", dex)
	}

	cfg, _ := loadConfig()
	t.Logf("HIP-3 dex=%q coin=%q account=%s", dex, coin, cfg.AccountAddr)

	// Verify dex-pinned account value. UserState(addr, dex) reports the
	// margin summary scoped to the HIP-3 dex, separate from the default
	// perp wallet.
	dexState, err := c.Info.UserState(cfg.AccountAddr, dex)
	if err != nil {
		t.Fatalf("Info.UserState(%s, %q): %v", cfg.AccountAddr, dex, err)
	}
	dexValue, _ := strconv.ParseFloat(dexState.MarginSummary.AccountValue, 64)
	t.Logf("dex account value: %.4f USDC (withdrawable=%s)", dexValue, dexState.Withdrawable)

	// If the dex wallet is short, attempt to move USDC in from the main
	// perp account. This is non-destructive (same beneficial owner) but
	// requires the main wallet to have the funds.
	movedIn := 0.0
	if dexValue < hip3CycleUSDC {
		mainState, err := c.Info.UserState(cfg.AccountAddr)
		if err != nil {
			t.Fatalf("Info.UserState(%s): %v", cfg.AccountAddr, err)
		}
		mainValue, _ := strconv.ParseFloat(mainState.MarginSummary.AccountValue, 64)
		mainWith, _ := strconv.ParseFloat(mainState.Withdrawable, 64)
		t.Logf("main perp account value=%.4f withdrawable=%.4f", mainValue, mainWith)
		need := hip3CycleUSDC - dexValue
		if mainWith < need {
			t.Skipf("dex wallet %.4f + main withdrawable %.4f cannot cover %.2f cycle budget; top up the account",
				dexValue, mainWith, hip3CycleUSDC)
		}
		t.Logf("step 0: moving %.4f USDC main perp -> dex %q", need, dex)
		if _, err := c.Trade.Transfer.MoveToDex(dex, "USDC", need); err != nil {
			t.Fatalf("MoveToDex: %v", err)
		}
		movedIn = need
		// Settle window.
		time.Sleep(2 * time.Second)
	}

	// Best-effort cleanup: if we moved USDC into the dex for this test,
	// move it back at the end so a re-run starts from the same place.
	if movedIn > 0 {
		t.Cleanup(func() {
			if _, err := c.Trade.Transfer.MoveFromDex(dex, "USDC", movedIn); err != nil {
				t.Logf("cleanup MoveFromDex: %v (best-effort)", err)
			}
		})
	}

	// Step 1: size the short to ~$10 notional at current mid.
	midPx, err := c.Info.Mid(coin)
	if err != nil || midPx <= 0 {
		t.Fatalf("Info.Mid(%q) on dex %q: %v (mid=%v)", coin, dex, err, midPx)
	}
	meta, err := c.Info.Asset(coin)
	if err != nil {
		t.Fatalf("Info.Asset(%q): %v", coin, err)
	}
	if meta.MinSize <= 0 {
		t.Fatalf("Asset(%q) has MinSize=%v (expected > 0)", coin, meta.MinSize)
	}
	target := hip3CycleUSDC / midPx
	steps := math.Ceil(target / meta.MinSize)
	if steps < 1 {
		steps = 1
	}
	size := steps * meta.MinSize
	t.Logf("step 1: SHORT %s for %v contracts at ~%.6f (~$%.2f notional)",
		coin, size, midPx, size*midPx)

	// Step 2: short = Sell at market.
	short, err := c.Trade.PlaceMarket(coin, types.Sell, size, trade.WithSlippage(0.05))
	if err != nil {
		t.Fatalf("PlaceMarket Sell %s: %v", coin, err)
	}
	if short.Error != "" {
		if strings.Contains(short.Error, "minimum value") {
			t.Skipf("HIP-3 short rejected: %s — increase hip3CycleUSDC or pick a higher-priced coin", short.Error)
		}
		t.Fatalf("short rejected by venue: %s", short.Error)
	}
	filled, _ := strconv.ParseFloat(short.TotalSz, 64)
	if filled <= 0 {
		t.Skipf("short did not fill on %s (totalSz=%q); thin book on dex %q", coin, short.TotalSz, dex)
	}
	t.Logf("  ack: oid=%d filled=%v avgPx=%s", short.OID, filled, short.AvgPx)

	// Defensive flatten if anything fatals before the close.
	flattened := false
	t.Cleanup(func() {
		if flattened {
			return
		}
		if _, err := c.Trade.ClosePosition(coin); err != nil {
			t.Logf("cleanup ClosePosition: %v (best-effort)", err)
		}
	})

	// Step 3: wait for the short to register in dex-pinned UserState.
	pos := awaitPositionOnDex(t, c, cfg.AccountAddr, coin, dex, 5*time.Second)
	if pos == nil {
		t.Skipf("short did not surface as a position within 5s; venue may be lagging or coin not in dex universe")
	}
	szi, _ := strconv.ParseFloat(pos.Szi, 64)
	entry := ""
	if pos.EntryPx != nil {
		entry = *pos.EntryPx
	}
	t.Logf("position opened: szi=%v entry=%s unrealizedPnl=%s (negative szi = short)",
		szi, entry, pos.UnrealizedPnl)
	if szi >= 0 {
		t.Errorf("expected short (szi < 0), got szi=%v", szi)
	}

	// Step 4: hold.
	t.Log("step 4: holding short 10s")
	time.Sleep(10 * time.Second)

	// Step 5: close. ClosePosition auto-direction reads the current
	// position and picks the opposite side — Buy in this case.
	closeRes, err := c.Trade.ClosePosition(coin)
	if err != nil {
		t.Fatalf("ClosePosition: %v", err)
	}
	flattened = true
	t.Logf("close ack: oid=%d status=%s avgPx=%s totalSz=%s",
		closeRes.OID, closeRes.Status, closeRes.AvgPx, closeRes.TotalSz)

	// Step 6: confirm the dex-pinned position is flat.
	final, err := c.Info.UserState(cfg.AccountAddr, dex)
	if err != nil {
		t.Fatalf("final UserState: %v", err)
	}
	for _, ap := range final.AssetPositions {
		if ap.Position.Coin == coin {
			fsz, _ := strconv.ParseFloat(ap.Position.Szi, 64)
			if fsz != 0 {
				t.Errorf("position not flat after ClosePosition on %s: szi=%v", coin, fsz)
			}
		}
	}
	t.Logf("final dex account value: %s USDC", final.MarginSummary.AccountValue)
}

// awaitPositionOnDex polls Info.UserState(addr, dex) until the named
// coin shows a non-zero position or the timeout elapses. The HIP-3
// equivalent of awaitPosition — the default perp helper does not pass
// the dex parameter, so it would not see HIP-3 positions.
func awaitPositionOnDex(t *testing.T, c *hl.Client, addr, coin, dex string, timeout time.Duration) *info.Position {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := c.Info.UserState(addr, dex)
		if err == nil {
			for _, ap := range state.AssetPositions {
				if ap.Position.Coin == coin {
					szi, _ := strconv.ParseFloat(ap.Position.Szi, 64)
					if szi != 0 {
						pos := ap.Position
						return &pos
					}
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil
}
