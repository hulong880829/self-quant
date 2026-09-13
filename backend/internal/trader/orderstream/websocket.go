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
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	lighterclient "github.com/elliottech/lighter-go/client"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/gorilla/websocket"
	corex "selfquant/backend/internal/exchange"
)

const websocketWriteTimeout = 5 * time.Second

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
	asterNonce       atomic.Int64
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
	if key.Venue == VenueBinance || key.Venue == VenueAster {
		listenKey, err = c.createListenKey(ctx, key, credentials)
		if err != nil {
			return nil, err
		}
	}
	if key.Venue == VenueLighter {
		credentials.AuthToken, err = lighterAuthToken(credentials, c.now())
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
		heartbeat: c.heartbeat, staleAfter: c.staleAfter, connectedAt: time.Now(),
	}
	if err := c.authenticateAndSubscribe(key, credentials, stream); err != nil {
		_ = stream.Close()
		return nil, newDisconnectError(classifyConnectError(err), err)
	}
	stream.configureControlHandlers()
	go stream.heartbeatLoop(streamCtx, key.Venue)
	if listenKey != "" {
		stream.onClose = func() {
			c.closeListenKey(key, credentials, listenKey)
		}
		go c.refreshListenKeyLoop(streamCtx, stream, key, credentials, listenKey)
	}
	switch key.Venue {
	case VenueAster:
		go rotateConnection(streamCtx, stream, 23*time.Hour)
	case VenueLighter:
		go rotateConnection(streamCtx, stream, 9*time.Minute)
	}
	return stream, nil
}

type websocketConnection struct {
	conn              *websocket.Conn
	cancel            context.CancelFunc
	done              chan struct{}
	closeOnce         sync.Once
	writeMu           sync.Mutex
	reasonMu          sync.Mutex
	heartbeat         time.Duration
	staleAfter        time.Duration
	connectedAt       time.Time
	lastWriteDeadline time.Time
	closeReason       disconnectReason
	heartbeatType     string
	heartbeatDeadline time.Time
	heartbeatErr      error
	onClose           func()
	pending           [][]byte
}

func (c *websocketConnection) configureControlHandlers() {
	c.conn.SetPongHandler(func(string) error {
		return c.refreshReadDeadline()
	})
	c.conn.SetPingHandler(func(appData string) error {
		if err := c.refreshReadDeadline(); err != nil {
			return err
		}
		err := c.writeControl(websocket.PongMessage, []byte(appData))
		if err == websocket.ErrCloseSent {
			return nil
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil
		}
		return err
	})
}

func (c *websocketConnection) refreshReadDeadline() error {
	if c.staleAfter <= 0 {
		return nil
	}
	return c.conn.SetReadDeadline(time.Now().Add(c.staleAfter))
}

func (c *websocketConnection) setCloseReason(reason disconnectReason) {
	if reason == "" {
		return
	}
	c.reasonMu.Lock()
	defer c.reasonMu.Unlock()
	if c.closeReason == "" {
		c.closeReason = reason
	}
}

func (c *websocketConnection) firstCloseReason() disconnectReason {
	c.reasonMu.Lock()
	defer c.reasonMu.Unlock()
	return c.closeReason
}

func (c *websocketConnection) closeWithReason(reason disconnectReason) error {
	c.setCloseReason(reason)
	return c.Close()
}

func (c *websocketConnection) wrapReadError(err error) error {
	if err == nil {
		return nil
	}
	reason := c.firstCloseReason()
	if reason == "" {
		reason = classifyIOError(err)
	}
	c.reasonMu.Lock()
	heartbeatType := c.heartbeatType
	heartbeatDeadline := c.heartbeatDeadline
	heartbeatErr := c.heartbeatErr
	c.reasonMu.Unlock()
	cause := err
	if heartbeatErr != nil {
		cause = heartbeatErr
	}
	wrapped := newDisconnectError(reason, cause)
	var de *disconnectError
	if errors.As(wrapped, &de) {
		de.HeartbeatType = heartbeatType
		de.WriteDeadline = heartbeatDeadline
	}
	return wrapped
}

