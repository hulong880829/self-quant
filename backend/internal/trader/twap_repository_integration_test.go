package trader

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"selfquant/backend/internal/database"
)

func TestTwapRepositoryIntegration(t *testing.T) {
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
	schema := fmt.Sprintf("trader_twap_test_%d", time.Now().UnixNano())
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
		) VALUES('admin','TWAP','binance','main','x'::bytea,'y'::bytea)
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
	jobs := make([]TwapJob, 0, 5)
	for i := 0; i < 5; i++ {
		job := integrationTwapJob(accountID, instrumentID, fmt.Sprintf("key-%d", i))
		created, inserted, err := repository.CreateTwap(ctx, job)
		if err != nil || !inserted {
			t.Fatalf("create job %d: inserted=%v err=%v", i, inserted, err)
		}
		jobs = append(jobs, created)
	}
	repeated, inserted, err := repository.CreateTwap(
		ctx, integrationTwapJob(accountID, instrumentID, "key-0"),
	)
	if err != nil || inserted || repeated.ID != jobs[0].ID {
		t.Fatalf("idempotent create: inserted=%v job=%s err=%v", inserted, repeated.ID, err)
	}
	if _, _, err := repository.CreateTwap(
		ctx, integrationTwapJob(accountID, instrumentID, "key-over-limit"),
	); err != ErrActiveTwapLimit {
		t.Fatalf("active limit err=%v", err)
	}

	child, inserted, err := repository.CreateTwapIntent(ctx, jobs[0].ID, Order{
		ID: uuid.NewString(), IdempotencyKey: "twap-child-0", OwnerUsername: "admin",
		TradingAccountID: accountID, ProductName: "TWAP", Exchange: "binance",
		InstrumentID: instrumentID, ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", ClientOrderID: "sqtintegration000001",
		Side: "buy", OrderType: "market", Quantity: "0.1",
		RequestFingerprint: "fingerprint", TwapJobID: jobs[0].ID, TwapSliceIndex: 0,
	})
	if err != nil || !inserted || child.TwapJobID != jobs[0].ID {
		t.Fatalf("create child: inserted=%v child=%+v err=%v", inserted, child, err)
	}
	if _, err := repository.UpdateResult(ctx, child.ID, VenueResult{
		Status: "canceled", FilledQuantity: "0.04", AveragePrice: "100",
	}); err != nil {
		t.Fatal(err)
	}
	retryInput := Order{
		ID: uuid.NewString(), IdempotencyKey: "twap-child-0-attempt-1", OwnerUsername: "admin",
		TradingAccountID: accountID, ProductName: "TWAP", Exchange: "binance",
		InstrumentID: instrumentID, ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", ClientOrderID: "sqtintegration000002",
		Side: "buy", OrderType: "limit", Quantity: "0.06", Price: "100",
		RequestFingerprint: "retry-fingerprint", TwapJobID: jobs[0].ID,
		TwapSliceIndex: 0, TwapAttemptIndex: 1,
	}
	retry, inserted, err := repository.CreateTwapIntent(ctx, jobs[0].ID, retryInput)
	if err != nil || !inserted || retry.TwapAttemptIndex != 1 {
		t.Fatalf("create retry: inserted=%v retry=%+v err=%v", inserted, retry, err)
	}
	repeatedRetry, inserted, err := repository.CreateTwapIntent(ctx, jobs[0].ID, retryInput)
	if err != nil || inserted || repeatedRetry.ID != retry.ID {
		t.Fatalf("idempotent retry: inserted=%v retry=%+v err=%v", inserted, repeatedRetry, err)
	}
	conflictingAttempt := retryInput
	conflictingAttempt.ID = uuid.NewString()
	conflictingAttempt.IdempotencyKey = "twap-child-active-conflict"
	conflictingAttempt.ClientOrderID = "sqtintegration000003"
	conflictingAttempt.RequestFingerprint = "active-conflict"
	conflictingAttempt.TwapAttemptIndex = 2
	if _, _, err := repository.CreateTwapIntent(ctx, jobs[0].ID, conflictingAttempt); err == nil {
		t.Fatal("expected active child uniqueness error")
	}
	closed, err := repository.CloseTwap(ctx, jobs[0].ID, "canceled", "test")
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != "canceled" || closed.NextActionAt.IsZero() {
		t.Fatalf("closed job was not readable: %+v", closed)
	}
	if got, err := repository.GetTwap(ctx, jobs[0].ID); err != nil || got.Status != "canceled" {
		t.Fatalf("get canceled job: status=%s err=%v", got.Status, err)
	}
	due := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	created := due.Add(-time.Hour)
	for index := 1; index < len(jobs); index++ {
		next := due.Add(time.Duration(index-1) * time.Second)
		if index == 2 {
			next = due
		}
		if _, err := pool.Exec(ctx, `
			UPDATE trader_twap_jobs
			SET next_action_at=$2,created_at=$3,scheduler_lease_until=NULL
			WHERE id=$1::uuid`,
			jobs[index].ID, next, created,
		); err != nil {
			t.Fatal(err)
		}
	}
	leased, err := repository.LeaseDueTwaps(ctx, 4, time.Minute)
	if err != nil || len(leased) != 4 {
		t.Fatalf("lease ordered twaps=%d err=%v", len(leased), err)
	}
	tied := []string{jobs[1].ID, jobs[2].ID}
	sort.Strings(tied)
	wantOrder := []string{tied[0], tied[1], jobs[3].ID, jobs[4].ID}
	for index := range wantOrder {
		if leased[index].ID != wantOrder[index] {
			t.Fatalf("leased order[%d]=%s want=%s", index, leased[index].ID, wantOrder[index])
		}
	}

	if _, err := pool.Exec(ctx, `
		UPDATE trader_twap_jobs SET scheduler_lease_until=NULL
		WHERE id=ANY($1::uuid[])`, wantOrder,
	); err != nil {
		t.Fatal(err)
	}
	concurrent := []*Repository{NewRepository(pool), NewRepository(pool)}
	results := make([][]TwapJob, len(concurrent))
	errs := make([]error, len(concurrent))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range concurrent {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			results[index], errs[index] = concurrent[index].LeaseDueTwaps(ctx, 2, time.Minute)
		}(index)
	}
	close(start)
	wait.Wait()
	seen := make(map[string]bool, 4)
	for index := range results {
		if errs[index] != nil {
			t.Fatal(errs[index])
		}
		for _, job := range results[index] {
			if seen[job.ID] {
				t.Fatalf("TWAP %s leased by both workers", job.ID)
			}
			seen[job.ID] = true
		}
	}
	if len(seen) != 4 {
		t.Fatalf("concurrent workers leased %d TWAPs, want 4", len(seen))
	}
	running, _, err := repository.ListTwaps(ctx, "admin", accountID, "running", "", 50, "")
	if err != nil || len(running) != 4 {
		t.Fatalf("running jobs=%d err=%v", len(running), err)
	}
	history, _, err := repository.ListTwaps(ctx, "admin", accountID, "closed", "", 50, "")
	if err != nil || len(history) != 1 || history[0].Status != "canceled" {
		t.Fatalf("closed jobs=%+v err=%v", history, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_twap_jobs SET closed_at=now()-interval '8 days'
		WHERE id=$1::uuid`, jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := repository.DeleteClosedTwaps(ctx, time.Now().UTC().Add(-7*24*time.Hour), 100)
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	var parent *string
	if err := pool.QueryRow(ctx, `
		SELECT twap_job_id::text FROM trader_orders WHERE id=$1::uuid`, child.ID).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if parent != nil {
		t.Fatalf("child parent was not cleared: %v", *parent)
	}
}

func integrationTwapJob(accountID, instrumentID int64, key string) TwapJob {
	start := time.Now().UTC().Add(time.Minute)
	return TwapJob{
		ID: uuid.NewString(), IdempotencyKey: key, RequestFingerprint: "same",
		OwnerUsername: "admin", TradingAccountID: accountID, ProductName: "TWAP",
		Exchange: "binance", InstrumentID: instrumentID, ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		Side: "buy", TotalQuantity: "1", StartAt: start, EndAt: start.Add(time.Hour),
		IntervalSeconds: 60, ExecutionType: "market", Status: "pending",
		NextActionAt: start,
	}
}
