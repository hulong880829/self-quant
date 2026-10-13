package trader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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
			scheduled_exit_at=now() - interval '1 second',
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

func TestCreateArbitrageCombinationExitAfterSecondsTyping(t *testing.T) {
	ctx, pool := openArbitrageRunModeTestDB(t)
	repository := NewRepository(pool)

	t.Run("spread_null_period", func(t *testing.T) {
		accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "NULLPERAUSDT")
		accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "NULLPERB-USDT")
		input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
		input.IdempotencyKey = "arb-spread-null-period"
		input.RequestFingerprint = "arb-spread-null-period-fp"
		input.ExitAfterSeconds = 0
		created, inserted, err := repository.CreateArbitrageCombination(ctx, input)
		if err != nil {
			if strings.Contains(err.Error(), "42P08") {
				t.Fatalf("spread create hit untyped NULL: %v", err)
			}
			t.Fatal(err)
		}
		if !inserted {
			t.Fatal("spread create not inserted")
		}
		assertExitPeriodColumns(t, ctx, pool, created.ID, true, true, 0)
	})

	t.Run("one_shot_time_scheduled_exit", func(t *testing.T) {
		accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "SCHEDAUSDT")
		accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "SCHEDB-USDT")
		input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
		input.IdempotencyKey = "arb-oneshot-86400"
		input.RequestFingerprint = "arb-oneshot-86400-fp"
		input.RunMode = "one_shot"
		input.EntryDirection = "ask"
		input.ExitPolicy = "time"
		input.ExitAfterSeconds = 86400
		input.OneShotPhase = "building_target"
		created, inserted, err := repository.CreateArbitrageCombination(ctx, input)
		if err != nil || !inserted {
			t.Fatalf("inserted=%v err=%v", inserted, err)
		}
		assertExitPeriodColumns(t, ctx, pool, created.ID, false, false, 86400)
	})

	t.Run("idempotent_replay", func(t *testing.T) {
		accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "IDEMAUSDT")
		accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "IDEMB-USDT")
		input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
		input.IdempotencyKey = "arb-spread-idem-replay"
		input.RequestFingerprint = "arb-spread-idem-replay-fp"
		created, inserted, err := repository.CreateArbitrageCombination(ctx, input)
		if err != nil || !inserted {
			t.Fatalf("create inserted=%v err=%v", inserted, err)
		}
		replay := input
		replay.ID = uuid.NewString()
		reused, inserted, err := repository.CreateArbitrageCombination(ctx, replay)
		if err != nil || inserted || reused.ID != created.ID {
			t.Fatalf("reuse inserted=%v id=%s want=%s err=%v", inserted, reused.ID, created.ID, err)
		}
	})
}

