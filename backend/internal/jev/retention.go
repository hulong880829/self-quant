package jev

import (
	"context"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"
)

const (
	snapshotRetention = 24 * time.Hour
	dataRetention     = 30 * 24 * time.Hour
	cleanupInterval   = time.Hour
	cleanupBatchSize  = 1000
	cleanupMaxBatches = 10
)

type RetentionStore interface {
	DeleteExpiredStoppedRuns(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	ArchiveAndDeleteClosedPositions(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	DeleteExpiredTerminalDecisions(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	DeleteExpiredEquityPoints(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	DeleteExpiredSnapshotsKeepingLatest(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}

type cycleLocker interface {
	AcquireCycleLock(context.Context) (CycleLease, bool, error)
}

type RetentionWorker struct {
	store  RetentionStore
	lock   cycleLocker
	now    func() time.Time
	logger *slog.Logger
}

func NewRetentionWorker(store RetentionStore, lock cycleLocker, logger *slog.Logger) *RetentionWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &RetentionWorker{
		store: store, lock: lock, now: func() time.Time { return time.Now().UTC() }, logger: logger,
	}
}

func (w *RetentionWorker) RunLoop(ctx context.Context) {
	if err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
		w.logger.Error("jev retention failed", "error", err)
	}
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
				w.logger.Error("jev retention failed", "error", err)
			}
		}
	}
}

func (w *RetentionWorker) RunOnce(ctx context.Context) error {
	now := w.now().UTC()
	dataCutoff := now.Add(-dataRetention)
	snapCutoff := now.Add(-snapshotRetention)
	if err := w.runBatches(ctx, func(limit int) (int64, error) {
		return w.store.DeleteExpiredStoppedRuns(ctx, dataCutoff, limit)
	}); err != nil {
		return err
	}
	if err := w.archiveClosedPositions(ctx, dataCutoff); err != nil {
		return err
	}
	if err := w.runBatches(ctx, func(limit int) (int64, error) {
		return w.store.DeleteExpiredTerminalDecisions(ctx, dataCutoff, limit)
	}); err != nil {
		return err
	}
	if err := w.runBatches(ctx, func(limit int) (int64, error) {
		return w.store.DeleteExpiredEquityPoints(ctx, dataCutoff, limit)
	}); err != nil {
		return err
	}
	return w.runBatches(ctx, func(limit int) (int64, error) {
		return w.store.DeleteExpiredSnapshotsKeepingLatest(ctx, snapCutoff, limit)
	})
}

func (w *RetentionWorker) archiveClosedPositions(ctx context.Context, cutoff time.Time) error {
	if w.lock == nil {
		return w.runBatches(ctx, func(limit int) (int64, error) {
			return w.store.ArchiveAndDeleteClosedPositions(ctx, cutoff, limit)
		})
	}
	lease, locked, err := w.lock.AcquireCycleLock(ctx)
	if err != nil {
		return err
	}
	if !locked {
		w.logger.Info("jev retention skipped position archive: lock_busy")
		return nil
	}
	defer func() {
		if releaseErr := lease.Release(); releaseErr != nil {
			w.logger.Error("jev retention cycle lock release failed", "error", releaseErr)
		}
	}()
	return w.runBatches(ctx, func(limit int) (int64, error) {
		return w.store.ArchiveAndDeleteClosedPositions(ctx, cutoff, limit)
	})
}

func (w *RetentionWorker) runBatches(ctx context.Context, fn func(int) (int64, error)) error {
	for i := 0; i < cleanupMaxBatches; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := fn(cleanupBatchSize)
		if err != nil {
			return err
		}
		if n < int64(cleanupBatchSize) {
			return nil
		}
	}
	return nil
}

func (r *Repository) DeleteExpiredStoppedRuns(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	tag, err := r.pool.Exec(ctx, `
DELETE FROM jev_arb_runs
WHERE id IN (
	SELECT id FROM jev_arb_runs
	WHERE status = 'stopped' AND updated_at < $1
	ORDER BY updated_at, id
	LIMIT $2
)`, cutoff.UTC(), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) ArchiveAndDeleteClosedPositions(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
SELECT id, run_id, realized_spread_pnl::text, funding_pnl::text, cost_paid::text
FROM jev_arb_positions
WHERE status = 'closed' AND closed_at IS NOT NULL AND closed_at < $1
ORDER BY closed_at, id
LIMIT $2
FOR UPDATE`, cutoff.UTC(), limit)
	if err != nil {
		return 0, err
	}
	type totals struct {
		realized decimal.Decimal
		funding  decimal.Decimal
		cost     decimal.Decimal
		count    int64
	}
	ids := make([]int64, 0, limit)
	byRun := map[int64]*totals{}
	for rows.Next() {
		var id, runID int64
		var realized, funding, cost string
		if err := rows.Scan(&id, &runID, &realized, &funding, &cost); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		item := byRun[runID]
		if item == nil {
			item = &totals{}
			byRun[runID] = item
		}
		item.realized = item.realized.Add(parseDecimalString(realized))
		item.funding = item.funding.Add(parseDecimalString(funding))
		item.cost = item.cost.Add(parseDecimalString(cost))
		item.count++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	for runID, item := range byRun {
		if _, err := tx.Exec(ctx, `
UPDATE jev_arb_runs SET
	archived_realized_spread_pnl = archived_realized_spread_pnl + $2,
	archived_funding_pnl = archived_funding_pnl + $3,
	archived_cost_paid = archived_cost_paid + $4,
	archived_position_count = archived_position_count + $5,
	updated_at = now()
WHERE id = $1`,
			runID, dec(item.realized), dec(item.funding), dec(item.cost), item.count,
		); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM jev_arb_positions WHERE id = ANY($1)`, ids); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int64(len(ids)), nil
}

func (r *Repository) DeleteExpiredTerminalDecisions(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	tag, err := r.pool.Exec(ctx, `
DELETE FROM jev_arb_decisions d
WHERE d.id IN (
	SELECT d2.id
	FROM jev_arb_decisions d2
	WHERE d2.execution_status <> 'pending'
	  AND d2.decision_at < $1
	  AND NOT EXISTS (
		SELECT 1
		FROM jev_arb_positions p
		WHERE p.id = d2.position_id
		  AND p.status = 'open'
	  )
	ORDER BY d2.decision_at, d2.id
	LIMIT $2
)`, cutoff.UTC(), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) DeleteExpiredEquityPoints(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	tag, err := r.pool.Exec(ctx, `
DELETE FROM jev_arb_equity_points
WHERE id IN (
	SELECT id FROM jev_arb_equity_points
	WHERE ts < $1
	ORDER BY ts, id
	LIMIT $2
)`, cutoff.UTC(), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) DeleteExpiredSnapshotsKeepingLatest(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	tag, err := r.pool.Exec(ctx, `
DELETE FROM jev_arb_snapshots s
WHERE s.id IN (
	SELECT s2.id
	FROM jev_arb_snapshots s2
	WHERE s2.created_at < $1
	  AND s2.id <> (
		SELECT MAX(s3.id) FROM jev_arb_snapshots s3 WHERE s3.run_id = s2.run_id
	  )
	ORDER BY s2.created_at, s2.id
	LIMIT $2
)`, cutoff.UTC(), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
