package ranking

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

func TestClickHouseMinuteQueryIntegration(t *testing.T) {
	repository := openClickHouseIntegration(t)
	defer repository.Close()
	now := time.Now().UTC()
	var pairs []HistoryPair
	for _, symbol := range strings.Split(envDefault("FUNDING_TEST_SYMBOLS", "BTCUSDT"), ",") {
		for _, venue := range strings.Split(envDefault("FUNDING_TEST_VENUES", "binance,okx"), ",") {
			pairs = append(pairs, HistoryPair{
				Venue: strings.TrimSpace(venue), SourceSymbol: strings.TrimSpace(symbol),
				CanonicalSymbol: strings.TrimSpace(symbol),
			})
		}
	}
	var durations []time.Duration
	for range 5 {
		started := time.Now()
		err := repository.QueryPairs(
			context.Background(), now.Add(-3*time.Minute), now, pairs,
			func(HistoryQuote) error { return nil },
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

func TestClickHouseSingleBatchSourceSymbolAndIndexPrune(t *testing.T) {
	repository := openClickHouseIntegration(t)
	defer repository.Close()
	if repository.settings["readonly"] != 1 || repository.settings["max_threads"] != 2 {
		t.Fatalf("connection settings=%v", repository.settings)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	from := now.Add(-30 * time.Minute)
	pairs := []HistoryPair{
		{Venue: "binance", SourceSymbol: "BTCUSDT", CanonicalSymbol: "BTCUSDT"},
		{Venue: "hyperliquid", SourceSymbol: "BTCUSDC", CanonicalSymbol: "BTCUSDT"},
	}
	collect := func(values []HistoryPair) map[HistoryPair]int {
		counts := make(map[HistoryPair]int)
		err := repository.QueryPairs(
			context.Background(), from, now, values,
			func(quote HistoryQuote) error {
				counts[quote.Pair]++
				return nil
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		return counts
	}
	batched := collect(pairs)
	separate := map[HistoryPair]int{}
	for _, pair := range pairs {
		for key, count := range collect([]HistoryPair{pair}) {
			separate[key] += count
		}
	}
	if len(batched) != len(separate) {
		t.Fatalf("batched pairs=%d separate=%d", len(batched), len(separate))
	}
	for pair, count := range batched {
		if separate[pair] != count {
			t.Fatalf("%s %s batched=%d separate=%d", pair.Venue, pair.SourceSymbol, count, separate[pair])
		}
		allowed := pair.Venue == "binance" && pair.SourceSymbol == "BTCUSDT" ||
			pair.Venue == "hyperliquid" && pair.SourceSymbol == "BTCUSDC"
		if !allowed {
			t.Fatalf("cartesian pair returned: %+v", pair)
		}
		if pair.Venue == "hyperliquid" && pair.CanonicalSymbol != "BTCUSDT" {
			t.Fatalf("hyperliquid canonical=%q", pair.CanonicalSymbol)
		}
	}
	explain := "EXPLAIN indexes = 1\n" + exactHistoryQuery(repository.database, repository.table)
	binding := historyQueryBindings(pairs)
	rows, err := repository.conn.Query(
		context.Background(), explain,
		clickhouse.Named("from", from),
		clickhouse.Named("to", now),
		clickhouse.Named("pair_venues", binding.pairVenues),
		clickhouse.Named("pair_symbols", binding.pairSymbols),
		clickhouse.Named("unique_venues", binding.uniqueVenues),
		clickhouse.Named("unique_symbols", binding.uniqueSymbols),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var explained strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		explained.WriteString(line)
		explained.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	text := explained.String()
	if !strings.Contains(text, "PrimaryKey") ||
		!strings.Contains(text, "canonical_symbol in") ||
		!strings.Contains(text, "venue in") {
		t.Fatalf("explain missing primary-key prune:\n%s", text)
	}
	t.Logf("explain indexes=\n%s", text)
}

func openClickHouseIntegration(t *testing.T) *Repository {
	t.Helper()
	if os.Getenv("FUNDING_CLICKHOUSE_INTEGRATION") != "1" {
		t.Skip("set FUNDING_CLICKHOUSE_INTEGRATION=1 to run against ClickHouse")
	}
	timeout := 5 * time.Second
	repository, err := Open(context.Background(), RepositoryConfig{
		Address:  envDefault("CLICKHOUSE_ADDR", "127.0.0.1:9000"),
		Database: envDefault("CLICKHOUSE_DATABASE", "market_data"),
		Table:    envDefault("CLICKHOUSE_TABLE", "crypto_bbo"),
		User:     envDefault("CLICKHOUSE_USER", "bbo_writer"),
		Password: os.Getenv("CLICKHOUSE_PASSWORD"), TLS: os.Getenv("CLICKHOUSE_TLS") == "true",
		QueryTimeout: timeout, PoolSize: 2, UseMinuteTable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
