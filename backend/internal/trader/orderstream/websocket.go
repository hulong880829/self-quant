package orderstream

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Connection is an authenticated, subscribed account stream. Close unblocks Read.
type Connection interface {
	Read() ([]byte, error)
	Close() error
}

type Connector interface {
	Connect(context.Context, Key, Credentials) (Connection, error)
}

type ConnectorOptions struct {
	URLs             URLs
	HTTPClient       *http.Client
	Dialer           *websocket.Dialer
	Heartbeat        time.Duration
	StaleAfter       time.Duration
	ListenKeyRefresh time.Duration
	Now              func() time.Time
}

type WebSocketConnector struct {
	urls             URLs
	httpClient       *http.Client
	dialer           *websocket.Dialer
	heartbeat        time.Duration
	staleAfter       time.Duration
	listenKeyRefresh time.Duration
	now              func() time.Time
}

func NewWebSocketConnector(options ConnectorOptions) *WebSocketConnector {
	defaults := DefaultURLs()
	mergeURLs(&options.URLs, defaults)
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if options.Dialer == nil {
		options.Dialer = &websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	}
	if options.Heartbeat <= 0 {
		options.Heartbeat = 20 * time.Second
	}
	if options.StaleAfter <= 0 {
		options.StaleAfter = 60 * time.Second
	}
	if options.ListenKeyRefresh <= 0 {
		options.ListenKeyRefresh = 30 * time.Minute
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &WebSocketConnector{
		urls: options.URLs, httpClient: options.HTTPClient, dialer: options.Dialer,
		heartbeat: options.Heartbeat, staleAfter: options.StaleAfter,
		listenKeyRefresh: options.ListenKeyRefresh, now: options.Now,
	}
}

func (c *WebSocketConnector) Connect(
	ctx context.Context,
	key Key,
	credentials Credentials,
) (Connection, error) {
	var listenKey string
	var err error
	if key.Venue == VenueBinance {
		listenKey, err = c.createListenKey(ctx, key, credentials)
		if err != nil {
			return nil, err
		}
	}
	endpoint, err := c.websocketURL(key, listenKey)
	if err != nil {
		return nil, err
	}
	connection, response, err := c.dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("dial %s private websocket (HTTP %d): %w", key.Venue, response.StatusCode, err)
		}
		return nil, fmt.Errorf("dial %s private websocket: %w", key.Venue, err)
	}
	streamCtx, cancel := context.WithCancel(context.Background())
	stream := &websocketConnection{
		conn: connection, cancel: cancel, done: make(chan struct{}),
		heartbeat: c.heartbeat, staleAfter: c.staleAfter,
	}
	if err := c.authenticateAndSubscribe(key, credentials, stream); err != nil {
		_ = stream.Close()
		return nil, err
	}
	go stream.heartbeatLoop(streamCtx, key.Venue)
	if listenKey != "" {
		stream.onClose = func() {
			c.closeListenKey(key, credentials)
		}
		go c.refreshListenKeyLoop(streamCtx, stream, key, credentials, listenKey)
	}
	return stream, nil
}

type websocketConnection struct {
	conn       *websocket.Conn
	cancel     context.CancelFunc
	done       chan struct{}
	closeOnce  sync.Once
	writeMu    sync.Mutex
	heartbeat  time.Duration
	staleAfter time.Duration
	onClose    func()
}

func (c *websocketConnection) Read() ([]byte, error) {
	for {
		if c.staleAfter > 0 {
			if err := c.conn.SetReadDeadline(time.Now().Add(c.staleAfter)); err != nil {
				return nil, err
			}
		}
		messageType, payload, err := c.conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(string(payload)), "pong") {
			continue
		}
		return payload, nil
	}
}

func (c *websocketConnection) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.cancel()
		close(c.done)
		err = c.conn.Close()
		if c.onClose != nil {
			go c.onClose()
		}
	})
	return err
}

func (c *WebSocketConnector) closeListenKey(key Key, credentials Credentials) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx, http.MethodDelete, c.restURL(key)+"/papi/v1/listenKey", nil,
	)
	if err != nil {
		return
	}
	request.Header.Set("X-MBX-APIKEY", credentials.APIKey)
	response, err := c.httpClient.Do(request)
	if err == nil && response != nil {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
}

func (c *websocketConnection) writeJSON(value any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return c.conn.WriteJSON(value)
}

