package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type WebSocketConnector struct {
	mu                    sync.Mutex
	pools                 map[websocketPoolKey]*websocketPool
	dial                  func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error)
	endpoint              func(Key) (string, error)
	heartbeat             time.Duration
	readWait              time.Duration
	writeWait             time.Duration
	ackWait               time.Duration
	now                   func() time.Time
	requestID             atomic.Uint64
	httpClient            *http.Client
	lighterRESTURL        string
	lighterMarkets        map[lighterMarketKey]uint64
	lighterMessages       *rollingMessageLimiter
	asterConnectionMaxAge time.Duration
}

type ConnectorOptions struct {
	Dialer                *websocket.Dialer
	Heartbeat             time.Duration
	ReadWait              time.Duration
	WriteWait             time.Duration
	AckWait               time.Duration
	Now                   func() time.Time
	HTTPClient            *http.Client
	LighterRESTURL        string
	LighterMessageLimit   int
	LighterMessageWindow  time.Duration
	AsterConnectionMaxAge time.Duration
}

func NewWebSocketConnector(optionList ...ConnectorOptions) *WebSocketConnector {
	var options ConnectorOptions
	if len(optionList) > 0 {
		options = optionList[0]
	}
	if options.Dialer == nil {
		options.Dialer = &websocket.Dialer{
			HandshakeTimeout: 10 * time.Second,
			ReadBufferSize:   32 * 1024,
			WriteBufferSize:  8 * 1024,
		}
	}
	if options.Heartbeat <= 0 {
		options.Heartbeat = 20 * time.Second
	}
	if options.ReadWait <= 0 {
		options.ReadWait = 45 * time.Second
	}
	if options.WriteWait <= 0 {
		options.WriteWait = 5 * time.Second
	}
	if options.AckWait <= 0 {
		options.AckWait = 5 * time.Second
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if strings.TrimSpace(options.LighterRESTURL) == "" {
		options.LighterRESTURL = "https://mainnet.zklighter.elliot.ai"
	}
	if options.LighterMessageLimit <= 0 {
		options.LighterMessageLimit = 150
	}
	if options.LighterMessageWindow <= 0 {
		options.LighterMessageWindow = time.Minute
	}
	if options.AsterConnectionMaxAge <= 0 {
		options.AsterConnectionMaxAge = 23*time.Hour + 55*time.Minute
	}
	lighterMessages := &rollingMessageLimiter{
		limit: options.LighterMessageLimit, window: options.LighterMessageWindow,
		now: options.Now,
	}
	return &WebSocketConnector{
		pools: make(map[websocketPoolKey]*websocketPool), dial: options.Dialer.DialContext,
		endpoint: websocketEndpoint, heartbeat: options.Heartbeat,
		readWait: options.ReadWait, writeWait: options.WriteWait,
		ackWait: options.AckWait, now: options.Now,
		httpClient:            options.HTTPClient,
		lighterRESTURL:        strings.TrimRight(options.LighterRESTURL, "/"),
		lighterMarkets:        make(map[lighterMarketKey]uint64),
		lighterMessages:       lighterMessages,
		asterConnectionMaxAge: options.AsterConnectionMaxAge,
	}
}

func (c *WebSocketConnector) Connect(ctx context.Context, key Key) (Connection, error) {
	normalized, err := NewKey(key.Venue, key.Product, key.Symbol)
	if err != nil {
		return nil, err
	}
	if normalized.Venue == VenueLighter {
		if _, err := c.resolveLighterMarketID(ctx, normalized); err != nil {
			return nil, err
		}
	}

	group := websocketPoolKey{venue: normalized.Venue, product: normalized.Product}
	c.mu.Lock()
	if c.pools == nil {
		c.pools = make(map[websocketPoolKey]*websocketPool)
	}
	pool := c.pools[group]
	owner := pool == nil
	if owner {
		pool = &websocketPool{
			connector:      c,
			key:            group,
			ready:          make(chan struct{}),
			done:           make(chan struct{}),
			logical:        make(map[string]map[*logicalConnection]struct{}),
			pending:        make(map[string]pendingSubscription),
			lighterSymbols: make(map[uint64]string),
			clientMessages: &rollingMessageLimiter{
				limit: 10, window: time.Second, now: c.now,
			},
		}
		c.pools[group] = pool
	}
	c.mu.Unlock()

	if owner {
		pool.open(ctx, normalized)
	} else {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-pool.ready:
		}
	}
	if pool.openErr != nil {
		return nil, pool.openErr
	}

	messageCapacity := 1
	if normalized.Venue == VenueLighter {
		messageCapacity = 256
	}
	logical := &logicalConnection{
		pool:     pool,
		key:      normalized,
		messages: make(chan websocketRead, messageCapacity),
	}
	if err := pool.add(logical); err != nil {
		return nil, err
	}
	return logical, nil
}

