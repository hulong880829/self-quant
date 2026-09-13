//go:build integration

package integration

import (
	"strconv"
	"testing"


	"github.com/Simon-Busch/hyperliquid-go/types"
)

// TestPlaceMarket opens a small market long, asserts the position
// appears in UserState, then closes it via ClosePosition and asserts the
// resulting position size is zero.
func TestPlaceMarket(t *testing.T) {
	c := newClient(t)
	skipIfNoBalance(t, c)

	coin := testCoin(t)
	size := testSize(t, c, coin)

	if _, err := c.Trade.PlaceMarket(coin, types.Buy, size); err != nil {
		t.Fatalf("PlaceMarket: %v", err)
	}
	t.Cleanup(func() { _, _ = c.Trade.ClosePosition(coin) })

	cfg, _ := loadConfig()
	pos, err := c.Info.Position(cfg.AccountAddr, coin)
	if err != nil {
		t.Fatalf("Position: %v", err)
	}
	if pos == nil {
		t.Fatalf("expected open position on %s after market buy", coin)
	}
	szi, _ := strconv.ParseFloat(pos.Szi, 64)
	if szi <= 0 {
		t.Fatalf("expected long position, szi=%v", szi)
	}

	if _, err := c.Trade.ClosePosition(coin); err != nil {
		t.Fatalf("ClosePosition: %v", err)
	}

	pos, err = c.Info.Position(cfg.AccountAddr, coin)
	if err != nil {
		t.Fatalf("Position post-close: %v", err)
	}
	if pos != nil {
		szi, _ := strconv.ParseFloat(pos.Szi, 64)
		if szi != 0 {
			t.Fatalf("expected zero position after close, got szi=%v", szi)
		}
	}
}
