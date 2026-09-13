//go:build integration

package integration

import (
	"testing"


	"github.com/Simon-Busch/hyperliquid-go/types"
)

// TestCancelAll places two resting limit orders on the test coin, calls
// CancelAll filtered by coin, and asserts the open-order set drops both.
func TestCancelAll(t *testing.T) {
	c := newClient(t)
	skipIfNoBalance(t, c)

	coin := testCoin(t)
	m := mid(t, c, coin)

	px1 := snapPrice(m*0.5, c, coin)
	px2 := snapPrice(m*0.55, c, coin)
	size := testSizeForLimit(t, c, coin, px1)

	r1, err := c.Trade.PlaceALO(coin, types.Buy, size, px1)
	if err != nil {
		t.Fatalf("PlaceALO 1: %v", err)
	}
	t.Cleanup(func() { cancelIfResting(t, c, coin, r1.OID) })
	r2, err := c.Trade.PlaceALO(coin, types.Buy, size, px2)
	if err != nil {
		t.Fatalf("PlaceALO 2: %v", err)
	}
	t.Cleanup(func() { cancelIfResting(t, c, coin, r2.OID) })

	if _, err := c.Trade.CancelAll(coin); err != nil {
		t.Fatalf("CancelAll: %v", err)
	}

	cfg, _ := loadConfig()
	open, err := c.Info.OpenOrders(cfg.AccountAddr)
	if err != nil {
		t.Fatalf("OpenOrders: %v", err)
	}
	for _, o := range open {
		if o.Oid == r1.OID || o.Oid == r2.OID {
			t.Fatalf("oid %d still resting after CancelAll", o.Oid)
		}
	}
}
