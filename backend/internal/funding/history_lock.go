package funding

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const historyLockCleanupTimeout = 3 * time.Second

type instrumentLocker interface {
	TryLock(ctx context.Context, instrumentID int64) (unlock func(), ok bool, err error)
}

type nopHistoryLock struct{}

func (nopHistoryLock) TryLock(context.Context, int64) (func(), bool, error) {
	return func() {}, true, nil
}

type HistoryLock struct {
	pool *pgxpool.Pool
}

func NewHistoryLock(pool *pgxpool.Pool) *HistoryLock {
	return &HistoryLock{pool: pool}
}

func historyLockKey(instrumentID int64) string {
	return fmt.Sprintf("funding-history:%d", instrumentID)
}

func (l *HistoryLock) TryLock(ctx context.Context, instrumentID int64) (func(), bool, error) {
	if instrumentID <= 0 {
		return nil, false, fmt.Errorf("history lock requires instrument id")
	}
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire history lock connection: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, `
		SELECT pg_try_advisory_lock(hashtextextended($1, 0))`,
		historyLockKey(instrumentID),
	).Scan(&locked); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("try history lock: %w", err)
	}
	if !locked {
		conn.Release()
		return nil, false, nil
	}
	held := &heldHistoryLock{conn: conn, instrumentID: instrumentID}
	return held.release, true, nil
}

type heldHistoryLock struct {
	conn         *pgxpool.Conn
	instrumentID int64
	done         bool
}

func (h *heldHistoryLock) release() {
	if h == nil || h.done || h.conn == nil {
		return
	}
	h.done = true
	cleanup, cancel := context.WithTimeout(context.Background(), historyLockCleanupTimeout)
	defer cancel()
	var unlocked bool
	err := h.conn.QueryRow(cleanup, `
		SELECT pg_advisory_unlock(hashtextextended($1, 0))`,
		historyLockKey(h.instrumentID),
	).Scan(&unlocked)
	if err == nil && unlocked {
		h.conn.Release()
		h.conn = nil
		return
	}
	h.destroy()
}

func (h *heldHistoryLock) destroy() {
	if h.conn == nil {
		return
	}
	raw := h.conn.Hijack()
	h.conn = nil
	if raw == nil {
		return
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), historyLockCleanupTimeout)
	defer cancel()
	_ = raw.Close(closeCtx)
}
