package config

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestJevFromEnvDefaults(t *testing.T) {
	t.Setenv("CLICKHOUSE_PASSWORD", "secret")
	t.Setenv("TYPESAFE_API_KEY", "")
	config, err := JevFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.GRPCAddress != ":9098" || config.ClickHouseTable != "crypto_bbo" ||
		!config.PaperRoundTripCostBPS.Equal(decimal.RequireFromString("12")) ||
		!config.PaperInitialEquity.Equal(decimal.RequireFromString("10000")) ||
		!config.PositionNotionalUSDT.Equal(decimal.RequireFromString("100")) ||
		config.MaxConcurrentDecisions != 2 || config.MaxEntryDecisionsPerCycle != 8 ||
		config.TypeSafeModel != "jev-latest" ||
		config.BasisLookback != 7*24*time.Hour || config.BasisForecastHorizon != 24*time.Hour ||
		config.BasisRefreshInterval != 10*time.Minute || config.BasisCacheMaxAge != 20*time.Minute ||
		!config.MinExpectedNetReturn24h.Equal(decimal.RequireFromString("0.002")) ||
		!config.MinConservativeNetReturn24h.Equal(decimal.RequireFromString("0.0005")) ||
		!config.MinNetProfitProbability.Equal(decimal.RequireFromString("0.60")) ||
		!config.TakeProfitPadUSDT.Equal(decimal.RequireFromString("0.05")) ||
		config.ExitConfirmCycles != 3 || config.MissingBBOForceAfter != 15*time.Minute ||
		!config.StopLossNetUSDT.Equal(decimal.RequireFromString("-1")) {
		t.Fatalf("unexpected jev defaults: %+v", config)
	}
}

func TestJevFromEnvRejectsPositiveStopLoss(t *testing.T) {
	t.Setenv("JEV_STOP_LOSS_NET_USDT", "1")
	if _, err := JevFromEnv(); err == nil {
		t.Fatal("positive stop loss was accepted")
	}
}

func TestJevFromEnvRejectsNonPositiveCost(t *testing.T) {
	t.Setenv("JEV_PAPER_ROUND_TRIP_COST_BPS", "0")
	if _, err := JevFromEnv(); err == nil {
		t.Fatal("zero cost bps was accepted")
	}
}

func TestJevFromEnvAcceptsOddCostBPS(t *testing.T) {
	t.Setenv("JEV_PAPER_ROUND_TRIP_COST_BPS", "12.5")
	config, err := JevFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !config.PaperRoundTripCostBPS.Equal(decimal.RequireFromString("12.5")) {
		t.Fatalf("cost=%s", config.PaperRoundTripCostBPS)
	}
}
