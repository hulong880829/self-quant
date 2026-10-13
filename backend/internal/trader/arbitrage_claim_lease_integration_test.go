package trader

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

func TestUpdateArbitragePositionAuditPersistsLiquidationDistance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	repository, pool, cleanup := openArbitrageIntegrationRepo(t, ctx, "trader_liq_distance")
	defer cleanup()

	combo := createRunningCombination(t, ctx, repository, pool, "liq-distance")
	written, err := repository.UpdateArbitragePositionAudit(
		ctx, combo.ID, combo.Version, "1", "-1", "0", "0", false, "", "18.42", "-3.5",
	)
	if err != nil || written.LegALiquidationDistancePct != "18.42" ||
		written.LegBLiquidationDistancePct != "-3.5" {
		t.Fatalf("written=%+v err=%v", written, err)
	}
	cleared, err := repository.UpdateArbitragePositionAudit(
		ctx, combo.ID, written.Version, "0", "0", "0", "0", false, "", "", "",
	)
	if err != nil || cleared.LegALiquidationDistancePct != "" ||
		cleared.LegBLiquidationDistancePct != "" {
		t.Fatalf("cleared=%+v err=%v", cleared, err)
	}
	if _, err := repository.UpdateArbitragePositionAudit(
		ctx, combo.ID, written.Version, "1", "-1", "0", "0", false, "", "9", "9",
	); err == nil {
		t.Fatal("stale version updated liquidation distance")
	}
	kept := reloadCombination(t, ctx, repository, combo.ID)
	if kept.LegALiquidationDistancePct != "" || kept.LegBLiquidationDistancePct != "" ||
		kept.Version != cleared.Version {
		t.Fatalf("kept=%+v", kept)
	}
}

