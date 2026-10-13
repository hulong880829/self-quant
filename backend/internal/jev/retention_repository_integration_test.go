package jev

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"selfquant/backend/internal/database"
)

func TestRetentionRepositoryPostgresIntegration(t *testing.T) {
	ctx, repo := openIsolatedJevRepo(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	run, err := repo.EnsureActiveRun(ctx, now, decimal.NewFromInt(10000), StrategyVersion)
	if err != nil {
		t.Fatal(err)
	}

	oldClosedAt := now.Add(-31 * 24 * time.Hour)
	closed := mustInsertPosition(t, ctx, repo, fixturePosition(run.ID, PositionClosed, oldClosedAt))
	closed.RealizedSpreadPnL = decimal.NewFromInt(5)
	closed.FundingPnL = decimal.NewFromInt(2)
	closed.CostPaid = decimal.NewFromInt(1)
	closed.UnrealizedSpreadPnL = decimal.Zero
	closed.ClosedAt = &oldClosedAt
	if err := repo.SavePosition(ctx, closed); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.InsertTrade(ctx, Trade{
		RunID: run.ID, PositionID: closed.ID, Side: DecisionEnter,
		SharedBaseQty: decimal.NewFromInt(1), LongFillPrice: decimal.NewFromInt(10),
		ShortFillPrice: decimal.NewFromInt(10), Notional: decimal.NewFromInt(10),
		Cost: decimal.RequireFromString("0.1"), FilledAt: oldClosedAt,
	}); err != nil {
		t.Fatal(err)
	}
	openPos := mustInsertPosition(t, ctx, repo, fixturePosition(run.ID, PositionOpen, now.Add(-time.Hour)))
	openPos.RealizedSpreadPnL = decimal.NewFromInt(3)
	openPos.FundingPnL = decimal.NewFromInt(1)
	openPos.CostPaid = decimal.NewFromInt(1)
	if err := repo.SavePosition(ctx, openPos); err != nil {
		t.Fatal(err)
	}

	positions, err := repo.ListAllPositions(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := RecomputeRunTotals(run, positions)
	if err := repo.SaveRun(ctx, before); err != nil {
		t.Fatal(err)
	}

	pendingAt := now.Add(-40 * 24 * time.Hour)
	keepPending := mustInsertDecision(t, ctx, repo, Decision{
		RunID: run.ID, CandidateKey: "pending", Kind: KindSingle, Decision: DecisionEnter,
		PolicyStatus: PolicyAccepted, ExecutionStatus: ExecPending,
		PromptVersion: PromptVersion, PolicyVersion: PolicyVersion, DecisionAt: pendingAt,
	})
	keepOpenLinked := mustInsertDecision(t, ctx, repo, Decision{
		RunID: run.ID, PositionID: ptrInt(openPos.ID), CandidateKey: "open-linked", Kind: KindSingle,
		Decision: DecisionEnter, PolicyStatus: PolicyAccepted, ExecutionStatus: ExecFilled,
		PromptVersion: PromptVersion, PolicyVersion: PolicyVersion, DecisionAt: pendingAt,
	})
	dropOrphan := mustInsertDecision(t, ctx, repo, Decision{
		RunID: run.ID, CandidateKey: "orphan", Kind: KindSingle, Decision: DecisionHold,
		PolicyStatus: PolicyAccepted, ExecutionStatus: ExecNotApplicable,
		PromptVersion: PromptVersion, PolicyVersion: PolicyVersion, DecisionAt: pendingAt,
	})

	oldEquityTS := now.Add(-40 * 24 * time.Hour)
	if err := repo.InsertEquityPoint(ctx, run.ID, EquityPoint{
		TS: oldEquityTS, Equity: decimal.NewFromInt(10000), NetPnL: decimal.Zero,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.InsertEquityPoint(ctx, run.ID, EquityPoint{
		TS: now, Equity: decimal.NewFromInt(10009), NetPnL: decimal.NewFromInt(9),
	}); err != nil {
		t.Fatal(err)
	}

	oldSnap, err := repo.PublishSnapshot(ctx, run.ID, Snapshot{Status: StatusRunning, ServerTime: now.Add(-48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	latestSnap, err := repo.PublishSnapshot(ctx, run.ID, Snapshot{Status: StatusRunning, ServerTime: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE jev_arb_snapshots SET created_at=$1 WHERE run_id=$2 AND snapshot_version=$3`,
		now.Add(-48*time.Hour), run.ID, oldSnap.Version); err != nil {
		t.Fatal(err)
	}

	stopped, err := repo.EnsureActiveRun(ctx, now, decimal.NewFromInt(1), StrategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.ID != run.ID {
		t.Fatal("expected same active run")
	}
	stopped.Status = "stopped"
	if err := repo.SaveRun(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE jev_arb_runs SET updated_at=$1 WHERE id=$2`, now.Add(-40*24*time.Hour), stopped.ID); err != nil {
		t.Fatal(err)
	}
	active, err := repo.EnsureActiveRun(ctx, now, decimal.NewFromInt(10000), StrategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if active.ID == stopped.ID {
		t.Fatal("new active run should differ from stopped run")
	}

	worker := NewRetentionWorker(repo, repo, slog.Default())
	worker.now = func() time.Time { return now }
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.loadRun(ctx, stopped.ID); err == nil {
		t.Fatal("expired stopped run must be deleted")
	}
	stillActive, err := repo.ActiveRun(ctx, StrategyVersion)
	if err != nil || stillActive == nil || stillActive.ID != active.ID {
		t.Fatalf("active run deleted: %+v %v", stillActive, err)
	}

	remaining, err := repo.ListAllPositions(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("stopped run cascade should remove its positions, got=%d", len(remaining))
	}

	activeOpen := mustInsertPosition(t, ctx, repo, fixturePosition(active.ID, PositionOpen, now.Add(-time.Hour)))
	activeClosed := mustInsertPosition(t, ctx, repo, fixturePosition(active.ID, PositionClosed, oldClosedAt))
	activeClosed.RealizedSpreadPnL = decimal.NewFromInt(5)
	activeClosed.FundingPnL = decimal.NewFromInt(2)
	activeClosed.CostPaid = decimal.NewFromInt(1)
	activeClosed.ClosedAt = &oldClosedAt
	if err := repo.SavePosition(ctx, activeClosed); err != nil {
		t.Fatal(err)
	}
	activeOpen.RealizedSpreadPnL = decimal.NewFromInt(3)
	activeOpen.FundingPnL = decimal.NewFromInt(1)
	activeOpen.CostPaid = decimal.NewFromInt(1)
	if err := repo.SavePosition(ctx, activeOpen); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.InsertTrade(ctx, Trade{
		RunID: active.ID, PositionID: activeClosed.ID, Side: DecisionExit,
		SharedBaseQty: decimal.NewFromInt(1), LongFillPrice: decimal.NewFromInt(11),
		ShortFillPrice: decimal.NewFromInt(11), Notional: decimal.NewFromInt(11),
		Cost: decimal.RequireFromString("0.1"), FilledAt: oldClosedAt,
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := repo.loadRun(ctx, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	all, err := repo.ListAllPositions(ctx, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	totalsBefore := RecomputeRunTotals(loaded, all)
	if err := repo.SaveRun(ctx, totalsBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ArchiveAndDeleteClosedPositions(ctx, now.Add(-dataRetention), cleanupBatchSize); err != nil {
		t.Fatal(err)
	}
	afterRun, err := repo.loadRun(ctx, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterPositions, err := repo.ListAllPositions(ctx, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterPositions) != 1 || afterPositions[0].Status != PositionOpen {
		t.Fatalf("open position must remain %+v", afterPositions)
	}
	totalsAfter := RecomputeRunTotals(afterRun, afterPositions)
	if !totalsAfter.Equity.Equal(totalsBefore.Equity) || !totalsAfter.NetPnL.Equal(totalsBefore.NetPnL) ||
		!totalsAfter.FundingPnL.Equal(totalsBefore.FundingPnL) || !totalsAfter.CumulativeCost.Equal(totalsBefore.CumulativeCost) ||
		!totalsAfter.RealizedSpreadPnL.Equal(totalsBefore.RealizedSpreadPnL) {
		t.Fatalf("totals changed before=%+v after=%+v archived=%s/%s/%s", totalsBefore, totalsAfter,
			afterRun.ArchivedRealizedSpreadPnL, afterRun.ArchivedFundingPnL, afterRun.ArchivedCostPaid)
	}
	trades, _, err := repo.ListTradesPage(ctx, active.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 0 {
		t.Fatalf("closed position trades must cascade, got=%d", len(trades))
	}

	keepPending.RunID = active.ID
	keepOpenLinked.RunID = active.ID
	keepOpenLinked.PositionID = ptrInt(activeOpen.ID)
	dropOrphan.RunID = active.ID
	keepPending = mustInsertDecision(t, ctx, repo, keepPending)
	keepOpenLinked = mustInsertDecision(t, ctx, repo, keepOpenLinked)
	dropOrphan = mustInsertDecision(t, ctx, repo, dropOrphan)
	if _, err := repo.DeleteExpiredTerminalDecisions(ctx, now.Add(-dataRetention), cleanupBatchSize); err != nil {
		t.Fatal(err)
	}
	page, _, err := repo.ListDecisionsPage(ctx, active.ID, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[int64]bool{}
	for _, item := range page {
		found[item.ID] = true
	}
	if !found[keepPending.ID] || !found[keepOpenLinked.ID] {
		t.Fatalf("protected decisions deleted %+v", found)
	}
	if found[dropOrphan.ID] {
		t.Fatal("orphan terminal decision should be deleted")
	}

	if err := repo.InsertEquityPoint(ctx, active.ID, EquityPoint{TS: oldEquityTS, Equity: decimal.NewFromInt(1), NetPnL: decimal.Zero}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DeleteExpiredEquityPoints(ctx, now.Add(-dataRetention), cleanupBatchSize); err != nil {
		t.Fatal(err)
	}
	points, err := repo.ListEquityPoints(ctx, active.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range points {
		if point.TS.Equal(oldEquityTS) {
			t.Fatal("old equity point should be deleted")
		}
	}

	oldS, err := repo.PublishSnapshot(ctx, active.ID, Snapshot{Status: StatusRunning, ServerTime: now.Add(-48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	newS, err := repo.PublishSnapshot(ctx, active.ID, Snapshot{Status: StatusRunning, ServerTime: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE jev_arb_snapshots SET created_at=$1 WHERE run_id=$2 AND snapshot_version=$3`,
		now.Add(-48*time.Hour), active.ID, oldS.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DeleteExpiredSnapshotsKeepingLatest(ctx, now.Add(-snapshotRetention), cleanupBatchSize); err != nil {
		t.Fatal(err)
	}
	latest, err := repo.LatestSnapshot(ctx, active.ID)
	if err != nil || latest == nil || latest.Version != newS.Version {
		t.Fatalf("latest snapshot=%+v err=%v want=%d", latest, err, newS.Version)
	}
	var snapCount int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM jev_arb_snapshots WHERE run_id=$1`, active.ID).Scan(&snapCount); err != nil {
		t.Fatal(err)
	}
	if snapCount != 1 {
		t.Fatalf("expected only latest snapshot, got=%d", snapCount)
	}

	_ = keepPending
	_ = latestSnap
}

func TestRetentionLockBusySkipsPositionsOnlyIntegration(t *testing.T) {
	ctx, repo := openIsolatedJevRepo(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	run, err := repo.EnsureActiveRun(ctx, now, decimal.NewFromInt(10000), StrategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	closedAt := now.Add(-31 * 24 * time.Hour)
	closed := mustInsertPosition(t, ctx, repo, fixturePosition(run.ID, PositionClosed, closedAt))
	closed.ClosedAt = &closedAt
	closed.RealizedSpreadPnL = decimal.NewFromInt(4)
	if err := repo.SavePosition(ctx, closed); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PublishSnapshot(ctx, run.ID, Snapshot{Status: StatusRunning, ServerTime: now.Add(-48 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	latest, err := repo.PublishSnapshot(ctx, run.ID, Snapshot{Status: StatusRunning, ServerTime: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE jev_arb_snapshots SET created_at=created_at - interval '48 hours' WHERE snapshot_version=$1`, 1); err != nil {
		t.Fatal(err)
	}

	lease, locked, err := repo.AcquireCycleLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Skip("cycle lock is held by another process")
	}
	defer func() { _ = lease.Release() }()

	worker := NewRetentionWorker(repo, repo, slog.Default())
	worker.now = func() time.Time { return now }
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	positions, err := repo.ListAllPositions(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 1 {
		t.Fatalf("lock busy must keep closed position, got=%d", len(positions))
	}
	gotLatest, err := repo.LatestSnapshot(ctx, run.ID)
	if err != nil || gotLatest == nil || gotLatest.Version != latest.Version {
		t.Fatalf("snapshots should still clean %+v", gotLatest)
	}
}

func openIsolatedJevRepo(t *testing.T) (context.Context, *Repository) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("jev_retention_test_%d", time.Now().UnixNano())
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
	return ctx, NewRepository(pool)
}

func fixturePosition(runID int64, status string, opened time.Time) Position {
	return Position{
		RunID: runID, Kind: KindSingle, Status: status, CandidateKey: "single:binance:BTCUSDT:BTCUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", GlobalSymbol: "BTCUSDT",
		LongExchange: "binance", LongSymbol: "BTCUSDT", LongProduct: ProductSpot,
		LongCanonicalSymbol: "BTCUSDT", LongQuoteAsset: "USDT",
		ShortExchange: "binance", ShortSymbol: "BTCUSDT", ShortProduct: ProductPerpetual,
		ShortCanonicalSymbol: "BTCUSDT", ShortQuoteAsset: "USDT",
		SharedBaseQty: decimal.NewFromInt(1), LongQty: decimal.NewFromInt(1), ShortQty: decimal.NewFromInt(1).Neg(),
		LongEntryPrice: decimal.NewFromInt(100), ShortEntryPrice: decimal.NewFromInt(100),
		OpenedAt: opened,
	}
}

func mustInsertPosition(t *testing.T, ctx context.Context, repo *Repository, position Position) Position {
	t.Helper()
	saved, err := repo.InsertPosition(ctx, position)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func mustInsertDecision(t *testing.T, ctx context.Context, repo *Repository, decision Decision) Decision {
	t.Helper()
	saved, err := repo.InsertDecision(ctx, decision)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}
