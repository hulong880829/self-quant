package trader

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"selfquant/backend/internal/database"
)

type metricsTestEnv struct {
	pool        *pgxpool.Pool
	repo        *Repository
	combo       ArbitrageCombination
	execution   ArbitrageExecution
	accountA    int64
	accountB    int64
	instrumentA int64
	instrumentB int64
	orderA      Order
	orderB      Order
	baselineAt  time.Time
}

func TestArbitragePositionMetricsSkipCombinationsWithoutBaseline(t *testing.T) {
	ctx, env := newMetricsTestEnv(t, "skip-missing-baseline", false)
	if _, err := env.pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			leg_a_venue_baseline_base_position=NULL,
			leg_b_venue_baseline_base_position=NULL,
			venue_baseline_captured_at=NULL,
			realized_spread_pnl=1.23,
			estimated_funding_pnl=4.56,
			gross_turnover_notional=7.89
		WHERE id=$1::uuid`, env.combo.ID); err != nil {
		t.Fatal(err)
	}
	listed, err := env.repo.ListRunningArbitrageCombinationsForMetrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range listed {
		if item.ID == env.combo.ID {
			t.Fatal("combination without baseline was listed")
		}
	}
	before, err := env.repo.GetArbitrageCombinationByOwner(ctx, "admin", env.combo.ID)
	if err != nil {
		t.Fatal(err)
	}
	recomputed := persistMetrics(
		t, ctx, env.repo, env.combo.ID, "100", "101", time.Now().UTC(),
	).Combination
	if recomputed.Version != before.Version ||
		!recomputed.UpdatedAt.Equal(before.UpdatedAt) ||
		recomputed.RealizedSpreadPnl != before.RealizedSpreadPnl ||
		recomputed.EstimatedFundingPnl != before.EstimatedFundingPnl ||
		recomputed.GrossTurnoverNotional != before.GrossTurnoverNotional {
		t.Fatalf("metrics recomputed for combination without baseline: before=%+v after=%+v",
			before, recomputed)
	}
}

func TestArbitragePositionMetricsWorkerSemantics(t *testing.T) {
	ctx, env := newMetricsTestEnv(t, "net-annualized", true)
	setAccountContractFees(t, ctx, env.pool, env.accountA, "0.0002", "0.0004")
	setAccountContractFees(t, ctx, env.pool, env.accountB, "-0.0002", "-0.0005")
	insertSettledFunding(t, ctx, env.pool, env.instrumentA, env.combo.CreatedAt.Add(2*time.Minute), "0.001")
	insertSettledFunding(t, ctx, env.pool, env.instrumentB, env.combo.CreatedAt.Add(2*time.Minute), "-0.002")
	insertSettledFunding(t, ctx, env.pool, env.instrumentA, env.baselineAt.Add(-time.Hour), "0.9")

	before, err := env.repo.GetArbitrageCombinationByOwner(ctx, "admin", env.combo.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := env.combo.CreatedAt.Add(2 * time.Hour)
	updated := persistMetrics(t, ctx, env.repo, env.combo.ID, "100", "101", now).Combination
	if updated.GrossTurnoverNotional != "1.909" ||
		updated.LegAAverageEntryPrice != "100" ||
		updated.LegBAverageEntryPrice != "101" ||
		updated.AverageEntrySpreadBps != "100" ||
		updated.EstimatedFundingPnl != "-0.002818" ||
		!updated.FundingHistoryComplete ||
		updated.Version != before.Version {
		t.Fatalf("running metrics=%+v version before=%d", updated, before.Version)
	}
	quality, fee, calculatedAt := loadStoredNetMetrics(t, ctx, env.pool, env.combo.ID)
	if quality != metricsQualityComplete || calculatedAt.IsZero() {
		t.Fatalf("quality=%s calculatedAt=%s", quality, calculatedAt)
	}
	assertDecimal(t, "estimated trading fee", fee, "0.0008545")
	var estimateCount int
	if err := env.pool.QueryRow(ctx, `
		SELECT count(*) FROM trader_arbitrage_funding_estimates
		WHERE combination_id=$1::uuid`, env.combo.ID,
	).Scan(&estimateCount); err != nil {
		t.Fatal(err)
	}
	if estimateCount != 0 {
		t.Fatalf("funding estimate rows=%d", estimateCount)
	}

	listed, err := env.repo.ListRunningArbitrageCombinationsForMetrics(ctx)
	if err != nil || len(listed) != 1 || listed[0].ID != env.combo.ID {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}

	closeExec, claimed, err := env.repo.ClaimArbitrageExecution(
		ctx, integrationArbitrageExecution(env.combo.ID, "bid"),
	)
	if err != nil || !claimed {
		t.Fatalf("claim close execution claimed=%v err=%v", claimed, err)
	}
	closeA, createdA, err := env.repo.CreateIntent(ctx, Order{
		IdempotencyKey: "net-annualized-close-a", OwnerUsername: "admin",
		TradingAccountID: env.accountA, ProductName: "ARBITRAGE", Exchange: "binance",
		InstrumentID: env.instrumentA, ContractType: "perpetual", ExchangeSymbol: "NETMETAAUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "sell", OrderType: "market",
		Quantity: "0.01", RequestFingerprint: "net-annualized-close-a-fp",
		ArbitrageExecutionID: closeExec.ID, ArbitrageLeg: "a", ArbitrageRole: "market",
	})
	if err != nil || !createdA {
		t.Fatalf("create close A created=%v err=%v", createdA, err)
	}
	closeB, createdB, err := env.repo.CreateIntent(ctx, Order{
		IdempotencyKey: "net-annualized-close-b", OwnerUsername: "admin",
		TradingAccountID: env.accountB, ProductName: "ARBITRAGE", Exchange: "okx",
		InstrumentID: env.instrumentB, ContractType: "perpetual", ExchangeSymbol: "NETMETAB-USDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
		Quantity: "0.009", RequestFingerprint: "net-annualized-close-b-fp",
		ArbitrageExecutionID: closeExec.ID, ArbitrageLeg: "b", ArbitrageRole: "hedge",
	})
	if err != nil || !createdB {
		t.Fatalf("create close B created=%v err=%v", createdB, err)
	}
	if _, err := env.repo.UpdateResult(ctx, closeA.ID, VenueResult{
		Status: "filled", FilledQuantity: "0.01", AveragePrice: "110",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.repo.UpdateResult(ctx, closeB.ID, VenueResult{
		Status: "filled", FilledQuantity: "0.009", AveragePrice: "90",
	}); err != nil {
		t.Fatal(err)
	}
	closeExec.Status = "completed"
	if _, err := env.repo.UpdateArbitrageExecution(ctx, closeExec); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET status='closing' WHERE id=$1::uuid`,
		env.combo.ID); err != nil {
		t.Fatal(err)
	}
	listed, err = env.repo.ListRunningArbitrageCombinationsForMetrics(ctx)
	if err != nil || len(listed) != 0 {
		t.Fatalf("closing combination listed=%+v err=%v", listed, err)
	}
	if _, err := env.pool.Exec(ctx, `
		UPDATE trader_orders SET last_venue_event_at=$2
		WHERE id IN ($1::uuid,$3::uuid)`,
		closeA.ID, now.Add(time.Minute), closeB.ID,
	); err != nil {
		t.Fatal(err)
	}
	listed, err = env.repo.ListRunningArbitrageCombinationsForMetrics(ctx)
	if err != nil || len(listed) != 0 {
		t.Fatalf("closing combination after newer order times listed=%+v err=%v", listed, err)
	}
	frozen := persistMetrics(
		t, ctx, env.repo, env.combo.ID, "100", "101", now.Add(2*time.Hour),
	).Combination
	frozenQuality, frozenFee, frozenAt := loadStoredNetMetrics(t, ctx, env.pool, env.combo.ID)
	if frozen.CombinedPositionAnnualized != updated.CombinedPositionAnnualized ||
		frozenFee != fee || !frozenAt.Equal(calculatedAt) ||
		frozenQuality != quality {
		t.Fatalf("closing persist overwrote running metrics: before=%+v after=%+v fee %s->%s at %s->%s",
			updated, frozen, fee, frozenFee, calculatedAt, frozenAt)
	}
}

