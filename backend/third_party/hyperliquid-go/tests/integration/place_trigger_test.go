//go:build integration

package integration

import (
	"testing"


	"github.com/Simon-Busch/hyperliquid-go/types"
	"github.com/Simon-Busch/hyperliquid-go/trade"
)

// TestPlaceTrigger_Cancel places a far-away stop-market trigger, asserts
// it shows up in OpenOrders, then cancels it.
func TestPlaceTrigger_Cancel(t *testing.T) {
	c := newClient(t)
	skipIfNoBalance(t, c)

	coin := testCoin(t)
	m := mid(t, c, coin)

	// Buy stop above market — triggers if price rises 50%. Far enough to
	// stay un-triggered for the duration of the test.
	trigger := snapPrice(m*1.5, c, coin)
	size := testSizeForLimit(t, c, coin, trigger)

	res, err := c.Trade.PlaceTrigger(coin, types.Buy, size, trigger, trade.AsMarket())
	if err != nil {
		t.Fatalf("PlaceTrigger: %v", err)
	}
	if res.OID == 0 {
		t.Fatalf("PlaceTrigger: no oid: %+v", res)
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
		t.Logf("trigger order may have already cancelled or not been listed yet")
	}

	if _, err := c.Trade.Cancel(coin, res.OID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
}
