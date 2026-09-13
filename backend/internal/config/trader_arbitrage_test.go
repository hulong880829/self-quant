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
	if config.ArbitrageControlInterval != time.Second ||
		config.ArbitrageCoalesceWindow != 50*time.Millisecond ||
		config.ArbitrageBBOStale != 60*time.Second ||
		config.ArbitrageSignalBBOStale != 2*time.Second ||
		config.ArbitrageBBOReconnectInitial != 250*time.Millisecond ||
		config.ArbitrageBBOReconnectMax != 30*time.Second ||
		config.ArbitrageBBOHeartbeat != 20*time.Second ||
		config.ArbitrageBBOReadWait != 45*time.Second ||
		config.ArbitrageBBOWriteWait != 5*time.Second ||
		config.ArbitrageBBOAckWait != 5*time.Second ||
		config.ArbitrageRepriceTicks != 2 ||
		!config.ArbitrageOrderStreamEnabled ||
		config.OrderStreamReconnectInitial != 250*time.Millisecond ||
		config.OrderStreamReconnectMax != 30*time.Second ||
		config.OrderStreamStale != 45*time.Second ||
		config.OrderStreamSessionIdle != 5*time.Minute ||
		config.ArbitrageRetention != 7*24*time.Hour ||
		config.ArbitrageScheduleWorkers != 16 ||
		config.ArbitrageHedgeEmergencyAfter != 2*time.Second ||
		!config.ArbitrageDryRun {
		t.Fatalf("unexpected arbitrage defaults: %+v", config)
	}
	for _, venue := range []string{
		"binance", "okx", "bybit", "bitget", "gate",
		"hyperliquid", "aster", "lighter",
	} {
		if !config.ArbitrageEnabledExchanges[venue] {
			t.Fatalf("default arbitrage allowlist missing %s", venue)
		}
	}
	if config.HyperliquidURL == "" || config.AsterURL == "" || config.LighterURL == "" {
		t.Fatalf("DEX REST defaults are incomplete: %+v", config)
	}
}

func TestTraderMarketDataRejectsInvalidHeartbeatRange(t *testing.T) {
	t.Setenv("TRADER_INTERNAL_TOKEN", "test-token")
	t.Setenv("TRADER_ARBITRAGE_BBO_HEARTBEAT", "45s")
	t.Setenv("TRADER_ARBITRAGE_BBO_READ_WAIT", "45s")
	if _, err := TraderFromEnv(); err == nil {
		t.Fatal("expected BBO heartbeat at read wait to fail")
	}
}

func TestTraderMarketDataRejectsInvalidReconnectRange(t *testing.T) {
	t.Setenv("TRADER_INTERNAL_TOKEN", "test-token")
	t.Setenv("TRADER_ARBITRAGE_BBO_RECONNECT_INITIAL", "10s")
	t.Setenv("TRADER_ARBITRAGE_BBO_RECONNECT_MAX", "1s")
	if _, err := TraderFromEnv(); err == nil {
		t.Fatal("expected invalid BBO reconnect range to fail")
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

func TestTraderArbitrageConfigRejectsNonPositiveSignalBBOStale(t *testing.T) {
	t.Setenv("TRADER_INTERNAL_TOKEN", "test-token")
	t.Setenv("TRADER_ARBITRAGE_SIGNAL_BBO_STALE", "0s")
	if _, err := TraderFromEnv(); err == nil {
		t.Fatal("expected non-positive signal BBO stale duration to fail")
	}
}

func TestTraderArbitrageConfigRejectsNonPositiveControlInterval(t *testing.T) {
	t.Setenv("TRADER_INTERNAL_TOKEN", "test-token")
	t.Setenv("TRADER_ARBITRAGE_CONTROL_INTERVAL", "0s")
	if _, err := TraderFromEnv(); err == nil {
		t.Fatal("expected non-positive control interval to fail")
	}
}

func TestTraderArbitrageConfigRejectsCoalesceWindowOutsideBounds(t *testing.T) {
	t.Setenv("TRADER_INTERNAL_TOKEN", "test-token")
	t.Setenv("TRADER_ARBITRAGE_COALESCE_WINDOW", "19ms")
	if _, err := TraderFromEnv(); err == nil {
		t.Fatal("expected too-small coalesce window to fail")
	}
	t.Setenv("TRADER_ARBITRAGE_COALESCE_WINDOW", "101ms")
	if _, err := TraderFromEnv(); err == nil {
		t.Fatal("expected too-large coalesce window to fail")
	}
}

func TestTraderHedgeEmergencyAfterConfig(t *testing.T) {
	t.Setenv("TRADER_INTERNAL_TOKEN", "test-token")
	t.Setenv("TRADER_ARBITRAGE_HEDGE_EMERGENCY_AFTER", "0")
	config, err := TraderFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.ArbitrageHedgeEmergencyAfter != 0 {
		t.Fatalf("0 should disable emergency, got %s", config.ArbitrageHedgeEmergencyAfter)
	}

	t.Setenv("TRADER_ARBITRAGE_HEDGE_EMERGENCY_AFTER", "100ms")
	config, err = TraderFromEnv()
	if err != nil || config.ArbitrageHedgeEmergencyAfter != 100*time.Millisecond {
		t.Fatalf("100ms got %s err=%v", config.ArbitrageHedgeEmergencyAfter, err)
	}
	t.Setenv("TRADER_ARBITRAGE_HEDGE_EMERGENCY_AFTER", "30s")
	config, err = TraderFromEnv()
	if err != nil || config.ArbitrageHedgeEmergencyAfter != 30*time.Second {
		t.Fatalf("30s got %s err=%v", config.ArbitrageHedgeEmergencyAfter, err)
	}

	for _, value := range []string{"50ms", "1m", "abc", "-1s"} {
		t.Setenv("TRADER_ARBITRAGE_HEDGE_EMERGENCY_AFTER", value)
		if _, err := TraderFromEnv(); err == nil {
			t.Fatalf("expected %q to fail", value)
		}
	}
}
