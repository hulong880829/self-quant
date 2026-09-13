// Package stream provides the WebSocket client surface for the
// Hyperliquid SDK. A Client manages a single ws:// connection with
// automatic reconnection, channel subscriptions, and POST request /
// response correlation.
//
// The top-level facade github.com/Simon-Busch/hyperliquid-go exposes a
// constructed Client on Client.Stream.
package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Simon-Busch/hyperliquid-go/types"
)

const (
	// pingInterval matches Hyperliquid's expected interval (upstream uses 50s)
	pingInterval = 50 * time.Second

	// readDeadline is the maximum time to wait for a read before timing out.
	// Set slightly longer than pingInterval to allow for network latency.
	readDeadline = pingInterval + 10*time.Second

	// reconnectBaseWait is the initial wait time before reconnecting
	reconnectBaseWait = time.Second

	// maxReconnectWait caps the exponential backoff
	maxReconnectWait = time.Minute

	// connectTimeout for dial operations
	connectTimeout = 10 * time.Second
)

// Client manages a WebSocket connection to the Hyperliquid API.
// It handles automatic reconnection, subscription management, and POST requests.
type Client struct {
	url string

	// Connection state (atomic for lock-free reads)
	connected atomic.Bool

	// Connection and synchronization
	conn    *websocket.Conn
	connMu  sync.RWMutex // Protects conn
	writeMu sync.Mutex   // Serializes writes
	cancel  func()       // Cancels current connection's goroutines
	wg      sync.WaitGroup

	// Subscription management
	subscriptions map[subKey]map[int]*subscriptionCallback
	subMu         sync.RWMutex
	nextSubID     atomic.Int32

	// Reconnection settings
	reconnectMu          sync.Mutex
	reconnectTimer       *time.Timer
	reconnectAttempts    int
	maxReconnectAttempts int           // 0 = unlimited (default)
	reconnectWait        time.Duration // initial backoff, reset on each successful Connect

	// POST request tracking
	nextPostID      atomic.Int32
	pendingRequests map[int]*pendingRequest
	pendingMu       sync.RWMutex

	// Lifecycle
	closed    atomic.Bool
	closedMu  sync.Mutex
	closeOnce sync.Once

	// Pluggable logger for non-fatal warnings; defaults to no-op.
	logger Logger
}

// SetLogger plugs in a Logger to receive warnings and reconnection
// diagnostics. A nil logger reverts to the no-op default.
func (c *Client) SetLogger(l Logger) {
	if l == nil {
		c.logger = nopLogger{}
		return
	}
	c.logger = l
}

// SetMaxReconnectAttempts caps how many times the Client will retry the
// websocket connection before giving up. A value of 0 means retry
// forever. Intended to be called once at construction time by the facade.
func (c *Client) SetMaxReconnectAttempts(n int) {
	c.maxReconnectAttempts = n
}

// SetReconnectWait overrides the initial backoff used by the reconnect
// loop. A zero value is ignored. Intended to be called once at
// construction time by the facade.
func (c *Client) SetReconnectWait(d time.Duration) {
	if d > 0 {
		c.reconnectWait = d
	}
}

// warnf dispatches to the configured Logger's Warnf, falling back to the
// no-op logger when none is set.
func (c *Client) warnf(format string, args ...any) {
	if c.logger == nil {
		return
	}
	c.logger.Warnf(format, args...)
}

// New creates a new WebSocket Client targeting baseURL. The Client is
// not connected until Connect is called. An error is returned if baseURL
// cannot be parsed as a URL.
func New(baseURL string) (*Client, error) {
	if baseURL == "" {
		baseURL = types.MainnetAPIURL
	}

	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("hyperliquid: invalid stream URL %q: %w", baseURL, err)
	}
	switch parsedURL.Scheme {
	case "http", "ws":
		parsedURL.Scheme = "ws"
	default:
		parsedURL.Scheme = "wss"
	}
	parsedURL.Path = "/ws"
	wsURL := parsedURL.String()

	return &Client{
		url:             wsURL,
		subscriptions:   make(map[subKey]map[int]*subscriptionCallback),
		pendingRequests: make(map[int]*pendingRequest),
		reconnectWait:   reconnectBaseWait,
		logger:          nopLogger{},
	}, nil
}

