package funding

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"selfquant/backend/internal/database"
	"selfquant/backend/internal/exchange"
)

func TestRepositoryPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("repository_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()

	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(pool)
	now := time.Now().UTC().Truncate(time.Second)
	instrument := exchange.Instrument{
		Exchange: "test", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC",
		QuoteAsset: "USDT", GlobalSymbol: "BTCUSDT", IntervalHours: 8,
		SettleAsset: "USDT", ContractType: "perpetual", Status: "active",
		SourceUpdatedAt: now,
	}
	if err := repository.UpsertInstruments(ctx, []exchange.Instrument{instrument}); err != nil {
		t.Fatal(err)
	}
	current := exchange.FundingRate{
		Exchange: "test", ExchangeSymbol: "BTCUSDT", Rate: 0.0001,
		FundingTime: now.Add(8 * time.Hour), IntervalHours: 8,
		LastPrice: 60000, OpenInterestBase: 100,
		OpenInterestNotionalUSD: 6000000, Turnover24hUSD: 100000000,
		SourceUpdatedAt: now,
	}
	if err := repository.UpsertRates(ctx, []exchange.FundingRate{current}); err != nil {
		t.Fatal(err)
	}
	current.Rate = 0.0002
	if err := repository.UpsertRates(ctx, []exchange.FundingRate{current}); err != nil {
		t.Fatal(err)
	}
	history := make([]exchange.FundingRate, 0, 12)
	for index := 0; index < 12; index++ {
		history = append(history, exchange.FundingRate{
			Exchange: "test", ExchangeSymbol: "BTCUSDT",
			Rate:        float64(index+1) / 100000,
			FundingTime: now.Add(-time.Duration(index+1) * 8 * time.Hour),
			Settled:     true, IntervalHours: 8, SourceUpdatedAt: now,
		})
	}
	history = append(history, history[0])
	if err := repository.UpsertRates(ctx, history); err != nil {
		t.Fatal(err)
	}
	rates, total, err := repository.List(ctx, Query{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(rates) != 1 {
		t.Fatalf("total=%d rates=%d", total, len(rates))
	}
	if rates[0].Rate != 0.0002 {
		t.Fatalf("current upsert failed: %v", rates[0].Rate)
	}
	if len(rates[0].History) != 10 {
		t.Fatalf("history len=%d", len(rates[0].History))
	}
	var currentRows, settledRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE record_kind='current'),
		       count(*) FILTER (WHERE record_kind='settled')
		FROM funding_rates`).Scan(&currentRows, &settledRows); err != nil {
		t.Fatal(err)
	}
	if currentRows != 1 || settledRows != 12 {
		t.Fatalf("current=%d settled=%d", currentRows, settledRows)
	}
}
