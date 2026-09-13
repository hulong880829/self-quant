package funding

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"selfquant/backend/internal/exchange"
)

func TestClassifyFetchedHistoryWritesPartialAsSourceLimited(t *testing.T) {
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	rates := []exchange.FundingRate{
		{FundingTime: from.Add(5 * 24 * time.Hour), Settled: true},
		{FundingTime: from.Add(6 * 24 * time.Hour), Settled: true},
		{FundingTime: from.Add(7 * 24 * time.Hour), Settled: true},
	}
	got, err := classifyFetchedHistory(nil, rates, from)
	if err != nil || got != HistoryOutcomeSourceLimited {
		t.Fatalf("partial history outcome=%s err=%v", got, err)
	}
}

func TestClassifyFetchedHistoryTreatsSentinelAsNoSettlement(t *testing.T) {
	got, err := classifyFetchedHistory(
		fmt.Errorf("empty: %w", exchange.ErrNoSettledHistory), nil, time.Now(),
	)
	if err != nil || got != HistoryOutcomeNoSettlementYet {
		t.Fatalf("outcome=%s err=%v", got, err)
	}
}

func TestClassifyFetchedHistoryFailsOnTimeoutAndEmptySlice(t *testing.T) {
	if got, err := classifyFetchedHistory(errors.New("timeout"), nil, time.Now()); err == nil || got != HistoryOutcomeFailed {
		t.Fatalf("timeout outcome=%s err=%v", got, err)
	}
	if got, err := classifyFetchedHistory(nil, nil, time.Now()); err == nil || got != HistoryOutcomeFailed {
		t.Fatalf("empty slice outcome=%s err=%v", got, err)
	}
}

func TestValidateSettledRatesRejectsCurrentAndMismatch(t *testing.T) {
	instrument := FundingInstrument{
		ID:         7,
		Instrument: exchange.Instrument{Exchange: "venue-a", ExchangeSymbol: "BTC"},
	}
	if err := validateSettledRates(instrument, []exchange.FundingRate{{
		Exchange: "venue-a", ExchangeSymbol: "BTC", Settled: false,
	}}); !errors.Is(err, ErrSettledRateRequired) {
		t.Fatalf("current error=%v", err)
	}
	if err := validateSettledRates(instrument, []exchange.FundingRate{{
		Exchange: "venue-b", ExchangeSymbol: "BTC", Settled: true,
	}}); !errors.Is(err, ErrSettledRateMismatch) {
		t.Fatalf("exchange error=%v", err)
	}
}

func TestDeepHistorySatisfiedAllowsNextSettlementAfterTarget(t *testing.T) {
	target := time.Date(2025, 8, 27, 19, 43, 0, 0, time.UTC)
	cases := []struct {
		name      string
		earliest  time.Time
		interval  time.Duration
		satisfied bool
	}{
		{
			name:      "1h contract 17 minutes after target",
			earliest:  time.Date(2025, 8, 27, 20, 0, 0, 0, time.UTC),
			interval:  time.Hour,
			satisfied: true,
		},
		{
			name:      "4h contract less than one interval after target",
			earliest:  target.Add(3 * time.Hour),
			interval:  4 * time.Hour,
			satisfied: true,
		},
		{
			name:      "8h contract less than one interval after target",
			earliest:  target.Add(7 * time.Hour),
			interval:  8 * time.Hour,
			satisfied: true,
		},
		{
			name:      "earliest later than target plus one interval",
			earliest:  target.Add(8*time.Hour + time.Minute),
			interval:  8 * time.Hour,
			satisfied: false,
		},
		{
			name:      "no history",
			interval:  time.Hour,
			satisfied: false,
		},
		{
			name:      "earliest before target",
			earliest:  target.Add(-time.Hour),
			interval:  time.Hour,
			satisfied: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deepHistorySatisfied(tc.earliest, target, tc.interval); got != tc.satisfied {
				t.Fatalf("deepHistorySatisfied(%v)=%v want=%v", tc.earliest, got, tc.satisfied)
			}
		})
	}
}

func TestDeepEligibleSkipsNoSettlement(t *testing.T) {
	eligible := DeepEligibleSet(map[int64]HistorySyncResult{
		1: {Outcome: HistoryOutcomeSourceLimited},
		2: {Outcome: HistoryOutcomeNoSettlementYet},
		3: {Outcome: HistoryOutcomeFailed},
		4: {Outcome: HistoryOutcomeComplete},
		5: {Outcome: HistoryOutcomeLocked},
	})
	if !eligible[1] || !eligible[4] || eligible[2] || eligible[3] || eligible[5] {
		t.Fatalf("eligible=%v", eligible)
	}
}

func TestSummariesIndicateFailure(t *testing.T) {
	if !SummariesIndicateFailure(nil, []string{"venue-a"}) {
		t.Fatal("zero exchange must fail")
	}
	if !SummariesIndicateFailure([]ExchangeHistorySummary{{
		ContractsTotal: 2, ContractsLocked: 1,
	}}, nil) {
		t.Fatal("locked must fail")
	}
	if SummariesIndicateFailure([]ExchangeHistorySummary{{
		ContractsTotal: 1, ContractsSourceLimited: 1,
	}}, nil) {
		t.Fatal("source_limited must succeed")
	}
}

func TestZeroContractExchanges(t *testing.T) {
	zero := ZeroContractExchanges(
		[]string{"venue-a", "venue-b"},
		[]FundingInstrument{{Instrument: exchange.Instrument{Exchange: "venue-a"}}},
	)
	if len(zero) != 1 || zero[0] != "venue-b" {
		t.Fatalf("zero=%v", zero)
	}
}
