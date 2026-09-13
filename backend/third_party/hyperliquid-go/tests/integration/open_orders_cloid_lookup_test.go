//go:build integration

package integration

import (
	"strings"
	"testing"
)

// TestInfo_OrderByCloid_NotFound queries Info.OrderByCloid with a cloid
// that was never placed and asserts the SDK returns cleanly. The wire
// shape on a miss is environment-dependent — the venue may answer with
// either a typed error or a zero-value OpenOrder — so the test asserts
// "no panic, no live order" rather than a specific shape. The cloid
// shape used (0x + 32 hex chars) is the documented 16-byte form.
func TestInfo_OrderByCloid_NotFound(t *testing.T) {
	c := newClient(t)
	cfg, _ := loadConfig()
	cloid := "0x" + strings.Repeat("ab", 16)

	got, err := c.Info.OrderByCloid(cfg.AccountAddr, cloid)
	if err != nil {
		t.Logf("OrderByCloid not-found returned typed error: %v", err)
		return
	}
	if got == nil {
		t.Logf("OrderByCloid not-found returned (nil, nil)")
		return
	}
	if got.Oid != 0 {
		t.Fatalf("OrderByCloid for a never-placed cloid returned oid=%d (expected 0): %+v", got.Oid, got)
	}
	t.Logf("OrderByCloid not-found returned zero-value OpenOrder: coin=%q oid=%d", got.Coin, got.Oid)
}