func TestArbitragePositionMetricsIncompleteKeepsAnnualizedGroup(t *testing.T) {
	ctx, env := newMetricsTestEnv(t, "partial-annualized", true)
	setAccountContractFees(t, ctx, env.pool, env.accountA, "0.0002", "0.0004")
	setAccountContractFees(t, ctx, env.pool, env.accountB, "-0.0002", "-0.0005")
	now := env.combo.CreatedAt.Add(time.Hour)
	complete := persistMetrics(t, ctx, env.repo, env.combo.ID, "100", "101", now).Combination
	if complete.CombinedPositionAnnualized == "" {
		t.Fatal("expected net annualized on complete pass")
	}
	quality, fee, calculatedAt := loadStoredNetMetrics(t, ctx, env.pool, env.combo.ID)
	if quality != metricsQualityComplete {
		t.Fatalf("quality=%s", quality)
	}

	if _, err := env.pool.Exec(ctx, `
		UPDATE trading_accounts SET contract_taker_fee_rate=NULL WHERE id=$1`,
		env.accountA,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `
		UPDATE trader_orders SET filled_quantity=0.02,average_price=110
		WHERE id=$1::uuid`, env.orderA.ID); err != nil {
		t.Fatal(err)
	}
	partial := persistMetrics(
		t, ctx, env.repo, env.combo.ID, "100", "101", now.Add(time.Minute),
	).Combination
	nextQuality, nextFee, nextAt := loadStoredNetMetrics(t, ctx, env.pool, env.combo.ID)
	if partial.CombinedPositionAnnualized != complete.CombinedPositionAnnualized ||
		nextFee != fee || !nextAt.Equal(calculatedAt) ||
		nextQuality != metricsQualityPartial {
		t.Fatalf("annualized group split on incomplete fees: before=%+v after=%+v fee %s->%s quality=%s",
			complete, partial, fee, nextFee, nextQuality)
	}
	if partial.GrossTurnoverNotional == complete.GrossTurnoverNotional {
		t.Fatal("ordinary turnover should update while annualized group stays")
	}
}

