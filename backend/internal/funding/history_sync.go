package funding

import (
	"context"
	"errors"
	"time"

	"selfquant/backend/internal/exchange"
)

func (s *Synchronizer) SyncIncremental(ctx context.Context) error {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	now := time.Now().UTC()
	instruments, bounds, err := s.loadHistoryTargets(ctx, adapterNames(s.adapters))
	if err != nil {
		return err
	}
	var errs []error
	byExchange := groupInstruments(instruments)
	for _, adapter := range s.adapters {
		summary := ExchangeHistorySummary{Mode: historyModeIncremental, Exchange: adapter.Name()}
		started := time.Now()
		for _, instrument := range byExchange[adapter.Name()] {
			summary.ContractsTotal++
			from, due, bootstrap := incrementalDecision(
				now, bounds[instrument.ID].Max, effectiveInterval(instrument.IntervalHours),
			)
			if bootstrap {
				summary.ContractsBootstrapRequired++
				continue
			}
			if !due {
				continue
			}
			summary.ContractsDue++
			result, syncErr := s.syncInstrumentHistory(
				ctx, adapter, instrument, from, now, historyModeIncremental,
			)
			s.recordHistoryResult(&summary, result, syncErr)
			if syncErr != nil && result.Outcome == HistoryOutcomeFailed {
				errs = append(errs, syncErr)
			}
		}
		summary.Elapsed = time.Since(started)
		s.logIncrementalSummary(ctx, summary)
	}
	return errors.Join(errs...)
}

func (s *Synchronizer) ReconcileRecentHistory(ctx context.Context) error {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	now := time.Now().UTC()
	from := now.Add(-recentHistoryWindow)
	if _, _, err := s.backfillWindow(ctx, s.adapters, from, now, historyModeRecent, nil); err != nil {
		return err
	}
	if err := s.refreshAggregates(ctx); err != nil {
		return err
	}
	return s.RefreshSnapshot(ctx)
}

func (s *Synchronizer) BackfillRecent(ctx context.Context, adapters []exchange.Adapter) ([]ExchangeHistorySummary, map[int64]HistorySyncResult, error) {
	now := time.Now().UTC()
	return s.backfillWindow(ctx, adapters, now.Add(-recentHistoryWindow), now, historyModeRecent, nil)
}

func (s *Synchronizer) BackfillDeep(ctx context.Context, adapters []exchange.Adapter) ([]ExchangeHistorySummary, map[int64]HistorySyncResult, error) {
	now := time.Now().UTC()
	return s.backfillWindow(ctx, adapters, deepHistoryStart(now), now, historyModeDeep, nil)
}

func (s *Synchronizer) BackfillDeepEligible(
	ctx context.Context,
	adapters []exchange.Adapter,
	eligible map[int64]HistorySyncResult,
) ([]ExchangeHistorySummary, map[int64]HistorySyncResult, error) {
	now := time.Now().UTC()
	return s.backfillWindow(ctx, adapters, deepHistoryStart(now), now, historyModeDeep, DeepEligibleSet(eligible))
}

func (s *Synchronizer) BackfillNewInstruments(ctx context.Context, instruments []FundingInstrument) {
	if len(instruments) == 0 {
		return
	}
	now := time.Now().UTC()
	from := now.Add(-recentHistoryWindow)
	byExchange := groupInstruments(instruments)
	for _, adapter := range s.adapters {
		for _, instrument := range byExchange[adapter.Name()] {
			if ctx.Err() != nil {
				return
			}
			result, err := s.syncInstrumentHistory(
				ctx, adapter, instrument, from, now, historyModeRecent,
			)
			if err != nil && result.Outcome != HistoryOutcomeLocked {
				s.logger.Debug("new instrument recent history failed",
					"exchange", adapter.Name(), "symbol", instrument.ExchangeSymbol,
					"error", err)
			}
		}
	}
}

