package trader

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"selfquant/backend/internal/database"
)

func TestApplyClosingExternalFlatReconcileIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("trader_closing_flat_%d", time.Now().UnixNano())
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
	repository := NewRepository(pool)

	t.Run("vvv residual flattens without new fills", func(t *testing.T) {
		combo, orders := setupClosingExternalFlatCombo(
			t, ctx, pool, repository, "vvv", "0", "0", false,
		)
		orderCount, fillCount := countComboOrdersAndFills(t, ctx, pool, combo.ID)
		applied, ok, err := repository.ApplyClosingExternalFlatReconcile(
			ctx, closingExternalFlatReconcileRequest{
				CombinationID:   combo.ID,
				ExpectedVersion: combo.Version,
				VenueA:          decimal.Zero,
				VenueB:          decimal.Zero,
			},
		)
		if err != nil || !ok {
			t.Fatalf("apply ok=%v err=%v", ok, err)
		}
		if applied.Status != "closing" || applied.RuntimeState != "closing" ||
			applied.PositionUncertain || !combinationLooksFlat(applied) {
			t.Fatalf("applied=%+v", applied)
		}
		if parseDecimal(applied.LegAReconciliationAdjustment).Cmp(decimal.RequireFromString("-0.5")) != 0 ||
			parseDecimal(applied.LegBReconciliationAdjustment).Cmp(decimal.RequireFromString("0.57")) != 0 {
			t.Fatalf("adjustments a=%s b=%s",
				applied.LegAReconciliationAdjustment, applied.LegBReconciliationAdjustment)
		}
		recomputed, err := repository.RecomputeArbitrageBasePositions(ctx, applied.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !combinationLooksFlat(recomputed) {
			t.Fatalf("recompute resurrected ledger=%+v", recomputed)
		}
		afterOrders, afterFills := countComboOrdersAndFills(t, ctx, pool, combo.ID)
		if afterOrders != orderCount || afterFills != fillCount || len(orders) != 2 {
			t.Fatalf("orders before=%d after=%d fills before=%d after=%d",
				orderCount, afterOrders, fillCount, afterFills)
		}
		events, err := repository.ListArbitrageEvents(ctx, applied.ID, 20)
		if err != nil {
			t.Fatal(err)
		}
		payload := closingExternalFlatEvent(t, events)
		for _, key := range []string{
			"reason", "legALocalBasePosition", "legBLocalBasePosition",
			"legAVenueBaselinePosition", "legBVenueBaselinePosition",
			"legAVenueBasePosition", "legBVenueBasePosition",
			"legAPositionDifference", "legBPositionDifference",
			"legAPreviousAdjustment", "legBPreviousAdjustment",
			"legAAdjustment", "legBAdjustment", "previousError",
		} {
			if _, ok := payload[key]; !ok {
				t.Fatalf("missing payload field %s: %v", key, payload)
			}
		}
		if payload["reason"] != "external_or_manual_flatten" ||
			fmt.Sprint(payload["legALocalBasePosition"]) != "0.5" ||
			fmt.Sprint(payload["legBLocalBasePosition"]) != "-0.57" ||
			fmt.Sprint(payload["legAVenueBaselinePosition"]) != "0" ||
			fmt.Sprint(payload["legBVenueBaselinePosition"]) != "0" ||
			fmt.Sprint(payload["legAVenueBasePosition"]) != "0" ||
			fmt.Sprint(payload["legBVenueBasePosition"]) != "0" ||
			fmt.Sprint(payload["legAPositionDifference"]) != "-0.5" ||
			fmt.Sprint(payload["legBPositionDifference"]) != "0.57" ||
			fmt.Sprint(payload["legAPreviousAdjustment"]) != "0" ||
			fmt.Sprint(payload["legBPreviousAdjustment"]) != "0" ||
			fmt.Sprint(payload["legAAdjustment"]) != "-0.5" ||
			fmt.Sprint(payload["legBAdjustment"]) != "0.57" ||
			!strings.HasPrefix(fmt.Sprint(payload["previousError"]), arbitragePositionAuditErrorPrefix) {
			t.Fatalf("payload=%v", payload)
		}
	})

	t.Run("non-zero baseline is preserved", func(t *testing.T) {
		combo, _ := setupClosingExternalFlatCombo(
			t, ctx, pool, repository, "baseline", "12.5", "-8", false,
		)
		applied, ok, err := repository.ApplyClosingExternalFlatReconcile(
			ctx, closingExternalFlatReconcileRequest{
				CombinationID:   combo.ID,
				ExpectedVersion: combo.Version,
				VenueA:          decimal.RequireFromString("12.5"),
				VenueB:          decimal.RequireFromString("-8"),
			},
		)
		if err != nil || !ok {
			t.Fatalf("apply ok=%v err=%v", ok, err)
		}
		if applied.LegAVenueBaselineBasePosition != "12.5" ||
			applied.LegBVenueBaselineBasePosition != "-8" ||
			!combinationLooksFlat(applied) {
			t.Fatalf("applied=%+v", applied)
		}
	})

	t.Run("last venue not yet at baseline is skipped", func(t *testing.T) {
		combo, _ := setupClosingExternalFlatCombo(
			t, ctx, pool, repository, "first-tick", "0", "0", false,
		)
		if _, err := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations SET
				leg_a_venue_base_position=0.5,
				leg_b_venue_base_position=-0.57,
				version=version+1
			WHERE id=$1::uuid`, combo.ID); err != nil {
			t.Fatal(err)
		}
		combo, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", combo.ID)
		if err != nil {
			t.Fatal(err)
		}
		applied, ok, err := repository.ApplyClosingExternalFlatReconcile(
			ctx, closingExternalFlatReconcileRequest{
				CombinationID:   combo.ID,
				ExpectedVersion: combo.Version,
				VenueA:          decimal.Zero,
				VenueB:          decimal.Zero,
			},
		)
		if err != nil || ok || combinationLooksFlat(applied) {
			t.Fatalf("ok=%v err=%v applied=%+v", ok, err, applied)
		}
	})

	t.Run("one leg still open is skipped", func(t *testing.T) {
		combo, _ := setupClosingExternalFlatCombo(
			t, ctx, pool, repository, "one-leg", "0", "0", false,
		)
		before := combo.Version
		applied, ok, err := repository.ApplyClosingExternalFlatReconcile(
			ctx, closingExternalFlatReconcileRequest{
				CombinationID:   combo.ID,
				ExpectedVersion: combo.Version,
				VenueA:          decimal.Zero,
				VenueB:          decimal.RequireFromString("0.1"),
			},
		)
		if err != nil || ok || applied.Version != before || combinationLooksFlat(applied) {
			t.Fatalf("ok=%v err=%v applied=%+v", ok, err, applied)
		}
	})

	t.Run("active execution is skipped", func(t *testing.T) {
		combo, _ := setupClosingExternalFlatCombo(
			t, ctx, pool, repository, "active-exec", "0", "0", true,
		)
		applied, ok, err := repository.ApplyClosingExternalFlatReconcile(
			ctx, closingExternalFlatReconcileRequest{
				CombinationID:   combo.ID,
				ExpectedVersion: combo.Version,
				VenueA:          decimal.Zero,
				VenueB:          decimal.Zero,
			},
		)
		if err != nil || ok || combinationLooksFlat(applied) {
			t.Fatalf("ok=%v err=%v applied=%+v", ok, err, applied)
		}
	})

	t.Run("non-terminal order is skipped", func(t *testing.T) {
		combo, orders := setupClosingExternalFlatCombo(
			t, ctx, pool, repository, "open-order", "0", "0", false,
		)
		if _, err := pool.Exec(ctx, `
			UPDATE trader_orders SET status='open',reconcile_failures=0
			WHERE id=$1::uuid`, orders[0].ID); err != nil {
			t.Fatal(err)
		}
		applied, ok, err := repository.ApplyClosingExternalFlatReconcile(
			ctx, closingExternalFlatReconcileRequest{
				CombinationID:   combo.ID,
				ExpectedVersion: combo.Version,
				VenueA:          decimal.Zero,
				VenueB:          decimal.Zero,
			},
		)
		if err != nil || ok || combinationLooksFlat(applied) {
			t.Fatalf("ok=%v err=%v applied=%+v", ok, err, applied)
		}
	})

	t.Run("version conflict is skipped", func(t *testing.T) {
		combo, _ := setupClosingExternalFlatCombo(
			t, ctx, pool, repository, "version", "0", "0", false,
		)
		applied, ok, err := repository.ApplyClosingExternalFlatReconcile(
			ctx, closingExternalFlatReconcileRequest{
				CombinationID:   combo.ID,
				ExpectedVersion: combo.Version - 1,
				VenueA:          decimal.Zero,
				VenueB:          decimal.Zero,
			},
		)
		if err != nil || ok || applied.Version != combo.Version {
			t.Fatalf("ok=%v err=%v applied=%+v", ok, err, applied)
		}
	})

	t.Run("same-account instrument conflict is skipped", func(t *testing.T) {
		combo, _ := setupClosingExternalFlatCombo(
			t, ctx, pool, repository, "conflict-a", "0", "0", false,
		)
		accountC, instrumentC := insertArbitrageFixture(t, ctx, pool, "bybit", "ETHUSDT")
		accountD, instrumentD := insertArbitrageFixture(t, ctx, pool, "bitget", "ETHUSDT")
		other := integrationArbitrageCombination(accountC, instrumentC, accountD, instrumentD)
		other.IdempotencyKey = "closing-flat-conflict-b"
		other.RequestFingerprint = "closing-flat-conflict-b-fp"
		other.LegA.Exchange = "bybit"
		other.LegA.ExchangeSymbol = "ETHUSDT"
		other.LegB.Exchange = "bitget"
		other.LegB.ExchangeSymbol = "ETHUSDT"
		created, inserted, err := repository.CreateArbitrageCombination(ctx, other)
		if err != nil || !inserted {
			t.Fatalf("create other inserted=%v err=%v", inserted, err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations SET
				leg_a_trading_account_id=$2,leg_a_instrument_id=$3
			WHERE id=$1::uuid`,
			created.ID, combo.LegA.TradingAccountID, combo.LegA.InstrumentID,
		); err != nil {
			t.Fatal(err)
		}
		applied, ok, err := repository.ApplyClosingExternalFlatReconcile(
			ctx, closingExternalFlatReconcileRequest{
				CombinationID:   combo.ID,
				ExpectedVersion: combo.Version,
				VenueA:          decimal.Zero,
				VenueB:          decimal.Zero,
			},
		)
		if err != nil || ok || combinationLooksFlat(applied) {
			t.Fatalf("ok=%v err=%v applied=%+v", ok, err, applied)
		}
	})
}

