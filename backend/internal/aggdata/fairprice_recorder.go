package aggdata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"selfquant/backend/internal/database"
)

type FairPriceRecorderConfig struct {
	DatabaseURL      string
	SampleInterval   time.Duration
	Retention        time.Duration
	CleanupInterval  time.Duration
	DeleteBatch      int
	DeleteMaxBatches int
	OperationTimeout time.Duration
}

func (c FairPriceRecorderConfig) validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("fair price database URL is required")
	}
	if c.SampleInterval <= 0 || c.Retention <= 0 || c.CleanupInterval <= 0 ||
		c.OperationTimeout <= 0 {
		return fmt.Errorf("fair price recorder durations must be positive")
	}
	if c.DeleteBatch <= 0 || c.DeleteMaxBatches <= 0 {
		return fmt.Errorf("fair price cleanup limits must be positive")
	}
	return nil
}

type fairPriceBatch struct {
	observedAt time.Time
	snapshots  []*FairPriceSnapshot
}

type FairPriceRecorder struct {
	store        *Store
	logger       *slog.Logger
	config       FairPriceRecorderConfig
	queue        chan fairPriceBatch
	repositoryMu sync.RWMutex
	repository   *FairPriceRepository

	droppedBatches   atomic.Uint64
	writeFailures    atomic.Uint64
	reconnects       atomic.Uint64
	cleanupFailures  atomic.Uint64
	deletedRows      atomic.Uint64
	skippedDuplicate atomic.Uint64
	skippedStale     atomic.Uint64
	lastPersisted    map[string]fairPriceSequence
}

type fairPriceSequence struct {
	epoch    uint64
	sequence uint64
}

var ErrFairPriceHistoryUnavailable = errors.New("fair price history is unavailable")

type FairPriceHistoryReader interface {
	QueryHistory(
		ctx context.Context,
		profile, symbol, modelID string,
		start, end time.Time,
		resolution time.Duration,
		limit int,
	) ([]FairPriceHistoryPoint, error)
}

func NewFairPriceRecorder(
	store *Store,
	logger *slog.Logger,
	config FairPriceRecorderConfig,
) (*FairPriceRecorder, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &FairPriceRecorder{
		store: store, logger: logger, config: config,
		queue:         make(chan fairPriceBatch, 1),
		lastPersisted: make(map[string]fairPriceSequence),
	}, nil
}

func (r *FairPriceRecorder) Run(ctx context.Context) {
	go r.sample(ctx)
	go r.logStats(ctx)
	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		connectCtx, cancel := context.WithTimeout(ctx, r.config.OperationTimeout)
		pool, err := database.Open(connectCtx, r.config.DatabaseURL)
		cancel()
		if err != nil {
			r.reconnects.Add(1)
			r.logger.Warn("fair price database unavailable", "error", err, "retry_in", backoff)
			if !waitContext(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		r.logger.Info("fair price recorder connected")
		backoff = 250 * time.Millisecond
		repository := NewFairPriceRepository(pool)
		r.setRepository(repository)
		reconnect := r.writeLoop(ctx, repository)
		r.clearRepository(repository)
		pool.Close()
		if !reconnect {
			return
		}
		r.reconnects.Add(1)
		if !waitContext(ctx, backoff) {
			return
		}
	}
}

func (r *FairPriceRecorder) QueryHistory(
	ctx context.Context,
	profile, symbol, modelID string,
	start, end time.Time,
	resolution time.Duration,
	limit int,
) ([]FairPriceHistoryPoint, error) {
	r.repositoryMu.RLock()
	defer r.repositoryMu.RUnlock()
	if r.repository == nil {
		return nil, ErrFairPriceHistoryUnavailable
	}
	return r.repository.QueryRange(
		ctx, profile, symbol, modelID, start, end, resolution, limit,
	)
}

func (r *FairPriceRecorder) setRepository(repository *FairPriceRepository) {
	r.repositoryMu.Lock()
	r.repository = repository
	r.repositoryMu.Unlock()
}

func (r *FairPriceRecorder) clearRepository(repository *FairPriceRepository) {
	r.repositoryMu.Lock()
	if r.repository == repository {
		r.repository = nil
	}
	r.repositoryMu.Unlock()
}

func (r *FairPriceRecorder) sample(ctx context.Context) {
	ticker := time.NewTicker(r.config.SampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			snapshots := r.store.FairSnapshots()
			fresh := snapshots[:0]
			for _, snapshot := range snapshots {
				if r.store.FairSnapshotFresh(snapshot, now) {
					fresh = append(fresh, snapshot)
				} else {
					r.skippedStale.Add(1)
				}
			}
			batch := fairPriceBatch{
				observedAt: now.UTC().Truncate(time.Second),
				snapshots:  fresh,
			}
			if len(batch.snapshots) == 0 {
				continue
			}
			if enqueueLatest(r.queue, batch) {
				r.droppedBatches.Add(1)
			}
		}
	}
}

