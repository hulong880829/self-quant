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
		MinQuantity: 0.001, MinNotional: 5,
		MinQuantityStatus: exchange.ConstraintKnown,
		MinNotionalStatus: exchange.ConstraintKnown,
		SourceUpdatedAt:   now,
	}
	inserted, err := repository.UpsertInstruments(ctx, []exchange.Instrument{instrument})
	if err != nil {
		t.Fatal(err)
	}
	if len(inserted) != 1 || inserted[0].ID <= 0 {
		t.Fatalf("inserted perpetual=%v", inserted)
	}
	fundingInstrument := inserted[0]
	spot := instrument
	spot.ContractType = exchange.ContractTypeSpot
	spot.IntervalHours = 0
	if _, err := repository.UpsertInstruments(ctx, []exchange.Instrument{spot}); err != nil {
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
	if err := repository.UpsertCurrentRates(ctx, []exchange.FundingRate{current}); err != nil {
		t.Fatal(err)
	}
	current.Rate = 0.0002
	if err := repository.UpsertCurrentRates(ctx, []exchange.FundingRate{current}); err != nil {
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
	if _, err := repository.UpsertSettledRates(ctx, fundingInstrument, history); err != nil {
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
	rangedHistory, err := repository.ListSettledHistoryRange(
		ctx,
		now.Add(-7*24*time.Hour),
		now,
		[]HistoryKey{
			{Exchange: "test", ExchangeSymbol: "BTCUSDT"},
			{Exchange: "other", ExchangeSymbol: "BTCUSDT"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	key := HistoryKey{Exchange: "test", ExchangeSymbol: "BTCUSDT"}
	if len(rangedHistory[key]) != 12 {
		t.Fatalf("ranged history len=%d", len(rangedHistory[key]))
	}
	for index := 1; index < len(rangedHistory[key]); index++ {
		if rangedHistory[key][index].SettledAt.Before(rangedHistory[key][index-1].SettledAt) {
			t.Fatal("ranged history is not sorted ascending")
		}
	}
	if len(rangedHistory[HistoryKey{Exchange: "other", ExchangeSymbol: "BTCUSDT"}]) != 0 {
		t.Fatal("ranged history included an unrequested exchange")
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
	if _, err := repository.UpsertInstruments(ctx, []exchange.Instrument{inactive}); err != nil {
		t.Fatal(err)
	}
	inactiveRate := current
	inactiveRate.ExchangeSymbol = inactive.ExchangeSymbol
	if err := repository.UpsertCurrentRates(ctx, []exchange.FundingRate{inactiveRate}); err != nil {
		t.Fatal(err)
	}
	instruments, err := repository.ListInstruments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range instruments {
		if item.MinQuantityStatus != exchange.ConstraintKnown ||
			item.MinNotionalStatus != exchange.ConstraintKnown ||
			item.MinQuantity != 0.001 || item.MinNotional != 5 {
			t.Fatalf("instrument constraints not preserved: %+v", item)
		}
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
	if _, err := repository.UpsertSettledRates(ctx, fundingInstrument, []exchange.FundingRate{expired}); err != nil {
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
	if _, err := repository.UpsertInstruments(ctx, []exchange.Instrument{instrument}); err != nil {
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

func TestSettledUpsertSkipsUnchangedRateAndInterval(t *testing.T) {
	repository, pool := openFundingTestRepo(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	instrument := testPerpetual("okx", "BTC-USDT-SWAP", now)
	inserted, err := repository.UpsertInstruments(ctx, []exchange.Instrument{instrument})
	if err != nil || len(inserted) != 1 {
		t.Fatalf("insert instrument: %v %v", inserted, err)
	}
	rate := exchange.FundingRate{
		Exchange: "okx", ExchangeSymbol: "BTC-USDT-SWAP", Rate: 0.0001,
		FundingTime: now.Add(-8 * time.Hour), Settled: true, IntervalHours: 8,
		SourceUpdatedAt: now,
	}
	first, err := repository.UpsertSettledRates(ctx, inserted[0], []exchange.FundingRate{rate})
	if err != nil || first.Changed != 1 {
		t.Fatalf("first write=%+v err=%v", first, err)
	}
	var updatedAt time.Time
	if err := pool.QueryRow(ctx, `
		SELECT updated_at FROM funding_rates
		WHERE instrument_id=$1 AND record_kind='settled'`, inserted[0].ID,
	).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	rate.SourceUpdatedAt = now.Add(time.Minute)
	second, err := repository.UpsertSettledRates(ctx, inserted[0], []exchange.FundingRate{rate})
	if err != nil || second.Changed != 0 || second.Unchanged != 1 {
		t.Fatalf("repeat write=%+v err=%v", second, err)
	}
	var nextUpdated time.Time
	if err := pool.QueryRow(ctx, `
		SELECT updated_at FROM funding_rates
		WHERE instrument_id=$1 AND record_kind='settled'`, inserted[0].ID,
	).Scan(&nextUpdated); err != nil {
		t.Fatal(err)
	}
	if !nextUpdated.Equal(updatedAt) {
		t.Fatalf("unchanged settled updated_at %v -> %v", updatedAt, nextUpdated)
	}
	rate.Rate = 0.0002
	changed, err := repository.UpsertSettledRates(ctx, inserted[0], []exchange.FundingRate{rate})
	if err != nil || changed.Changed != 1 {
		t.Fatalf("rate change=%+v err=%v", changed, err)
	}
}

func TestSetBasedFundingWritesUseLastValuePerUniqueKey(t *testing.T) {
	repository, pool := openFundingTestRepo(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	firstInstrument := testPerpetual("okx", "ETH-USDT-SWAP", now)
	firstInstrument.BaseAsset = "WRONG"
	lastInstrument := firstInstrument
	lastInstrument.BaseAsset = "ETH"
	lastInstrument.Metadata = []byte(`{"source":"last"}`)
	inserted, err := repository.UpsertInstruments(
		ctx, []exchange.Instrument{firstInstrument, lastInstrument},
	)
	if err != nil || len(inserted) != 1 {
		t.Fatalf("last-wins instrument insert=%+v err=%v", inserted, err)
	}
	var baseAsset string
	var metadataSource string
	var instrumentUpdated time.Time
	if err := pool.QueryRow(ctx, `
		SELECT base_asset,metadata->>'source',updated_at FROM instruments WHERE id=$1`,
		inserted[0].ID,
	).Scan(&baseAsset, &metadataSource, &instrumentUpdated); err != nil {
		t.Fatal(err)
	}
	if baseAsset != "ETH" || metadataSource != "last" {
		t.Fatalf("instrument base=%s metadata source=%s", baseAsset, metadataSource)
	}
	time.Sleep(10 * time.Millisecond)
	if noops, err := repository.UpsertInstruments(ctx, []exchange.Instrument{lastInstrument}); err != nil {
		t.Fatal(err)
	} else if len(noops) != 0 {
		t.Fatalf("unchanged instruments reported as inserted: %+v", noops)
	}
	var repeatedInstrumentUpdated time.Time
	if err := pool.QueryRow(ctx, `
		SELECT updated_at FROM instruments WHERE id=$1`, inserted[0].ID,
	).Scan(&repeatedInstrumentUpdated); err != nil {
		t.Fatal(err)
	}
	if !repeatedInstrumentUpdated.Equal(instrumentUpdated) {
		t.Fatalf(
			"unchanged instrument updated_at changed: %s -> %s",
			instrumentUpdated, repeatedInstrumentUpdated,
		)
	}

	firstCurrent := exchange.FundingRate{
		Exchange: "okx", ExchangeSymbol: "ETH-USDT-SWAP",
		Rate: 0.001, FundingTime: now.Add(4 * time.Hour), IntervalHours: 4,
		SourceUpdatedAt: now,
	}
	lastCurrent := firstCurrent
	lastCurrent.Rate = 0.002
	lastCurrent.IntervalHours = 8
	if err := repository.UpsertCurrentRates(
		ctx, []exchange.FundingRate{firstCurrent, lastCurrent},
	); err != nil {
		t.Fatal(err)
	}
	if err := repository.UpdateInstrumentIntervals(
		ctx, []exchange.FundingRate{firstCurrent, lastCurrent},
	); err != nil {
		t.Fatal(err)
	}
	var currentRate float64
	var intervalSeconds int32
	if err := pool.QueryRow(ctx, `
		SELECT f.funding_rate::float8,i.funding_interval_seconds
		FROM funding_rates f JOIN instruments i ON i.id=f.instrument_id
		WHERE f.instrument_id=$1 AND f.record_kind='current'`,
		inserted[0].ID,
	).Scan(&currentRate, &intervalSeconds); err != nil {
		t.Fatal(err)
	}
	if currentRate != lastCurrent.Rate || intervalSeconds != 8*60*60 {
		t.Fatalf("current rate=%v interval=%d", currentRate, intervalSeconds)
	}
	var currentUpdated time.Time
	if err := pool.QueryRow(ctx, `
		SELECT updated_at FROM funding_rates
		WHERE instrument_id=$1 AND record_kind='current'`,
		inserted[0].ID,
	).Scan(&currentUpdated); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := repository.UpsertCurrentRates(
		ctx, []exchange.FundingRate{firstCurrent, lastCurrent},
	); err != nil {
		t.Fatal(err)
	}
	var repeatedCurrentUpdated time.Time
	if err := pool.QueryRow(ctx, `
		SELECT updated_at FROM funding_rates
		WHERE instrument_id=$1 AND record_kind='current'`,
		inserted[0].ID,
	).Scan(&repeatedCurrentUpdated); err != nil {
		t.Fatal(err)
	}
	if !repeatedCurrentUpdated.After(currentUpdated) {
		t.Fatalf("current updated_at did not advance: %s -> %s", currentUpdated, repeatedCurrentUpdated)
	}

	firstSettled := firstCurrent
	firstSettled.Settled = true
	firstSettled.FundingTime = now.Add(-8 * time.Hour)
	lastSettled := firstSettled
	lastSettled.Rate = 0.003
	lastSettled.IntervalHours = 8
	stats, err := repository.UpsertSettledRates(
		ctx, inserted[0], []exchange.FundingRate{firstSettled, lastSettled},
	)
	if err != nil || stats.Changed != 1 || stats.Unchanged != 0 {
		t.Fatalf("first duplicate settled stats=%+v err=%v", stats, err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT funding_rate::float8 FROM funding_rates
		WHERE instrument_id=$1 AND record_kind='settled'`,
		inserted[0].ID,
	).Scan(&currentRate); err != nil {
		t.Fatal(err)
	}
	if currentRate != lastSettled.Rate {
		t.Fatalf("settled rate=%v want=%v", currentRate, lastSettled.Rate)
	}
	stats, err = repository.UpsertSettledRates(
		ctx, inserted[0], []exchange.FundingRate{firstSettled, lastSettled},
	)
	if err != nil || stats.Changed != 0 || stats.Unchanged != 1 {
		t.Fatalf("repeat duplicate settled stats=%+v err=%v", stats, err)
	}
}

func TestRefreshAggregatesSkipsUnchangedCurrentRows(t *testing.T) {
	repository, pool := openFundingTestRepo(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	instrument := testPerpetual("bybit", "SOLUSDT", now)
	inserted, err := repository.UpsertInstruments(ctx, []exchange.Instrument{instrument})
	if err != nil || len(inserted) != 1 {
		t.Fatalf("insert instrument=%+v err=%v", inserted, err)
	}
	current := exchange.FundingRate{
		Exchange: "bybit", ExchangeSymbol: "SOLUSDT", Rate: 0.001,
		FundingTime: now.Add(8 * time.Hour), IntervalHours: 8, SourceUpdatedAt: now,
	}
	if err := repository.UpsertCurrentRates(ctx, []exchange.FundingRate{current}); err != nil {
		t.Fatal(err)
	}
	if err := repository.RefreshAggregates(ctx); err != nil {
		t.Fatal(err)
	}
	var firstXmin string
	var firstUpdated time.Time
	if err := pool.QueryRow(ctx, `
		SELECT xmin::text,updated_at FROM funding_rates
		WHERE instrument_id=$1 AND record_kind='current'`,
		inserted[0].ID,
	).Scan(&firstXmin, &firstUpdated); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := repository.RefreshAggregates(ctx); err != nil {
		t.Fatal(err)
	}
	var secondXmin string
	var secondUpdated time.Time
	if err := pool.QueryRow(ctx, `
		SELECT xmin::text,updated_at FROM funding_rates
		WHERE instrument_id=$1 AND record_kind='current'`,
		inserted[0].ID,
	).Scan(&secondXmin, &secondUpdated); err != nil {
		t.Fatal(err)
	}
	if secondXmin != firstXmin || !secondUpdated.Equal(firstUpdated) {
		t.Fatalf(
			"unchanged aggregate rewrote row: xmin %s -> %s updated %s -> %s",
			firstXmin, secondXmin, firstUpdated, secondUpdated,
		)
	}

	settled := current
	settled.Settled = true
	settled.FundingTime = now.Add(-8 * time.Hour)
	if _, err := repository.UpsertSettledRates(ctx, inserted[0], []exchange.FundingRate{settled}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := repository.RefreshAggregates(ctx); err != nil {
		t.Fatal(err)
	}
	var cumulative float64
	var thirdXmin string
	if err := pool.QueryRow(ctx, `
		SELECT cumulative_24h::float8,xmin::text FROM funding_rates
		WHERE instrument_id=$1 AND record_kind='current'`,
		inserted[0].ID,
	).Scan(&cumulative, &thirdXmin); err != nil {
		t.Fatal(err)
	}
	if cumulative != settled.Rate || thirdXmin == secondXmin {
		t.Fatalf("aggregate cumulative=%v xmin=%s previous=%s", cumulative, thirdXmin, secondXmin)
	}
}

func TestHistoryLockExclusivePerInstrument(t *testing.T) {
	_, pool := openFundingTestRepo(t)
	ctx := context.Background()
	lock := NewHistoryLock(pool)
	unlockA, ok, err := lock.TryLock(ctx, 42)
	if err != nil || !ok {
		t.Fatalf("first lock ok=%v err=%v", ok, err)
	}
	defer unlockA()
	_, locked, err := lock.TryLock(ctx, 42)
	if err != nil || locked {
		t.Fatalf("same instrument should be locked: ok=%v err=%v", locked, err)
	}
	unlockB, ok, err := lock.TryLock(ctx, 43)
	if err != nil || !ok {
		t.Fatalf("different instrument lock ok=%v err=%v", ok, err)
	}
	unlockB()
	unlockA()
	unlockA = nil
	unlockC, ok, err := lock.TryLock(ctx, 42)
	if err != nil || !ok {
		t.Fatalf("lock after release ok=%v err=%v", ok, err)
	}
	unlockC()
}

func TestListActivePerpetualInstrumentsSkipsSpot(t *testing.T) {
	repository, _ := openFundingTestRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()
	perp := testPerpetual("venue-a", "BTC", now)
	spot := perp
	spot.ContractType = exchange.ContractTypeSpot
	spot.ExchangeSymbol = "BTC-SPOT"
	if _, err := repository.UpsertInstruments(ctx, []exchange.Instrument{perp, spot}); err != nil {
		t.Fatal(err)
	}
	got, err := repository.ListActivePerpetualInstruments(ctx, []string{"venue-a"})
	if err != nil || len(got) != 1 || got[0].ExchangeSymbol != "BTC" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func openFundingTestRepo(t *testing.T) (*Repository, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("funding_hist_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return NewRepository(pool), pool
}

func testPerpetual(exchangeName, symbol string, now time.Time) exchange.Instrument {
	return exchange.Instrument{
		Exchange: exchangeName, ExchangeSymbol: symbol, BaseAsset: "BTC",
		QuoteAsset: "USDT", GlobalSymbol: "BTCUSDT", IntervalHours: 8,
		SettleAsset: "USDT", ContractType: "perpetual", Status: "active",
		SourceUpdatedAt: now,
	}
}

func TestListCurrentByKeysPrefersExactOverHigherAnnualizedFallback(t *testing.T) {
	repository, pool := openFundingTestRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Second)
	exact := testPerpetual("binance", "BTCUSDT", now)
	fallback := testPerpetual("binance", "BTCUSDC", now)
	fallback.GlobalSymbol = "BTCUSDC"
	if _, err := repository.UpsertInstruments(ctx, []exchange.Instrument{exact, fallback}); err != nil {
		t.Fatal(err)
	}
	if err := repository.UpsertCurrentRates(ctx, []exchange.FundingRate{
		{
			Exchange: "binance", ExchangeSymbol: "BTCUSDT", Rate: 0.0001,
			FundingTime: now.Add(8 * time.Hour), IntervalHours: 8, SourceUpdatedAt: now,
		},
		{
			Exchange: "binance", ExchangeSymbol: "BTCUSDC", Rate: 0.0002,
			FundingTime: now.Add(8 * time.Hour), IntervalHours: 8, SourceUpdatedAt: now,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE funding_rates AS current
		SET annualized_rate = CASE i.exchange_symbol
			WHEN 'BTCUSDT' THEN 0.1
			WHEN 'BTCUSDC' THEN 0.9
		END
		FROM instruments i
		WHERE i.id=current.instrument_id AND current.record_kind='current'`); err != nil {
		t.Fatal(err)
	}
	rates, err := repository.ListCurrentByKeys(ctx, []RateLookupKey{{
		Exchange: "binance", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 1 || rates[0] == nil || rates[0].ExchangeSymbol != "BTCUSDT" ||
		rates[0].AnnualizedRate != 0.1 {
		t.Fatalf("exact must win: %+v", rates)
	}
}

