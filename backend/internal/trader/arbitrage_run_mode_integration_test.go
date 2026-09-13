package trader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"selfquant/backend/internal/database"
)

func TestArbitrageRunModePersistenceAndClaimAbort(t *testing.T) {
	ctx, pool := openArbitrageRunModeTestDB(t)
	repository := NewRepository(pool)
	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "RUNMODEAUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "RUNMODEB-USDT")

	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.IdempotencyKey = "arb-run-mode-null-notional"
	input.RequestFingerprint = "arb-run-mode-null-fp"
	input.OrderNotional = ""
	created, inserted, err := repository.CreateArbitrageCombination(ctx, input)
	if err != nil || !inserted {
		t.Fatalf("inserted=%v err=%v", inserted, err)
	}
	if created.OrderNotional != "" {
		t.Fatalf("new combination order_notional=%q", created.OrderNotional)
	}

	loaded, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.OrderNotional != "" || loaded.RunMode != "spread" {
		t.Fatalf("loaded=%+v", loaded)
	}
	listed, _, err := repository.ListArbitrageCombinations(ctx, "admin", "running", 50, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range listed {
		if item.ID == created.ID {
			found = true
			if item.OrderNotional != "" {
				t.Fatalf("list scanned order_notional=%q", item.OrderNotional)
			}
		}
	}
	if !found {
		t.Fatal("created combination missing from list")
	}

	if _, applied, err := repository.MarkOneShotExiting(ctx, created.ID, created.Version, "annualized", nil); err != nil || applied {
		t.Fatalf("spread exiting applied=%v err=%v", applied, err)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			leg_a_base_position=100, leg_b_base_position=-100
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	execution := integrationArbitrageExecution(created.ID, "ask")
	_, claimed, err := repository.ClaimArbitrageExecution(ctx, execution)
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("claim should abort when remaining is below requested notional")
	}
	var executionCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM trader_arbitrage_executions WHERE combination_id=$1::uuid`,
		created.ID,
	).Scan(&executionCount); err != nil {
		t.Fatal(err)
	}
	if executionCount != 0 {
		t.Fatalf("aborted claim inserted %d executions", executionCount)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			run_mode='one_shot',
			entry_direction='ask',
			exit_policy='time',
			exit_after_seconds=3600,
			exit_annualized_rate=NULL,
			one_shot_phase='waiting_exit',
			status='running'
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	var version int64
	if err := pool.QueryRow(ctx, `
		SELECT version FROM trader_arbitrage_combinations WHERE id=$1::uuid`, created.ID,
	).Scan(&version); err != nil {
		t.Fatal(err)
	}
	exiting, applied, err := repository.MarkOneShotExiting(ctx, created.ID, version, "time", nil)
	if err != nil || !applied {
		t.Fatalf("waiting_exit exiting applied=%v err=%v", applied, err)
	}
	if exiting.Status != "running" || exiting.OneShotPhase != "exiting" {
		t.Fatalf("combination=%+v", exiting)
	}
	if _, applied, err := repository.MarkOneShotExiting(ctx, created.ID, exiting.Version, "time", nil); err != nil || applied {
		t.Fatalf("second exiting applied=%v err=%v", applied, err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM trader_arbitrage_executions WHERE combination_id=$1::uuid`,
		created.ID,
	).Scan(&executionCount); err != nil {
		t.Fatal(err)
	}
	if executionCount != 0 {
		t.Fatalf("exiting flip claimed executions=%d", executionCount)
	}
	backoff, err := repository.RecordArbitrageCloseFailure(
		ctx, created.ID, "close_rejected", "reduce-only rejected",
	)
	if err != nil {
		t.Fatal(err)
	}
	if backoff.Status != "running" || backoff.OneShotPhase != "exiting" ||
		backoff.RuntimeState != "backoff" {
		t.Fatalf("one-shot close failure=%+v", backoff)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			runtime_state='manual_intervention'
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	staleRuntime := backoff
	staleRuntime.RuntimeState = "monitoring"
	staleRuntime.ErrorMessage = ""
	stickyManual, err := repository.UpdateArbitrageCombinationRuntime(
		ctx, staleRuntime,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stickyManual.RuntimeState != "manual_intervention" {
		t.Fatalf("stale runtime cleared manual intervention: %+v", stickyManual)
	}
	if _, err := repository.RecordArbitrageCloseFailure(
		ctx, created.ID, "close_rejected", "repeat",
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("manual intervention close failure err=%v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			runtime_state='monitoring',
			next_retry_at='-infinity'
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			leg_a_base_position=5,
			leg_b_base_position=-5,
			leg_a_venue_baseline_base_position=100,
			leg_b_venue_baseline_base_position=-100,
			venue_baseline_captured_at=now()
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	oversizedClose := integrationArbitrageExecution(created.ID, "bid")
	oversizedClose.PositionEffect = "close"
	oversizedClose.ReduceOnly = true
	oversizedClose.TargetBaseQuantity = "6"
	oversizedClose.RequestedNotional = "612"
	if _, claimed, claimErr := repository.ClaimArbitrageExecution(
		ctx, oversizedClose,
	); claimErr != nil || claimed {
		t.Fatalf("owned cap claimed=%v err=%v", claimed, claimErr)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			leg_a_venue_baseline_base_position=-200,
			leg_b_venue_baseline_base_position=200
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	reverseVenueClose := integrationArbitrageExecution(created.ID, "bid")
	reverseVenueClose.PositionEffect = "close"
	reverseVenueClose.ReduceOnly = true
	if _, claimed, claimErr := repository.ClaimArbitrageExecution(
		ctx, reverseVenueClose,
	); claimErr != nil || claimed {
		t.Fatalf("reverse venue claimed=%v err=%v", claimed, claimErr)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			leg_a_venue_baseline_base_position=100,
			leg_b_venue_baseline_base_position=-100
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	validClose := integrationArbitrageExecution(created.ID, "bid")
	validClose.PositionEffect = "close"
	validClose.ReduceOnly = true
	claimedClose, claimed, claimErr := repository.ClaimArbitrageExecution(
		ctx, validClose,
	)
	if claimErr != nil || !claimed {
		t.Fatalf("valid owned close claimed=%v err=%v", claimed, claimErr)
	}
	claimedClose.Status = "completed"
	if _, err := repository.UpdateArbitrageExecution(ctx, claimedClose); err != nil {
		t.Fatal(err)
	}
	exitVersion := arbitrageVersion(t, ctx, pool, created.ID)
	if _, applied, err := repository.MarkOneShotExited(
		ctx, created.ID, exitVersion,
	); err != nil || applied {
		t.Fatalf("non-flat exited applied=%v err=%v", applied, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			leg_a_base_position=0,
			leg_b_base_position=0
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	exited, applied, err := repository.MarkOneShotExited(
		ctx, created.ID, exitVersion,
	)
	if err != nil || !applied {
		t.Fatalf("flat exited applied=%v err=%v", applied, err)
	}
	if exited.Status != "running" || exited.OneShotPhase != "exited" {
		t.Fatalf("exited combination=%+v", exited)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			status='running',
			runtime_state='monitoring',
			exit_after_seconds=604800
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatalf("604800 constraint rejected: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET exit_after_seconds=123
		WHERE id=$1::uuid`, created.ID); err == nil {
		t.Fatal("unsupported hold seconds should fail constraint")
	}
	closing, err := repository.MarkArbitrageClosing(ctx, "admin", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closing.Status != "closing" || closing.OneShotPhase != "exited" {
		t.Fatalf("user close from exited=%+v", closing)
	}
}

func openArbitrageRunModeTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	schema := fmt.Sprintf("trader_run_mode_%d", time.Now().UnixNano())
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
	t.Cleanup(func() { pool.Close() })
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return ctx, pool
}

func TestMarkOneShotExitedWithDustCAS(t *testing.T) {
	ctx, pool := openArbitrageRunModeTestDB(t)
	repository := NewRepository(pool)
	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "DUSTAUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "DUSTB-USDT")
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.IdempotencyKey = "arb-one-shot-dust-cas"
	input.RequestFingerprint = "arb-one-shot-dust-fp"
	input.RunMode = "one_shot"
	input.EntryDirection = "ask"
	input.ExitPolicy = "time"
	input.ExitAfterSeconds = 3600
	input.OneShotPhase = "building_target"
	created, inserted, err := repository.CreateArbitrageCombination(ctx, input)
	if err != nil || !inserted {
		t.Fatalf("inserted=%v err=%v", inserted, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			one_shot_phase='exiting',
			leg_a_base_position=0,
			leg_b_base_position=-10,
			carry_base_quantity=-10,
			runtime_state='monitoring',
			position_uncertain=false
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	version := arbitrageVersion(t, ctx, pool, created.ID)
	if _, applied, err := repository.MarkOneShotExited(
		ctx, created.ID, version,
	); err != nil || applied {
		t.Fatalf("non-zero MarkOneShotExited applied=%v err=%v", applied, err)
	}

	liveExec := integrationArbitrageExecution(created.ID, "ask")
	if _, err := pool.Exec(ctx, `
		INSERT INTO trader_arbitrage_executions (
			id, combination_id, direction, sequence, status,
			trigger_ask_spread_bps, trigger_bid_spread_bps,
			trigger_leg_a_bid, trigger_leg_a_ask, trigger_leg_b_bid, trigger_leg_b_ask,
			target_base_quantity, requested_notional
		) VALUES (
			$1::uuid, $2::uuid, 'ask', 1, 'claimed',
			12, -8, 1, 1, 1, 1, 1, 1
		)`, liveExec.ID, created.ID); err != nil {
		t.Fatal(err)
	}
	orderID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO trader_orders (
			id, idempotency_key, owner_username, trading_account_id, product_name,
			exchange, instrument_id, contract_type, exchange_symbol, client_order_id,
			side, order_type, quantity, status, request_fingerprint,
			base_asset, quote_asset, arbitrage_execution_id, filled_quantity
		) VALUES (
			$1::uuid, $2, 'admin', $3, 'ARBITRAGE',
			'okx', $4, 'perpetual', 'DUSTB-USDT', $5,
			'buy', 'market', 10, 'open', $6,
			'BTC', 'USDT', $7::uuid, 0
		)`, orderID, "dust-live-"+orderID, accountB, instrumentB,
		"dust-client-"+orderID, "dust-fp-"+orderID, liveExec.ID); err != nil {
		t.Fatal(err)
	}
	if _, applied, err := repository.MarkOneShotExitedWithDust(
		ctx, created.ID, version, "0", "-10", "-10",
	); err != nil || applied {
		t.Fatalf("live order applied=%v err=%v", applied, err)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE trader_orders SET status='canceled' WHERE id=$1::uuid`, orderID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_executions SET status='canceled' WHERE id=$1::uuid`, liveExec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET circuit_open=true WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, applied, err := repository.MarkOneShotExitedWithDust(
		ctx, created.ID, version, "0", "-10", "-10",
	); err != nil || applied {
		t.Fatalf("circuit_open applied=%v err=%v", applied, err)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET circuit_open=false WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, applied, err := repository.MarkOneShotExitedWithDust(
		ctx, created.ID, version, "0", "-10", "0",
	); err != nil || applied {
		t.Fatalf("carry=0 expected applied=%v err=%v", applied, err)
	}
	if _, applied, err := repository.MarkOneShotExitedWithDust(
		ctx, created.ID, version, "0", "-9", "-10",
	); err != nil || applied {
		t.Fatalf("expected mismatch applied=%v err=%v", applied, err)
	}

	exited, applied, err := repository.MarkOneShotExitedWithDust(
		ctx, created.ID, version, "0", "-10", "-10",
	)
	if err != nil || !applied {
		t.Fatalf("matching dust applied=%v err=%v", applied, err)
	}
	if exited.OneShotPhase != "exited" || exited.Status != "running" ||
		exited.LegBBasePosition != "-10" || exited.CarryBaseQuantity != "-10" {
		t.Fatalf("exited=%+v", exited)
	}
}
