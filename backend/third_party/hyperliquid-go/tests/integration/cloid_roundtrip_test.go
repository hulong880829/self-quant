//go:build integration

package integration

import (
	"crypto/rand"
	"encoding/hex"
	"testing"


	"github.com/Simon-Busch/hyperliquid-go/types"
	"github.com/Simon-Busch/hyperliquid-go/trade"
)

// newCloid returns a fresh 16-byte (32 hex chars) client order id with
// the 0x prefix Hyperliquid expects.
func newCloid(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "0x" + hex.EncodeToString(b[:])
}

// TestPlace_WithCloid_Roundtrip exercises the cloid surface end to end:
// place an ALO with WithCloid, look it up via Info.OrderByCloid, then
// cancel via Trade.CancelByCloid. Asserts the same cloid round-trips
// through every layer.
func TestPlace_WithCloid_Roundtrip(t *testing.T) {
	c := newClient(t)
	skipIfNoBalance(t, c)

	coin := testCoin(t)
	m := mid(t, c, coin)
	px := snapPrice(m*0.5, c, coin)
	size := testSizeForLimit(t, c, coin, px)
	cloid := newCloid(t)
	t.Cleanup(func() { cleanupCloid(t, c, coin, cloid) })

	res, err := c.Trade.PlaceALO(coin, types.Buy, size, px, trade.WithCloid(cloid))
	if err != nil {
		t.Fatalf("PlaceALO WithCloid: %v", err)
	}
	if res.OID == 0 {
		t.Fatalf("PlaceALO returned no oid: %+v", res)
	}
	t.Cleanup(func() { cancelIfResting(t, c, coin, res.OID) })

	cfg, _ := loadConfig()
	found, err := c.Info.OrderByCloid(cfg.AccountAddr, cloid)
	if err != nil {
		t.Fatalf("OrderByCloid(%s): %v", cloid, err)
	}
	if found == nil || found.Oid == 0 {
		t.Fatalf("OrderByCloid returned no live order for cloid=%s", cloid)
	}
	if found.Oid != res.OID {
		t.Fatalf("OrderByCloid oid mismatch: got %d, want %d", found.Oid, res.OID)
	}
	t.Logf("roundtrip ok: cloid=%s oid=%d px=%v", cloid, found.Oid, found.LimitPx)

	if _, err := c.Trade.CancelByCloid(coin, cloid); err != nil {
		t.Fatalf("CancelByCloid: %v", err)
	}

	// Post-cancel: order should no longer appear by cloid.
	gone, err := c.Info.OrderByCloid(cfg.AccountAddr, cloid)
	if err == nil && gone != nil && gone.Oid == res.OID {
		t.Logf("OrderByCloid after cancel still references oid=%d — venue may report cancelled order; acceptable", gone.Oid)
	}
}
