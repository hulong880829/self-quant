package funding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	"selfquant/backend/internal/exchange"
)

type Synchronizer struct {
	repository       *Repository
	adapters         []exchange.Adapter
	snapshots        *SnapshotStore
	logger           *slog.Logger
	instrumentMu     sync.Mutex
	currentMu        sync.Mutex
	historyMu        sync.Mutex
	dataMu           sync.RWMutex
	instruments      map[string][]exchange.Instrument
	historyAttempted map[string]bool
}

func NewSynchronizer(
	repository *Repository,
	adapters []exchange.Adapter,
	snapshots *SnapshotStore,
	logger *slog.Logger,
) *Synchronizer {
	return &Synchronizer{
		repository: repository, adapters: adapters, snapshots: snapshots, logger: logger,
		instruments:      make(map[string][]exchange.Instrument),
		historyAttempted: make(map[string]bool),
	}
}

func instrumentBucket(exchangeName, contractType string) string {
	return exchangeName + "\x00" + contractType
}

func (s *Synchronizer) Hydrate(ctx context.Context) error {
	instruments, err := s.repository.ListInstruments(ctx)
	if err != nil {
		return err
	}
	grouped := make(map[string][]exchange.Instrument)
	for _, instrument := range instruments {
		key := instrumentBucket(instrument.Exchange, instrument.ContractType)
		grouped[key] = append(grouped[key], instrument)
	}
	s.dataMu.Lock()
	s.instruments = grouped
	s.dataMu.Unlock()
	if err := s.repository.RefreshAggregates(ctx); err != nil {
		return err
	}
	return s.RefreshSnapshot(ctx)
}

func (s *Synchronizer) RefreshSnapshot(ctx context.Context) error {
	rates, total, err := s.repository.List(ctx, Query{Limit: 10000})
	if err != nil {
		return err
	}
	s.snapshots.Replace(rates, total, time.Now())
	return nil
}

