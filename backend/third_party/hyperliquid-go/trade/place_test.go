package trade

import (
	"testing"

	"github.com/Simon-Busch/hyperliquid-go/info"
	xtransport "github.com/Simon-Busch/hyperliquid-go/internal/transport"
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

// skrTestInfo registers SKR at a stable asset id >= 3 with szDecimals=0 so
// formatPriceToTickSize can keep six price decimals (perp MaxPriceDecimals=6).
func skrTestInfo(t *testing.T, baseURL string) *info.Client {
	t.Helper()
	if baseURL == "" {
		baseURL = "http://127.0.0.1"
	}
	return info.NewForTest(
		xtransport.New(baseURL, nil),
		map[string]int{"SKR": 10},
		map[string]string{"SKR": "SKR"},
		map[int]int{10: 0},
	)
}

func TestSlippagePriceSKRKeepsFiveSigFigs(t *testing.T) {
	client := NewForTest(xtransport.New("http://127.0.0.1", nil), skrTestInfo(t, ""), nil, "")
	mid := 0.018003

	buy, err := client.SlippagePrice("SKR", true, 0.05, &mid)
	if err != nil {
		t.Fatal(err)
	}
	if buy != 0.018903 {
		t.Fatalf("buy protective price=%v, want 0.018903", buy)
	}
	if buy == 0.02 {
		t.Fatal("buy protective price was rounded to the 0.01 perp tick")
	}
	oldBuy, err := validateAndAdjustPrice(buy, 10)
	if err != nil {
		t.Fatal(err)
	}
	if oldBuy != 0.02 {
		t.Fatalf("old tick path=%v, want 0.02 to document the bug", oldBuy)
	}

	sell, err := client.SlippagePrice("SKR", false, 0.05, &mid)
	if err != nil {
		t.Fatal(err)
	}
	if sell != 0.017103 {
		t.Fatalf("sell protective price=%v, want 0.017103", sell)
	}
	if sell == 0.02 {
		t.Fatal("sell protective price was rounded up to 0.02")
	}
}
