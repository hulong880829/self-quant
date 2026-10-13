package funding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
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
	tradefi           *TradefiService
	logger            *slog.Logger
	instrumentWrites  instrumentWriter
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

type instrumentWriter interface {
	HasExchangeInstruments(context.Context, string) (bool, error)
	UpsertInstruments(context.Context, []exchange.Instrument) ([]FundingInstrument, error)
	DeactivateMissingInstruments(context.Context, string, string, []exchange.Instrument) error
}

func (s *Synchronizer) WithLock(lock instrumentLocker) *Synchronizer {
	if lock != nil {
		s.lock = lock
	}
	return s
}

func (s *Synchronizer) WithTradefi(service *TradefiService) *Synchronizer {
	s.tradefi = service
	return s
}

func (s *Synchronizer) writer() instrumentWriter {
	if s.instrumentWrites != nil {
		return s.instrumentWrites
	}
	return s.repository
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
	writer := s.writer()
	type stagedCatalog struct {
		name, contractType string
		hadCatalog         bool
		items              []exchange.Instrument
	}
	var staged []stagedCatalog
	refreshedPerpetual := map[string]bool{}
	for _, adapter := range s.adapters {
		hadCatalog, err := writer.HasExchangeInstruments(ctx, adapter.Name())
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
			staged = append(staged, stagedCatalog{
				name: adapter.Name(), contractType: contractType, hadCatalog: hadCatalog, items: refreshed,
			})
			if contractType == exchange.ContractTypePerpetual {
				refreshedPerpetual[adapter.Name()] = true
			}
		}
	}
	previous := s.cachedPerpetuals()
	var targets, sources []exchange.Instrument
	for _, stage := range staged {
		if stage.contractType != exchange.ContractTypePerpetual {
			continue
		}
		targets = append(targets, stage.items...)
		sources = append(sources, stage.items...)
	}
	for _, item := range previous {
		if !refreshedPerpetual[item.Exchange] {
			sources = append(sources, item)
		}
	}
	identified := exchange.ApplyTradfiIdentities(targets, sources, previous)
	offset := 0
	for index := range staged {
		if staged[index].contractType != exchange.ContractTypePerpetual {
			continue
		}
		count := len(staged[index].items)
		staged[index].items = identified[offset : offset+count]
		offset += count
	}
	for _, stage := range staged {
		inserted, err := writer.UpsertInstruments(ctx, stage.items)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if stage.contractType == exchange.ContractTypePerpetual {
			newPerpetuals = append(newPerpetuals, qualifyNewPerpetuals(stage.hadCatalog, inserted)...)
		}
		if err := writer.DeactivateMissingInstruments(
			ctx, stage.name, stage.contractType, stage.items,
		); err != nil {
			errs = append(errs, err)
			continue
		}
		s.dataMu.Lock()
		s.instruments[instrumentBucket(stage.name, stage.contractType)] = stage.items
		s.dataMu.Unlock()
		s.logger.Info("instrument market synchronized",
			"exchange", stage.name, "contract_type", stage.contractType,
			"instruments", len(stage.items), "inserted", len(inserted))
	}
	if s.repository != nil {
		if err := s.RefreshSnapshot(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if s.tradefi != nil {
		if err := s.tradefi.Reconcile(ctx, s.allPerpetuals()); err != nil {
			errs = append(errs, err)
		}
	}
	return newPerpetuals, errors.Join(errs...)
}

func (s *Synchronizer) cachedPerpetuals() []exchange.Instrument {
	s.dataMu.RLock()
	defer s.dataMu.RUnlock()
	var result []exchange.Instrument
	for key, items := range s.instruments {
		if !strings.HasSuffix(key, "\x00"+exchange.ContractTypePerpetual) {
			continue
		}
		result = append(result, items...)
	}
	return result
}

func (s *Synchronizer) allPerpetuals() []exchange.Instrument {
	s.dataMu.RLock()
	defer s.dataMu.RUnlock()
	var result []exchange.Instrument
	for _, adapter := range s.adapters {
		result = append(result, s.instruments[instrumentBucket(adapter.Name(), exchange.ContractTypePerpetual)]...)
	}
	return result
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
	if s.tradefi != nil {
		s.tradefi.PublishFunding(s.snapshots.View().Rates)
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
