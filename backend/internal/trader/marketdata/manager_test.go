package marketdata

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeRead struct {
	payload []byte
	err     error
}

type fakeConnection struct {
	reads  chan fakeRead
	closed chan struct{}
	once   sync.Once
}

func newFakeConnection() *fakeConnection {
	return &fakeConnection{
		reads:  make(chan fakeRead, 16),
		closed: make(chan struct{}),
	}
}

func (c *fakeConnection) Read() ([]byte, error) {
	select {
	case item := <-c.reads:
		return item.payload, item.err
	case <-c.closed:
		return nil, io.EOF
	}
}

func (c *fakeConnection) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

type fakeConnector struct {
	mu          sync.Mutex
	connections []*fakeConnection
	failures    map[int]error
	connects    int
	connected   chan int
}

func (c *fakeConnector) Connect(context.Context, Key) (Connection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	index := c.connects
	c.connects++
	if err := c.failures[index]; err != nil {
		select {
		case c.connected <- c.connects:
		default:
		}
		return nil, err
	}
	if index >= len(c.connections) {
		return nil, errors.New("no fake connection")
	}
	select {
	case c.connected <- c.connects:
	default:
	}
	return c.connections[index], nil
}

func testParser(key Key, payload []byte, received time.Time) (BBO, bool, error) {
	if len(payload) == 0 {
		return BBO{}, false, nil
	}
	price := string(payload)
	return BBO{
		Key: key, BidPrice: price, AskPrice: price,
		VenueTimestamp: received.Add(-time.Millisecond), ReceiveTimestamp: received,
	}, true, nil
}

