package funding

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"selfquant/backend/internal/exchange"
)

type Synchronizer struct {
	repository  *Repository
	adapters    []exchange.Adapter
	logger      *slog.Logger
	mu          sync.Mutex
	instruments map[string][]exchange.Instrument
}

func NewSynchronizer(repository *Repository, adapters []exchange.Adapter, logger *slog.Logger) *Synchronizer {
	return &Synchronizer{
		repository: repository, adapters: adapters, logger: logger,
		instruments: make(map[string][]exchange.Instrument),
	}
}

func (s *Synchronizer) SyncAll(ctx context.Context, includeHistory bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for _, adapter := range s.adapters {
		started := time.Now()
		instruments := s.instruments[adapter.Name()]
		if includeHistory || len(instruments) == 0 {
			refreshed, err := adapter.SyncInstruments(ctx)
			if err != nil {
				errs = append(errs, err)
				s.logger.Error("instrument synchronization failed", "exchange", adapter.Name(), "error", err)
				if len(instruments) == 0 {
					continue
				}
			} else {
				instruments = refreshed
				s.instruments[adapter.Name()] = refreshed
				if err := s.repository.UpsertInstruments(ctx, instruments); err != nil {
					errs = append(errs, err)
					continue
				}
				if err := s.repository.MarkMissingInstrumentsInactive(ctx, adapter.Name(), instruments); err != nil {
					errs = append(errs, err)
				}
			}
		}
		current, err := adapter.FetchCurrent(ctx, instruments)
		if err != nil {
			errs = append(errs, err)
			s.logger.Error("current funding synchronization failed", "exchange", adapter.Name(), "error", err)
		} else if err := s.repository.UpsertRates(ctx, current); err != nil {
			errs = append(errs, err)
		}
		if includeHistory {
			if err := s.syncHistory(ctx, adapter, instruments); err != nil {
				errs = append(errs, err)
			}
		}
		s.logger.Info("exchange synchronized", "exchange", adapter.Name(), "instruments", len(instruments), "current_rates", len(current), "elapsed", time.Since(started))
	}
	return errors.Join(errs...)
}

func (s *Synchronizer) syncHistory(ctx context.Context, adapter exchange.Adapter, instruments []exchange.Instrument) error {
	type result struct {
		rates []exchange.FundingRate
		err   error
	}
	jobs := make(chan exchange.Instrument)
	results := make(chan result)
	workers := min(8, len(instruments))
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for instrument := range jobs {
				historyLimit := max(10, int(7*24/instrument.IntervalHours)+1)
				rates, err := adapter.FetchHistory(
					ctx, instrument, time.Now().AddDate(0, 0, -8), historyLimit,
				)
				results <- result{rates: rates, err: err}
			}
		}()
	}
	go func() {
		for _, instrument := range instruments {
			jobs <- instrument
		}
		close(jobs)
		group.Wait()
		close(results)
	}()

	var errs []error
	var batch []exchange.FundingRate
	for item := range results {
		if item.err != nil {
			errs = append(errs, item.err)
			continue
		}
		batch = append(batch, item.rates...)
		if len(batch) >= 500 {
			if err := s.repository.UpsertRates(ctx, batch); err != nil {
				errs = append(errs, err)
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if err := s.repository.UpsertRates(ctx, batch); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