func TestOneShotTimeCreateAndPatchEnablement(t *testing.T) {
	ctx, pool := openArbitrageRunModeTestDB(t)
	repository := NewRepository(pool)
	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "TIMEAUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "TIMEB-USDT")
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.IdempotencyKey = "arb-time-create"
	input.RequestFingerprint = "arb-time-create-fp"
	input.RunMode = "one_shot"
	input.EntryDirection = "ask"
	input.ExitPolicy = "time"
	input.ExitAfterSeconds = 86400
	input.ExitAnnualizedRate = "0.15"
	input.EarlyExitFunding8hAnnualizedFloor = "0.05"
	input.EarlyExitLiquidationDistancePct = "20"
	input.OneShotPhase = "building_target"
	created, inserted, err := repository.CreateArbitrageCombination(ctx, input)
	if err != nil || !inserted {
		t.Fatalf("inserted=%v err=%v", inserted, err)
	}
	if created.ScheduledExitAt.IsZero() {
		t.Fatal("scheduled_exit_at is zero")
	}
	period := created.ScheduledExitAt.Sub(created.CreatedAt)
	if period < 86399*time.Second || period > 86401*time.Second {
		t.Fatalf("scheduled_exit_at=%s created_at=%s period=%s", created.ScheduledExitAt, created.CreatedAt, period)
	}
	rate := "0.20"
	updated, err := repository.UpdateArbitrageCombinationConfig(ctx, "admin", created.ID, UpdateArbitrageInput{
		ExitAnnualizedRate: &rate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ExitAnnualizedRate != "0.2" && updated.ExitAnnualizedRate != "0.20" {
		t.Fatalf("rate=%q", updated.ExitAnnualizedRate)
	}
	target := "12000"
	exitAfter := 14400
	liquidationDistance := "15"
	updated, err = repository.UpdateArbitrageCombinationConfig(
		ctx, "admin", created.ID,
		UpdateArbitrageInput{
			TargetNotional:                  &target,
			ExitAfterSeconds:                &exitAfter,
			EarlyExitLiquidationDistancePct: &liquidationDistance,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	wantScheduled := created.CreatedAt.Add(4 * time.Hour)
	if updated.TargetNotional != target ||
		updated.EarlyExitLiquidationDistancePct != liquidationDistance ||
		!updated.CreatedAt.Equal(created.CreatedAt) ||
		!updated.ScheduledExitAt.Equal(wantScheduled) {
		t.Fatalf("updated=%+v want scheduled=%s", updated, wantScheduled)
	}
	persisted, err := repository.GetArbitrageCombinationByOwner(
		ctx, "admin", created.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.TargetNotional != target ||
		persisted.EarlyExitLiquidationDistancePct != liquidationDistance ||
		!persisted.ScheduledExitAt.Equal(wantScheduled) {
		t.Fatalf("persisted=%+v want scheduled=%s", persisted, wantScheduled)
	}
	enable := "0.12"
	blankA, blankInstrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "TIMEBLANKUSDT")
	blankB, blankInstrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "TIMEBLANK-USDT")
	blank := integrationArbitrageCombination(blankA, blankInstrumentA, blankB, blankInstrumentB)
	blank.IdempotencyKey = "arb-time-no-rate"
	blank.RequestFingerprint = "arb-time-no-rate-fp"
	blank.RunMode = "one_shot"
	blank.EntryDirection = "ask"
	blank.ExitPolicy = "time"
	blank.ExitAfterSeconds = 3600
	blank.OneShotPhase = "building_target"
	createdBlank, inserted, err := repository.CreateArbitrageCombination(ctx, blank)
	if err != nil || !inserted {
		t.Fatalf("blank inserted=%v err=%v", inserted, err)
	}
	if _, err := repository.UpdateArbitrageCombinationConfig(ctx, "admin", createdBlank.ID, UpdateArbitrageInput{
		ExitAnnualizedRate: &enable,
	}); !errors.Is(err, ErrArbitrageConfigInvalid) {
		t.Fatalf("enable after create err=%v", err)
	}
}

func TestOneShotBuildingTargetTimeExitAndFinalZeroFill(t *testing.T) {
	ctx, pool := openArbitrageRunModeTestDB(t)
	repository := NewRepository(pool)
	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "ZEROAUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "ZEROB-USDT")
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.IdempotencyKey = "arb-zero-fill"
	input.RequestFingerprint = "arb-zero-fill-fp"
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
			scheduled_exit_at=now() - interval '1 second',
			leg_a_base_position=0,
			leg_b_base_position=0
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	version := arbitrageVersion(t, ctx, pool, created.ID)
	exiting, applied, err := repository.MarkOneShotExiting(ctx, created.ID, version, "time", nil)
	if err != nil || !applied {
		t.Fatalf("building_target time exit applied=%v err=%v", applied, err)
	}
	listed, err := repository.ListRunningArbitrageCombinationsForMetrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range listed {
		if item.ID == created.ID {
			t.Fatal("exiting combination listed for periodic metrics")
		}
	}
	exited, applied, err := repository.MarkOneShotExited(ctx, created.ID, exiting.Version)
	if err != nil || !applied {
		t.Fatalf("zero-fill exited applied=%v err=%v", applied, err)
	}
	if exited.OneShotPhase != "exited" || exited.FinalMetricsStatus != finalMetricsPending {
		t.Fatalf("exited=%+v", exited)
	}
	persisted, err := repository.PersistFinalArbitragePositionMetrics(
		ctx, created.ID, exited.Version, false, time.Time{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.CombinedPositionAnnualized != "" {
		t.Fatalf("zero-fill annualized=%q", persisted.CombinedPositionAnnualized)
	}
	if persisted.FinalMetricsStatus != finalMetricsCompleted || persisted.Version != exited.Version {
		t.Fatalf("persisted=%+v", persisted)
	}
}

func TestMarkArbitrageClosedAfterFinalMetricsCAS(t *testing.T) {
	ctx, pool := openArbitrageRunModeTestDB(t)
	repository := NewRepository(pool)
	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "CLOSEAUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "CLOSEB-USDT")
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.IdempotencyKey = "arb-closed-cas"
	input.RequestFingerprint = "arb-closed-cas-fp"
	created, inserted, err := repository.CreateArbitrageCombination(ctx, input)
	if err != nil || !inserted {
		t.Fatalf("inserted=%v err=%v", inserted, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET status='closing' WHERE id=$1::uuid`,
		created.ID); err != nil {
		t.Fatal(err)
	}
	version := arbitrageVersion(t, ctx, pool, created.ID)
	closed, applied, err := repository.MarkArbitrageClosedAndScheduleFinalMetrics(ctx, created.ID, version)
	if err != nil || !applied {
		t.Fatalf("applied=%v err=%v", applied, err)
	}
	if closed.Status != "closed" || closed.Version != version+1 {
		t.Fatalf("closed=%+v", closed)
	}
	if _, applied, err := repository.MarkArbitrageClosedAndScheduleFinalMetrics(ctx, created.ID, version); err != nil || applied {
		t.Fatalf("stale version applied=%v err=%v", applied, err)
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

func assertExitPeriodColumns(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	combinationID string,
	wantExitNull bool,
	wantScheduledNull bool,
	wantSeconds int,
) {
	t.Helper()
	var exitNull, scheduledNull bool
	var seconds *int
	var createdAt, scheduledAt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT
			exit_after_seconds IS NULL,
			scheduled_exit_at IS NULL,
			exit_after_seconds,
			created_at,
			scheduled_exit_at
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid`, combinationID,
	).Scan(&exitNull, &scheduledNull, &seconds, &createdAt, &scheduledAt); err != nil {
		t.Fatal(err)
	}
	if exitNull != wantExitNull || scheduledNull != wantScheduledNull {
		t.Fatalf(
			"exit_after_seconds NULL=%v want=%v scheduled_exit_at NULL=%v want=%v",
			exitNull, wantExitNull, scheduledNull, wantScheduledNull,
		)
	}
	if wantExitNull {
		return
	}
	if seconds == nil || *seconds != wantSeconds {
		t.Fatalf("exit_after_seconds=%v want=%d", seconds, wantSeconds)
	}
	if createdAt == nil || scheduledAt == nil {
		t.Fatal("created_at or scheduled_exit_at missing")
	}
	period := scheduledAt.Sub(*createdAt)
	want := time.Duration(wantSeconds) * time.Second
	if period < want-time.Second || period > want+time.Second {
		t.Fatalf("scheduled_exit_at=%s created_at=%s period=%s want=%s", scheduledAt, createdAt, period, want)
	}
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
