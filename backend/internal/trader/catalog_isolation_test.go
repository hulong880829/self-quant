package trader

import (
	"os"
	"strings"
	"testing"
)

func TestCatalogKeepsCEXProductsAndAddsDEXPerpetuals(t *testing.T) {
	source, err := os.ReadFile("catalog.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if !strings.Contains(text, "exchange IN ('binance', 'okx', 'bybit', 'bitget', 'gate')") {
		t.Fatal("trader catalog must retain the five CEX venues")
	}
	if !strings.Contains(text, "exchange IN ('hyperliquid', 'lighter', 'aster')") ||
		!strings.Contains(text, "contract_type='perpetual'") {
		t.Fatal("trader catalog must expose only perpetual instruments for the new venues")
	}
}
