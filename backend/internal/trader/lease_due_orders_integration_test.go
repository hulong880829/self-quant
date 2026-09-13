package trader

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"selfquant/backend/internal/database"
)

func TestLeaseDueOrdersIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := newLeaseDueOrdersTestPool(t, ctx, dsn)
	defer pool.Close()

	accountA, instrumentA := insertLeaseDueOrdersTradingFixture(
		t, ctx, pool, "binance", "LEASEBTCUSDT",
	)
	accountB, instrumentB := insertLeaseDueOrdersTradingFixture(
		t, ctx, pool, "okx", "LEASE-BTC-USDT-SWAP",
	)
	repository := newLeaseDueOrdersRepository(pool)
	executionID := insertLeaseDueOrdersArbitrageFixture(
		t, ctx, pool, repository,
		accountA, instrumentA, accountB, instrumentB,
	)
	wrongAccountA, wrongInstrumentA := insertLeaseDueOrdersTradingFixture(
		t, ctx, pool, "bybit", "LEASE-WRONG-A",
	)
	wrongAccountB, wrongInstrumentB := insertLeaseDueOrdersTradingFixture(
		t, ctx, pool, "gate", "LEASE-WRONG-B",
	)
	wrongErrorExecutionID := insertLeaseDueOrdersArbitrageFixtureState(
		t, ctx, pool, repository,
		wrongAccountA, wrongInstrumentA, wrongAccountB, wrongInstrumentB,
		"wrong-error", "running", true, "different reconciliation error",
	)
	certainAccountA, certainInstrumentA := insertLeaseDueOrdersTradingFixture(
		t, ctx, pool, "bitget", "LEASE-CERTAIN-A",
	)
	certainAccountB, certainInstrumentB := insertLeaseDueOrdersTradingFixture(
		t, ctx, pool, "aster", "LEASE-CERTAIN-B",
	)
	notUncertainExecutionID := insertLeaseDueOrdersArbitrageFixtureState(
		t, ctx, pool, repository,
		certainAccountA, certainInstrumentA, certainAccountB, certainInstrumentB,
		"not-uncertain", "running", false, arbitrageOrderReconcileError,
	)
	closingAccountA, closingInstrumentA := insertLeaseDueOrdersTradingFixture(
		t, ctx, pool, "hyperliquid", "LEASE-CLOSING-A",
	)
	closingAccountB, closingInstrumentB := insertLeaseDueOrdersTradingFixture(
		t, ctx, pool, "lighter", "LEASE-CLOSING-B",
	)
	notRunningExecutionID := insertLeaseDueOrdersArbitrageFixtureState(
		t, ctx, pool, repository,
		closingAccountA, closingInstrumentA, closingAccountB, closingInstrumentB,
		"not-running", "closing", true, arbitrageOrderReconcileError,
	)

	t.Run("eligibility ordering limit and returned fields", func(t *testing.T) {
		testCtx, testCancel := context.WithTimeout(ctx, 10*time.Second)
		defer testCancel()
		now := time.Now().UTC().Truncate(time.Microsecond)
		expiredLease := now.Add(-time.Minute)
		validLease := now.Add(time.Hour)
		tiedUpdatedAt := now.Add(-2 * time.Hour)

		type expectedOrder struct {
			name string
			id   string
		}
		expected := []expectedOrder{
			{name: "global active"},
			{name: "expired lease"},
			{name: "old uncertain arbitrage"},
			{name: "active uncertain overlap"},
			{name: "recent terminal uncertain overlap"},
		}
		options := []leaseDueOrderOptions{
			{
				name: "eligibility-global", status: "pending",
				nextReconcileAt: now.Add(-10 * time.Minute),
				updatedAt:       now.Add(-3 * time.Hour),
			},
			{
				name: "eligibility-expired-lease", status: "open",
				nextReconcileAt: now.Add(-9 * time.Minute),
				updatedAt:       now.Add(-3 * time.Hour),
				leaseUntil:      &expiredLease,
			},
			{
				name: "eligibility-old-uncertain", status: "canceled",
				createdAt:         now.Add(-time.Hour),
				nextReconcileAt:   now.Add(-7 * time.Minute),
				updatedAt:         now.Add(-3 * time.Hour),
				reconcileFailures: 1,
				executionID:       executionID,
			},
			{
				name: "eligibility-active-overlap", status: "unknown",
				createdAt:         now.Add(-time.Hour),
				nextReconcileAt:   now.Add(-6 * time.Minute),
				updatedAt:         now.Add(-3 * time.Hour),
				reconcileFailures: 2,
				executionID:       executionID,
			},
			{
				name: "eligibility-terminal-overlap", status: "rejected",
				createdAt:         now.Add(-4 * time.Minute),
				nextReconcileAt:   now.Add(-5 * time.Minute),
				updatedAt:         now.Add(-3 * time.Hour),
				reconcileFailures: 3,
				executionID:       executionID,
			},
		}
		for index := range options {
			expected[index].id = insertLeaseDueOrder(
				t, testCtx, pool, accountA, instrumentA, options[index],
			)
		}

		tiedIDs := []string{
			insertLeaseDueOrder(t, testCtx, pool, accountA, instrumentA, leaseDueOrderOptions{
				name: "eligibility-tie-a", status: "partially_filled",
				nextReconcileAt: now.Add(-4 * time.Minute), updatedAt: tiedUpdatedAt,
			}),
			insertLeaseDueOrder(t, testCtx, pool, accountA, instrumentA, leaseDueOrderOptions{
				name: "eligibility-tie-b", status: "pending",
				nextReconcileAt: now.Add(-4 * time.Minute), updatedAt: tiedUpdatedAt,
			}),
		}

		insertLeaseDueOrder(t, testCtx, pool, accountA, instrumentA, leaseDueOrderOptions{
			name: "eligibility-recent-terminal", status: "filled",
			createdAt: now.Add(-5 * time.Minute), nextReconcileAt: now.Add(-8 * time.Minute),
			updatedAt: now.Add(-3 * time.Hour), executionID: executionID,
		})
		insertLeaseDueOrder(t, testCtx, pool, accountA, instrumentA, leaseDueOrderOptions{
			name: "eligibility-future", status: "pending",
			nextReconcileAt: now.Add(time.Hour), updatedAt: now.Add(-4 * time.Hour),
		})
		insertLeaseDueOrder(t, testCtx, pool, accountA, instrumentA, leaseDueOrderOptions{
			name: "eligibility-valid-lease", status: "open",
			nextReconcileAt: now.Add(-time.Hour), updatedAt: now.Add(-4 * time.Hour),
			leaseUntil: &validLease,
		})
		insertLeaseDueOrder(t, testCtx, pool, accountA, instrumentA, leaseDueOrderOptions{
			name: "eligibility-old-terminal", status: "expired",
			createdAt: now.Add(-time.Hour), nextReconcileAt: now.Add(-time.Hour),
			updatedAt: now.Add(-4 * time.Hour), executionID: executionID,
		})
		insertLeaseDueOrder(t, testCtx, pool, accountA, instrumentA, leaseDueOrderOptions{
			name: "eligibility-global-terminal", status: "filled",
			createdAt: now.Add(-time.Minute), nextReconcileAt: now.Add(-time.Hour),
			updatedAt: now.Add(-4 * time.Hour),
		})
		for _, excluded := range []struct {
			name        string
			executionID string
		}{
			{name: "eligibility-wrong-error", executionID: wrongErrorExecutionID},
			{name: "eligibility-not-uncertain", executionID: notUncertainExecutionID},
			{name: "eligibility-not-running", executionID: notRunningExecutionID},
		} {
			insertLeaseDueOrder(
				t, testCtx, pool, accountA, instrumentA, leaseDueOrderOptions{
					name: excluded.name, status: "canceled",
					createdAt: now.Add(-11 * time.Minute), nextReconcileAt: now.Add(-time.Hour),
					updatedAt: now.Add(-4 * time.Hour), executionID: excluded.executionID,
					reconcileFailures: 1,
				},
			)
		}

		preLease := make(map[string]Order, len(expected)+len(tiedIDs))
		for _, item := range expected {
			preLease[item.id] = getLeaseDueOrder(t, testCtx, repository, item.id)
		}
		for _, id := range tiedIDs {
			preLease[id] = getLeaseDueOrder(t, testCtx, repository, id)
		}

		items, err := repository.LeaseDueOrders(testCtx, 7, 5*time.Minute)
		if err != nil {
			t.Fatalf("lease due orders: %v", err)
		}
		if len(items) != 7 {
			t.Fatalf("leased %d orders, want 7: %+v", len(items), items)
		}
		for index, want := range expected {
			if items[index].ID != want.id {
				t.Fatalf(
					"item %d id=%s want %s (%s)",
					index, items[index].ID, want.id, want.name,
				)
			}
		}
		assertLeaseDueOrderIDSet(t, items[len(expected):], tiedIDs)
		for _, item := range items {
			if !reflect.DeepEqual(item, preLease[item.ID]) {
				t.Fatalf(
					"leased order changed from pre-lease value:\ngot  %+v\nwant %+v",
					item, preLease[item.ID],
				)
			}
			var leaseUntil, updatedAt time.Time
			if err := pool.QueryRow(testCtx, `
				SELECT reconcile_lease_until,updated_at
				FROM trader_orders WHERE id=$1::uuid`, item.ID,
			).Scan(&leaseUntil, &updatedAt); err != nil {
				t.Fatal(err)
			}
			if !leaseUntil.After(now) {
				t.Fatalf("order %s lease_until=%s, want after %s", item.ID, leaseUntil, now)
			}
			if !updatedAt.Equal(preLease[item.ID].UpdatedAt) {
				t.Fatalf(
					"order %s updated_at changed: got %s want %s",
					item.ID, updatedAt, preLease[item.ID].UpdatedAt,
				)
			}
		}

		empty, err := repository.LeaseDueOrders(testCtx, 7, time.Minute)
		if err != nil || len(empty) != 0 {
			t.Fatalf("empty lease: items=%+v err=%v", empty, err)
		}
		stats := repository.SQLStats()
		if stats.LeaseCalls != 2 || stats.LeaseReturned != 7 ||
			stats.LeaseWrites != 7 {
			t.Fatalf("unexpected lease SQL stats: %+v", stats)
		}

		goldenIDs := make([]string, 0, len(items))
		for _, item := range items {
			goldenIDs = append(goldenIDs, item.ID)
		}
		if _, err := pool.Exec(testCtx, `
			UPDATE trader_orders SET reconcile_lease_until=NULL
			WHERE id=ANY($1::uuid[])`, goldenIDs,
		); err != nil {
			t.Fatal(err)
		}
		retryRepository := newLeaseDueOrdersRepository(pool)
		retryGolden := make(map[string]Order, len(goldenIDs))
		for _, id := range goldenIDs {
			retryGolden[id] = getLeaseDueOrder(t, testCtx, retryRepository, id)
		}
		retryItems, err := retryRepository.LeaseDueOrders(testCtx, 7, 5*time.Minute)
		if err != nil {
			t.Fatalf("repeat lease: %v", err)
		}
		if len(retryItems) != 7 {
			t.Fatalf("repeat leased %d orders, want 7", len(retryItems))
		}
		for index, want := range expected {
			if retryItems[index].ID != want.id {
				t.Fatalf(
					"repeat item %d id=%s want %s (%s)",
					index, retryItems[index].ID, want.id, want.name,
				)
			}
		}
		assertLeaseDueOrderIDSet(t, retryItems[len(expected):], tiedIDs)
		for _, item := range retryItems {
			if !reflect.DeepEqual(item, retryGolden[item.ID]) {
				t.Fatalf(
					"repeat returned fields differ from pre-lease golden: got=%+v want=%+v",
					item, retryGolden[item.ID],
				)
			}
			var leaseUntil, updatedAt time.Time
			if err := pool.QueryRow(testCtx, `
				SELECT reconcile_lease_until,updated_at
				FROM trader_orders WHERE id=$1::uuid`, item.ID,
			).Scan(&leaseUntil, &updatedAt); err != nil {
				t.Fatal(err)
			}
			if !leaseUntil.After(now) || !updatedAt.Equal(retryGolden[item.ID].UpdatedAt) {
				t.Fatalf("lease timestamp changed business fields for %s", item.ID)
			}
		}
	})

	t.Run("concurrent repositories lease disjoint orders", func(t *testing.T) {
		testCtx, testCancel := context.WithTimeout(ctx, 10*time.Second)
		defer testCancel()
		now := time.Now().UTC().Truncate(time.Microsecond)
		wantIDs := make([]string, 0, 6)
		for index := 0; index < 6; index++ {
			wantIDs = append(wantIDs, insertLeaseDueOrder(
				t, testCtx, pool, accountA, instrumentA, leaseDueOrderOptions{
					name: fmt.Sprintf("concurrent-%d", index), status: "pending",
					nextReconcileAt: now.Add(-time.Minute + time.Duration(index)*time.Second),
					updatedAt:       now.Add(-time.Hour),
				},
			))
		}

		repositories := []*Repository{
			newLeaseDueOrdersRepository(pool),
			newLeaseDueOrdersRepository(pool),
		}
		results := make([][]Order, len(repositories))
		errorsByRepository := make([]error, len(repositories))
		start := make(chan struct{})
		var waitGroup sync.WaitGroup
		for index := range repositories {
			waitGroup.Add(1)
			go func(index int) {
				defer waitGroup.Done()
				<-start
				results[index], errorsByRepository[index] = repositories[index].LeaseDueOrders(
					testCtx, 3, 5*time.Minute,
				)
			}(index)
		}
		close(start)
		waitGroup.Wait()
		for index, err := range errorsByRepository {
			if err != nil {
				t.Fatalf("repository %d lease: %v", index, err)
			}
			if len(results[index]) != 3 {
				t.Fatalf("repository %d leased %d orders, want 3", index, len(results[index]))
			}
		}
		seen := make(map[string]bool, len(wantIDs))
		for _, result := range results {
			for _, item := range result {
				if seen[item.ID] {
					t.Fatalf("order %s was leased by both repositories", item.ID)
				}
				seen[item.ID] = true
			}
		}
		assertLeaseDueOrderIDMap(t, seen, wantIDs)
	})

	t.Run("skip locked backfills past held top row", func(t *testing.T) {
		testCtx, testCancel := context.WithTimeout(ctx, 10*time.Second)
		defer testCancel()
		now := time.Now().UTC().Truncate(time.Microsecond)
		ids := make([]string, 0, 3)
		for index := 0; index < 3; index++ {
			ids = append(ids, insertLeaseDueOrder(
				t, testCtx, pool, accountA, instrumentA, leaseDueOrderOptions{
					name: fmt.Sprintf("skip-locked-%d", index), status: "pending",
					nextReconcileAt: now.Add(-time.Minute + time.Duration(index)*time.Second),
					updatedAt:       now.Add(-time.Hour),
				},
			))
		}

		lockTx, err := pool.Begin(testCtx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lockTx.Rollback(context.Background()) }()
		var lockedID string
		if err := lockTx.QueryRow(testCtx, `
			SELECT id::text FROM trader_orders
			WHERE id=$1::uuid FOR UPDATE`, ids[0],
		).Scan(&lockedID); err != nil {
			t.Fatal(err)
		}

		leaseCtx, leaseCancel := context.WithTimeout(testCtx, 2*time.Second)
		defer leaseCancel()
		items, err := newLeaseDueOrdersRepository(pool).
			LeaseDueOrders(leaseCtx, 2, 5*time.Minute)
		if err != nil {
			t.Fatalf("lease with held lock: %v", err)
		}
		assertLeaseDueOrderIDSet(t, items, ids[1:])
		if err := lockTx.Rollback(testCtx); err != nil {
			t.Fatal(err)
		}

		top, err := repository.LeaseDueOrders(testCtx, 1, 5*time.Minute)
		if err != nil || len(top) != 1 || top[0].ID != ids[0] {
			t.Fatalf("lease released top row: items=%+v err=%v", top, err)
		}
	})

	t.Run("row count mismatch rolls back and releases locks", func(t *testing.T) {
		testCtx, testCancel := context.WithTimeout(ctx, 10*time.Second)
		defer testCancel()
		now := time.Now().UTC().Truncate(time.Microsecond)
		ids := []string{
			insertLeaseDueOrder(t, testCtx, pool, accountA, instrumentA, leaseDueOrderOptions{
				name: "rollback-skip", status: "pending",
				nextReconcileAt: now.Add(-2 * time.Minute), updatedAt: now.Add(-time.Hour),
			}),
			insertLeaseDueOrder(t, testCtx, pool, accountA, instrumentA, leaseDueOrderOptions{
				name: "rollback-update", status: "pending",
				nextReconcileAt: now.Add(-time.Minute), updatedAt: now.Add(-time.Hour),
			}),
		}
		if _, err := pool.Exec(testCtx, `
			CREATE FUNCTION lease_due_orders_skip_update() RETURNS trigger
			LANGUAGE plpgsql AS $$
			BEGIN
				IF OLD.idempotency_key='lease-rollback-skip' THEN
					RETURN NULL;
				END IF;
				RETURN NEW;
			END
			$$;
			CREATE TRIGGER lease_due_orders_skip_update
			BEFORE UPDATE OF reconcile_lease_until ON trader_orders
			FOR EACH ROW EXECUTE FUNCTION lease_due_orders_skip_update()`); err != nil {
			t.Fatal(err)
		}

		items, err := repository.LeaseDueOrders(testCtx, 2, 5*time.Minute)
		if err == nil || !strings.Contains(err.Error(), "updated 1 trader orders, expected 2") {
			t.Fatalf("row count mismatch: items=%+v err=%v", items, err)
		}
		for _, id := range ids {
			var leaseIsNull bool
			if err := pool.QueryRow(testCtx, `
				SELECT reconcile_lease_until IS NULL
				FROM trader_orders WHERE id=$1::uuid`, id,
			).Scan(&leaseIsNull); err != nil {
				t.Fatal(err)
			}
			if !leaseIsNull {
				t.Fatalf("order %s retained a lease after rollback", id)
			}
		}

		if _, err := pool.Exec(testCtx, `
			DROP TRIGGER lease_due_orders_skip_update ON trader_orders;
			DROP FUNCTION lease_due_orders_skip_update()`); err != nil {
			t.Fatal(err)
		}
		retried, err := repository.LeaseDueOrders(testCtx, 2, 5*time.Minute)
		if err != nil {
			t.Fatalf("retry after rollback: %v", err)
		}
		assertLeaseDueOrderIDSet(t, retried, ids)
	})
}

