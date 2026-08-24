package trader

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"selfquant/backend/internal/trader/marketdata"
)

type dryRunStore struct {
	arbitrageStore
	mu         sync.Mutex
	leased     bool
	item       ArbitrageCombination
	eventTypes []string
	claims     int
	claim      bool
}

func (s *dryRunStore) LeaseArbitrageCombinations(
	context.Context, int, time.Duration,
) ([]ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leased {
		return nil, nil
	}
	s.leased = true
	return []ArbitrageCombination{s.item}, nil
}

func (s *dryRunStore) UpdateArbitrageMarketSnapshot(
	_ context.Context,
	_ string,
	ask, bid string,
	stale bool,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.item.CurrentAskSpreadBps = ask
	s.item.CurrentBidSpreadBps = bid
	s.item.MarketDataStale = stale
	return s.item, nil
}

func (s *dryRunStore) AppendArbitrageEvent(
	_ context.Context,
	_, _, eventType string,
	_ map[string]any,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventTypes = append(s.eventTypes, eventType)
	return nil
}

func (s *dryRunStore) ClaimArbitrageExecution(
	_ context.Context, execution ArbitrageExecution,
) (ArbitrageExecution, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	return execution, s.claim, nil
}

func (s *dryRunStore) GetActiveArbitrageExecution(
	context.Context, string,
) (ArbitrageExecution, error) {
	return ArbitrageExecution{}, ErrNotFound
}

func (s *dryRunStore) GetArbitrageCombinationByOwner(
	_ context.Context, _, _ string,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.item, nil
}

func (s *dryRunStore) RenewArbitrageLease(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}

type schedulerConnection struct {
	reads chan []byte
	done  chan struct{}
	once  sync.Once
}

func (c *schedulerConnection) Read() ([]byte, error) {
	select {
	case payload := <-c.reads:
		return payload, nil
	case <-c.done:
		return nil, io.EOF
	}
}

func (c *schedulerConnection) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

type schedulerConnector struct {
	mu          sync.Mutex
	connections map[marketdata.Key]*schedulerConnection
}

type schedulerRunner struct {
	executed chan ArbitrageExecution
}

func (r *schedulerRunner) Execute(
	_ context.Context,
	_ ArbitrageCombination,
	execution ArbitrageExecution,
	_, _ marketdata.BBO,
) {
	r.executed <- execution
}

func (*schedulerRunner) CloseCombination(context.Context, ArbitrageCombination) error {
	return nil
}

func (c *schedulerConnector) Connect(
	_ context.Context,
	key marketdata.Key,
) (marketdata.Connection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	connection := c.connections[key]
	if connection == nil {
		return nil, fmt.Errorf("missing connection for %v", key)
	}
	return connection, nil
}