func (r *FairPriceRecorder) writeLoop(
	ctx context.Context,
	repository *FairPriceRepository,
) bool {
	cleanup := time.NewTicker(r.config.CleanupInterval)
	defer cleanup.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case batch := <-r.queue:
			batch.snapshots = r.unpersisted(batch.snapshots)
			if len(batch.snapshots) == 0 {
				continue
			}
			writeCtx, cancel := context.WithTimeout(ctx, r.config.OperationTimeout)
			err := repository.Upsert(writeCtx, batch.observedAt, batch.snapshots)
			cancel()
			if err != nil {
				r.writeFailures.Add(1)
				r.logger.Warn("fair price snapshot write failed", "error", err)
				select {
				case r.queue <- batch:
				default:
				}
				return true
			}
			for _, snapshot := range batch.snapshots {
				r.lastPersisted[fairPriceSnapshotKey(snapshot)] = fairPriceSequence{
					epoch: snapshot.RingEpoch, sequence: snapshot.RingSequence,
				}
			}
		case now := <-cleanup.C:
			r.cleanup(ctx, repository, now.Add(-r.config.Retention))
		}
	}
}

func (r *FairPriceRecorder) cleanup(
	ctx context.Context,
	repository *FairPriceRepository,
	cutoff time.Time,
) {
	var deleted int64
	for range r.config.DeleteMaxBatches {
		deleteCtx, cancel := context.WithTimeout(ctx, r.config.OperationTimeout)
		rows, err := repository.DeleteExpired(deleteCtx, cutoff, r.config.DeleteBatch)
		cancel()
		if err != nil {
			r.cleanupFailures.Add(1)
			r.logger.Warn("fair price retention cleanup failed", "error", err)
			return
		}
		deleted += rows
		if rows < int64(r.config.DeleteBatch) {
			break
		}
	}
	if deleted > 0 {
		r.deletedRows.Add(uint64(deleted))
		r.logger.Info("fair price retention cleanup completed", "rows", deleted)
	}
}

func (r *FairPriceRecorder) logStats(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.logger.Info(
				"fair price recorder statistics",
				"dropped_batches", r.droppedBatches.Load(),
				"write_failures", r.writeFailures.Load(),
				"reconnects", r.reconnects.Load(),
				"cleanup_failures", r.cleanupFailures.Load(),
				"deleted_rows", r.deletedRows.Load(),
				"skipped_duplicate_snapshots", r.skippedDuplicate.Load(),
				"skipped_stale_snapshots", r.skippedStale.Load(),
			)
		}
	}
}

func (r *FairPriceRecorder) unpersisted(
	snapshots []*FairPriceSnapshot,
) []*FairPriceSnapshot {
	result := snapshots[:0]
	for _, snapshot := range snapshots {
		previous, exists := r.lastPersisted[fairPriceSnapshotKey(snapshot)]
		if exists && previous.epoch == snapshot.RingEpoch &&
			previous.sequence == snapshot.RingSequence {
			r.skippedDuplicate.Add(1)
			continue
		}
		result = append(result, snapshot)
	}
	return result
}

func fairPriceSnapshotKey(snapshot *FairPriceSnapshot) string {
	return snapshot.Profile + "\x00" + snapshot.Symbol + "\x00" + snapshot.ModelID
}

func enqueueLatest(queue chan fairPriceBatch, value fairPriceBatch) bool {
	select {
	case queue <- value:
		return false
	default:
	}
	select {
	case <-queue:
	default:
	}
	select {
	case queue <- value:
	default:
	}
	return true
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
