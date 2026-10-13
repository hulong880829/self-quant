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

func TestArbitrageBatchRepositoryIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("trader_batch_repo_%d", time.Now().UnixNano())
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

	accountA, instrumentA := insertArbitrageFixture(t, ctx, pool, "binance", "BATCHA")
	accountB, instrumentB := insertArbitrageFixture(t, ctx, pool, "okx", "BATCHB")
	input := integrationArbitrageCombination(accountA, instrumentA, accountB, instrumentB)
	input.IdempotencyKey = "arb-batch-repo"
	input.RequestFingerprint = "arb-batch-repo-fp"
	created, inserted, err := repository.CreateArbitrageCombination(ctx, input)
	if err != nil || !inserted {
		t.Fatalf("create inserted=%v err=%v", inserted, err)
	}
	missingID := uuid.NewString()

	t.Run("control summaries three states", func(t *testing.T) {
		batch, err := repository.ListArbitrageControlSummaries(ctx, []string{
			created.ID, missingID, "not-a-uuid",
		})
		if err != nil {
			t.Fatal(err)
		}
		summary, ok := batch.Found[created.ID]
		if !ok || summary.Status != "running" {
			t.Fatalf("found=%v missing=%v", batch.Found, batch.Missing)
		}
		if len(batch.Missing) != 2 {
			t.Fatalf("missing=%v", batch.Missing)
		}
	})

	t.Run("renew leases partial success", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations
			SET scheduler_lease_until=now()+interval '1 minute'
			WHERE id=$1::uuid`, created.ID); err != nil {
			t.Fatal(err)
		}
		renewed, err := repository.RenewArbitrageLeases(
			ctx, []string{created.ID, missingID}, 10*time.Second,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !renewed[created.ID] || renewed[missingID] {
			t.Fatalf("renewed=%v", renewed)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations
			SET scheduler_lease_until=now()-interval '1 second'
			WHERE id=$1::uuid`, created.ID); err != nil {
			t.Fatal(err)
		}
		lost, err := repository.RenewArbitrageLeases(
			ctx, []string{created.ID}, 10*time.Second,
		)
		if err != nil {
			t.Fatal(err)
		}
		if lost[created.ID] {
			t.Fatalf("expired lease should not renew: %v", lost)
		}
	})

	t.Run("steady snapshot skips version and updated_at mismatch", func(t *testing.T) {
		loaded, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations
			SET market_data_stale=FALSE
			WHERE id=$1::uuid`, loaded.ID); err != nil {
			t.Fatal(err)
		}
		loaded, err = repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
		if err != nil {
			t.Fatal(err)
		}
		applied, err := repository.BatchUpdateArbitrageMarketSnapshots(ctx, []arbitrageMarketSnapshotWrite{{
			CombinationID:     loaded.ID,
			AskSpread:         "12",
			BidSpread:         "-8",
			ExpectedVersion:   loaded.Version,
			ExpectedUpdatedAt: loaded.UpdatedAt,
			Sequence:          1,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if len(applied) != 1 || !applied[0].Applied || applied[0].RuntimeState != "monitoring" {
			t.Fatalf("applied=%+v", applied)
		}
		skipped, err := repository.BatchUpdateArbitrageMarketSnapshots(ctx, []arbitrageMarketSnapshotWrite{{
			CombinationID:     loaded.ID,
			AskSpread:         "99",
			BidSpread:         "-9",
			ExpectedVersion:   loaded.Version,
			ExpectedUpdatedAt: loaded.UpdatedAt,
			Sequence:          2,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if len(skipped) != 1 || skipped[0].Applied || skipped[0].AskSpread != "12" {
			t.Fatalf("skipped=%+v", skipped)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations
			SET runtime_state='closing',status='closing'
			WHERE id=$1::uuid`, loaded.ID); err != nil {
			t.Fatal(err)
		}
		current, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
		if err != nil {
			t.Fatal(err)
		}
		closing, err := repository.BatchUpdateArbitrageMarketSnapshots(ctx, []arbitrageMarketSnapshotWrite{{
			CombinationID:     current.ID,
			AskSpread:         "1",
			BidSpread:         "-1",
			ExpectedVersion:   current.Version,
			ExpectedUpdatedAt: current.UpdatedAt,
			Sequence:          3,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if len(closing) != 1 || closing[0].Applied || closing[0].Status != "closing" ||
			closing[0].RuntimeState != "closing" {
			t.Fatalf("closing skip=%+v", closing)
		}
	})

	t.Run("idle runtime states recover and protected states skip", func(t *testing.T) {
		resetRunning := func(runtimeState string) ArbitrageCombination {
			t.Helper()
			if _, err := pool.Exec(ctx, `
				UPDATE trader_arbitrage_combinations SET
					status='running',runtime_state=$2,market_data_stale=FALSE,
					position_uncertain=FALSE,error_message='',
					current_ask_spread_bps=1,current_bid_spread_bps=-1
				WHERE id=$1::uuid`, created.ID, runtimeState); err != nil {
				t.Fatal(err)
			}
			loaded, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
			if err != nil {
				t.Fatal(err)
			}
			return loaded
		}
		write := func(loaded ArbitrageCombination, ask, bid string, seq uint64) []arbitrageMarketSnapshotBatchItem {
			t.Helper()
			items, err := repository.BatchUpdateArbitrageMarketSnapshots(ctx, []arbitrageMarketSnapshotWrite{{
				CombinationID:     loaded.ID,
				AskSpread:         ask,
				BidSpread:         bid,
				ExpectedVersion:   loaded.Version,
				ExpectedUpdatedAt: loaded.UpdatedAt,
				Sequence:          seq,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 {
				t.Fatalf("items=%+v", items)
			}
			return items
		}

		seq := uint64(10)
		for _, state := range []string{"opportunity_gone", "repricing"} {
			loaded := resetRunning(state)
			version := loaded.Version
			items := write(loaded, "15", "-6", seq)
			seq++
			if !items[0].Applied || items[0].RuntimeState != "monitoring" ||
				items[0].AskSpread != "15" || items[0].BidSpread != "-6" ||
				items[0].Version != version {
				t.Fatalf("%s applied=%+v", state, items[0])
			}
			current, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.RuntimeState != "monitoring" || current.Version != version ||
				current.CurrentAskSpreadBps != "15" || current.CurrentBidSpreadBps != "-6" {
				t.Fatalf("%s persisted=%+v", state, current)
			}
		}

		for _, state := range []string{
			"maker_open", "maker_canceling", "hedging", "reconciling",
			"backoff", "hedge_deferred_dust", "position_uncertain", "manual_intervention",
		} {
			loaded := resetRunning(state)
			items := write(loaded, "20", "-3", seq)
			seq++
			if items[0].Applied || items[0].RuntimeState != state {
				t.Fatalf("%s skip=%+v", state, items[0])
			}
			if items[0].AskSpread == "20" || items[0].BidSpread == "-3" {
				t.Fatalf("%s spreads overwritten=%+v", state, items[0])
			}
		}

		if _, err := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations SET
				status='closing',runtime_state='closing',market_data_stale=FALSE,
				current_ask_spread_bps=1,current_bid_spread_bps=-1
			WHERE id=$1::uuid`, created.ID); err != nil {
			t.Fatal(err)
		}
		closing, err := repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
		if err != nil {
			t.Fatal(err)
		}
		items := write(closing, "21", "-2", seq)
		seq++
		if items[0].Applied || items[0].Status != "closing" || items[0].RuntimeState != "closing" ||
			items[0].AskSpread != "1" {
			t.Fatalf("closing skip=%+v", items[0])
		}

		loaded := resetRunning("opportunity_gone")
		execution, claimed, err := repository.ClaimArbitrageExecution(
			ctx, integrationArbitrageExecution(created.ID, "ask"),
		)
		if err != nil || !claimed {
			t.Fatalf("claim claimed=%v err=%v", claimed, err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE trader_arbitrage_combinations
			SET runtime_state='opportunity_gone',market_data_stale=FALSE,
			    current_ask_spread_bps=1,current_bid_spread_bps=-1
			WHERE id=$1::uuid`, created.ID); err != nil {
			t.Fatal(err)
		}
		loaded, err = repository.GetArbitrageCombinationByOwner(ctx, "admin", created.ID)
		if err != nil {
			t.Fatal(err)
		}
		items = write(loaded, "22", "-4", seq)
		if items[0].Applied || items[0].RuntimeState != "opportunity_gone" ||
			items[0].AskSpread != "1" {
			t.Fatalf("live execution skip=%+v execution=%s", items[0], execution.ID)
		}
	})
}
