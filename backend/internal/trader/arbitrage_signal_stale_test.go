package trader

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

func TestManagerLatestAllowsBBOAgeInsideConnectionStale(t *testing.T) {
	ctx := context.Background()
	keyA, _ := marketdata.NewKey("bybit", "perpetual", "COTIUSDT")
	keyB, _ := marketdata.NewKey("bitget", "perpetual", "COTIUSDT")
	market, connectionA, connectionB := newAgedSignalMarket(t, 3*time.Second, 60*time.Second)
	if _, err := market.Subscribe(ctx, keyA); err != nil {
		t.Fatal(err)
	}
	if _, err := market.Subscribe(ctx, keyB); err != nil {
		t.Fatal(err)
	}
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")
	waitForFreshBBO(t, market, keyA)
	waitForFreshBBO(t, market, keyB)
	if _, err := market.Latest(keyA); err != nil {
		t.Fatalf("Latest A with 3s age under 60s stale: %v", err)
	}
	if _, err := market.Latest(keyB); err != nil {
		t.Fatalf("Latest B with 3s age under 60s stale: %v", err)
	}
}

func TestArbitrageSchedulerRejectsNewSignalWhenBBOOlderThanTwoSeconds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	market, connectionA, connectionB := newAgedSignalMarket(t, 3*time.Second, 60*time.Second)
	store := &dryRunStore{claim: true, item: signalStaleCombination()}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")
	waitForFreshBBO(t, market, marketdata.Key{Venue: "bybit", Product: "perpetual", Symbol: "COTIUSDT"})
	waitForFreshBBO(t, market, marketdata.Key{Venue: "bitget", Product: "perpetual", Symbol: "COTIUSDT"})
	staleReads := scheduler.Stats().StaleBBOReads
	scheduler.runOnce(ctx)
	defer scheduler.closeAll()

	select {
	case <-runner.executed:
		t.Fatal("3s BBO created a new execution")
	default:
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claims != 0 {
		t.Fatalf("claims=%d, want 0", store.claims)
	}
	if store.item.MarketDataStale {
		t.Fatal("2s signal stale was recorded as MarketDataStale")
	}
	stats := scheduler.Stats()
	if stats.SignalStaleRejected == 0 {
		t.Fatal("signal_stale_rejected_total did not increase")
	}
	if stats.StaleBBOReads != staleReads {
		t.Fatalf("signal stale incremented stale_bbo_reads_total %d -> %d", staleReads, stats.StaleBBOReads)
	}
}

func TestArbitrageSchedulerClaimsWhenBothLegsWithinTwoSeconds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	market, connectionA, connectionB := newAgedSignalMarket(t, 0, 60*time.Second)
	store := &dryRunStore{claim: true, item: signalStaleCombination()}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")
	waitForFreshBBO(t, market, marketdata.Key{Venue: "bybit", Product: "perpetual", Symbol: "COTIUSDT"})
	waitForFreshBBO(t, market, marketdata.Key{Venue: "bitget", Product: "perpetual", Symbol: "COTIUSDT"})
	scheduler.runOnce(ctx)
	defer scheduler.closeAll()
	select {
	case <-runner.executed:
	case <-time.After(time.Second):
		t.Fatal("fresh BBO within 2s did not claim")
	}
	if scheduler.Stats().SignalStaleRejected != 0 {
		t.Fatalf("signal stale rejected=%d", scheduler.Stats().SignalStaleRejected)
	}
}

