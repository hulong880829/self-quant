package trader

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"selfquant/backend/internal/database"
)

func TestArbitrageRepositoryIntegration(t *testing.T) {
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
	schema := fmt.Sprintf("trader_arbitrage_test_%d", time.Now().UnixNano())
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

	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "BTCUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "BTC-USDT-SWAP")
	repository := NewRepository(pool)
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	created, inserted, err := repository.CreateArbitrageCombination(ctx, input)
	if err != nil || !inserted {
		t.Fatalf("create inserted=%v err=%v", inserted, err)
	}
	repeated, inserted, err := repository.CreateArbitrageCombination(ctx, input)
	if err != nil || inserted || repeated.ID != created.ID {
		t.Fatalf("idempotency inserted=%v id=%s err=%v", inserted, repeated.ID, err)
	}
	if _, err := repository.GetArbitrageCombinationByOwner(ctx, "other-owner", created.ID); err != ErrNotFound {
		t.Fatalf("owner isolation err=%v", err)
	}
	leased, err := repository.LeaseArbitrageCombinations(ctx, 10, 10*time.Second)
	if err != nil || len(leased) != 1 || leased[0].ID != created.ID {
		t.Fatalf("leased=%+v err=%v", leased, err)
	}
	renewed, err := repository.RenewArbitrageLease(ctx, created.ID, 10*time.Second)
	if err != nil || !renewed {
		t.Fatalf("renewed=%v err=%v", renewed, err)
	}

	var claimWG sync.WaitGroup
	var claimMu sync.Mutex
	var execution ArbitrageExecution
	claimCount := 0
	claimErrors := make(chan error, 16)
	for index := 0; index < 16; index++ {
		claimWG.Add(1)
		go func() {
			defer claimWG.Done()
			item, claimed, claimErr := repository.ClaimArbitrageExecution(
				ctx, integrationArbitrageExecution(created.ID, "ask"),
			)
			if claimErr != nil {
				claimErrors <- claimErr
				return
			}
			if claimed {
				claimMu.Lock()
				claimCount++
				execution = item
				claimMu.Unlock()
			}
		}()
	}
	claimWG.Wait()
	close(claimErrors)
	for claimErr := range claimErrors {
		t.Fatal(claimErr)
	}
	if claimCount != 1 {
		t.Fatalf("concurrent claim count=%d", claimCount)
	}
	_, claimed, err := repository.ClaimArbitrageExecution(ctx, integrationArbitrageExecution(created.ID, "ask"))
	if err != nil || claimed {
		t.Fatalf("duplicate claim=%v err=%v", claimed, err)
	}
	_, claimed, err = repository.ClaimArbitrageExecution(ctx, integrationArbitrageExecution(created.ID, "bid"))
	if err != nil || !claimed {
		t.Fatalf("bid claim=%v err=%v", claimed, err)
	}
	order, _, err := repository.CreateIntent(ctx, Order{
		IdempotencyKey: "arb-order-integration", OwnerUsername: "admin",
		TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
		InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
		Quantity: "0.01", RequestFingerprint: "arb-order-fingerprint",
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "market",
	})
	if err != nil {
		t.Fatal(err)
	}
	pairInput := []Order{
		{
			IdempotencyKey: "arb-pair-a", OwnerUsername: "admin",
			TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
			InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
			Quantity: "0.01", RequestFingerprint: "arb-pair-a-fingerprint",
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "market",
		},
		{
			IdempotencyKey: "arb-pair-b", OwnerUsername: "admin",
			TradingAccountID: accountB, ProductName: "ARBITRAGE", Exchange: "okx",
			InstrumentID: instrumentB, ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
			BaseAsset: "BTC", QuoteAsset: "USDT", Side: "sell", OrderType: "market",
			Quantity: "0.01", RequestFingerprint: "arb-pair-b-fingerprint",
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "b", ArbitrageRole: "market",
		},
	}
	_, createdFlags, err := repository.CreateArbitrageIntents(ctx, pairInput)
	if err != nil || len(createdFlags) != 2 || !createdFlags[0] || !createdFlags[1] {
		t.Fatalf("atomic pair flags=%v err=%v", createdFlags, err)
	}
	_, createdFlags, err = repository.CreateArbitrageIntents(ctx, pairInput)
	if err != nil || createdFlags[0] || createdFlags[1] {
		t.Fatalf("recovered pair flags=%v err=%v", createdFlags, err)
	}
	for _, direction := range []string{"ask", "bid"} {
		active, err := repository.GetActiveArbitrageExecution(ctx, created.ID, direction)
		if err != nil {
			t.Fatal(err)
		}
		active.Status = "completed"
		active.HedgeSequence = 7
		updated, err := repository.UpdateArbitrageExecution(ctx, active)
		if err != nil {
			t.Fatal(err)
		}
		if updated.HedgeSequence != 7 {
			t.Fatalf("hedge sequence=%d want=7", updated.HedgeSequence)
		}
	}
	var completionWG sync.WaitGroup
	completionErrors := make(chan error, 16)
	for index := 0; index < 16; index++ {
		completionWG.Add(1)
		go func() {
			defer completionWG.Done()
			if _, err := repository.AddArbitrageCompletedNotional(ctx, created.ID, "10"); err != nil {
				completionErrors <- err
			}
		}()
	}
	completionWG.Wait()
	close(completionErrors)
	for completionErr := range completionErrors {
		t.Fatal(completionErr)
	}
	refreshed, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.CompletedNotional != "160" {
		t.Fatalf("atomic completed notional=%s", refreshed.CompletedNotional)
	}
	created = refreshed
	created.Status = "closed"
	if _, err := repository.UpdateArbitrageCombinationRuntime(ctx, created); err != nil {
		t.Fatal(err)
	}
	deleted, err := repository.DeleteExpiredArbitrageCombinations(ctx, time.Now().Add(time.Hour), 10)
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	var executionID *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT arbitrage_execution_id FROM trader_orders WHERE id=$1::uuid`, order.ID).Scan(&executionID); err != nil {
		t.Fatal(err)
	}
	if executionID != nil {
		t.Fatalf("order execution reference retained: %v", executionID)
	}
}

func insertArbitrageFixture(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	venue, symbol string,
) (int64, int64) {
	t.Helper()
	var accountID, instrumentID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO trading_accounts(
			owner_username,product_name,exchange,account_name,api_key_enc,api_secret_enc
		) VALUES('admin','ARBITRAGE',$1,$1 || '-main','x'::bytea,'y'::bytea)
		RETURNING id`, venue).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO instruments(
			exchange,exchange_symbol,base_asset,quote_asset,global_symbol,
			contract_type,status,settle_asset,contract_size,price_tick,quantity_step
		) VALUES($1,$2,'BTC','USDT','BTCUSDT','perpetual','active','USDT',1,0.1,0.001)
		RETURNING id`, venue, symbol).Scan(&instrumentID); err != nil {
		t.Fatal(err)
	}
	return accountID, instrumentID
}

func integrationArbitrageCombination(
	accountA, instrumentA, accountB, instrumentB int64,
) ArbitrageCombination {
	return ArbitrageCombination{
		ID: uuid.NewString(), IdempotencyKey: "arb-combination-integration",
		RequestFingerprint: "arb-fingerprint", OwnerUsername: "admin",
		LegA: ArbitrageLeg{
			TradingAccountID: accountA, InstrumentID: instrumentA,
			ProductName: "ARBITRAGE", AccountName: "binance-main", Exchange: "binance",
			ContractType: "perpetual", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: accountB, InstrumentID: instrumentB,
			ProductName: "ARBITRAGE", AccountName: "okx-main", Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP", BaseAsset: "BTC", QuoteAsset: "USDT",
		},
		AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "10000", OrderNotional: "500", MaxDeltaNotional: "10",
		ExecutionMode: "simultaneous_market", Status: "running",
		CompletedNotional: "0", MarketDataStale: true,
	}
}

func integrationArbitrageExecution(combinationID, direction string) ArbitrageExecution {
	return ArbitrageExecution{
		ID: uuid.NewString(), CombinationID: combinationID, Direction: direction, Status: "claimed",
		TriggerAskSpread: "12", TriggerBidSpread: "-8",
		TriggerLegABid: "100", TriggerLegAAsk: "101",
		TriggerLegBBid: "99", TriggerLegBAsk: "102",
		TargetBaseQuantity: "1", LegAFilledQuantity: "0",
		LegBFilledQuantity: "0", DeltaNotional: "0",
	}
}
