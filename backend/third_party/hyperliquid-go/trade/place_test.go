package trade

import (
	"testing"

	"github.com/Simon-Busch/hyperliquid-go/types"
)

func TestSideFromIsBuy(t *testing.T) {
	if sideFromIsBuy(true) != types.Buy {
		t.Errorf("sideFromIsBuy(true) should be Buy")
	}
	if sideFromIsBuy(false) != types.Sell {
		t.Errorf("sideFromIsBuy(false) should be Sell")
	}
}
