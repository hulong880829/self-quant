package trader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"selfquant/backend/internal/database"
)

type hedgeFastPathRepoEnv struct {
	ctx         context.Context
	pool        *pgxpool.Pool
	repo        *Repository
	combo       ArbitrageCombination
	accountA    int64
	accountB    int64
	instrumentA int64
	instrumentB int64
}

func openHedgeFastPathRepo(t *testing.T) *hedgeFastPathRepoEnv {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	schema := fmt.Sprintf("trader_hedge_fast_path_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		cancel()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
		cancel()
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
		cancel()
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, pool); err != nil {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
		cancel()
	})

	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "BTCUSDT")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "BTC-USDT-SWAP")
	repo := NewRepository(pool)
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.IdempotencyKey = "hedge-fast-path-" + uuid.NewString()
	input.RequestFingerprint = "hedge-fast-path-fp-" + uuid.NewString()
	input.ExecutionMode = "maker_then_hedge"
	input.MakerLeg = "a"
	combo, inserted, err := repo.CreateArbitrageCombination(ctx, input)
	if err != nil || !inserted {
		t.Fatalf("create inserted=%v err=%v", inserted, err)
	}
	return &hedgeFastPathRepoEnv{
		ctx: ctx, pool: pool, repo: repo, combo: combo,
		accountA: accountA, accountB: accountB,
		instrumentA: instrumentA, instrumentB: instrumentB,
	}
}

func (env *hedgeFastPathRepoEnv) claimMakerFilled(
	t *testing.T, suffix, filled string,
) (ArbitrageExecution, Order) {
	t.Helper()
	execution, claimed, err := env.repo.ClaimArbitrageExecution(
		env.ctx, integrationArbitrageExecution(env.combo.ID, "ask"),
	)
	if err != nil || !claimed {
		t.Fatalf("claim claimed=%v err=%v", claimed, err)
	}
	maker, created, err := env.repo.CreateIntent(env.ctx, Order{
		IdempotencyKey: "fast-maker-" + suffix, OwnerUsername: "admin",
		TradingAccountID: env.accountA, ProductName: "ARBITRAGE", Exchange: "binance",
		InstrumentID: env.instrumentA, ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "buy", OrderType: "limit",
		Quantity: filled, Price: "100", RequestFingerprint: "fast-maker-fp-" + suffix,
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", ArbitrageRole: "maker",
	})
	if err != nil || !created {
		t.Fatalf("maker created=%v err=%v", created, err)
	}
	maker, err = env.repo.UpdateResult(env.ctx, maker.ID, VenueResult{
		Status: "filled", FilledQuantity: filled, AveragePrice: "100",
	})
	if err != nil {
		t.Fatal(err)
	}
	execution.Status = "maker_open"
	execution.MakerOrderID = maker.ID
	execution.LegAFilledQuantity = maker.FilledQuantity
	execution.LegBFilledQuantity = "0"
	execution, err = env.repo.UpdateArbitrageExecution(env.ctx, execution)
	if err != nil {
		t.Fatal(err)
	}
	return execution, maker
}

func (env *hedgeFastPathRepoEnv) hedgeIntent(
	execution ArbitrageExecution, suffix, quantity string,
) Order {
	return Order{
		IdempotencyKey: "fast-hedge-" + suffix, OwnerUsername: "admin",
		TradingAccountID: env.accountB, ProductName: "ARBITRAGE", Exchange: "okx",
		InstrumentID: env.instrumentB, ContractType: "perpetual",
		ExchangeSymbol: "BTC-USDT-SWAP", BaseAsset: "BTC", QuoteAsset: "USDT",
		Side: "sell", OrderType: "limit", Quantity: quantity, Price: "100",
		RequestFingerprint:   "fast-hedge-fp-" + suffix,
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "b", ArbitrageRole: "hedge",
	}
}