// Connect establishes the WebSocket connection.
// It's safe to call multiple times - subsequent calls return immediately if already connected.
func (c *Client) Connect(ctx context.Context) error {
	if c.closed.Load() {
		return ErrNotConnected
	}

	if c.connected.Load() {
		return nil
	}

	c.connMu.Lock()
	defer c.connMu.Unlock()

	// Double-check after acquiring lock
	if c.conn != nil && c.connected.Load() {
		return nil
	}

	// Clean up any existing connection
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}

	// Cancel previous goroutines if any
	if c.cancel != nil {
		c.cancel()
		c.wg.Wait()
	}

	// Dial new connection
	dialer := websocket.Dialer{
		ReadBufferSize:  16384,
		WriteBufferSize: 4096,
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, connectTimeout)
	defer dialCancel()

	//nolint:bodyclose // WebSocket connections don't have response bodies to close
	conn, _, err := dialer.DialContext(dialCtx, c.url, nil)
	if err != nil {
		return fmt.Errorf("%w: websocket dial: %v", ErrNotConnected, err)
	}

	c.conn = conn
	c.connected.Store(true)

	// Reset reconnection state on successful connection.
	// reconnectWait is reset only if it was never customized via
	// SetReconnectWait; in that case it remains at its starting value.
	c.reconnectMu.Lock()
	c.reconnectAttempts = 0
	c.reconnectMu.Unlock()

	// Create context for this connection's goroutines
	connCtx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel

	// Start read and ping pumps (per-connection, like upstream)
	c.wg.Add(2)
	go c.readPump(connCtx)
	go c.pingPump(connCtx)

	// Resubscribe to all previous subscriptions
	if err := c.resubscribeAll(); err != nil {
		c.connected.Store(false)
		cancel()
		_ = conn.Close()
		c.conn = nil
		return fmt.Errorf("resubscribe failed: %w", err)
	}

	return nil
}

// Subscription is the handle returned by Client.Subscribe. Call Close to
// deregister the callback and emit an unsubscribe frame when no listener
// remains.
type Subscription struct {
	filter SubscriptionFilter
	id     int
	stream *Client
	closed atomic.Bool
}

// Close deregisters the callback registered by Client.Subscribe. It is
// safe to call multiple times: subsequent calls after the first return
// nil without sending another unsubscribe frame.
func (s *Subscription) Close() error {
	if s == nil {
		return nil
	}
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	return s.stream.unsubscribe(s)
}

// Subscribe registers a callback for the given subscription filter and
// returns a Subscription handle. Call sub.Close() to deregister.
func (c *Client) Subscribe(filter SubscriptionFilter, callback func(WSMessage)) (*Subscription, error) {
	if callback == nil {
		return nil, fmt.Errorf("callback cannot be nil")
	}

	c.subMu.Lock()
	key := filter.key()
	id := int(c.nextSubID.Add(1))

	if c.subscriptions[key] == nil {
		c.subscriptions[key] = make(map[int]*subscriptionCallback)
	}

	c.subscriptions[key][id] = &subscriptionCallback{
		id:       id,
		callback: callback,
	}
	c.subMu.Unlock()

	// Send subscribe message (outside lock to avoid deadlock)
	if err := c.sendSubscribe(filter); err != nil {
		c.subMu.Lock()
		delete(c.subscriptions[key], id)
		c.subMu.Unlock()
		return nil, fmt.Errorf("subscribe: %w", err)
	}

	return &Subscription{filter: filter, id: id, stream: c}, nil
}

// unsubscribe drops the callback registration recorded by Subscribe and
// emits an unsubscribe frame once no listener remains for the filter.
func (c *Client) unsubscribe(sub *Subscription) error {
	c.subMu.Lock()
	key := sub.filter.key()
	subs, ok := c.subscriptions[key]
	if !ok {
		c.subMu.Unlock()
		return fmt.Errorf("subscription not found")
	}

	if _, ok := subs[sub.id]; !ok {
		c.subMu.Unlock()
		return fmt.Errorf("subscription ID not found")
	}

	delete(subs, sub.id)

	shouldUnsubscribe := len(subs) == 0
	if shouldUnsubscribe {
		delete(c.subscriptions, key)
	}
	c.subMu.Unlock()

	if shouldUnsubscribe {
		if err := c.sendUnsubscribe(sub.filter); err != nil {
			return fmt.Errorf("unsubscribe: %w", err)
		}
	}

	return nil
}