func TestClaimLastCloseResidualFlattenWithoutRecoveredEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	repository, pool, cleanup := openArbitrageIntegrationRepo(t, ctx, "trader_residual_claim")
	defer cleanup()

	combo := markCombinationClosing(t, ctx, repository, pool, createRunningCombination(t, ctx, repository, pool, "tiny-claim"))
	executionID := lastCloseResidualFlattenExecutionID(combo.ID)
	orders := residualClaimOrders(combo, executionID)
	claimed, won, err := repository.ClaimLastCloseResidualFlattenExecution(ctx, LastCloseResidualFlattenClaim{
		ExpectedVersion: combo.Version, Combination: combo, Orders: orders,
	})
	if err != nil || !won || claimed.ID != executionID {
		t.Fatalf("won=%v execution=%s err=%v", won, claimed.ID, err)
	}
	stored := countResidualClaimRows(t, ctx, pool, combo.ID, executionID)
	if stored.executions != 1 || stored.orders != 2 || stored.matchingKeys != 2 || stored.recovered != 0 {
		t.Fatalf("created=%+v", stored)
	}

	again, wonAgain, err := repository.ClaimLastCloseResidualFlattenExecution(ctx, LastCloseResidualFlattenClaim{
		ExpectedVersion: combo.Version, Combination: combo, Orders: orders,
	})
	if err != nil || wonAgain || again.ID != executionID {
		t.Fatalf("repeat won=%v execution=%s err=%v", wonAgain, again.ID, err)
	}
	repeated := countResidualClaimRows(t, ctx, pool, combo.ID, executionID)
	if repeated.executions != 1 || repeated.orders != 2 {
		t.Fatalf("repeat created=%+v", repeated)
	}

	blocked := []struct {
		name string
		sql  string
	}{
		{name: "manual intervention", sql: "runtime_state='manual_intervention'"},
		{name: "position uncertain", sql: "position_uncertain=TRUE"},
		{name: "circuit open", sql: "circuit_open=TRUE"},
	}
	for _, test := range blocked {
		t.Run(test.name, func(t *testing.T) {
			item := markCombinationClosing(t, ctx, repository, pool, createRunningCombination(t, ctx, repository, pool, "block-"+test.name))
			if _, err := pool.Exec(ctx, `
				UPDATE trader_arbitrage_combinations SET `+test.sql+`
				WHERE id=$1::uuid`, item.ID); err != nil {
				t.Fatal(err)
			}
			item = reloadCombination(t, ctx, repository, item.ID)
			executionID := lastCloseResidualFlattenExecutionID(item.ID)
			_, won, err := repository.ClaimLastCloseResidualFlattenExecution(ctx, LastCloseResidualFlattenClaim{
				ExpectedVersion: item.Version,
				Combination:     item,
				Orders:          residualClaimOrders(item, executionID),
			})
			if err != nil || won {
				t.Fatalf("won=%v err=%v", won, err)
			}
			got := countResidualClaimRows(t, ctx, pool, item.ID, executionID)
			if got.executions != 0 || got.orders != 0 {
				t.Fatalf("created=%+v", got)
			}
		})
	}

	t.Run("other active execution", func(t *testing.T) {
		item := createRunningCombination(t, ctx, repository, pool, "block-execution")
		execution, claimed, err := repository.ClaimArbitrageExecution(
			ctx, integrationArbitrageExecution(item.ID, "ask"),
		)
		if err != nil || !claimed || execution.ID == "" {
			t.Fatalf("seed execution claimed=%v err=%v", claimed, err)
		}
		item = markCombinationClosing(t, ctx, repository, pool, item)
		executionID := lastCloseResidualFlattenExecutionID(item.ID)
		_, won, err := repository.ClaimLastCloseResidualFlattenExecution(ctx, LastCloseResidualFlattenClaim{
			ExpectedVersion: item.Version,
			Combination:     item,
			Orders:          residualClaimOrders(item, executionID),
		})
		if err != nil || won {
			t.Fatalf("won=%v err=%v", won, err)
		}
		got := countResidualClaimRows(t, ctx, pool, item.ID, executionID)
		if got.executions != 0 || got.orders != 0 {
			t.Fatalf("created=%+v", got)
		}
	})

	t.Run("other active order", func(t *testing.T) {
		item := createRunningCombination(t, ctx, repository, pool, "block-order")
		execution, claimed, err := repository.ClaimArbitrageExecution(
			ctx, integrationArbitrageExecution(item.ID, "bid"),
		)
		if err != nil || !claimed {
			t.Fatalf("seed execution claimed=%v err=%v", claimed, err)
		}
		execution.Status = "completed"
		if _, err := repository.UpdateArbitrageExecution(ctx, execution); err != nil {
			t.Fatal(err)
		}
		pending, created, err := repository.CreateIntent(ctx, Order{
			IdempotencyKey: "other-active-" + item.ID, OwnerUsername: "admin",
			TradingAccountID: item.LegA.TradingAccountID, ProductName: "ARBITRAGE",
			Exchange: item.LegA.Exchange, InstrumentID: item.LegA.InstrumentID,
			ContractType: item.LegA.ContractType, ExchangeSymbol: item.LegA.ExchangeSymbol,
			BaseAsset: "BTC", QuoteAsset: "USDT", Side: "sell", OrderType: "market",
			Quantity: "1", RequestFingerprint: "other-active-" + item.ID,
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "market",
		})
		if err != nil || !created || pending.Status != "pending" {
			t.Fatalf("pending=%+v created=%v err=%v", pending, created, err)
		}
		item = markCombinationClosing(t, ctx, repository, pool, item)
		executionID := lastCloseResidualFlattenExecutionID(item.ID)
		_, won, err := repository.ClaimLastCloseResidualFlattenExecution(ctx, LastCloseResidualFlattenClaim{
			ExpectedVersion: item.Version,
			Combination:     item,
			Orders:          residualClaimOrders(item, executionID),
		})
		if err != nil || won {
			t.Fatalf("won=%v err=%v", won, err)
		}
		got := countResidualClaimRows(t, ctx, pool, item.ID, executionID)
		if got.executions != 0 || got.orders != 0 {
			t.Fatalf("created=%+v", got)
		}
	})
}

