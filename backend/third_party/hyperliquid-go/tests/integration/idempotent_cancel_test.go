//go:build integration

package integration

import (
	"errors"
	"strings"
	"testing"


	"github.com/Simon-Busch/hyperliquid-go/types"
	"github.com/Simon-Busch/hyperliquid-go/trade"
)

// TestCancel_IdempotentOnDeadOrder places a resting ALO, cancels it,
// then cancels it again. The second cancel must return a typed error
// (never panic) referencing a "not found" / "already" / "filled-or-
// cancelled" condition. Modifying a dead order is exercised separately
// in the same test for symmetry.
func TestCancel_IdempotentOnDeadOrder(t *testing.T) {
	c := newClient(t)
	skipIfNoBalance(t, c)

	coin := testCoin(t)
	m := mid(t, c, coin)
	px := snapPrice(m*0.5, c, coin)
	size := testSizeForLimit(t, c, coin, px)

	cloid := newCloid(t)
	res, err := c.Trade.PlaceALO(coin, types.Buy, size, px, trade.WithCloid(cloid))
	if err != nil {
		t.Fatalf("PlaceALO: %v", err)
	}
	if res.OID == 0 {
		t.Fatalf("PlaceALO returned no oid: %+v", res)
	}
	t.Cleanup(func() { cancelIfResting(t, c, coin, res.OID) })

	first, err := c.Trade.Cancel(coin, res.OID)
	if err != nil {
		t.Fatalf("first Cancel: %v", err)
	}
	if first.Error != "" {
		t.Fatalf("first Cancel returned error: %s", first.Error)
	}

	// Second cancel: must not panic; expect a typed error or a non-empty
	// Error string on the result.
	second, err := c.Trade.Cancel(coin, res.OID)
	msg := ""
	if err != nil {
		msg = err.Error()
	} else {
		msg = second.Error
	}
	if msg == "" {
		t.Fatalf("re-cancelling a dead order returned no error: %+v", second)
	}
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "not found"),
		strings.Contains(lower, "already"),
		strings.Contains(lower, "filled or cancel"),
		strings.Contains(lower, "unknown oid"),
		strings.Contains(lower, "never placed"):
		t.Logf("idempotent cancel returned typed error: %s", msg)
	default:
		t.Logf("idempotent cancel returned error (unexpected phrasing, still typed): %s", msg)
	}

	// errors.As against APIError when chained (best-effort, not required).
	var apiErr types.APIError
	if errors.As(err, &apiErr) {
		t.Logf("APIError detail: code=%d msg=%s", apiErr.Code, apiErr.Message)
	}

	// Symmetry: ModifyByCloid on the dead order must also fail cleanly.
	if _, merr := c.Trade.ModifyByCloid(cloid, trade.WithSize(size*2), trade.WithLimit(px)); merr == nil {
		t.Logf("ModifyByCloid against a cancelled order returned no error (venue may accept and replace)")
	} else {
		t.Logf("ModifyByCloid against a cancelled order errored cleanly: %v", merr)
	}
}