func (c *websocketConnection) Read() ([]byte, error) {
	for {
		if len(c.pending) > 0 {
			payload := c.pending[0]
			c.pending = c.pending[1:]
			return payload, nil
		}
		if err := c.refreshReadDeadline(); err != nil {
			return nil, c.wrapReadError(err)
		}
		messageType, payload, err := c.conn.ReadMessage()
		if err != nil {
			return nil, c.wrapReadError(err)
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

func (c *WebSocketConnector) closeListenKey(
	key Key,
	credentials Credentials,
	listenKey string,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := c.listenKeyRequest(ctx, http.MethodDelete, key, credentials, listenKey)
	if err != nil {
		return
	}
	response, err := c.httpClient.Do(request)
	if err == nil && response != nil {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
}

func (c *websocketConnection) seedExpiredWriteDeadlineForTest() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.setWriteDeadlineLocked(time.Now().Add(-time.Second))
}

func (c *websocketConnection) closeUnderlyingForTest() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.Close()
}

func (c *websocketConnection) setWriteDeadlineLocked(deadline time.Time) error {
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if netConn := c.conn.NetConn(); netConn != nil {
		return netConn.SetWriteDeadline(deadline)
	}
	return nil
}

func (c *websocketConnection) writeLocked(write func() error) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	deadline := time.Now().Add(websocketWriteTimeout)
	c.lastWriteDeadline = deadline
	if err := c.setWriteDeadlineLocked(deadline); err != nil {
		return err
	}
	writeErr := write()
	if clearErr := c.setWriteDeadlineLocked(time.Time{}); clearErr != nil && writeErr == nil {
		return clearErr
	}
	return writeErr
}

func (c *websocketConnection) writeJSON(value any) error {
	return c.writeLocked(func() error {
		return c.conn.WriteJSON(value)
	})
}

func (c *websocketConnection) writeText(payload []byte) error {
	return c.writeLocked(func() error {
		return c.conn.WriteMessage(websocket.TextMessage, payload)
	})
}

func (c *websocketConnection) writeControl(messageType int, data []byte) error {
	return c.writeLocked(func() error {
		return c.conn.WriteControl(messageType, data, time.Now().Add(websocketWriteTimeout))
	})
}

func (c *websocketConnection) failHeartbeat(heartbeatType string, err error) {
	c.writeMu.Lock()
	deadline := c.lastWriteDeadline
	c.writeMu.Unlock()
	c.reasonMu.Lock()
	if c.heartbeatErr == nil {
		c.heartbeatType = heartbeatType
		c.heartbeatDeadline = deadline
		c.heartbeatErr = err
	}
	c.reasonMu.Unlock()
	_ = c.closeWithReason(reasonHeartbeatWriteFailed)
}

func (c *websocketConnection) heartbeatLoop(ctx context.Context, venue string) {
	if err := c.sendHeartbeat(venue); err != nil {
		return
	}
	ticker := time.NewTicker(c.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.sendHeartbeat(venue); err != nil {
				return
			}
		}
	}
}

func (c *websocketConnection) sendHeartbeat(venue string) error {
	heartbeatType := "control_ping"
	var err error
	switch venue {
	case VenueOKX, VenueBitget:
		heartbeatType = "text_ping"
		err = c.writeText([]byte("ping"))
	case VenueBybit:
		heartbeatType = "json_ping"
		err = c.writeJSON(map[string]string{"op": "ping"})
	case VenueHyperliquid:
		heartbeatType = "json_ping"
		err = c.writeJSON(map[string]string{"method": "ping"})
	case VenueLighter:
		heartbeatType = "json_ping"
		err = c.writeJSON(map[string]string{"type": "ping"})
	default:
		err = c.writeControl(websocket.PingMessage, []byte{})
	}
	if err != nil {
		c.failHeartbeat(heartbeatType, err)
	}
	return err
}

