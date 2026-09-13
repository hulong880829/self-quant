package trader

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

type funding8hExitStore interface {
	ListOneShotFunding8hExitRows(context.Context, time.Time, time.Time) ([]funding8hExitRow, error)
	MarkOneShotExiting(context.Context, string, int64, string, map[string]any) (ArbitrageCombination, bool, error)
}

type ArbitrageFunding8hExitWorker struct {
	store        funding8hExitStore
	interval     time.Duration
	queryTimeout time.Duration
	now          func() time.Time
	inFlight     atomic.Bool
	logger       *slog.Logger
}

func NewArbitrageFunding8hExitWorker(
	store funding8hExitStore,
	logger *slog.Logger,
) *ArbitrageFunding8hExitWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &ArbitrageFunding8hExitWorker{
		store:        store,
		interval:     funding8hExitInterval,
		queryTimeout: funding8hExitQueryTimeout,
		now:          func() time.Time { return time.Now().UTC() },
		logger:       logger,
	}
}

func (w *ArbitrageFunding8hExitWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

func (w *ArbitrageFunding8hExitWorker) tick(ctx context.Context) {
	if !w.inFlight.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer w.inFlight.Store(false)
		defer func() {
			if recovered := recover(); recovered != nil {
				w.logger.Error("one-shot 8h funding exit panic", "panic", recovered)
			}
		}()
		w.runOnce(ctx)
	}()
}

func (w *ArbitrageFunding8hExitWorker) runOnce(ctx context.Context) {
	now := w.now()
	windowStart, windowEnd := funding8hWindows(now)
	queryCtx, cancel := context.WithTimeout(ctx, w.queryTimeout)
	defer cancel()
	rows, err := w.store.ListOneShotFunding8hExitRows(queryCtx, windowStart, windowEnd)
	if err != nil {
		if ctx.Err() == nil {
			w.logger.Error("list one-shot 8h funding exit candidates failed", "error", err)
		}
		return
	}
	combos, duplicates := groupFunding8hExitRows(rows)
	for id := range duplicates {
		w.logger.Error(
			"one-shot 8h funding exit skipped duplicate settled rows",
			"combination_id", id,
		)
	}
	for id, combo := range combos {
		if _, duplicated := duplicates[id]; duplicated {
			continue
		}
		result := evaluateFunding8hExit(*combo, windowStart, windowEnd)
		if !result.OK || !result.Trigger {
			continue
		}
		_, applied, markErr := w.store.MarkOneShotExiting(
			ctx, combo.ID, combo.Version, "funding_8h_floor",
			map[string]any{
				"reason":             "funding_8h_floor",
				"windowStart":        windowStart.Format(time.RFC3339Nano),
				"windowEnd":          windowEnd.Format(time.RFC3339Nano),
				"legACumulativeRate": result.CumA.String(),
				"legBCumulativeRate": result.CumB.String(),
				"observedAnnualized": result.Observed.String(),
				"configuredFloor":    combo.Floor,
			},
		)
		if markErr != nil {
			if ctx.Err() == nil {
				w.logger.Error("mark one-shot 8h funding exiting failed",
					"combination_id", combo.ID, "error", markErr)
			}
			continue
		}
		if !applied {
			w.logger.Info("one-shot 8h funding exit lost the CAS race",
				"combination_id", combo.ID)
		}
	}
}
