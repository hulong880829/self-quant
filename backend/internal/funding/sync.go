package funding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"selfquant/backend/internal/exchange"
)

type Synchronizer struct {
	repository        *Repository
	lock              instrumentLocker
	adapters          []exchange.Adapter
	snapshots         *SnapshotStore
	logger            *slog.Logger
	instrumentMu      sync.Mutex
	currentMu         sync.Mutex
	historyMu         sync.Mutex
	dataMu            sync.RWMutex
	instruments       map[string][]exchange.Instrument
	aggregateRuns     atomic.Uint64
	snapshotPublishes atomic.Uint64
}

func NewSynchronizer(
	repository *Repository,
	adapters []exchange.Adapter,
	snapshots *SnapshotStore,
	logger *slog.Logger,
) *Synchronizer {
	if snapshots == nil {
		snapshots = NewSnapshotStore()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Synchronizer{
		repository: repository, lock: nopHistoryLock{}, adapters: adapters,
		snapshots: snapshots, logger: logger,
		instruments: make(map[string][]exchange.Instrument),
	}
}

func (s *Synchronizer) WithLock(lock instrumentLocker) *Synchronizer {
	if lock != nil {
		s.lock = lock
	}
	return s
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
	if err := s.refreshAggregates(ctx); err != nil {
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
	s.snapshotPublishes.Add(1)
	return nil
}

func (s *Synchronizer) refreshAggregates(ctx context.Context) error {
	s.aggregateRuns.Add(1)
	return s.repository.RefreshAggregates(ctx)
}

func (s *Synchronizer) SyncInstruments(ctx context.Context) ([]FundingInstrument, error) {
	s.instrumentMu.Lock()
	defer s.instrumentMu.Unlock()
	var errs []error
	var newPerpetuals []FundingInstrument
	for _, adapter := range s.adapters {
		hadCatalog, err := s.repository.HasExchangeInstruments(ctx, adapter.Name())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, contractType := range exchange.AdapterContractTypes(adapter) {
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
			inserted, err := s.repository.UpsertInstruments(ctx, refreshed)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if contractType == exchange.ContractTypePerpetual {
				newPerpetuals = append(newPerpetuals, qualifyNewPerpetuals(hadCatalog, inserted)...)
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
				"instruments", len(refreshed), "inserted", len(inserted))
		}
	}
	if err := s.RefreshSnapshot(ctx); err != nil {
		errs = append(errs, err)
	}
	return newPerpetuals, errors.Join(errs...)
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
		} else if err := s.repository.UpsertCurrentRates(ctx, current); err != nil {
			errs = append(errs, err)
		} else {
			if err := s.repository.UpdateInstrumentIntervals(ctx, current); err != nil {
				errs = append(errs, err)
			}
			s.updateIntervals(adapter.Name(), current)
			if err := s.RefreshSnapshot(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		s.logger.Info("funding market synchronized",
			"exchange", adapter.Name(), "instruments", len(instruments),
			"current_rates", len(current), "elapsed", time.Since(started))
	}
	if err := s.refreshAggregates(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.RefreshSnapshot(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
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

const (
	snapshotRowLimit  = 10000
	snapshotByteLimit = 16 << 20
)

func EstimateSnapshotBytes(rates []Rate) int {
	bytes := 0
	for _, rate := range rates {
		bytes += 192 + len(rate.Exchange) + len(rate.ExchangeSymbol) +
			len(rate.GlobalSymbol) + len(rate.BaseAsset) + len(rate.QuoteAsset)
	}
	return bytes
}

func SnapshotCapacityExceeded(rates []Rate) bool {
	return len(rates) >= snapshotRowLimit || EstimateSnapshotBytes(rates) >= snapshotByteLimit
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
