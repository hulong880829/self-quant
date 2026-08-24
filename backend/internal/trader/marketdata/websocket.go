package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type WebSocketConnector struct {
	mu       sync.Mutex
	pools    map[websocketPoolKey]*websocketPool
	dial     func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error)
	endpoint func(Key) (string, error)
}

func NewWebSocketConnector() *WebSocketConnector {
	dialer := &websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   32 * 1024,
		WriteBufferSize:  8 * 1024,
	}
	return &WebSocketConnector{
		pools:    make(map[websocketPoolKey]*websocketPool),
		dial:     dialer.DialContext,
		endpoint: websocketEndpoint,
	}
}

func (c *WebSocketConnector) Connect(ctx context.Context, key Key) (Connection, error) {
	normalized, err := NewKey(key.Venue, key.Product, key.Symbol)
	if err != nil {
		return nil, err
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
			connector: c,
			key:       group,
			ready:     make(chan struct{}),
			done:      make(chan struct{}),
			logical:   make(map[string]map[*logicalConnection]struct{}),
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

	logical := &logicalConnection{
		pool:     pool,
		key:      normalized,
		messages: make(chan websocketRead, 1),
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

type websocketPool struct {
	connector  *WebSocketConnector
	key        websocketPoolKey
	ready      chan struct{}
	openErr    error
	connection *websocket.Conn

	subscriptionMu sync.Mutex
	mu             sync.Mutex
	logical        map[string]map[*logicalConnection]struct{}
	writeMu        sync.Mutex
	closeOnce      sync.Once
	done           chan struct{}
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
	p.connection.SetReadLimit(256 * 1024)
	close(p.ready)
	go p.readLoop()
	go p.heartbeat()
}

func (p *websocketPool) add(connection *logicalConnection) error {
	p.subscriptionMu.Lock()
	defer p.subscriptionMu.Unlock()

	symbol := canonicalSymbol(connection.key.Symbol)
	p.mu.Lock()
	select {
	case <-p.done:
		p.mu.Unlock()
		return io.ErrClosedPipe
	default:
	}
	connections := p.logical[symbol]
	first := len(connections) == 0
	if connections == nil {
		connections = make(map[*logicalConnection]struct{})
		p.logical[symbol] = connections
	}
	connections[connection] = struct{}{}
	p.mu.Unlock()

	if first {
		if err := p.writeJSON(subscriptionRequest(connection.key, true)); err != nil {
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
	}
	lastConnection := len(p.logical) == 0
	p.mu.Unlock()

	if lastConnection {
		p.shutdown(ErrClosed)
		return nil
	}
	if lastSymbol {
		if err := p.writeJSON(subscriptionRequest(connection.key, false)); err != nil {
			wrapped := fmt.Errorf("unsubscribe %s BBO: %w", connection.key.Venue, err)
			p.shutdown(wrapped)
			return wrapped
		}
	}
	return nil
}

func (p *websocketPool) readLoop() {
	for {
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
		symbols, err := messageSymbols(p.key.venue, payload)
		if err != nil {
			p.shutdown(fmt.Errorf("demux %s websocket message: %w", p.key.venue, err))
			return
		}
		for _, symbol := range symbols {
			if symbol != "" {
				p.deliver(symbol, payload)
			}
		}
	}
}

func (p *websocketPool) deliver(symbol string, payload []byte) {
	p.mu.Lock()
	connections := make([]*logicalConnection, 0, len(p.logical[canonicalSymbol(symbol)]))
	for connection := range p.logical[canonicalSymbol(symbol)] {
		connections = append(connections, connection)
	}
	p.mu.Unlock()
	for _, connection := range connections {
		connection.deliver(payload)
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
		for _, connection := range connections {
			connection.finish(err)
		}
	})
}

func (p *websocketPool) writeJSON(value any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := p.connection.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return p.connection.WriteJSON(value)
}

func (p *websocketPool) heartbeat() {
	ticker := time.NewTicker(20 * time.Second)
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
			case VenueGate:
				p.writeMu.Lock()
				err = p.connection.WriteControl(
					websocket.PingMessage, nil, time.Now().Add(5*time.Second),
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
	if err := p.connection.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
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
		c.messages <- websocketRead{payload: payload}
	}
}

func (c *logicalConnection) finish(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		select {
		case <-c.messages:
		default:
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
		url := "wss://api.gateio.ws/ws/v4/"
		if key.Product == ProductPerpetual {
			return url + "futures/usdt", nil
		}
		return url + "spot", nil
	default:
		return "", fmt.Errorf("%w: venue %q", ErrUnsupportedKey, key.Venue)
	}
}

func subscriptionRequest(key Key, subscribe bool) any {
	operation := "subscribe"
	if !subscribe {
		operation = "unsubscribe"
	}
	switch key.Venue {
	case VenueBinance:
		return map[string]any{
			"method": strings.ToUpper(operation),
			"params": []string{strings.ToLower(key.Symbol) + "@depth10@100ms"},
			"id":     time.Now().UnixNano(),
		}
	case VenueOKX:
		return map[string]any{
			"op": operation,
			"args": []map[string]string{{
				"channel": "books5",
				"instId":  key.Symbol,
			}},
		}
	case VenueBybit:
		return map[string]any{
			"op":   operation,
			"args": []string{"orderbook.1." + key.Symbol},
		}
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
		}
	case VenueGate:
		channel := "spot.order_book"
		payload := []string{key.Symbol, "10", "100ms"}
		if key.Product == ProductPerpetual {
			channel = "futures.order_book"
			// Gate's legacy futures snapshot channel only accepts interval "0".
			payload = []string{key.Symbol, "10", "0"}
		}
		return map[string]any{
			"time":    time.Now().Unix(),
			"channel": channel,
			"event":   operation,
			"payload": payload,
		}
	default:
		return nil
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