func (c *websocketConnection) heartbeatLoop(ctx context.Context, venue string) {
	ticker := time.NewTicker(c.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var err error
			switch venue {
			case VenueOKX, VenueBitget:
				c.writeMu.Lock()
				err = c.conn.WriteMessage(websocket.TextMessage, []byte("ping"))
				c.writeMu.Unlock()
			case VenueBybit:
				err = c.writeJSON(map[string]string{"op": "ping"})
			default:
				c.writeMu.Lock()
				err = c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
				c.writeMu.Unlock()
			}
			if err != nil {
				_ = c.Close()
				return
			}
		}
	}
}

func (c *WebSocketConnector) authenticateAndSubscribe(
	key Key,
	credentials Credentials,
	connection *websocketConnection,
) error {
	if key.Venue != VenueGate && key.Venue != VenueBinance {
		login := loginRequest(key.Venue, credentials, c.now())
		if err := connection.writeJSON(login); err != nil {
			return fmt.Errorf("authenticate %s websocket: %w", key.Venue, err)
		}
		if err := awaitAuthentication(connection.conn, key.Venue); err != nil {
			return err
		}
	}
	for _, request := range subscriptionRequests(key, credentials, c.now()) {
		if err := connection.writeJSON(request); err != nil {
			return fmt.Errorf("subscribe %s private websocket: %w", key.Venue, err)
		}
	}
	return nil
}

func awaitAuthentication(connection *websocket.Conn, venue string) error {
	if err := connection.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	_, payload, err := connection.ReadMessage()
	if err != nil {
		return fmt.Errorf("read %s authentication response: %w", venue, err)
	}
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		return err
	}
	var response map[string]any
	if err := json.Unmarshal(payload, &response); err != nil {
		return fmt.Errorf("decode %s authentication response: %w", venue, err)
	}
	success, exists := response["success"].(bool)
	if exists && !success {
		return fmt.Errorf("%s websocket authentication rejected", venue)
	}
	if code, ok := response["code"].(float64); ok && code != 0 {
		return fmt.Errorf("%s websocket authentication rejected with code %.0f", venue, code)
	}
	if code, ok := response["code"].(string); ok && code != "" && code != "0" {
		return fmt.Errorf("%s websocket authentication rejected with code %s", venue, code)
	}
	if event, _ := response["event"].(string); event == "error" {
		return fmt.Errorf("%s websocket authentication rejected", venue)
	}
	return nil
}

func loginRequest(venue string, credentials Credentials, now time.Time) any {
	switch venue {
	case VenueOKX:
		timestamp := strconv.FormatFloat(float64(now.UnixMilli())/1000, 'f', 3, 64)
		signature := base64HMAC(credentials.Secret, timestamp+"GET"+"/users/self/verify")
		return map[string]any{"op": "login", "args": []map[string]string{{
			"apiKey": credentials.APIKey, "passphrase": credentials.Passphrase,
			"timestamp": timestamp, "sign": signature,
		}}}
	case VenueBybit:
		expires := now.Add(10 * time.Second).UnixMilli()
		signature := hexHMAC(credentials.Secret, "GET/realtime"+strconv.FormatInt(expires, 10))
		return map[string]any{"op": "auth", "args": []any{credentials.APIKey, expires, signature}}
	case VenueBitget:
		timestamp := strconv.FormatInt(now.Unix(), 10)
		signature := base64HMAC(credentials.Secret, timestamp+"GET"+"/user/verify")
		return map[string]any{"op": "login", "args": []map[string]string{{
			"apiKey": credentials.APIKey, "passphrase": credentials.Passphrase,
			"timestamp": timestamp, "sign": signature,
		}}}
	default:
		return nil
	}
}

func subscriptionRequests(key Key, credentials Credentials, now time.Time) []any {
	switch key.Venue {
	case VenueBinance:
		return nil
	case VenueOKX:
		return []any{map[string]any{"op": "subscribe", "args": []map[string]string{{
			"channel": "orders", "instType": "ANY",
		}}}}
	case VenueBybit:
		return []any{map[string]any{"op": "subscribe", "args": []string{"order", "execution.fast"}}}
	case VenueBitget:
		return []any{map[string]any{"op": "subscribe", "args": []map[string]string{
			{"instType": "UTA", "topic": "order"},
			{"instType": "UTA", "topic": "fill"},
			{"instType": "UTA", "topic": "fast-fill", "symbol": "default"},
		}}}
	case VenueGate:
		prefix := "spot"
		if key.Product == ProductPerpetual {
			prefix = "futures"
		}
		return []any{
			gateSubscription(prefix+".orders", credentials, now),
			gateSubscription(prefix+".usertrades", credentials, now),
		}
	default:
		return nil
	}
}