// Close shuts down the WebSocket client and releases all resources.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		c.connected.Store(false)

		// Cancel reconnection timer
		c.reconnectMu.Lock()
		if c.reconnectTimer != nil {
			c.reconnectTimer.Stop()
			c.reconnectTimer = nil
		}
		c.reconnectMu.Unlock()

		c.failAllPending(ErrConnectionLost)

		// Cancel goroutines and close connection
		c.connMu.Lock()
		if c.cancel != nil {
			c.cancel()
		}
		if c.conn != nil {
			err = c.conn.Close()
			c.conn = nil
		}
		c.connMu.Unlock()

		// Wait for goroutines to finish
		c.wg.Wait()
	})
	return err
}

// readPump reads messages from the WebSocket connection.
// Runs for the lifetime of a single connection (context-aware, like upstream).
func (c *Client) readPump(ctx context.Context) {
	defer c.wg.Done()
	defer c.handleDisconnect()

	// Grab conn once — if it changes, context will be cancelled and we exit.
	c.connMu.RLock()
	conn := c.conn
	c.connMu.RUnlock()
	if conn == nil {
		return
	}

	for {
		// Set read deadline to detect dead connections.
		_ = conn.SetReadDeadline(time.Now().Add(readDeadline))

		_, msg, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() == nil && !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				c.warnf("websocket read error: %v", err)
			}
			return // Exit pump, handleDisconnect will trigger reconnection
		}

		// Skip server hello
		if string(msg) == "Websocket connection established." {
			continue
		}

		// Parse and dispatch
		var wsMsg WSMessage
		if err := json.Unmarshal(msg, &wsMsg); err != nil {
			c.warnf("websocket message parse error: %v", err)
			continue
		}

		c.dispatch(wsMsg)
	}
}

// pingPump sends periodic ping messages to keep the connection alive.
// Runs for the lifetime of a single connection (context-aware, like upstream).
func (c *Client) pingPump(ctx context.Context) {
	defer c.wg.Done()

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.sendPing(); err != nil {
				c.warnf("ping error: %v", err)
				// Don't return here - readPump will detect the error
				// and handleDisconnect will trigger reconnection
			}
		}
	}
}

// dispatch routes messages to appropriate callbacks.
func (c *Client) dispatch(msg WSMessage) {
	// Handle pong responses (sent by Hyperliquid as JSON, not WebSocket protocol pongs)
	if msg.Channel == "pong" {
		// Pong received - connection is alive, nothing to do
		return
	}

	// Handle subscription confirmations
	if msg.Channel == "subscriptionResponse" {
		// Subscription confirmed by server, nothing to do
		return
	}

	// Handle POST responses
	if msg.Channel == "post" {
		var postResp WsPostResponseData
		if err := json.Unmarshal(msg.Data, &postResp); err != nil {
			c.warnf("failed to unmarshal post response: %v", err)
			return
		}

		c.pendingMu.RLock()
		pending, ok := c.pendingRequests[postResp.ID]
		c.pendingMu.RUnlock()

		if ok {
			select {
			case pending.responseChan <- postResp:
			default:
				// Channel closed (timeout) or full
			}
		}
		return
	}

	// Copy matching callbacks under lock
	c.subMu.RLock()
	var callbacks []func(WSMessage)
	for key, subs := range c.subscriptions {
		if matchSubscription(key, msg) {
			for _, sub := range subs {
				callbacks = append(callbacks, sub.callback)
			}
		}
	}
	c.subMu.RUnlock()

	// Execute callbacks without holding lock
	for _, cb := range callbacks {
		cb(msg)
	}
}

