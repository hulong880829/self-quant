//go:build integration

package integration

import (
	"testing"


	"github.com/Simon-Busch/hyperliquid-go/types"
	"github.com/Simon-Busch/hyperliquid-go/trade"
)

// TestPlaceMany_Batch builds two far-from-mid ALO specs via trade.ALO and
// submits them as a single signed batch through Trade.PlaceMany. The
// test asserts both legs come back with OIDs and both rest in
// OpenOrders. Cleanup cancels every order on the test coin.
func TestPlaceMany_Batch(t *testing.T) {
	c := newClient(t)
	skipIfNoBalance(t, c)

	coin := testCoin(t)
	m := mid(t, c, coin)

	// Two buys far below mid — no risk of crossing, both should rest.
	px1 := snapPrice(m*0.5, c, coin)
	px2 := snapPrice(m*0.4, c, coin)
	size1 := testSizeForLimit(t, c, coin, px1)
	size2 := testSizeForLimit(t, c, coin, px2)

	t.Cleanup(func() {
		if _, err := c.Trade.CancelAll(coin); err != nil {
			t.Logf("CancelAll(%s) cleanup: %v", coin, err)
		}
	})

	batch, err := c.Trade.PlaceMany(
		trade.ALO(coin, types.Buy, size1, px1),
		trade.ALO(coin, types.Buy, size2, px2),
	)
	if err != nil {
		t.Fatalf("PlaceMany: %v", err)
	}
	if batch.Error != "" {
		t.Fatalf("PlaceMany batch error: %s", batch.Error)
	}
	if len(batch.Results) != 2 {
		t.Fatalf("PlaceMany returned %d results, want 2: %+v", len(batch.Results), batch)
	}

	for i, r := range batch.Results {
		if r.Error != "" {
			t.Fatalf("leg %d returned error: %s", i, r.Error)
		}
		if r.OID == 0 {
			t.Fatalf("leg %d returned no oid: %+v", i, r)
		}
	}

	cfg, _ := loadConfig()
	open, err := c.Info.OpenOrders(cfg.AccountAddr)
	if err != nil {
		t.Fatalf("OpenOrders: %v", err)
	}
	seen := map[int64]bool{}
	for _, o := range open {
		seen[o.Oid] = true
	}
	for i, r := range batch.Results {
		if !seen[r.OID] {
			t.Fatalf("batch leg %d oid=%d not in OpenOrders", i, r.OID)
		}
	}
}
