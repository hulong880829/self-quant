package trader

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMarkOneShotAnnualizedExitingIgnoresUnrelatedVersion(t *testing.T) {
	ctx, pool := openArbitrageRunModeTestDB(t)
	repository := NewRepository(pool)
	loaded, guard := seedAnnualizedExitCombo(t, ctx, pool, repository, "audit-version")
	before := loaded.Version
	audited, err := repository.UpdateArbitragePositionAudit(
		ctx, loaded.ID, loaded.Version,
		loaded.LegAVenueBasePosition, loaded.LegBVenueBasePosition,
		loaded.LegAPositionDifference, loaded.LegBPositionDifference,
		false, "", "12.5", "8.5",
	)
	if err != nil {
		t.Fatal(err)
	}
	if audited.Version != before+1 {
		t.Fatalf("audit version=%d before=%d", audited.Version, before)
	}
	if audited.LegAVenueBasePosition != loaded.LegAVenueBasePosition ||
		audited.LegBPositionDifference != loaded.LegBPositionDifference ||
		!audited.MetricsCalculatedAt.Equal(loaded.MetricsCalculatedAt) {
		t.Fatalf("audit changed exit inputs audited=%+v loaded=%+v", audited, loaded)
	}

	exiting, applied, err := repository.MarkOneShotAnnualizedExiting(ctx, guard)
	if err != nil || !applied || exiting.OneShotPhase != "exiting" {
		t.Fatalf("applied=%v err=%v phase=%s", applied, err, exiting.OneShotPhase)
	}
	if exiting.Version != audited.Version+1 {
		t.Fatalf("exit version=%d audit=%d", exiting.Version, audited.Version)
	}
	assertAnnualizedExitEvent(t, ctx, pool, loaded.ID, 1)

	scheduler := &ArbitrageScheduler{store: repository}
	runtime := &arbitrageRuntime{combination: ArbitrageCombination{
		ID: loaded.ID, OwnerUsername: "admin", Status: "running",
		RunMode: "one_shot", OneShotPhase: "waiting_exit", Version: before,
	}}
	stop, refreshed := scheduler.reloadCombination(ctx, runtime)
	if stop || !refreshed || !oneShotRunningExiting(runtime.combination) {
		t.Fatalf("reload stop=%v refreshed=%v combo=%+v", stop, refreshed, runtime.combination)
	}
	if runtime.combination.Version != exiting.Version ||
		runtime.combination.OneShotPhase != "exiting" {
		t.Fatalf("reload did not adopt exiting combo=%+v", runtime.combination)
	}
}