type websocketPoolKey struct {
	venue   string
	product string
}

type lighterMarketKey struct {
	product string
	symbol  string
}

type websocketPool struct {
	connector  *WebSocketConnector
	key        websocketPoolKey
	ready      chan struct{}
	openErr    error
	connection *websocket.Conn

	subscriptionMu sync.Mutex
	mu             sync.Mutex
	logical        map[string]map[*logicalConnection]struct{}
	pendingMu      sync.Mutex
	pending        map[string]pendingSubscription
	writeMu        sync.Mutex
	closeOnce      sync.Once
	done           chan struct{}
	lighterSymbols map[uint64]string
	clientMessages *rollingMessageLimiter
}

type pendingSubscription struct {
	key       Key
	operation string
	requestID uint64
}

type websocketRead struct {
	payload []byte
	err     error
}

type logicalConnection struct {
	pool      *websocketPool
	key       Key
	mu        sync.Mutex
	messages  chan websocketRead
	closed    bool
	closeOnce sync.Once
}

type rollingMessageLimiter struct {
	mu         sync.Mutex
	limit      int
	window     time.Duration
	now        func() time.Time
	timestamps []time.Time
}

func (l *rollingMessageLimiter) wait(done <-chan struct{}) error {
	if l == nil || l.limit <= 0 || l.window <= 0 {
		return nil
	}
	for {
		l.mu.Lock()
		now := l.now()
		cutoff := now.Add(-l.window)
		first := 0
		for first < len(l.timestamps) && !l.timestamps[first].After(cutoff) {
			first++
		}
		if first > 0 {
			l.timestamps = append(l.timestamps[:0], l.timestamps[first:]...)
		}
		if len(l.timestamps) < l.limit {
			l.timestamps = append(l.timestamps, now)
			l.mu.Unlock()
			return nil
		}
		wait := l.timestamps[0].Add(l.window).Sub(now)
		l.mu.Unlock()
		timer := time.NewTimer(max(time.Millisecond, wait))
		select {
		case <-done:
			timer.Stop()
			return io.ErrClosedPipe
		case <-timer.C:
		}
	}
}