func TestArbitrageSchedulerDryRunRecordsTriggerWithoutClaimingExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keyA, _ := marketdata.NewKey("binance", "perpetual", "BTCUSDT")
	keyB, _ := marketdata.NewKey("okx", "perpetual", "BTC-USDT-SWAP")
	connectionA := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connectionB := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connector := &schedulerConnector{connections: map[marketdata.Key]*schedulerConnection{
		keyA: connectionA, keyB: connectionB,
	}}
	parser := func(key marketdata.Key, payload []byte, received time.Time) (marketdata.BBO, bool, error) {
		parts := strings.Split(string(payload), ",")
		if len(parts) != 2 {
			return marketdata.BBO{}, false, fmt.Errorf("bad fixture")
		}
		if _, err := strconv.ParseFloat(parts[0], 64); err != nil {
			return marketdata.BBO{}, false, err
		}
		return marketdata.BBO{
			Key: key, BidPrice: parts[0], AskPrice: parts[1],
			VenueTimestamp: received, ReceiveTimestamp: received,
		}, true, nil
	}
	market, err := marketdata.New(marketdata.Options{
		Connector: connector,
		Parsers: map[string]marketdata.Parser{
			"binance": parser, "okx": parser,
		},
		StaleAfter: time.Second, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer market.Close()
	store := &dryRunStore{item: ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61731", OwnerUsername: "admin",
		Status: "running", AskThresholdBps: "12", BidThresholdBps: "-8",
		TargetNotional: "10000", OrderNotional: "500", PositionNotional: "0",
		MarketDataStale: true,
		LegA: ArbitrageLeg{
			TradingAccountID: 1, Exchange: "binance",
			ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
		},
	}}
	scheduler := NewArbitrageScheduler(
		store, market, nil, time.Millisecond, time.Second,
		10, 2, 2, 2, true, nil,
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("100,101")
	connectionB.reads <- []byte("101,102")
	deadline := time.Now().Add(time.Second)
	for {
		if _, errA := market.Latest(keyA); errA == nil {
			if _, errB := market.Latest(keyB); errB == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("BBO fixtures were not consumed")
		}
		time.Sleep(time.Millisecond)
	}
	scheduler.runOnce(ctx)
	scheduler.closeAll()

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claims != 0 {
		t.Fatalf("dry-run claimed %d executions", store.claims)
	}
	if len(store.eventTypes) != 1 || store.eventTypes[0] != "dry_run_trigger" {
		t.Fatalf("events=%v", store.eventTypes)
	}
}

func TestArbitrageSchedulerLiveModeClaimsAndExecutesFreshTrigger(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keyA, _ := marketdata.NewKey("bybit", "perpetual", "COTIUSDT")
	keyB, _ := marketdata.NewKey("bitget", "perpetual", "COTIUSDT")
	connectionA := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connectionB := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connector := &schedulerConnector{connections: map[marketdata.Key]*schedulerConnection{
		keyA: connectionA, keyB: connectionB,
	}}
	parser := func(key marketdata.Key, payload []byte, received time.Time) (marketdata.BBO, bool, error) {
		parts := strings.Split(string(payload), ",")
		return marketdata.BBO{
			Key: key, BidPrice: parts[0], AskPrice: parts[1],
			VenueTimestamp: received, ReceiveTimestamp: received,
		}, true, nil
	}
	market, err := marketdata.New(marketdata.Options{
		Connector: connector,
		Parsers: map[string]marketdata.Parser{
			"bybit": parser, "bitget": parser,
		},
		StaleAfter: time.Second, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer market.Close()
	store := &dryRunStore{claim: true, item: ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61732", OwnerUsername: "admin",
		Status: "running", AskThresholdBps: "10", BidThresholdBps: "-10",
		TargetNotional: "10000", OrderNotional: "500", PositionNotional: "0",
		MarketDataStale: true,
		LegA: ArbitrageLeg{
			TradingAccountID: 1, Exchange: "bybit",
			ContractType: "perpetual", ExchangeSymbol: "COTIUSDT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, Exchange: "bitget",
			ContractType: "perpetual", ExchangeSymbol: "COTIUSDT",
		},
	}}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := NewArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")
	deadline := time.Now().Add(time.Second)
	for {
		if _, errA := market.Latest(keyA); errA == nil {
			if _, errB := market.Latest(keyB); errB == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("BBO fixtures were not consumed")
		}
		time.Sleep(time.Millisecond)
	}
	scheduler.runOnce(ctx)
	defer scheduler.closeAll()

	select {
	case execution := <-runner.executed:
		if execution.Direction != "ask" || execution.RequestedNotional != "500" ||
			execution.PositionEffect != "open" || execution.ReduceOnly {
			t.Fatalf("execution=%+v", execution)
		}
	case <-time.After(time.Second):
		t.Fatal("live trigger was not executed")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claims != 1 {
		t.Fatalf("claims=%d", store.claims)
	}
}

func TestArbitrageSchedulerDoesNotTriggerWithStaleLeg(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keyA, _ := marketdata.NewKey("bybit", "perpetual", "COTIUSDT")
	keyB, _ := marketdata.NewKey("bitget", "perpetual", "COTIUSDT")
	connectionA := &schedulerConnection{reads: make(chan []byte, 1), done: make(chan struct{})}
	connectionB := &schedulerConnection{reads: make(chan []byte, 1), done: make(chan struct{})}
	connector := &schedulerConnector{connections: map[marketdata.Key]*schedulerConnection{
		keyA: connectionA, keyB: connectionB,
	}}
	market, err := marketdata.New(marketdata.Options{
		Connector: connector,
		Parsers: map[string]marketdata.Parser{
			"bybit": func(key marketdata.Key, payload []byte, received time.Time) (marketdata.BBO, bool, error) {
				return marketdata.BBO{
					Key: key, BidPrice: "100", AskPrice: "100",
					ReceiveTimestamp: received,
				}, true, nil
			},
			"bitget": testStaleParser,
		},
		StaleAfter: time.Millisecond, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer market.Close()
	store := &dryRunStore{claim: true, item: ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61733", OwnerUsername: "admin",
		Status: "running", AskThresholdBps: "-100", BidThresholdBps: "100",
		TargetNotional: "10000", OrderNotional: "500", PositionNotional: "0",
		LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "bybit", ContractType: "perpetual", ExchangeSymbol: "COTIUSDT"},
		LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "bitget", ContractType: "perpetual", ExchangeSymbol: "COTIUSDT"},
	}}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := NewArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("fresh")
	connectionB.reads <- []byte("stale")
	deadline := time.Now().Add(time.Second)
	for market.Stats().Updates < 2 {
		if time.Now().After(deadline) {
			t.Fatal("BBO fixtures were not consumed")
		}
		time.Sleep(time.Millisecond)
	}
	scheduler.runOnce(ctx)
	scheduler.closeAll()

	select {
	case <-runner.executed:
		t.Fatal("stale BBO triggered execution")
	default:
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claims != 0 {
		t.Fatalf("stale BBO claims=%d", store.claims)
	}
}

func testStaleParser(key marketdata.Key, _ []byte, received time.Time) (marketdata.BBO, bool, error) {
	return marketdata.BBO{
		Key: key, BidPrice: "101", AskPrice: "101",
		ReceiveTimestamp: received.Add(-time.Second),
	}, true, nil
}

func TestArbitrageSchedulerSkipsAskAtPositiveCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keyA, _ := marketdata.NewKey("bybit", "perpetual", "COTIUSDT")
	keyB, _ := marketdata.NewKey("bitget", "perpetual", "COTIUSDT")
	connectionA := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connectionB := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connector := &schedulerConnector{connections: map[marketdata.Key]*schedulerConnection{
		keyA: connectionA, keyB: connectionB,
	}}
	parser := func(key marketdata.Key, payload []byte, received time.Time) (marketdata.BBO, bool, error) {
		parts := strings.Split(string(payload), ",")
		return marketdata.BBO{
			Key: key, BidPrice: parts[0], AskPrice: parts[1],
			VenueTimestamp: received, ReceiveTimestamp: received,
		}, true, nil
	}
	market, err := marketdata.New(marketdata.Options{
		Connector:  connector,
		Parsers:    map[string]marketdata.Parser{"bybit": parser, "bitget": parser},
		StaleAfter: time.Second, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer market.Close()
	store := &dryRunStore{claim: true, item: ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61736", OwnerUsername: "admin",
		Status: "running", AskThresholdBps: "10", BidThresholdBps: "-10",
		TargetNotional: "10000", OrderNotional: "500", PositionNotional: "10000",
		LegA: ArbitrageLeg{TradingAccountID: 1, Exchange: "bybit", ContractType: "perpetual", ExchangeSymbol: "COTIUSDT"},
		LegB: ArbitrageLeg{TradingAccountID: 2, Exchange: "bitget", ContractType: "perpetual", ExchangeSymbol: "COTIUSDT"},
	}}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := NewArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")
	deadline := time.Now().Add(time.Second)
	for {
		if _, errA := market.Latest(keyA); errA == nil {
			if _, errB := market.Latest(keyB); errB == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("BBO fixtures were not consumed")
		}
		time.Sleep(time.Millisecond)
	}
	scheduler.runOnce(ctx)
	scheduler.closeAll()
	select {
	case <-runner.executed:
		t.Fatal("capped ask position triggered execution")
	default:
	}
	if store.claims != 0 {
		t.Fatalf("capped claims=%d", store.claims)
	}
}

func TestArbitrageSchedulerSkipsDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keyA, _ := marketdata.NewKey("bybit", "perpetual", "COTIUSDT")
	keyB, _ := marketdata.NewKey("bitget", "perpetual", "COTIUSDT")
	connectionA := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connectionB := &schedulerConnection{reads: make(chan []byte, 2), done: make(chan struct{})}
	connector := &schedulerConnector{connections: map[marketdata.Key]*schedulerConnection{
		keyA: connectionA, keyB: connectionB,
	}}
	parser := func(key marketdata.Key, payload []byte, received time.Time) (marketdata.BBO, bool, error) {
		parts := strings.Split(string(payload), ",")
		return marketdata.BBO{
			Key: key, BidPrice: parts[0], AskPrice: parts[1],
			VenueTimestamp: received, ReceiveTimestamp: received,
		}, true, nil
	}
	market, err := marketdata.New(marketdata.Options{
		Connector:  connector,
		Parsers:    map[string]marketdata.Parser{"bybit": parser, "bitget": parser},
		StaleAfter: time.Second, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer market.Close()
	store := &dryRunStore{claim: true, item: ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61737", OwnerUsername: "admin",
		Status: "running", AskThresholdBps: "10", BidThresholdBps: "-10",
		TargetNotional: "10000", OrderNotional: "500", PositionNotional: "0",
		NextRetryAt: time.Now().UTC().Add(time.Hour),
		LegA:        ArbitrageLeg{TradingAccountID: 1, Exchange: "bybit", ContractType: "perpetual", ExchangeSymbol: "COTIUSDT"},
		LegB:        ArbitrageLeg{TradingAccountID: 2, Exchange: "bitget", ContractType: "perpetual", ExchangeSymbol: "COTIUSDT"},
	}}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := NewArbitrageScheduler(
		store, market, runner, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	scheduler.runOnce(ctx)
	connectionA.reads <- []byte("100,100.1")
	connectionB.reads <- []byte("100.3,100.4")
	deadline := time.Now().Add(time.Second)
	for {
		if _, errA := market.Latest(keyA); errA == nil {
			if _, errB := market.Latest(keyB); errB == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("BBO fixtures were not consumed")
		}
		time.Sleep(time.Millisecond)
	}
	scheduler.runOnce(ctx)
	scheduler.closeAll()
	if store.claims != 0 {
		t.Fatalf("backoff claims=%d", store.claims)
	}
}