func TestManagerDeduplicatesAndKeepsLatestWithoutBlocking(t *testing.T) {
	t.Parallel()
	connection := newFakeConnection()
	connector := &fakeConnector{
		connections: []*fakeConnection{connection},
		connected:   make(chan int, 4),
	}
	var nowNanos atomic.Int64
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	nowNanos.Store(base.UnixNano())
	manager, err := New(Options{
		Connector:        connector,
		Parsers:          map[string]Parser{VenueBinance: testParser},
		StaleAfter:       time.Second,
		ReconnectInitial: time.Hour,
		ReconnectMax:     time.Hour,
		Now: func() time.Time {
			return time.Unix(0, nowNanos.Load())
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	key := Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "BTCUSDT"}
	first, err := manager.Subscribe(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Subscribe(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case count := <-connector.connected:
		if count != 1 {
			t.Fatalf("connect count = %d, want 1", count)
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not connect")
	}

	connection.reads <- fakeRead{payload: []byte("1")}
	connection.reads <- fakeRead{payload: []byte("2")}
	connection.reads <- fakeRead{payload: []byte("3")}
	waitForPrice(t, manager, key, "3")

	select {
	case update := <-first.Updates():
		if update.BidPrice != "3" {
			t.Fatalf("slow subscriber received %q, want latest value 3", update.BidPrice)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive update")
	}
	select {
	case update := <-second.Updates():
		if update.BidPrice != "3" {
			t.Fatalf("second subscriber received %q, want 3", update.BidPrice)
		}
	case <-time.After(time.Second):
		t.Fatal("second subscriber did not receive update")
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connection.closed:
		t.Fatal("connection closed while one reference remained")
	default:
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connection.closed:
	case <-time.After(time.Second):
		t.Fatal("connection remained open after last reference")
	}

	third, err := manager.Subscribe(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	if _, err := third.Latest(); !errors.Is(err, ErrNoValue) {
		t.Fatalf("new stream Latest error = %v, want ErrNoValue", err)
	}
}

func TestManagerReconnectsAndRestoresSubscription(t *testing.T) {
	t.Parallel()
	firstConnection := newFakeConnection()
	secondConnection := newFakeConnection()
	connector := &fakeConnector{
		connections: []*fakeConnection{firstConnection, secondConnection},
		connected:   make(chan int, 4),
	}
	manager, err := New(Options{
		Connector:        connector,
		Parsers:          map[string]Parser{VenueBinance: testParser},
		ReconnectInitial: time.Millisecond,
		ReconnectMax:     2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	key := Key{Venue: VenueBinance, Product: ProductPerpetual, Symbol: "ETHUSDT"}
	subscription, err := manager.Subscribe(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	waitForConnectCount(t, connector.connected, 1)
	firstConnection.reads <- fakeRead{err: io.ErrUnexpectedEOF}
	waitForConnectCount(t, connector.connected, 2)
	secondConnection.reads <- fakeRead{payload: []byte("42")}

	select {
	case update := <-subscription.Updates():
		if update.BidPrice != "42" {
			t.Fatalf("reconnected stream price = %q, want 42", update.BidPrice)
		}
	case <-time.After(time.Second):
		t.Fatal("restored subscription did not publish")
	}
}

func TestManagerRetriesInitialConnectionFailure(t *testing.T) {
	t.Parallel()
	connection := newFakeConnection()
	connector := &fakeConnector{
		connections: []*fakeConnection{nil, connection},
		failures:    map[int]error{0: errors.New("handshake rejected")},
		connected:   make(chan int, 4),
	}
	manager, err := New(Options{
		Connector:        connector,
		Parsers:          map[string]Parser{VenueBitget: testParser},
		ReconnectInitial: time.Millisecond,
		ReconnectMax:     2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	key := Key{Venue: VenueBitget, Product: ProductPerpetual, Symbol: "COTIUSDT"}
	subscription, err := manager.Subscribe(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	waitForConnectCount(t, connector.connected, 1)
	waitForConnectCount(t, connector.connected, 2)
	connection.reads <- fakeRead{payload: []byte("42")}
	waitForPrice(t, manager, key, "42")
}

func TestManagerStaleCheckUsesReceiveTimestamp(t *testing.T) {
	t.Parallel()
	connection := newFakeConnection()
	connector := &fakeConnector{
		connections: []*fakeConnection{connection},
		connected:   make(chan int, 1),
	}
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	var nowNanos atomic.Int64
	nowNanos.Store(base.UnixNano())
	manager, err := New(Options{
		Connector:  connector,
		Parsers:    map[string]Parser{VenueBinance: testParser},
		StaleAfter: 2 * time.Second,
		Now: func() time.Time {
			return time.Unix(0, nowNanos.Load())
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	key := Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "SOLUSDT"}
	subscription, err := manager.Subscribe(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	waitForConnectCount(t, connector.connected, 1)
	connection.reads <- fakeRead{payload: []byte("100")}
	waitForPrice(t, manager, key, "100")

	nowNanos.Store(base.Add(2 * time.Second).UnixNano())
	if _, err := manager.Latest(key); err != nil {
		t.Fatalf("Latest at stale boundary: %v", err)
	}
	connection.reads <- fakeRead{payload: []byte("101")}
	waitForPrice(t, manager, key, "101")

	nowNanos.Store(base.Add(4*time.Second + time.Nanosecond).UnixNano())
	value, err := manager.Latest(key)
	if !errors.Is(err, ErrStale) || value.BidPrice != "101" {
		t.Fatalf("Latest = (%#v, %v), want value and ErrStale", value, err)
	}
	if !manager.IsStale(key) {
		t.Fatal("IsStale returned false")
	}
}

func waitForPrice(t *testing.T, manager *Manager, key Key, price string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		value, err := manager.Latest(key)
		if err == nil && value.BidPrice == price {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("price did not become %q", price)
}

func waitForConnectCount(t *testing.T, connected <-chan int, want int) {
	t.Helper()
	for {
		select {
		case got := <-connected:
			if got == want {
				return
			}
		case <-time.After(time.Second):
			t.Fatalf("connect count did not become %d", want)
		}
	}
}
