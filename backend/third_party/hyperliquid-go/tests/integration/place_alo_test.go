//go:build integration

package integration

import (
	"testing"


	"github.com/Simon-Busch/hyperliquid-go/types"
)

// TestPlaceALO_QueryAndCancel places a far-from-mid post-only limit order,
// asserts it shows up in OpenOrders, and cancels it. Cleanup cancels the
// order even when the assertions fail.
func TestPlaceALO_QueryAndCancel(t *testing.T) {
	c := newClient(t)
	skipIfNoBalance(t, c)

	coin := testCoin(t)
	m := mid(t, c, coin)
	px := snapPrice(m*0.5, c, coin) // 50% below mid → no risk of crossing
	size := testSizeForLimit(t, c, coin, px)

	res, err := c.Trade.PlaceALO(coin, types.Buy, size, px)
	if err != nil {
		t.Fatalf("PlaceALO: %v", err)
	}
	if res.OID == 0 {
		t.Fatalf("PlaceALO returned no OID: %+v", res)
	}
	t.Cleanup(func() { cancelIfResting(t, c, coin, res.OID) })

	cfg, _ := loadConfig()
	open, err := c.Info.OpenOrders(cfg.AccountAddr)
	if err != nil {
		t.Fatalf("OpenOrders: %v", err)
	}
	found := false
	for _, o := range open {
		if o.Oid == res.OID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("placed oid %d not in OpenOrders", res.OID)
	}

	if _, err := c.Trade.Cancel(coin, res.OID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
}