func gateSubscription(channel string, credentials Credentials, now time.Time) any {
	timestamp := now.Unix()
	signatureText := "channel=" + channel + "&event=subscribe&time=" + strconv.FormatInt(timestamp, 10)
	return map[string]any{
		"time": timestamp, "channel": channel, "event": "subscribe",
		"payload": []string{"!all"},
		"auth": map[string]string{
			"method": "api_key", "KEY": credentials.APIKey,
			"SIGN": hexHMAC(credentials.Secret, signatureText),
		},
	}
}

func (c *WebSocketConnector) createListenKey(
	ctx context.Context,
	key Key,
	credentials Credentials,
) (string, error) {
	path := "/papi/v1/listenKey"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.restURL(key)+path, nil)
	if err != nil {
		return "", fmt.Errorf("create Binance listen-key request: %w", err)
	}
	request.Header.Set("X-MBX-APIKEY", credentials.APIKey)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("create Binance listen key: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return "", fmt.Errorf("create Binance listen key: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var result struct {
		ListenKey string `json:"listenKey"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode Binance listen key: %w", err)
	}
	if result.ListenKey == "" {
		return "", errors.New("Binance returned an empty listen key")
	}
	return result.ListenKey, nil
}

func (c *WebSocketConnector) refreshListenKeyLoop(
	ctx context.Context,
	connection *websocketConnection,
	key Key,
	credentials Credentials,
	listenKey string,
) {
	ticker := time.NewTicker(c.listenKeyRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			path := "/papi/v1/listenKey"
			endpoint := c.restURL(key) + path + "?listenKey=" + url.QueryEscape(listenKey)
			request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, nil)
			if err == nil {
				request.Header.Set("X-MBX-APIKEY", credentials.APIKey)
				var response *http.Response
				response, err = c.httpClient.Do(request)
				if response != nil {
					_, _ = io.Copy(io.Discard, response.Body)
					_ = response.Body.Close()
					if response.StatusCode/100 != 2 {
						err = fmt.Errorf("HTTP %d", response.StatusCode)
					}
				}
			}
			if err != nil {
				_ = connection.Close()
				return
			}
		}
	}
}

func (c *WebSocketConnector) websocketURL(key Key, listenKey string) (string, error) {
	urls := c.venueURLs(key.Venue)
	endpoint := urls.SpotWS
	if key.Product == ProductPerpetual {
		endpoint = urls.PerpetualWS
	}
	if endpoint == "" {
		return "", fmt.Errorf("%w: empty %s websocket URL", ErrUnsupportedKey, key.Venue)
	}
	if listenKey != "" {
		endpoint = strings.TrimRight(endpoint, "/") + "/" + url.PathEscape(listenKey)
	}
	return endpoint, nil
}

func (c *WebSocketConnector) restURL(key Key) string {
	urls := c.venueURLs(key.Venue)
	if key.Product == ProductPerpetual {
		return strings.TrimRight(urls.PerpetualREST, "/")
	}
	return strings.TrimRight(urls.SpotREST, "/")
}

func (c *WebSocketConnector) venueURLs(venue string) VenueURLs {
	switch venue {
	case VenueBinance:
		return c.urls.Binance
	case VenueOKX:
		return c.urls.OKX
	case VenueBybit:
		return c.urls.Bybit
	case VenueBitget:
		return c.urls.Bitget
	case VenueGate:
		return c.urls.Gate
	default:
		return VenueURLs{}
	}
}

func mergeURLs(target *URLs, defaults URLs) {
	mergeVenueURLs(&target.Binance, defaults.Binance)
	mergeVenueURLs(&target.OKX, defaults.OKX)
	mergeVenueURLs(&target.Bybit, defaults.Bybit)
	mergeVenueURLs(&target.Bitget, defaults.Bitget)
	mergeVenueURLs(&target.Gate, defaults.Gate)
}

func mergeVenueURLs(target *VenueURLs, defaults VenueURLs) {
	if target.SpotWS == "" {
		target.SpotWS = defaults.SpotWS
	}
	if target.PerpetualWS == "" {
		target.PerpetualWS = defaults.PerpetualWS
	}
	if target.SpotREST == "" {
		target.SpotREST = defaults.SpotREST
	}
	if target.PerpetualREST == "" {
		target.PerpetualREST = defaults.PerpetualREST
	}
}

func hexHMAC(secret, value string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func base64HMAC(secret, value string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(value))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

var _ Connector = (*WebSocketConnector)(nil)
var _ Connection = (*websocketConnection)(nil)
