package report

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"selfquant/backend/internal/database"
)

func TestReportPeriodPostgresIntegration(t *testing.T) {
	pool := openReportTestDB(t)
	ctx := context.Background()
	repository := NewRepository(pool)
	location := PeriodLocation()

	var accountID, productID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO trading_accounts (
			owner_username, product_name, exchange, account_name, api_key_enc, api_secret_enc
		) VALUES ('admin','funding-arb-test','binance','main','key','secret')
		RETURNING id, product_id`).Scan(&accountID, &productID); err != nil {
		t.Fatal(err)
	}

	sampled29 := wall(location, 2026, 8, 29, 8, 56, 0).UTC()
	sampled30 := wall(location, 2026, 8, 30, 8, 56, 0).UTC()
	if err := repository.InsertEquitySamples(ctx, productID, sampled29, []AccountEquity{{
		TradingAccountID: accountID, EquityUSD: "7174.1791", SourceUpdatedAt: sampled29,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertEquitySamples(ctx, productID, sampled30, []AccountEquity{{
		TradingAccountID: accountID, EquityUSD: "5525.3778", SourceUpdatedAt: sampled30,
	}}); err != nil {
		t.Fatal(err)
	}

	occurredAt := wall(location, 2026, 8, 29, 18, 17, 0)
	var sqlDate string
	if err := pool.QueryRow(ctx, `SELECT report_period_date($1::timestamptz)::text`, occurredAt).
		Scan(&sqlDate); err != nil {
		t.Fatal(err)
	}
	if sqlDate != ReportDate(occurredAt) || sqlDate != "2026-08-30" {
		t.Fatalf("sql date=%s go date=%s", sqlDate, ReportDate(occurredAt))
	}

	ok, err := repository.FinalizeDate(ctx, productID, wall(location, 2026, 8, 29, 9, 1, 0), location, time.Now().UTC())
	if err != nil || !ok {
		t.Fatalf("finalize 08-29: ok=%v err=%v", ok, err)
	}
	flow, err := repository.CreateCashFlow(
		ctx, "admin", productID, ReportDate(occurredAt), occurredAt.UTC(),
		"-1500", "redemption", "test", true,
		DefaultIdempotencyKey("redemption", occurredAt, "-1500"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if flow.FlowDate != "2026-08-30" {
		t.Fatalf("flow_date=%s", flow.FlowDate)
	}
	if _, err := repository.CreateCashFlow(
		ctx, "admin", productID, flow.FlowDate, occurredAt.UTC(),
		"-1500", "redemption", "dup", true,
		DefaultIdempotencyKey("redemption", occurredAt, "-1500"),
	); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate err=%v", err)
	}

	ok, err = repository.FinalizeDate(ctx, productID, wall(location, 2026, 8, 30, 9, 1, 0), location, time.Now().UTC())
	if err != nil || !ok {
		t.Fatalf("finalize 08-30: ok=%v err=%v", ok, err)
	}
	before, err := repository.LoadSnapshot(ctx, "funding-arb-test", "2026-08-30")
	if err != nil {
		t.Fatal(err)
	}
	if !decEqual(before.ClosingEquityUSD, "5525.3778") {
		t.Fatalf("aum=%s", before.ClosingEquityUSD)
	}
	if !decEqual(before.NetCashFlowUSD, "-1500") {
		t.Fatalf("net cf=%s", before.NetCashFlowUSD)
	}
	if !decEqual(before.PnLUSD, "-148.8013") {
		t.Fatalf("pnl=%s", before.PnLUSD)
	}

	exists, err := repository.SnapshotExists(ctx, productID, "2026-08-30")
	if err != nil || !exists {
		t.Fatalf("snapshot exists=%v err=%v", exists, err)
	}
	if err := repository.EnqueueRecompute(ctx, productID, "2026-08-30"); err != nil {
		t.Fatal(err)
	}
	marked, err := repository.LoadSnapshot(ctx, "funding-arb-test", "2026-08-30")
	if err != nil {
		t.Fatal(err)
	}
	if marked.Status != "recomputing" {
		t.Fatalf("status=%s", marked.Status)
	}
	if err := repository.EnqueueRecompute(ctx, productID, "2026-08-30"); err != nil {
		t.Fatal(err)
	}
	jobs, err := repository.ClaimRecomputeJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ProductID != productID || jobs[0].ReportDate != "2026-08-30" {
		t.Fatalf("jobs=%+v", jobs)
	}
	ok, err = repository.RecomputeDate(ctx, productID, wall(location, 2026, 8, 30, 9, 1, 0), location, time.Now().UTC())
	if err != nil || !ok {
		t.Fatalf("recompute: ok=%v err=%v", ok, err)
	}
	if err := repository.FinishRecomputeJob(ctx, jobs[0].ID, productID, "2026-08-30", nil); err != nil {
		t.Fatal(err)
	}
	after, err := repository.LoadSnapshot(ctx, "funding-arb-test", "2026-08-30")
	if err != nil {
		t.Fatal(err)
	}
	if after.ClosingEquityUSD != before.ClosingEquityUSD {
		t.Fatalf("recompute mutated AUM %s -> %s", before.ClosingEquityUSD, after.ClosingEquityUSD)
	}
	if after.OpeningEquityUSD != before.OpeningEquityUSD {
		t.Fatalf("recompute mutated opening %s -> %s", before.OpeningEquityUSD, after.OpeningEquityUSD)
	}
	if !decEqual(after.PnLUSD, "-148.8013") || after.CashFlowCount != 1 {
		t.Fatalf("recomputed snapshot=%+v", after)
	}
	if after.Status == "recomputing" {
		t.Fatal("finished recompute left snapshot recomputing")
	}

	start := wall(location, 2026, 8, 29, 8, 59, 59)
	end := wall(location, 2026, 8, 29, 9, 0, 0)
	var startDate, endDate string
	if err := pool.QueryRow(ctx, `SELECT report_period_date($1)::text, report_period_date($2)::text`, start, end).
		Scan(&startDate, &endDate); err != nil {
		t.Fatal(err)
	}
	if startDate != "2026-08-29" || endDate != "2026-08-30" {
		t.Fatalf("boundary sql=%s %s", startDate, endDate)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, lockErr := repository.FinalizeDate(
				ctx, productID, wall(location, 2026, 8, 30, 9, 1, 0), location, time.Now().UTC(),
			)
			errs <- lockErr
		}()
	}
	wg.Wait()
	close(errs)
	for lockErr := range errs {
		if lockErr != nil {
			t.Fatal(lockErr)
		}
	}
}

func TestOrphanProductPrunedOnLastAccountDelete(t *testing.T) {
	pool := openReportTestDB(t)
	ctx := context.Background()
	repository := NewRepository(pool)

	var firstID, secondID, productID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO trading_accounts (
			owner_username, product_name, exchange, account_name, api_key_enc, api_secret_enc
		) VALUES ('admin','fund-01','binance','a','key','secret')
		RETURNING id, product_id`).Scan(&firstID, &productID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO trading_accounts (
			owner_username, product_name, exchange, account_name, api_key_enc, api_secret_enc
		) VALUES ('admin','fund-01','okx','b','key','secret')
		RETURNING id`).Scan(&secondID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO product_daily_snapshots (
			product_id,report_date,opening_equity_usd,closing_equity_usd,
			net_cash_flow_usd,pnl_usd,sample_count,status,finalized_at)
		VALUES ($1,'2026-08-15',100,110,0,10,1,'final',now())`, productID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO product_cash_flows (
			product_id,flow_date,occurred_at,amount_usd,flow_type,note,confirmed,
			confirmed_by,confirmed_at,created_by,idempotency_key)
		VALUES ($1,'2026-08-15',TIMESTAMPTZ '2026-08-14 18:00:00+08',50,'subscription','',
		        TRUE,'admin',now(),'admin','test-fund-01')`, productID); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM trading_accounts WHERE id=$1`, firstID); err != nil {
		t.Fatal(err)
	}
	listed, err := repository.ListProducts(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != productID || listed[0].AccountCount != 1 {
		t.Fatalf("after deleting one account: %+v", listed)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM trading_accounts WHERE id=$1`, secondID); err != nil {
		t.Fatal(err)
	}
	listed, err = repository.ListProducts(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("orphan product still listed: %+v", listed)
	}
	active, err := repository.ListActiveProducts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range active {
		if item.ID == productID || item.Name == "fund-01" {
			t.Fatalf("orphan product still active: %+v", item)
		}
	}
	var leftover int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM products WHERE id=$1`, productID).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover != 0 {
		t.Fatal("products row was not pruned")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM product_daily_snapshots WHERE product_id=$1`, productID).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover != 0 {
		t.Fatal("daily snapshots were not cascaded")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM product_cash_flows WHERE product_id=$1`, productID).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover != 0 {
		t.Fatal("cash flows were not cascaded")
	}
}

func TestListProductsSkipsAccountlessRows(t *testing.T) {
	pool := openReportTestDB(t)
	ctx := context.Background()
	repository := NewRepository(pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO products (owner_username, name, display_name)
		VALUES ('admin','ghost','ghost')`); err != nil {
		t.Fatal(err)
	}
	listed, err := repository.ListProducts(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range listed {
		if item.Name == "ghost" {
			t.Fatal("accountless product must not appear in report list")
		}
	}
	active, err := repository.ListActiveProducts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range active {
		if item.Name == "ghost" {
			t.Fatal("accountless product must not be settled")
		}
	}
}

func decEqual(got, want string) bool {
	left, err := decimal.NewFromString(got)
	if err != nil {
		return false
	}
	right, err := decimal.NewFromString(want)
	if err != nil {
		return false
	}
	return left.Equal(right)
}

func openReportTestDB(t *testing.T) *pgxpool.Pool {
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
	schema := fmt.Sprintf("report_period_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	})
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
	return pool
}
