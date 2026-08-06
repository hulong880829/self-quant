package database

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigrationsAreVersionedAndIdempotent(t *testing.T) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected versioned migrations, got %d", len(entries))
	}
	for _, entry := range entries {
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		sql := strings.ToUpper(string(content))
		if strings.Contains(sql, "DROP TABLE INSTRUMENTS") ||
			strings.Contains(sql, "DROP TABLE FUNDING_RATES") {
			t.Fatalf("%s contains destructive table migration", entry.Name())
		}
	}
}

func TestMigratePostgresIntegration(t *testing.T) {
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
	schema := fmt.Sprintf("funding_test_%d", time.Now().UnixNano())
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
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second migration must be a no-op: %v", err)
	}
	var versions int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != len(mustMigrationEntries(t)) {
		t.Fatalf("versions=%d", versions)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO instruments (
			exchange,exchange_symbol,base_asset,quote_asset,global_symbol)
		VALUES ('test','BTCUSDT','BTC','USDT','BTCUSDT')`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var instruments int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM instruments").Scan(&instruments); err != nil {
		t.Fatal(err)
	}
	if instruments != 1 {
		t.Fatalf("migration removed existing data")
	}
}

func mustMigrationEntries(t *testing.T) []fs.DirEntry {
	t.Helper()
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