// resubscribeAll resends subscribe messages for all active subscriptions.
func (c *Client) resubscribeAll() error {
	c.subMu.RLock()
	keys := make([]subKey, 0, len(c.subscriptions))
	for key, subs := range c.subscriptions {
		if len(subs) > 0 {
			keys = append(keys, key)
		}
	}
	c.subMu.RUnlock()

	for _, key := range keys {
		f := SubscriptionFilter{
			Type:     key.typ,
			Coin:     key.coin,
			User:     key.user,
			Interval: key.interval,
			Dex:      key.dex,
		}
		if err := c.sendSubscribe(f); err != nil {
			return fmt.Errorf("resubscribe %s: %w", key.typ, err)
		}
	}
	return nil
}

func (c *Client) sendSubscribe(f SubscriptionFilter) error {
	return c.writeJSON(WsCommand{
		Method:       "subscribe",
		Subscription: &f,
	})
}

func (c *Client) sendUnsubscribe(f SubscriptionFilter) error {
	return c.writeJSON(WsCommand{
		Method:       "unsubscribe",
		Subscription: &f,
	})
}

func (c *Client) sendPing() error {
	return c.writeJSON(WsCommand{Method: "ping"})
}

func (c *Client) writeJSON(v any) error {
	// Marshal outside the lock so serialization doesn't block other writers
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("json marshal: %w", err)
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if !c.connected.Load() {
		return fmt.Errorf("not connected")
	}

	c.connMu.RLock()
	conn := c.conn
	c.connMu.RUnlock()

	if conn == nil {
		return fmt.Errorf("connection closed")
	}

	// WriteMessage is a single frame write — no NextWriter/Close dance
	return conn.WriteMessage(websocket.TextMessage, data)
}

// matchSubscription checks if a message matches a subscription key.
func matchSubscription(key subKey, msg WSMessage) bool {
	// Channel matching
	// NOTE: Most subscription types have matching channel names, but there are exceptions:
	// - "userEvents" subscription sends messages on "user" channel (Hyperliquid API quirk)
	channelMatch := false
	switch key.typ {
	case "allMids":
		channelMatch = msg.Channel == "allMids"
	case "notification":
		channelMatch = msg.Channel == "notification"
	case "webData2":
		channelMatch = msg.Channel == "webData2"
	case "candle":
		channelMatch = msg.Channel == "candle"
	case "l2Book":
		channelMatch = msg.Channel == "l2Book"
	case "trades":
		channelMatch = msg.Channel == "trades"
	case "orderUpdates":
		channelMatch = msg.Channel == "orderUpdates"
	case "userEvents":
		// API quirk: "userEvents" subscription type receives messages on "user" channel
		channelMatch = msg.Channel == "user"
	case "userFills":
		channelMatch = msg.Channel == "userFills"
	case "userFundings":
		channelMatch = msg.Channel == "userFundings"
	case "userNonFundingLedgerUpdates":
		channelMatch = msg.Channel == "userNonFundingLedgerUpdates"
	case "activeAssetCtx":
		channelMatch = msg.Channel == "activeAssetCtx"
	case "activeAssetData":
		channelMatch = msg.Channel == "activeAssetData"
	case "userTwapSliceFills":
		channelMatch = msg.Channel == "userTwapSliceFills"
	case "userTwapHistory":
		channelMatch = msg.Channel == "userTwapHistory"
	case "bbo":
		channelMatch = msg.Channel == "bbo"
	default:
		return false
	}

	if !channelMatch {
		return false
	}

	// Early return if no additional filtering needed
	if key.coin == "" && key.user == "" {
		return true
	}

	// orderUpdates data is a JSON array, not an object — matching is purely by channel.
	// User is implicit from the subscription, not present in message data.
	if key.typ == "orderUpdates" {
		return true
	}

	// Single unmarshal for both coin and user matching
	var msgData struct {
		Coin string `json:"coin"`
		User string `json:"user"`
	}
	if err := json.Unmarshal(msg.Data, &msgData); err != nil {
		return false
	}

	// Coin matching
	if key.coin != "" && msgData.Coin != key.coin {
		return false
	}

	// User matching
	if key.user != "" {
		if !strings.EqualFold(msgData.User, key.user) {
			return false
		}
	}

	return true
}