type leaseDueOrderOptions struct {
	name              string
	status            string
	createdAt         time.Time
	updatedAt         time.Time
	nextReconcileAt   time.Time
	leaseUntil        *time.Time
	reconcileFailures int
	executionID       string
}

func newLeaseDueOrdersTestPool(
	t *testing.T,
	ctx context.Context,
	dsn string,
) *pgxpool.Pool {
	t.Helper()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("trader_lease_due_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE")
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
	if err := database.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	return pool
}

func insertLeaseDueOrdersTradingFixture(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	exchange, symbol string,
) (int64, int64) {
	t.Helper()
	var accountID, instrumentID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO trading_accounts(
			owner_username,product_name,exchange,account_name,api_key_enc,api_secret_enc
		) VALUES('admin','ARBITRAGE',$1,$2,'x'::bytea,'y'::bytea)
		RETURNING id`, exchange, "lease-"+exchange,
	).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO instruments(
			exchange,exchange_symbol,base_asset,quote_asset,global_symbol,
			contract_type,status,settle_asset,contract_size,price_tick,quantity_step
		) VALUES($1,$2,'BTC','USDT','LEASEBTCUSDT',
			'perpetual','active','USDT',1,0.1,0.001)
		RETURNING id`, exchange, symbol,
	).Scan(&instrumentID); err != nil {
		t.Fatal(err)
	}
	return accountID, instrumentID
}

