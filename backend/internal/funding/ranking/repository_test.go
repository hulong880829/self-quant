package ranking

import (
	"math"
	"strings"
	"testing"
)

func TestHistoryQueryIsSetBasedPerpetualAggregation(t *testing.T) {
	query := historyQuery("market_data", "crypto_bbo")
	for _, fragment := range []string{
		"PREWHERE product = 'perpetual'",
		"canonical_symbol IN @symbols",
		"venue IN @venues",
		"toStartOfMinute(ts)",
		"argMax(bid_price, ts)",
		"GROUP BY bucket, canonical_symbol, venue",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("query missing %q:\n%s", fragment, query)
		}
	}
	if strings.Contains(strings.ToUpper(query), " JOIN ") {
		t.Fatalf("query must not join raw BBO rows:\n%s", query)
	}
}

func TestDecodePrice(t *testing.T) {
	value, ok := decodePrice(123456, 3)
	if !ok || math.Abs(value-123.456) > 1e-9 {
		t.Fatalf("value=%f ok=%v", value, ok)
	}
	if _, ok := decodePrice(0, 2); ok {
		t.Fatal("zero must be rejected")
	}
}
