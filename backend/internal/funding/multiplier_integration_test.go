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

func TestContractMultiplierRoundTripAndPairing(t *testing.T) {
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
	schema := fmt.Sprintf("multiplier_test_%d", time.Now().UnixNano())
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
	instruments := []exchange.Instrument{
		{
			Exchange: "aster", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
			GlobalSymbol: "BTCUSDT", IntervalHours: 8, SettleAsset: "USDT",
			ContractType: "perpetual", Status: "active", ContractSize: 1, SourceUpdatedAt: now,
		},
		{
			Exchange: "lighter", ExchangeSymbol: "BTC", BaseAsset: "BTC", QuoteAsset: "USDC",
			GlobalSymbol: "BTCUSDC", IntervalHours: 1, SettleAsset: "USDC",
			ContractType: "perpetual", Status: "active", ContractSize: 0.01, SourceUpdatedAt: now,
		},
		{
			Exchange: "binance", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
			GlobalSymbol: "BTCUSDT", IntervalHours: 8, SettleAsset: "USDT",
			ContractType: "perpetual", Status: "active", SourceUpdatedAt: now,
		},
	}
	if _, err := repository.UpsertInstruments(ctx, instruments); err != nil {
		t.Fatal(err)
	}
	rates := []exchange.FundingRate{
		{Exchange: "aster", ExchangeSymbol: "BTCUSDT", Rate: 0.0001, FundingTime: now.Add(time.Hour), IntervalHours: 8, SourceUpdatedAt: now},
		{Exchange: "lighter", ExchangeSymbol: "BTC", Rate: 0.00001, FundingTime: now.Add(time.Hour), IntervalHours: 1, SourceUpdatedAt: now},
		{Exchange: "binance", ExchangeSymbol: "BTCUSDT", Rate: 0.0002, FundingTime: now.Add(time.Hour), IntervalHours: 8, SourceUpdatedAt: now},
	}
	if err := repository.UpsertCurrentRates(ctx, rates); err != nil {
		t.Fatal(err)
	}
	listed, _, err := repository.List(ctx, Query{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	bySymbol := map[string]Rate{}
	for _, item := range listed {
		bySymbol[item.Exchange+":"+item.ExchangeSymbol] = item
	}
	if bySymbol["aster:BTCUSDT"].ContractMultiplier != 1 ||
		bySymbol["lighter:BTC"].ContractMultiplier != 0.01 ||
		bySymbol["binance:BTCUSDT"].ContractMultiplier != 1 {
		t.Fatalf("multipliers=%+v", bySymbol)
	}
	spreads := BuildSpreads(listed)
	pairs := spreadPairSet(spreads)
	if !pairs["aster/BTCUSDT|binance/BTCUSDT"] {
		t.Fatalf("same multiplier should pair: %v", pairs)
	}
	if _, ok := pairs["binance/BTCUSDT|lighter/BTC"]; ok {
		t.Fatal("different multiplier must not pair")
	}
}
