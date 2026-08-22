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
	spot := instrument
	spot.ContractType = exchange.ContractTypeSpot
	spot.IntervalHours = 0
	if err := repository.UpsertInstruments(ctx, []exchange.Instrument{spot}); err != nil {
		t.Fatal(err)
	}
	var instrumentRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM instruments`).Scan(&instrumentRows); err != nil {
		t.Fatal(err)
	}
	if instrumentRows != 2 {
		t.Fatalf("spot/perpetual symbol collision: instruments=%d", instrumentRows)
	}
	current := exchange.FundingRate{
		Exchange: "test", ExchangeSymbol: "BTCUSDT", Rate: 0.0001,
		FundingTime: now.Add(8 * time.Hour), IntervalHours: 8,
		MarkPrice: 60000.75, LastPrice: 60000.25,
		OpenInterestContracts: 2_858_756_654.5, OpenInterestBase: 100.25,
		OpenInterestNotionalUSD: 3_000_000_000.75,
		Volume24hBase:           2_858_756_654.25, Turnover24hUSD: 5_000_000_000.5,
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
	if err := repository.RefreshAggregates(ctx); err != nil {
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
	if rates[0].MarkPrice != 60000.75 ||
		rates[0].Volume24hBase != 2_858_756_654.25 ||
		rates[0].Turnover24hUSD != 5_000_000_000.5 {
		t.Fatalf("large numeric fields lost precision: %+v", rates[0])
	}
	if len(rates[0].History) != 0 {
		t.Fatalf("list must not include history: %d", len(rates[0].History))
	}
	historyPoints, err := repository.ListHistory(ctx, "test", "BTCUSDT", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(historyPoints) != 10 {
		t.Fatalf("history len=%d", len(historyPoints))
	}
	if rates[0].Cumulative7d <= 0 || rates[0].AnnualizedRate <= 0 {
		t.Fatalf("aggregates were not calculated: %+v", rates[0])
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
	inactive := instrument
	inactive.ExchangeSymbol = "ETHUSDT"
	inactive.GlobalSymbol = "ETHUSDT"
	inactive.BaseAsset = "ETH"
	inactive.Status = "inactive"
	if err := repository.UpsertInstruments(ctx, []exchange.Instrument{inactive}); err != nil {
		t.Fatal(err)
	}
	inactiveRate := current
	inactiveRate.ExchangeSymbol = inactive.ExchangeSymbol
	if err := repository.UpsertRates(ctx, []exchange.FundingRate{inactiveRate}); err != nil {
		t.Fatal(err)
	}
	instruments, err := repository.ListInstruments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range instruments {
		if item.ExchangeSymbol == inactive.ExchangeSymbol {
			t.Fatalf("inactive instrument was hydrated: %+v", item)
		}
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM funding_rates f
		JOIN instruments i ON i.id=f.instrument_id
		WHERE i.exchange_symbol='ETHUSDT'`).Scan(&currentRows); err != nil {
		t.Fatal(err)
	}
	if currentRows != 0 {
		t.Fatalf("inactive instrument received funding rows: %d", currentRows)
	}
	expired := history[0]
	expired.FundingTime = now.AddDate(-2, 0, 0)
	if err := repository.UpsertRates(ctx, []exchange.FundingRate{expired}); err != nil {
		t.Fatal(err)
	}
	if deleted, err := repository.DeleteExpiredHistory(ctx, now.AddDate(-1, 0, 0), 100); err != nil {
		t.Fatal(err)
	} else if deleted != 1 {
		t.Fatalf("expired history deleted=%d", deleted)
	}
	var instrumentID, tradingAccountID int64
	if err := pool.QueryRow(ctx, `
		SELECT id FROM instruments
		WHERE exchange='test' AND contract_type='perpetual' AND exchange_symbol='BTCUSDT'
	`).Scan(&instrumentID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO trading_accounts (
			owner_username, product_name, exchange, account_name, api_key_enc, api_secret_enc
		) VALUES ('admin','test','binance','main','key','secret')
		RETURNING id
	`).Scan(&tradingAccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO trader_orders (
			id,idempotency_key,owner_username,trading_account_id,product_name,
			exchange,instrument_id,contract_type,exchange_symbol,client_order_id,
			side,order_type,quantity,price,status,request_fingerprint
		) VALUES (
			'00000000-0000-0000-0000-000000000001','integration-order','admin',$1,'test',
			'test',$2,'perpetual','BTCUSDT','integration-client',
			'buy','limit',1,1,'filled','fingerprint'
		)`, tradingAccountID, instrumentID); err != nil {
		t.Fatal(err)
	}

	if err := repository.DeactivateMissingInstruments(
		ctx, "test", exchange.ContractTypePerpetual, nil,
	); err != nil {
		t.Fatal(err)
	}
	var active bool
	var status string
	if err := pool.QueryRow(ctx, `
		SELECT active,status FROM instruments WHERE id=$1`, instrumentID,
	).Scan(&active, &status); err != nil {
		t.Fatal(err)
	}
	if !active || status != "active" {
		t.Fatalf("empty catalog deactivated instrument: active=%v status=%s", active, status)
	}

	if err := repository.DeactivateMissingInstruments(
		ctx, "test", exchange.ContractTypePerpetual,
		[]exchange.Instrument{{ExchangeSymbol: "ETHUSDT"}},
	); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT active,status FROM instruments WHERE id=$1`, instrumentID,
	).Scan(&active, &status); err != nil {
		t.Fatal(err)
	}
	if active || status != "inactive" {
		t.Fatalf("missing instrument not deactivated: active=%v status=%s", active, status)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM funding_rates WHERE instrument_id=$1`, instrumentID,
	).Scan(&currentRows); err != nil {
		t.Fatal(err)
	}
	if currentRows != 13 {
		t.Fatalf("soft deactivation removed funding history: rows=%d", currentRows)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM trader_orders WHERE instrument_id=$1`, instrumentID,
	).Scan(&currentRows); err != nil {
		t.Fatal(err)
	}
	if currentRows != 1 {
		t.Fatalf("soft deactivation removed trader order: rows=%d", currentRows)
	}
	rates, total, err = repository.List(ctx, Query{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || len(rates) != 0 {
		t.Fatalf("inactive instrument remained visible: total=%d rates=%d", total, len(rates))
	}

	instrument.SourceUpdatedAt = now.Add(time.Minute)
	if err := repository.UpsertInstruments(ctx, []exchange.Instrument{instrument}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT active,status FROM instruments WHERE id=$1`, instrumentID,
	).Scan(&active, &status); err != nil {
		t.Fatal(err)
	}
	if !active || status != "active" {
		t.Fatalf("returned instrument not reactivated: active=%v status=%s", active, status)
	}
}
