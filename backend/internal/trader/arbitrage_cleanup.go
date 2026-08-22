package trader

import (
	"context"
	"log/slog"
	"time"
)

type ArbitrageCleanup struct {
	store      arbitrageStore
	retention  time.Duration
	interval   time.Duration
	batch      int
	maxBatches int
	logger     *slog.Logger
}

func NewArbitrageCleanup(
	store arbitrageStore,
	retention, interval time.Duration,
	batch, maxBatches int,
	logger *slog.Logger,
) *ArbitrageCleanup {
	if logger == nil {
		logger = slog.Default()
	}
	return &ArbitrageCleanup{
		store: store, retention: retention, interval: interval,
		batch: batch, maxBatches: maxBatches, logger: logger,
	}
}

func (c *ArbitrageCleanup) Run(ctx context.Context) {
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

func (c *ArbitrageCleanup) runOnce(ctx context.Context) {
	var total int64
	for index := 0; index < c.maxBatches; index++ {
		deleted, err := c.store.DeleteExpiredArbitrageCombinations(
			ctx, time.Now().UTC().Add(-c.retention), c.batch,
		)
		if err != nil {
			if ctx.Err() == nil {
				c.logger.Error("arbitrage cleanup failed", "error", err)
			}
			return
		}
		total += deleted
		if deleted < int64(c.batch) {
			break
		}
	}
	if total > 0 {
		c.logger.Info("arbitrage cleanup complete", "deleted", total)
	}
}
