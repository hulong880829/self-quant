package funding

import (
	"errors"
	"fmt"
	"time"

	"selfquant/backend/internal/exchange"
)

const (
	recentHistoryWindow = 8 * 24 * time.Hour
	historyOverlap      = 2
)

func deepHistoryStart(now time.Time) time.Time {
	return now.UTC().AddDate(-1, 0, 0)
}

func deepHistorySatisfied(earliest, target time.Time, interval time.Duration) bool {
	if earliest.IsZero() {
		return false
	}
	return !earliest.After(target.Add(interval))
}

type historyMode string

const (
	historyModeIncremental historyMode = "incremental"
	historyModeRecent      historyMode = "recent"
	historyModeDeep        historyMode = "deep"
)

type HistoryOutcome string

const (
	HistoryOutcomeComplete          HistoryOutcome = "complete"
	HistoryOutcomeSourceLimited     HistoryOutcome = "source_limited"
	HistoryOutcomeNoSettlementYet   HistoryOutcome = "no_settlement_yet"
	HistoryOutcomeBootstrapRequired HistoryOutcome = "bootstrap_required"
	HistoryOutcomeSkipped           HistoryOutcome = "skipped"
	HistoryOutcomeLocked            HistoryOutcome = "locked"
	HistoryOutcomeFailed            HistoryOutcome = "failed"
)

type FundingInstrument struct {
	ID int64
	exchange.Instrument
}

type HistorySyncResult struct {
	Outcome   HistoryOutcome
	Changed   int
	Unchanged int
	From      time.Time
	To        time.Time
}

func (r HistorySyncResult) HasData() bool {
	return r.Outcome == HistoryOutcomeComplete || r.Outcome == HistoryOutcomeSourceLimited
}

func (r HistorySyncResult) Succeeded() bool {
	return r.Outcome == HistoryOutcomeComplete ||
		r.Outcome == HistoryOutcomeSourceLimited ||
		r.Outcome == HistoryOutcomeNoSettlementYet ||
		r.Outcome == HistoryOutcomeSkipped ||
		r.Outcome == HistoryOutcomeBootstrapRequired
}

type UpsertStats struct {
	Changed   int
	Unchanged int
}

type ExchangeHistorySummary struct {
	Mode                       historyMode
	Exchange                   string
	ContractsTotal             int
	ContractsDue               int
	ContractsProcessed         int
	ContractsComplete          int
	ContractsChanged           int
	ContractsUnchanged         int
	ContractsBootstrapRequired int
	ContractsSourceLimited     int
	ContractsNoSettlementYet   int
	ContractsLocked            int
	ContractsFailed            int
	RatesChanged               int
	RatesUnchanged             int
	HistoryFrom                time.Time
	HistoryTo                  time.Time
	Elapsed                    time.Duration
}

func qualifyNewPerpetuals(hadCatalog bool, inserted []FundingInstrument) []FundingInstrument {
	if !hadCatalog {
		return nil
	}
	return append([]FundingInstrument(nil), inserted...)
}

func effectiveInterval(hours float64) time.Duration {
	if hours <= 0 {
		hours = 8
	}
	return time.Duration(hours * float64(time.Hour))
}

func effectiveIntervalHours(hours float64) float64 {
	if hours <= 0 {
		return 8
	}
	return hours
}

func historyLimit(from, to time.Time, intervalHours float64) int {
	hours := effectiveIntervalHours(intervalHours)
	window := to.Sub(from).Hours()
	if window < 0 {
		return historyOverlap
	}
	return int(window/hours) + historyOverlap + 1
}

func incrementalDecision(now, watermark time.Time, interval time.Duration) (from time.Time, due, bootstrap bool) {
	if watermark.IsZero() {
		return time.Time{}, false, true
	}
	if now.Before(watermark.Add(interval)) {
		return time.Time{}, false, false
	}
	return watermark.Add(-time.Duration(historyOverlap) * interval), true, false
}

func classifyFetchedHistory(err error, rates []exchange.FundingRate, from time.Time) (HistoryOutcome, error) {
	if errors.Is(err, exchange.ErrNoSettledHistory) {
		return HistoryOutcomeNoSettlementYet, nil
	}
	if err != nil {
		return HistoryOutcomeFailed, err
	}
	if len(rates) == 0 {
		return HistoryOutcomeFailed, fmt.Errorf("history returned no settled rows")
	}
	earliest := rates[0].FundingTime
	for _, rate := range rates[1:] {
		if rate.FundingTime.Before(earliest) {
			earliest = rate.FundingTime
		}
	}
	if earliest.After(from) {
		return HistoryOutcomeSourceLimited, nil
	}
	return HistoryOutcomeComplete, nil
}

func adapterNames(adapters []exchange.Adapter) []string {
	names := make([]string, 0, len(adapters))
	for _, adapter := range adapters {
		names = append(names, adapter.Name())
	}
	return names
}
