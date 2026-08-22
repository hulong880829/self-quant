package orderstream

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

type fakeRead struct {
	payload []byte
	err     error
}

type fakeConnection struct {
	reads chan fakeRead
	done  chan struct{}
	once  sync.Once
}

func newFakeConnection() *fakeConnection {
	return &fakeConnection{reads: make(chan fakeRead, 8), done: make(chan struct{})}
}

func (c *fakeConnection) Read() ([]byte, error) {
	select {
	case <-c.done:
		return nil, io.EOF
	case result := <-c.reads:
		return result.payload, result.err
	}
}

func (c *fakeConnection) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

type fakeConnector struct {
	mu          sync.Mutex
	connections []*fakeConnection
	calls       int
	called      chan int
}

func (c *fakeConnector) Connect(
	ctx context.Context,
	_ Key,
	_ Credentials,
) (Connection, error) {
	c.mu.Lock()
	index := c.calls
	c.calls++
	c.mu.Unlock()
	select {
	case c.called <- index + 1:
	default:
	}
	if index >= len(c.connections) {
		return nil, errors.New("no fake connection")
	}
	return c.connections[index], nil
}

func newTestManager(t *testing.T, connector Connector) *Manager {
	t.Helper()
	manager, err := New(Options{
		Connector: connector, ReconnectInitial: time.Millisecond,
		ReconnectMax: 2 * time.Millisecond, StaleAfter: time.Second,
		IdleTimeout: -1,
		Jitter:      func(time.Duration) time.Duration { return 0 },
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func TestManagerSharesSessionAndRoutesClientOrder(t *testing.T) {
	t.Parallel()
	connection := newFakeConnection()
	connector := &fakeConnector{
		connections: []*fakeConnection{connection}, called: make(chan int, 4),
	}
	manager := newTestManager(t, connector)
	key := Key{Account: "acct", Venue: VenueBinance, Product: ProductSpot}
	credentials := Credentials{APIKey: "key", Secret: "secret"}

	first, err := manager.Subscribe(context.Background(), key, credentials)
	if err != nil {
		t.Fatalf("first subscribe: %v", err)
	}
	crossProductKey := key
	crossProductKey.Product = ProductPerpetual
	second, err := manager.Subscribe(context.Background(), crossProductKey, credentials)
	if err != nil {
		t.Fatalf("second subscribe: %v", err)
	}
	waitForConnection(t, connector.called, 1)
	waitForStatus(t, first.Updates(), StatusConnected)
	waitForStatus(t, second.Updates(), StatusConnected)

	watch, err := second.WatchOrder(context.Background(), "client-1")
	if err != nil {
		t.Fatalf("watch order: %v", err)
	}
	connection.reads <- fakeRead{payload: []byte(
		`{"e":"executionReport","E":1700000000123,"c":"client-1","i":42,"X":"FILLED","z":"2","Z":"20","t":7}`,
	)}
	update := waitForStatus(t, watch.Updates(), StatusFilled)
	if update.ClientOrderID != "client-1" || update.AveragePrice != "10" {
		t.Fatalf("unexpected routed update: %+v", update)
	}
	if !first.Healthy() || !second.Healthy() {
		t.Fatal("shared session should be healthy")
	}
	stats := manager.Stats()
	if stats.ActiveSessions != 1 || stats.References != 2 || stats.Watchers != 1 || stats.Connects != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	_ = watch.Close()
	_ = first.Close()
	if manager.Stats().ActiveSessions != 1 {
		t.Fatal("closing one reference closed the shared session")
	}
	_ = second.Close()
	if manager.Stats().ActiveSessions != 0 {
		t.Fatal("last close did not release the shared session")
	}
}

func TestManagerReconnectsAndNotifiesDisconnect(t *testing.T) {
	t.Parallel()
	firstConnection := newFakeConnection()
	secondConnection := newFakeConnection()
	connector := &fakeConnector{
		connections: []*fakeConnection{firstConnection, secondConnection},
		called:      make(chan int, 4),
	}
	manager := newTestManager(t, connector)
	subscription, err := manager.Subscribe(
		context.Background(),
		Key{Account: "acct", Venue: VenueBinance, Product: ProductPerpetual},
		Credentials{APIKey: "key", Secret: "secret"},
	)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	waitForConnection(t, connector.called, 1)
	waitForStatus(t, subscription.Updates(), StatusConnected)
	firstGeneration := subscription.Generation()
	firstConnection.reads <- fakeRead{err: errors.New("socket lost")}
	disconnected := waitForStatus(t, subscription.Updates(), StatusDisconnected)
	if disconnected.Error == "" || subscription.Healthy() {
		t.Fatalf("unexpected disconnected update: %+v", disconnected)
	}
	waitForConnection(t, connector.called, 2)
	waitForStatus(t, subscription.Updates(), StatusConnected)
	if !subscription.Healthy() {
		t.Fatal("session should recover after reconnect")
	}
	if subscription.Generation() <= firstGeneration {
		t.Fatal("reconnect did not advance stream generation")
	}
	stats := manager.Stats()
	if stats.Connects != 2 || stats.Reconnects != 1 || stats.Disconnects != 1 {
		t.Fatalf("unexpected reconnect stats: %+v", stats)
	}
}

func TestManagerRejectsCredentialChange(t *testing.T) {
	t.Parallel()
	connector := &fakeConnector{
		connections: []*fakeConnection{newFakeConnection()}, called: make(chan int, 1),
	}
	manager := newTestManager(t, connector)
	key := Key{Account: "acct", Venue: VenueOKX, Product: ProductSpot}
	_, err := manager.Subscribe(context.Background(), key, Credentials{APIKey: "one", Secret: "secret", Passphrase: "pass"})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	_, err = manager.Subscribe(context.Background(), key, Credentials{APIKey: "two", Secret: "secret", Passphrase: "pass"})
	if !errors.Is(err, ErrCredentialMismatch) {
		t.Fatalf("expected credential mismatch, got %v", err)
	}
}

func TestManagerReusesHealthyIdleSession(t *testing.T) {
	connection := newFakeConnection()
	connector := &fakeConnector{
		connections: []*fakeConnection{connection}, called: make(chan int, 2),
	}
	manager, err := New(Options{
		Connector: connector, ReconnectInitial: time.Millisecond,
		ReconnectMax: 2 * time.Millisecond, IdleTimeout: time.Second,
		Jitter: func(time.Duration) time.Duration { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	key := Key{Account: "acct", Venue: VenueBinance, Product: ProductSpot}
	credentials := Credentials{APIKey: "key", Secret: "secret"}
	first, err := manager.Subscribe(context.Background(), key, credentials)
	if err != nil {
		t.Fatal(err)
	}
	waitForConnection(t, connector.called, 1)
	waitForStatus(t, first.Updates(), StatusConnected)
	_ = first.Close()
	second, err := manager.Subscribe(context.Background(), key, credentials)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	select {
	case call := <-connector.called:
		t.Fatalf("idle session unexpectedly reconnected: call=%d", call)
	case <-time.After(20 * time.Millisecond):
	}
	if !second.Healthy() || manager.Stats().ActiveSessions != 1 {
		t.Fatalf("idle session was not reused: %+v", manager.Stats())
	}
}

func waitForConnection(t *testing.T, calls <-chan int, want int) {
	t.Helper()
	select {
	case got := <-calls:
		if got != want {
			t.Fatalf("connection call=%d, want %d", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for connection %d", want)
	}
}

func waitForStatus(t *testing.T, updates <-chan Update, status Status) Update {
	t.Helper()
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for {
		select {
		case update, ok := <-updates:
			if !ok {
				t.Fatalf("updates closed while waiting for %s", status)
			}
			if update.Status == status {
				return update
			}
		case <-timeout.C:
			t.Fatalf("timed out waiting for %s", status)
		}
	}
}