func (s *Synchronizer) backfillWindow(
	ctx context.Context,
	adapters []exchange.Adapter,
	from, to time.Time,
	mode historyMode,
	only map[int64]bool,
) ([]ExchangeHistorySummary, map[int64]HistorySyncResult, error) {
	instruments, bounds, err := s.loadHistoryTargets(ctx, adapterNames(adapters))
	if err != nil {
		return nil, nil, err
	}
	byExchange := groupInstruments(instruments)
	summaries := make([]ExchangeHistorySummary, 0, len(adapters))
	results := make(map[int64]HistorySyncResult)
	var errs []error
	for _, adapter := range adapters {
		summary := ExchangeHistorySummary{
			Mode: mode, Exchange: adapter.Name(), HistoryFrom: from, HistoryTo: to,
		}
		started := time.Now()
		for _, instrument := range byExchange[adapter.Name()] {
			if only != nil && !only[instrument.ID] {
				continue
			}
			if ctx.Err() != nil {
				break
			}
			summary.ContractsTotal++
			if mode == historyModeDeep {
				bound := bounds[instrument.ID]
				interval := effectiveInterval(instrument.IntervalHours)
				if deepHistorySatisfied(bound.Min, deepHistoryStart(to), interval) {
					summary.ContractsComplete++
					summary.ContractsProcessed++
					results[instrument.ID] = HistorySyncResult{Outcome: HistoryOutcomeComplete}
					continue
				}
			}
			result, syncErr := s.syncInstrumentHistory(ctx, adapter, instrument, from, to, mode)
			results[instrument.ID] = result
			s.recordHistoryResult(&summary, result, syncErr)
			if syncErr != nil && result.Outcome == HistoryOutcomeFailed {
				errs = append(errs, syncErr)
			}
		}
		summary.Elapsed = time.Since(started)
		s.logBackfillSummary(ctx, summary)
		summaries = append(summaries, summary)
	}
	return summaries, results, errors.Join(errs...)
}

func (s *Synchronizer) syncInstrumentHistory(
	ctx context.Context,
	adapter exchange.Adapter,
	instrument FundingInstrument,
	from, to time.Time,
	mode historyMode,
) (result HistorySyncResult, err error) {
	unlock, ok, lockErr := s.lock.TryLock(ctx, instrument.ID)
	if lockErr != nil {
		return HistorySyncResult{Outcome: HistoryOutcomeFailed}, lockErr
	}
	if !ok {
		return HistorySyncResult{Outcome: HistoryOutcomeLocked}, nil
	}
	defer unlock()

	limit := historyLimit(from, to, instrument.IntervalHours)
	rates, fetchErr := adapter.FetchHistory(ctx, instrument.Instrument, from, limit)
	outcome, classErr := classifyFetchedHistory(fetchErr, rates, from)
	if outcome == HistoryOutcomeNoSettlementYet {
		s.logger.Debug("funding history has no settlement yet",
			"mode", mode, "exchange", adapter.Name(), "symbol", instrument.ExchangeSymbol)
		return HistorySyncResult{Outcome: HistoryOutcomeNoSettlementYet}, nil
	}
	if classErr != nil {
		return HistorySyncResult{Outcome: HistoryOutcomeFailed}, classErr
	}
	normalizeHistoryIntervals(rates, effectiveIntervalHours(instrument.IntervalHours))
	if s.repository == nil {
		return HistorySyncResult{
			Outcome: outcome, From: rates[0].FundingTime, To: rates[len(rates)-1].FundingTime,
		}, nil
	}
	stats, upsertErr := s.repository.UpsertSettledRates(ctx, instrument, rates)
	if upsertErr != nil {
		return HistorySyncResult{Outcome: HistoryOutcomeFailed}, upsertErr
	}
	earliest, latest := rates[0].FundingTime, rates[len(rates)-1].FundingTime
	s.logger.Debug("funding history synchronized",
		"mode", mode, "exchange", adapter.Name(), "symbol", instrument.ExchangeSymbol,
		"from", earliest, "to", latest, "changed", stats.Changed, "unchanged", stats.Unchanged,
		"outcome", outcome)
	return HistorySyncResult{
		Outcome: outcome, Changed: stats.Changed, Unchanged: stats.Unchanged,
		From: earliest, To: latest,
	}, nil
}

