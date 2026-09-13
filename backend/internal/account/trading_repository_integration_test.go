package account

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"selfquant/backend/internal/database"
)

func TestDeleteByOwnerGuardsActiveJobs(t *testing.T) {
	pool := openTradingRepoTestPool(t)
	ctx := context.Background()
	repo := NewTradingRepository(pool)

	t.Run("closed arb and rejected order can delete", func(t *testing.T) {
		accountID, otherID, instrumentA, instrumentB := insertDeleteGuardAccounts(t, ctx, pool, "closed")
		insertArbitrageCombination(t, ctx, pool, accountID, otherID, instrumentA, instrumentB, "closed-arb", "closed")
		insertRejectedOrder(t, ctx, pool, accountID, instrumentA, "closed-order")
		if err := repo.DeleteByOwner(ctx, "admin", accountID); err != nil {
			t.Fatalf("delete closed: %v", err)
		}
		assertAccountGone(t, ctx, pool, accountID)
	})

	t.Run("running arb blocks delete", func(t *testing.T) {
		accountID, otherID, instrumentA, instrumentB := insertDeleteGuardAccounts(t, ctx, pool, "run")
		insertArbitrageCombination(t, ctx, pool, accountID, otherID, instrumentA, instrumentB, "run-arb", "running")
		if err := repo.DeleteByOwner(ctx, "admin", accountID); !errors.Is(err, ErrTradingAccountHasActiveArbitrage) {
			t.Fatalf("err=%v", err)
		}
		assertAccountPresent(t, ctx, pool, accountID)
	})

	t.Run("closing arb blocks delete", func(t *testing.T) {
		accountID, otherID, instrumentA, instrumentB := insertDeleteGuardAccounts(t, ctx, pool, "close")
		insertArbitrageCombination(t, ctx, pool, accountID, otherID, instrumentA, instrumentB, "closing-arb", "closing")
		if err := repo.DeleteByOwner(ctx, "admin", accountID); !errors.Is(err, ErrTradingAccountHasActiveArbitrage) {
			t.Fatalf("err=%v", err)
		}
		assertAccountPresent(t, ctx, pool, accountID)
	})

	t.Run("pending twap blocks delete", func(t *testing.T) {
		accountID, _, instrumentA, _ := insertDeleteGuardAccounts(t, ctx, pool, "twap-p")
		insertTwapJob(t, ctx, pool, accountID, instrumentA, "pending-twap", "pending")
		if err := repo.DeleteByOwner(ctx, "admin", accountID); !errors.Is(err, ErrTradingAccountHasActiveTWAP) {
			t.Fatalf("err=%v", err)
		}
		assertAccountPresent(t, ctx, pool, accountID)
	})

	t.Run("running twap blocks delete", func(t *testing.T) {
		accountID, _, instrumentA, _ := insertDeleteGuardAccounts(t, ctx, pool, "twap-r")
		insertTwapJob(t, ctx, pool, accountID, instrumentA, "running-twap", "running")
		if err := repo.DeleteByOwner(ctx, "admin", accountID); !errors.Is(err, ErrTradingAccountHasActiveTWAP) {
			t.Fatalf("err=%v", err)
		}
		assertAccountPresent(t, ctx, pool, accountID)
	})

	t.Run("completed and canceled twap can delete", func(t *testing.T) {
		accountID, _, instrumentA, _ := insertDeleteGuardAccounts(t, ctx, pool, "twap-d")
		insertTwapJob(t, ctx, pool, accountID, instrumentA, "done-twap", "completed")
		insertTwapJob(t, ctx, pool, accountID, instrumentA, "canceled-twap", "canceled")
		if err := repo.DeleteByOwner(ctx, "admin", accountID); err != nil {
			t.Fatalf("delete finished twap: %v", err)
		}
		assertAccountGone(t, ctx, pool, accountID)
	})
}

func openTradingRepoTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("account_delete_test_%d", time.Now().UnixNano())
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
	t.Cleanup(pool.Close)
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func insertDeleteGuardAccounts(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	suffix string,
) (accountID, otherID, instrumentA, instrumentB int64) {
	t.Helper()
	if err := pool.QueryRow(ctx, `
		INSERT INTO trading_accounts(
			owner_username,product_name,exchange,account_name,api_key_enc,api_secret_enc
		) VALUES('admin','DeleteGuard','binance','a-`+suffix+`','x'::bytea,'y'::bytea)
		RETURNING id`).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO trading_accounts(
			owner_username,product_name,exchange,account_name,api_key_enc,api_secret_enc
		) VALUES('admin','DeleteGuard','okx','b-`+suffix+`','x'::bytea,'y'::bytea)
		RETURNING id`).Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO instruments(
			exchange,exchange_symbol,base_asset,quote_asset,global_symbol,
			contract_type,status,settle_asset,contract_size,price_tick,quantity_step
		) VALUES('binance',$1,'BTC','USDT','BTCUSDT',
			'perpetual','active','USDT',1,0.1,0.001)
		RETURNING id`, "BTCUSDT-"+suffix).Scan(&instrumentA); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO instruments(
			exchange,exchange_symbol,base_asset,quote_asset,global_symbol,
			contract_type,status,settle_asset,contract_size,price_tick,quantity_step
		) VALUES('okx',$1,'BTC','USDT','BTCUSDT',
			'perpetual','active','USDT',1,0.1,0.001)
		RETURNING id`, "BTC-USDT-"+suffix).Scan(&instrumentB); err != nil {
		t.Fatal(err)
	}
	return accountID, otherID, instrumentA, instrumentB
}

func insertArbitrageCombination(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	accountA, accountB, instrumentA, instrumentB int64,
	key, status string,
) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO trader_arbitrage_combinations (
			id,idempotency_key,request_fingerprint,owner_username,
			leg_a_trading_account_id,leg_a_instrument_id,leg_a_product_name,leg_a_account_name,
			leg_a_exchange,leg_a_contract_type,leg_a_exchange_symbol,leg_a_base_asset,leg_a_quote_asset,
			leg_b_trading_account_id,leg_b_instrument_id,leg_b_product_name,leg_b_account_name,
			leg_b_exchange,leg_b_contract_type,leg_b_exchange_symbol,leg_b_base_asset,leg_b_quote_asset,
			ask_threshold_bps,bid_threshold_bps,target_notional,order_notional,
			execution_mode,status,closed_at
		) VALUES (
			$1::uuid,$2,'fp','admin',
			$3,$4,'DeleteGuard','a','binance','perpetual','BTCUSDT','BTC','USDT',
			$5,$6,'DeleteGuard','b','okx','perpetual','BTC-USDT-SWAP','BTC','USDT',
			12,-8,10000,500,'simultaneous_market',$7,
			CASE WHEN $7 IN ('closed','failed') THEN now() ELSE NULL END
		)`, uuid.NewString(), key, accountA, instrumentA, accountB, instrumentB, status); err != nil {
		t.Fatal(err)
	}
}

func insertRejectedOrder(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	accountID, instrumentID int64,
	key string,
) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO trader_orders (
			id,idempotency_key,owner_username,trading_account_id,product_name,
			exchange,instrument_id,contract_type,exchange_symbol,client_order_id,
			side,order_type,quantity,status,request_fingerprint,base_asset,quote_asset
		) VALUES (
			$1::uuid,$2,'admin',$3,'DeleteGuard','binance',$4,'perpetual','BTCUSDT',$2,
			'buy','limit',0.001,'rejected',$2,'BTC','USDT'
		)`, uuid.NewString(), key, accountID, instrumentID); err != nil {
		t.Fatal(err)
	}
}

func insertTwapJob(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	accountID, instrumentID int64,
	key, status string,
) {
	t.Helper()
	start := time.Now().UTC().Add(time.Minute)
	if _, err := pool.Exec(ctx, `
		INSERT INTO trader_twap_jobs (
			id,idempotency_key,request_fingerprint,owner_username,trading_account_id,
			product_name,exchange,instrument_id,contract_type,exchange_symbol,
			base_asset,quote_asset,side,total_quantity,start_at,end_at,
			interval_seconds,execution_type,status,next_action_at,closed_at
		) VALUES (
			$1::uuid,$2,'fp','admin',$3,'DeleteGuard','binance',$4,'perpetual','BTCUSDT',
			'BTC','USDT','buy',1,$5,$6,60,'market',$7,$5,
			CASE WHEN $7 IN ('completed','partially_completed','canceled','failed')
			     THEN now() ELSE NULL END
		)`, uuid.NewString(), key, accountID, instrumentID, start, start.Add(time.Hour), status); err != nil {
		t.Fatal(err)
	}
}

func assertAccountGone(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) {
	t.Helper()
	var leftover int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM trading_accounts WHERE id=$1`, id).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover != 0 {
		t.Fatalf("account %d still present", id)
	}
}

func assertAccountPresent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) {
	t.Helper()
	var leftover int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM trading_accounts WHERE id=$1`, id).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover != 1 {
		t.Fatalf("account %d missing", id)
	}
}
