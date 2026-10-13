package trader

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"selfquant/backend/internal/database"
)

func TestFinalMetricsBackfillIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("trader_final_metrics_%d", time.Now().UnixNano())
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
	var indexName string
	if err := pool.QueryRow(ctx, `
		SELECT indexname FROM pg_indexes
		WHERE schemaname = current_schema()
		  AND indexname = 'trader_arbitrage_final_metrics_due_idx'`).Scan(&indexName); err != nil {
		t.Fatal(err)
	}

	repo := NewRepository(pool)
	closingID := createBackfillCombo(t, ctx, pool, repo, "closing-not-ready")
	completedID := createBackfillCombo(t, ctx, pool, repo, "exit-completed")
	dustID := createBackfillCombo(t, ctx, pool, repo, "closed-dust")
	oneShotDustID := createBackfillCombo(t, ctx, pool, repo, "oneshot-dust")
	timestampID := createBackfillCombo(t, ctx, pool, repo, "closed-timestamp")

	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET status='closing', run_mode='one_shot', one_shot_phase='waiting_exit',
		    last_failure_key='final_metrics_not_ready', error_message='metrics',
		    consecutive_failures=3, next_retry_at=now(),
		    metrics_calculated_at=now()
		WHERE id=$1::uuid`, closingID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET status='running', run_mode='one_shot', one_shot_phase='exited',
		    metrics_calculated_at=now()
		WHERE id=$1::uuid`, completedID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO trader_arbitrage_events(combination_id, event_type)
		VALUES ($1::uuid, 'one_shot_exit_completed')`, completedID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET status='closed' WHERE id=$1::uuid`, dustID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO trader_arbitrage_events(combination_id, event_type)
		VALUES ($1::uuid, 'closed_with_dust')`, dustID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET status='running', run_mode='one_shot', one_shot_phase='exited'
		WHERE id=$1::uuid`, oneShotDustID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO trader_arbitrage_events(combination_id, event_type)
		VALUES ($1::uuid, 'one_shot_exited_with_dust')`, oneShotDustID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET status='closed', metrics_calculated_at=now()
		WHERE id=$1::uuid`, timestampID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET final_metrics_status='pending',
		    final_metrics_retry_count=0,
		    final_metrics_last_error='',
		    final_metrics_next_retry_at='-infinity',
		    final_metrics_completed_at=NULL
		WHERE id = ANY($1::uuid[])`,
		[]string{closingID, completedID, dustID, oneShotDustID, timestampID},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, finalMetricsBackfillSQL(t)); err != nil {
		t.Fatal(err)
	}

	assertBackfill(t, ctx, pool, closingID, "pending", true, false, "closing", "waiting_exit", "")
	assertBackfill(t, ctx, pool, completedID, "completed", false, true, "running", "exited", "")
	assertBackfill(t, ctx, pool, dustID, "partial", false, false, "closed", "", "")
	assertBackfill(t, ctx, pool, oneShotDustID, "partial", false, false, "running", "exited", "")
	assertBackfill(t, ctx, pool, timestampID, "pending", false, false, "closed", "", "")
}

func createBackfillCombo(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, repo *Repository, key string,
) string {
	t.Helper()
	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", key+"A")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", key+"B")
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.ID = uuid.NewString()
	input.IdempotencyKey = key
	input.RequestFingerprint = key + "-fingerprint"
	input.LegA.ExchangeSymbol = key + "A"
	input.LegB.ExchangeSymbol = key + "B"
	input.LegA.AccountName = "binance-" + key
	input.LegB.AccountName = "okx-" + key
	created, inserted, err := repo.CreateArbitrageCombination(ctx, input)
	if err != nil || !inserted {
		t.Fatalf("create %s inserted=%v err=%v", key, inserted, err)
	}
	return created.ID
}

func assertBackfill(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	id, status string, due, completed bool, comboStatus, phase, failureKey string,
) {
	t.Helper()
	var gotStatus, gotComboStatus, gotPhase, gotFailure string
	var finite, hasCompleted bool
	if err := pool.QueryRow(ctx, `
		SELECT final_metrics_status, status, COALESCE(one_shot_phase, ''), last_failure_key,
		       final_metrics_next_retry_at < 'infinity'::timestamptz
		         AND final_metrics_next_retry_at > '-infinity'::timestamptz,
		       final_metrics_completed_at IS NOT NULL
		FROM trader_arbitrage_combinations WHERE id=$1::uuid`, id).Scan(
		&gotStatus, &gotComboStatus, &gotPhase, &gotFailure, &finite, &hasCompleted,
	); err != nil {
		t.Fatal(err)
	}
	if gotStatus != status || gotComboStatus != comboStatus || gotPhase != phase ||
		gotFailure != failureKey || finite != due || hasCompleted != completed {
		t.Fatalf("id=%s status=%s combo=%s phase=%s failure=%s finite=%v completed=%v",
			id, gotStatus, gotComboStatus, gotPhase, gotFailure, finite, hasCompleted)
	}
}

func finalMetricsBackfillSQL(t *testing.T) string {
	t.Helper()
	content, err := os.ReadFile("../database/migrations/061_trader_final_metrics_status.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(content)
	index := strings.Index(sql, "UPDATE trader_arbitrage_combinations")
	if index < 0 {
		t.Fatal("backfill update missing")
	}
	return sql[index:]
}
