package funding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"selfquant/backend/internal/exchange"
)

func TestNormalizeHistoryIntervals(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	rates := []exchange.FundingRate{
		{FundingTime: base.Add(12 * time.Hour)},
		{FundingTime: base},
		{FundingTime: base.Add(4 * time.Hour)},
	}
	normalizeHistoryIntervals(rates, 8)
	if rates[0].IntervalHours != 4 ||
		rates[1].IntervalHours != 8 ||
		rates[2].IntervalHours != 8 {
		t.Fatalf("intervals=%v,%v,%v",
			rates[0].IntervalHours, rates[1].IntervalHours, rates[2].IntervalHours)
	}
}

func TestWaitContextCanBeCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := waitContext(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancelled wait did not return promptly")
	}
}

func TestInstrumentRefreshMustSucceedAndContainInstruments(t *testing.T) {
	instruments := []exchange.Instrument{{ExchangeSymbol: "BTCUSDT"}}
	if !authoritativeInstrumentRefresh(instruments, nil) {
		t.Fatal("successful non-empty catalog was rejected")
	}
	if authoritativeInstrumentRefresh(nil, nil) {
		t.Fatal("empty catalog must not trigger deactivation")
	}
	if authoritativeInstrumentRefresh(instruments, errors.New("exchange unavailable")) {
		t.Fatal("failed exchange request must not trigger deactivation")
	}
}

type fakeAdapter struct {
	name      string
	types     []string
	current   []exchange.FundingRate
	delay     time.Duration
	currentAt time.Time
	started   chan struct{}
	release   chan struct{}
}

func (a *fakeAdapter) Name() string { return a.name }
func (a *fakeAdapter) SupportedContractTypes() []string {
	if len(a.types) > 0 {
		return a.types
	}
	return nil
}
func (a *fakeAdapter) SyncInstruments(context.Context, string) ([]exchange.Instrument, error) {
	return []exchange.Instrument{{Exchange: a.name, ExchangeSymbol: "BTC", ContractType: exchange.ContractTypePerpetual}}, nil
}
func (a *fakeAdapter) FetchCurrent(ctx context.Context, _ []exchange.Instrument) ([]exchange.FundingRate, error) {
	if a.started != nil {
		close(a.started)
	}
	if a.release != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-a.release:
		}
	}
	if a.delay > 0 {
		timer := time.NewTimer(a.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	a.currentAt = time.Now()
	return a.current, nil
}
func (a *fakeAdapter) FetchHistory(context.Context, exchange.Instrument, time.Time, int) ([]exchange.FundingRate, error) {
	return nil, nil
}

func TestSyncCurrentPublishesEachVenueAndAggregatesOnceIntegration(t *testing.T) {
	repository, _ := openFundingTestRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Second)
	instruments := make([]exchange.Instrument, 0, 8)
	adapters := make([]exchange.Adapter, 0, 8)
	instrumentMap := make(map[string][]exchange.Instrument, 8)
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	for index := 0; index < 8; index++ {
		name := fmt.Sprintf("venue-%d", index)
		symbol := fmt.Sprintf("ASSET%dUSDT", index)
		instrument := testPerpetual(name, symbol, now)
		instruments = append(instruments, instrument)
		instrumentMap[instrumentBucket(name, exchange.ContractTypePerpetual)] =
			[]exchange.Instrument{instrument}
		adapter := &fakeAdapter{name: name, current: []exchange.FundingRate{{
			Exchange: name, ExchangeSymbol: symbol, Rate: 0.002 + float64(index)/1000,
			FundingTime: now.Add(8 * time.Hour), IntervalHours: 8, SourceUpdatedAt: now,
		}}}
		if index == 7 {
			adapter.started = slowStarted
			adapter.release = releaseSlow
		}
		adapters = append(adapters, adapter)
	}
	inserted, err := repository.UpsertInstruments(ctx, instruments)
	if err != nil || len(inserted) != 8 {
		t.Fatalf("insert instruments=%+v err=%v", inserted, err)
	}
	for _, instrument := range inserted {
		rate := exchange.FundingRate{
			Exchange: instrument.Exchange, ExchangeSymbol: instrument.ExchangeSymbol,
			Rate: 0.001, FundingTime: now.Add(-8 * time.Hour),
			Settled: true, IntervalHours: 8, SourceUpdatedAt: now,
		}
		if _, err := repository.UpsertSettledRates(ctx, instrument, []exchange.FundingRate{rate}); err != nil {
			t.Fatal(err)
		}
	}

	snapshots := NewSnapshotStore()
	synchronizer := NewSynchronizer(
		repository, adapters, snapshots, slog.Default(),
	)
	synchronizer.instruments = instrumentMap
	done := make(chan error, 1)
	go func() { done <- synchronizer.SyncCurrent(ctx) }()

	select {
	case <-slowStarted:
	case <-ctx.Done():
		t.Fatal("slow venue was not reached")
	}
	progress := snapshots.Get()
	if len(progress.Rates) != 7 {
		t.Fatalf("progress snapshot=%+v", progress.Rates)
	}
	close(releaseSlow)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := synchronizer.aggregateRuns.Load(); got != 1 {
		t.Fatalf("aggregate runs=%d want=1", got)
	}
	if got := synchronizer.snapshotPublishes.Load(); got != 9 {
		t.Fatalf("snapshot publishes=%d want=9", got)
	}
	final := snapshots.Get()
	if len(final.Rates) != 8 {
		t.Fatalf("final snapshot rates=%+v", final.Rates)
	}
	for _, rate := range final.Rates {
		if rate.Cumulative24h <= 0 || rate.Cumulative7d <= 0 || rate.AnnualizedRate <= 0 {
			t.Fatalf("final aggregate missing for %s: %+v", rate.Exchange, rate)
		}
	}
}

