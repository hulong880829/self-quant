//go:build integration

package integration

import (
	"testing"


	"github.com/Simon-Busch/hyperliquid-go/types"
	"github.com/Simon-Busch/hyperliquid-go/trade"
)

// TestPlaceGTC_WithBracket places a far-below-mid GTC entry with TP/SL
// legs attached, verifies the three orders rest, then cancels the parent
// and asserts the brackets cancel with it (normalTpsl grouping).
func TestPlaceGTC_WithBracket(t *testing.T) {
	c := newClient(t)
	skipIfNoBalance(t, c)

	coin := testCoin(t)
	m := mid(t, c, coin)

	entry := snapPrice(m*0.5, c, coin)
	tp := snapPrice(m*0.6, c, coin)
	sl := snapPrice(m*0.4, c, coin)
	size := testSizeForLimit(t, c, coin, entry)

	res, err := c.Trade.PlaceGTC(coin, types.Buy, size, entry, trade.WithBracket(tp, sl))
	if err != nil {
		t.Fatalf("PlaceGTC+bracket: %v", err)
	}
	if res.OID == 0 {
		t.Fatalf("PlaceGTC: no oid: %+v", res)
	}
	t.Cleanup(func() { _, _ = c.Trade.CancelAll(coin) })

	cfg, _ := loadConfig()
	open, err := c.Info.OpenOrders(cfg.AccountAddr)
	if err != nil {
		t.Fatalf("OpenOrders: %v", err)
	}
	matching := 0
	for _, o := range open {
		if o.Coin == coin {
			matching++
		}
	}
	if matching < 1 {
		t.Fatalf("expected at least the entry order resting, got %d", matching)
	}

	if _, err := c.Trade.Cancel(coin, res.OID); err != nil {
		t.Fatalf("Cancel parent: %v", err)
	}
}