func (env *hedgeFastPathRepoEnv) countHedgeOrders(t *testing.T) int {
	t.Helper()
	orders, err := env.repo.ListArbitrageOrders(env.ctx, "admin", env.combo.ID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, order := range orders {
		if order.ArbitrageRole == "hedge" {
			count++
		}
	}
	return count
}

func TestHedgeFastPathConcurrentExecutionsOnlyActiveCreatesIntent(t *testing.T) {
	env := openHedgeFastPathRepo(t)
	stale, staleMaker := env.claimMakerFilled(t, "stale", "0.01")
	stale.Status = "completed"
	if _, err := env.repo.UpdateArbitrageExecution(env.ctx, stale); err != nil {
		t.Fatal(err)
	}
	active, activeMaker := env.claimMakerFilled(t, "active", "0.01")

	type outcome struct {
		created bool
		err     error
		exec    ArbitrageExecution
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	prepare := func(execution ArbitrageExecution, maker Order, suffix string) {
		defer wg.Done()
		got, err := env.repo.PrepareArbitrageHedgeIntent(env.ctx, PrepareArbitrageHedgeIntentInput{
			Combination: env.combo, Execution: execution,
			Order:                env.hedgeIntent(execution, suffix, maker.FilledQuantity),
			ExpectedSequence:     0,
			HedgeLeg:             env.combo.LegB,
			HedgeSide:            "sell",
			TargetQuantity:       maker.FilledQuantity,
			FastPathAdmission:    true,
			ConfirmedMakerFilled: maker.FilledQuantity,
			ConfirmedHedgeFilled: "0",
		})
		results <- outcome{created: got.Created, err: err, exec: got.Execution}
	}
	wg.Add(2)
	go prepare(stale, staleMaker, "stale-hedge")
	go prepare(active, activeMaker, "active-hedge")
	wg.Wait()
	close(results)

	var activeCreated, staleConflict int
	for item := range results {
		if item.err == nil {
			if !item.created || item.exec.ID != active.ID || item.exec.HedgeSequence != 1 {
				t.Fatalf("unexpected success: %+v", item.exec)
			}
			activeCreated++
			continue
		}
		if errors.Is(item.err, ErrArbitrageExecutionTerminal) ||
			errors.Is(item.err, ErrArbitrageHedgeAdmissionConflict) {
			staleConflict++
			continue
		}
		t.Fatalf("unexpected err=%v", item.err)
	}
	if activeCreated != 1 || staleConflict != 1 {
		t.Fatalf("created=%d conflict=%d", activeCreated, staleConflict)
	}
	if env.countHedgeOrders(t) != 1 {
		t.Fatalf("hedge orders=%d", env.countHedgeOrders(t))
	}
	executions, err := env.repo.ListArbitrageExecutions(env.ctx, env.combo.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	gotStale := executionByID(t, executions, stale.ID)
	gotActive := executionByID(t, executions, active.ID)
	if gotStale.HedgeSequence != 0 || gotStale.HedgeOrderID != "" {
		t.Fatalf("stale mutated: %+v", gotStale)
	}
	if gotActive.HedgeSequence != 1 || gotActive.HedgeOrderID == "" {
		t.Fatalf("active=%+v", gotActive)
	}
}

func TestHedgeFastPathFillMismatchRollsBack(t *testing.T) {
	env := openHedgeFastPathRepo(t)
	execution, maker := env.claimMakerFilled(t, "mismatch", "0.01")
	_, err := env.repo.PrepareArbitrageHedgeIntent(env.ctx, PrepareArbitrageHedgeIntentInput{
		Combination: env.combo, Execution: execution,
		Order:                env.hedgeIntent(execution, "mismatch", maker.FilledQuantity),
		ExpectedSequence:     0,
		HedgeLeg:             env.combo.LegB,
		HedgeSide:            "sell",
		TargetQuantity:       maker.FilledQuantity,
		FastPathAdmission:    true,
		ConfirmedMakerFilled: "1",
		ConfirmedHedgeFilled: "0",
	})
	if !errors.Is(err, ErrArbitrageHedgeAdmissionConflict) {
		t.Fatalf("err=%v", err)
	}
	if env.countHedgeOrders(t) != 0 {
		t.Fatalf("hedge orders=%d", env.countHedgeOrders(t))
	}
	got, err := env.repo.GetActiveArbitrageExecution(env.ctx, env.combo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HedgeSequence != 0 || got.HedgeOrderID != "" || got.Status != "maker_open" {
		t.Fatalf("execution mutated: %+v", got)
	}
}

func TestHedgeFastPathConcurrentIdempotencyCreatedReused(t *testing.T) {
	env := openHedgeFastPathRepo(t)
	execution, maker := env.claimMakerFilled(t, "idem", "0.01")
	intent := env.hedgeIntent(execution, "idem", maker.FilledQuantity)
	type outcome struct {
		created bool
		err     error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := env.repo.PrepareArbitrageHedgeIntent(env.ctx, PrepareArbitrageHedgeIntentInput{
				Combination: env.combo, Execution: execution, Order: intent,
				ExpectedSequence:     0,
				HedgeLeg:             env.combo.LegB,
				HedgeSide:            "sell",
				TargetQuantity:       maker.FilledQuantity,
				FastPathAdmission:    true,
				ConfirmedMakerFilled: maker.FilledQuantity,
				ConfirmedHedgeFilled: "0",
			})
			results <- outcome{created: got.Created, err: err}
		}()
	}
	wg.Wait()
	close(results)
	created := 0
	reused := 0
	for item := range results {
		if item.err != nil {
			t.Fatal(item.err)
		}
		if item.created {
			created++
		} else {
			reused++
		}
	}
	if created != 1 || reused != 1 {
		t.Fatalf("created=%d reused=%d", created, reused)
	}
	if env.countHedgeOrders(t) != 1 {
		t.Fatalf("hedge orders=%d", env.countHedgeOrders(t))
	}
	got, err := env.repo.GetActiveArbitrageExecution(env.ctx, env.combo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HedgeSequence != 1 {
		t.Fatalf("sequence=%d", got.HedgeSequence)
	}
}

func TestClaimArbitrageExecutionPersistsLastCloseClip(t *testing.T) {
	env := openHedgeFastPathRepo(t)
	input := integrationArbitrageExecution(env.combo.ID, "ask")
	input.LastCloseClip = true
	execution, claimed, err := env.repo.ClaimArbitrageExecution(env.ctx, input)
	if err != nil || !claimed || !execution.LastCloseClip {
		t.Fatalf("claimed=%v lastCloseClip=%v err=%v", claimed, execution.LastCloseClip, err)
	}
	execution.Status = "maker_open"
	updated, err := env.repo.UpdateArbitrageExecution(env.ctx, execution)
	if err != nil || !updated.LastCloseClip || updated.Status != "maker_open" {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
}

func TestAggregateCarryAdmissionConflictAndReuse(t *testing.T) {
	env := openHedgeFastPathRepo(t)
	execution, _ := env.claimMakerFilled(t, "agg", "0.01")
	if _, err := env.pool.Exec(env.ctx, `
		UPDATE trader_arbitrage_combinations
		SET leg_a_base_position=90, leg_b_base_position=-129
		WHERE id=$1::uuid`, env.combo.ID); err != nil {
		t.Fatal(err)
	}
	combo, err := env.repo.GetArbitrageCombinationByOwner(env.ctx, "admin", env.combo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !parseDecimal(combo.CarryBaseQuantity).Equal(parseDecimal("-39")) {
		t.Fatalf("carry=%s", combo.CarryBaseQuantity)
	}
	intent := env.hedgeIntent(execution, "agg", "39")
	_, err = env.repo.PrepareArbitrageHedgeIntent(env.ctx, PrepareArbitrageHedgeIntentInput{
		Combination: combo, Execution: execution, Order: intent,
		ExpectedSequence:      0,
		HedgeLeg:              env.combo.LegB,
		HedgeSide:             "buy",
		TargetQuantity:        "39",
		AggregateCarry:        true,
		ExpectedCarryQuantity: "0",
	})
	if !errors.Is(err, ErrArbitrageHedgeAdmissionConflict) {
		t.Fatalf("mismatch err=%v", err)
	}
	if env.countHedgeOrders(t) != 0 {
		t.Fatalf("hedge orders=%d", env.countHedgeOrders(t))
	}

	type outcome struct {
		created bool
		err     error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, prepErr := env.repo.PrepareArbitrageHedgeIntent(env.ctx, PrepareArbitrageHedgeIntentInput{
				Combination: combo, Execution: execution, Order: intent,
				ExpectedSequence:      0,
				HedgeLeg:              env.combo.LegB,
				HedgeSide:             "buy",
				TargetQuantity:        "39",
				AggregateCarry:        true,
				ExpectedCarryQuantity: combo.CarryBaseQuantity,
			})
			results <- outcome{created: got.Created, err: prepErr}
		}()
	}
	wg.Wait()
	close(results)
	created, reused := 0, 0
	for item := range results {
		if item.err != nil {
			t.Fatal(item.err)
		}
		if item.created {
			created++
		} else {
			reused++
		}
	}
	if created != 1 || reused != 1 {
		t.Fatalf("created=%d reused=%d", created, reused)
	}
	if env.countHedgeOrders(t) != 1 {
		t.Fatalf("hedge orders=%d", env.countHedgeOrders(t))
	}

	if _, err := env.pool.Exec(env.ctx, `
		UPDATE trader_arbitrage_combinations
		SET leg_a_base_position=1, leg_b_base_position=-1
		WHERE id=$1::uuid`, env.combo.ID); err != nil {
		t.Fatal(err)
	}
	changed := env.hedgeIntent(execution, "agg-changed", "39")
	_, err = env.repo.PrepareArbitrageHedgeIntent(env.ctx, PrepareArbitrageHedgeIntentInput{
		Combination: combo, Execution: execution, Order: changed,
		ExpectedSequence:      1,
		HedgeLeg:              env.combo.LegB,
		HedgeSide:             "buy",
		TargetQuantity:        "39",
		AggregateCarry:        true,
		ExpectedCarryQuantity: combo.CarryBaseQuantity,
	})
	if !errors.Is(err, ErrArbitrageHedgeAdmissionConflict) {
		t.Fatalf("stale carry err=%v", err)
	}
	if env.countHedgeOrders(t) != 1 {
		t.Fatalf("stale carry inserted extra hedge: %d", env.countHedgeOrders(t))
	}
}

func (env *hedgeFastPathRepoEnv) reloadCombo(t *testing.T) ArbitrageCombination {
	t.Helper()
	combo, err := env.repo.GetArbitrageCombinationByOwner(env.ctx, "admin", env.combo.ID)
	if err != nil {
		t.Fatal(err)
	}
	env.combo = combo
	return combo
}

func (env *hedgeFastPathRepoEnv) setCarryRuntime(
	t *testing.T, legA, legB string,
) ArbitrageCombination {
	t.Helper()
	if _, err := env.pool.Exec(env.ctx, `
		UPDATE trader_arbitrage_combinations
		SET leg_a_base_position=$2::numeric,
		    leg_b_base_position=$3::numeric,
		    runtime_state='reconciling',
		    circuit_open=TRUE
		WHERE id=$1::uuid`, env.combo.ID, legA, legB); err != nil {
		t.Fatal(err)
	}
	return env.reloadCombo(t)
}

func (env *hedgeFastPathRepoEnv) insertRoleOrder(
	t *testing.T,
	execution ArbitrageExecution,
	suffix, quantity, status, filled, leg, role string,
) Order {
	t.Helper()
	accountID, instrumentID, venue, symbol := env.accountB, env.instrumentB, "okx", "BTC-USDT-SWAP"
	if leg == "a" {
		accountID, instrumentID, venue, symbol = env.accountA, env.instrumentA, "binance", "BTCUSDT"
	}
	order, created, err := env.repo.CreateIntent(env.ctx, Order{
		IdempotencyKey: "seed-order-" + suffix, OwnerUsername: "admin",
		TradingAccountID: accountID, ProductName: "ARBITRAGE", Exchange: venue,
		InstrumentID: instrumentID, ContractType: "perpetual", ExchangeSymbol: symbol,
		BaseAsset: "BTC", QuoteAsset: "USDT", Side: "sell", OrderType: "limit",
		Quantity: quantity, Price: "100", RequestFingerprint: "seed-fp-" + suffix,
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: leg, ArbitrageRole: role,
	})
	if err != nil || !created {
		t.Fatalf("seed order created=%v err=%v", created, err)
	}
	if status == "unknown" {
		if _, err := env.pool.Exec(env.ctx, `
			UPDATE trader_orders SET status='unknown' WHERE id=$1::uuid`, order.ID); err != nil {
			t.Fatal(err)
		}
		order.Status = "unknown"
		return order
	}
	if status != "" && status != "pending" {
		order, err = env.repo.UpdateResult(env.ctx, order.ID, VenueResult{
			Status: status, FilledQuantity: filled, AveragePrice: "100",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return order
}

func (env *hedgeFastPathRepoEnv) pointHedge(
	t *testing.T, execution ArbitrageExecution, hedgeID string, seq int64,
) ArbitrageExecution {
	t.Helper()
	execution.HedgeOrderID = hedgeID
	execution.HedgeSequence = seq
	execution.Status = "reconciling"
	updated, err := env.repo.UpdateArbitrageExecution(env.ctx, execution)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func (env *hedgeFastPathRepoEnv) carryHedgeIntent(
	execution ArbitrageExecution, seq int64, quantity string,
) Order {
	intent := env.hedgeIntent(execution, fmt.Sprintf("carry-%d-%s", seq, quantity), quantity)
	intent.IdempotencyKey = fmt.Sprintf("arb:%s:b:hedge:%d", execution.ID, seq)
	return intent
}

func (env *hedgeFastPathRepoEnv) prepareAggregateCarry(
	combo ArbitrageCombination,
	execution ArbitrageExecution,
	order Order,
	seq int64,
) (PrepareArbitrageHedgeIntentResult, error) {
	return env.repo.PrepareArbitrageHedgeIntent(env.ctx, PrepareArbitrageHedgeIntentInput{
		Combination: combo, Execution: execution, Order: order,
		ExpectedSequence:      seq,
		HedgeLeg:              env.combo.LegB,
		HedgeSide:             order.Side,
		TargetQuantity:        order.Quantity,
		AggregateCarry:        true,
		ExpectedCarryQuantity: combo.CarryBaseQuantity,
	})
}

func (env *hedgeFastPathRepoEnv) seedReplaceableHedge(
	t *testing.T, status, filled, legA, legB, hedgeLeg, role string, seq int64,
) (ArbitrageCombination, ArbitrageExecution, Order, Order) {
	t.Helper()
	execution, maker := env.claimMakerFilled(t, "replace-"+uuid.NewString(), "0.01")
	combo := env.setCarryRuntime(t, legA, legB)
	old := env.insertRoleOrder(
		t, execution, "old-"+uuid.NewString(), "3343", status, filled, hedgeLeg, role,
	)
	execution = env.pointHedge(t, execution, old.ID, seq)
	return combo, execution, maker, old
}

func TestAggregateCarryReplaceTerminalHedge(t *testing.T) {
	for _, status := range []string{"rejected", "canceled"} {
		t.Run(status, func(t *testing.T) {
			env := openHedgeFastPathRepo(t)
			combo, execution, _, old := env.seedReplaceableHedge(
				t, status, "0", "3343", "0", "b", "hedge", 3,
			)
			if !parseDecimal(combo.CarryBaseQuantity).Equal(parseDecimal("3343")) {
				t.Fatalf("carry=%s", combo.CarryBaseQuantity)
			}
			if !combo.CircuitOpen {
				t.Fatal("expected circuit_open")
			}
			got, err := env.prepareAggregateCarry(
				combo, execution, env.carryHedgeIntent(execution, 3, "3343"), 3,
			)
			if err != nil || !got.Created || got.Execution.HedgeSequence != 4 ||
				got.Execution.HedgeOrderID == "" || got.Execution.HedgeOrderID == old.ID {
				t.Fatalf("replace=%+v err=%v", got, err)
			}
			if env.countHedgeOrders(t) != 2 {
				t.Fatalf("hedge orders=%d", env.countHedgeOrders(t))
			}
			kept, err := env.repo.GetByOwner(env.ctx, "admin", old.ID)
			if err != nil || kept.Status != status || !parseDecimal(kept.FilledQuantity).IsZero() {
				t.Fatalf("old hedge mutated: %+v err=%v", kept, err)
			}
			reloaded := env.reloadCombo(t)
			if !reloaded.CircuitOpen {
				t.Fatalf("circuit_open cleared: %+v", reloaded)
			}
		})
	}
}

func TestAggregateCarryReplaceRejectsNonReplaceableHedge(t *testing.T) {
	cases := []struct {
		name    string
		status  string
		filled  string
		leg     string
		role    string
		fail    int
		pointID string
	}{
		{name: "pending", status: "pending"},
		{name: "open", status: "open"},
		{name: "unknown", status: "unknown"},
		{name: "uncertain", status: "rejected", fail: 1},
		{name: "wrong_leg", status: "rejected", leg: "a"},
		{name: "not_hedge", status: "rejected", role: "maker", leg: "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := openHedgeFastPathRepo(t)
			leg := tc.leg
			if leg == "" {
				leg = "b"
			}
			role := tc.role
			if role == "" {
				role = "hedge"
			}
			combo, execution, maker, old := env.seedReplaceableHedge(
				t, tc.status, tc.filled, "3343", "0", leg, role, 3,
			)
			if tc.fail > 0 {
				if _, err := env.pool.Exec(env.ctx, `
					UPDATE trader_orders SET reconcile_failures=$2 WHERE id=$1::uuid`,
					old.ID, tc.fail); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "not_hedge" {
				execution = env.pointHedge(t, execution, maker.ID, 3)
				old = maker
			}
			_, err := env.prepareAggregateCarry(
				combo, execution, env.carryHedgeIntent(execution, 3, "3343"), 3,
			)
			if !errors.Is(err, ErrArbitrageHedgeAdmissionConflict) {
				t.Fatalf("err=%v", err)
			}
			got, err := env.repo.GetActiveArbitrageExecution(env.ctx, env.combo.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.HedgeSequence != 3 || got.HedgeOrderID != old.ID {
				t.Fatalf("pointer mutated: %+v", got)
			}
			if env.countHedgeOrders(t) != 1 && tc.name != "not_hedge" {
				t.Fatalf("hedge orders=%d", env.countHedgeOrders(t))
			}
		})
	}
}

func TestAggregateCarryReplaceRejectsLiveHedgeBesideTerminalPointer(t *testing.T) {
	for _, liveStatus := range []string{"pending", "open", "partially_filled", "unknown"} {
		t.Run(liveStatus, func(t *testing.T) {
			env := openHedgeFastPathRepo(t)
			combo, execution, _, old := env.seedReplaceableHedge(
				t, "rejected", "0", "3343", "0", "b", "hedge", 3,
			)
			filled := "0"
			if liveStatus == "partially_filled" {
				filled = "1"
			}
			env.insertRoleOrder(
				t, execution, "live-"+uuid.NewString(), "10", liveStatus, filled, "b", "hedge",
			)
			_, err := env.prepareAggregateCarry(
				combo, execution, env.carryHedgeIntent(execution, 3, "3343"), 3,
			)
			if !errors.Is(err, ErrArbitrageHedgeAdmissionConflict) {
				t.Fatalf("err=%v", err)
			}
			got, err := env.repo.GetActiveArbitrageExecution(env.ctx, env.combo.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.HedgeSequence != 3 || got.HedgeOrderID != old.ID {
				t.Fatalf("pointer mutated: %+v", got)
			}
			if env.countHedgeOrders(t) != 2 {
				t.Fatalf("hedge orders=%d", env.countHedgeOrders(t))
			}
		})
	}
}

func TestAggregateCarryReplaceRejectsForeignExecutionHedge(t *testing.T) {
	env := openHedgeFastPathRepo(t)
	stale, _ := env.claimMakerFilled(t, "stale-owner", "0.01")
	foreign := env.insertRoleOrder(
		t, stale, "foreign-"+uuid.NewString(), "3343", "rejected", "0", "b", "hedge",
	)
	stale.Status = "completed"
	if _, err := env.repo.UpdateArbitrageExecution(env.ctx, stale); err != nil {
		t.Fatal(err)
	}
	execution, _ := env.claimMakerFilled(t, "active-owner", "0.01")
	combo := env.setCarryRuntime(t, "3343", "0")
	execution = env.pointHedge(t, execution, foreign.ID, 3)
	_, err := env.prepareAggregateCarry(
		combo, execution, env.carryHedgeIntent(execution, 3, "3343"), 3,
	)
	if !errors.Is(err, ErrArbitrageHedgeAdmissionConflict) {
		t.Fatalf("err=%v", err)
	}
	got, err := env.repo.GetActiveArbitrageExecution(env.ctx, env.combo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HedgeSequence != 3 || got.HedgeOrderID != foreign.ID {
		t.Fatalf("pointer mutated: %+v", got)
	}
}

func TestAggregateCarryReplaceConcurrentSameKey(t *testing.T) {
	env := openHedgeFastPathRepo(t)
	combo, execution, _, old := env.seedReplaceableHedge(
		t, "rejected", "0", "3343", "0", "b", "hedge", 3,
	)
	intent := env.carryHedgeIntent(execution, 3, "3343")
	type outcome struct {
		created bool
		err     error
		exec    ArbitrageExecution
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := env.prepareAggregateCarry(combo, execution, intent, 3)
			results <- outcome{created: got.Created, err: err, exec: got.Execution}
		}()
	}
	wg.Wait()
	close(results)
	created, reused := 0, 0
	for item := range results {
		if item.err != nil {
			t.Fatal(item.err)
		}
		if item.created {
			created++
		} else {
			reused++
		}
		if item.exec.HedgeSequence != 4 || item.exec.HedgeOrderID == old.ID {
			t.Fatalf("execution=%+v", item.exec)
		}
	}
	if created != 1 || reused != 1 {
		t.Fatalf("created=%d reused=%d", created, reused)
	}
	if env.countHedgeOrders(t) != 2 {
		t.Fatalf("hedge orders=%d", env.countHedgeOrders(t))
	}
}

func TestAggregateCarryReplaceConcurrentDifferentKeys(t *testing.T) {
	env := openHedgeFastPathRepo(t)
	combo, execution, _, old := env.seedReplaceableHedge(
		t, "rejected", "0", "3343", "0", "b", "hedge", 3,
	)
	first := env.carryHedgeIntent(execution, 3, "3343")
	second := env.hedgeIntent(execution, "other-key", "3343")
	second.IdempotencyKey = fmt.Sprintf("arb:%s:b:hedge:3-other", execution.ID)
	type outcome struct {
		created bool
		err     error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, intent := range []Order{first, second} {
		wg.Add(1)
		go func(order Order) {
			defer wg.Done()
			got, err := env.prepareAggregateCarry(combo, execution, order, 3)
			results <- outcome{created: got.Created, err: err}
		}(intent)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for item := range results {
		if item.err == nil {
			if !item.created {
				t.Fatal("expected created winner")
			}
			success++
			continue
		}
		if errors.Is(item.err, ErrArbitrageHedgeAdmissionConflict) ||
			errors.Is(item.err, ErrArbitrageHedgeSequence) {
			conflict++
			continue
		}
		t.Fatalf("err=%v", item.err)
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	if env.countHedgeOrders(t) != 2 {
		t.Fatalf("hedge orders=%d", env.countHedgeOrders(t))
	}
	got, err := env.repo.GetActiveArbitrageExecution(env.ctx, env.combo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HedgeSequence != 4 || got.HedgeOrderID == old.ID {
		t.Fatalf("execution=%+v", got)
	}
}

func TestHedgeFastPathTerminalPointerStillAlreadySet(t *testing.T) {
	env := openHedgeFastPathRepo(t)
	combo, execution, maker, old := env.seedReplaceableHedge(
		t, "rejected", "0", "3343", "0", "b", "hedge", 3,
	)
	intent := env.hedgeIntent(execution, "fast-after-terminal", maker.FilledQuantity)
	intent.IdempotencyKey = fmt.Sprintf("arb:%s:b:hedge:3", execution.ID)
	_, err := env.repo.PrepareArbitrageHedgeIntent(env.ctx, PrepareArbitrageHedgeIntentInput{
		Combination: combo, Execution: execution, Order: intent,
		ExpectedSequence:     3,
		HedgeLeg:             env.combo.LegB,
		HedgeSide:            "sell",
		TargetQuantity:       maker.FilledQuantity,
		FastPathAdmission:    true,
		ConfirmedMakerFilled: maker.FilledQuantity,
		ConfirmedHedgeFilled: "0",
	})
	if !errors.Is(err, ErrArbitrageHedgeAdmissionConflict) {
		t.Fatalf("err=%v", err)
	}
	got, err := env.repo.GetActiveArbitrageExecution(env.ctx, env.combo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HedgeSequence != 3 || got.HedgeOrderID != old.ID {
		t.Fatalf("execution=%+v", got)
	}
	if env.countHedgeOrders(t) != 1 {
		t.Fatalf("hedge orders=%d", env.countHedgeOrders(t))
	}
}

func TestAggregateCarryReplacePartialFillRemainingCarry(t *testing.T) {
	env := openHedgeFastPathRepo(t)
	combo, execution, _, old := env.seedReplaceableHedge(
		t, "canceled", "10", "29", "0", "b", "hedge", 3,
	)
	if !parseDecimal(combo.CarryBaseQuantity).Equal(parseDecimal("29")) {
		t.Fatalf("carry=%s", combo.CarryBaseQuantity)
	}
	got, err := env.prepareAggregateCarry(
		combo, execution, env.carryHedgeIntent(execution, 3, "29"), 3,
	)
	if err != nil || !got.Created ||
		!parseDecimal(got.Order.Quantity).Equal(parseDecimal("29")) ||
		got.Execution.HedgeSequence != 4 || got.Execution.HedgeOrderID == old.ID {
		t.Fatalf("replace=%+v err=%v", got, err)
	}
	kept, err := env.repo.GetByOwner(env.ctx, "admin", old.ID)
	if err != nil || kept.Status != "canceled" ||
		!parseDecimal(kept.FilledQuantity).Equal(parseDecimal("10")) {
		t.Fatalf("old hedge mutated: %+v err=%v", kept, err)
	}
}
