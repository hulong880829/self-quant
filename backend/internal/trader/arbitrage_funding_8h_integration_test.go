package trader

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

func TestOneShotFunding8hExitIntegration(t *testing.T) {
	ctx, pool := openArbitrageRunModeTestDB(t)
	repository := NewRepository(pool)
	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "F8HAUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "F8HB-USDT")
	accountC, instrumentC := insertArbitrageFixture(t, ctx, pool, "binance", "F8HCUSDT")
	accountD, instrumentD := insertArbitrageFixture(t, ctx, pool, "okx", "F8HD-USDT")

	spread := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	spread.IdempotencyKey = "arb-f8h-spread"
	spread.RequestFingerprint = "arb-f8h-spread-fp"
	createdSpread, inserted, err := repository.CreateArbitrageCombination(ctx, spread)
	if err != nil || !inserted {
		t.Fatalf("spread inserted=%v err=%v", inserted, err)
	}
	if createdSpread.EarlyExitFunding8hAnnualizedFloor != "" {
		t.Fatalf("historical spread floor=%q", createdSpread.EarlyExitFunding8hAnnualizedFloor)
	}

	oneShot := integrationArbitrageCombination(accountC, instrumentC, accountD, instrumentD)
	oneShot.ID = ""
	oneShot.IdempotencyKey = "arb-f8h-oneshot"
	oneShot.RequestFingerprint = "arb-f8h-oneshot-fp"
	oneShot.RunMode = "one_shot"
	oneShot.EntryDirection = "ask"
	oneShot.ExitPolicy = "time"
	oneShot.ExitAfterSeconds = 3600
	oneShot.OneShotPhase = "waiting_exit"
	oneShot.AskThresholdBps = "0"
	oneShot.BidThresholdBps = "0"
	oneShot.EarlyExitFunding8hAnnualizedFloor = "0.05"
	created, inserted, err := repository.CreateArbitrageCombination(ctx, oneShot)
	if err != nil || !inserted {
		t.Fatalf("one_shot inserted=%v err=%v", inserted, err)
	}
	if created.EarlyExitFunding8hAnnualizedFloor != "0.05" {
		t.Fatalf("persisted floor=%q", created.EarlyExitFunding8hAnnualizedFloor)
	}

	for _, value := range []string{"NaN", "Infinity", "-Infinity"} {
		if _, execErr := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations
			SET early_exit_funding_8h_annualized_floor='`+value+`'::numeric
			WHERE id=$1::uuid`, created.ID); execErr == nil {
			t.Fatalf("finite check accepted %s", value)
		}
	}

	end := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	start := end.Add(-8 * time.Hour)
	insertSettledFundingRate(t, ctx, pool, instrumentC, start, "9", "1")
	insertSettledFundingRate(t, ctx, pool, instrumentD, start, "9", "1")
	for hour := 1; hour <= 8; hour++ {
		at := start.Add(time.Duration(hour) * time.Hour)
		insertSettledFundingRate(t, ctx, pool, instrumentC, at, "0.001", "1")
		insertSettledFundingRate(t, ctx, pool, instrumentD, at, "0", "1")
	}

	rows, err := repository.ListOneShotFunding8hExitRows(ctx, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 18 {
		t.Fatalf("raw rows=%d want 18 (not a cartesian 64)", len(rows))
	}
	seen := map[string]int{}
	windowByLeg := map[string]int{}
	anchorByLeg := map[string]int{}
	for _, row := range rows {
		if row.FundingTime == nil {
			t.Fatalf("nil funding time: %+v", row)
		}
		key := row.CombinationID + "|" + row.Leg + "|" + row.FundingTime.UTC().Format(time.RFC3339Nano)
		seen[key]++
		if !row.FundingTime.After(start) {
			anchorByLeg[row.Leg]++
			continue
		}
		if row.FundingTime.After(end) {
			t.Fatalf("row past window: %+v", row)
		}
		windowByLeg[row.Leg]++
	}
	for key, count := range seen {
		if count != 1 {
			t.Fatalf("duplicate raw key %s count=%d", key, count)
		}
	}
	if windowByLeg["a"] != 8 || windowByLeg["b"] != 8 ||
		anchorByLeg["a"] != 1 || anchorByLeg["b"] != 1 {
		t.Fatalf("window=%v anchor=%v", windowByLeg, anchorByLeg)
	}

	worker := NewArbitrageFunding8hExitWorker(repository, nil)
	worker.now = func() time.Time { return end }
	worker.runOnce(ctx)
	loaded, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.OneShotPhase != "exiting" {
		t.Fatalf("phase=%q", loaded.OneShotPhase)
	}
	var payload []byte
	if err := pool.QueryRow(ctx, `
		SELECT payload FROM trader_arbitrage_events
		WHERE combination_id=$1::uuid AND event_type='one_shot_exit_started'
		ORDER BY created_at DESC LIMIT 1`, created.ID,
	).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	if event["reason"] != "funding_8h_floor" {
		t.Fatalf("event=%v", event)
	}
	observed, err := decimal.NewFromString(event["observedAnnualized"].(string))
	if err != nil {
		t.Fatal(err)
	}
	wantObserved := decimal.RequireFromString("-0.008").Mul(decimal.NewFromInt(3)).Mul(decimal.NewFromInt(365))
	if !observed.Equal(wantObserved) {
		t.Fatalf("observed=%s want=%s event=%v", observed, wantObserved, event)
	}
	if event["configuredFloor"] != "0.05" ||
		event["legACumulativeRate"] != "0.008" ||
		event["legBCumulativeRate"] != "0" {
		t.Fatalf("event=%v", event)
	}
}

func TestMarkOneShotExitingFunding8hFloor(t *testing.T) {
	ctx, pool := openArbitrageRunModeTestDB(t)
	repository := NewRepository(pool)
	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "F8HCAUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "F8HCB-USDT")
	item := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	item.IdempotencyKey = "arb-f8h-cas"
	item.RequestFingerprint = "arb-f8h-cas-fp"
	item.RunMode = "one_shot"
	item.EntryDirection = "ask"
	item.ExitPolicy = "time"
	item.ExitAfterSeconds = 3600
	item.OneShotPhase = "waiting_exit"
	item.AskThresholdBps = "0"
	item.BidThresholdBps = "0"
	item.EarlyExitFunding8hAnnualizedFloor = "0.05"
	created, inserted, err := repository.CreateArbitrageCombination(ctx, item)
	if err != nil || !inserted {
		t.Fatalf("inserted=%v err=%v", inserted, err)
	}
	exiting, applied, err := repository.MarkOneShotExiting(
		ctx, created.ID, created.Version, "funding_8h_floor",
		map[string]any{"reason": "funding_8h_floor"},
	)
	if err != nil || !applied || exiting.OneShotPhase != "exiting" {
		t.Fatalf("applied=%v err=%v combo=%+v", applied, err, exiting)
	}
	if _, applied, err := repository.MarkOneShotExiting(
		ctx, created.ID, exiting.Version, "funding_8h_floor", nil,
	); err != nil || applied {
		t.Fatalf("second CAS applied=%v err=%v", applied, err)
	}
}

func insertSettledFundingRate(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	instrumentID int64,
	at time.Time,
	rate, intervalHours string,
) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO funding_rates (
			instrument_id,funding_rate,funding_time,record_kind,interval_hours
		) VALUES ($1,$2::numeric,$3,'settled',$4::numeric)`,
		instrumentID, rate, at, intervalHours,
	); err != nil {
		t.Fatal(err)
	}
}