func TestFinalMetricsLeaseDoesNotOverwriteDustOrNewerClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	repository, pool, cleanup := openArbitrageIntegrationRepo(t, ctx, "trader_metrics_lease")
	defer cleanup()

	t.Run("dust wins over old lease", func(t *testing.T) {
		combo := prepareClosingAverageGap(t, ctx, repository, pool, "lease-dust")
		claimed, err := repository.ClaimDueFinalMetrics(ctx, 20)
		if err != nil || len(claimed) != 1 || claimed[0].ID != combo.ID {
			t.Fatalf("claimed=%+v err=%v", claimed, err)
		}
		lease := claimed[0]
		closed, applied, err := repository.CloseLastCloseResidualWithDust(ctx, LastCloseResidualDustClose{
			CombinationID:   combo.ID,
			ExpectedVersion: lease.Version,
			LegANotional:    decimal.NewFromInt(1),
			LegBNotional:    decimal.NewFromInt(1),
		})
		if err != nil || !applied || closed.FinalMetricsStatus != finalMetricsPartial ||
			closed.FinalMetricsRetryFinite {
			t.Fatalf("dust applied=%v combo=%+v err=%v", applied, closed, err)
		}
		_, retryApplied, err := repository.RecordFinalMetricsRetry(
			ctx, lease.ID, lease.Version, lease.FinalMetricsNextRetryAt, "stale worker",
		)
		if err != nil || retryApplied {
			t.Fatalf("stale retry applied=%v err=%v", retryApplied, err)
		}
		released, err := repository.ReleaseFinalMetricsClaim(
			ctx, lease.ID, lease.Version, lease.FinalMetricsNextRetryAt,
		)
		if err != nil || released {
			t.Fatalf("stale release applied=%v err=%v", released, err)
		}
		kept := reloadCombination(t, ctx, repository, combo.ID)
		if kept.Status != "closed" || kept.FinalMetricsStatus != finalMetricsPartial ||
			kept.FinalMetricsRetryFinite || kept.FinalMetricsLastError != residualDustMetricsSkipped {
			t.Fatalf("kept=%+v", kept)
		}
	})

	t.Run("old lease cannot overwrite a newer claim", func(t *testing.T) {
		combo := prepareClosingAverageGap(t, ctx, repository, pool, "lease-renew")
		first, err := repository.ClaimDueFinalMetrics(ctx, 20)
		if err != nil || len(first) != 1 || first[0].ID != combo.ID {
			t.Fatalf("first=%+v err=%v", first, err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations
			SET final_metrics_next_retry_at=now() - interval '1 second'
			WHERE id=$1::uuid AND version=$2`, combo.ID, first[0].Version); err != nil {
			t.Fatal(err)
		}
		second, err := repository.ClaimDueFinalMetrics(ctx, 20)
		if err != nil || len(second) != 1 || second[0].ID != combo.ID {
			t.Fatalf("second=%+v err=%v", second, err)
		}
		if second[0].FinalMetricsNextRetryAt.Equal(first[0].FinalMetricsNextRetryAt) {
			t.Fatal("renewed claim kept the old lease")
		}
		_, applied, err := repository.RecordFinalMetricsRetry(
			ctx, first[0].ID, first[0].Version, first[0].FinalMetricsNextRetryAt, "old lease",
		)
		if err != nil || applied {
			t.Fatalf("old retry applied=%v err=%v", applied, err)
		}
		released, err := repository.ReleaseFinalMetricsClaim(
			ctx, first[0].ID, first[0].Version, first[0].FinalMetricsNextRetryAt,
		)
		if err != nil || released {
			t.Fatalf("old release applied=%v err=%v", released, err)
		}
		updated, applied, err := repository.RecordFinalMetricsRetry(
			ctx, second[0].ID, second[0].Version, second[0].FinalMetricsNextRetryAt, "current lease",
		)
		if err != nil || !applied || updated.FinalMetricsStatus != finalMetricsPartial ||
			updated.FinalMetricsLastError != "current lease" || updated.Status != "closing" ||
			!updated.FinalMetricsRetryFinite ||
			updated.FinalMetricsNextRetryAt.Equal(second[0].FinalMetricsNextRetryAt) {
			t.Fatalf("current retry applied=%v updated=%+v err=%v", applied, updated, err)
		}
	})
}

func createRunningCombination(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	pool *pgxpool.Pool,
	key string,
) ArbitrageCombination {
	t.Helper()
	slug := residualClaimSlug(key)
	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", slug+"USDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", slug+"-SWAP")
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.IdempotencyKey = "claim-" + slug
	input.RequestFingerprint = "claim-fp-" + slug
	input.LegA.ExchangeSymbol = slug + "USDT"
	input.LegB.ExchangeSymbol = slug + "-SWAP"
	created, inserted, err := repository.CreateArbitrageCombination(ctx, input)
	if err != nil || !inserted {
		t.Fatalf("create inserted=%v err=%v", inserted, err)
	}
	return created
}

func markCombinationClosing(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	pool *pgxpool.Pool,
	item ArbitrageCombination,
) ArbitrageCombination {
	t.Helper()
	tag, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET status='closing', runtime_state='closing', version=version+1, updated_at=now()
		WHERE id=$1::uuid AND status='running'`, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("closing update affected %d", tag.RowsAffected())
	}
	return reloadCombination(t, ctx, repository, item.ID)
}

func prepareClosingAverageGap(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	pool *pgxpool.Pool,
	key string,
) ArbitrageCombination {
	t.Helper()
	item := createRunningCombination(t, ctx, repository, pool, key)
	execution, claimed, err := repository.ClaimArbitrageExecution(
		ctx, integrationArbitrageExecution(item.ID, "ask"),
	)
	if err != nil || !claimed {
		t.Fatalf("seed execution claimed=%v err=%v", claimed, err)
	}
	filled := insertFilledArbitrageOrder(t, ctx, repository, Order{
		IdempotencyKey: "gap-" + item.ID, OwnerUsername: "admin",
		TradingAccountID: item.LegA.TradingAccountID, ProductName: "ARBITRAGE",
		Exchange: item.LegA.Exchange, InstrumentID: item.LegA.InstrumentID,
		ContractType: item.LegA.ContractType, ExchangeSymbol: item.LegA.ExchangeSymbol,
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
		Quantity: "1", RequestFingerprint: "gap-fp-" + item.ID,
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "market",
	}, "1")
	if _, err := pool.Exec(ctx, `
		UPDATE trader_orders SET average_price=0 WHERE id=$1::uuid`, filled.ID); err != nil {
		t.Fatal(err)
	}
	execution.Status = "completed"
	if _, err := repository.UpdateArbitrageExecution(ctx, execution); err != nil {
		t.Fatal(err)
	}
	tag, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET status='closing', runtime_state='closing', version=version+1,
		    last_position_reconciled_at=now(), updated_at=now()
		WHERE id=$1::uuid AND status='running'`, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("closing update affected %d", tag.RowsAffected())
	}
	return reloadCombination(t, ctx, repository, item.ID)
}

func reloadCombination(
	t *testing.T, ctx context.Context, repository *Repository, id string,
) ArbitrageCombination {
	t.Helper()
	item, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", id)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

type residualClaimCounts struct {
	executions   int
	orders       int
	matchingKeys int
	recovered    int
}

func countResidualClaimRows(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, combinationID, executionID string,
) residualClaimCounts {
	t.Helper()
	var counts residualClaimCounts
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM trader_arbitrage_executions WHERE id=$2::uuid),
			(SELECT count(*) FROM trader_orders WHERE arbitrage_execution_id=$2::uuid),
			(SELECT count(*) FROM trader_orders
			 WHERE arbitrage_execution_id=$2::uuid
			   AND idempotency_key IN ($3, $4)),
			(SELECT count(*) FROM trader_arbitrage_events
			 WHERE combination_id=$1::uuid AND event_type=$5)`,
		combinationID, executionID,
		lastCloseResidualFlattenIdempotencyKey(executionID, "a"),
		lastCloseResidualFlattenIdempotencyKey(executionID, "b"),
		lastCloseResidualRecoveredEvent,
	).Scan(&counts.executions, &counts.orders, &counts.matchingKeys, &counts.recovered); err != nil {
		t.Fatal(err)
	}
	return counts
}

func residualClaimOrders(combo ArbitrageCombination, executionID string) []Order {
	return []Order{
		{
			IdempotencyKey:   lastCloseResidualFlattenIdempotencyKey(executionID, "a"),
			OwnerUsername:    combo.OwnerUsername,
			TradingAccountID: combo.LegA.TradingAccountID,
			ProductName:      "ARBITRAGE",
			Exchange:         combo.LegA.Exchange,
			InstrumentID:     combo.LegA.InstrumentID,
			ContractType:     combo.LegA.ContractType,
			ExchangeSymbol:   combo.LegA.ExchangeSymbol,
			BaseAsset:        "BTC", QuoteAsset: "USDT",
			Side: "sell", OrderType: "market", Quantity: "1.4",
			RequestFingerprint:   "residual-claim-a-" + combo.ID,
			ArbitrageExecutionID: executionID,
			ArbitrageLeg:         "a",
			ArbitrageRole:        "market",
			ReduceOnly:           true,
		},
		{
			IdempotencyKey:   lastCloseResidualFlattenIdempotencyKey(executionID, "b"),
			OwnerUsername:    combo.OwnerUsername,
			TradingAccountID: combo.LegB.TradingAccountID,
			ProductName:      "ARBITRAGE",
			Exchange:         combo.LegB.Exchange,
			InstrumentID:     combo.LegB.InstrumentID,
			ContractType:     combo.LegB.ContractType,
			ExchangeSymbol:   combo.LegB.ExchangeSymbol,
			BaseAsset:        "BTC", QuoteAsset: "USDT",
			Side: "buy", OrderType: "market", Quantity: "2",
			RequestFingerprint:   "residual-claim-b-" + combo.ID,
			ArbitrageExecutionID: executionID,
			ArbitrageLeg:         "b",
			ArbitrageRole:        "market",
			ReduceOnly:           true,
		},
	}
}

func residualClaimSlug(key string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(key) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}
