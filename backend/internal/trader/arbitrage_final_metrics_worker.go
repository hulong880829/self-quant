package trader

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
)

type finalMetricsTaskStore interface {
	ClaimDueFinalMetrics(context.Context, int) ([]ArbitrageCombination, error)
	PersistFinalArbitragePositionMetrics(
		context.Context, string, int64, bool, time.Time,
	) (ArbitrageCombination, error)
	RecordFinalMetricsRetry(
		context.Context, string, int64, time.Time, string,
	) (ArbitrageCombination, bool, error)
	ReleaseFinalMetricsClaim(context.Context, string, int64, time.Time) (bool, error)
	ListMissingArbitrageOrderAverages(context.Context, []string) ([]missingArbitrageAverageOrder, error)
}

type ArbitrageFinalMetricsWorker struct {
	store       finalMetricsTaskStore
	compensator arbitrageFillAverageCompensator
	logger      *slog.Logger
	batch       int
}

func NewArbitrageFinalMetricsWorker(
	store finalMetricsTaskStore,
	compensator arbitrageFillAverageCompensator,
	logger *slog.Logger,
) *ArbitrageFinalMetricsWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &ArbitrageFinalMetricsWorker{
		store: store, compensator: compensator, logger: logger,
		batch: finalMetricsBatchSize,
	}
}

func (w *ArbitrageFinalMetricsWorker) Run(ctx context.Context) {
	if w == nil || w.store == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runOnce(ctx)
		}
	}
}

func (w *ArbitrageFinalMetricsWorker) runOnce(ctx context.Context) {
	if w == nil || w.store == nil || ctx.Err() != nil {
		return
	}
	claimed, err := w.store.ClaimDueFinalMetrics(ctx, w.batch)
	if err != nil {
		if ctx.Err() == nil {
			w.logger.Error("claim final metrics failed", "error", err)
		}
		return
	}
	for _, item := range claimed {
		if ctx.Err() != nil {
			return
		}
		w.process(ctx, item)
	}
}

func (w *ArbitrageFinalMetricsWorker) process(ctx context.Context, item ArbitrageCombination) {
	if item.FinalMetricsStatus == finalMetricsCompleted || !item.FinalMetricsRetryFinite {
		return
	}
	if !combinationLooksFlat(item) && !strings.EqualFold(item.Status, "closing") {
		return
	}
	if w.compensator != nil {
		w.compensator.Compensate(ctx, []string{item.ID})
	}
	if strings.EqualFold(item.Status, "closing") {
		w.finishClosingCompensation(ctx, item)
		return
	}
	if _, err := w.store.PersistFinalArbitragePositionMetrics(
		ctx, item.ID, item.Version, false, time.Time{},
	); err != nil {
		if ctx.Err() != nil {
			return
		}
		w.recordRetry(ctx, item, err)
	}
}

func (w *ArbitrageFinalMetricsWorker) finishClosingCompensation(
	ctx context.Context,
	item ArbitrageCombination,
) {
	missing, err := w.store.ListMissingArbitrageOrderAverages(ctx, []string{item.ID})
	if err != nil {
		if ctx.Err() == nil {
			w.recordRetry(ctx, item, err)
		}
		return
	}
	if len(missing) == 0 {
		if _, releaseErr := w.store.ReleaseFinalMetricsClaim(
			ctx, item.ID, item.Version, item.FinalMetricsNextRetryAt,
		); releaseErr != nil && ctx.Err() == nil {
			w.logger.Error("release final metrics claim failed",
				"combination_id", item.ID, "error", releaseErr)
		}
		return
	}
	w.recordRetry(ctx, item, errors.New("missing fill averages"))
}

func (w *ArbitrageFinalMetricsWorker) recordRetry(
	ctx context.Context, item ArbitrageCombination, cause error,
) {
	updated, applied, err := w.store.RecordFinalMetricsRetry(
		ctx, item.ID, item.Version, item.FinalMetricsNextRetryAt, sanitizeError(cause),
	)
	if err != nil {
		if ctx.Err() == nil {
			w.logger.Error("record final metrics retry failed",
				"combination_id", item.ID, "error", err)
		}
		return
	}
	if !applied {
		return
	}
	if updated.FinalMetricsRetryCount >= 6 {
		w.logger.Warn("final metrics still incomplete after close",
			"combination_id", item.ID,
			"retry_count", updated.FinalMetricsRetryCount,
			"error", updated.FinalMetricsLastError)
	}
}
