package trader

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"selfquant/backend/internal/database"
)

func TestAuthoritativeTerminalNextReconcileInfinity(t *testing.T) {
	ctx, pool, repository := newTerminalReconcileTestEnv(t)
	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "TERMA")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "TERMB")
	combo := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	combo.IdempotencyKey = "arb-terminal-infinity"
	combo.RequestFingerprint = "arb-terminal-infinity-fp"
	created, inserted, err := repository.CreateArbitrageCombination(ctx, combo)
	if err != nil || !inserted {
		t.Fatalf("create combo inserted=%v err=%v", inserted, err)
	}
	execution, claimed, err := repository.ClaimArbitrageExecution(
		ctx, integrationArbitrageExecution(created.ID, "ask"),
	)
	if err != nil || !claimed {
		t.Fatalf("claim execution claimed=%v err=%v", claimed, err)
	}

	t.Run("wss filled writes infinity and repeat stays infinity", func(t *testing.T) {
		order := createTerminalTestOrder(t, ctx, repository, accountA, instrumentA, execution.ID, "wss-filled")
		updated, err := repository.ApplyStreamUpdate(ctx, order.ID, StreamUpdate{
			Result: VenueResult{VenueOrderID: "venue-filled", Status: "filled", FilledQuantity: "0.001"},
		})
		if err != nil || updated.Status != "filled" {
			t.Fatalf("filled=%+v err=%v", updated, err)
		}
		assertNextReconcileInfinity(t, ctx, pool, order.ID, true)
		if _, err := repository.ApplyStreamUpdate(ctx, order.ID, StreamUpdate{
			Result: VenueResult{VenueOrderID: "venue-filled", Status: "filled", FilledQuantity: "0.001"},
		}); err != nil {
			t.Fatal(err)
		}
		assertNextReconcileInfinity(t, ctx, pool, order.ID, true)
		leased, err := repository.LeaseDueOrders(ctx, 10, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range leased {
			if item.ID == order.ID {
				t.Fatal("filled arbitrage order was leased")
			}
		}
	})

	t.Run("hl placement local ack terminal writes infinity", func(t *testing.T) {
		order := createTerminalTestOrder(t, ctx, repository, accountA, instrumentA, execution.ID, "hl-canceled")
		updated, err := repository.UpdateResult(ctx, order.ID, VenueResult{
			VenueOrderID: "hl-1", Status: "canceled", LocalCommandAck: true,
		})
		if err != nil || updated.Status != "canceled" {
			t.Fatalf("canceled=%+v err=%v", updated, err)
		}
		assertNextReconcileInfinity(t, ctx, pool, order.ID, true)
	})

	t.Run("hl placement rejected writes infinity", func(t *testing.T) {
		order := createTerminalTestOrder(t, ctx, repository, accountA, instrumentA, execution.ID, "hl-rejected")
		updated, err := repository.UpdateResult(ctx, order.ID, VenueResult{
			VenueOrderID: "hl-2", Status: "rejected", LocalCommandAck: true, ErrorCode: "ORDER_REJECTED",
		})
		if err != nil || updated.Status != "rejected" {
			t.Fatalf("rejected=%+v err=%v", updated, err)
		}
		assertNextReconcileInfinity(t, ctx, pool, order.ID, true)
	})

	t.Run("hl placement filled writes infinity", func(t *testing.T) {
		order := createTerminalTestOrder(t, ctx, repository, accountA, instrumentA, execution.ID, "hl-filled")
		updated, err := repository.UpdateResult(ctx, order.ID, VenueResult{
			VenueOrderID: "hl-3", Status: "filled", FilledQuantity: "0.001", AveragePrice: "100",
			LocalCommandAck: true,
		})
		if err != nil || updated.Status != "filled" {
			t.Fatalf("filled=%+v err=%v", updated, err)
		}
		assertNextReconcileInfinity(t, ctx, pool, order.ID, true)
	})

	t.Run("command only pending does not write infinity", func(t *testing.T) {
		order := createTerminalTestOrder(t, ctx, repository, accountA, instrumentA, execution.ID, "cmd-ack")
		updated, err := repository.UpdateResult(ctx, order.ID, VenueResult{
			Status: "pending", LocalCommandAck: true,
		})
		if err != nil || updated.Status != "pending" {
			t.Fatalf("pending=%+v err=%v", updated, err)
		}
		assertNextReconcileInfinity(t, ctx, pool, order.ID, false)
	})

	t.Run("confirmed absent rejected writes infinity", func(t *testing.T) {
		order := createTerminalTestOrder(t, ctx, repository, accountA, instrumentA, execution.ID, "absent")
		for i := 0; i < confirmedAbsentRejectAfter; i++ {
			current, err := repository.GetByOwner(ctx, "admin", order.ID)
			if err != nil {
				t.Fatal(err)
			}
			updated, converted, confirmErr := repository.ConfirmOrderAbsence(
				ctx, order.ID, current.UpdatedAt, time.Now().UTC().Add(2*time.Second),
			)
			if confirmErr != nil {
				t.Fatal(confirmErr)
			}
			if i+1 < confirmedAbsentRejectAfter {
				if converted || updated.Status != "pending" {
					t.Fatalf("early convert=%v order=%+v", converted, updated)
				}
				continue
			}
			if !converted || updated.Status != "rejected" {
				t.Fatalf("convert=%v order=%+v", converted, updated)
			}
			assertNextReconcileInfinity(t, ctx, pool, order.ID, true)
		}
	})

	t.Run("historical terminal normalize leaves fills", func(t *testing.T) {
		order := createTerminalTestOrder(t, ctx, repository, accountA, instrumentA, execution.ID, "normalize")
		if _, err := pool.Exec(ctx, `
			UPDATE trader_orders
			SET status='filled',filled_quantity=0.5,average_price=100,
			    next_reconcile_at=now(),reconcile_lease_until=now()+interval '1 minute'
			WHERE id=$1::uuid`, order.ID); err != nil {
			t.Fatal(err)
		}
		var beforeQty, beforeAvg string
		var beforeCount int64
		if err := pool.QueryRow(ctx, `
			SELECT filled_quantity::text,average_price::text,
			       (SELECT count(*) FROM trader_order_fills WHERE order_id=$1::uuid)
			FROM trader_orders WHERE id=$1::uuid`, order.ID,
		).Scan(&beforeQty, &beforeAvg, &beforeCount); err != nil {
			t.Fatal(err)
		}
		var affected int64
		if err := pool.QueryRow(ctx, `
			WITH updated AS (
				UPDATE trader_orders
				SET next_reconcile_at='infinity'::timestamptz,
				    reconcile_lease_until=NULL
				WHERE arbitrage_execution_id IS NOT NULL
				  AND status IN ('filled','canceled','rejected','expired')
				  AND next_reconcile_at < 'infinity'::timestamptz
				  AND id=$1::uuid
				RETURNING id
			)
			SELECT count(*) FROM updated`, order.ID,
		).Scan(&affected); err != nil {
			t.Fatal(err)
		}
		if affected != 1 {
			t.Fatalf("updated=%d", affected)
		}
		var afterQty, afterAvg string
		var afterCount int64
		if err := pool.QueryRow(ctx, `
			SELECT filled_quantity::text,average_price::text,
			       (SELECT count(*) FROM trader_order_fills WHERE order_id=$1::uuid)
			FROM trader_orders WHERE id=$1::uuid`, order.ID,
		).Scan(&afterQty, &afterAvg, &afterCount); err != nil {
			t.Fatal(err)
		}
		if afterQty != beforeQty || afterAvg != beforeAvg || afterCount != beforeCount {
			t.Fatalf("fills changed qty=%s/%s avg=%s/%s fills=%d/%d",
				afterQty, beforeQty, afterAvg, beforeAvg, afterCount, beforeCount)
		}
		assertNextReconcileInfinity(t, ctx, pool, order.ID, true)
	})
}