func (s *Synchronizer) SyncInstruments(ctx context.Context) error {
	s.instrumentMu.Lock()
	defer s.instrumentMu.Unlock()
	var errs []error
	for _, adapter := range s.adapters {
		for _, contractType := range []string{
			exchange.ContractTypeSpot,
			exchange.ContractTypePerpetual,
		} {
			refreshed, err := adapter.SyncInstruments(ctx, contractType)
			if !authoritativeInstrumentRefresh(refreshed, err) {
				if err != nil {
					errs = append(errs, err)
					s.logger.Warn("instrument market synchronization failed",
						"exchange", adapter.Name(), "contract_type", contractType, "error", err)
				} else {
					err = fmt.Errorf("%s %s instrument synchronization returned an empty catalog",
						adapter.Name(), contractType)
					errs = append(errs, err)
					s.logger.Warn("instrument market synchronization ignored empty catalog",
						"exchange", adapter.Name(), "contract_type", contractType)
				}
				continue
			}
			if err := s.repository.UpsertInstruments(ctx, refreshed); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := s.repository.DeactivateMissingInstruments(
				ctx, adapter.Name(), contractType, refreshed,
			); err != nil {
				errs = append(errs, err)
				continue
			}
			s.dataMu.Lock()
			s.instruments[instrumentBucket(adapter.Name(), contractType)] = refreshed
			s.dataMu.Unlock()
			s.logger.Info("instrument market synchronized",
				"exchange", adapter.Name(), "contract_type", contractType,
				"instruments", len(refreshed))
		}
	}
	if err := s.RefreshSnapshot(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func authoritativeInstrumentRefresh(instruments []exchange.Instrument, err error) bool {
	return err == nil && len(instruments) > 0
}

func (s *Synchronizer) perpetuals(exchangeName string) []exchange.Instrument {
	s.dataMu.RLock()
	result := append([]exchange.Instrument(nil),
		s.instruments[instrumentBucket(exchangeName, exchange.ContractTypePerpetual)]...)
	s.dataMu.RUnlock()
	return result
}

func (s *Synchronizer) updateIntervals(exchangeName string, rates []exchange.FundingRate) {
	intervals := make(map[string]float64, len(rates))
	for _, rate := range rates {
		if rate.IntervalHours > 0 {
			intervals[rate.ExchangeSymbol] = rate.IntervalHours
		}
	}
	s.dataMu.Lock()
	key := instrumentBucket(exchangeName, exchange.ContractTypePerpetual)
	for index := range s.instruments[key] {
		if interval := intervals[s.instruments[key][index].ExchangeSymbol]; interval > 0 {
			s.instruments[key][index].IntervalHours = interval
		}
	}
	s.dataMu.Unlock()
}

func (s *Synchronizer) SyncCurrent(ctx context.Context) error {
	s.currentMu.Lock()
	defer s.currentMu.Unlock()
	var errs []error
	for _, adapter := range s.adapters {
		started := time.Now()
		instruments := s.perpetuals(adapter.Name())
		if len(instruments) == 0 {
			continue
		}
		current, err := adapter.FetchCurrent(ctx, instruments)
		if err != nil {
			errs = append(errs, err)
			s.logger.Warn("current funding synchronization failed",
				"exchange", adapter.Name(), "error", err)
		} else if err := s.repository.UpsertRates(ctx, current); err != nil {
			errs = append(errs, err)
		} else {
			if err := s.repository.UpdateInstrumentIntervals(ctx, current); err != nil {
				errs = append(errs, err)
			}
			s.updateIntervals(adapter.Name(), current)
			if err := s.repository.RefreshAggregates(ctx); err != nil {
				errs = append(errs, err)
			} else if err := s.RefreshSnapshot(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		s.logger.Info("funding market synchronized",
			"exchange", adapter.Name(), "instruments", len(instruments),
			"current_rates", len(current), "elapsed", time.Since(started))
	}
	if err := s.repository.RefreshAggregates(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.RefreshSnapshot(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *Synchronizer) SyncHistory(ctx context.Context, bootstrap bool) error {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	watermarks, err := s.repository.HistoryWatermarks(ctx)
	if err != nil {
		return err
	}
	starts, err := s.repository.HistoryStarts(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, adapter := range s.adapters {
		instruments := s.perpetuals(adapter.Name())
		if err := s.syncHistory(
			ctx, adapter, instruments, watermarks, starts, bootstrap,
		); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.repository.RefreshAggregates(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.RefreshSnapshot(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *Synchronizer) syncHistory(
	ctx context.Context,
	adapter exchange.Adapter,
	instruments []exchange.Instrument,
	watermarks map[string]time.Time,
	starts map[string]time.Time,
	bootstrap bool,
) error {
	now := time.Now().UTC()
	var firstErr error
	failures := 0
	processed := 0
	for _, instrument := range instruments {
		effectiveHours := instrument.IntervalHours
		if effectiveHours <= 0 {
			effectiveHours = 8
		}
		interval := time.Duration(effectiveHours * float64(time.Hour))
		key := instrumentKey(instrument.Exchange, instrument.ExchangeSymbol)
		watermark := watermarks[key]
		if !bootstrap && watermark.IsZero() && s.historyAttempted[key] {
			continue
		}
		if !bootstrap && !watermark.IsZero() && now.Before(watermark.Add(interval)) {
			continue
		}
		historyStart := starts[key]
		since := historySince(now, watermark, historyStart, interval, bootstrap)
		historyLimit := int(math.Ceil(now.Sub(since).Hours()/effectiveHours)) + 2
		rates, err := adapter.FetchHistory(ctx, instrument, since, historyLimit)
		if err != nil {
			failures++
			if firstErr == nil {
				firstErr = err
			}
		} else {
			normalizeHistoryIntervals(rates, effectiveHours)
			if err := s.repository.UpsertRates(ctx, rates); err != nil {
				failures++
				if firstErr == nil {
					firstErr = err
				}
			} else {
				s.historyAttempted[key] = true
				if len(rates) > 0 {
					s.logger.Debug("funding history coverage",
						"exchange", adapter.Name(), "symbol", instrument.ExchangeSymbol,
						"from", rates[0].FundingTime,
						"to", rates[len(rates)-1].FundingTime, "records", len(rates))
				}
			}
		}
		processed++
		if processed%100 == 0 {
			if err := waitContext(ctx, time.Second); err != nil {
				return err
			}
		}
	}
	if failures > 0 {
		return fmt.Errorf("%s history synchronization: %d requests failed: %w",
			adapter.Name(), failures, firstErr)
	}
	return nil
}

func historySince(
	now time.Time,
	watermark time.Time,
	historyStart time.Time,
	interval time.Duration,
	bootstrap bool,
) time.Time {
	yearAgo := now.AddDate(-1, 0, 0)
	hasFullWindow := !historyStart.IsZero() &&
		!historyStart.After(yearAgo.Add(interval))
	if !watermark.IsZero() && (!bootstrap || hasFullWindow) {
		if overlap := watermark.Add(-2 * interval); overlap.After(yearAgo) {
			return overlap
		}
	}
	return yearAgo
}

func normalizeHistoryIntervals(rates []exchange.FundingRate, fallbackHours float64) {
	if fallbackHours <= 0 {
		fallbackHours = 8
	}
	sort.Slice(rates, func(left, right int) bool {
		return rates[left].FundingTime.Before(rates[right].FundingTime)
	})
	for index := range rates {
		hours := fallbackHours
		if index+1 < len(rates) {
			delta := rates[index+1].FundingTime.Sub(rates[index].FundingTime).Hours()
			if delta > 0 && delta <= 24 {
				hours = delta
			}
		} else if index > 0 {
			delta := rates[index].FundingTime.Sub(rates[index-1].FundingTime).Hours()
			if delta > 0 && delta <= 24 {
				hours = delta
			}
		}
		rates[index].IntervalHours = hours
	}
}

func (s *Synchronizer) ReconcileRecentHistory(ctx context.Context) error {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	since := time.Now().UTC().AddDate(0, 0, -8)
	var errs []error
	for _, adapter := range s.adapters {
		instruments := s.perpetuals(adapter.Name())
		emptyWatermarks := make(map[string]time.Time)
		fullWindowStarts := make(map[string]time.Time)
		for _, instrument := range instruments {
			key := instrumentKey(instrument.Exchange, instrument.ExchangeSymbol)
			emptyWatermarks[key] = since
			fullWindowStarts[key] = time.Now().UTC().AddDate(-1, 0, 0)
		}
		if err := s.syncHistory(
			ctx, adapter, instruments, emptyWatermarks, fullWindowStarts, true,
		); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.repository.RefreshAggregates(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.RefreshSnapshot(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *Synchronizer) CleanupHistory(ctx context.Context) error {
	cutoff := time.Now().UTC().AddDate(-1, 0, 0)
	for {
		deleted, err := s.repository.DeleteExpiredHistory(ctx, cutoff, 5000)
		if err != nil {
			return err
		}
		if deleted < 5000 {
			return nil
		}
		if err := waitContext(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
