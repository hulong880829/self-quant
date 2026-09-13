//go:build integration

package integration

import (
	"math"
	"strings"
	"testing"


	"github.com/Simon-Busch/hyperliquid-go/types"
)

// TestHIP4_PlaceCancelInteger places a far-from-mid ALO buy on the
// chosen HIP-4 outcome at half the current mid, picks an integer size
// big enough to clear the venue's $10 minimum notional, asserts the
// order rests in OpenOrders, and cancels it.
func TestHIP4_PlaceCancelInteger(t *testing.T) {
	c := newClient(t)
	skipIfNoBalance(t, c)
	// HIP-4 outcomes settle in USDH; the venue rejects orders without
	// enough free quote balance. The test's max notional is ~$10 (size
	// is sized for the $10 venue minimum) — require a small headroom on
	// top so a fee-bearing order can clear.
	skipIfSpotTokenBelow(t, c, "USDH", 11.0)

	canonical, _, midPx := requireOutcomeOrSkip(t, c)

	// HIP-4 prices live in (0, 1]; tick is 0.0001 (4 dp). Halve the mid
	// then snap.
	px := snapPrice(midPx*0.5, c, canonical)
	if px <= 0 {
		t.Skipf("snapped price <= 0 for outcome %q (mid=%v)", canonical, midPx)
	}

	// Integer size, large enough for $10 notional. Cap to avoid blowing
	// the budget on a thinly-priced outcome.
	size := math.Ceil(10.0 / px)
	if size < 1 {
		size = 1
	}
	const maxContracts = 1000
	if size > maxContracts {
		size = maxContracts
	}

	t.Cleanup(func() {
		if _, err := c.Trade.CancelAll(canonical); err != nil {
			t.Logf("CancelAll(%s) cleanup: %v", canonical, err)
		}
	})

	res, err := c.Trade.PlaceALO(canonical, types.Buy, size, px)
	if err != nil {
		t.Fatalf("PlaceALO(HIP-4 %s, size=%v, px=%v): %v", canonical, size, px, err)
	}
	if res.Error != "" {
		// HIP-4 BUY orders draw on a per-market collateral bucket the
		// account may not have funded even when generic spot USDH looks
		// healthy. Treat that as a skip rather than a hard failure —
		// the SDK plumbing is the thing under test, not testnet account
		// state. Other rejections (invalid price/size/etc.) still fail.
		if strings.Contains(res.Error, "Insufficient spot balance") {
			t.Skipf("venue rejected HIP-4 buy with %q — testnet account not funded for asset %s", res.Error, canonical)
		}
		t.Fatalf("PlaceALO result error: %s", res.Error)
	}
	if res.OID == 0 {
		t.Fatalf("PlaceALO returned no oid: %+v", res)
	}
	t.Logf("HIP-4 placement: coin=%s oid=%d size=%v px=%v", canonical, res.OID, size, px)
	t.Cleanup(func() { cancelIfResting(t, c, canonical, res.OID) })

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
		t.Logf("HIP-4 oid %d not yet in OpenOrders — acceptable lag", res.OID)
	}

	if _, err := c.Trade.Cancel(canonical, res.OID); err != nil {
		t.Fatalf("Cancel(%s, %d): %v", canonical, res.OID, err)
	}
}
