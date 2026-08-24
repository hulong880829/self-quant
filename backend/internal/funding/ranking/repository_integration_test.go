package ranking

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestClickHouseMinuteQueryIntegration(t *testing.T) {
	if os.Getenv("FUNDING_CLICKHOUSE_INTEGRATION") != "1" {
		t.Skip("set FUNDING_CLICKHOUSE_INTEGRATION=1 to run against ClickHouse")
	}
	timeout := 5 * time.Second
	repository, err := Open(context.Background(), RepositoryConfig{
		Address: os.Getenv("CLICKHOUSE_ADDR"), Database: envDefault("CLICKHOUSE_DATABASE", "market_data"),
		Table: envDefault("CLICKHOUSE_TABLE", "crypto_bbo"), User: os.Getenv("CLICKHOUSE_USER"),
		Password: os.Getenv("CLICKHOUSE_PASSWORD"), TLS: os.Getenv("CLICKHOUSE_TLS") == "true",
		QueryTimeout: timeout, PoolSize: 2, UseMinuteTable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	now := time.Now().UTC()
	var durations []time.Duration
	for range 5 {
		started := time.Now()
		_, err = repository.Query(
			context.Background(), now.Add(-3*time.Minute), now,
			strings.Split(envDefault("FUNDING_TEST_SYMBOLS", "BTCUSDT"), ","),
			strings.Split(envDefault("FUNDING_TEST_VENUES", "binance,okx"), ","),
		)
		if err != nil {
			t.Fatal(err)
		}
		durations = append(durations, time.Since(started))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95 := durations[len(durations)-1]
	if p95 >= 2*time.Second {
		t.Fatalf("minute query p95=%s, want <2s", p95)
	}
	t.Logf("minute query p95=%s", p95)
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