func (s *Synchronizer) loadHistoryTargets(
	ctx context.Context,
	exchanges []string,
) ([]FundingInstrument, map[int64]settledBound, error) {
	instruments, err := s.repository.ListActivePerpetualInstruments(ctx, exchanges)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]int64, 0, len(instruments))
	for _, instrument := range instruments {
		ids = append(ids, instrument.ID)
	}
	bounds, err := s.repository.SettledBounds(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	return instruments, bounds, nil
}

func groupInstruments(instruments []FundingInstrument) map[string][]FundingInstrument {
	grouped := make(map[string][]FundingInstrument)
	for _, instrument := range instruments {
		grouped[instrument.Exchange] = append(grouped[instrument.Exchange], instrument)
	}
	return grouped
}

func (s *Synchronizer) recordHistoryResult(summary *ExchangeHistorySummary, result HistorySyncResult, err error) {
	summary.ContractsProcessed++
	summary.RatesChanged += result.Changed
	summary.RatesUnchanged += result.Unchanged
	switch result.Outcome {
	case HistoryOutcomeComplete:
		summary.ContractsComplete++
		if result.Changed > 0 {
			summary.ContractsChanged++
		} else {
			summary.ContractsUnchanged++
		}
	case HistoryOutcomeSourceLimited:
		summary.ContractsSourceLimited++
		if result.Changed > 0 {
			summary.ContractsChanged++
		} else {
			summary.ContractsUnchanged++
		}
	case HistoryOutcomeNoSettlementYet:
		summary.ContractsNoSettlementYet++
	case HistoryOutcomeLocked:
		summary.ContractsLocked++
		summary.ContractsProcessed--
	case HistoryOutcomeFailed:
		summary.ContractsFailed++
	case HistoryOutcomeBootstrapRequired:
		summary.ContractsBootstrapRequired++
	}
	if err != nil && result.Outcome == HistoryOutcomeFailed {
		s.logger.Debug("funding history contract failed",
			"mode", summary.Mode, "exchange", summary.Exchange, "error", err)
	}
}

func (s *Synchronizer) logIncrementalSummary(_ context.Context, summary ExchangeHistorySummary) {
	s.logger.Info("funding history synchronized",
		"mode", summary.Mode, "exchange", summary.Exchange,
		"contracts_total", summary.ContractsTotal,
		"contracts_due", summary.ContractsDue,
		"contracts_changed", summary.ContractsChanged,
		"contracts_unchanged", summary.ContractsUnchanged,
		"contracts_bootstrap_required", summary.ContractsBootstrapRequired,
		"contracts_locked", summary.ContractsLocked,
		"contracts_failed", summary.ContractsFailed,
		"elapsed", summary.Elapsed)
}

func (s *Synchronizer) logBackfillSummary(_ context.Context, summary ExchangeHistorySummary) {
	s.logger.Info("funding history backfill synchronized",
		"mode", summary.Mode, "exchange", summary.Exchange,
		"contracts_total", summary.ContractsTotal,
		"contracts_processed", summary.ContractsProcessed,
		"contracts_complete", summary.ContractsComplete,
		"contracts_source_limited", summary.ContractsSourceLimited,
		"contracts_no_settlement_yet", summary.ContractsNoSettlementYet,
		"contracts_locked", summary.ContractsLocked,
		"contracts_failed", summary.ContractsFailed,
		"rates_changed", summary.RatesChanged,
		"rates_unchanged", summary.RatesUnchanged,
		"history_from", summary.HistoryFrom,
		"history_to", summary.HistoryTo,
		"elapsed", summary.Elapsed)
}

func SummariesIndicateFailure(summaries []ExchangeHistorySummary, zeroExchanges []string) bool {
	if len(zeroExchanges) > 0 {
		return true
	}
	for _, summary := range summaries {
		if summary.ContractsFailed > 0 || summary.ContractsLocked > 0 {
			return true
		}
		if summary.ContractsTotal == 0 {
			return true
		}
	}
	return false
}

func DeepEligibleSet(results map[int64]HistorySyncResult) map[int64]bool {
	eligible := make(map[int64]bool, len(results))
	for id, result := range results {
		if result.HasData() {
			eligible[id] = true
		}
	}
	return eligible
}