func TestArbitragePositionMetricsWritesAnnualizedWithoutBBO(t *testing.T) {
	ctx, env := newMetricsTestEnv(t, "annualized-without-bbo", true)
	setAccountContractFees(t, ctx, env.pool, env.accountA, "0.0002", "0.0004")
	setAccountContractFees(t, ctx, env.pool, env.accountB, "-0.0002", "-0.0005")
	now := env.combo.CreatedAt.Add(time.Hour)
	seeded := persistMetrics(t, ctx, env.repo, env.combo.ID, "100", "101", now).Combination
	_, _, seededAt := loadStoredNetMetrics(t, ctx, env.pool, env.combo.ID)
	withoutBBO := persistMetrics(
		t, ctx, env.repo, env.combo.ID, "", "", now.Add(time.Minute),
	).Combination
	quality, _, calculatedAt := loadStoredNetMetrics(t, ctx, env.pool, env.combo.ID)
	if quality != metricsQualityComplete || calculatedAt.Equal(seededAt) {
		t.Fatalf("missing BBO froze annualized group: quality=%s at=%s seeded=%s",
			quality, calculatedAt, seededAt)
	}
	if withoutBBO.GrossTurnoverNotional != seeded.GrossTurnoverNotional {
		t.Fatalf("turnover changed without new fills: %+v", withoutBBO)
	}
}