func newTerminalReconcileTestEnv(t *testing.T) (context.Context, *pgxpool.Pool, *Repository) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("trader_terminal_inf_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return ctx, pool, NewRepository(pool)
}

func createTerminalTestOrder(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	accountID, instrumentID int64,
	executionID, key string,
) Order {
	t.Helper()
	created, inserted, err := repository.CreateIntent(ctx, Order{
		IdempotencyKey: "term-" + key + uuid.NewString(), OwnerUsername: "admin",
		TradingAccountID: accountID, ProductName: "ARBITRAGE", Exchange: "hyperliquid",
		InstrumentID: instrumentID, ContractType: "perpetual", ExchangeSymbol: "TERMA",
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "limit",
		Quantity: "0.001", Price: "100", RequestFingerprint: "fp-" + key + uuid.NewString(),
		ArbitrageExecutionID: executionID, ArbitrageLeg: "a", ArbitrageRole: "maker",
	})
	if err != nil || !inserted {
		t.Fatalf("create order inserted=%v err=%v", inserted, err)
	}
	return created
}

func assertNextReconcileInfinity(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	orderID string,
	want bool,
) {
	t.Helper()
	var infinite bool
	if err := pool.QueryRow(ctx, `
		SELECT next_reconcile_at = 'infinity'::timestamptz
		FROM trader_orders WHERE id=$1::uuid`, orderID,
	).Scan(&infinite); err != nil {
		t.Fatal(err)
	}
	if infinite != want {
		t.Fatalf("infinity=%v want %v", infinite, want)
	}
}
