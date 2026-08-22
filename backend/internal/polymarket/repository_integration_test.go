package polymarket

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"selfquant/backend/internal/database"
)

func TestCleanupMarketDataPostgresIntegration(t *testing.T) {
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
	schema := fmt.Sprintf("polymarket_repository_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	}()

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
	oldStart := now.Add(-73 * time.Hour)
	recentStart := now.Add(-time.Hour)
	markets := []Market{
		testRetentionMarket("old-unreferenced", oldStart),
		testRetentionMarket("old-referenced", oldStart),
		testRetentionMarket("recent", recentStart),
	}
	if err := repository.UpsertMarkets(ctx, markets); err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertPricePoint(ctx, markets[1].ID, PricePoint{
		Timestamp: now.Add(-72 * time.Hour), ChainlinkPrice: "100",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertPricePoint(ctx, markets[2].ID, PricePoint{
		Timestamp: now.Add(-30 * time.Minute), ChainlinkPrice: "101",
	}); err != nil {
		t.Fatal(err)
	}

	var accountID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO trading_accounts (
			owner_username, product_name, exchange, account_name,
			api_key_enc, api_secret_enc
		) VALUES ('admin','poly','polymarket','retention',decode('01','hex'),decode('02','hex'))
		RETURNING id`).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO polymarket_orders (
			id, idempotency_key, trading_account_id, market_id, token_id,
			outcome, side, requested_amount, amount_unit, status
		) VALUES (
			'00000000-0000-0000-0000-000000000001',
			'retention-test', $1, $2, 'token', 'up', 'buy', 1, 'usd', 'filled'
		)`, accountID, markets[1].ID); err != nil {
		t.Fatal(err)
	}

	if err := repository.CleanupMarketData(ctx, now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	assertRetentionCount(t, ctx, pool,
		`SELECT count(*) FROM polymarket_markets WHERE id='old-unreferenced'`, 0)
	assertRetentionCount(t, ctx, pool,
		`SELECT count(*) FROM polymarket_markets WHERE id='old-referenced'`, 1)
	assertRetentionCount(t, ctx, pool,
		`SELECT count(*) FROM polymarket_markets WHERE id='recent'`, 1)
	assertRetentionCount(t, ctx, pool,
		`SELECT count(*) FROM polymarket_price_points WHERE observed_at < $1`,
		0, now.Add(-48*time.Hour))
	assertRetentionCount(t, ctx, pool,
		`SELECT count(*) FROM polymarket_price_points WHERE market_id='recent'`, 1)
}

func testRetentionMarket(id string, start time.Time) Market {
	return Market{
		ID: id, ConditionID: "condition-" + id, Slug: "slug-" + id,
		Asset: "BTC", Period: "5m", Title: id,
		WindowStart: start, WindowEnd: start.Add(5 * time.Minute),
		UpTokenID: "up-" + id, DownTokenID: "down-" + id,
		TickSize: "0.01", Active: true, SourceUpdatedAt: start,
	}
}

func assertRetentionCount(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	query string,
	want int,
	args ...any,
) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, query, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("count=%d want=%d query=%q", got, want, query)
	}
}