func (c *WebSocketConnector) authenticateAndSubscribe(
	key Key,
	credentials Credentials,
	connection *websocketConnection,
) error {
	if key.Venue == VenueOKX || key.Venue == VenueBybit || key.Venue == VenueBitget {
		login := loginRequest(key.Venue, credentials, c.now())
		if err := connection.writeJSON(login); err != nil {
			return fmt.Errorf("authenticate %s websocket: %w", key.Venue, err)
		}
		if err := awaitAuthentication(connection.conn, key.Venue); err != nil {
			return err
		}
	}
	requests := subscriptionRequests(key, credentials, c.now())
	for _, request := range requests {
		if err := connection.writeJSON(request); err != nil {
			return fmt.Errorf("subscribe %s private websocket: %w", key.Venue, err)
		}
	}
	if key.Venue == VenueHyperliquid || key.Venue == VenueLighter {
		if err := awaitSubscriptionMessages(connection, key.Venue, len(requests)); err != nil {
			return err
		}
	}
	return nil
}

func awaitSubscriptionMessages(
	connection *websocketConnection,
	venue string,
	count int,
) error {
	if count == 0 {
		return nil
	}
	if err := connection.conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	defer connection.conn.SetReadDeadline(time.Time{})
	for range count {
		_, payload, err := connection.conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read %s subscription response: %w", venue, err)
		}
		connection.pending = append(connection.pending, payload)
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
	case VenueHyperliquid:
		user := firstNonEmpty(credentials.VaultAddress, credentials.SigningAddress, credentials.APIKey)
		return []any{
			map[string]any{"method": "subscribe", "subscription": map[string]string{
				"type": "orderUpdates", "user": user,
			}},
			map[string]any{"method": "subscribe", "subscription": map[string]string{
				"type": "userFills", "user": user,
			}},
		}
	case VenueLighter:
		accountIndex := int64(0)
		if credentials.AccountIndex != nil {
			accountIndex = *credentials.AccountIndex
		}
		return []any{map[string]any{
			"type":    "subscribe",
			"channel": "account_all_orders/" + strconv.FormatInt(accountIndex, 10),
			"auth":    credentials.AuthToken,
		}}
	case VenueAster:
		return nil
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
	request, err := c.listenKeyRequest(ctx, http.MethodPost, key, credentials, "")
	if err != nil {
		return "", fmt.Errorf("create %s listen-key request: %w", key.Venue, err)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("create %s listen key: %w", key.Venue, err)
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return "", fmt.Errorf("create %s listen key: HTTP %d: %s", key.Venue, response.StatusCode, strings.TrimSpace(string(body)))
	}
	var result struct {
		ListenKey string `json:"listenKey"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode %s listen key: %w", key.Venue, err)
	}
	if result.ListenKey == "" {
		return "", fmt.Errorf("%s returned an empty listen key", key.Venue)
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
	cycle := int64(0)
	for {
		delay := c.listenKeyRefresh
		if key.Venue == VenueAster && delay >= 25*time.Minute {
			const spread = 5 * time.Minute
			jitter := time.Duration((c.now().UnixNano()+cycle)%int64(spread)) - spread/2
			delay += jitter
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			cycle++
			request, err := c.listenKeyRequest(
				ctx, http.MethodPut, key, credentials, listenKey,
			)
			if err == nil {
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
				_ = connection.closeWithReason(reasonListenKeyRefreshFailed)
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
	case VenueHyperliquid:
		return c.urls.Hyperliquid
	case VenueLighter:
		return c.urls.Lighter
	case VenueAster:
		return c.urls.Aster
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
	mergeVenueURLs(&target.Hyperliquid, defaults.Hyperliquid)
	mergeVenueURLs(&target.Lighter, defaults.Lighter)
	mergeVenueURLs(&target.Aster, defaults.Aster)
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

func (c *WebSocketConnector) listenKeyRequest(
	ctx context.Context,
	method string,
	key Key,
	credentials Credentials,
	listenKey string,
) (*http.Request, error) {
	path := listenKeyPath(key.Venue, credentials)
	values := url.Values{}
	if listenKey != "" {
		values.Set("listenKey", listenKey)
	}
	if key.Venue == VenueAster && asterUsesAPIWallet(credentials) {
		return c.asterV3Request(ctx, method, c.restURL(key)+path, credentials, values)
	}
	endpoint := c.restURL(key) + path
	if len(values) > 0 {
		endpoint += "?" + values.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-MBX-APIKEY", credentials.APIKey)
	return request, nil
}

func (c *WebSocketConnector) asterV3Request(
	ctx context.Context,
	method string,
	endpoint string,
	credentials Credentials,
	values url.Values,
) (*http.Request, error) {
	user := strings.TrimSpace(credentials.APIKey)
	if !common.IsHexAddress(user) {
		return nil, errors.New("Aster API wallet address is required")
	}
	privateKey, err := crypto.HexToECDSA(
		strings.TrimPrefix(strings.TrimSpace(credentials.Secret), "0x"),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid Aster API wallet key: %w", err)
	}
	params := make(map[string]string, len(values)+5)
	for name, items := range values {
		if len(items) > 0 && name != "signature" {
			params[name] = items[0]
		}
	}
	now := c.now()
	params["user"] = common.HexToAddress(user).Hex()
	params["signer"] = crypto.PubkeyToAddress(privateKey.PublicKey).Hex()
	params["nonce"] = strconv.FormatInt(c.nextAsterNonce(now), 10)
	params["timestamp"] = strconv.FormatInt(now.UnixMilli(), 10)
	params["recvWindow"] = "5000"
	message := corex.AsterParamString(params)
	signature, err := corex.SignAsterV3(privateKey, message)
	if err != nil {
		return nil, err
	}
	encoded := message + "&signature=" + url.QueryEscape(signature)
	var body io.Reader
	if method == http.MethodGet {
		endpoint += "?" + encoded
	} else {
		body = strings.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return request, nil
}

func (c *WebSocketConnector) nextAsterNonce(now time.Time) int64 {
	candidate := now.UnixMicro()
	for {
		previous := c.asterNonce.Load()
		if candidate <= previous {
			candidate = previous + 1
		}
		if c.asterNonce.CompareAndSwap(previous, candidate) {
			return candidate
		}
	}
}

func asterUsesAPIWallet(credentials Credentials) bool {
	switch strings.TrimSpace(credentials.CredentialKind) {
	case "aster_hmac":
		return false
	case "aster_api_wallet", "":
		return common.IsHexAddress(strings.TrimSpace(credentials.APIKey))
	default:
		return false
	}
}

func listenKeyPath(venue string, credentials Credentials) string {
	if venue == VenueAster && asterUsesAPIWallet(credentials) {
		return "/fapi/v3/listenKey"
	}
	if venue == VenueAster {
		return "/fapi/v1/listenKey"
	}
	return "/papi/v1/listenKey"
}

func lighterAuthToken(credentials Credentials, now time.Time) (string, error) {
	if credentials.APIKeyIndex == nil || credentials.AccountIndex == nil ||
		*credentials.APIKeyIndex < 0 || *credentials.APIKeyIndex > 255 ||
		*credentials.AccountIndex < 0 {
		return "", errors.New("invalid Lighter order stream account indexes")
	}
	privateKey, err := corex.LighterTxPrivateKey(credentials.Secret)
	if err != nil {
		return "", fmt.Errorf("normalize Lighter order stream key: %w", err)
	}
	signer, err := lighterclient.NewTxClient(
		nil, privateKey, *credentials.AccountIndex,
		uint8(*credentials.APIKeyIndex), 304,
	)
	if err != nil {
		return "", fmt.Errorf("initialize Lighter order stream signer: %w", err)
	}
	token, err := signer.GetAuthToken(now.Add(10 * time.Minute))
	if err != nil {
		return "", fmt.Errorf("sign Lighter order stream token: %w", err)
	}
	return token, nil
}

func rotateConnection(ctx context.Context, connection *websocketConnection, after time.Duration) {
	timer := time.NewTimer(after)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
		_ = connection.closeWithReason(reasonPlannedRotation)
	}
}

var _ Connector = (*WebSocketConnector)(nil)
var _ Connection = (*websocketConnection)(nil)
