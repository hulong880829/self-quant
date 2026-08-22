package trader

import (
	"context"
	"log/slog"
	"time"
)

type TwapCleanup struct {
	store      twapStore
	retention  time.Duration
	interval   time.Duration
	batch      int
	maxBatches int
	logger     *slog.Logger
}

func NewTwapCleanup(
	store twapStore,
	retention time.Duration,
	interval time.Duration,
	batch int,
	maxBatches int,
	logger *slog.Logger,
) *TwapCleanup {
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	if interval <= 0 {
		interval = time.Hour
	}
	if batch <= 0 {
		batch = 100
	}
	if maxBatches <= 0 {
		maxBatches = 10
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &TwapCleanup{
		store: store, retention: retention, interval: interval,
		batch: batch, maxBatches: maxBatches, logger: logger,
	}
}

func (c *TwapCleanup) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		c.runOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *TwapCleanup) runOnce(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-c.retention)
	var total int64
	for i := 0; i < c.maxBatches && ctx.Err() == nil; i++ {
		deleted, err := c.store.DeleteClosedTwaps(ctx, cutoff, c.batch)
		if err != nil {
			c.logger.Error("cleanup closed twaps failed", "error", err)
			return
		}
		total += deleted
		if deleted < int64(c.batch) {
			break
		}
	}
	if total > 0 {
		c.logger.Info("closed twaps cleaned", "count", total)
	}
}
