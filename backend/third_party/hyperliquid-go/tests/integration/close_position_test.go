//go:build integration

package integration

import (
	"strconv"
	"testing"
	"time"


	"github.com/Simon-Busch/hyperliquid-go/types"
)

// TestClosePosition_AutoDirection opens a small long with PlaceMarket
// then calls ClosePosition with no side hint; the auto-direction logic
// must pick Sell. Final position size must be zero.
func TestClosePosition_AutoDirection(t *testing.T) {
	c := newClient(t)
	skipIfNoBalance(t, c)

	coin := testCoin(t)
	size := testSize(t, c, coin)

	if _, err := c.Trade.PlaceMarket(coin, types.Buy, size); err != nil {
		t.Fatalf("PlaceMarket buy: %v", err)
	}
	t.Cleanup(func() { _, _ = c.Trade.ClosePosition(coin) })

	// IOC market may not fill on thin books — skip rather than fail.
	if awaitPosition(t, c, coin, 5*time.Second) == nil {
		t.Skipf("PlaceMarket did not produce a position on %s within 5s (likely thin book); skipping", coin)
	}

	if _, err := c.Trade.ClosePosition(coin); err != nil {
		t.Fatalf("ClosePosition: %v", err)
	}

	cfg, _ := loadConfig()
	pos, err := c.Info.Position(cfg.AccountAddr, coin)
	if err != nil {
		t.Fatalf("Position: %v", err)
	}
	if pos != nil {
		szi, _ := strconv.ParseFloat(pos.Szi, 64)
		if szi != 0 {
			t.Fatalf("position not flat after ClosePosition: szi=%v", szi)
		}
	}
}
