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
	context.Context, ArbitrageExecution,
) (ArbitrageExecution, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	return ArbitrageExecution{}, false, nil
}

func (s *dryRunStore) GetActiveArbitrageExecution(
	context.Context, string, string,
) (ArbitrageExecution, error) {
	return ArbitrageExecution{}, ErrNotFound
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
		CompletedNotional: "0", MarketDataStale: true,
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