func insertLeaseDueOrdersArbitrageFixture(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	accountA, instrumentA, accountB, instrumentB int64,
) string {
	return insertLeaseDueOrdersArbitrageFixtureState(
		t, ctx, pool, repository,
		accountA, instrumentA, accountB, instrumentB,
		"primary", "running", true, arbitrageOrderReconcileError,
	)
}

func insertLeaseDueOrdersArbitrageFixtureState(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	accountA, instrumentA, accountB, instrumentB int64,
	suffix, status string,
	positionUncertain bool,
	errorMessage string,
) string {
	t.Helper()
	loadLeg := func(accountID, instrumentID int64) ArbitrageLeg {
		var leg ArbitrageLeg
		if err := pool.QueryRow(ctx, `
			SELECT a.product_name,a.account_name,a.exchange,
			       i.exchange_symbol,i.contract_type,i.base_asset,i.quote_asset
			FROM trading_accounts a
			JOIN instruments i ON i.id=$2
			WHERE a.id=$1`,
			accountID, instrumentID,
		).Scan(
			&leg.ProductName, &leg.AccountName, &leg.Exchange,
			&leg.ExchangeSymbol, &leg.ContractType, &leg.BaseAsset, &leg.QuoteAsset,
		); err != nil {
			t.Fatal(err)
		}
		leg.TradingAccountID = accountID
		leg.InstrumentID = instrumentID
		return leg
	}
	combination, inserted, err := repository.CreateArbitrageCombination(
		ctx,
		ArbitrageCombination{
			ID: uuid.NewString(), IdempotencyKey: "lease-arbitrage-combination-" + suffix,
			RequestFingerprint: "lease-arbitrage-fingerprint-" + suffix, OwnerUsername: "admin",
			LegA:            loadLeg(accountA, instrumentA),
			LegB:            loadLeg(accountB, instrumentB),
			AskThresholdBps: "10", BidThresholdBps: "-10",
			TargetNotional: "1000", OrderNotional: "100",
			ExecutionMode: "simultaneous_market", Status: "running",
			PositionNotional: "0", CumulativeTurnoverNotional: "0", MarketDataStale: true,
		},
	)
	if err != nil || !inserted {
		t.Fatalf("create arbitrage combination: inserted=%v err=%v", inserted, err)
	}
	execution, claimed, err := repository.ClaimArbitrageExecution(ctx, ArbitrageExecution{
		ID: uuid.NewString(), CombinationID: combination.ID, Direction: "ask", Status: "completed",
		TriggerAskSpread: "10", TriggerBidSpread: "-10",
		TriggerLegABid: "100", TriggerLegAAsk: "101",
		TriggerLegBBid: "99", TriggerLegBAsk: "102",
		TargetBaseQuantity: "1", RequestedNotional: "100", PositionEffect: "open",
	})
	if err != nil || !claimed {
		t.Fatalf("claim arbitrage execution: claimed=%v err=%v", claimed, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET status=$2,position_uncertain=$3,error_message=$4,
		    runtime_state=CASE WHEN $3 THEN 'position_uncertain' ELSE 'monitoring' END
		WHERE id=$1::uuid`,
		combination.ID, status, positionUncertain, errorMessage,
	); err != nil {
		t.Fatal(err)
	}
	return execution.ID
}

func insertLeaseDueOrder(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	accountID, instrumentID int64,
	options leaseDueOrderOptions,
) string {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if options.createdAt.IsZero() {
		options.createdAt = now.Add(-time.Hour)
	}
	if options.updatedAt.IsZero() {
		options.updatedAt = now.Add(-30 * time.Minute)
	}
	if options.nextReconcileAt.IsZero() {
		options.nextReconcileAt = now.Add(-time.Minute)
	}
	id := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO trader_orders(
			id,idempotency_key,owner_username,trading_account_id,product_name,
			exchange,instrument_id,contract_type,exchange_symbol,client_order_id,
			venue_order_id,side,order_type,quantity,price,filled_quantity,average_price,
			status,error_code,error_message,request_fingerprint,created_at,updated_at,
			base_asset,quote_asset,last_reconciled_at,next_reconcile_at,reconcile_failures,
			reconcile_lease_until,last_stream_event_at,last_venue_event_at,
			arbitrage_execution_id,arbitrage_leg,arbitrage_role,reduce_only
		) VALUES(
			$1::uuid,$2,'admin',$3,'Direct','binance',$4,'perpetual','LEASEBTCUSDT',$5,
			$6,'buy','limit',1.25,123.45,0.25,122.5,
			$7,'lease-code','lease-message',$8,$9,$10,
			'BTC','USDT',$9,$11,$12,$13,$9,$9,
			NULLIF($14,'')::uuid,
			CASE WHEN $14='' THEN NULL ELSE 'a' END,
			CASE WHEN $14='' THEN NULL ELSE 'market' END,
			$15
		)`,
		id, "lease-"+options.name, accountID, instrumentID,
		"lease-client-"+options.name, "venue-"+options.name, options.status,
		"lease-fingerprint-"+options.name, options.createdAt, options.updatedAt,
		options.nextReconcileAt, options.reconcileFailures, options.leaseUntil,
		options.executionID, options.name == "eligibility-global",
	); err != nil {
		t.Fatal(err)
	}
	return id
}

func getLeaseDueOrder(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	id string,
) Order {
	t.Helper()
	item, err := repository.GetByOwner(ctx, "admin", id)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func newLeaseDueOrdersRepository(pool *pgxpool.Pool) *Repository {
	return NewRepository(pool)
}

func assertLeaseDueOrderIDSet(t *testing.T, items []Order, want []string) {
	t.Helper()
	got := make(map[string]bool, len(items))
	for _, item := range items {
		got[item.ID] = true
	}
	assertLeaseDueOrderIDMap(t, got, want)
}

func assertLeaseDueOrderIDMap(t *testing.T, got map[string]bool, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("id count=%d want=%d: got=%v want=%v", len(got), len(want), got, want)
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("missing order %s: got=%v want=%v", id, got, want)
		}
	}
}
