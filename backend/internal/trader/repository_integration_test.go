package trader

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"selfquant/backend/internal/database"
)

func TestCreateIntentIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("trader_order_test_%d", time.Now().UnixNano())
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

	var accountID, instrumentID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO trading_accounts(
			owner_username,product_name,exchange,account_name,api_key_enc,api_secret_enc
		) VALUES('admin','Direct','binance','main','x'::bytea,'y'::bytea)
		RETURNING id`).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO instruments(
			exchange,exchange_symbol,base_asset,quote_asset,global_symbol,
			contract_type,status,settle_asset,contract_size,price_tick,quantity_step
		) VALUES('binance','BTCUSDT','BTC','USDT','BTCUSDT',
			'perpetual','active','USDT',1,0.1,0.001)
		RETURNING id`).Scan(&instrumentID); err != nil {
		t.Fatal(err)
	}

	repository := NewRepository(pool)
	input := Order{
		IdempotencyKey: "direct-order-key", OwnerUsername: "admin",
		TradingAccountID: accountID, ProductName: "Direct", Exchange: "binance",
		InstrumentID: instrumentID, ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "limit",
		Quantity: "0.001", Price: "100", RequestFingerprint: "direct-fingerprint",
		TwapJobID: "must-be-ignored", TwapSliceIndex: 9, TwapAttemptIndex: 2,
	}
	created, inserted, err := repository.CreateIntent(ctx, input)
	if err != nil || !inserted {
		t.Fatalf("create intent: inserted=%v err=%v", inserted, err)
	}
	var jobIsNull, sliceIsNull bool
	var attempt int
	if err := pool.QueryRow(ctx, `
		SELECT twap_job_id IS NULL, twap_slice_index IS NULL, twap_attempt_index
		FROM trader_orders WHERE id=$1::uuid`, created.ID,
	).Scan(&jobIsNull, &sliceIsNull, &attempt); err != nil {
		t.Fatal(err)
	}
	if !jobIsNull || !sliceIsNull || attempt != 0 {
		t.Fatalf(
			"unexpected TWAP fields: job_null=%v slice_null=%v attempt=%d",
			jobIsNull, sliceIsNull, attempt,
		)
	}
	replayed, inserted, err := repository.CreateIntent(ctx, input)
	if err != nil || inserted || replayed.ID != created.ID {
		t.Fatalf(
			"replay: inserted=%v id=%s want=%s err=%v",
			inserted, replayed.ID, created.ID, err,
		)
	}

	firstEventAt := time.Now().Add(-time.Minute)
	first, err := repository.ApplyStreamUpdate(ctx, created.ID, StreamUpdate{
		Result: VenueResult{
			VenueOrderID: "venue-1", Status: "partially_filled",
			FilledQuantity: "0.0004", AveragePrice: "100",
		},
		EventAt: firstEventAt,
		Fills: []OrderFill{{
			TradeID: "trade-1", Quantity: "0.0004", Price: "100",
			ExecutedAt: firstEventAt,
		}},
	})
	if err != nil || first.Status != "partially_filled" ||
		!parseDecimal(first.FilledQuantity).Equal(parseDecimal("0.0004")) {
		t.Fatalf("first stream update: order=%+v err=%v", first, err)
	}
	duplicate, err := repository.ApplyStreamUpdate(ctx, created.ID, StreamUpdate{
		Result: VenueResult{
			VenueOrderID: "venue-1", Status: "open",
			FilledQuantity: "0.0001", AveragePrice: "99",
		},
		EventAt: firstEventAt.Add(-time.Second),
		Fills: []OrderFill{{
			TradeID: "trade-1", Quantity: "0.0004", Price: "100",
			ExecutedAt: firstEventAt,
		}},
	})
	if err != nil || duplicate.Status != "partially_filled" ||
		!parseDecimal(duplicate.FilledQuantity).Equal(parseDecimal("0.0004")) {
		t.Fatalf("duplicate stream update regressed order: order=%+v err=%v", duplicate, err)
	}
	canceled, err := repository.ApplyStreamUpdate(ctx, created.ID, StreamUpdate{
		Result:  VenueResult{VenueOrderID: "venue-1", Status: "canceled", FilledQuantity: "0.0004"},
		EventAt: firstEventAt.Add(time.Second),
	})
	if err != nil || canceled.Status != "canceled" {
		t.Fatalf("cancel stream update: order=%+v err=%v", canceled, err)
	}
	filled, err := repository.ApplyStreamUpdate(ctx, created.ID, StreamUpdate{
		Result:  VenueResult{VenueOrderID: "venue-1", Status: "canceled"},
		EventAt: firstEventAt.Add(2 * time.Second),
		Fills: []OrderFill{{
			TradeID: "trade-2", Quantity: "0.0006", Price: "101",
			ExecutedAt: firstEventAt.Add(2 * time.Second),
		}},
	})
	if err != nil || filled.Status != "filled" ||
		!parseDecimal(filled.FilledQuantity).Equal(parseDecimal("0.001")) {
		t.Fatalf("late fill did not converge: order=%+v err=%v", filled, err)
	}
	afterREST, err := repository.UpdateResult(ctx, created.ID, VenueResult{
		VenueOrderID: "venue-1", Status: "open", FilledQuantity: "0.0002", AveragePrice: "98",
	})
	if err != nil || afterREST.Status != "filled" ||
		!parseDecimal(afterREST.FilledQuantity).Equal(parseDecimal("0.001")) {
		t.Fatalf("REST update regressed order: order=%+v err=%v", afterREST, err)
	}
	var fillCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM trader_order_fills WHERE order_id=$1::uuid`, created.ID,
	).Scan(&fillCount); err != nil {
		t.Fatal(err)
	}
	if fillCount != 2 {
		t.Fatalf("fill count=%d want=2", fillCount)
	}
}
