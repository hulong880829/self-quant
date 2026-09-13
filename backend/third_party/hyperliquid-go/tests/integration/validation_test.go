//go:build integration

package integration

import (
	"errors"
	"testing"
	"time"


	"github.com/Simon-Busch/hyperliquid-go/types"
	"github.com/Simon-Busch/hyperliquid-go/trade"
)

// TestValidation_LongShortHardErrors opens a tiny long, then attempts a
// Buy reduce-only on the same coin. Validation must reject it with
// ValidationError{Code:"wrong_side_for_reduce"}. The tiny long is closed
// at test end. If PlaceMarket does not produce a position (thin book on
// testnet), the scenario skips rather than asserting the wrong-side
// check against an empty account.
func TestValidation_LongShortHardErrors(t *testing.T) {
	c := newClient(t)
	skipIfNoBalance(t, c)

	coin := testCoin(t)
	size := testSize(t, c, coin)

	if _, err := c.Trade.PlaceMarket(coin, types.Buy, size); err != nil {
		t.Fatalf("PlaceMarket buy: %v", err)
	}
	t.Cleanup(func() { _, _ = c.Trade.ClosePosition(coin) })

	if awaitPosition(t, c, coin, 5*time.Second) == nil {
		t.Skipf("PlaceMarket did not produce a position on %s within 5s (likely thin book); cannot exercise reduce-only validation", coin)
	}

	// Reduce-only Buy on a long must be rejected by validate().
	m := mid(t, c, coin)
	px := snapPrice(m*1.01, c, coin)
	_, err := c.Trade.PlaceIOC(coin, types.Buy, size, px, trade.WithReduceOnly())
	if err == nil {
		t.Fatalf("expected ValidationError wrong_side_for_reduce, got nil")
	}
	var ve *types.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	if ve.Code != "wrong_side_for_reduce" {
		t.Errorf("ValidationError.Code = %q, want %q", ve.Code, "wrong_side_for_reduce")
	}
}