func (c *WebSocketConnector) resolveLighterMarketID(
	ctx context.Context,
	key Key,
) (uint64, error) {
	cacheKey := lighterMarketKey{
		product: key.Product,
		symbol:  canonicalLighterSymbol(key.Symbol),
	}
	c.mu.Lock()
	marketID, ok := c.lighterMarkets[cacheKey]
	c.mu.Unlock()
	if ok {
		return marketID, nil
	}

	filter := "perp"
	fieldSpot := false
	if key.Product == ProductSpot {
		filter, fieldSpot = "spot", true
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		c.lighterRESTURL+"/api/v1/orderBookDetails?filter="+filter,
		nil,
	)
	if err != nil {
		return 0, fmt.Errorf("build Lighter market metadata request: %w", err)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return 0, fmt.Errorf("resolve Lighter market_id for %s: %w", key.Symbol, err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return 0, fmt.Errorf(
			"resolve Lighter market_id for %s: HTTP %d", key.Symbol, response.StatusCode,
		)
	}
	var payload struct {
		Code             json.Number `json:"code"`
		OrderBookDetails []struct {
			Symbol     string      `json:"symbol"`
			MarketID   json.Number `json:"market_id"`
			MarketType string      `json:"market_type"`
			Status     string      `json:"status"`
		} `json:"order_book_details"`
		SpotOrderBookDetails []struct {
			Symbol     string      `json:"symbol"`
			MarketID   json.Number `json:"market_id"`
			MarketType string      `json:"market_type"`
			Status     string      `json:"status"`
		} `json:"spot_order_book_details"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 32<<20))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return 0, fmt.Errorf("decode Lighter market metadata: %w", err)
	}
	if payload.Code.String() != "200" {
		return 0, fmt.Errorf("resolve Lighter market_id: response code %s", payload.Code)
	}
	type market struct {
		symbol     string
		marketID   json.Number
		marketType string
		status     string
	}
	markets := make([]market, 0, len(payload.OrderBookDetails)+len(payload.SpotOrderBookDetails))
	if fieldSpot {
		for _, item := range payload.SpotOrderBookDetails {
			markets = append(markets, market{
				symbol: item.Symbol, marketID: item.MarketID,
				marketType: item.MarketType, status: item.Status,
			})
		}
	} else {
		for _, item := range payload.OrderBookDetails {
			markets = append(markets, market{
				symbol: item.Symbol, marketID: item.MarketID,
				marketType: item.MarketType, status: item.Status,
			})
		}
	}
	resolved := make(map[lighterMarketKey]uint64, len(markets)*2)
	seenIDs := make(map[uint64]string, len(markets))
	for _, item := range markets {
		expectedType := "perp"
		if fieldSpot {
			expectedType = "spot"
		}
		if !strings.EqualFold(item.status, "active") ||
			(item.marketType != "" && !strings.EqualFold(item.marketType, expectedType)) {
			continue
		}
		id, parseErr := strconv.ParseUint(item.marketID.String(), 10, 64)
		if parseErr != nil {
			return 0, fmt.Errorf("parse Lighter market_id %q: %w", item.marketID, parseErr)
		}
		rawSymbol := canonicalLighterSymbol(item.symbol)
		if rawSymbol == "" {
			continue
		}
		if previous, duplicate := seenIDs[id]; duplicate && previous != rawSymbol {
			return 0, fmt.Errorf("duplicate Lighter market_id %d", id)
		}
		seenIDs[id] = rawSymbol
		resolved[lighterMarketKey{product: key.Product, symbol: rawSymbol}] = id
		if !fieldSpot {
			resolved[lighterMarketKey{
				product: key.Product, symbol: rawSymbol + "USDC",
			}] = id
		}
	}
	c.mu.Lock()
	for item, id := range resolved {
		c.lighterMarkets[item] = id
	}
	marketID, ok = c.lighterMarkets[cacheKey]
	c.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf(
			"%w: no active Lighter %s market for symbol %q",
			ErrSubscriptionRejected, key.Product, key.Symbol,
		)
	}
	return marketID, nil
}

func canonicalLighterSymbol(symbol string) string {
	var builder strings.Builder
	for _, character := range strings.ToUpper(strings.TrimSpace(symbol)) {
		if (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') {
			builder.WriteRune(character)
		}
	}
	return builder.String()
}

func (c *WebSocketConnector) lighterMarketID(key Key) (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.lighterMarkets[lighterMarketKey{
		product: key.Product, symbol: canonicalLighterSymbol(key.Symbol),
	}]
	return id, ok
}

func (p *websocketPool) open(ctx context.Context, key Key) {
	endpoint := p.connector.endpoint
	dial := p.connector.dial
	if endpoint == nil || dial == nil {
		defaultConnector := NewWebSocketConnector()
		if endpoint == nil {
			endpoint = defaultConnector.endpoint
		}
		if dial == nil {
			dial = defaultConnector.dial
		}
	}

	url, err := endpoint(key)
	if err == nil {
		var response *http.Response
		p.connection, response, err = dial(ctx, url, http.Header{})
		if err != nil && response != nil {
			err = fmt.Errorf("dial %s websocket (HTTP %d): %w", key.Venue, response.StatusCode, err)
		} else if err != nil {
			err = fmt.Errorf("dial %s websocket: %w", key.Venue, err)
		}
	}
	if err != nil {
		p.openErr = err
		p.connector.remove(p)
		close(p.ready)
		return
	}
	readLimit := int64(256 * 1024)
	if key.Venue == VenueLighter {
		readLimit = 32 << 20
	}
	p.connection.SetReadLimit(readLimit)
	if err := p.refreshReadDeadline(); err != nil {
		p.openErr = fmt.Errorf("set %s websocket read deadline: %w", key.Venue, err)
		p.connector.remove(p)
		_ = p.connection.Close()
		close(p.ready)
		return
	}
	p.connection.SetPongHandler(func(string) error {
		return p.refreshReadDeadline()
	})
	p.connection.SetPingHandler(func(message string) error {
		if err := p.refreshReadDeadline(); err != nil {
			return err
		}
		p.writeMu.Lock()
		defer p.writeMu.Unlock()
		return p.connection.WriteControl(
			websocket.PongMessage,
			[]byte(message),
			p.connector.now().Add(p.connector.writeWait),
		)
	})
	close(p.ready)
	go p.readLoop()
	go p.heartbeat()
	if key.Venue == VenueAster {
		go p.rotateAsterConnection()
	}
}

func (p *websocketPool) rotateAsterConnection() {
	timer := time.NewTimer(p.connector.asterConnectionMaxAge)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		p.shutdown(ErrConnectionRotation)
	}
}

func (p *websocketPool) add(connection *logicalConnection) error {
	p.subscriptionMu.Lock()
	defer p.subscriptionMu.Unlock()

	symbol := canonicalSymbol(connection.key.Symbol)
	var lighterMarketID uint64
	if p.key.venue == VenueLighter {
		var ok bool
		lighterMarketID, ok = p.connector.lighterMarketID(connection.key)
		if !ok {
			return fmt.Errorf("Lighter market_id cache missing for %s", connection.key.Symbol)
		}
	}
	p.mu.Lock()
	select {
	case <-p.done:
		p.mu.Unlock()
		return io.ErrClosedPipe
	default:
	}
	connections := p.logical[symbol]
	first := len(connections) == 0
	if first && p.key.venue == VenueAster && len(p.logical) >= 200 {
		p.mu.Unlock()
		return fmt.Errorf(
			"%w: Aster connection stream limit reached", ErrSubscriptionRejected,
		)
	}
	if first && p.key.venue == VenueLighter && len(p.logical) >= 500 {
		p.mu.Unlock()
		return fmt.Errorf(
			"%w: Lighter connection subscription limit reached", ErrSubscriptionRejected,
		)
	}
	if first && p.key.venue == VenueLighter {
		if existing, exists := p.lighterSymbols[lighterMarketID]; exists && existing != symbol {
			p.mu.Unlock()
			return fmt.Errorf("Lighter market_id %d maps to multiple symbols", lighterMarketID)
		}
		p.lighterSymbols[lighterMarketID] = symbol
	}
	if connections == nil {
		connections = make(map[*logicalConnection]struct{})
		p.logical[symbol] = connections
	}
	connections[connection] = struct{}{}
	p.mu.Unlock()

	if first {
		if err := p.writeSubscription(connection.key, true); err != nil {
			wrapped := fmt.Errorf("subscribe %s BBO: %w", connection.key.Venue, err)
			p.shutdown(wrapped)
			return wrapped
		}
	}
	return nil
}

func (p *websocketPool) remove(connection *logicalConnection) error {
	p.subscriptionMu.Lock()
	defer p.subscriptionMu.Unlock()

	symbol := canonicalSymbol(connection.key.Symbol)
	var lighterMarketID uint64
	if p.key.venue == VenueLighter {
		lighterMarketID, _ = p.connector.lighterMarketID(connection.key)
	}
	p.mu.Lock()
	connections := p.logical[symbol]
	if _, ok := connections[connection]; !ok {
		p.mu.Unlock()
		return nil
	}
	delete(connections, connection)
	lastSymbol := len(connections) == 0
	if lastSymbol {
		delete(p.logical, symbol)
		if p.key.venue == VenueLighter {
			delete(p.lighterSymbols, lighterMarketID)
		}
	}
	lastConnection := len(p.logical) == 0
	p.mu.Unlock()

	if lastConnection {
		p.shutdown(ErrClosed)
		return nil
	}
	if lastSymbol {
		p.cancelPendingForSymbol(symbol)
		if err := p.writeSubscription(connection.key, false); err != nil {
			wrapped := fmt.Errorf("unsubscribe %s BBO: %w", connection.key.Venue, err)
			p.shutdown(wrapped)
			return wrapped
		}
	}
	return nil
}

func (p *websocketPool) writeSubscription(key Key, subscribe bool) error {
	requestID := p.connector.requestID.Add(1)
	request, correlation, err := p.subscriptionRequestWithID(key, subscribe, requestID)
	if err != nil {
		return err
	}
	operation := "subscribe"
	if !subscribe {
		operation = "unsubscribe"
	}
	if err := p.waitClientMessage(); err != nil {
		return err
	}
	pending := pendingSubscription{
		key: key, operation: operation, requestID: requestID,
	}
	p.registerPending(correlation, pending)
	if err := p.writeJSONRaw(request); err != nil {
		p.cancelPending(correlation, requestID)
		return err
	}
	return nil
}

func (p *websocketPool) subscriptionRequestWithID(
	key Key,
	subscribe bool,
	requestID uint64,
) (any, string, error) {
	if key.Venue != VenueLighter {
		request, correlation := subscriptionRequestWithID(key, subscribe, requestID)
		if request == nil || correlation == "" {
			return nil, "", fmt.Errorf(
				"%w: subscription request venue %q", ErrUnsupportedKey, key.Venue,
			)
		}
		return request, correlation, nil
	}
	marketID, ok := p.connector.lighterMarketID(key)
	if !ok {
		return nil, "", fmt.Errorf("Lighter market_id cache missing for %s", key.Symbol)
	}
	operation := "subscribe"
	if !subscribe {
		operation = "unsubscribe"
	}
	id := strconv.FormatUint(marketID, 10)
	return map[string]string{
		"type": operation, "channel": "order_book/" + id,
	}, lighterCorrelation(operation, id), nil
}

func (p *websocketPool) registerPending(
	correlation string,
	pending pendingSubscription,
) {
	p.pendingMu.Lock()
	if p.pending == nil {
		p.pending = make(map[string]pendingSubscription)
	}
	p.pending[correlation] = pending
	p.pendingMu.Unlock()
	time.AfterFunc(p.connector.ackWait, func() {
		p.expirePending(correlation, pending.requestID)
	})
}

func (p *websocketPool) expirePending(correlation string, requestID uint64) {
	p.pendingMu.Lock()
	pending, ok := p.pending[correlation]
	if !ok || pending.requestID != requestID {
		p.pendingMu.Unlock()
		return
	}
	delete(p.pending, correlation)
	p.pendingMu.Unlock()
	if pending.operation != "subscribe" {
		return
	}
	p.failSymbol(pending.key.Symbol, fmt.Errorf(
		"%w: %s %s %s",
		ErrSubscriptionAckTimeout,
		pending.key.Venue,
		pending.key.Product,
		pending.key.Symbol,
	))
}

func (p *websocketPool) cancelPending(correlation string, requestID uint64) {
	p.pendingMu.Lock()
	if pending, ok := p.pending[correlation]; ok && pending.requestID == requestID {
		delete(p.pending, correlation)
	}
	p.pendingMu.Unlock()
}

func (p *websocketPool) cancelPendingForSymbol(symbol string) {
	canonical := canonicalSymbol(symbol)
	p.pendingMu.Lock()
	for correlation, pending := range p.pending {
		if canonicalSymbol(pending.key.Symbol) == canonical {
			delete(p.pending, correlation)
		}
	}
	p.pendingMu.Unlock()
}

func (p *websocketPool) readLoop() {
	for {
		if err := p.refreshReadDeadline(); err != nil {
			p.shutdown(fmt.Errorf("set %s websocket read deadline: %w", p.key.venue, err))
			return
		}
		messageType, payload, err := p.connection.ReadMessage()
		if err != nil {
			p.shutdown(fmt.Errorf("read %s websocket: %w", p.key.venue, err))
			return
		}
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			continue
		}
		text := strings.TrimSpace(string(payload))
		if strings.EqualFold(text, "pong") || strings.EqualFold(text, "ping") {
			continue
		}
		control, handled, err := parseSubscriptionControl(p.key.venue, payload)
		if err != nil {
			continue
		}
		if handled {
			p.handleSubscriptionControl(control)
			if !control.deliver {
				continue
			}
		}
		symbols, err := p.messageSymbols(payload)
		if err != nil {
			continue
		}
		for _, symbol := range symbols {
			if symbol != "" {
				p.deliver(symbol, payload)
			}
		}
	}
}

func (p *websocketPool) messageSymbols(payload []byte) ([]string, error) {
	if p.key.venue != VenueLighter {
		return messageSymbols(p.key.venue, payload)
	}
	var message struct {
		Channel string `json:"channel"`
	}
	if err := json.Unmarshal(payload, &message); err != nil {
		return nil, err
	}
	idText, ok := lighterChannelID(message.Channel)
	if !ok {
		return nil, nil
	}
	marketID, err := strconv.ParseUint(idText, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse Lighter channel market_id: %w", err)
	}
	p.mu.Lock()
	symbol := p.lighterSymbols[marketID]
	p.mu.Unlock()
	if symbol == "" {
		return nil, nil
	}
	return []string{symbol}, nil
}

func (p *websocketPool) handleSubscriptionControl(control subscriptionControl) {
	pending, ok := p.takePending(control)
	if !ok {
		if control.err != nil &&
			(p.key.venue == VenueHyperliquid || p.key.venue == VenueLighter) {
			p.shutdown(fmt.Errorf(
				"%w: %s: %v", ErrSubscriptionRejected, p.key.venue, control.err,
			))
		}
		return
	}
	if control.err == nil || pending.operation != "subscribe" {
		return
	}
	p.failSymbol(pending.key.Symbol, fmt.Errorf(
		"%w: %s %s %s: %v",
		ErrSubscriptionRejected,
		pending.key.Venue,
		pending.key.Product,
		pending.key.Symbol,
		control.err,
	))
}

func (p *websocketPool) takePending(
	control subscriptionControl,
) (pendingSubscription, bool) {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	if control.correlation != "" {
		pending, ok := p.pending[control.correlation]
		if ok {
			delete(p.pending, control.correlation)
		}
		return pending, ok
	}
	if control.symbol == "" {
		return pendingSubscription{}, false
	}
	for _, operation := range []string{"subscribe", "unsubscribe"} {
		correlation := symbolCorrelation(operation, control.symbol)
		if pending, ok := p.pending[correlation]; ok {
			delete(p.pending, correlation)
			return pending, true
		}
	}
	return pendingSubscription{}, false
}

func (p *websocketPool) refreshReadDeadline() error {
	return p.connection.SetReadDeadline(p.connector.now().Add(p.connector.readWait))
}

func (p *websocketPool) deliver(symbol string, payload []byte) {
	p.mu.Lock()
	connections := make([]*logicalConnection, 0, len(p.logical[canonicalSymbol(symbol)]))
	for connection := range p.logical[canonicalSymbol(symbol)] {
		connections = append(connections, connection)
	}
	p.mu.Unlock()
	for _, connection := range connections {
		if p.key.venue == VenueLighter {
			if !connection.deliverSequenced(payload) {
				p.failSymbol(symbol, fmt.Errorf(
					"%w: Lighter sequence buffer overflow", ErrBookUnavailable,
				))
				return
			}
			continue
		}
		connection.deliver(payload)
	}
}

func (p *websocketPool) failSymbol(symbol string, err error) {
	canonical := canonicalSymbol(symbol)
	p.cancelPendingForSymbol(canonical)
	p.mu.Lock()
	bySymbol := p.logical[canonical]
	delete(p.logical, canonical)
	connections := make([]*logicalConnection, 0, len(bySymbol))
	for connection := range bySymbol {
		connections = append(connections, connection)
	}
	lastSymbol := len(p.logical) == 0
	p.mu.Unlock()
	for _, connection := range connections {
		connection.finish(err)
	}
	if lastSymbol {
		p.shutdown(err)
	}
}

func (p *websocketPool) shutdown(err error) {
	if err == nil {
		err = io.EOF
	}
	p.closeOnce.Do(func() {
		close(p.done)
		p.connector.remove(p)
		_ = p.connection.Close()

		p.mu.Lock()
		var connections []*logicalConnection
		for _, bySymbol := range p.logical {
			for connection := range bySymbol {
				connections = append(connections, connection)
			}
		}
		p.logical = make(map[string]map[*logicalConnection]struct{})
		p.mu.Unlock()
		p.pendingMu.Lock()
		p.pending = make(map[string]pendingSubscription)
		p.pendingMu.Unlock()
		for _, connection := range connections {
			connection.finish(err)
		}
	})
}

func (p *websocketPool) writeJSON(value any) error {
	if err := p.waitClientMessage(); err != nil {
		return err
	}
	return p.writeJSONRaw(value)
}

func (p *websocketPool) waitClientMessage() error {
	switch p.key.venue {
	case VenueLighter:
		return p.connector.lighterMessages.wait(p.done)
	case VenueAster:
		return p.clientMessages.wait(p.done)
	default:
		return nil
	}
}

func (p *websocketPool) writeJSONRaw(value any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := p.connection.SetWriteDeadline(
		p.connector.now().Add(p.connector.writeWait),
	); err != nil {
		return err
	}
	return p.connection.WriteJSON(value)
}

func (p *websocketPool) heartbeat() {
	ticker := time.NewTicker(p.connector.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			var err error
			switch p.key.venue {
			case VenueOKX, VenueBitget:
				err = p.writeText("ping")
			case VenueBybit:
				err = p.writeJSON(map[string]string{"op": "ping"})
			case VenueHyperliquid:
				err = p.writeJSON(map[string]string{"method": "ping"})
			case VenueLighter:
				err = p.writeJSON(map[string]string{"type": "ping"})
			case VenueGate, VenueAster:
				if p.key.venue == VenueAster {
					err = p.waitClientMessage()
					if err != nil {
						break
					}
				}
				p.writeMu.Lock()
				err = p.connection.WriteControl(
					websocket.PingMessage, nil,
					p.connector.now().Add(p.connector.writeWait),
				)
				p.writeMu.Unlock()
			}
			if err != nil {
				p.shutdown(fmt.Errorf("heartbeat %s websocket: %w", p.key.venue, err))
				return
			}
		}
	}
}

func (p *websocketPool) writeText(value string) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := p.connection.SetWriteDeadline(
		p.connector.now().Add(p.connector.writeWait),
	); err != nil {
		return err
	}
	return p.connection.WriteMessage(websocket.TextMessage, []byte(value))
}

func (c *WebSocketConnector) remove(pool *websocketPool) {
	c.mu.Lock()
	if c.pools[pool.key] == pool {
		delete(c.pools, pool.key)
	}
	c.mu.Unlock()
}

func (c *logicalConnection) Read() ([]byte, error) {
	item, ok := <-c.messages
	if !ok {
		return nil, ErrClosed
	}
	return item.payload, item.err
}

func (c *logicalConnection) Close() error {
	err := c.pool.remove(c)
	c.finish(ErrClosed)
	return err
}

func (c *logicalConnection) deliver(payload []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.messages <- websocketRead{payload: payload}:
	default:
		select {
		case <-c.messages:
		default:
		}
		select {
		case c.messages <- websocketRead{payload: payload}:
		default:
		}
	}
}

func (c *logicalConnection) deliverSequenced(payload []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	select {
	case c.messages <- websocketRead{payload: payload}:
		return true
	default:
		return false
	}
}

func (c *logicalConnection) finish(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		for len(c.messages) > 0 {
			<-c.messages
		}
		c.messages <- websocketRead{err: err}
		close(c.messages)
		c.mu.Unlock()
	})
}

func websocketEndpoint(key Key) (string, error) {
	switch key.Venue {
	case VenueBinance:
		baseURL := "wss://stream.binance.com:9443/stream"
		if key.Product == ProductPerpetual {
			baseURL = "wss://fstream.binance.com/stream"
		}
		return baseURL, nil
	case VenueOKX:
		return "wss://ws.okx.com:8443/ws/v5/public", nil
	case VenueBybit:
		category := "spot"
		if key.Product == ProductPerpetual {
			category = "linear"
		}
		return "wss://stream.bybit.com/v5/public/" + category, nil
	case VenueBitget:
		return "wss://ws.bitget.com/v2/ws/public", nil
	case VenueGate:
		if key.Product == ProductPerpetual {
			return "wss://fx-ws.gateio.ws/v4/ws/usdt", nil
		}
		return "wss://api.gateio.ws/ws/v4/spot", nil
	case VenueHyperliquid:
		return "wss://api.hyperliquid.xyz/ws", nil
	case VenueAster:
		return "wss://fstream.asterdex.com/ws", nil
	case VenueLighter:
		return "wss://mainnet.zklighter.elliot.ai/stream", nil
	default:
		return "", fmt.Errorf("%w: venue %q", ErrUnsupportedKey, key.Venue)
	}
}

func subscriptionRequest(key Key, subscribe bool) any {
	request, _ := subscriptionRequestWithID(
		key, subscribe, uint64(time.Now().UnixNano()),
	)
	return request
}

func subscriptionRequestWithID(
	key Key,
	subscribe bool,
	requestID uint64,
) (any, string) {
	operation := "subscribe"
	if !subscribe {
		operation = "unsubscribe"
	}
	switch key.Venue {
	case VenueBinance:
		return map[string]any{
			"method": strings.ToUpper(operation),
			"params": []string{strings.ToLower(key.Symbol) + "@depth10@100ms"},
			"id":     requestID,
		}, requestCorrelation(strconv.FormatUint(requestID, 10))
	case VenueOKX:
		return map[string]any{
			"op": operation,
			"args": []map[string]string{{
				"channel": "books5",
				"instId":  key.Symbol,
			}},
		}, symbolCorrelation(operation, key.Symbol)
	case VenueBybit:
		return map[string]any{
			"req_id": strconv.FormatUint(requestID, 10),
			"op":     operation,
			"args":   []string{"orderbook.1." + key.Symbol},
		}, requestCorrelation(strconv.FormatUint(requestID, 10))
	case VenueBitget:
		instrumentType := "SPOT"
		if key.Product == ProductPerpetual {
			instrumentType = bitgetFuturesType(key.Symbol)
		}
		return map[string]any{
			"op": operation,
			"args": []map[string]string{{
				"instType": instrumentType,
				"channel":  "books15",
				"instId":   key.Symbol,
			}},
		}, symbolCorrelation(operation, key.Symbol)
	case VenueGate:
		channel := "spot.order_book"
		payload := []string{key.Symbol, "10", "100ms"}
		if key.Product == ProductPerpetual {
			channel = "futures.book_ticker"
			payload = []string{key.Symbol}
		}
		return map[string]any{
			"time":    time.Now().Unix(),
			"id":      requestID,
			"channel": channel,
			"event":   operation,
			"payload": payload,
		}, requestCorrelation(strconv.FormatUint(requestID, 10))
	case VenueHyperliquid:
		return map[string]any{
			"method": operation,
			"subscription": map[string]string{
				"type": "bbo", "coin": key.Symbol,
			},
		}, symbolCorrelation(operation, key.Symbol)
	case VenueAster:
		return map[string]any{
			"method": strings.ToUpper(operation),
			"params": []string{strings.ToLower(key.Symbol) + "@bookTicker"},
			"id":     requestID,
		}, requestCorrelation(strconv.FormatUint(requestID, 10))
	default:
		return nil, ""
	}
}

func messageSymbols(venue string, payload []byte) ([]string, error) {
	switch venue {
	case VenueBinance:
		var message struct {
			Stream string `json:"stream"`
		}
		if err := json.Unmarshal(payload, &message); err != nil {
			return nil, err
		}
		stream, _, _ := strings.Cut(message.Stream, "@")
		return []string{strings.ToUpper(stream)}, nil
	case VenueOKX:
		var message struct {
			Argument struct {
				Symbol string `json:"instId"`
			} `json:"arg"`
			Data []struct {
				Symbol string `json:"instId"`
			} `json:"data"`
		}
		if err := json.Unmarshal(payload, &message); err != nil {
			return nil, err
		}
		symbols := make([]string, 0, len(message.Data))
		for _, item := range message.Data {
			if item.Symbol != "" {
				symbols = append(symbols, item.Symbol)
			}
		}
		if len(symbols) == 0 && message.Argument.Symbol != "" {
			symbols = append(symbols, message.Argument.Symbol)
		}
		return symbols, nil
	case VenueBybit:
		var message struct {
			Topic string `json:"topic"`
			Data  struct {
				Symbol string `json:"s"`
			} `json:"data"`
		}
		if err := json.Unmarshal(payload, &message); err != nil {
			return nil, err
		}
		if message.Data.Symbol != "" {
			return []string{message.Data.Symbol}, nil
		}
		topic := strings.TrimPrefix(message.Topic, "orderbook.1.")
		if topic == message.Topic {
			return nil, nil
		}
		return []string{topic}, nil
	case VenueBitget:
		var message struct {
			Argument struct {
				Symbol string `json:"instId"`
			} `json:"arg"`
		}
		if err := json.Unmarshal(payload, &message); err != nil {
			return nil, err
		}
		return []string{message.Argument.Symbol}, nil
	case VenueGate:
		var message struct {
			Channel string `json:"channel"`
			Result  struct {
				Symbol   string `json:"s"`
				Contract string `json:"contract"`
			} `json:"result"`
		}
		if err := json.Unmarshal(payload, &message); err != nil {
			return nil, err
		}
		symbol := message.Result.Symbol
		if symbol == "" {
			symbol = message.Result.Contract
		}
		return []string{symbol}, nil
	case VenueHyperliquid:
		var message struct {
			Channel string `json:"channel"`
			Data    struct {
				Coin string `json:"coin"`
			} `json:"data"`
		}
		if err := json.Unmarshal(payload, &message); err != nil {
			return nil, err
		}
		if message.Channel != "bbo" {
			return nil, nil
		}
		return []string{message.Data.Coin}, nil
	case VenueAster:
		var message struct {
			Symbol string `json:"s"`
		}
		if err := json.Unmarshal(payload, &message); err != nil {
			return nil, err
		}
		return []string{message.Symbol}, nil
	}
	return nil, errors.New("unsupported websocket venue")
}

func canonicalSymbol(symbol string) string {
	return strings.ToUpper(strings.TrimSpace(symbol))
}

func bitgetFuturesType(symbol string) string {
	upper := strings.ToUpper(symbol)
	if strings.HasSuffix(upper, "USDC") {
		return "USDC-FUTURES"
	}
	return "USDT-FUTURES"
}

var _ Connector = (*WebSocketConnector)(nil)
var _ Connection = (*logicalConnection)(nil)
