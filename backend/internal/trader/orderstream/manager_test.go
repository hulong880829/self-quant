package orderstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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
	return newTestManagerWithLogger(t, connector, nil)
}

func newTestManagerWithLogger(t *testing.T, connector Connector, logger *slog.Logger) *Manager {
	t.Helper()
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	manager, err := New(Options{
		Connector: connector, ReconnectInitial: time.Millisecond,
		ReconnectMax: 2 * time.Millisecond, StaleAfter: time.Second,
		IdleTimeout: -1,
		Jitter:      func(time.Duration) time.Duration { return 0 },
		Logger:      logger,
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

func TestCredentialEqualityUsesOptionalIndexValues(t *testing.T) {
	t.Parallel()
	firstAccount, secondAccount := int64(0), int64(0)
	firstKey, secondKey := int32(0), int32(0)
	first := Credentials{
		Secret: "secret", AccountIndex: &firstAccount, APIKeyIndex: &firstKey,
	}
	second := Credentials{
		Secret: "secret", AccountIndex: &secondAccount, APIKeyIndex: &secondKey,
	}
	if !credentialsEqual(first, second) {
		t.Fatal("equal set index values were treated as different credentials")
	}
	second.AccountIndex = nil
	if credentialsEqual(first, second) {
		t.Fatal("unset and explicitly zero indexes were treated as equal")
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

func TestManagerSuppressesDEXDuplicatesAndTerminalRegression(t *testing.T) {
	t.Parallel()
	connection := newFakeConnection()
	connector := &fakeConnector{
		connections: []*fakeConnection{connection}, called: make(chan int, 1),
	}
	manager := newTestManager(t, connector)
	subscription, err := manager.Subscribe(
		context.Background(),
		Key{Account: "acct", Venue: VenueHyperliquid, Product: ProductPerpetual},
		Credentials{Secret: "agent-key", SigningAddress: "0xuser"},
	)
	if err != nil {
		t.Fatal(err)
	}
	waitForConnection(t, connector.called, 1)
	waitForStatus(t, subscription.Updates(), StatusConnected)
	watch, err := subscription.WatchOrder(context.Background(), "0xclient")
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	open := `{"channel":"orderUpdates","data":[{
		"order":{"oid":49,"cloid":"0xclient","origSz":"2","sz":"1","timestamp":10},
		"status":"open","statusTimestamp":10
	}]}`
	connection.reads <- fakeRead{payload: []byte(open)}
	connection.reads <- fakeRead{payload: []byte(open)}
	connection.reads <- fakeRead{payload: []byte(`{"channel":"orderUpdates","data":[{
		"order":{"oid":49,"cloid":"0xclient","origSz":"2","sz":"1.5","timestamp":9},
		"status":"open","statusTimestamp":9
	}]}`)}
	connection.reads <- fakeRead{payload: []byte(`{"channel":"orderUpdates","data":[{
		"order":{"oid":49,"cloid":"0xclient","origSz":"2","sz":"0","filledSz":"2","timestamp":11},
		"status":"filled","statusTimestamp":11
	}]}`)}
	connection.reads <- fakeRead{payload: []byte(`{"channel":"orderUpdates","data":[{
		"order":{"oid":49,"cloid":"0xclient","origSz":"2","sz":"1","timestamp":12},
		"status":"open","statusTimestamp":12
	}]}`)}
	first := waitForStatus(t, watch.Updates(), StatusNew)
	if first.CumulativeFilled != "1" {
		t.Fatalf("open update=%+v", first)
	}
	terminal := waitForStatus(t, watch.Updates(), StatusFilled)
	if terminal.CumulativeFilled != "2" {
		t.Fatalf("terminal update=%+v", terminal)
	}
	select {
	case update := <-watch.Updates():
		t.Fatalf("unexpected duplicate/regression: %+v", update)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestManagerRoutesHyperliquidFillByKnownVenueOrder(t *testing.T) {
	t.Parallel()
	connection := newFakeConnection()
	connector := &fakeConnector{
		connections: []*fakeConnection{connection}, called: make(chan int, 1),
	}
	manager := newTestManager(t, connector)
	subscription, err := manager.Subscribe(
		context.Background(),
		Key{Account: "acct", Venue: VenueHyperliquid, Product: ProductPerpetual},
		Credentials{Secret: "agent-key", SigningAddress: "0xuser"},
	)
	if err != nil {
		t.Fatal(err)
	}
	waitForConnection(t, connector.called, 1)
	waitForStatus(t, subscription.Updates(), StatusConnected)
	watch, err := subscription.WatchOrder(context.Background(), "0xclient")
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	connection.reads <- fakeRead{payload: []byte(`{"channel":"orderUpdates","data":[{
		"order":{"oid":49,"cloid":"0xclient","origSz":"2","sz":"2"},
		"status":"open"
	}]}`)}
	waitForStatus(t, watch.Updates(), StatusNew)
	connection.reads <- fakeRead{payload: []byte(`{"channel":"userFills","data":{"fills":[{
		"oid":49,"tid":7,"sz":"0.5","px":"100","time":1700000000123
	}]}}`)}
	select {
	case update := <-watch.Updates():
		if update.Type != UpdateTrade || update.ClientOrderID != "0xclient" ||
			update.TradeID != "7" || update.LastFilled != "0.5" {
			t.Fatalf("fill update=%+v", update)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for routed fill")
	}
}

func TestManagerBindsACKVenueOrderForHyperliquidFill(t *testing.T) {
	t.Parallel()
	connection := newFakeConnection()
	connector := &fakeConnector{
		connections: []*fakeConnection{connection}, called: make(chan int, 1),
	}
	manager := newTestManager(t, connector)
	key := Key{Account: "acct", Venue: VenueHyperliquid, Product: ProductPerpetual}
	subscription, err := manager.Subscribe(
		context.Background(), key,
		Credentials{Secret: "agent-key", SigningAddress: "0xuser"},
	)
	if err != nil {
		t.Fatal(err)
	}
	waitForConnection(t, connector.called, 1)
	waitForStatus(t, subscription.Updates(), StatusConnected)
	watch, err := subscription.WatchOrder(context.Background(), "0xclient")
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	manager.BindClientVenueIDs(key, "0xclient", "49")
	connection.reads <- fakeRead{payload: []byte(`{"channel":"userFills","data":{"fills":[{
		"oid":49,"tid":7,"sz":"0.5","px":"100","time":1700000000123
	}]}}`)}
	select {
	case update := <-watch.Updates():
		if update.Type != UpdateTrade || update.ClientOrderID != "0xclient" ||
			update.TradeID != "7" {
			t.Fatalf("fill update=%+v", update)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ACK-mapped fill")
	}
}

func TestManagerWarnsHyperliquidFillWithoutClientOrder(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	connection := newFakeConnection()
	connector := &fakeConnector{
		connections: []*fakeConnection{connection}, called: make(chan int, 1),
	}
	manager := newTestManagerWithLogger(t, connector, logger)
	subscription, err := manager.Subscribe(
		context.Background(),
		Key{Account: "acct", Venue: VenueHyperliquid, Product: ProductPerpetual},
		Credentials{Secret: "agent-key", SigningAddress: "0xuser"},
	)
	if err != nil {
		t.Fatal(err)
	}
	waitForConnection(t, connector.called, 1)
	waitForStatus(t, subscription.Updates(), StatusConnected)
	watch, err := subscription.WatchOrder(context.Background(), "0xclient")
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	connection.reads <- fakeRead{payload: []byte(`{"channel":"userFills","data":{"fills":[{
		"oid":49,"tid":7,"sz":"0.5","px":"100","time":1700000000123
	}]}}`)}
	deadline := time.Now().Add(time.Second)
	for {
		if strings.Contains(logs.String(), "hyperliquid_fill_order_not_found") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing orphan log: %s", logs.String())
		}
		select {
		case update := <-watch.Updates():
			t.Fatalf("orphan fill published: %+v", update)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestManagerWarnsHyperliquidVenueOrderMismatch(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	connection := newFakeConnection()
	connector := &fakeConnector{
		connections: []*fakeConnection{connection}, called: make(chan int, 1),
	}
	manager := newTestManagerWithLogger(t, connector, logger)
	subscription, err := manager.Subscribe(
		context.Background(),
		Key{Account: "acct", Venue: VenueHyperliquid, Product: ProductPerpetual},
		Credentials{Secret: "agent-key", SigningAddress: "0xuser"},
	)
	if err != nil {
		t.Fatal(err)
	}
	waitForConnection(t, connector.called, 1)
	waitForStatus(t, subscription.Updates(), StatusConnected)
	watchA, err := subscription.WatchOrder(context.Background(), "0xA")
	if err != nil {
		t.Fatal(err)
	}
	defer watchA.Close()
	connection.reads <- fakeRead{payload: []byte(`{"channel":"orderUpdates","data":[{
		"order":{"oid":1,"cloid":"0xA","origSz":"2","sz":"2"},
		"status":"open"
	}]}`)}
	waitForStatus(t, watchA.Updates(), StatusNew)
	watchB, err := subscription.WatchOrder(context.Background(), "0xB")
	if err != nil {
		t.Fatal(err)
	}
	defer watchB.Close()
	connection.reads <- fakeRead{payload: []byte(`{"channel":"orderUpdates","data":[{
		"order":{"oid":2,"cloid":"0xB","origSz":"2","sz":"2"},
		"status":"open"
	}]}`)}
	waitForStatus(t, watchB.Updates(), StatusNew)
	connection.reads <- fakeRead{payload: []byte(`{"channel":"userFills","data":{"fills":[{
		"oid":2,"tid":9,"sz":"0.5","px":"100","cloid":"0xA","time":1700000000123
	}]}}`)}
	deadline := time.Now().Add(time.Second)
	for {
		if strings.Contains(logs.String(), "hyperliquid_fill_venue_order_mismatch") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing mismatch log: %s", logs.String())
		}
		select {
		case update := <-watchA.Updates():
			t.Fatalf("mismatched fill published: %+v", update)
		case <-time.After(10 * time.Millisecond):
		}
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

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "i/o timeout" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

func TestClassifyDisconnectReasons(t *testing.T) {
	t.Parallel()
	if got := classifyDisconnect(timeoutNetError{}); got != reasonReadTimeout {
		t.Fatalf("timeout reason=%s", got)
	}
	if got := classifyDisconnect(&websocket.CloseError{
		Code: websocket.CloseAbnormalClosure, Text: "bye",
	}); got != reasonRemoteClose {
		t.Fatalf("close reason=%s", got)
	}
	if got := classifyDisconnect(newDisconnectError(
		reasonHeartbeatWriteFailed, errors.New("write"),
	)); got != reasonHeartbeatWriteFailed {
		t.Fatalf("heartbeat reason=%s", got)
	}
	if got := classifyConnectError(fmt.Errorf("authenticate gate websocket: denied")); got != reasonAuthenticationFailed {
		t.Fatalf("auth reason=%s", got)
	}
}

func TestManagerLogsDisconnectReasons(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		reason string
		warn   bool
		secret string
	}{
		{
			name:   "read timeout",
			err:    timeoutNetError{},
			reason: string(reasonReadTimeout),
			warn:   true,
		},
		{
			name: "remote close",
			err: &websocket.CloseError{
				Code: websocket.CloseGoingAway, Text: "closed",
			},
			reason: string(reasonRemoteClose),
			warn:   true,
		},
		{
			name:   "heartbeat write",
			err:    newDisconnectError(reasonHeartbeatWriteFailed, errors.New("write tcp")),
			reason: string(reasonHeartbeatWriteFailed),
			warn:   true,
		},
		{
			name:   "planned rotation",
			err:    newDisconnectError(reasonPlannedRotation, errors.New("rotated")),
			reason: string(reasonPlannedRotation),
			warn:   false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logs := &logBuffer{}
			logger := slog.New(slog.NewJSONHandler(logs, nil))
			first := newFakeConnection()
			second := newFakeConnection()
			connector := &fakeConnector{
				connections: []*fakeConnection{first, second},
				called:      make(chan int, 4),
			}
			manager := newTestManagerWithLogger(t, connector, logger)
			subscription, err := manager.Subscribe(
				context.Background(),
				Key{Account: "acct", Venue: VenueBinance, Product: ProductPerpetual},
				Credentials{APIKey: "leak-api-key-value", Secret: "leak-secret-value"},
			)
			if err != nil {
				t.Fatal(err)
			}
			waitForConnection(t, connector.called, 1)
			waitForStatus(t, subscription.Updates(), StatusConnected)
			firstGeneration := subscription.Generation()
			first.reads <- fakeRead{err: test.err}
			waitForStatus(t, subscription.Updates(), StatusDisconnected)
			waitForConnection(t, connector.called, 2)
			waitForStatus(t, subscription.Updates(), StatusConnected)
			if subscription.Generation() <= firstGeneration {
				t.Fatal("reconnect did not advance stream generation")
			}
			_ = manager.Close()
			dump := logs.String()
			if strings.Contains(dump, "leak-api-key-value") ||
				strings.Contains(dump, "leak-secret-value") ||
				strings.Contains(dump, "listenKey") {
				t.Fatalf("logs leaked credentials: %s", dump)
			}
			disconnected := lastLogEvent(t, dump, "order_stream_disconnected")
			if disconnected["reason"] != test.reason {
				t.Fatalf("disconnected=%v", disconnected)
			}
			if disconnected["venue"] != VenueBinance || disconnected["product"] != ProductPerpetual {
				t.Fatalf("disconnected=%v", disconnected)
			}
			if test.warn && disconnected["level"] != slog.LevelWarn.String() {
				t.Fatalf("level=%v", disconnected["level"])
			}
			if !test.warn && disconnected["level"] != slog.LevelInfo.String() {
				t.Fatalf("planned rotation logged as %v", disconnected["level"])
			}
			reconnected := lastLogEvent(t, dump, "order_stream_reconnected")
			if generation, _ := reconnected["generation"].(float64); int(generation) <= int(firstGeneration) {
				t.Fatalf("reconnected generation=%v first=%d", reconnected["generation"], firstGeneration)
			}
			stats := manager.Stats()
			if stats.Connects != 2 || stats.Reconnects != 1 || stats.Disconnects != 1 {
				t.Fatalf("stats=%+v", stats)
			}
		})
	}
}

func lastLogEvent(t *testing.T, logs, msg string) map[string]any {
	t.Helper()
	var last map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if line == "" {
			continue
		}
		event := map[string]any{}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("unmarshal log %q: %v", line, err)
		}
		if event["msg"] == msg {
			last = event
		}
	}
	if last == nil {
		t.Fatalf("missing log %s in %s", msg, logs)
	}
	return last
}
