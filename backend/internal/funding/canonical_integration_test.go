package funding

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"selfquant/backend/internal/database"
)

func TestCanonicalMigrationExpiresClearedRows(t *testing.T) {
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
	schema := fmt.Sprintf("canonical_test_%d", time.Now().UnixNano())
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
	if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS instruments_ib_con_id_uq`); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(24 * time.Hour)
	insert := `
		INSERT INTO instruments (
			exchange, exchange_symbol, base_asset, quote_asset, global_symbol,
			active, settle_asset, contract_type, status, funding_interval_seconds,
			metadata, ib_con_id, next_verify_at
		) VALUES (
			'ibkr', $1, $2, 'USD', 'IBKR:' || $1,
			$3, 'USD', 'spot', $4, 0,
			$5, $6, $7
		)`
	if _, err := pool.Exec(ctx, insert, "stock|AAPL|USD|US", "AAPL", true, "active",
		[]byte(`{"assetClass":"stock","underlyingSymbol":"AAPL","underlyingCurrency":"USD","region":"US","secType":"STK","candidateAliases":["binance|AAPLUSDT"],"pairMatches":{"binance|AAPLUSDT":{"MatchClass":"verified_listing"}}}`),
		int64(265598), future); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insert, "stock|AAPL|USDT|US", "AAPL", true, "active",
		[]byte(`{"assetClass":"stock","underlyingSymbol":"AAPL","underlyingCurrency":"USD","region":"US","candidateAliases":["okx|AAPLUSDT"],"pairMatches":{"okx|AAPLUSDT":{"MatchClass":"verified_listing"}}}`),
		int64(265598), future); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insert, "stock|OPENAI|USD|", "OPENAI", true, "candidate",
		[]byte(`{"assetClass":"stock","underlyingSymbol":"OPENAI","underlyingCurrency":"USD"}`), nil, future); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insert, "crypto|BTC|USD", "BTC", false, "not_found",
		[]byte(`{"assetClass":"crypto","underlyingSymbol":"BTC"}`), nil, future); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(pool)
	if _, err := repository.MigrateIBCanonical(ctx, true); err != nil {
		t.Fatal(err)
	}
	if verify, err := nextVerify(ctx, pool, "stock|OPENAI|USD|"); err != nil || !verify.Equal(future.UTC().Truncate(time.Microsecond)) && verify.Sub(future) > time.Second {
		t.Fatalf("dry-run changed next_verify=%s err=%v", verify, err)
	}
	if _, err := repository.MigrateIBCanonical(ctx, false); err != nil {
		t.Fatal(err)
	}
	rows, err := repository.ListIBInstrumentResolutions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var survivor IBResolution
	var found bool
	for _, row := range rows {
		if row.Symbol == "conid:265598" {
			survivor = row
			found = true
		}
	}
	if !found || !survivor.Active || survivor.ConID == nil || *survivor.ConID != 265598 {
		t.Fatalf("survivor=%+v", survivor)
	}
	aliases := map[string]bool{}
	for _, alias := range survivor.Aliases {
		aliases[alias] = true
	}
	if !aliases["binance|AAPLUSDT"] || !aliases["okx|AAPLUSDT"] {
		t.Fatalf("aliases=%v", survivor.Aliases)
	}
	var clearedSymbol string
	for _, symbol := range []string{"stock|AAPL|USD|US", "stock|AAPL|USDT|US"} {
		var status string
		var conID *int64
		err := pool.QueryRow(ctx, `SELECT status, ib_con_id FROM instruments WHERE exchange='ibkr' AND exchange_symbol=$1`, symbol).Scan(&status, &conID)
		if err != nil {
			continue
		}
		if status == "inactive" && conID == nil {
			clearedSymbol = symbol
		}
	}
	if clearedSymbol == "" {
		t.Fatal("alias row was not cleared")
	}
	cleared, err := nextVerify(ctx, pool, clearedSymbol)
	if err != nil {
		t.Fatal(err)
	}
	if !cleared.Before(time.Now()) {
		t.Fatalf("cleared next_verify=%s", cleared)
	}
	unresolved, err := nextVerify(ctx, pool, "stock|OPENAI|USD|")
	if err != nil {
		t.Fatal(err)
	}
	if !unresolved.Before(time.Now()) {
		t.Fatalf("unresolved next_verify=%s", unresolved)
	}
	cryptoBefore, err := nextVerify(ctx, pool, "crypto|BTC|USD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.MigrateIBCanonical(ctx, false); err != nil {
		t.Fatal(err)
	}
	cryptoAfter, err := nextVerify(ctx, pool, "crypto|BTC|USD")
	if err != nil {
		t.Fatal(err)
	}
	if !cryptoBefore.Equal(cryptoAfter) {
		t.Fatalf("crypto next_verify changed %s -> %s", cryptoBefore, cryptoAfter)
	}
}

func TestUpsertCanonicalResolutionMergesAliasesAndPairs(t *testing.T) {
	ctx, pool := openCanonicalTestDB(t)
	repository := NewRepository(pool)
	future := time.Now().UTC().Add(24 * time.Hour)
	if _, err := pool.Exec(ctx, `
		INSERT INTO instruments (
			exchange, exchange_symbol, base_asset, quote_asset, global_symbol,
			active, settle_asset, contract_type, status, funding_interval_seconds,
			metadata, ib_con_id, next_verify_at
		) VALUES (
			'ibkr', 'conid:265598', 'AAPL', 'USD', 'IBKR:conid:265598',
			true, 'USD', 'spot', 'active', 0,
			$1, 265598, $2
		)`, []byte(`{"assetClass":"stock","underlyingSymbol":"AAPL","underlyingCurrency":"USD","region":"US","secType":"STK","candidateAliases":["AAPL"],"pairMatches":{"binance|AAPLUSDT":{"MatchClass":"verified_listing","PerpPriceCurrency":"USDT"}}}`), future); err != nil {
		t.Fatal(err)
	}
	conID := int64(265598)
	saved, err := repository.UpsertIBInstrumentResolution(ctx, IBResolution{
		UnderlyingSymbol: "AAPL", UnderlyingCurrency: "USD", AssetClass: "stock",
		Region: "US", SecType: "STK", Active: true, Status: "active", ConID: &conID,
		Aliases:      []string{"stock|AAPL|USD|US"},
		PairMatches:  map[string]PairMatch{"okx|AAPL-USDT-SWAP": {MatchClass: MatchVerifiedListing, PerpPriceCurrency: "USDT"}},
		LastVerified: time.Now().UTC(), NextVerify: future,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertMergedAAPL(t, saved)
	rows, err := repository.ListIBInstrumentResolutions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var stored IBResolution
	var found bool
	for _, row := range rows {
		if row.Symbol == "conid:265598" {
			stored = row
			found = true
		}
	}
	if !found {
		t.Fatal("canonical row missing after upsert")
	}
	assertMergedAAPL(t, stored)
}

func TestPersistLeavesMemoryUnchangedWhenUpsertFails(t *testing.T) {
	ctx, pool := openCanonicalTestDB(t)
	service := NewTradefiService(NewRepository(pool), nil, nil, nil, nil)
	conID := int64(265598)
	service.store(IBResolution{
		Symbol: "conid:265598", ConID: &conID, Active: true, Status: "active",
		Aliases: []string{"AAPL"},
	})
	failed, cancel := context.WithCancel(ctx)
	cancel()
	err := service.persist(failed, IBResolution{
		ConID: &conID, Active: false, Status: "inactive",
		Aliases: []string{"stock|AAPL|USD|US"},
	})
	if err == nil {
		t.Fatal("expected upsert to fail")
	}
	got, ok := service.cached("AAPL")
	if !ok || !got.Active || got.Status != "active" || got.Symbol != "conid:265598" {
		t.Fatalf("memory changed ok=%v item=%+v", ok, got)
	}
	if _, ok := service.cached("stock|AAPL|USD|US"); ok {
		t.Fatal("failed upsert installed an alias")
	}
}

func assertMergedAAPL(t *testing.T, item IBResolution) {
	t.Helper()
	aliases := map[string]bool{}
	for _, alias := range item.Aliases {
		aliases[alias] = true
	}
	if !aliases["AAPL"] || !aliases["stock|AAPL|USD|US"] {
		t.Fatalf("aliases=%v", item.Aliases)
	}
	if _, ok := item.PairMatches["binance|AAPLUSDT"]; !ok {
		t.Fatalf("pairs=%v", item.PairMatches)
	}
	if _, ok := item.PairMatches["okx|AAPL-USDT-SWAP"]; !ok {
		t.Fatalf("pairs=%v", item.PairMatches)
	}
}

func openCanonicalTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
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
	schema := fmt.Sprintf("canonical_test_%d", time.Now().UnixNano())
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
	return ctx, pool
}

func nextVerify(ctx context.Context, pool *pgxpool.Pool, symbol string) (time.Time, error) {
	var value time.Time
	err := pool.QueryRow(ctx, `SELECT next_verify_at FROM instruments WHERE exchange='ibkr' AND exchange_symbol=$1`, symbol).Scan(&value)
	return value.UTC(), err
}
