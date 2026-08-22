package config

import "testing"

func TestSpreadFromEnvDefaults(t *testing.T) {
	t.Setenv("CLICKHOUSE_PASSWORD", "secret")
	config, err := SpreadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.GRPCAddress != ":9097" || config.ClickHouseAddr != "127.0.0.1:9000" ||
		config.ClickHouseDatabase != "market_data" || config.ClickHouseTable != "crypto_bbo" ||
		config.PoolSize != 8 || config.MaxConcurrentQuery != 8 {
		t.Fatalf("unexpected spread defaults: %+v", config)
	}
}

func TestSpreadFromEnvRejectsInvalidTable(t *testing.T) {
	t.Setenv("CLICKHOUSE_TABLE", "crypto bbo")
	if _, err := SpreadFromEnv(); err == nil {
		t.Fatal("invalid table identifier was accepted")
	}
}