func TestArbitrageSchedulerResumesActiveExecutionWhenSignalBBOStale(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	market, connectionA, connectionB := newAgedSignalMarket(t, 3*time.Second, 60*time.Second)
	item := signalStaleCombination()
	execution := ArbitrageExecution{
		ID: "execution-1", CombinationID: item.ID, Status: "hedging",
	}
	store := &dryRunStore{claim: true, item: item, activeExecution: &execution}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")
	waitForFreshBBO(t, market, marketdata.Key{Venue: "bybit", Product: "perpetual", Symbol: "COTIUSDT"})
	waitForFreshBBO(t, market, marketdata.Key{Venue: "bitget", Product: "perpetual", Symbol: "COTIUSDT"})
	scheduler.runOnce(ctx)
	defer scheduler.closeAll()
	select {
	case got := <-runner.executed:
		if got.ID != execution.ID {
			t.Fatalf("resumed=%+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("active execution was blocked by 2s signal stale")
	}
	if store.claims != 0 {
		t.Fatalf("resume claimed a new execution: %d", store.claims)
	}
}

func TestArbitrageSchedulerClosingIgnoresSignalBBOStale(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	market, _, _ := newAgedSignalMarket(t, 3*time.Second, 60*time.Second)
	item := signalStaleCombination()
	item.Status = "closing"
	item.RuntimeState = "closing"
	store := &dryRunStore{item: item}
	runner := &schedulerRunner{closed: make(chan ArbitrageCombination, 1)}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	scheduler.runOnce(ctx)
	defer scheduler.closeAll()
	select {
	case closed := <-runner.closed:
		if closed.ID != item.ID {
			t.Fatalf("closed=%+v", closed)
		}
	case <-time.After(time.Second):
		t.Fatal("closing was blocked by 2s signal stale")
	}
	if scheduler.Stats().SignalStaleRejected != 0 {
		t.Fatalf("closing incremented signal stale rejected")
	}
}

func TestArbitrageSchedulerRechecksSignalStaleBeforeClaim(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	market, connectionA, connectionB := newAgedSignalMarket(t, 1900*time.Millisecond, 60*time.Second)
	item := signalStaleCombination()
	item.LegA.InstrumentID = 101
	item.LegB.InstrumentID = 202
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	instrument := func(id int64) Instrument {
		return Instrument{
			ID: id, Exchange: "binance", ContractType: "perpetual",
			QuantityStep: "1", PriceTick: "0.1",
			MinQuantity: "1", MinNotional: "1",
			MinQuantityStatus:  exchange.ConstraintKnown,
			MaxQuantityStatus:  exchange.ConstraintNotApplicable,
			MinNotionalStatus:  exchange.ConstraintKnown,
			MarketQuantityStep: "1", MarketMinQuantity: "1",
			MarketMinNotional:        "1",
			MarketQuantityStepStatus: exchange.ConstraintKnown,
			MarketMinQuantityStatus:  exchange.ConstraintKnown,
			MarketMaxQuantityStatus:  exchange.ConstraintNotApplicable,
			MarketMinNotionalStatus:  exchange.ConstraintKnown,
		}
	}
	scheduler.ConfigureArbitrageInstruments(delayingCatalog{
		silentRiskCatalog: silentRiskCatalog{101: instrument(101), 202: instrument(202)},
		delay:             200 * time.Millisecond,
	})
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")
	waitForFreshBBO(t, market, marketdata.Key{Venue: "bybit", Product: "perpetual", Symbol: "COTIUSDT"})
	waitForFreshBBO(t, market, marketdata.Key{Venue: "bitget", Product: "perpetual", Symbol: "COTIUSDT"})
	scheduler.runOnce(ctx)
	defer scheduler.closeAll()
	select {
	case <-runner.executed:
		t.Fatal("catalog delay allowed a stale claim")
	default:
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claims != 0 {
		t.Fatalf("claims=%d after delayed catalog", store.claims)
	}
	if scheduler.Stats().SignalStaleRejected == 0 {
		t.Fatal("second signal stale check did not increment")
	}
}

type delayingCatalog struct {
	silentRiskCatalog
	delay time.Duration
}

func (c delayingCatalog) Get(ctx context.Context, id int64) (Instrument, error) {
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	return c.silentRiskCatalog.Get(ctx, id)
}

func signalStaleCombination() ArbitrageCombination {
	return ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61800", OwnerUsername: "admin",
		Status: "running", AskThresholdBps: "10", BidThresholdBps: "-10",
		TargetNotional: "10000", OrderNotional: "500", PositionNotional: "0",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, Exchange: "bybit",
			ContractType: "perpetual", ExchangeSymbol: "COTIUSDT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, Exchange: "bitget",
			ContractType: "perpetual", ExchangeSymbol: "COTIUSDT",
		},
	}
}

func newAgedSignalMarket(
	t *testing.T,
	age, staleAfter time.Duration,
) (*marketdata.Manager, *schedulerConnection, *schedulerConnection) {
	t.Helper()
	keyA, _ := marketdata.NewKey("bybit", "perpetual", "COTIUSDT")
	keyB, _ := marketdata.NewKey("bitget", "perpetual", "COTIUSDT")
	connectionA := &schedulerConnection{reads: make(chan []byte, 4), done: make(chan struct{})}
	connectionB := &schedulerConnection{reads: make(chan []byte, 4), done: make(chan struct{})}
	connector := &schedulerConnector{connections: map[marketdata.Key]*schedulerConnection{
		keyA: connectionA, keyB: connectionB,
	}}
	parser := func(
		key marketdata.Key, payload []byte, received time.Time,
	) (marketdata.BBO, bool, error) {
		parts := strings.Split(string(payload), ",")
		if len(parts) != 2 {
			return marketdata.BBO{}, false, fmt.Errorf("bad BBO fixture")
		}
		return marketdata.BBO{
			Key: key, BidPrice: parts[0], AskPrice: parts[1],
			BidQuantity: "10000", AskQuantity: "10000",
			VenueTimestamp: received, ReceiveTimestamp: received.Add(-age),
		}, true, nil
	}
	market, err := marketdata.New(marketdata.Options{
		Connector: connector,
		Parsers: map[string]marketdata.Parser{
			"bybit": parser, "bitget": parser,
		},
		StaleAfter: staleAfter, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = market.Close() })
	return market, connectionA, connectionB
}
