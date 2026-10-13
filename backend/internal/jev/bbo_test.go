package jev

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

func TestLatestBBOQueryAliasesTimestampWithoutShadowingPrewhere(t *testing.T) {
	query := latestBBOQuery("market_data", "crypto_bbo")
	if !strings.Contains(query, "max(ts) AS latest_ts") {
		t.Fatalf("missing latest_ts alias: %s", query)
	}
	if strings.Contains(query, "max(ts) AS ts") {
		t.Fatalf("aliased aggregate still shadows ts: %s", query)
	}
	if !strings.Contains(query, "AND ts >= now() - INTERVAL 1 HOUR") {
		t.Fatalf("PREWHERE must keep the raw ts column: %s", query)
	}
	if !strings.Contains(query, "FROM market_data.crypto_bbo") {
		t.Fatalf("unexpected table: %s", query)
	}
}

func TestDecodeScaledAndMarketKey(t *testing.T) {
	got, ok := decodeScaled(123450000, 4)
	if !ok || !got.Equal(decimal.RequireFromString("12345")) {
		t.Fatalf("scaled=%s ok=%v", got, ok)
	}
	key := NormalizeMarketKey("Binance", "btcusdt", ProductPerpetual)
	if key.Venue != "binance" || key.Symbol != "BTCUSDT" || key.Product != ProductPerpetual {
		t.Fatalf("%+v", key)
	}
}