func TestArbitragePositionMetricsReplayUsesTerminalOrdersWithoutFills(t *testing.T) {
	ctx, env := newMetricsTestEnv(t, "order-aggregate-replay", true)
	if _, err := env.pool.Exec(ctx, `DELETE FROM trader_order_fills`); err != nil {
		t.Fatal(err)
	}
	setAccountContractFees(t, ctx, env.pool, env.accountA, "0.0002", "0.0004")
	setAccountContractFees(t, ctx, env.pool, env.accountB, "-0.0002", "-0.0005")
	updated := persistMetrics(
		t, ctx, env.repo, env.combo.ID, "100", "101", env.combo.CreatedAt.Add(time.Hour),
	).Combination
	quality, _, _ := loadStoredNetMetrics(t, ctx, env.pool, env.combo.ID)
	if quality != metricsQualityComplete || updated.GrossTurnoverNotional != "1.909" {
		t.Fatalf("missing fills should still be complete: quality=%s metrics=%+v", quality, updated)
	}

	replay, err := loadArbitrageReplay(ctx, env.pool, env.combo.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.coverageComplete || replay.fillRowCount != 2 || len(replay.fills) != 2 {
		t.Fatalf("terminal filled orders replay=%+v", replay)
	}

	partial, createdPartial, err := env.repo.CreateIntent(ctx, Order{
		IdempotencyKey: "order-aggregate-replay-partial", OwnerUsername: "admin",
		TradingAccountID: env.accountA, ProductName: "ARBITRAGE", Exchange: "binance",
		InstrumentID: env.instrumentA, ContractType: "perpetual", ExchangeSymbol: "NETMETAAUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
		Quantity: "0.02", RequestFingerprint: "order-aggregate-replay-partial-fp",
		ArbitrageExecutionID: env.execution.ID, ArbitrageLeg: "a", ArbitrageRole: "market",
	})
	if err != nil || !createdPartial {
		t.Fatalf("create partial order created=%v err=%v", createdPartial, err)
	}
	partialUpdated, err := env.repo.UpdateResult(ctx, partial.ID, VenueResult{
		VenueOrderID: "partial-venue", Status: "partially_filled",
		FilledQuantity: "0.01", AveragePrice: "100",
	})
	if err != nil {
		t.Fatal(err)
	}
	if partialUpdated.Status != "partially_filled" {
		t.Fatalf("partial status=%s", partialUpdated.Status)
	}
	replay, err = loadArbitrageReplay(ctx, env.pool, env.combo.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.coverageComplete || replay.fillRowCount != 2 {
		t.Fatalf("partially_filled order must be excluded: %+v", replay)
	}

	canceled, createdCanceled, err := env.repo.CreateIntent(ctx, Order{
		IdempotencyKey: "order-aggregate-replay-canceled", OwnerUsername: "admin",
		TradingAccountID: env.accountA, ProductName: "ARBITRAGE", Exchange: "binance",
		InstrumentID: env.instrumentA, ContractType: "perpetual", ExchangeSymbol: "NETMETAAUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
		Quantity: "0.05", RequestFingerprint: "order-aggregate-replay-canceled-fp",
		ArbitrageExecutionID: env.execution.ID, ArbitrageLeg: "a", ArbitrageRole: "market",
	})
	if err != nil || !createdCanceled {
		t.Fatalf("create canceled order created=%v err=%v", createdCanceled, err)
	}
	canceledUpdated, err := env.repo.UpdateResult(ctx, canceled.ID, VenueResult{
		VenueOrderID: "canceled-venue", Status: "canceled",
		FilledQuantity: "0.02", AveragePrice: "90",
	})
	if err != nil {
		t.Fatal(err)
	}
	if canceledUpdated.Status != "canceled" {
		t.Fatalf("canceled status=%s", canceledUpdated.Status)
	}
	replay, err = loadArbitrageReplay(ctx, env.pool, env.combo.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.coverageComplete || replay.fillRowCount != 3 {
		t.Fatalf("canceled filled order must be included: %+v", replay)
	}
	foundCanceled := false
	for _, fill := range replay.fills {
		if fill.orderID == canceled.ID {
			foundCanceled = true
			if !fill.quantity.Equal(decimal.RequireFromString("0.02")) ||
				!fill.price.Equal(decimal.NewFromInt(90)) {
				t.Fatalf("canceled fill=%+v", fill)
			}
		}
	}
	if !foundCanceled {
		t.Fatal("canceled order was not synthesized into replay fills")
	}
}

func TestArbitragePositionMetricsWriteGuardDiscardsInFlight(t *testing.T) {
	ctx, env := newMetricsTestEnv(t, "inflight-guard", true)
	setAccountContractFees(t, ctx, env.pool, env.accountA, "0.0002", "0.0004")
	setAccountContractFees(t, ctx, env.pool, env.accountB, "-0.0002", "-0.0005")
	seeded := persistMetrics(
		t, ctx, env.repo, env.combo.ID, "100", "101", env.combo.CreatedAt.Add(time.Hour),
	).Combination
	inFlight, claimed, err := env.repo.ClaimArbitrageExecution(
		ctx, integrationArbitrageExecution(env.combo.ID, "ask"),
	)
	if err != nil || !claimed {
		t.Fatalf("claim in-flight execution claimed=%v err=%v", claimed, err)
	}
	inFlight.Status = "hedging"
	if _, err := env.repo.UpdateArbitrageExecution(ctx, inFlight); err != nil {
		t.Fatal(err)
	}
	listed, err := env.repo.ListRunningArbitrageCombinationsForMetrics(ctx)
	if err != nil || len(listed) != 0 {
		t.Fatalf("in-flight execution listed=%+v err=%v", listed, err)
	}
	if _, err := env.pool.Exec(ctx, `
		UPDATE trader_orders SET filled_quantity=0.05 WHERE id=$1::uuid`, env.orderA.ID,
	); err != nil {
		t.Fatal(err)
	}
	guardedResult := persistMetrics(
		t, ctx, env.repo, env.combo.ID, "100", "101", env.combo.CreatedAt.Add(2*time.Hour),
	)
	if guardedResult.Applied {
		t.Fatal("in-flight persist should not apply")
	}
	if guardedResult.Reason != MetricsActiveWork {
		t.Fatalf("reason=%s", guardedResult.Reason)
	}
	guarded := guardedResult.Combination
	if guarded.GrossTurnoverNotional != seeded.GrossTurnoverNotional ||
		guarded.CombinedPositionAnnualized != seeded.CombinedPositionAnnualized {
		t.Fatalf("in-flight persist overwrote metrics: seeded=%+v guarded=%+v", seeded, guarded)
	}
}

func newMetricsTestEnv(t *testing.T, key string, completeExecution bool) (context.Context, metricsTestEnv) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	schema := fmt.Sprintf("trader_net_metrics_%d", time.Now().UnixNano())
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

	repo := NewRepository(pool)
	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "NETMETAAUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "NETMETAB-USDT")
	baselineAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.ID = uuid.NewString()
	input.IdempotencyKey = key
	input.RequestFingerprint = key + "-fingerprint"
	input.LegA.ExchangeSymbol = "NETMETAAUSDT"
	input.LegB.ExchangeSymbol = "NETMETAB-USDT"
	input.LegAVenueBaselineBasePosition = "0"
	input.LegBVenueBaselineBasePosition = "0"
	input.VenueBaselineCapturedAt = baselineAt
	combo, inserted, err := repo.CreateArbitrageCombination(ctx, input)
	if err != nil || !inserted {
		t.Fatalf("create combination inserted=%v err=%v", inserted, err)
	}
	execution, claimed, err := repo.ClaimArbitrageExecution(
		ctx, integrationArbitrageExecution(combo.ID, "ask"),
	)
	if err != nil || !claimed {
		t.Fatalf("claim execution claimed=%v err=%v", claimed, err)
	}
	orderA, createdA, err := repo.CreateIntent(ctx, Order{
		IdempotencyKey: key + "-a", OwnerUsername: "admin",
		TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
		InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: "NETMETAAUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
		Quantity: "0.01", RequestFingerprint: key + "-a-fp",
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "market",
	})
	if err != nil || !createdA {
		t.Fatalf("create order A created=%v err=%v", createdA, err)
	}
	orderB, createdB, err := repo.CreateIntent(ctx, Order{
		IdempotencyKey: key + "-b", OwnerUsername: "admin",
		TradingAccountID: accountB, ProductName: "ARBITRAGE", Exchange: "okx",
		InstrumentID: instrumentB, ContractType: "perpetual", ExchangeSymbol: "NETMETAB-USDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "sell", OrderType: "market",
		Quantity: "0.01", RequestFingerprint: key + "-b-fp",
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "b", ArbitrageRole: "hedge",
	})
	if err != nil || !createdB {
		t.Fatalf("create order B created=%v err=%v", createdB, err)
	}
	if _, err := repo.UpdateResult(ctx, orderA.ID, VenueResult{
		Status: "filled", FilledQuantity: "0.01", AveragePrice: "100",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateResult(ctx, orderB.ID, VenueResult{
		Status: "filled", FilledQuantity: "0.009", AveragePrice: "101",
	}); err != nil {
		t.Fatal(err)
	}
	filledAt := combo.CreatedAt.Add(time.Minute)
	for _, fill := range []struct {
		orderID, venue, trade, qty, price string
		account                           int64
	}{
		{orderA.ID, "binance", key + "-ta", "0.01", "100", accountA},
		{orderB.ID, "okx", key + "-tb", "0.009", "101", accountB},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO trader_order_fills (
				order_id,trading_account_id,exchange,venue_order_id,trade_id,
				quantity,price,executed_at
			) VALUES ($1::uuid,$2,$3,$4,$5,$6::numeric,$7::numeric,$8)`,
			fill.orderID, fill.account, fill.venue, fill.trade, fill.trade,
			fill.qty, fill.price, filledAt,
		); err != nil {
			t.Fatal(err)
		}
	}
	if completeExecution {
		execution.Status = "completed"
		if execution, err = repo.UpdateArbitrageExecution(ctx, execution); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, metricsTestEnv{
		pool: pool, repo: repo, combo: combo, execution: execution,
		accountA: accountA, accountB: accountB,
		instrumentA: instrumentA, instrumentB: instrumentB,
		orderA: orderA, orderB: orderB, baselineAt: baselineAt,
	}
}

func setAccountContractFees(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	accountID int64,
	maker, taker string,
) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		UPDATE trading_accounts SET
			contract_maker_fee_rate=$2::numeric,
			contract_taker_fee_rate=$3::numeric,
			spot_maker_fee_rate=$2::numeric,
			spot_taker_fee_rate=$3::numeric
		WHERE id=$1`, accountID, maker, taker); err != nil {
		t.Fatal(err)
	}
}

func insertSettledFunding(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	instrumentID int64,
	at time.Time,
	rate string,
) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO funding_rates (
			instrument_id,funding_rate,funding_time,record_kind,interval_hours
		) VALUES ($1,$2::float8,$3,'settled',4)`,
		instrumentID, rate, at,
	); err != nil {
		t.Fatal(err)
	}
}

func loadStoredNetMetrics(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	combinationID string,
) (quality, fee string, calculatedAt time.Time) {
	t.Helper()
	var feeRaw *string
	var at *time.Time
	var qualityRaw *string
	if err := pool.QueryRow(ctx, `
		SELECT metrics_quality,trim_scale(estimated_trading_fee)::text,metrics_calculated_at
		FROM trader_arbitrage_combinations WHERE id=$1::uuid`, combinationID,
	).Scan(&qualityRaw, &feeRaw, &at); err != nil {
		t.Fatal(err)
	}
	if qualityRaw != nil {
		quality = *qualityRaw
	}
	if feeRaw != nil {
		fee = *feeRaw
	}
	if at != nil {
		calculatedAt = at.UTC()
	}
	return quality, fee, calculatedAt
}

func assertDecimal(t *testing.T, name, got, want string) {
	t.Helper()
	gotDec, err := decimal.NewFromString(got)
	if err != nil {
		t.Fatalf("%s invalid decimal %q: %v", name, got, err)
	}
	if !gotDec.Equal(decimal.RequireFromString(want)) {
		t.Fatalf("%s=%s want=%s", name, got, want)
	}
}
