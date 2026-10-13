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
	"github.com/shopspring/decimal"
	"selfquant/backend/internal/database"
)

func TestOneShotFinalFlattenClaimAndFinalizeIntegration(t *testing.T) {
	ctx, pool := openOneShotFinalFlattenTestDB(t)
	repository := NewRepository(pool)
	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "FLATTAUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "FLATTB-USDT")
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.IdempotencyKey = "arb-one-shot-final-flatten"
	input.RequestFingerprint = "arb-one-shot-final-flatten-fp"
	input.RunMode = "one_shot"
	input.EntryDirection = "bid"
	input.ExitPolicy = "time"
	input.ExitAfterSeconds = 3600
	input.OneShotPhase = "exiting"
	created, inserted, err := repository.CreateArbitrageCombination(ctx, input)
	if err != nil || !inserted {
		t.Fatalf("inserted=%v err=%v", inserted, err)
	}
	reconciledAt := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			one_shot_phase='exiting',
			leg_a_base_position=88,
			leg_b_base_position=-80,
			leg_a_venue_base_position=88,
			leg_b_venue_base_position=-80,
			leg_a_venue_baseline_base_position=0,
			leg_b_venue_baseline_base_position=0,
			venue_baseline_captured_at=now(),
			last_position_reconciled_at=$2
		WHERE id=$1::uuid`, created.ID, reconciledAt); err != nil {
		t.Fatal(err)
	}
	combo, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	qtyA, qtyB := decimal.NewFromInt(88), decimal.NewFromInt(80)
	price := decimal.RequireFromString("0.214")
	notionalA := qtyA.Mul(price)
	notionalB := qtyB.Mul(price)
	mismatch := ArbitrageExecution{
		ID: uuid.NewString(), CombinationID: combo.ID,
		Direction: "bid", Status: "claimed",
		TriggerAskSpread: "12", TriggerBidSpread: "-8",
		TriggerLegABid: "0.214", TriggerLegAAsk: "0.214",
		TriggerLegBBid: "0.214", TriggerLegBAsk: "0.214",
		TargetBaseQuantity: "80",
		RequestedNotional:  decimal.Max(notionalA, notionalB).String(),
		PositionEffect:     "close", ReduceOnly: true, LastCloseClip: true,
		LegAFilledQuantity: "0", LegBFilledQuantity: "0", DeltaNotional: "0",
	}
	if _, claimed, claimErr := repository.ClaimOneShotFinalFlattenExecution(ctx, OneShotFinalFlattenClaim{
		Execution: mismatch, LegAQuantity: qtyA, LegBQuantity: qtyB,
		LegANotional: notionalA, LegBNotional: notionalB,
		ReconciledAt: combo.LastPositionReconciledAt,
	}); claimErr != nil || claimed {
		t.Fatalf("target mismatch claimed=%v err=%v", claimed, claimErr)
	}

	execution := mismatch
	execution.ID = uuid.NewString()
	execution.TargetBaseQuantity = "88"
	claimed, ok, err := repository.ClaimOneShotFinalFlattenExecution(ctx, OneShotFinalFlattenClaim{
		Execution: execution, LegAQuantity: qtyA, LegBQuantity: qtyB,
		LegANotional: notionalA, LegBNotional: notionalB,
		ReconciledAt: combo.LastPositionReconciledAt,
	})
	if err != nil || !ok {
		t.Fatalf("claimed=%v err=%v", ok, err)
	}
	if claimed.TargetBaseQuantity != "88" || !claimed.LastCloseClip {
		t.Fatalf("execution=%+v", claimed)
	}

	intents := []Order{
		oneShotFinalFlattenOrderIntent(combo, claimed, OwnedFlattenLeg{
			Leg: combo.LegA, LegName: "a", Side: "sell",
			BaseQuantity: qtyA, ReduceOnly: true, OrderType: "market",
		}, Instrument{ID: instrumentA, Exchange: "binance", ContractType: "perpetual",
			ExchangeSymbol: "FLATTAUSDT", BaseAsset: "BTC", QuoteAsset: "USDT"}),
		oneShotFinalFlattenOrderIntent(combo, claimed, OwnedFlattenLeg{
			Leg: combo.LegB, LegName: "b", Side: "buy",
			BaseQuantity: qtyB, ReduceOnly: true, OrderType: "market",
		}, Instrument{ID: instrumentB, Exchange: "okx", ContractType: "perpetual",
			ExchangeSymbol: "FLATTB-USDT", BaseAsset: "BTC", QuoteAsset: "USDT"}),
	}
	createdOrders, _, err := repository.CreateArbitrageIntents(ctx, intents)
	if err != nil || len(createdOrders) != 2 {
		t.Fatalf("intents err=%v n=%d", err, len(createdOrders))
	}
	for _, order := range createdOrders {
		if order.ArbitrageRole != "market" ||
			!strings.Contains(order.IdempotencyKey, ":final-flatten:") {
			t.Fatalf("order=%+v", order)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE trader_orders SET
				status='filled', filled_quantity=quantity, average_price=0.214, updated_at=now()
			WHERE id=$1::uuid`, order.ID); err != nil {
			t.Fatal(err)
		}
	}
	first, err := repository.FinalizeOneShotFinalFlattenExecution(ctx, claimed.ID)
	if err != nil || !first.Applied {
		t.Fatalf("first finalize applied=%v err=%v", first.Applied, err)
	}
	turnover := parseDecimal(first.Combination.CumulativeTurnoverNotional)
	if !turnover.IsPositive() {
		t.Fatalf("turnover=%s", turnover)
	}
	second, err := repository.FinalizeOneShotFinalFlattenExecution(ctx, claimed.ID)
	if err != nil || second.Applied {
		t.Fatalf("second finalize applied=%v err=%v", second.Applied, err)
	}
	if parseDecimal(second.Combination.CumulativeTurnoverNotional).Cmp(turnover) != 0 {
		t.Fatalf("turnover changed %s -> %s", turnover, second.Combination.CumulativeTurnoverNotional)
	}
}

func openOneShotFinalFlattenTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
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
	schema := fmt.Sprintf("trader_one_shot_flatten_%d", time.Now().UnixNano())
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