func setupClosingExternalFlatCombo(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	key, baselineA, baselineB string,
	leaveExecutionActive bool,
) (ArbitrageCombination, []Order) {
	t.Helper()
	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", key+"A")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", key+"B")
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.IdempotencyKey = "closing-flat-" + key
	input.RequestFingerprint = "closing-flat-" + key + "-fp"
	input.LegA.ExchangeSymbol = key + "A"
	input.LegB.ExchangeSymbol = key + "B"
	input.LegAVenueBaselineBasePosition = baselineA
	input.LegBVenueBaselineBasePosition = baselineB
	input.VenueBaselineCapturedAt = time.Now().UTC().Truncate(time.Microsecond)
	created, inserted, err := repository.CreateArbitrageCombination(ctx, input)
	if err != nil || !inserted {
		t.Fatalf("create inserted=%v err=%v", inserted, err)
	}
	execution, claimed, err := repository.ClaimArbitrageExecution(
		ctx, integrationArbitrageExecution(created.ID, "ask"),
	)
	if err != nil || !claimed {
		t.Fatalf("claim claimed=%v err=%v", claimed, err)
	}
	orders, flags, err := repository.CreateArbitrageIntents(ctx, []Order{
		{
			IdempotencyKey: key + "-a", OwnerUsername: "admin",
			TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
			InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: key + "A",
			BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
			Quantity: "0.5", RequestFingerprint: key + "-a-fp",
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "market",
		},
		{
			IdempotencyKey: key + "-b", OwnerUsername: "admin",
			TradingAccountID: accountB, ProductName: "ARBITRAGE", Exchange: "okx",
			InstrumentID: instrumentB, ContractType: "perpetual", ExchangeSymbol: key + "B",
			BaseAsset: "BTC", QuoteAsset: "USDT", Side: "sell", OrderType: "market",
			Quantity: "0.57", RequestFingerprint: key + "-b-fp",
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "b", ArbitrageRole: "market",
		},
	})
	if err != nil || len(flags) != 2 || !flags[0] || !flags[1] {
		t.Fatalf("intents flags=%v err=%v", flags, err)
	}
	if _, err := repository.UpdateResult(ctx, orders[0].ID, VenueResult{
		Status: "filled", FilledQuantity: "0.5", AveragePrice: "100",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.UpdateResult(ctx, orders[1].ID, VenueResult{
		Status: "filled", FilledQuantity: "0.57", AveragePrice: "100",
	}); err != nil {
		t.Fatal(err)
	}
	if !leaveExecutionActive {
		execution.Status = "completed"
		if _, err := repository.UpdateArbitrageExecution(ctx, execution); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repository.RecomputeArbitrageBasePositions(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			status='closing',
			runtime_state='position_uncertain',
			position_uncertain=TRUE,
			error_message=$2,
			leg_a_venue_base_position=$3::numeric,
			leg_b_venue_base_position=$4::numeric,
			last_position_reconciled_at=now(),
			version=version+1
		WHERE id=$1::uuid`,
		created.ID,
		arbitragePositionAuditErrorPrefix+" residual",
		baselineA, baselineB,
	); err != nil {
		t.Fatal(err)
	}
	combo, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if combo.LegABasePosition != "0.5" || combo.LegBBasePosition != "-0.57" {
		t.Fatalf("setup ledger=%+v", combo)
	}
	return combo, orders
}

func countComboOrdersAndFills(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	combinationID string,
) (int, int) {
	t.Helper()
	var orders, fills int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM trader_orders o
		JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
		WHERE e.combination_id=$1::uuid`, combinationID,
	).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM trader_order_fills f
		JOIN trader_orders o ON o.id=f.order_id
		JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
		WHERE e.combination_id=$1::uuid`, combinationID,
	).Scan(&fills); err != nil {
		t.Fatal(err)
	}
	return orders, fills
}

func closingExternalFlatEvent(t *testing.T, events []ArbitrageEvent) map[string]any {
	t.Helper()
	for _, event := range events {
		if event.Type != "closing_external_flat_reconciled" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(event.Message), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	t.Fatal("missing closing_external_flat_reconciled event")
	return nil
}
