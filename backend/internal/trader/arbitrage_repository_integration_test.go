package trader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"selfquant/backend/internal/database"
	"selfquant/backend/internal/trader/exchange"
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
	assertArbitrageThresholdSignChecksDropped(t, ctx, pool)
	assertArbitrageSameVenueChecksDropped(t, ctx, pool)
	assertArbitrageMaxDeltaDropped(t, ctx, pool)

	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "BTCUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "BTC-USDT-SWAP")
	repository := NewRepository(pool)
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.LegAVenueBaselineBasePosition = "12.5"
	input.LegBVenueBaselineBasePosition = "-8"
	input.VenueBaselineCapturedAt = time.Now().UTC().Truncate(time.Microsecond)
	created, inserted, err := repository.CreateArbitrageCombination(ctx, input)
	if err != nil || !inserted {
		t.Fatalf("create inserted=%v err=%v", inserted, err)
	}
	if created.RuntimeState != "monitoring" {
		t.Fatalf("initial runtime state=%s", created.RuntimeState)
	}
	if created.LegAVenueBaselineBasePosition != "12.5" ||
		created.LegBVenueBaselineBasePosition != "-8" ||
		!created.VenueBaselineCapturedAt.Equal(input.VenueBaselineCapturedAt) {
		t.Fatalf("baseline was not persisted: %+v", created)
	}
	conflicting := input
	conflicting.ID = uuid.NewString()
	conflicting.IdempotencyKey = "arb-combination-active-conflict"
	conflicting.RequestFingerprint = "arb-fingerprint-active-conflict"
	if _, _, err := repository.CreateArbitrageCombination(ctx, conflicting); !errors.Is(
		err, ErrActiveArbitrageInstrumentConflict,
	) || !strings.Contains(err.Error(), created.ID) {
		t.Fatalf("active instrument conflict err=%v", err)
	}
	uniqueAccountA, uniqueInstrumentA := insertArbitrageFixture(
		t, ctx, pool, "bybit", "UNIQUEAUSDT",
	)
	uniqueAccountB, uniqueInstrumentB := insertArbitrageFixture(
		t, ctx, pool, "bitget", "UNIQUEBUSDT",
	)
	uniqueInput := integrationArbitrageCombination(
		uniqueAccountA, uniqueInstrumentA, uniqueAccountB, uniqueInstrumentB,
	)
	conflictLegs := []struct {
		name string
		a    ArbitrageLeg
		b    ArbitrageLeg
	}{
		{name: "existing-a-to-proposed-a", a: input.LegA, b: uniqueInput.LegB},
		{name: "existing-a-to-proposed-b", a: uniqueInput.LegA, b: input.LegA},
		{name: "existing-b-to-proposed-a", a: input.LegB, b: uniqueInput.LegB},
		{name: "existing-b-to-proposed-b", a: uniqueInput.LegA, b: input.LegB},
	}
	for index, test := range conflictLegs {
		candidate := uniqueInput
		candidate.ID = uuid.NewString()
		candidate.IdempotencyKey = fmt.Sprintf("arb-active-conflict-%d", index)
		candidate.RequestFingerprint = fmt.Sprintf("arb-active-conflict-fingerprint-%d", index)
		candidate.LegA, candidate.LegB = test.a, test.b
		if _, _, err := repository.CreateArbitrageCombination(
			ctx, candidate,
		); !errors.Is(err, ErrActiveArbitrageInstrumentConflict) {
			t.Fatalf("%s err=%v", test.name, err)
		}
	}
	statusCombo := uniqueInput
	statusCombo.ID = uuid.NewString()
	statusCombo.IdempotencyKey = "arb-active-status-owner"
	statusCombo.RequestFingerprint = "arb-active-status-owner-fingerprint"
	statusCombo, inserted, err = repository.CreateArbitrageCombination(ctx, statusCombo)
	if err != nil || !inserted {
		t.Fatalf("create status combo inserted=%v err=%v", inserted, err)
	}
	statusCombo.Status = "closing"
	if _, err := repository.UpdateArbitrageCombinationRuntime(ctx, statusCombo); err != nil {
		t.Fatal(err)
	}
	statusReplacement := uniqueInput
	statusReplacement.ID = uuid.NewString()
	statusReplacement.IdempotencyKey = "arb-active-status-replacement"
	statusReplacement.RequestFingerprint = "arb-active-status-replacement-fingerprint"
	if _, _, err := repository.CreateArbitrageCombination(
		ctx, statusReplacement,
	); !errors.Is(err, ErrActiveArbitrageInstrumentConflict) {
		t.Fatalf("closing combination did not retain ownership: %v", err)
	}
	statusCombo.Status = "closed"
	if _, err := repository.UpdateArbitrageCombinationRuntime(ctx, statusCombo); err != nil {
		t.Fatal(err)
	}
	statusReplacement, inserted, err = repository.CreateArbitrageCombination(
		ctx, statusReplacement,
	)
	if err != nil || !inserted {
		t.Fatalf("closed combination did not release ownership: inserted=%v err=%v", inserted, err)
	}
	statusReplacement.Status = "closed"
	if _, err := repository.UpdateArbitrageCombinationRuntime(ctx, statusReplacement); err != nil {
		t.Fatal(err)
	}
	concurrentAccountA, concurrentInstrumentA := insertArbitrageFixture(
		t, ctx, pool, "gate", "CONCURRENTA_USDT",
	)
	concurrentAccountB, concurrentInstrumentB := insertArbitrageFixture(
		t, ctx, pool, "okx", "CONCURRENTB-USDT-SWAP",
	)
	concurrentBase := integrationArbitrageCombination(
		concurrentAccountA, concurrentInstrumentA,
		concurrentAccountB, concurrentInstrumentB,
	)
	type concurrentCreateResult struct {
		item     ArbitrageCombination
		inserted bool
		err      error
	}
	results := make(chan concurrentCreateResult, 2)
	var creates sync.WaitGroup
	for index := 0; index < 2; index++ {
		creates.Add(1)
		go func(index int) {
			defer creates.Done()
			candidate := concurrentBase
			candidate.ID = uuid.NewString()
			candidate.IdempotencyKey = fmt.Sprintf("arb-concurrent-create-%d", index)
			candidate.RequestFingerprint = fmt.Sprintf("arb-concurrent-fingerprint-%d", index)
			item, inserted, createErr := repository.CreateArbitrageCombination(ctx, candidate)
			results <- concurrentCreateResult{item: item, inserted: inserted, err: createErr}
		}(index)
	}
	creates.Wait()
	close(results)
	successes, conflicts := 0, 0
	var concurrentWinner ArbitrageCombination
	for result := range results {
		switch {
		case result.err == nil && result.inserted:
			successes++
			concurrentWinner = result.item
		case errors.Is(result.err, ErrActiveArbitrageInstrumentConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent create result=%+v", result)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent creates successes=%d conflicts=%d", successes, conflicts)
	}
	concurrentWinner.Status = "closed"
	if _, err := repository.UpdateArbitrageCombinationRuntime(
		ctx, concurrentWinner,
	); err != nil {
		t.Fatal(err)
	}
	askThreshold := "-7.5"
	configured, err := repository.UpdateArbitrageCombinationConfig(
		ctx, "admin", created.ID, UpdateArbitrageInput{
			AskThresholdBps: &askThreshold,
		},
	)
	if err != nil || configured.AskThresholdBps != "-7.5" ||
		configured.BidThresholdBps != created.BidThresholdBps ||
		configured.TargetNotional != created.TargetNotional {
		t.Fatalf("configured=%+v err=%v", configured, err)
	}
	events, err := repository.ListArbitrageEvents(ctx, created.ID, 10)
	if err != nil || len(events) == 0 || events[0].Type != "config_updated" {
		t.Fatalf("config events=%+v err=%v", events, err)
	}
	if _, err := repository.UpdateArbitrageCombinationConfig(
		ctx, "other-owner", created.ID, UpdateArbitrageInput{AskThresholdBps: &askThreshold},
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner config update err=%v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET position_notional=11000
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	askWhileOverTarget := "-6"
	bidWhileOverTarget := "5"
	overTarget, err := repository.UpdateArbitrageCombinationConfig(
		ctx, "admin", created.ID, UpdateArbitrageInput{AskThresholdBps: &askWhileOverTarget},
	)
	if err != nil || overTarget.AskThresholdBps != "-6" {
		t.Fatalf("ask update while over target=%+v err=%v", overTarget, err)
	}
	overTarget, err = repository.UpdateArbitrageCombinationConfig(
		ctx, "admin", created.ID, UpdateArbitrageInput{BidThresholdBps: &bidWhileOverTarget},
	)
	if err != nil || overTarget.BidThresholdBps != "5" {
		t.Fatalf("bid update while over target=%+v err=%v", overTarget, err)
	}
	tooSmallTarget := "2000"
	if _, err := repository.UpdateArbitrageCombinationConfig(
		ctx, "admin", created.ID, UpdateArbitrageInput{TargetNotional: &tooSmallTarget},
	); !errors.Is(err, ErrArbitrageConfigInvalid) ||
		!strings.Contains(err.Error(), "current absolute position 11000") {
		t.Fatalf("target below position err=%v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET position_notional=0
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	auditMessage := arbitragePositionAuditErrorPrefix + " leg_a=0.01 leg_b=0"
	auditMarked, err := repository.UpdateArbitragePositionAudit(
		ctx, created.ID, arbitrageVersion(t, ctx, pool, created.ID),
		"0.01", "0", "0.01", "0", true, auditMessage,
	)
	if err != nil || !auditMarked.PositionUncertain ||
		auditMarked.RuntimeState != "position_uncertain" ||
		!strings.HasPrefix(auditMarked.ErrorMessage, arbitragePositionAuditErrorPrefix) {
		t.Fatalf("audit marked=%+v err=%v", auditMarked, err)
	}
	auditMarked, err = repository.UpdateArbitragePositionAudit(
		ctx, created.ID, auditMarked.Version, "0.02", "0", "0.02", "0", true,
		arbitragePositionAuditErrorPrefix+" leg_a=0.02 leg_b=0",
	)
	if err != nil || !strings.HasPrefix(
		auditMarked.ErrorMessage, arbitragePositionAuditErrorPrefix,
	) {
		t.Fatalf("repeated audit marked=%+v err=%v", auditMarked, err)
	}
	auditCleared, err := repository.UpdateArbitragePositionAudit(
		ctx, created.ID, auditMarked.Version, "0", "0", "0", "0", false, "",
	)
	if err != nil || auditCleared.PositionUncertain ||
		auditCleared.RuntimeState != "monitoring" || auditCleared.ErrorMessage != "" {
		t.Fatalf("audit cleared=%+v err=%v", auditCleared, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET runtime_state='hedge_deferred_dust'
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	dustSnapshot, err := repository.UpdateArbitrageMarketSnapshot(
		ctx, created.ID, "10", "-10", false,
	)
	if err != nil || dustSnapshot.RuntimeState != "hedge_deferred_dust" {
		t.Fatalf("dust snapshot=%+v err=%v", dustSnapshot, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET runtime_state='monitoring'
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	const orderUncertainMessage = "order state remained uncertain after bounded reconciliation"
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET position_uncertain=TRUE,runtime_state='position_uncertain',error_message=$2
		WHERE id=$1::uuid`, created.ID, orderUncertainMessage); err != nil {
		t.Fatal(err)
	}
	orderUncertain, err := repository.UpdateArbitragePositionAudit(
		ctx, created.ID, arbitrageVersion(t, ctx, pool, created.ID),
		"0", "0", "0", "0", false, "",
	)
	if err != nil || !orderUncertain.PositionUncertain ||
		orderUncertain.ErrorMessage != orderUncertainMessage {
		t.Fatalf("order uncertain=%+v err=%v", orderUncertain, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET position_uncertain=FALSE,runtime_state='monitoring',error_message=''
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	infinityAccountA, infinityInstrumentA := insertArbitrageFixture(
		t, ctx, pool, "bybit", "ETHUSDT",
	)
	infinityAccountB, infinityInstrumentB := insertArbitrageFixture(
		t, ctx, pool, "bitget", "ETHUSDT",
	)
	infinityCombo := integrationArbitrageCombination(
		infinityAccountA, infinityInstrumentA,
		infinityAccountB, infinityInstrumentB,
	)
	infinityCombo.IdempotencyKey = "arb-combination-infinity-retry"
	infinityCombo.RequestFingerprint = "arb-fingerprint-infinity"
	createdInfinity, inserted, err := repository.CreateArbitrageCombination(ctx, infinityCombo)
	if err != nil || !inserted {
		t.Fatalf("infinity combo inserted=%v err=%v", inserted, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET next_retry_at='infinity'::timestamptz
		WHERE id=$1::uuid`, createdInfinity.ID); err != nil {
		t.Fatal(err)
	}
	gotInfinity, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", createdInfinity.ID)
	if err != nil || !gotInfinity.NextRetryAt.Equal(time.Unix(0, 0).UTC()) {
		t.Fatalf("infinity next_retry_at get=%+v err=%v", gotInfinity, err)
	}
	listedInfinity, _, err := repository.ListArbitrageCombinations(ctx, "admin", "running", 50, "")
	if err != nil {
		t.Fatalf("infinity list err=%v", err)
	}
	foundInfinity := false
	for _, item := range listedInfinity {
		if item.ID == createdInfinity.ID {
			foundInfinity = true
			if !item.NextRetryAt.Equal(time.Unix(0, 0).UTC()) {
				t.Fatalf("infinity list next_retry_at=%v", item.NextRetryAt)
			}
		}
	}
	if !foundInfinity {
		t.Fatal("infinity combo missing from list")
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET scheduler_lease_until=now() + interval '1 hour'
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	leasedInfinity, err := repository.LeaseArbitrageCombinations(ctx, 10, 10*time.Second)
	if err != nil || len(leasedInfinity) != 1 || leasedInfinity[0].ID != createdInfinity.ID ||
		!leasedInfinity[0].NextRetryAt.Equal(time.Unix(0, 0).UTC()) {
		t.Fatalf("infinity lease=%+v err=%v", leasedInfinity, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET scheduler_lease_until='-infinity'
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	createdInfinity.Status = "closed"
	if _, err := repository.UpdateArbitrageCombinationRuntime(ctx, createdInfinity); err != nil {
		t.Fatal(err)
	}
	failedRuntime, err := repository.RecordArbitrageFailure(
		ctx, created.ID, "venue rejected order: 51008",
	)
	if err != nil || failedRuntime.RuntimeState != "backoff" ||
		failedRuntime.ConsecutiveFailures != 1 {
		t.Fatalf("failed runtime=%+v err=%v", failedRuntime, err)
	}
	created, err = repository.ClearArbitrageFailureIfUnchanged(
		ctx, failedRuntime.ID, failedRuntime.ErrorMessage,
	)
	if err != nil || created.ConsecutiveFailures != 0 || !created.NextRetryAt.Equal(time.Unix(0, 0).UTC()) {
		t.Fatalf("cleared runtime=%+v err=%v", created, err)
	}
	signedAccountA, signedInstrumentA := insertArbitrageFixture(
		t, ctx, pool, "gate", "SOL_USDT",
	)
	signedAccountB, signedInstrumentB := insertArbitrageFixture(
		t, ctx, pool, "binance", "SOLUSDT",
	)
	signed := integrationArbitrageCombination(
		signedAccountA, signedInstrumentA, signedAccountB, signedInstrumentB,
	)
	signed.IdempotencyKey = "arb-combination-signed-thresholds"
	signed.RequestFingerprint = "arb-fingerprint-signed"
	signed.AskThresholdBps = "-70"
	signed.BidThresholdBps = "8"
	createdSigned, inserted, err := repository.CreateArbitrageCombination(ctx, signed)
	if err != nil || !inserted {
		t.Fatalf("signed thresholds inserted=%v err=%v", inserted, err)
	}
	if createdSigned.AskThresholdBps != "-70" || createdSigned.BidThresholdBps != "8" {
		t.Fatalf("persisted thresholds ask=%s bid=%s", createdSigned.AskThresholdBps, createdSigned.BidThresholdBps)
	}
	createdSigned.Status = "closed"
	if _, err := repository.UpdateArbitrageCombinationRuntime(ctx, createdSigned); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.UpdateArbitrageCombinationConfig(
		ctx, "admin", createdSigned.ID, UpdateArbitrageInput{AskThresholdBps: &askThreshold},
	); !errors.Is(err, ErrArbitrageConflict) {
		t.Fatalf("closed config update err=%v", err)
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
	if err != nil || claimed {
		t.Fatalf("parallel bid claim=%v err=%v", claimed, err)
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
	listedOrders, err := repository.ListArbitrageOrders(ctx, "admin", created.ID)
	if err != nil || len(listedOrders) != 1 || listedOrders[0].ID != order.ID {
		t.Fatalf("listed arbitrage orders=%+v err=%v", listedOrders, err)
	}
	otherOwnerOrders, err := repository.ListArbitrageOrders(ctx, "other-owner", created.ID)
	if err != nil || len(otherOwnerOrders) != 0 {
		t.Fatalf("other owner arbitrage orders=%+v err=%v", otherOwnerOrders, err)
	}
	otherCombinationOrders, err := repository.ListArbitrageOrders(
		ctx, "admin", createdInfinity.ID,
	)
	if err != nil || len(otherCombinationOrders) != 0 {
		t.Fatalf(
			"other combination arbitrage orders=%+v err=%v",
			otherCombinationOrders, err,
		)
	}
	recentOrders, err := repository.ListRecentSubmittedArbitrageOrders(
		ctx, "admin", created.ID, 10,
	)
	if err != nil || len(recentOrders) != 0 {
		t.Fatalf("intent-only recent orders=%+v err=%v", recentOrders, err)
	}
	submittedIDs := make([]string, 0, 11)
	for index := 0; index < 11; index++ {
		submitted, _, createErr := repository.CreateIntent(ctx, Order{
			IdempotencyKey:   fmt.Sprintf("arb-recent-submitted-%02d", index),
			OwnerUsername:    "admin",
			TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
			InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
			Quantity: "0.01", RequestFingerprint: fmt.Sprintf("arb-recent-fingerprint-%02d", index),
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "market",
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if appendErr := repository.AppendEvent(
			ctx, submitted.ID, "submitted", map[string]any{"exchange": "binance"},
		); appendErr != nil {
			t.Fatal(appendErr)
		}
		submitted, createErr = repository.UpdateResult(ctx, submitted.ID, VenueResult{
			Status: "open",
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		submittedIDs = append(submittedIDs, submitted.ID)
		time.Sleep(time.Millisecond)
	}
	recentOrders, err = repository.ListRecentSubmittedArbitrageOrders(
		ctx, "admin", created.ID, 10,
	)
	if err != nil || len(recentOrders) != 10 {
		t.Fatalf("recent submitted orders=%+v err=%v", recentOrders, err)
	}
	if recentOrders[0].ID != submittedIDs[10] ||
		recentOrders[9].ID != submittedIDs[1] {
		t.Fatalf("recent submitted order order=%v expected=%v", recentOrders, submittedIDs)
	}
	otherOwnerRecent, err := repository.ListRecentSubmittedArbitrageOrders(
		ctx, "other-owner", created.ID, 10,
	)
	if err != nil || len(otherOwnerRecent) != 0 {
		t.Fatalf("other owner recent submitted orders=%+v err=%v", otherOwnerRecent, err)
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
	pairOrders, createdFlags, err := repository.CreateArbitrageIntents(ctx, pairInput)
	if err != nil || len(createdFlags) != 2 || !createdFlags[0] || !createdFlags[1] {
		t.Fatalf("atomic pair flags=%v err=%v", createdFlags, err)
	}
	_, createdFlags, err = repository.CreateArbitrageIntents(ctx, pairInput)
	if err != nil || createdFlags[0] || createdFlags[1] {
		t.Fatalf("recovered pair flags=%v err=%v", createdFlags, err)
	}
	if _, err := repository.UpdateResult(ctx, pairOrders[0].ID, VenueResult{
		Status: "filled", FilledQuantity: "0.01", AveragePrice: "100",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.UpdateResult(ctx, pairOrders[1].ID, VenueResult{
		Status: "filled", FilledQuantity: "0.009", AveragePrice: "101",
	}); err != nil {
		t.Fatal(err)
	}
	positioned, err := repository.RecomputeArbitrageBasePositions(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if positioned.LegABasePosition != "0.01" ||
		positioned.LegBBasePosition != "-0.009" ||
		positioned.CarryBaseQuantity != "0.001" {
		t.Fatalf("base positions=%+v", positioned)
	}
	positionedAgain, err := repository.RecomputeArbitrageBasePositions(ctx, created.ID)
	if err != nil || positionedAgain.CarryBaseQuantity != "0.001" {
		t.Fatalf("idempotent positions=%+v err=%v", positionedAgain, err)
	}
	positioned, err = repository.UpdateArbitragePositionFromBase(
		ctx, created.ID, "100", "0.9",
	)
	if err != nil || positioned.PositionNotional != "0.9" {
		t.Fatalf("position from base=%+v err=%v", positioned, err)
	}
	unboundedFromBase, err := repository.UpdateArbitragePositionFromBase(
		ctx, created.ID, "2000000", "0",
	)
	if err != nil || unboundedFromBase.PositionNotional != "18000" {
		t.Fatalf("unbounded position from base=%+v err=%v", unboundedFromBase, err)
	}
	positioned, err = repository.UpdateArbitragePositionFromBase(
		ctx, created.ID, "100", "0",
	)
	if err != nil || positioned.PositionNotional != "0.9" {
		t.Fatalf("restored position from base=%+v err=%v", positioned, err)
	}
	execution.Status = "completed"
	if _, err := repository.UpdateArbitrageExecution(ctx, execution); err != nil {
		t.Fatal(err)
	}
	metrics := persistMetrics(
		t, ctx, repository, created.ID, "100", "101", time.Now().UTC(),
	).Combination
	if metrics.GrossTurnoverNotional != "1.909" ||
		metrics.LegAAverageEntryPrice != "100" ||
		metrics.LegBAverageEntryPrice != "101" ||
		metrics.AverageEntrySpreadBps != "100" {
		t.Fatalf("position metrics=%+v", metrics)
	}
	metricsAgain := persistMetrics(
		t, ctx, repository, created.ID, "100", "101", metrics.ExposureUpdatedAt,
	).Combination
	if metricsAgain.GrossTurnoverNotional != metrics.GrossTurnoverNotional {
		t.Fatalf("idempotent metrics=%+v", metricsAgain)
	}
	filledAt := time.Now().UTC().Add(-time.Hour)
	for index, fill := range []struct {
		orderID, venueOrderID, tradeID, quantity, price string
		accountID                                       int64
		exchange                                        string
	}{
		{pairOrders[0].ID, "venue-a", "trade-a", "0.01", "100", accountA, "binance"},
		{pairOrders[1].ID, "venue-b", "trade-b", "0.009", "101", accountB, "okx"},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO trader_order_fills (
				order_id,trading_account_id,exchange,venue_order_id,trade_id,
				quantity,price,executed_at
			) VALUES ($1::uuid,$2,$3,$4,$5,$6::numeric,$7::numeric,$8)`,
			fill.orderID, fill.accountID, fill.exchange, fill.venueOrderID,
			fill.tradeID, fill.quantity, fill.price, filledAt.Add(time.Duration(index)*time.Second),
		); err != nil {
			t.Fatal(err)
		}
	}
	fundingTime := time.Now().UTC()
	for _, funding := range []struct {
		instrumentID int64
		rate         float64
	}{
		{instrumentA, 0.001},
		{instrumentB, -0.002},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO funding_rates (
				instrument_id,funding_rate,funding_time,record_kind,interval_hours
			) VALUES ($1,$2,$3,'settled',4)
			ON CONFLICT (instrument_id,funding_time) WHERE record_kind='settled'
			DO UPDATE SET funding_rate=EXCLUDED.funding_rate`,
			funding.instrumentID, funding.rate, fundingTime,
		); err != nil {
			t.Fatal(err)
		}
	}
	fundedMetrics := persistMetrics(
		t, ctx, repository, created.ID, "100", "101", time.Now().UTC(),
	).Combination
	if !fundedMetrics.FundingHistoryComplete ||
		fundedMetrics.EstimatedFundingPnl != "-0.002818" {
		t.Fatalf("funded metrics=%+v", fundedMetrics)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_orders SET status='canceled',reconcile_failures=0
		WHERE arbitrage_execution_id IN (
			SELECT id FROM trader_arbitrage_executions WHERE combination_id=$1::uuid
		)
		  AND status IN ('pending','open','partially_filled','unknown')`,
		created.ID,
	); err != nil {
		t.Fatal(err)
	}
	active, claimedActive, err := repository.ClaimArbitrageExecution(
		ctx, integrationArbitrageExecution(created.ID, "ask"),
	)
	if err != nil || !claimedActive {
		t.Fatalf("reclaim active execution claimed=%v err=%v", claimedActive, err)
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
	if _, err := pool.Exec(ctx, `
		UPDATE trader_orders SET status='canceled',reconcile_failures=0
		WHERE arbitrage_execution_id=$1::uuid
		  AND status IN ('pending','open','partially_filled','unknown')`,
		execution.ID,
	); err != nil {
		t.Fatal(err)
	}
	bid, claimed, err := repository.ClaimArbitrageExecution(ctx, integrationArbitrageExecution(created.ID, "bid"))
	if err != nil || !claimed {
		t.Fatalf("serial bid claim=%v err=%v", claimed, err)
	}
	bid.Status = "completed"
	if _, err := repository.UpdateArbitrageExecution(ctx, bid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			position_uncertain=TRUE,runtime_state='position_uncertain',
			error_message=$2,leg_a_position_difference=0,leg_b_position_difference=0
		WHERE id=$1::uuid`, created.ID, arbitrageOrderReconcileError); err != nil {
		t.Fatal(err)
	}
	clearedOrderUncertain, err := repository.UpdateArbitragePositionAudit(
		ctx, created.ID, arbitrageVersion(t, ctx, pool, created.ID),
		"0.01", "-0.009", "0", "0", false, "",
	)
	if err != nil || clearedOrderUncertain.PositionUncertain ||
		clearedOrderUncertain.RuntimeState != "monitoring" ||
		clearedOrderUncertain.ErrorMessage != "" {
		t.Fatalf("safe order uncertain clear=%+v err=%v", clearedOrderUncertain, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			position_uncertain=TRUE,runtime_state='position_uncertain',error_message=$2
		WHERE id=$1::uuid`,
		created.ID, arbitrageOrderReconcileError,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_orders SET reconcile_failures=1,
			created_at=now()-interval '1 hour',next_reconcile_at=now(),
			reconcile_lease_until=NULL
		WHERE id=$1::uuid`, pairOrders[0].ID,
	); err != nil {
		t.Fatal(err)
	}
	notCleared, err := repository.UpdateArbitragePositionAudit(
		ctx, created.ID, arbitrageVersion(t, ctx, pool, created.ID),
		"0.01", "-0.009", "0", "0", false, "",
	)
	var deferred *ArbitragePositionAuditDeferredError
	if !errors.As(err, &deferred) ||
		deferred.Reason != ArbitragePositionAuditDeferredReconcileFailure {
		t.Fatalf("unsafe order uncertain defer=%+v err=%v", deferred, err)
	}
	notCleared, err = repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
	if err != nil || !notCleared.PositionUncertain ||
		notCleared.ErrorMessage != arbitrageOrderReconcileError {
		t.Fatalf("unsafe order uncertain clear=%+v err=%v", notCleared, err)
	}
	rescanned, err := repository.LeaseDueOrders(ctx, 100, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	foundRescan := false
	for _, order := range rescanned {
		if order.ID == pairOrders[0].ID {
			foundRescan = true
			break
		}
	}
	if !foundRescan {
		t.Fatalf("old reconcile failure was not rescanned: %+v", rescanned)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_orders SET reconcile_failures=0,
			reconcile_lease_until=NULL,next_reconcile_at='infinity'
		WHERE id=$1::uuid`, pairOrders[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			position_uncertain=FALSE,runtime_state='monitoring',error_message=''
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	var completionWG sync.WaitGroup
	completionErrors := make(chan error, 16)
	for index := 0; index < 16; index++ {
		completionWG.Add(1)
		go func() {
			defer completionWG.Done()
			if _, err := repository.AddArbitragePositionDelta(ctx, created.ID, "10"); err != nil {
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
	if refreshed.PositionNotional != "160.9" || refreshed.CumulativeTurnoverNotional != "160.9" ||
		refreshed.Status != "running" {
		t.Fatalf("atomic position=%+v", refreshed)
	}
	closedByTarget, err := repository.AddArbitragePositionDelta(ctx, created.ID, "20000")
	if err != nil || closedByTarget.Status != "running" || closedByTarget.PositionNotional != "20160.9" {
		t.Fatalf("unbounded position=%+v err=%v", closedByTarget, err)
	}
	reversed, err := repository.AddArbitragePositionDelta(ctx, created.ID, "-4000")
	if err != nil || reversed.PositionNotional != "16160.9" || reversed.CumulativeTurnoverNotional != "24160.9" ||
		reversed.Status != "running" {
		t.Fatalf("reversed position=%+v err=%v", reversed, err)
	}
	created = refreshed
	created.Status = "closed"
	if _, err := repository.UpdateArbitrageCombinationRuntime(ctx, created); err != nil {
		t.Fatal(err)
	}
	deleted, err := repository.DeleteExpiredArbitrageCombinations(ctx, time.Now().Add(time.Hour), 10)
	if err != nil || deleted != 6 {
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

func TestArbitragePositionAuditSafetyIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("trader_audit_safety_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	}()
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
	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "AUDITAUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "AUDITB-USDT-SWAP")
	repository := NewRepository(pool)
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.IdempotencyKey = "arb-audit-safety"
	input.RequestFingerprint = "arb-audit-safety-fingerprint"
	created, inserted, err := repository.CreateArbitrageCombination(ctx, input)
	if err != nil || !inserted || created.Version <= 0 {
		t.Fatalf("create audit combo=%+v inserted=%v err=%v", created, inserted, err)
	}

	reset := func(t *testing.T) ArbitrageCombination {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			DELETE FROM trader_orders
			WHERE arbitrage_execution_id IN (
				SELECT id FROM trader_arbitrage_executions WHERE combination_id=$1::uuid
			)`, created.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			DELETE FROM trader_arbitrage_executions WHERE combination_id=$1::uuid`,
			created.ID,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations SET
				status='running',runtime_state='monitoring',position_uncertain=FALSE,
				error_message='',circuit_open=FALSE,
				leg_a_venue_base_position=11,leg_b_venue_base_position=-11,
				leg_a_position_difference=1,leg_b_position_difference=-1,
				last_position_reconciled_at='2026-01-01T00:00:00Z',
				closed_at=NULL,version=version+1
			WHERE id=$1::uuid`,
			created.ID,
		); err != nil {
			t.Fatal(err)
		}
		item, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}

	t.Run("sticky empty error clears only at exact zero", func(t *testing.T) {
		item := reset(t)
		if _, err := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations SET
				position_uncertain=TRUE,runtime_state='monitoring',error_message='',
				version=version+1
			WHERE id=$1::uuid`, item.ID); err != nil {
			t.Fatal(err)
		}
		item, _ = repository.GetArbitrageCombinationByOwner(ctx, "admin", item.ID)
		notCleared, err := repository.UpdateArbitragePositionAudit(
			ctx, item.ID, item.Version, "10", "-10", "0.0001", "0", false, "",
		)
		if err != nil || !notCleared.PositionUncertain {
			t.Fatalf("nonzero sticky state cleared=%+v err=%v", notCleared, err)
		}
		cleared, err := repository.UpdateArbitragePositionAudit(
			ctx, item.ID, notCleared.Version, "10", "-10", "0", "0", false, "",
		)
		if err != nil || cleared.PositionUncertain ||
			cleared.RuntimeState != "monitoring" || cleared.ErrorMessage != "" {
			t.Fatalf("sticky state not cleared=%+v err=%v", cleared, err)
		}
	})

	t.Run("audit owned risk clears below material threshold", func(t *testing.T) {
		item := reset(t)
		if _, err := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations SET
				position_uncertain=TRUE,runtime_state='position_uncertain',
				error_message=$2,version=version+1
			WHERE id=$1::uuid`,
			item.ID, arbitragePositionAuditErrorPrefix+" old material drift",
		); err != nil {
			t.Fatal(err)
		}
		item, _ = repository.GetArbitrageCombinationByOwner(ctx, "admin", item.ID)
		cleared, err := repository.UpdateArbitragePositionAudit(
			ctx, item.ID, item.Version, "10.0001", "-10",
			"0.0001", "0", false, "",
		)
		if err != nil || cleared.PositionUncertain ||
			cleared.RuntimeState != "monitoring" || cleared.ErrorMessage != "" {
			t.Fatalf("sub-threshold risk not cleared=%+v err=%v", cleared, err)
		}
	})

	t.Run("runtime preserves risk while lifecycle advances", func(t *testing.T) {
		for _, risk := range []string{
			arbitragePositionAuditErrorPrefix + " prior drift",
			"arbitrage hedge would reverse position",
		} {
			item := reset(t)
			if _, err := pool.Exec(ctx, `
				UPDATE trader_arbitrage_combinations SET
					position_uncertain=TRUE,runtime_state='position_uncertain',
					error_message=$2,version=version+1
				WHERE id=$1::uuid`, item.ID, risk); err != nil {
				t.Fatal(err)
			}
			item, _ = repository.GetArbitrageCombinationByOwner(ctx, "admin", item.ID)
			item.Status = "closed"
			item.RuntimeState = "monitoring"
			item.ErrorMessage = ""
			item.PositionUncertain = false
			item.CurrentAskSpreadBps = "8"
			item.CurrentBidSpreadBps = "-9"
			updated, err := repository.UpdateArbitrageCombinationRuntime(ctx, item)
			if err != nil || updated.Status != "closed" || updated.ClosedAt.IsZero() ||
				!updated.PositionUncertain || updated.RuntimeState != "position_uncertain" ||
				updated.ErrorMessage != risk || updated.CurrentAskSpreadBps != "8" {
				t.Fatalf("runtime risk/lifecycle=%+v err=%v", updated, err)
			}
		}
	})

	t.Run("unsafe and order risks are not audit cleared", func(t *testing.T) {
		for _, risk := range []string{
			"arbitrage hedge would reverse position",
			arbitrageOrderReconcileError,
		} {
			item := reset(t)
			if _, err := pool.Exec(ctx, `
				UPDATE trader_arbitrage_combinations SET
					position_uncertain=TRUE,runtime_state='position_uncertain',
					error_message=$2,version=version+1
				WHERE id=$1::uuid`, item.ID, risk); err != nil {
				t.Fatal(err)
			}
			item, _ = repository.GetArbitrageCombinationByOwner(ctx, "admin", item.ID)
			kept, err := repository.UpdateArbitragePositionAudit(
				ctx, item.ID, item.Version, "10", "-10", "0", "0", false, "",
			)
			if err != nil || !kept.PositionUncertain || kept.ErrorMessage != risk {
				t.Fatalf("risk %q was cleared: item=%+v err=%v", risk, kept, err)
			}
		}
	})

	t.Run("list and write gates active work", func(t *testing.T) {
		item := reset(t)
		active, claimed, err := repository.ClaimArbitrageExecution(
			ctx, integrationArbitrageExecution(item.ID, "ask"),
		)
		if err != nil || !claimed {
			t.Fatalf("claim active=%+v claimed=%v err=%v", active, claimed, err)
		}
		listed, err := repository.ListArbitrageCombinationsForPositionAudit(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range listed {
			if candidate.ID == item.ID {
				t.Fatal("active execution was listed for audit")
			}
		}
		assertAuditDeferredReason(
			t,
			func() error {
				_, err := repository.UpdateArbitragePositionAudit(
					ctx, item.ID, arbitrageVersion(t, ctx, pool, item.ID),
					"11", "-10", "1", "0", true,
					arbitragePositionAuditErrorPrefix+" transient",
				)
				return err
			},
			ArbitragePositionAuditDeferredActiveExecution,
		)
	})

	t.Run("active order and reconcile failure have distinct reasons", func(t *testing.T) {
		item := reset(t)
		execution, claimed, err := repository.ClaimArbitrageExecution(
			ctx, integrationArbitrageExecution(item.ID, "ask"),
		)
		if err != nil || !claimed {
			t.Fatalf("claim=%v err=%v", claimed, err)
		}
		execution.Status = "completed"
		if _, err := repository.UpdateArbitrageExecution(ctx, execution); err != nil {
			t.Fatal(err)
		}
		order, _, err := repository.CreateIntent(ctx, Order{
			IdempotencyKey: "audit-active-order", OwnerUsername: "admin",
			TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
			InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: "AUDITAUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
			Quantity: "1", RequestFingerprint: "audit-active-order-fingerprint",
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "market",
		})
		if err != nil {
			t.Fatal(err)
		}
		item, _ = repository.GetArbitrageCombinationByOwner(ctx, "admin", item.ID)
		listed, err := repository.ListArbitrageCombinationsForPositionAudit(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range listed {
			if candidate.ID == item.ID {
				t.Fatal("active order was listed for audit")
			}
		}
		assertAuditDeferredReason(t, func() error {
			_, err := repository.UpdateArbitragePositionAudit(
				ctx, item.ID, item.Version, "10", "-10", "0", "0", false, "",
			)
			return err
		}, ArbitragePositionAuditDeferredActiveOrder)
		if _, err := pool.Exec(ctx, `
			UPDATE trader_orders SET status='filled',reconcile_failures=1
			WHERE id=$1::uuid`, order.ID); err != nil {
			t.Fatal(err)
		}
		item, _ = repository.GetArbitrageCombinationByOwner(ctx, "admin", item.ID)
		listed, err = repository.ListArbitrageCombinationsForPositionAudit(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range listed {
			if candidate.ID == item.ID {
				t.Fatal("reconcile failure was listed for audit")
			}
		}
		assertAuditDeferredReason(t, func() error {
			_, err := repository.UpdateArbitragePositionAudit(
				ctx, item.ID, item.Version, "10", "-10", "0", "0", false, "",
			)
			return err
		}, ArbitragePositionAuditDeferredReconcileFailure)
	})

	t.Run("version conflict changes no audit fields", func(t *testing.T) {
		item := reset(t)
		before := item.LastPositionReconciledAt
		if _, err := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations
			SET current_ask_spread_bps=42,version=version+1
			WHERE id=$1::uuid`, item.ID); err != nil {
			t.Fatal(err)
		}
		assertAuditDeferredReason(t, func() error {
			_, err := repository.UpdateArbitragePositionAudit(
				ctx, item.ID, item.Version, "99", "-99", "88", "-88", true,
				arbitragePositionAuditErrorPrefix+" stale write",
			)
			return err
		}, ArbitragePositionAuditDeferredVersionConflict)
		after, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.LegAVenueBasePosition != item.LegAVenueBasePosition ||
			after.LegBVenueBasePosition != item.LegBVenueBasePosition ||
			after.LegAPositionDifference != item.LegAPositionDifference ||
			after.LegBPositionDifference != item.LegBPositionDifference ||
			!after.LastPositionReconciledAt.Equal(before) ||
			after.PositionUncertain || after.ErrorMessage != "" {
			t.Fatalf("version conflict changed audit state: before=%+v after=%+v", item, after)
		}
	})

	t.Run("claim rechecks after audit row lock", func(t *testing.T) {
		item := reset(t)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `
			SELECT 1 FROM trader_arbitrage_combinations
			WHERE id=$1::uuid FOR UPDATE`, item.ID); err != nil {
			t.Fatal(err)
		}
		type claimResult struct {
			claimed bool
			err     error
		}
		result := make(chan claimResult, 1)
		go func() {
			_, claimed, claimErr := repository.ClaimArbitrageExecution(
				context.Background(), integrationArbitrageExecution(item.ID, "ask"),
			)
			result <- claimResult{claimed: claimed, err: claimErr}
		}()
		select {
		case got := <-result:
			t.Fatalf("claim did not wait for audit lock: %+v", got)
		case <-time.After(100 * time.Millisecond):
		}
		if _, err := tx.Exec(ctx, `
			UPDATE trader_arbitrage_combinations SET
				position_uncertain=TRUE,runtime_state='position_uncertain',
				error_message=$2,version=version+1
			WHERE id=$1::uuid`,
			item.ID, arbitragePositionAuditErrorPrefix+" locked drift",
		); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-result:
			if got.err != nil || got.claimed {
				t.Fatalf("claim crossed audit risk: %+v", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("claim remained blocked after audit commit")
		}
	})

	t.Run("audit CAS rejects claim committed while waiting", func(t *testing.T) {
		item := reset(t)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `
			SELECT 1 FROM trader_arbitrage_combinations
			WHERE id=$1::uuid FOR UPDATE`, item.ID); err != nil {
			t.Fatal(err)
		}
		executionID := uuid.NewString()
		if _, err := tx.Exec(ctx, `
			INSERT INTO trader_arbitrage_executions (
				id,combination_id,direction,sequence,status,
				trigger_ask_spread_bps,trigger_bid_spread_bps,
				trigger_leg_a_bid,trigger_leg_a_ask,trigger_leg_b_bid,trigger_leg_b_ask,
				target_base_quantity
			) VALUES ($1::uuid,$2::uuid,'ask',1,'claimed',12,-8,100,101,99,102,1)`,
			executionID, item.ID,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE trader_arbitrage_combinations
			SET version=version+1,updated_at=now()
			WHERE id=$1::uuid`,
			item.ID,
		); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, auditErr := repository.UpdateArbitragePositionAudit(
				context.Background(), item.ID, item.Version,
				"99", "-99", "88", "-88", true,
				arbitragePositionAuditErrorPrefix+" stale concurrent snapshot",
			)
			result <- auditErr
		}()
		select {
		case got := <-result:
			t.Fatalf("audit did not wait for claim lock: %v", got)
		case <-time.After(100 * time.Millisecond):
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case auditErr := <-result:
			var deferred *ArbitragePositionAuditDeferredError
			if !errors.As(auditErr, &deferred) ||
				deferred.Reason != ArbitragePositionAuditDeferredVersionConflict {
				t.Fatalf("audit defer=%v", auditErr)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("audit remained blocked after claim commit")
		}
		after, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.PositionUncertain ||
			after.LegAVenueBasePosition != item.LegAVenueBasePosition ||
			!after.LastPositionReconciledAt.Equal(item.LastPositionReconciledAt) {
			t.Fatalf("stale audit wrote after claim: before=%+v after=%+v", item, after)
		}
	})

	t.Run("closing and closed audit owned recovery", func(t *testing.T) {
		for _, status := range []string{"closing", "closed"} {
			item := reset(t)
			if _, err := pool.Exec(ctx, `
				UPDATE trader_arbitrage_combinations SET
					status=$2,runtime_state='position_uncertain',position_uncertain=TRUE,
					error_message=$3,closed_at=CASE WHEN $2='closed' THEN now() ELSE NULL END,
					version=version+1
				WHERE id=$1::uuid`,
				item.ID, status, arbitragePositionAuditErrorPrefix+" prior drift",
			); err != nil {
				t.Fatal(err)
			}
			item, _ = repository.GetArbitrageCombinationByOwner(ctx, "admin", item.ID)
			if status == "closed" {
				listed, err := repository.ListArbitrageCombinationsForPositionAudit(ctx, 100)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, candidate := range listed {
					found = found || candidate.ID == item.ID
				}
				if !found {
					t.Fatal("closed audit-owned risk was not listed for final recovery")
				}
			}
			cleared, err := repository.UpdateArbitragePositionAudit(
				ctx, item.ID, item.Version, "10", "-10", "0.0001", "0", false, "",
			)
			wantRuntime := "closing"
			if status == "closed" {
				wantRuntime = "monitoring"
			}
			if err != nil || cleared.PositionUncertain ||
				cleared.RuntimeState != wantRuntime || cleared.ErrorMessage != "" {
				t.Fatalf("%s recovery=%+v err=%v", status, cleared, err)
			}
		}
		for _, risk := range []struct {
			runtime string
			message string
		}{
			{runtime: "position_uncertain", message: "arbitrage hedge would reverse position"},
			{runtime: "position_uncertain", message: arbitrageOrderReconcileError},
			{runtime: "monitoring", message: ""},
		} {
			item := reset(t)
			if _, err := pool.Exec(ctx, `
				UPDATE trader_arbitrage_combinations SET
					status='closed',runtime_state=$2,position_uncertain=TRUE,
					error_message=$3,closed_at=now(),version=version+1
				WHERE id=$1::uuid`,
				item.ID, risk.runtime, risk.message,
			); err != nil {
				t.Fatal(err)
			}
			listed, err := repository.ListArbitrageCombinationsForPositionAudit(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, candidate := range listed {
				if candidate.ID == item.ID {
					t.Fatalf("closed non-audit risk was listed: %+v", risk)
				}
			}
		}
	})

	t.Run("claim rejects live orders after completed execution", func(t *testing.T) {
		item := reset(t)
		execution, claimed, err := repository.ClaimArbitrageExecution(
			ctx, integrationArbitrageExecution(item.ID, "ask"),
		)
		if err != nil || !claimed {
			t.Fatalf("claim=%v err=%v", claimed, err)
		}
		execution.Status = "completed"
		if _, err := repository.UpdateArbitrageExecution(ctx, execution); err != nil {
			t.Fatal(err)
		}
		if _, _, err := repository.CreateIntent(ctx, Order{
			IdempotencyKey: "live-order-blocks-claim", OwnerUsername: "admin",
			TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "hyperliquid",
			InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: "AUDITAUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "limit",
			Quantity: "1", RequestFingerprint: "live-order-blocks-claim-fp",
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "maker",
		}); err != nil {
			t.Fatal(err)
		}
		_, claimed, err = repository.ClaimArbitrageExecution(
			ctx, integrationArbitrageExecution(item.ID, "bid"),
		)
		if err != nil || claimed {
			t.Fatalf("live order should block claim, claimed=%v err=%v", claimed, err)
		}
	})
}

func TestArbitragePositionAuditManualInterventionRecoverIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("trader_audit_manual_recover_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	}()
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
	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "MANUALAUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "MANUALB-USDT-SWAP")
	repository := NewRepository(pool)
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.IdempotencyKey = "arb-audit-manual-recover"
	input.RequestFingerprint = "arb-audit-manual-recover-fingerprint"
	created, inserted, err := repository.CreateArbitrageCombination(ctx, input)
	if err != nil || !inserted || created.Version <= 0 {
		t.Fatalf("create audit combo=%+v inserted=%v err=%v", created, inserted, err)
	}

	reset := func(t *testing.T) ArbitrageCombination {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			DELETE FROM trader_orders
			WHERE arbitrage_execution_id IN (
				SELECT id FROM trader_arbitrage_executions WHERE combination_id=$1::uuid
			)`, created.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			DELETE FROM trader_arbitrage_executions WHERE combination_id=$1::uuid`,
			created.ID,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations SET
				status='running',runtime_state='monitoring',position_uncertain=FALSE,
				error_message='',circuit_open=FALSE,
				leg_a_base_position=0,leg_b_base_position=0,
				consecutive_failures=0,repeated_failure_count=0,last_failure_key='',
				market_data_stale=FALSE,
				leg_a_venue_base_position=0,leg_b_venue_base_position=0,
				leg_a_position_difference=0,leg_b_position_difference=0,
				last_position_reconciled_at='2026-01-01T00:00:00Z',
				closed_at=NULL,version=version+1
			WHERE id=$1::uuid`,
			created.ID,
		); err != nil {
			t.Fatal(err)
		}
		item, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}

	completeAsk := func(t *testing.T, comboID string) ArbitrageExecution {
		t.Helper()
		execution, claimed, err := repository.ClaimArbitrageExecution(
			ctx, integrationArbitrageExecution(comboID, "ask"),
		)
		if err != nil || !claimed {
			t.Fatalf("claim=%v err=%v", claimed, err)
		}
		execution.Status = "completed"
		updated, err := repository.UpdateArbitrageExecution(ctx, execution)
		if err != nil {
			t.Fatal(err)
		}
		return updated
	}

	createOrder := func(t *testing.T, executionID, status string, reconcileFailures int) {
		t.Helper()
		order, _, err := repository.CreateIntent(ctx, Order{
			IdempotencyKey: uuid.NewString(), OwnerUsername: "admin",
			TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
			InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: "MANUALAUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
			Quantity: "1", RequestFingerprint: uuid.NewString(),
			ArbitrageExecutionID: executionID, ArbitrageLeg: "a", ArbitrageRole: "market",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE trader_orders
			SET status=$2,reconcile_failures=$3
			WHERE id=$1::uuid`,
			order.ID, status, reconcileFailures,
		); err != nil {
			t.Fatal(err)
		}
	}

	markManual := func(t *testing.T, extra string) ArbitrageCombination {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations SET
				runtime_state='manual_intervention',position_uncertain=FALSE,
				error_message='',circuit_open=FALSE,
				leg_a_base_position=0,leg_b_base_position=0,
				consecutive_failures=0,repeated_failure_count=0,last_failure_key='',
				version=version+1
			WHERE id=$1::uuid`, created.ID); err != nil {
			t.Fatal(err)
		}
		if extra != "" {
			if _, err := pool.Exec(ctx, `
				UPDATE trader_arbitrage_combinations SET `+extra+`,version=version+1
				WHERE id=$1::uuid`, created.ID); err != nil {
				t.Fatal(err)
			}
		}
		item, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}

	auditZero := func(t *testing.T, item ArbitrageCombination, uncertain bool) (ArbitrageCombination, error) {
		t.Helper()
		return repository.UpdateArbitragePositionAudit(
			ctx, item.ID, item.Version, "0", "0", "0", "0", uncertain, "",
		)
	}

	t.Run("soph leftover recovers to monitoring", func(t *testing.T) {
		item := reset(t)
		execution := completeAsk(t, item.ID)
		createOrder(t, execution.ID, "filled", 0)
		item = markManual(t, "")
		updated, err := auditZero(t, item, false)
		if err != nil || updated.RuntimeState != "monitoring" ||
			updated.PositionUncertain || updated.ErrorMessage != "" ||
			updated.CircuitOpen {
			t.Fatalf("recovered=%+v err=%v", updated, err)
		}
	})

	t.Run("nonzero carry stays manual", func(t *testing.T) {
		item := reset(t)
		_ = completeAsk(t, item.ID)
		item = markManual(t, "leg_a_base_position=1,leg_b_base_position=0")
		kept, err := auditZero(t, item, false)
		if err != nil || kept.RuntimeState != "manual_intervention" {
			t.Fatalf("carry kept=%+v err=%v", kept, err)
		}
	})

	t.Run("nonzero leg difference stays manual", func(t *testing.T) {
		for _, diffs := range [][2]string{{"1", "0"}, {"0", "1"}} {
			item := reset(t)
			_ = completeAsk(t, item.ID)
			item = markManual(t, "")
			kept, err := repository.UpdateArbitragePositionAudit(
				ctx, item.ID, item.Version, "1", "0", diffs[0], diffs[1], false, "",
			)
			if err != nil || kept.RuntimeState != "manual_intervention" {
				t.Fatalf("diffs=%v kept=%+v err=%v", diffs, kept, err)
			}
		}
	})

	t.Run("position uncertain does not overlay to monitoring", func(t *testing.T) {
		item := reset(t)
		_ = completeAsk(t, item.ID)
		item = markManual(t, "position_uncertain=TRUE")
		kept, err := auditZero(t, item, false)
		if err != nil || kept.RuntimeState != "manual_intervention" ||
			!kept.PositionUncertain {
			t.Fatalf("uncertain overlay=%+v err=%v", kept, err)
		}
	})

	t.Run("circuit open stays manual", func(t *testing.T) {
		item := reset(t)
		_ = completeAsk(t, item.ID)
		item = markManual(t, "circuit_open=TRUE")
		kept, err := auditZero(t, item, false)
		if err != nil || kept.RuntimeState != "manual_intervention" || !kept.CircuitOpen {
			t.Fatalf("circuit kept=%+v err=%v", kept, err)
		}
	})

	t.Run("active execution defers and stays manual", func(t *testing.T) {
		item := reset(t)
		_, claimed, err := repository.ClaimArbitrageExecution(
			ctx, integrationArbitrageExecution(item.ID, "ask"),
		)
		if err != nil || !claimed {
			t.Fatalf("claim=%v err=%v", claimed, err)
		}
		item = markManual(t, "")
		assertAuditDeferredReason(t, func() error {
			_, err := auditZero(t, item, false)
			return err
		}, ArbitragePositionAuditDeferredActiveExecution)
		after, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", item.ID)
		if err != nil || after.RuntimeState != "manual_intervention" {
			t.Fatalf("active runtime=%+v err=%v", after, err)
		}
	})

	t.Run("active orders defer and stay manual", func(t *testing.T) {
		for _, status := range []string{"pending", "open", "unknown"} {
			item := reset(t)
			execution := completeAsk(t, item.ID)
			createOrder(t, execution.ID, status, 0)
			item = markManual(t, "")
			assertAuditDeferredReason(t, func() error {
				_, err := auditZero(t, item, false)
				return err
			}, ArbitragePositionAuditDeferredActiveOrder)
			after, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", item.ID)
			if err != nil || after.RuntimeState != "manual_intervention" {
				t.Fatalf("status=%s after=%+v err=%v", status, after, err)
			}
		}
	})

	t.Run("reconcile failure defers and stays manual", func(t *testing.T) {
		item := reset(t)
		execution := completeAsk(t, item.ID)
		createOrder(t, execution.ID, "filled", 1)
		item = markManual(t, "")
		assertAuditDeferredReason(t, func() error {
			_, err := auditZero(t, item, false)
			return err
		}, ArbitragePositionAuditDeferredReconcileFailure)
		after, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", item.ID)
		if err != nil || after.RuntimeState != "manual_intervention" {
			t.Fatalf("reconcile after=%+v err=%v", after, err)
		}
	})

	t.Run("error or failure counters stay manual", func(t *testing.T) {
		cases := []string{
			"error_message='still broken'",
			"consecutive_failures=1",
			"repeated_failure_count=1",
			"last_failure_key='venue_error'",
		}
		for _, extra := range cases {
			item := reset(t)
			_ = completeAsk(t, item.ID)
			item = markManual(t, extra)
			kept, err := auditZero(t, item, false)
			if err != nil || kept.RuntimeState != "manual_intervention" {
				t.Fatalf("extra=%s kept=%+v err=%v", extra, kept, err)
			}
		}
	})

	t.Run("missing completed execution stays manual", func(t *testing.T) {
		item := reset(t)
		execution, claimed, err := repository.ClaimArbitrageExecution(
			ctx, integrationArbitrageExecution(item.ID, "ask"),
		)
		if err != nil || !claimed {
			t.Fatalf("claim=%v err=%v", claimed, err)
		}
		execution.Status = "failed"
		if _, err := repository.UpdateArbitrageExecution(ctx, execution); err != nil {
			t.Fatal(err)
		}
		item = markManual(t, "")
		kept, err := auditZero(t, item, false)
		if err != nil || kept.RuntimeState != "manual_intervention" {
			t.Fatalf("failed-only kept=%+v err=%v", kept, err)
		}
	})

	t.Run("closing never becomes monitoring", func(t *testing.T) {
		item := reset(t)
		_ = completeAsk(t, item.ID)
		item = markManual(t, "status='closing'")
		kept, err := auditZero(t, item, false)
		if err != nil || kept.Status != "closing" ||
			kept.RuntimeState == "monitoring" {
			t.Fatalf("closing kept=%+v err=%v", kept, err)
		}
	})

	t.Run("monitoring and backoff semantics unchanged", func(t *testing.T) {
		item := reset(t)
		_ = completeAsk(t, item.ID)
		item, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", item.ID)
		if err != nil {
			t.Fatal(err)
		}
		updated, err := auditZero(t, item, false)
		if err != nil || updated.RuntimeState != "monitoring" {
			t.Fatalf("monitoring=%+v err=%v", updated, err)
		}
		item = reset(t)
		_ = completeAsk(t, item.ID)
		if _, err := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations
			SET runtime_state='backoff',version=version+1
			WHERE id=$1::uuid`, item.ID); err != nil {
			t.Fatal(err)
		}
		item, _ = repository.GetArbitrageCombinationByOwner(ctx, "admin", item.ID)
		kept, err := auditZero(t, item, false)
		if err != nil || kept.RuntimeState != "backoff" {
			t.Fatalf("backoff=%+v err=%v", kept, err)
		}
	})

	t.Run("market data stale still recovers", func(t *testing.T) {
		item := reset(t)
		_ = completeAsk(t, item.ID)
		item = markManual(t, "market_data_stale=TRUE")
		updated, err := auditZero(t, item, false)
		if err != nil || updated.RuntimeState != "monitoring" ||
			!updated.MarketDataStale {
			t.Fatalf("stale recover=%+v err=%v", updated, err)
		}
	})
}

func assertAuditDeferredReason(
	t *testing.T,
	call func() error,
	want ArbitragePositionAuditDeferredReason,
) {
	t.Helper()
	var deferred *ArbitragePositionAuditDeferredError
	if err := call(); !errors.As(err, &deferred) || deferred.Reason != want {
		t.Fatalf("deferred err=%v reason=%v want=%v", err, deferred, want)
	}
}

func assertArbitrageThresholdSignChecksDropped(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var remaining int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_constraint con
		JOIN pg_class rel ON rel.oid = con.conrelid
		JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
		WHERE rel.relname = 'trader_arbitrage_combinations'
		  AND nsp.nspname = current_schema()
		  AND con.contype = 'c'
		  AND (
		      pg_get_constraintdef(con.oid) LIKE '%ask_threshold_bps >%'
		      OR pg_get_constraintdef(con.oid) LIKE '%bid_threshold_bps <%'
		  )`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("threshold sign checks remaining=%d", remaining)
	}
}

func assertArbitrageSameVenueChecksDropped(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var remaining int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_constraint con
		JOIN pg_class rel ON rel.oid = con.conrelid
		JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
		WHERE rel.relname = 'trader_arbitrage_combinations'
		  AND nsp.nspname = current_schema()
		  AND con.contype = 'c'
		  AND (
		      pg_get_constraintdef(con.oid) LIKE '%leg_a_trading_account_id <> leg_b_trading_account_id%'
		      OR pg_get_constraintdef(con.oid) LIKE '%leg_a_exchange <> leg_b_exchange%'
		  )`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("same-account/same-venue checks remaining=%d", remaining)
	}
}

func assertArbitrageMaxDeltaDropped(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var remaining int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema=current_schema()
		  AND table_name='trader_arbitrage_combinations'
		  AND column_name='max_delta_notional'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("max_delta_notional columns remaining=%d", remaining)
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
		) VALUES('admin','ARBITRAGE',$1,$1 || '-' || $2 || '-main','x'::bytea,'y'::bytea)
		RETURNING id`, venue, symbol).Scan(&accountID); err != nil {
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
		TargetNotional: "10000", OrderNotional: "500",
		ExecutionMode: "simultaneous_market", Status: "running",
		PositionNotional: "0", CumulativeTurnoverNotional: "0", MarketDataStale: true,
	}
}

func integrationArbitrageExecution(combinationID, direction string) ArbitrageExecution {
	return ArbitrageExecution{
		ID: uuid.NewString(), CombinationID: combinationID, Direction: direction, Status: "claimed",
		TriggerAskSpread: "12", TriggerBidSpread: "-8",
		TriggerLegABid: "100", TriggerLegAAsk: "101",
		TriggerLegBBid: "99", TriggerLegBAsk: "102",
		TargetBaseQuantity: "1", RequestedNotional: "102",
		PositionEffect:     "open",
		LegAFilledQuantity: "0",
		LegBFilledQuantity: "0", DeltaNotional: "0",
	}
}

func arbitrageVersion(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	combinationID string,
) int64 {
	t.Helper()
	var version int64
	if err := pool.QueryRow(
		ctx,
		`SELECT version FROM trader_arbitrage_combinations WHERE id=$1::uuid`,
		combinationID,
	).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func TestCircuitOpenExternalReconcileIntegration(t *testing.T) {
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
	schema := fmt.Sprintf("trader_circuit_open_test_%d", time.Now().UnixNano())
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

	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "XMRUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "XMR-USDT-SWAP")
	repository := NewRepository(pool)
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.IdempotencyKey = "arb-circuit-open"
	input.RequestFingerprint = "arb-circuit-open-fp"
	input.LegAVenueBaselineBasePosition = "0"
	input.LegBVenueBaselineBasePosition = "0"
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
			IdempotencyKey: "circuit-a", OwnerUsername: "admin",
			TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
			InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: "XMRUSDT",
			BaseAsset: "XMR", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
			Quantity: "0.029", RequestFingerprint: "circuit-a-fp",
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "market",
		},
		{
			IdempotencyKey: "circuit-b", OwnerUsername: "admin",
			TradingAccountID: accountB, ProductName: "ARBITRAGE", Exchange: "okx",
			InstrumentID: instrumentB, ContractType: "perpetual", ExchangeSymbol: "XMR-USDT-SWAP",
			BaseAsset: "XMR", QuoteAsset: "USDT", Side: "sell", OrderType: "market",
			Quantity: "0.029", RequestFingerprint: "circuit-b-fp",
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "b", ArbitrageRole: "market",
		},
	})
	if err != nil || len(flags) != 2 || !flags[0] || !flags[1] {
		t.Fatalf("intents flags=%v err=%v", flags, err)
	}
	if _, err := repository.UpdateResult(ctx, orders[0].ID, VenueResult{
		Status: "filled", FilledQuantity: "0.026", AveragePrice: "100",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.UpdateResult(ctx, orders[1].ID, VenueResult{
		Status: "filled", FilledQuantity: "0.029", AveragePrice: "100",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			circuit_open=TRUE,runtime_state='manual_intervention',
			error_message='venue result uncertain: timeout',
			last_failure_key='venue result uncertain: timeout'
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.UpdateResult(ctx, orders[0].ID, VenueResult{
		Status: "filled", FilledQuantity: "0.029", AveragePrice: "100",
	}); err != nil {
		t.Fatal(err)
	}
	combo, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	stepInstrument := Instrument{
		QuantityStep: "0.001", MinQuantity: "0.001",
		MinQuantityStatus: exchange.ConstraintKnown,
		MinNotional:       "0.1", MinNotionalStatus: exchange.ConstraintKnown,
	}
	applied, ok, err := repository.ApplyCircuitOpenExternalReconcile(
		ctx, circuitOpenExternalReconcileRequest{
			CombinationID:   combo.ID,
			ExecutionID:     execution.ID,
			ExpectedVersion: combo.Version,
			VenueA:          decimal.RequireFromString("0.029"),
			VenueB:          decimal.RequireFromString("-0.029"),
			MarkA:           decimal.RequireFromString("100"),
			MarkB:           decimal.RequireFromString("100"),
			InstrumentA:     stepInstrument,
			InstrumentB:     stepInstrument,
		},
	)
	if err != nil || !ok {
		t.Fatalf("apply ok=%v err=%v combo=%+v", ok, err, combo)
	}
	if applied.CircuitOpen || applied.RuntimeState != "monitoring" {
		t.Fatalf("applied=%+v", applied)
	}
	if !parseDecimal(applied.LegAReconciliationAdjustment).IsZero() ||
		applied.LegABasePosition != "0.029" || applied.LegBBasePosition != "-0.029" {
		t.Fatalf("positions=%+v", applied)
	}
	recomputed, err := repository.RecomputeArbitrageBasePositions(ctx, applied.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recomputed.LegABasePosition != "0.029" ||
		recomputed.LegBBasePosition != "-0.029" ||
		recomputed.LegAReconciliationAdjustment != applied.LegAReconciliationAdjustment {
		t.Fatalf("recompute dropped adjustment: %+v", recomputed)
	}
	active, err := repository.GetActiveArbitrageExecution(ctx, applied.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("active execution=%+v err=%v", active, err)
	}
}

func TestCircuitOpenExternalReconcileOverTargetIntegration(t *testing.T) {
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
	schema := fmt.Sprintf("trader_circuit_open_over_target_%d", time.Now().UnixNano())
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

	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "UAIUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "UAI-USDT-SWAP")
	repository := NewRepository(pool)
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.IdempotencyKey = "arb-circuit-over-target"
	input.RequestFingerprint = "arb-circuit-over-target-fp"
	input.TargetNotional = "500"
	input.OrderNotional = "20"
	input.LegA.ExchangeSymbol = "UAIUSDT"
	input.LegA.BaseAsset = "UAI"
	input.LegB.ExchangeSymbol = "UAI-USDT-SWAP"
	input.LegB.BaseAsset = "UAI"
	input.LegAVenueBaselineBasePosition = "0"
	input.LegBVenueBaselineBasePosition = "0"
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
			IdempotencyKey: "over-a", OwnerUsername: "admin",
			TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
			InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: "UAIUSDT",
			BaseAsset: "UAI", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
			Quantity: "1340", RequestFingerprint: "over-a-fp",
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "market",
		},
		{
			IdempotencyKey: "over-b", OwnerUsername: "admin",
			TradingAccountID: accountB, ProductName: "ARBITRAGE", Exchange: "okx",
			InstrumentID: instrumentB, ContractType: "perpetual", ExchangeSymbol: "UAI-USDT-SWAP",
			BaseAsset: "UAI", QuoteAsset: "USDT", Side: "sell", OrderType: "market",
			Quantity: "1287", RequestFingerprint: "over-b-fp",
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "b", ArbitrageRole: "market",
		},
	})
	if err != nil || len(flags) != 2 || !flags[0] || !flags[1] {
		t.Fatalf("intents flags=%v err=%v", flags, err)
	}
	if _, err := repository.UpdateResult(ctx, orders[0].ID, VenueResult{
		Status: "filled", FilledQuantity: "1340", AveragePrice: "0.463",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.UpdateResult(ctx, orders[1].ID, VenueResult{
		Status: "filled", FilledQuantity: "1287", AveragePrice: "0.463",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			circuit_open=TRUE,runtime_state='manual_intervention',
			error_message='Margin is insufficient',
			last_failure_key='Margin is insufficient',
			leg_a_reconciliation_adjustment=0,
			leg_b_reconciliation_adjustment=-38,
			leg_a_base_position=1340,
			leg_b_base_position=-1325,
			position_notional=493.86725
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	combo, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	stepInstrument := Instrument{
		QuantityStep: "0.001", MinQuantity: "0.001",
		MinQuantityStatus: exchange.ConstraintKnown,
		MinNotional:       "0.1", MinNotionalStatus: exchange.ConstraintKnown,
	}
	applied, ok, err := repository.ApplyCircuitOpenExternalReconcile(
		ctx, circuitOpenExternalReconcileRequest{
			CombinationID:   combo.ID,
			ExecutionID:     execution.ID,
			ExpectedVersion: combo.Version,
			VenueA:          decimal.RequireFromString("1340"),
			VenueB:          decimal.RequireFromString("-1340"),
			MarkA:           decimal.RequireFromString("0.463"),
			MarkB:           decimal.RequireFromString("0.463"),
			InstrumentA:     stepInstrument,
			InstrumentB:     stepInstrument,
		},
	)
	if err != nil || !ok {
		t.Fatalf("apply ok=%v err=%v combo=%+v", ok, err, combo)
	}
	if applied.CircuitOpen || applied.RuntimeState != "monitoring" {
		t.Fatalf("applied=%+v", applied)
	}
	if applied.TargetNotional != "500" {
		t.Fatalf("target mutated=%s", applied.TargetNotional)
	}
	if parseDecimal(applied.LegAReconciliationAdjustment).Cmp(decimal.Zero) != 0 ||
		parseDecimal(applied.LegBReconciliationAdjustment).Cmp(decimal.RequireFromString("-53")) != 0 {
		t.Fatalf("adjustments=%+v", applied)
	}
	if parseDecimal(applied.LegABasePosition).Cmp(decimal.RequireFromString("1340")) != 0 ||
		parseDecimal(applied.LegBBasePosition).Cmp(decimal.RequireFromString("-1340")) != 0 {
		t.Fatalf("positions=%+v", applied)
	}
	if parseDecimal(applied.CarryBaseQuantity).Cmp(decimal.Zero) != 0 {
		t.Fatalf("carry=%s", applied.CarryBaseQuantity)
	}
	wantNotional := decimal.RequireFromString("1340").Mul(decimal.RequireFromString("0.463"))
	if parseDecimal(applied.PositionNotional).Cmp(wantNotional) != 0 {
		t.Fatalf("position_notional=%s want=%s", applied.PositionNotional, wantNotional)
	}
	var execStatus, execError string
	if err := pool.QueryRow(ctx, `
		SELECT status, error_message
		FROM trader_arbitrage_executions WHERE id=$1::uuid`,
		execution.ID,
	).Scan(&execStatus, &execError); err != nil {
		t.Fatal(err)
	}
	if execStatus != "canceled" || execError != "externally_reconciled" {
		t.Fatalf("execution status=%s error=%s", execStatus, execError)
	}
	events, err := repository.ListArbitrageEvents(ctx, applied.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	for _, event := range events {
		if event.Type != "externally_reconciled" {
			continue
		}
		if err := json.Unmarshal([]byte(event.Message), &payload); err != nil {
			t.Fatal(err)
		}
		break
	}
	if payload == nil {
		t.Fatal("missing externally_reconciled event")
	}
	if payload["errorMessage"] != "Margin is insufficient" {
		t.Fatalf("errorMessage=%v", payload["errorMessage"])
	}
	if payload["overTarget"] != true {
		t.Fatalf("overTarget=%v", payload["overTarget"])
	}
	if fmt.Sprint(payload["legBPreviousAdjustment"]) != "-38" &&
		parseDecimal(fmt.Sprint(payload["legBPreviousAdjustment"])).Cmp(decimal.RequireFromString("-38")) != 0 {
		t.Fatalf("previous B adjustment=%v", payload["legBPreviousAdjustment"])
	}
	if parseDecimal(fmt.Sprint(payload["legBAdjustment"])).Cmp(decimal.RequireFromString("-53")) != 0 {
		t.Fatalf("new B adjustment=%v", payload["legBAdjustment"])
	}
	recomputed, err := repository.RecomputeArbitrageBasePositions(ctx, applied.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parseDecimal(recomputed.LegBBasePosition).Cmp(decimal.RequireFromString("-1340")) != 0 ||
		parseDecimal(recomputed.LegABasePosition).Cmp(decimal.RequireFromString("1340")) != 0 ||
		parseDecimal(recomputed.LegBReconciliationAdjustment).Cmp(decimal.RequireFromString("-53")) != 0 {
		t.Fatalf("recompute dropped adjustment: %+v", recomputed)
	}
}

func TestCircuitOpenExternalReconcilePersistsSnapshotWhenCarryExecutable(t *testing.T) {
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
	schema := fmt.Sprintf("trader_circuit_open_snap_%d", time.Now().UnixNano())
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

	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "XMRUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "XMR-USDT-SWAP")
	repository := NewRepository(pool)
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.IdempotencyKey = "arb-circuit-snap"
	input.RequestFingerprint = "arb-circuit-snap-fp"
	input.LegAVenueBaselineBasePosition = "0"
	input.LegBVenueBaselineBasePosition = "0"
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
			IdempotencyKey: "snap-a", OwnerUsername: "admin",
			TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
			InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: "XMRUSDT",
			BaseAsset: "XMR", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
			Quantity: "0.01", RequestFingerprint: "snap-a-fp",
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "market",
		},
		{
			IdempotencyKey: "snap-b", OwnerUsername: "admin",
			TradingAccountID: accountB, ProductName: "ARBITRAGE", Exchange: "okx",
			InstrumentID: instrumentB, ContractType: "perpetual", ExchangeSymbol: "XMR-USDT-SWAP",
			BaseAsset: "XMR", QuoteAsset: "USDT", Side: "sell", OrderType: "market",
			Quantity: "0.005", RequestFingerprint: "snap-b-fp",
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "b", ArbitrageRole: "market",
		},
	})
	if err != nil || len(flags) != 2 || !flags[0] || !flags[1] {
		t.Fatalf("intents flags=%v err=%v", flags, err)
	}
	if _, err := repository.UpdateResult(ctx, orders[0].ID, VenueResult{
		Status: "filled", FilledQuantity: "0.01", AveragePrice: "100",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.UpdateResult(ctx, orders[1].ID, VenueResult{
		Status: "filled", FilledQuantity: "0.005", AveragePrice: "100",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			circuit_open=TRUE,runtime_state='reconciling',
			error_message='venue result uncertain: timeout',
			last_failure_key='venue result uncertain: timeout'
		WHERE id=$1::uuid`, created.ID); err != nil {
		t.Fatal(err)
	}
	combo, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeVersion := combo.Version
	beforeLedgerA, beforeLedgerB := combo.LegABasePosition, combo.LegBBasePosition
	applied, ok, err := repository.ApplyCircuitOpenExternalReconcile(
		ctx, circuitOpenExternalReconcileRequest{
			CombinationID:   combo.ID,
			ExecutionID:     execution.ID,
			ExpectedVersion: combo.Version,
			VenueA:          decimal.RequireFromString("0.01"),
			VenueB:          decimal.RequireFromString("-0.005"),
			MarkA:           decimal.RequireFromString("100"),
			MarkB:           decimal.RequireFromString("100"),
			InstrumentA: Instrument{
				QuantityStep: "0.001", MinQuantity: "0.001",
				MinQuantityStatus: exchange.ConstraintKnown,
				MinNotional:       "0.1", MinNotionalStatus: exchange.ConstraintKnown,
			},
			InstrumentB: Instrument{
				QuantityStep: "0.01", MinQuantity: "0.01",
				MinQuantityStatus: exchange.ConstraintKnown,
				MinNotional:       "0.1", MinNotionalStatus: exchange.ConstraintKnown,
			},
		},
	)
	if err != nil || ok {
		t.Fatalf("apply ok=%v err=%v combo=%+v", ok, err, applied)
	}
	if !applied.CircuitOpen || applied.RuntimeState != "reconciling" ||
		applied.Version != beforeVersion ||
		applied.LegABasePosition != beforeLedgerA ||
		applied.LegBBasePosition != beforeLedgerB {
		t.Fatalf("unlock mutated combo: %+v", applied)
	}
	if parseDecimal(applied.LegAVenueBasePosition).Cmp(decimal.RequireFromString("0.01")) != 0 ||
		parseDecimal(applied.LegBVenueBasePosition).Cmp(decimal.RequireFromString("-0.005")) != 0 ||
		applied.LastPositionReconciledAt.IsZero() ||
		applied.LastPositionReconciledAt.Equal(time.Unix(0, 0).UTC()) {
		t.Fatalf("snapshot=%+v", applied)
	}

	pending, flags, err := repository.CreateArbitrageIntents(ctx, []Order{{
		IdempotencyKey: "snap-pending", OwnerUsername: "admin",
		TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
		InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: "XMRUSDT",
		BaseAsset: "XMR", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
		Quantity: "0.001", RequestFingerprint: "snap-pending-fp",
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "market",
	}})
	if err != nil || len(flags) != 1 || !flags[0] {
		t.Fatalf("pending flags=%v err=%v", flags, err)
	}
	_ = pending
	staleVenue := applied.LegAVenueBasePosition
	staleAt := applied.LastPositionReconciledAt
	combo, err = repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	skipped, ok, err := repository.ApplyCircuitOpenExternalReconcile(
		ctx, circuitOpenExternalReconcileRequest{
			CombinationID:   combo.ID,
			ExecutionID:     execution.ID,
			ExpectedVersion: combo.Version,
			VenueA:          decimal.RequireFromString("9"),
			VenueB:          decimal.RequireFromString("-9"),
			MarkA:           decimal.RequireFromString("100"),
			MarkB:           decimal.RequireFromString("100"),
			InstrumentA: Instrument{
				QuantityStep: "0.001", MinQuantity: "0.001",
				MinQuantityStatus: exchange.ConstraintKnown,
				MinNotional:       "0.1", MinNotionalStatus: exchange.ConstraintKnown,
			},
			InstrumentB: Instrument{
				QuantityStep: "0.01", MinQuantity: "0.01",
				MinQuantityStatus: exchange.ConstraintKnown,
				MinNotional:       "0.1", MinNotionalStatus: exchange.ConstraintKnown,
			},
		},
	)
	if err != nil || ok {
		t.Fatalf("unconfirmed ok=%v err=%v", ok, err)
	}
	if skipped.LegAVenueBasePosition != staleVenue ||
		!skipped.LastPositionReconciledAt.Equal(staleAt) {
		t.Fatalf("unconfirmed overwrote snapshot: %+v", skipped)
	}
}

func TestConfirmedAbsentFinalizeRepository(t *testing.T) {
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
	schema := fmt.Sprintf("trader_absent_finalize_%d", time.Now().UnixNano())
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

	t.Run("canceled_exec_enters_position_uncertain", func(t *testing.T) {
		fixture := insertConfirmedAbsentStuckCombo(t, ctx, pool, repository, "REC", false, "0")
		result, err := repository.FinalizeConfirmedAbsentZeroFillExecution(ctx, fixture.execution.ID)
		if err != nil || result != confirmedAbsentFinalizeCanceledExec {
			t.Fatalf("result=%s err=%v", result, err)
		}
		combo, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", fixture.combo.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !combo.PositionUncertain || combo.RuntimeState != "position_uncertain" ||
			combo.ErrorMessage != arbitrageOrderReconcileError {
			t.Fatalf("combo=%+v", combo)
		}
		executions, err := repository.ListArbitrageExecutions(ctx, fixture.combo.ID, 10)
		if err != nil || len(executions) != 1 {
			t.Fatalf("executions=%+v err=%v", executions, err)
		}
		if executions[0].Status != "canceled" || executions[0].ErrorMessage != "" {
			t.Fatalf("execution=%+v", executions[0])
		}
		events, err := repository.ListArbitrageEvents(ctx, fixture.combo.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if !hasArbitrageEventType(events, confirmedAbsentCanceledEvent) {
			t.Fatalf("events=%+v", events)
		}
	})

	t.Run("enters_position_uncertain_from_backoff", func(t *testing.T) {
		fixture := insertConfirmedAbsentStuckCombo(t, ctx, pool, repository, "BCK", false, "0")
		if _, err := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations SET
				position_uncertain=FALSE,
				runtime_state='backoff',
				error_message='temporary'
			WHERE id=$1::uuid`, fixture.combo.ID); err != nil {
			t.Fatal(err)
		}
		result, err := repository.FinalizeConfirmedAbsentZeroFillExecution(ctx, fixture.execution.ID)
		if err != nil || result != confirmedAbsentFinalizeCanceledExec {
			t.Fatalf("result=%s err=%v", result, err)
		}
		combo, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", fixture.combo.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !combo.PositionUncertain || combo.RuntimeState != "position_uncertain" ||
			combo.ErrorMessage != arbitrageOrderReconcileError {
			t.Fatalf("combo=%+v", combo)
		}
		executions, err := repository.ListArbitrageExecutions(ctx, fixture.combo.ID, 10)
		if err != nil || executions[0].Status != "canceled" {
			t.Fatalf("execution=%+v err=%v", executions, err)
		}
	})

	t.Run("circuit_open_skipped", func(t *testing.T) {
		fixture := insertConfirmedAbsentStuckCombo(t, ctx, pool, repository, "CIR", true, "0")
		result, err := repository.FinalizeConfirmedAbsentZeroFillExecution(ctx, fixture.execution.ID)
		if err != nil || result != confirmedAbsentFinalizeSkipped {
			t.Fatalf("result=%s err=%v", result, err)
		}
		executions, err := repository.ListArbitrageExecutions(ctx, fixture.combo.ID, 10)
		if err != nil || len(executions) != 1 || executions[0].Status != "maker_open" {
			t.Fatalf("execution must stay maker_open: %+v err=%v", executions, err)
		}
	})

	t.Run("leg_fill_skipped", func(t *testing.T) {
		fixture := insertConfirmedAbsentStuckCombo(t, ctx, pool, repository, "LEG", false, "0.4")
		result, err := repository.FinalizeConfirmedAbsentZeroFillExecution(ctx, fixture.execution.ID)
		if err != nil || result != confirmedAbsentFinalizeSkipped {
			t.Fatalf("result=%s err=%v", result, err)
		}
		executions, err := repository.ListArbitrageExecutions(ctx, fixture.combo.ID, 10)
		if err != nil || executions[0].Status != "maker_open" {
			t.Fatalf("execution=%+v err=%v", executions, err)
		}
	})

	t.Run("canceled_exec_keeps_position_uncertain", func(t *testing.T) {
		fixture := insertConfirmedAbsentStuckCombo(t, ctx, pool, repository, "CAN", false, "0")
		other := integrationArbitrageExecution(fixture.combo.ID, "bid")
		if _, err := pool.Exec(ctx, `
			INSERT INTO trader_arbitrage_executions (
				id,combination_id,direction,sequence,status,
				trigger_ask_spread_bps,trigger_bid_spread_bps,
				trigger_leg_a_bid,trigger_leg_a_ask,trigger_leg_b_bid,trigger_leg_b_ask,
				target_base_quantity,closed_at
			) VALUES (
				$1::uuid,$2::uuid,'bid',1,'completed',
				12,-8,100,101,99,102,1,now()
			)`, other.ID, fixture.combo.ID); err != nil {
			t.Fatal(err)
		}
		blocking, createdBlocking, err := repository.CreateIntent(ctx, Order{
			IdempotencyKey: "absent-block-CAN", OwnerUsername: "admin",
			TradingAccountID: fixture.combo.LegA.TradingAccountID,
			ProductName:      "ARBITRAGE", Exchange: "binance",
			InstrumentID: fixture.combo.LegA.InstrumentID, ContractType: "perpetual",
			ExchangeSymbol: fixture.combo.LegA.ExchangeSymbol,
			BaseAsset:      "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
			Quantity: "1", RequestFingerprint: "absent-block-fp-CAN",
			ArbitrageExecutionID: other.ID, ArbitrageLeg: "a", ArbitrageRole: "market",
		})
		if err != nil || !createdBlocking {
			t.Fatalf("blocking intent created=%v err=%v", createdBlocking, err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE trader_orders
			SET status='filled',filled_quantity=1,reconcile_failures=1
			WHERE id=$1::uuid`, blocking.ID); err != nil {
			t.Fatal(err)
		}
		result, err := repository.FinalizeConfirmedAbsentZeroFillExecution(ctx, fixture.execution.ID)
		if err != nil || result != confirmedAbsentFinalizeCanceledExec {
			t.Fatalf("result=%s err=%v", result, err)
		}
		combo, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", fixture.combo.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !combo.PositionUncertain || combo.RuntimeState != "position_uncertain" ||
			combo.ErrorMessage != arbitrageOrderReconcileError {
			t.Fatalf("combo=%+v", combo)
		}
		executions, err := repository.ListArbitrageExecutions(ctx, fixture.combo.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		var canceled, completed bool
		for _, item := range executions {
			if item.ID == fixture.execution.ID {
				if item.Status != "canceled" || item.ErrorMessage != "" {
					t.Fatalf("canceled execution=%+v", item)
				}
				canceled = true
			}
			if item.ID == other.ID && item.Status == "completed" {
				completed = true
			}
		}
		if !canceled || !completed {
			t.Fatalf("executions=%+v", executions)
		}
		events, err := repository.ListArbitrageEvents(ctx, fixture.combo.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if !hasArbitrageEventType(events, confirmedAbsentCanceledEvent) {
			t.Fatalf("events=%+v", events)
		}
	})

	t.Run("terminal_execution_cannot_return_to_reconciling", func(t *testing.T) {
		fixture := insertConfirmedAbsentStuckCombo(t, ctx, pool, repository, "ONE", false, "0")
		fixture.execution.Status = "canceled"
		updated, err := repository.UpdateArbitrageExecution(ctx, fixture.execution)
		if err != nil {
			t.Fatal(err)
		}
		updated.Status = "reconciling"
		if _, err := repository.UpdateArbitrageExecution(ctx, updated); !errors.Is(
			err, ErrArbitrageExecutionTerminal,
		) {
			t.Fatalf("err=%v", err)
		}
		executions, err := repository.ListArbitrageExecutions(ctx, fixture.combo.ID, 10)
		if err != nil || executions[0].Status != "canceled" {
			t.Fatalf("execution=%+v err=%v", executions, err)
		}
	})
}

type confirmedAbsentStuckFixture struct {
	combo     ArbitrageCombination
	execution ArbitrageExecution
	order     Order
}

func insertConfirmedAbsentStuckCombo(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	repository *Repository,
	suffix string,
	circuitOpen bool,
	legAFilled string,
) confirmedAbsentStuckFixture {
	t.Helper()
	accountA, instrumentA := insertArbitrageFixture(
		t, ctx, pool, "binance", "ABSENTA"+suffix,
	)
	accountB, instrumentB := insertArbitrageFixture(
		t, ctx, pool, "okx", "ABSENTB"+suffix,
	)
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.ID = uuid.NewString()
	input.IdempotencyKey = "arb-absent-" + suffix
	input.RequestFingerprint = "arb-absent-fp-" + suffix
	input.LegA.ExchangeSymbol = "ABSENTA" + suffix
	input.LegB.ExchangeSymbol = "ABSENTB" + suffix
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
	order, createdOrder, err := repository.CreateIntent(ctx, Order{
		IdempotencyKey: "absent-order-" + suffix, OwnerUsername: "admin",
		TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
		InstrumentID: instrumentA, ContractType: "perpetual",
		ExchangeSymbol: "ABSENTA" + suffix, BaseAsset: "BTC", QuoteAsset: "USDT",
		Side: "buy", OrderType: "market", Quantity: "1",
		RequestFingerprint:   "absent-order-fp-" + suffix,
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "maker",
	})
	if err != nil || !createdOrder {
		t.Fatalf("intent created=%v err=%v", createdOrder, err)
	}
	order, err = repository.UpdateResult(ctx, order.ID, VenueResult{
		Status: "rejected", FilledQuantity: "0",
		ErrorCode: errorConfirmedAbsentAfterUncertainSubmit,
	})
	if err != nil {
		t.Fatal(err)
	}
	execution.Status = "maker_open"
	execution.MakerOrderID = order.ID
	execution.LegAFilledQuantity = legAFilled
	execution.LegBFilledQuantity = "0"
	execution, err = repository.UpdateArbitrageExecution(ctx, execution)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET
			position_uncertain=TRUE,
			runtime_state='position_uncertain',
			error_message=$2,
			circuit_open=$3
		WHERE id=$1::uuid`,
		created.ID, arbitrageOrderReconcileError, circuitOpen,
	); err != nil {
		t.Fatal(err)
	}
	combo, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return confirmedAbsentStuckFixture{combo: combo, execution: execution, order: order}
}

func hasArbitrageEventType(events []ArbitrageEvent, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func openArbitrageIntegrationRepo(t *testing.T, ctx context.Context, schemaPrefix string) (*Repository, *pgxpool.Pool, func()) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("%s_%d", schemaPrefix, time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, pool); err != nil {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
		t.Fatal(err)
	}
	cleanup := func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	}
	return NewRepository(pool), pool, cleanup
}

func executionByID(t *testing.T, items []ArbitrageExecution, id string) ArbitrageExecution {
	t.Helper()
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("execution %s not found", id)
	return ArbitrageExecution{}
}

func TestArbitrageRecomputeDoesNotRewriteHistoricalExecutions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repository, pool, cleanup := openArbitrageIntegrationRepo(t, ctx, "trader_recompute_hist")
	defer cleanup()

	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "BTCUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "BTC-USDT-SWAP")
	created, inserted, err := repository.CreateArbitrageCombination(
		ctx, integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB),
	)
	if err != nil || !inserted {
		t.Fatalf("create inserted=%v err=%v", inserted, err)
	}
	exec1, claimed, err := repository.ClaimArbitrageExecution(
		ctx, integrationArbitrageExecution(created.ID, "ask"),
	)
	if err != nil || !claimed {
		t.Fatalf("claim1 claimed=%v err=%v", claimed, err)
	}
	pairInput := []Order{
		{
			IdempotencyKey: "hist-a", OwnerUsername: "admin",
			TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
			InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
			Quantity: "0.01", RequestFingerprint: "hist-a-fp",
			ArbitrageExecutionID: exec1.ID, ArbitrageLeg: "a", ArbitrageRole: "maker",
		},
		{
			IdempotencyKey: "hist-b", OwnerUsername: "admin",
			TradingAccountID: accountB, ProductName: "ARBITRAGE", Exchange: "okx",
			InstrumentID: instrumentB, ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
			BaseAsset: "BTC", QuoteAsset: "USDT", Side: "sell", OrderType: "market",
			Quantity: "0.01", RequestFingerprint: "hist-b-fp",
			ArbitrageExecutionID: exec1.ID, ArbitrageLeg: "b", ArbitrageRole: "hedge",
		},
	}
	if _, _, err := repository.CreateArbitrageIntents(ctx, pairInput); err != nil {
		t.Fatal(err)
	}
	orders, err := repository.ListArbitrageOrders(ctx, "admin", created.ID)
	if err != nil || len(orders) != 2 {
		t.Fatalf("orders=%d err=%v", len(orders), err)
	}
	for _, order := range orders {
		if _, err := repository.UpdateResult(ctx, order.ID, VenueResult{
			Status: "filled", FilledQuantity: "0.01", AveragePrice: "100",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repository.RecomputeArbitrageBasePositionsForExecution(ctx, exec1.ID); err != nil {
		t.Fatal(err)
	}
	exec1.Status = "completed"
	if _, err := repository.UpdateArbitrageExecution(ctx, exec1); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_executions
		SET leg_a_filled_quantity=999,leg_b_filled_quantity=888
		WHERE id=$1::uuid`, exec1.ID); err != nil {
		t.Fatal(err)
	}

	exec2, claimed, err := repository.ClaimArbitrageExecution(
		ctx, integrationArbitrageExecution(created.ID, "bid"),
	)
	if err != nil || !claimed {
		t.Fatalf("claim2 claimed=%v err=%v", claimed, err)
	}
	liveInput := []Order{
		{
			IdempotencyKey: "live-a", OwnerUsername: "admin",
			TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
			InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
			Quantity: "0.02", RequestFingerprint: "live-a-fp",
			ArbitrageExecutionID: exec2.ID, ArbitrageLeg: "a", ArbitrageRole: "maker",
		},
		{
			IdempotencyKey: "live-b", OwnerUsername: "admin",
			TradingAccountID: accountB, ProductName: "ARBITRAGE", Exchange: "okx",
			InstrumentID: instrumentB, ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
			BaseAsset: "BTC", QuoteAsset: "USDT", Side: "sell", OrderType: "market",
			Quantity: "0.02", RequestFingerprint: "live-b-fp",
			ArbitrageExecutionID: exec2.ID, ArbitrageLeg: "b", ArbitrageRole: "hedge",
		},
	}
	if _, _, err := repository.CreateArbitrageIntents(ctx, liveInput); err != nil {
		t.Fatal(err)
	}
	liveOrders, err := repository.ListArbitrageOrders(ctx, "admin", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range liveOrders {
		if order.ArbitrageExecutionID != exec2.ID {
			continue
		}
		if _, err := repository.UpdateResult(ctx, order.ID, VenueResult{
			Status: "filled", FilledQuantity: "0.02", AveragePrice: "100",
		}); err != nil {
			t.Fatal(err)
		}
	}

	combo, err := repository.RecomputeArbitrageBasePositions(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if combo.LegABasePosition != "0.03" || combo.LegBBasePosition != "-0.03" {
		t.Fatalf("combo positions=%+v", combo)
	}
	executions, err := repository.ListArbitrageExecutions(ctx, created.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	historical := executionByID(t, executions, exec1.ID)
	if parseDecimal(historical.LegAFilledQuantity).Cmp(decimal.NewFromInt(999)) != 0 ||
		parseDecimal(historical.LegBFilledQuantity).Cmp(decimal.NewFromInt(888)) != 0 {
		t.Fatalf("historical execution rewritten: %+v", historical)
	}
	current := executionByID(t, executions, exec2.ID)
	if parseDecimal(current.LegAFilledQuantity).Cmp(decimal.Zero) != 0 ||
		parseDecimal(current.LegBFilledQuantity).Cmp(decimal.Zero) != 0 {
		t.Fatalf("combo recompute wrote current execution: %+v", current)
	}

	combo, err = repository.RecomputeArbitrageBasePositionsForExecution(ctx, exec2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if combo.LegABasePosition != "0.03" || combo.LegBBasePosition != "-0.03" {
		t.Fatalf("for-execution combo=%+v", combo)
	}
	executions, err = repository.ListArbitrageExecutions(ctx, created.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	historical = executionByID(t, executions, exec1.ID)
	if parseDecimal(historical.LegAFilledQuantity).Cmp(decimal.NewFromInt(999)) != 0 ||
		parseDecimal(historical.LegBFilledQuantity).Cmp(decimal.NewFromInt(888)) != 0 {
		t.Fatalf("for-execution rewrote historical: %+v", historical)
	}
	current = executionByID(t, executions, exec2.ID)
	if parseDecimal(current.LegAFilledQuantity).Cmp(decimal.RequireFromString("0.02")) != 0 ||
		parseDecimal(current.LegBFilledQuantity).Cmp(decimal.RequireFromString("0.02")) != 0 {
		t.Fatalf("current execution fills=%+v", current)
	}
}

func TestArbitragePrepareRecomputeUpdateResultConcurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	repository, pool, cleanup := openArbitrageIntegrationRepo(t, ctx, "trader_deadlock_conc")
	defer cleanup()

	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "BTCUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "BTC-USDT-SWAP")
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.IdempotencyKey = "arb-deadlock-combo"
	input.RequestFingerprint = "arb-deadlock-fp"
	created, inserted, err := repository.CreateArbitrageCombination(ctx, input)
	if err != nil || !inserted {
		t.Fatalf("create inserted=%v err=%v", inserted, err)
	}
	historical, claimed, err := repository.ClaimArbitrageExecution(
		ctx, integrationArbitrageExecution(created.ID, "ask"),
	)
	if err != nil || !claimed {
		t.Fatalf("historical claim claimed=%v err=%v", claimed, err)
	}
	histOrders := []Order{
		{
			IdempotencyKey: "deadlock-hist-a", OwnerUsername: "admin",
			TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
			InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
			Quantity: "0.01", RequestFingerprint: "deadlock-hist-a-fp",
			ArbitrageExecutionID: historical.ID, ArbitrageLeg: "a", ArbitrageRole: "maker",
		},
		{
			IdempotencyKey: "deadlock-hist-b", OwnerUsername: "admin",
			TradingAccountID: accountB, ProductName: "ARBITRAGE", Exchange: "okx",
			InstrumentID: instrumentB, ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
			BaseAsset: "BTC", QuoteAsset: "USDT", Side: "sell", OrderType: "market",
			Quantity: "0.01", RequestFingerprint: "deadlock-hist-b-fp",
			ArbitrageExecutionID: historical.ID, ArbitrageLeg: "b", ArbitrageRole: "hedge",
		},
	}
	createdHist, _, err := repository.CreateArbitrageIntents(ctx, histOrders)
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range createdHist {
		if _, err := repository.UpdateResult(ctx, order.ID, VenueResult{
			Status: "filled", FilledQuantity: "0.01", AveragePrice: "100",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repository.RecomputeArbitrageBasePositionsForExecution(ctx, historical.ID); err != nil {
		t.Fatal(err)
	}
	historical.Status = "completed"
	if _, err := repository.UpdateArbitrageExecution(ctx, historical); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_executions
		SET leg_a_filled_quantity=999,leg_b_filled_quantity=888
		WHERE id=$1::uuid`, historical.ID); err != nil {
		t.Fatal(err)
	}

	execution, claimed, err := repository.ClaimArbitrageExecution(
		ctx, integrationArbitrageExecution(created.ID, "bid"),
	)
	if err != nil || !claimed {
		t.Fatalf("live claim claimed=%v err=%v", claimed, err)
	}
	maker, createdMaker, err := repository.CreateIntent(ctx, Order{
		IdempotencyKey: "deadlock-maker", OwnerUsername: "admin",
		TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
		InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
		Quantity: "0.02", RequestFingerprint: "deadlock-maker-fp",
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "maker",
	})
	if err != nil || !createdMaker {
		t.Fatalf("maker created=%v err=%v", createdMaker, err)
	}
	if _, err := repository.UpdateResult(ctx, maker.ID, VenueResult{
		Status: "open", FilledQuantity: "0.01", AveragePrice: "100",
	}); err != nil {
		t.Fatal(err)
	}

	hedgeIntent := Order{
		IdempotencyKey: "deadlock-hedge", OwnerUsername: "admin",
		TradingAccountID: accountB, ProductName: "ARBITRAGE", Exchange: "okx",
		InstrumentID: instrumentB, ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "sell", OrderType: "limit",
		Quantity: "0.01", Price: "100", RequestFingerprint: "deadlock-hedge-fp",
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "b", ArbitrageRole: "hedge",
	}
	if _, err := repository.PrepareArbitrageHedgeIntent(ctx, PrepareArbitrageHedgeIntentInput{
		Combination: created, Execution: execution, Order: hedgeIntent,
		ExpectedSequence: 0, HedgeSide: "sell", TargetQuantity: "0.01",
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 96)
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				_, err := repository.PrepareArbitrageHedgeIntent(ctx, PrepareArbitrageHedgeIntentInput{
					Combination: created, Execution: execution, Order: hedgeIntent,
					ExpectedSequence: 0, HedgeSide: "sell", TargetQuantity: "0.01",
				})
				if err != nil {
					errCh <- err
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				if _, err := repository.RecomputeArbitrageBasePositionsForExecution(ctx, execution.ID); err != nil {
					errCh <- err
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				if _, err := repository.UpdateResult(ctx, maker.ID, VenueResult{
					Status: "open", FilledQuantity: "0.01", AveragePrice: "100",
				}); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if isPostgresDeadlock(err) {
			t.Fatalf("unretried deadlock: %v", err)
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if _, err := repository.RecomputeArbitrageBasePositionsForExecution(ctx, execution.ID); err != nil {
		t.Fatal(err)
	}

	combo, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	orders, err := repository.ListArbitrageOrders(ctx, "admin", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	legA, legB := decimal.Zero, decimal.Zero
	currentA, currentB := decimal.Zero, decimal.Zero
	for _, order := range orders {
		filled := parseDecimal(order.FilledQuantity)
		signed := filled
		if strings.EqualFold(order.Side, "sell") {
			signed = signed.Neg()
		}
		if order.ArbitrageLeg == "b" {
			legB = legB.Add(signed)
		} else {
			legA = legA.Add(signed)
		}
		if order.ArbitrageExecutionID != execution.ID {
			continue
		}
		if order.ArbitrageLeg == "b" {
			currentB = currentB.Add(filled)
		} else {
			currentA = currentA.Add(filled)
		}
	}
	if parseDecimal(combo.LegABasePosition).Cmp(legA) != 0 ||
		parseDecimal(combo.LegBBasePosition).Cmp(legB) != 0 {
		t.Fatalf("combo=%+v want a=%s b=%s", combo, legA, legB)
	}
	executions, err := repository.ListArbitrageExecutions(ctx, created.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	gotHistorical := executionByID(t, executions, historical.ID)
	if parseDecimal(gotHistorical.LegAFilledQuantity).Cmp(decimal.NewFromInt(999)) != 0 ||
		parseDecimal(gotHistorical.LegBFilledQuantity).Cmp(decimal.NewFromInt(888)) != 0 {
		t.Fatalf("historical rewritten: %+v", gotHistorical)
	}
	gotCurrent := executionByID(t, executions, execution.ID)
	if parseDecimal(gotCurrent.LegAFilledQuantity).Cmp(currentA) != 0 ||
		parseDecimal(gotCurrent.LegBFilledQuantity).Cmp(currentB) != 0 {
		t.Fatalf("current fills=%+v want a=%s b=%s", gotCurrent, currentA, currentB)
	}
}

func insertFilledArbitrageOrder(
	t *testing.T,
	ctx context.Context,
	repository *Repository,
	order Order,
	filled string,
) Order {
	t.Helper()
	created, inserted, err := repository.CreateIntent(ctx, order)
	if err != nil || !inserted {
		t.Fatalf("create intent inserted=%v err=%v", inserted, err)
	}
	updated, err := repository.UpdateResult(ctx, created.ID, VenueResult{
		Status: "filled", FilledQuantity: filled, AveragePrice: "100",
	})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func TestArbitrageRecomputePairedFillsExcludeResidualIncludeNullRole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repository, pool, cleanup := openArbitrageIntegrationRepo(t, ctx, "trader_recompute_residual")
	defer cleanup()

	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "BTCUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "BTC-USDT-SWAP")
	created, inserted, err := repository.CreateArbitrageCombination(
		ctx, integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB),
	)
	if err != nil || !inserted {
		t.Fatalf("create inserted=%v err=%v", inserted, err)
	}
	execution, claimed, err := repository.ClaimArbitrageExecution(
		ctx, integrationArbitrageExecution(created.ID, "ask"),
	)
	if err != nil || !claimed {
		t.Fatalf("claim claimed=%v err=%v", claimed, err)
	}
	insertFilledArbitrageOrder(t, ctx, repository, Order{
		IdempotencyKey: "residual-paired-maker", OwnerUsername: "admin",
		TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
		InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
		Quantity: "10401", RequestFingerprint: "residual-paired-maker-fp",
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "maker",
	}, "10401")
	insertFilledArbitrageOrder(t, ctx, repository, Order{
		IdempotencyKey: "residual-paired-hedge", OwnerUsername: "admin",
		TradingAccountID: accountB, ProductName: "ARBITRAGE", Exchange: "okx",
		InstrumentID: instrumentB, ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "sell", OrderType: "market",
		Quantity: "10401", RequestFingerprint: "residual-paired-hedge-fp",
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "b", ArbitrageRole: "hedge",
	}, "10401")
	insertFilledArbitrageOrder(t, ctx, repository, Order{
		IdempotencyKey: "residual-paired-residual", OwnerUsername: "admin",
		TradingAccountID: accountB, ProductName: "ARBITRAGE", Exchange: "okx",
		InstrumentID: instrumentB, ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "sell", OrderType: "market",
		Quantity: "4501", RequestFingerprint: "residual-paired-residual-fp",
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "b", ArbitrageRole: "residual",
	}, "4501")
	insertFilledArbitrageOrder(t, ctx, repository, Order{
		IdempotencyKey: "residual-paired-null", OwnerUsername: "admin",
		TradingAccountID: accountA, ProductName: "ARBITRAGE", Exchange: "binance",
		InstrumentID: instrumentA, ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "market",
		Quantity: "100", RequestFingerprint: "residual-paired-null-fp",
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a",
	}, "100")

	combo, err := repository.RecomputeArbitrageBasePositionsForExecution(ctx, execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	executions, err := repository.ListArbitrageExecutions(ctx, created.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	got := executionByID(t, executions, execution.ID)
	if !parseDecimal(got.LegAFilledQuantity).Equal(decimal.NewFromInt(10501)) ||
		!parseDecimal(got.LegBFilledQuantity).Equal(decimal.NewFromInt(10401)) {
		t.Fatalf("paired fills=%+v", got)
	}
	wantA := decimal.NewFromInt(10501)
	wantB := decimal.NewFromInt(-10401).Sub(decimal.NewFromInt(4501))
	if !parseDecimal(combo.LegABasePosition).Equal(wantA) ||
		!parseDecimal(combo.LegBBasePosition).Equal(wantB) {
		t.Fatalf("combo ledger a=%s b=%s want a=%s b=%s",
			combo.LegABasePosition, combo.LegBBasePosition, wantA, wantB)
	}

	t.Run("empty_role_counts_as_paired", func(t *testing.T) {
		emptyRole, insertedEmpty, err := repository.CreateIntent(ctx, Order{
			IdempotencyKey: "residual-paired-empty", OwnerUsername: "admin",
			TradingAccountID: accountB, ProductName: "ARBITRAGE", Exchange: "okx",
			InstrumentID: instrumentB, ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
			BaseAsset: "BTC", QuoteAsset: "USDT", Side: "sell", OrderType: "market",
			Quantity: "50", RequestFingerprint: "residual-paired-empty-fp",
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "b", ArbitrageRole: "hedge",
		})
		if err != nil || !insertedEmpty {
			t.Fatalf("empty role insert inserted=%v err=%v", insertedEmpty, err)
		}
		if _, err := repository.UpdateResult(ctx, emptyRole.ID, VenueResult{
			Status: "filled", FilledQuantity: "50", AveragePrice: "100",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE trader_orders SET arbitrage_role='' WHERE id=$1::uuid`, emptyRole.ID); err != nil {
			t.Skipf("empty arbitrage_role is rejected by check constraint: %v", err)
		}
		combo, err := repository.RecomputeArbitrageBasePositionsForExecution(ctx, execution.ID)
		if err != nil {
			t.Fatal(err)
		}
		executions, err := repository.ListArbitrageExecutions(ctx, created.ID, 20)
		if err != nil {
			t.Fatal(err)
		}
		got := executionByID(t, executions, execution.ID)
		if !parseDecimal(got.LegAFilledQuantity).Equal(decimal.NewFromInt(10501)) ||
			!parseDecimal(got.LegBFilledQuantity).Equal(decimal.NewFromInt(10451)) {
			t.Fatalf("empty-role paired fills=%+v", got)
		}
		if !parseDecimal(combo.LegBBasePosition).Equal(wantB.Sub(decimal.NewFromInt(50))) {
			t.Fatalf("combo ledger after empty role b=%s", combo.LegBBasePosition)
		}
	})
}

func TestFailLastCloseClipUnbalancedFailClosedAndClosedComboRollback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repository, pool, cleanup := openArbitrageIntegrationRepo(t, ctx, "trader_last_close_unbalanced")
	defer cleanup()

	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "BTCUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "BTC-USDT-SWAP")
	created, inserted, err := repository.CreateArbitrageCombination(
		ctx, integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB),
	)
	if err != nil || !inserted {
		t.Fatalf("create inserted=%v err=%v", inserted, err)
	}
	execution, claimed, err := repository.ClaimArbitrageExecution(
		ctx, integrationArbitrageExecution(created.ID, "ask"),
	)
	if err != nil || !claimed {
		t.Fatalf("claim claimed=%v err=%v", claimed, err)
	}
	combo, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := repository.FailLastCloseClipUnbalanced(
		ctx, combo, execution, "10", "0", "10",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Execution.Status != "failed" ||
		result.Execution.ClosedAt.IsZero() ||
		result.Execution.ClosedAt.Equal(time.Unix(0, 0).UTC()) {
		t.Fatalf("execution=%+v", result.Execution)
	}
	if result.Combination.RuntimeState != "manual_intervention" ||
		result.Combination.Status != "running" ||
		!result.Combination.CircuitOpen {
		t.Fatalf("combo=%+v", result.Combination)
	}
	if _, err := repository.GetActiveArbitrageExecution(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("active err=%v", err)
	}
	events, err := repository.ListArbitrageEvents(ctx, created.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	unbalanced := 0
	for _, event := range events {
		if event.Type == "last_close_clip_unbalanced" {
			unbalanced++
			var payload map[string]any
			if err := json.Unmarshal([]byte(event.Message), &payload); err != nil {
				t.Fatalf("payload=%s err=%v", event.Message, err)
			}
			if payload["legAFilledQuantity"] != "10" ||
				payload["legBFilledQuantity"] != "0" ||
				payload["remainingHedgeQuantity"] != "10" {
				t.Fatalf("payload=%v", payload)
			}
		}
	}
	if unbalanced != 1 {
		t.Fatalf("events=%+v", events)
	}
	again, err := repository.FailLastCloseClipUnbalanced(
		ctx, result.Combination, result.Execution, "10", "0", "10",
	)
	if err != nil {
		t.Fatal(err)
	}
	if again.Execution.Status != "failed" {
		t.Fatalf("idempotent execution=%+v", again.Execution)
	}
	events, err = repository.ListArbitrageEvents(ctx, created.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	unbalanced = 0
	for _, event := range events {
		if event.Type == "last_close_clip_unbalanced" {
			unbalanced++
		}
	}
	if unbalanced != 1 {
		t.Fatalf("idempotent events=%+v", events)
	}

	closedInput := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	closedInput.ID = uuid.NewString()
	closedInput.IdempotencyKey = "arb-last-close-closed"
	closedInput.RequestFingerprint = "arb-last-close-closed-fp"
	closedCombo, closedInserted, err := repository.CreateArbitrageCombination(ctx, closedInput)
	if err != nil || !closedInserted {
		t.Fatalf("closed create inserted=%v err=%v", closedInserted, err)
	}
	closedExec, closedClaimed, err := repository.ClaimArbitrageExecution(
		ctx, integrationArbitrageExecution(closedCombo.ID, "ask"),
	)
	if err != nil || !closedClaimed {
		t.Fatalf("closed claim claimed=%v err=%v", closedClaimed, err)
	}
	closedCombo, err = repository.GetArbitrageCombinationByOwner(ctx, "admin", closedCombo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations SET status='closed' WHERE id=$1::uuid`,
		closedCombo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.FailLastCloseClipUnbalanced(
		ctx, closedCombo, closedExec, "10", "0", "10",
	); !errors.Is(err, ErrArbitrageLastCloseClipUnbalancedConflict) {
		t.Fatalf("closed combo err=%v", err)
	}
	executions, err := repository.ListArbitrageExecutions(ctx, closedCombo.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	kept := executionByID(t, executions, closedExec.ID)
	if kept.Status == "failed" {
		t.Fatalf("closed combo execution became failed: %+v", kept)
	}
	closedAfter, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", closedCombo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closedAfter.RuntimeState == "manual_intervention" {
		t.Fatalf("closed combo entered MI: %+v", closedAfter)
	}
	closedEvents, err := repository.ListArbitrageEvents(ctx, closedCombo.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range closedEvents {
		if event.Type == "last_close_clip_unbalanced" {
			t.Fatalf("closed combo events=%+v", closedEvents)
		}
	}
}
