package ranking

import (
	"math"
	"strings"
	"testing"
)

func TestHistoryQueryUsesMinuteAggregationStates(t *testing.T) {
	query := historyQuery("market_data", "crypto_bbo_minute")
	for _, fragment := range []string{
		"PREWHERE product = 'perpetual'",
		"canonical_symbol IN @symbols",
		"venue IN @venues",
		"bucket >= toStartOfMinute(@from)",
		"argMaxMerge(bbo_state)",
		"GROUP BY bucket, canonical_symbol, venue",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("query missing %q:\n%s", fragment, query)
		}
	}
	for _, forbidden := range []string{"toStartOfMinute(ts)", "argMax(bid_price", "crypto_bbo\n"} {
		if strings.Contains(query, forbidden) {
			t.Fatalf("query must not aggregate raw BBO rows; found %q:\n%s", forbidden, query)
		}
	}
}

func TestMinuteTable(t *testing.T) {
	if got := minuteTable("crypto_bbo"); got != "crypto_bbo_minute" {
		t.Fatalf("table=%q", got)
	}
	if got := minuteTable("custom_minute"); got != "custom_minute" {
		t.Fatalf("table=%q", got)
	}
}

func TestHistoryQueryRollbackSelectsRawTable(t *testing.T) {
	raw := historyQueryForTable("market_data", "crypto_bbo")
	if !strings.Contains(raw, "toStartOfMinute(ts)") ||
		!strings.Contains(raw, "argMax(bid_price, ts)") {
		t.Fatalf("raw rollback query is invalid:\n%s", raw)
	}
	minute := historyQueryForTable("market_data", "crypto_bbo_minute")
	if !strings.Contains(minute, "argMaxMerge(bbo_state)") {
		t.Fatalf("minute query is invalid:\n%s", minute)
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
