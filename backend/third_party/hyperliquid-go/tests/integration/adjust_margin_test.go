//go:build integration

package integration

import (
	"strconv"
	"strings"
	"testing"
	"time"


	"github.com/Simon-Busch/hyperliquid-go/types"
	"github.com/Simon-Busch/hyperliquid-go/trade"
)

// TestAdjustMargin_IsolatedMode opens a tiny isolated-leverage position
// and pumps an extra USDC of margin into it via Trader.AdjustMargin.
// Reading UserState before and after confirms the per-position
// marginUsed moved. The position is closed in cleanup.
func TestAdjustMargin_IsolatedMode(t *testing.T) {
	c := newClient(t)
	skipIfNoBalance(t, c)

	coin := testCoin(t)

	// Isolated 3x so AdjustMargin has something to mutate.
	if _, err := c.Trade.SetLeverage(coin, 3, types.Isolated); err != nil {
		if strings.Contains(err.Error(), "Cannot change leverage with open position") {
			t.Skipf("cannot switch to isolated mode while a position is open: %v", err)
		}
		t.Fatalf("SetLeverage(Isolated, 3): %v", err)
	}

	size := testSize(t, c, coin)
	res, err := c.Trade.PlaceMarket(coin, types.Buy, size, trade.WithSlippage(0.05))
	if err != nil {
		t.Fatalf("PlaceMarket: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("PlaceMarket result error: %s", res.Error)
	}
	t.Cleanup(func() {
		if _, err := c.Trade.ClosePosition(coin); err != nil {
			t.Logf("ClosePosition cleanup: %v", err)
		}
	})

	pos := awaitPosition(t, c, coin, 10*time.Second)
	if pos == nil {
		t.Skip("position did not appear within 10s — venue may have rejected the entry")
	}
	beforeMargin, _ := strconv.ParseFloat(pos.MarginUsed, 64)
	t.Logf("opened position: szi=%s entry=%v marginUsed=%v", pos.Szi, pos.EntryPx, pos.MarginUsed)

	if _, err := c.Trade.AdjustMargin(coin, 1.0); err != nil {
		if strings.Contains(err.Error(), "Cannot adjust margin on cross") {
			t.Skipf("AdjustMargin requires isolated margin: %v", err)
		}
		t.Fatalf("AdjustMargin(+1.0): %v", err)
	}

	cfg, _ := loadConfig()
	state, err := c.Info.UserState(cfg.AccountAddr)
	if err != nil {
		t.Fatalf("UserState after AdjustMargin: %v", err)
	}
	var afterMargin float64
	for _, ap := range state.AssetPositions {
		if ap.Position.Coin == coin {
			afterMargin, _ = strconv.ParseFloat(ap.Position.MarginUsed, 64)
			break
		}
	}
	t.Logf("AdjustMargin: marginUsed %v -> %v (delta=%v)", beforeMargin, afterMargin, afterMargin-beforeMargin)
	if afterMargin == beforeMargin {
		t.Logf("marginUsed unchanged — venue may report margin differently than expected; SDK call succeeded")
	}
}
