package report

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type Scheduler struct {
	repository *Repository
	source     AccountSource
	metrics    *Metrics
	logger     *slog.Logger
	location   *time.Location
	now        func() time.Time
	mu         sync.Mutex
	lastSample string
	lastFinal  string
}

func NewScheduler(
	repository *Repository,
	source AccountSource,
	metrics *Metrics,
	logger *slog.Logger,
	location *time.Location,
) *Scheduler {
	return &Scheduler{
		repository: repository,
		source:     source,
		metrics:    metrics,
		logger:     logger,
		location:   location,
		now:        time.Now,
	}
}

func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	s.tick(ctx, s.now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.tick(ctx, now)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context, now time.Time) {
	local := now.In(s.location)
	if local.Hour() == 8 && local.Minute() >= 55 && local.Minute() <= 59 {
		key := local.Format("2006-01-02T15:04")
		if s.claimSample(key) {
			s.sample(ctx, now.UTC())
		}
	}
	if local.Hour() == 9 && (local.Minute() > 0 || local.Second() >= 5) {
		key := local.Format(time.DateOnly)
		if s.claimFinal(key) {
			s.finalize(ctx, local, now.UTC())
		}
	}
	s.processRecomputeJobs(ctx, now.UTC())
}

func (s *Scheduler) claimSample(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastSample == key {
		return false
	}
	s.lastSample = key
	return true
}

func (s *Scheduler) claimFinal(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastFinal == key {
		return false
	}
	s.lastFinal = key
	return true
}

func (s *Scheduler) sample(ctx context.Context, sampledAt time.Time) {
	if s.source == nil {
		s.metrics.RecordSample(false)
		s.logger.Warn("report sampling skipped", "error", ErrSourceDisabled)
		return
	}
	products, err := s.repository.ListActiveProducts(ctx)
	if err != nil {
		s.metrics.RecordSample(false)
		s.logger.Error("report products unavailable for sampling", "error", err)
		return
	}
	success := true
	for _, product := range products {
		items, sampleErr := s.source.SampleProduct(ctx, product)
		if sampleErr != nil {
			success = false
			s.logger.Warn("product equity sample failed", "product_id", product.ID, "error", sampleErr)
			continue
		}
		if len(items) == 0 {
			continue
		}
		if sampleErr := s.repository.InsertEquitySamples(
			ctx, product.ID, sampledAt.Truncate(time.Second), items,
		); sampleErr != nil {
			success = false
			s.logger.Warn("store product equity samples failed", "product_id", product.ID, "error", sampleErr)
		}
	}
	s.metrics.RecordSample(success)
}

func (s *Scheduler) finalize(ctx context.Context, reportDate time.Time, finalizedAt time.Time) {
	products, err := s.repository.ListActiveProducts(ctx)
	if err != nil {
		s.metrics.RecordFinalize(false, 0)
		s.logger.Error("report products unavailable for finalize", "error", err)
		return
	}
	written := 0
	var finalErr error
	for _, product := range products {
		if s.source != nil {
			if syncErr := s.source.SyncProductTradeFills(ctx, product, finalizedAt); syncErr != nil {
				finalErr = fmt.Errorf("product %d trade fills: %w", product.ID, syncErr)
				s.logger.Warn(
					"product trade fill sync failed",
					"product_id", product.ID,
					"error", syncErr,
				)
			}
		}
		ok, err := s.repository.FinalizeDate(
			ctx, product.ID, reportDate, s.location, finalizedAt,
		)
		if err != nil {
			finalErr = fmt.Errorf("product %d: %w", product.ID, err)
			s.logger.Warn("product report finalize failed", "product_id", product.ID, "error", err)
			continue
		}
		if ok {
			written++
		}
	}
	s.metrics.RecordFinalize(finalErr == nil, written)
	s.logger.Info("product reports finalized", "date", reportDate.Format(time.DateOnly), "written", written)
}

func (s *Scheduler) processRecomputeJobs(ctx context.Context, now time.Time) {
	jobs, err := s.repository.ClaimRecomputeJobs(ctx, 25)
	if err != nil {
		s.logger.Warn("claim report recompute jobs failed", "error", err)
		return
	}
	for _, job := range jobs {
		day, parseErr := time.ParseInLocation(time.DateOnly, job.ReportDate, PeriodLocation())
		if parseErr != nil {
			_ = s.repository.FinishRecomputeJob(ctx, job.ID, job.ProductID, job.ReportDate, parseErr)
			continue
		}
		ok, finalizeErr := s.repository.RecomputeDate(ctx, job.ProductID, day, PeriodLocation(), now)
		if finalizeErr == nil && !ok {
			finalizeErr = fmt.Errorf("snapshot samples missing for %s", job.ReportDate)
		}
		if finishErr := s.repository.FinishRecomputeJob(
			ctx, job.ID, job.ProductID, job.ReportDate, finalizeErr,
		); finishErr != nil {
			s.logger.Warn("finish report recompute job failed", "job_id", job.ID, "error", finishErr)
		}
		if finalizeErr != nil {
			s.logger.Warn(
				"product report recompute failed",
				"product_id", job.ProductID,
				"report_date", job.ReportDate,
				"attempt", job.Attempts,
				"error", finalizeErr,
			)
			continue
		}
		s.logger.Info(
			"product report recomputed",
			"product_id", job.ProductID,
			"report_date", job.ReportDate,
			"attempt", job.Attempts,
		)
	}
}