func TestMarkOneShotAnnualizedExitingRejectsSemanticChanges(t *testing.T) {
	ctx, pool := openArbitrageRunModeTestDB(t)
	repository := NewRepository(pool)
	cases := []struct {
		name string
		sql  string
	}{
		{"threshold", "exit_annualized_rate=3"},
		{"metrics time", "metrics_calculated_at=metrics_calculated_at + interval '1 second'"},
		{"combo position", "leg_a_base_position=leg_a_base_position+1"},
		{"average", "leg_a_average_entry_price=leg_a_average_entry_price+1"},
		{"baseline", "leg_a_venue_baseline_base_position=leg_a_venue_baseline_base_position+1"},
		{"venue position", "leg_a_venue_base_position=leg_a_venue_base_position+1"},
		{"difference", "leg_a_position_difference=leg_a_position_difference+1"},
		{"uncertain", "position_uncertain=TRUE"},
		{"manual", "runtime_state='manual_intervention'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loaded, guard := seedAnnualizedExitCombo(t, ctx, pool, repository, tc.name)
			if _, err := pool.Exec(ctx, `
				UPDATE trader_arbitrage_combinations SET `+tc.sql+`
				WHERE id=$1::uuid`, loaded.ID); err != nil {
				t.Fatal(err)
			}
			_, applied, err := repository.MarkOneShotAnnualizedExiting(ctx, guard)
			if err != nil || applied {
				t.Fatalf("applied=%v err=%v", applied, err)
			}
		})
	}

	t.Run("active execution", func(t *testing.T) {
		loaded, guard := seedAnnualizedExitCombo(t, ctx, pool, repository, "active-exec")
		if _, err := pool.Exec(ctx, `
			INSERT INTO trader_arbitrage_executions (
				id,combination_id,direction,sequence,status,
				trigger_ask_spread_bps,trigger_bid_spread_bps,
				trigger_leg_a_bid,trigger_leg_a_ask,trigger_leg_b_bid,trigger_leg_b_ask,
				target_base_quantity
			) VALUES ($1::uuid,$2::uuid,'ask',1,'claimed',12,-8,100,101,99,102,1)`,
			uuid.NewString(), loaded.ID); err != nil {
			t.Fatal(err)
		}
		_, applied, err := repository.MarkOneShotAnnualizedExiting(ctx, guard)
		if err != nil || applied {
			t.Fatalf("applied=%v err=%v", applied, err)
		}
	})

	t.Run("filled nonterminal order", func(t *testing.T) {
		loaded, guard := seedAnnualizedExitCombo(t, ctx, pool, repository, "open-fill")
		execID := uuid.NewString()
		if _, err := pool.Exec(ctx, `
			INSERT INTO trader_arbitrage_executions (
				id,combination_id,direction,sequence,status,
				trigger_ask_spread_bps,trigger_bid_spread_bps,
				trigger_leg_a_bid,trigger_leg_a_ask,trigger_leg_b_bid,trigger_leg_b_ask,
				target_base_quantity
			) VALUES ($1::uuid,$2::uuid,'ask',1,'completed',12,-8,100,101,99,102,1)`,
			execID, loaded.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO trader_orders (
				id, idempotency_key, owner_username, trading_account_id, product_name,
				exchange, instrument_id, contract_type, exchange_symbol, client_order_id,
				side, order_type, quantity, status, request_fingerprint,
				base_asset, quote_asset, arbitrage_execution_id, filled_quantity
			) VALUES (
				$1::uuid, $2, 'admin', $3, 'ARBITRAGE',
				'binance', $4, 'perpetual', $5, $6,
				'buy', 'market', 1, 'open', $7,
				'BTC', 'USDT', $8::uuid, 0.1
			)`, uuid.NewString(), "ann-open-"+execID, loaded.LegA.TradingAccountID,
			loaded.LegA.InstrumentID, loaded.LegA.ExchangeSymbol, "ann-client-"+execID,
			"ann-fp-"+execID, execID); err != nil {
			t.Fatal(err)
		}
		_, applied, err := repository.MarkOneShotAnnualizedExiting(ctx, guard)
		if err != nil || applied {
			t.Fatalf("applied=%v err=%v", applied, err)
		}
	})
}

func TestMarkOneShotAnnualizedExitingOnce(t *testing.T) {
	ctx, pool := openArbitrageRunModeTestDB(t)
	repository := NewRepository(pool)
	loaded, guard := seedAnnualizedExitCombo(t, ctx, pool, repository, "once")
	var appliedCount int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, applied, err := repository.MarkOneShotAnnualizedExiting(ctx, guard)
			if err != nil {
				t.Error(err)
				return
			}
			if applied {
				mu.Lock()
				appliedCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if appliedCount != 1 {
		t.Fatalf("applied=%d", appliedCount)
	}
	assertAnnualizedExitEvent(t, ctx, pool, loaded.ID, 1)
	reloaded, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", loaded.ID)
	if err != nil || reloaded.OneShotPhase != "exiting" {
		t.Fatalf("phase=%s err=%v", reloaded.OneShotPhase, err)
	}
}

func seedAnnualizedExitCombo(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	suffix string,
) (ArbitrageCombination, oneShotAnnualizedExitGuard) {
	t.Helper()
	suffix = strings.ReplaceAll(suffix, " ", "")
	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "ANNA"+suffix)
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "ANNB"+suffix)
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.IdempotencyKey = "ann-exit-" + suffix
	input.RequestFingerprint = "ann-exit-fp-" + suffix
	input.LegA.ExchangeSymbol = "ANNA" + suffix
	input.LegB.ExchangeSymbol = "ANNB" + suffix
	created, inserted, err := repository.CreateArbitrageCombination(ctx, input)
	if err != nil || !inserted {
		t.Fatalf("inserted=%v err=%v", inserted, err)
	}
	calculatedAt := time.Now().UTC().Add(-time.Minute)
	baselineAt := calculatedAt.Add(-time.Hour)
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			run_mode='one_shot',
			one_shot_phase='waiting_exit',
			status='running',
			runtime_state='monitoring',
			position_uncertain=FALSE,
			exit_policy='annualized',
			exit_annualized_rate=2,
			leg_a_base_position=1,
			leg_b_base_position=-1,
			leg_a_average_entry_price=100,
			leg_b_average_entry_price=101,
			leg_a_venue_baseline_base_position=0,
			leg_b_venue_baseline_base_position=0,
			venue_baseline_captured_at=$2,
			metrics_calculated_at=$3,
			leg_a_venue_base_position=1,
			leg_b_venue_base_position=-1,
			leg_a_position_difference=0,
			leg_b_position_difference=0
		WHERE id=$1::uuid`, created.ID, baselineAt, calculatedAt); err != nil {
		t.Fatal(err)
	}
	loaded, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return loaded, annualizedExitGuard(oneShotExitBasis{
		CombinationID:      loaded.ID,
		Version:            loaded.Version,
		ExitAnnualizedRate: parseDecimal(loaded.ExitAnnualizedRate),
		ComboA:             parseDecimal(loaded.LegABasePosition),
		ComboB:             parseDecimal(loaded.LegBBasePosition),
		AvgA:               parseDecimal(loaded.LegAAverageEntryPrice),
		AvgB:               parseDecimal(loaded.LegBAverageEntryPrice),
		BaselineA:          parseDecimal(loaded.LegAVenueBaselineBasePosition),
		BaselineB:          parseDecimal(loaded.LegBVenueBaselineBasePosition),
		BaselineCapturedAt: loaded.VenueBaselineCapturedAt,
		VenueA:             parseDecimal(loaded.LegAVenueBasePosition),
		VenueB:             parseDecimal(loaded.LegBVenueBasePosition),
		DifferenceA:        parseDecimal(loaded.LegAPositionDifference),
		DifferenceB:        parseDecimal(loaded.LegBPositionDifference),
		CalculatedAt:       loaded.MetricsCalculatedAt,
	})
}

func assertAnnualizedExitEvent(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	combinationID string,
	want int,
) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM trader_arbitrage_events
		WHERE combination_id=$1::uuid AND event_type='one_shot_exit_started'`,
		combinationID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("exit events=%d want=%d", count, want)
	}
	var payload []byte
	if err := pool.QueryRow(ctx, `
		SELECT payload FROM trader_arbitrage_events
		WHERE combination_id=$1::uuid AND event_type='one_shot_exit_started'
		ORDER BY created_at DESC LIMIT 1`, combinationID,
	).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	if event["reason"] != "annualized" {
		t.Fatalf("event=%v", event)
	}
}
