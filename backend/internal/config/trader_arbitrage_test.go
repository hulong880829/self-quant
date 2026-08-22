package config

import (
	"testing"
	"time"
)

func TestTraderArbitrageDefaults(t *testing.T) {
	t.Setenv("TRADER_INTERNAL_TOKEN", "test-token")
	config, err := TraderFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.ArbitrageScheduleInterval != 100*time.Millisecond ||
		config.ArbitrageBBOStale != 2*time.Second ||
		config.ArbitrageRepriceTicks != 2 ||
		!config.ArbitrageOrderStreamEnabled ||
		config.OrderStreamReconnectInitial != 250*time.Millisecond ||
		config.OrderStreamReconnectMax != 30*time.Second ||
		config.OrderStreamStale != 45*time.Second ||
		config.OrderStreamSessionIdle != 5*time.Minute ||
		config.ArbitrageRetention != 7*24*time.Hour ||
		!config.ArbitrageDryRun {
		t.Fatalf("unexpected arbitrage defaults: %+v", config)
	}
}

func TestTraderOrderStreamRejectsInvalidReconnectRange(t *testing.T) {
	t.Setenv("TRADER_INTERNAL_TOKEN", "test-token")
	t.Setenv("TRADER_ORDER_STREAM_RECONNECT_INITIAL", "10s")
	t.Setenv("TRADER_ORDER_STREAM_RECONNECT_MAX", "1s")
	if _, err := TraderFromEnv(); err == nil {
		t.Fatal("expected invalid order stream reconnect range to fail")
	}
}

func TestTraderArbitrageConfigRejectsNonPositiveDuration(t *testing.T) {
	t.Setenv("TRADER_INTERNAL_TOKEN", "test-token")
	t.Setenv("TRADER_ARBITRAGE_BBO_STALE", "0s")
	if _, err := TraderFromEnv(); err == nil {
		t.Fatal("expected non-positive BBO stale duration to fail")
	}
}
