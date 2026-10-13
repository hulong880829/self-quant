package trader

import (
	"testing"
)

func TestMarkOneShotExitingLiquidationDistance(t *testing.T) {
	ctx, pool := openArbitrageRunModeTestDB(t)
	repository := NewRepository(pool)
	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "LIQAUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "LIQB-USDT")
	item := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	item.IdempotencyKey = "arb-liq-cas"
	item.RequestFingerprint = "arb-liq-cas-fp"
	item.RunMode = "one_shot"
	item.EntryDirection = "ask"
	item.ExitPolicy = "time"
	item.ExitAfterSeconds = 3600
	item.OneShotPhase = "waiting_exit"
	item.AskThresholdBps = "0"
	item.BidThresholdBps = "0"
	item.EarlyExitLiquidationDistancePct = "20"
	created, inserted, err := repository.CreateArbitrageCombination(ctx, item)
	if err != nil || !inserted {
		t.Fatalf("inserted=%v err=%v", inserted, err)
	}
	if created.EarlyExitLiquidationDistancePct != "20" {
		t.Fatalf("persisted threshold=%q", created.EarlyExitLiquidationDistancePct)
	}
	for _, value := range []string{"'NaN'::numeric", "'Infinity'::numeric", "'-Infinity'::numeric", "0", "100"} {
		if _, execErr := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations
			SET early_exit_liquidation_distance_pct=`+value+`
			WHERE id=$1::uuid`, created.ID); execErr == nil {
			t.Fatalf("check accepted %s", value)
		}
	}

	if _, applied, err := repository.MarkOneShotExiting(
		ctx, created.ID, created.Version, "liquidation_distance_threshold",
		map[string]any{"reason": "liquidation_distance_threshold"},
	); err != nil || applied {
		t.Fatalf("unhit CAS applied=%v err=%v", applied, err)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET leg_a_liquidation_distance_pct=25,
		    leg_b_liquidation_distance_pct=NULL
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, applied, err := repository.MarkOneShotExiting(
		ctx, created.ID, created.Version, "liquidation_distance_threshold", nil,
	); err != nil || applied {
		t.Fatalf("above-threshold CAS applied=%v err=%v", applied, err)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET leg_a_liquidation_distance_pct=18.6,
		    leg_b_liquidation_distance_pct=35.2
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	exiting, applied, err := repository.MarkOneShotExiting(
		ctx, created.ID, created.Version, "liquidation_distance_threshold",
		map[string]any{"reason": "liquidation_distance_threshold", "triggeredLeg": "a"},
	)
	if err != nil || !applied || exiting.OneShotPhase != "exiting" {
		t.Fatalf("applied=%v err=%v combo=%+v", applied, err, exiting)
	}
	if _, applied, err := repository.MarkOneShotExiting(
		ctx, created.ID, exiting.Version, "liquidation_distance_threshold", nil,
	); err != nil || applied {
		t.Fatalf("second CAS applied=%v err=%v", applied, err)
	}
}
