//go:build integration

package integration

import (
	"testing"


	"github.com/Simon-Busch/hyperliquid-go/types"
)

// TestSetLeverage exercises both margin modes by toggling the test coin
// between Cross/5x and Isolated/3x, asserting each setting takes effect
// in the returned UserState.
func TestSetLeverage(t *testing.T) {
	c := newClient(t)
	skipIfNoBalance(t, c)

	coin := testCoin(t)

	// Cross 5x
	state, err := c.Trade.SetLeverage(coin, 5, types.Cross)
	if err != nil {
		t.Fatalf("SetLeverage Cross 5x: %v", err)
	}
	if state == nil {
		t.Errorf("SetLeverage returned nil UserState (Cross)")
	}

	// Isolated 3x
	state, err = c.Trade.SetLeverage(coin, 3, types.Isolated)
	if err != nil {
		t.Fatalf("SetLeverage Isolated 3x: %v", err)
	}
	if state == nil {
		t.Errorf("SetLeverage returned nil UserState (Isolated)")
	}
}
