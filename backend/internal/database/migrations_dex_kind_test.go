package database

import (
	"os"
	"strings"
	"testing"
)

func TestMigration038BackfillsTrimmedEmptyKind(t *testing.T) {
	raw, err := os.ReadFile("migrations/038_dex_api_wallet_kinds.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	for _, kind := range []string{
		"aster_hmac", "aster_api_wallet", "hyperliquid_agent",
		"hyperliquid_api_wallet", "lighter_api", "lighter_api_wallet",
	} {
		if !strings.Contains(sql, "'"+kind+"'") {
			t.Fatalf("missing kind %s", kind)
		}
	}
	if !strings.Contains(sql, "NULLIF(TRIM(credential_kind), '') IS NULL") {
		t.Fatal("038 must backfill NULL and blank credential_kind")
	}
	if !strings.Contains(sql, "WHEN 'hyperliquid' THEN 'hyperliquid_api_wallet'") ||
		!strings.Contains(sql, "WHEN 'aster' THEN 'aster_api_wallet'") ||
		!strings.Contains(sql, "WHEN 'lighter' THEN 'lighter_api_wallet'") {
		t.Fatal("038 must map DEX exchanges to api_wallet kinds")
	}
}

func TestMigration044AddsNetMetricsColumns(t *testing.T) {
	raw, err := os.ReadFile("migrations/044_trader_arbitrage_net_metrics.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	for _, fragment := range []string{
		"estimated_trading_fee",
		"metrics_calculated_at",
		"metrics_quality",
		"'complete'",
		"'estimated'",
		"'partial'",
		"DELETE FROM trader_arbitrage_funding_estimates",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("044 missing %s", fragment)
		}
	}
}
