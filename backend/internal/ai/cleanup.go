package ai

import (
	"context"
	"log/slog"
	"time"
)

type expiredConversationStore interface {
	DeleteExpired(context.Context, int) (int64, error)
}

type CleanupWorker struct {
	store      expiredConversationStore
	interval   time.Duration
	batchSize  int
	maxBatches int
	logger     *slog.Logger
}

func NewCleanupWorker(
	store expiredConversationStore,
	interval time.Duration,
	batchSize int,
	maxBatches int,
	logger *slog.Logger,
) *CleanupWorker {
	return &CleanupWorker{
		store: store, interval: interval, batchSize: batchSize,
		maxBatches: maxBatches, logger: logger,
	}
}

func (w *CleanupWorker) Run(ctx context.Context) {
	w.cleanup(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.cleanup(ctx)
		}
	}
}

func (w *CleanupWorker) cleanup(ctx context.Context) {
	var deleted int64
	for batch := 0; batch < w.maxBatches; batch++ {
		count, err := w.store.DeleteExpired(ctx, w.batchSize)
		if err != nil {
			w.logger.Error("ai conversation cleanup failed", "error", err)
			return
		}
		deleted += count
		if count < int64(w.batchSize) {
			break
		}
	}
	if deleted > 0 {
		w.logger.Info("expired ai conversations deleted", "count", deleted)
	}
}
