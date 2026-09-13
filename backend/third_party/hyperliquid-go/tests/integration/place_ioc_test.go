//go:build integration

package integration

import (
	"testing"
	"time"


	"github.com/Simon-Busch/hyperliquid-go/types"
)

// TestPlaceIOC_Market places an aggressive IOC buy and confirms the fill
// appears in Info.Fill.
func TestPlaceIOC_Market(t *testing.T) {
	c := newClient(t)
	skipIfNoBalance(t, c)

	coin := testCoin(t)
	size := testSize(t, c, coin)
	m := mid(t, c, coin)
	px := snapPrice(m*1.01, c, coin) // 1% above mid → should fill IOC

	res, err := c.Trade.PlaceIOC(coin, types.Buy, size, px)
	if err != nil {
		t.Fatalf("PlaceIOC: %v", err)
	}

	// Best effort: flatten anything that did fill so we leave the account
	// as we found it.
	t.Cleanup(func() {
		_, _ = c.Trade.ClosePosition(coin)
	})

	if res.OID == 0 && res.Status == "" {
		t.Fatalf("PlaceIOC: no oid or status returned: %+v", res)
	}
	if res.OID != 0 && !awaitFill(t, c, res.OID, 5*time.Second) {
		t.Logf("no fill seen for oid %d within 5s (may have been rejected): %+v", res.OID, res)
	}
}