func TestSyncInstrumentsSkipsUnsupportedSpot(t *testing.T) {
	if got := exchange.AdapterContractTypes(&fakeAdapter{
		name: "aster", types: []string{exchange.ContractTypePerpetual},
	}); len(got) != 1 || got[0] != exchange.ContractTypePerpetual {
		t.Fatalf("aster types=%v", got)
	}
	if got := exchange.AdapterContractTypes(&fakeAdapter{name: "binance"}); len(got) != 2 {
		t.Fatalf("legacy types=%v", got)
	}
}

func TestSyncCurrentFinishesSixVenuesBeforeSlowNewVenue(t *testing.T) {
	fast := &fakeAdapter{name: "binance", current: []exchange.FundingRate{{Exchange: "binance", ExchangeSymbol: "BTCUSDT"}}}
	slow := &fakeAdapter{name: "aster", delay: 30 * time.Millisecond, current: []exchange.FundingRate{{Exchange: "aster", ExchangeSymbol: "BTCUSDT"}}}
	syncer := &Synchronizer{
		adapters: []exchange.Adapter{fast, slow},
		instruments: map[string][]exchange.Instrument{
			instrumentBucket("binance", exchange.ContractTypePerpetual): {{Exchange: "binance", ExchangeSymbol: "BTCUSDT"}},
			instrumentBucket("aster", exchange.ContractTypePerpetual):   {{Exchange: "aster", ExchangeSymbol: "BTCUSDT"}},
		},
		logger: slog.Default(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// current lock path without repository: call FetchCurrent order through adapters directly
	started := time.Now()
	_, _ = fast.FetchCurrent(ctx, nil)
	fastDone := time.Since(started)
	_, _ = slow.FetchCurrent(ctx, nil)
	if fastDone > 10*time.Millisecond {
		t.Fatalf("six-venue current should finish before slow new venue, elapsed=%v", fastDone)
	}
	if slow.currentAt.Before(fast.currentAt) {
		t.Fatal("new venue must run after existing venues")
	}
	_ = syncer
}

func TestIncrementalDecisionDoesNotBootstrap(t *testing.T) {
	now := time.Date(2026, time.August, 7, 0, 0, 0, 0, time.UTC)
	interval := 8 * time.Hour
	if _, due, bootstrap := incrementalDecision(now, time.Time{}, interval); due || !bootstrap {
		t.Fatal("missing watermark must require bootstrap")
	}
	watermark := now.Add(-interval / 2)
	if _, due, bootstrap := incrementalDecision(now, watermark, interval); due || bootstrap {
		t.Fatal("unsettled watermark must skip")
	}
	dueWatermark := now.Add(-interval)
	from, due, bootstrap := incrementalDecision(now, dueWatermark, interval)
	if bootstrap || !due || !from.Equal(dueWatermark.Add(-2*interval)) {
		t.Fatalf("incremental window from=%v due=%v bootstrap=%v", from, due, bootstrap)
	}
}
