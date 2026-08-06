package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Funding struct {
	DatabaseURL            string
	GRPCAddress            string
	SyncInterval           time.Duration
	HTTPTimeout            time.Duration
	InstrumentSyncInterval time.Duration
	LogLevel               string
	Development            bool
	EnabledExchanges       map[string]bool
}

type Gateway struct {
	HTTPAddress        string
	FundingGRPCAddress string
	LogLevel           string
}

func FundingFromEnv() (Funding, error) {
	c := Funding{
		DatabaseURL:            env("DATABASE_URL", "postgres://selfquant:selfquant@localhost:5432/selfquant?sslmode=disable"),
		GRPCAddress:            env("FUNDING_GRPC_ADDRESS", ":9090"),
		SyncInterval:           duration("SYNC_INTERVAL", 10*time.Second),
		HTTPTimeout:            duration("EXCHANGE_HTTP_TIMEOUT", 10*time.Second),
		InstrumentSyncInterval: duration("INSTRUMENT_SYNC_INTERVAL", time.Hour),
		LogLevel:               env("LOG_LEVEL", "info"),
		Development:            boolean("DEVELOPMENT", false),
		EnabledExchanges: enabledExchanges(env(
			"ENABLED_EXCHANGES",
			"binance,okx,bybit,bitget,gate,hyperliquid",
		)),
	}
	if c.SyncInterval <= 0 || c.HTTPTimeout <= 0 || c.InstrumentSyncInterval <= 0 {
		return c, fmt.Errorf("durations must be positive")
	}
	return c, nil
}

func enabledExchanges(value string) map[string]bool {
	result := make(map[string]bool)
	for _, name := range strings.Split(value, ",") {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			result[name] = true
		}
	}
	return result
}

func boolean(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func GatewayFromEnv() Gateway {
	return Gateway{
		HTTPAddress:        env("API_HTTP_ADDRESS", ":8080"),
		FundingGRPCAddress: env("FUNDING_GRPC_TARGET", "localhost:9090"),
		LogLevel:           env("LOG_LEVEL", "info"),
	}
}

func LoggerLevel(s string) int {
	switch s {
	case "debug":
		return -4
	case "warn":
		return 4
	case "error":
		return 8
	default:
		return 0
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func duration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	if d, err := time.ParseDuration(value); err == nil {
		return d
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(seconds) * time.Second
	}
	return fallback
}
