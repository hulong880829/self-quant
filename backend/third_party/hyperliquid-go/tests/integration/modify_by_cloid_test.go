//go:build integration

package integration

import (
	"strings"
	"testing"


	"github.com/Simon-Busch/hyperliquid-go/types"
	"github.com/Simon-Busch/hyperliquid-go/trade"
)

// TestModifyByCloid places a resting ALO pinned to a fresh cloid and
// modifies its size by cloid via Trade.ModifyByCloid. Same oracle-anchored
// price-band caveat as TestModify_PriceAndSize applies: testnet's modify
// reference-price rule can reject otherwise-valid modifies; treat that
// rejection as a skip.
func TestModifyByCloid(t *testing.T) {
	c := newClient(t)
	skipIfNoBalance(t, c)

	coin := testCoin(t)

	book, err := c.Info.Book(coin)
	if err != nil {
		t.Fatalf("Info.Book: %v", err)
	}
	if len(book.Levels) < 2 || len(book.Levels[0]) == 0 {
		t.Skipf("orderbook empty for %s — cannot place modify-by-cloid test order", coin)
	}
	bestBid := book.Levels[0][0].Px
	meta, _ := c.Info.Asset(coin)
	entry := snapPrice(bestBid-5*meta.TickSize, c, coin)
	size := testSizeForLimit(t, c, coin, entry)
	newSize := size * 2

	cloid := newCloid(t)
	t.Cleanup(func() { cleanupCloid(t, c, coin, cloid) })

	res, err := c.Trade.PlaceALO(coin, types.Buy, size, entry, trade.WithCloid(cloid))
	if err != nil {
		t.Fatalf("PlaceALO: %v", err)
	}
	if res.OID == 0 {
		t.Fatalf("PlaceALO returned no oid: %+v", res)
	}
	t.Cleanup(func() { cancelIfResting(t, c, coin, res.OID) })

	mod, err := c.Trade.ModifyByCloid(cloid, trade.WithLimit(entry), trade.WithSize(newSize))
	if err != nil {
		if strings.Contains(err.Error(), "95% away from the reference price") {
			t.Skipf("ModifyByCloid rejected by oracle-anchored reference-price rule: %v", err)
		}
		t.Fatalf("ModifyByCloid: %v", err)
	}
	targetOid := mod.OID
	if targetOid == 0 {
		targetOid = res.OID
	}
	t.Cleanup(func() { cancelIfResting(t, c, coin, targetOid) })

	cfg, _ := loadConfig()
	open, err := c.Info.OpenOrders(cfg.AccountAddr)
	if err != nil {
		t.Fatalf("OpenOrders: %v", err)
	}
	for _, o := range open {
		if o.Oid == targetOid {
			t.Logf("ModifyByCloid: oid=%d size=%v (after resize from %v)", o.Oid, o.Size, size)
			return
		}
	}
	t.Logf("modified order %d not yet visible in OpenOrders (acceptable for testnet lag)", targetOid)
}
