package orderstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestBinanceConnectorUsesLocalListenKeyAndRefresh(t *testing.T) {
	t.Parallel()
	var creates atomic.Int64
	var refreshes atomic.Int64
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/papi/v1/listenKey" && request.Method == http.MethodPost:
			if request.Header.Get("X-MBX-APIKEY") != "key" {
				t.Errorf("missing API key header")
			}
			creates.Add(1)
			_ = json.NewEncoder(writer).Encode(map[string]string{"listenKey": "local-listen-key"})
		case request.URL.Path == "/papi/v1/listenKey" && request.Method == http.MethodPut:
			refreshes.Add(1)
			writer.WriteHeader(http.StatusOK)
		case request.URL.Path == "/ws/local-listen-key":
			connection, err := upgrader.Upgrade(writer, request, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			defer connection.Close()
			_ = connection.WriteMessage(websocket.TextMessage, []byte(`{"e":"executionReport"}`))
			<-time.After(100 * time.Millisecond)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	connector := NewWebSocketConnector(ConnectorOptions{
		URLs: URLs{Binance: VenueURLs{
			SpotWS: wsURL, PerpetualWS: wsURL,
			SpotREST: server.URL, PerpetualREST: server.URL,
		}},
		HTTPClient: server.Client(), Heartbeat: time.Hour,
		StaleAfter: time.Second, ListenKeyRefresh: 5 * time.Millisecond,
	})
	connection, err := connector.Connect(
		context.Background(),
		Key{Account: "acct", Venue: VenueBinance, Product: ProductSpot},
		Credentials{APIKey: "key", Secret: "secret"},
	)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer connection.Close()
	payload, err := connection.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(payload), "executionReport") {
		t.Fatalf("unexpected payload: %s", payload)
	}
	deadline := time.Now().Add(time.Second)
	for refreshes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if creates.Load() != 1 || refreshes.Load() == 0 {
		t.Fatalf("creates=%d refreshes=%d", creates.Load(), refreshes.Load())
	}
}

func TestPrivateSubscriptionRequestsCoverEveryVenueAndProduct(t *testing.T) {
	t.Parallel()
	credentials := Credentials{APIKey: "key", Secret: "secret", Passphrase: "pass"}
	now := time.Unix(1_700_000_000, 0)
	for _, venue := range []string{VenueBinance, VenueOKX, VenueBybit, VenueBitget, VenueGate} {
		for _, product := range []string{ProductSpot, ProductPerpetual} {
			key := Key{Account: "acct", Venue: venue, Product: product}
			requests := subscriptionRequests(key, credentials, now)
			if venue == VenueBinance {
				if len(requests) != 0 {
					t.Fatalf("Binance listen-key stream should not subscribe: %v", requests)
				}
				continue
			}
			if len(requests) == 0 {
				t.Fatalf("no %s/%s subscription request", venue, product)
			}
			for _, request := range requests {
				payload, err := json.Marshal(request)
				if err != nil {
					t.Fatalf("marshal %s/%s request: %v", venue, product, err)
				}
				if !strings.Contains(string(payload), "subscribe") {
					t.Fatalf("request does not subscribe: %s", payload)
				}
				if venue == VenueBitget &&
					(!strings.Contains(string(payload), `"instType":"UTA"`) ||
						!strings.Contains(string(payload), `"topic":"fast-fill"`)) {
					t.Fatalf("Bitget did not use UTA private topics: %s", payload)
				}
			}
		}
	}
}

func TestDEXPrivateSubscriptionsAndCredentials(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	hyperliquid := Credentials{
		Secret: "agent-key", SigningAddress: "0xsigner", VaultAddress: "0xvault",
	}
	if err := hyperliquid.validate(VenueHyperliquid); err != nil {
		t.Fatalf("Hyperliquid credentials: %v", err)
	}
	requests := subscriptionRequests(
		Key{Account: "acct", Venue: VenueHyperliquid, Product: ProductPerpetual},
		hyperliquid,
		now,
	)
	raw, _ := json.Marshal(requests)
	if len(requests) != 2 || !strings.Contains(string(raw), "orderUpdates") ||
		!strings.Contains(string(raw), "userFills") ||
		!strings.Contains(string(raw), "0xvault") {
		t.Fatalf("Hyperliquid subscriptions=%s", raw)
	}

	accountIndex, apiKeyIndex := int64(42), int32(3)
	lighter := Credentials{
		Secret: "api-key", AccountIndex: &accountIndex, APIKeyIndex: &apiKeyIndex,
		AuthToken: "token",
	}
	if err := lighter.validate(VenueLighter); err != nil {
		t.Fatalf("Lighter credentials: %v", err)
	}
	requests = subscriptionRequests(
		Key{Account: "acct", Venue: VenueLighter, Product: ProductPerpetual},
		lighter,
		now,
	)
	raw, _ = json.Marshal(requests)
	if len(requests) != 1 || !strings.Contains(string(raw), "account_all_orders/42") ||
		!strings.Contains(string(raw), `"auth":"token"`) {
		t.Fatalf("Lighter subscriptions=%s", raw)
	}
	if len(subscriptionRequests(
		Key{Account: "acct", Venue: VenueAster, Product: ProductPerpetual},
		Credentials{APIKey: "key", Secret: "secret"},
		now,
	)) != 0 {
		t.Fatal("Aster listen-key stream should not send a subscription")
	}
}

func TestLoginRequestsUseVenueSignatures(t *testing.T) {
	t.Parallel()
	credentials := Credentials{APIKey: "key", Secret: "secret", Passphrase: "pass"}
	now := time.Unix(1_700_000_000, 0)
	for _, venue := range []string{VenueOKX, VenueBybit, VenueBitget} {
		payload, err := json.Marshal(loginRequest(venue, credentials, now))
		if err != nil {
			t.Fatalf("marshal %s login: %v", venue, err)
		}
		text := string(payload)
		if !strings.Contains(text, `"key"`) || !strings.Contains(text, `"op"`) {
			t.Fatalf("incomplete %s login request: %s", venue, text)
		}
		if strings.Contains(text, `"secret"`) {
			t.Fatalf("%s login leaked raw secret: %s", venue, text)
		}
	}
}

func TestLighterCredentialsDistinguishUnsetFromZeroIndexes(t *testing.T) {
	t.Parallel()
	zeroAccount, zeroAPIKey := int64(0), int32(0)
	valid := Credentials{
		Secret:       "secret",
		AccountIndex: &zeroAccount,
		APIKeyIndex:  &zeroAPIKey,
	}
	if err := valid.validate(VenueLighter); err != nil {
		t.Fatalf("zero indexes should be valid when set: %v", err)
	}
	valid.AccountIndex = nil
	if err := valid.validate(VenueLighter); err == nil {
		t.Fatal("unset account index was accepted")
	}
}

func TestLighterAuthTokenNormalizesKeyAndChangesWithExpiry(t *testing.T) {
	t.Parallel()
	accountIndex, apiKeyIndex := int64(0), int32(0)
	credentials := Credentials{
		Secret:       "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
		AccountIndex: &accountIndex,
		APIKeyIndex:  &apiKeyIndex,
	}
	first, err := lighterAuthToken(credentials, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	second, err := lighterAuthToken(credentials, time.Unix(1_700_000_001, 0))
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || second == "" || first == second {
		t.Fatalf("tokens were not reissued: first=%q second=%q", first, second)
	}
}

func TestAsterHMACListenKeyRequestRetainsV1(t *testing.T) {
	t.Parallel()
	connector := NewWebSocketConnector(ConnectorOptions{
		URLs: URLs{Aster: VenueURLs{PerpetualREST: "https://aster.invalid"}},
	})
	request, err := connector.listenKeyRequest(
		context.Background(),
		http.MethodPut,
		Key{Account: "acct", Venue: VenueAster, Product: ProductPerpetual},
		Credentials{
			APIKey: "hmac-key", Secret: "hmac-secret", CredentialKind: "aster_hmac",
		},
		"listen-key",
	)
	if err != nil {
		t.Fatal(err)
	}
	if request.URL.Path != "/fapi/v1/listenKey" ||
		request.URL.Query().Get("listenKey") != "listen-key" ||
		request.Header.Get("X-MBX-APIKEY") != "hmac-key" {
		t.Fatalf("request=%s headers=%v", request.URL.String(), request.Header)
	}
}

func TestAsterAPIWalletListenKeyLifecycleUsesSignedV3(t *testing.T) {
	t.Parallel()
	const (
		walletKey     = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
		walletAddress = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	)
	var creates, refreshes, deletes atomic.Int64
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/ws/v3-listen-key" {
			connection, err := upgrader.Upgrade(writer, request, nil)
			if err != nil {
				return
			}
			defer connection.Close()
			_ = connection.WriteMessage(
				websocket.TextMessage,
				[]byte(`{"e":"ORDER_TRADE_UPDATE","o":{"c":"client","X":"NEW"}}`),
			)
			<-time.After(100 * time.Millisecond)
			return
		}
		if request.URL.Path != "/fapi/v3/listenKey" {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("X-MBX-APIKEY") != "" {
			t.Error("V3 request unexpectedly used HMAC API key header")
		}
		if request.URL.Query().Get("signature") != "" {
			t.Errorf("%s V3 auth should be sent in the request body", request.Method)
		}
		if request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("%s V3 content type=%q", request.Method, request.Header.Get("Content-Type"))
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read V3 form: %v", err)
			http.Error(writer, "bad form", http.StatusBadRequest)
			return
		}
		request.Form, err = url.ParseQuery(string(body))
		if err != nil {
			t.Errorf("parse V3 form: %v", err)
			http.Error(writer, "bad form", http.StatusBadRequest)
			return
		}
		for _, field := range []string{"user", "signer", "nonce", "timestamp", "recvWindow", "signature"} {
			if request.Form.Get(field) == "" {
				t.Errorf("%s missing from signed V3 request", field)
			}
		}
		switch request.Method {
		case http.MethodPost:
			creates.Add(1)
			_ = json.NewEncoder(writer).Encode(map[string]string{"listenKey": "v3-listen-key"})
		case http.MethodPut:
			if request.Form.Get("listenKey") != "v3-listen-key" {
				t.Errorf("refresh listenKey=%q", request.Form.Get("listenKey"))
			}
			refreshes.Add(1)
			writer.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			if request.Form.Get("listenKey") != "v3-listen-key" {
				t.Errorf("delete listenKey=%q", request.Form.Get("listenKey"))
			}
			deletes.Add(1)
			writer.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	connector := NewWebSocketConnector(ConnectorOptions{
		URLs: URLs{Aster: VenueURLs{
			PerpetualWS: wsURL, PerpetualREST: server.URL,
		}},
		HTTPClient: server.Client(), Heartbeat: time.Hour,
		StaleAfter: time.Second, ListenKeyRefresh: 5 * time.Millisecond,
		Now: func() time.Time {
			return time.Unix(1_700_000_000, 123_000_000).UTC()
		},
	})
	connection, err := connector.Connect(
		context.Background(),
		Key{Account: "acct", Venue: VenueAster, Product: ProductPerpetual},
		Credentials{
			APIKey: walletAddress, Secret: walletKey, CredentialKind: "aster_api_wallet",
		},
	)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := connection.Read(); err != nil {
		t.Fatalf("read: %v", err)
	}
	waitForAtomic(t, &refreshes)
	if err := connection.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitForAtomic(t, &deletes)
	if creates.Load() != 1 {
		t.Fatalf("creates=%d", creates.Load())
	}
}

func waitForAtomic(t *testing.T, value *atomic.Int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for value.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if value.Load() == 0 {
		t.Fatal("timed out waiting for request")
	}
}

func TestControlPongExtendsReadDeadline(t *testing.T) {
	heartbeat := 40 * time.Millisecond
	staleAfter := 250 * time.Millisecond
	connector, pings, cleanup := newControlPingConnector(t, heartbeat, staleAfter, true)
	defer cleanup()

	connection, err := connector.Connect(
		context.Background(),
		Key{Account: "acct", Venue: VenueGate, Product: ProductSpot},
		Credentials{APIKey: "key", Secret: "secret"},
	)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer connection.Close()

	errCh := make(chan error, 1)
	go func() {
		_, readErr := connection.Read()
		errCh <- readErr
	}()
	waitForTextPings(t, pings, 3, 2*time.Second)
	select {
	case readErr := <-errCh:
		t.Fatalf("idle connection ended before multiple stale intervals: %v", readErr)
	case <-time.After(3*staleAfter + heartbeat):
	}
}

func TestMissingControlPongTriggersTimeout(t *testing.T) {
	heartbeat := 80 * time.Millisecond
	staleAfter := 400 * time.Millisecond
	connector, _, cleanup := newControlPingConnector(t, heartbeat, staleAfter, false)
	defer cleanup()

	connection, err := connector.Connect(
		context.Background(),
		Key{Account: "acct", Venue: VenueGate, Product: ProductSpot},
		Credentials{APIKey: "key", Secret: "secret"},
	)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer connection.Close()

	started := time.Now()
	_, readErr := connection.Read()
	elapsed := time.Since(started)
	if readErr == nil {
		t.Fatal("expected read timeout when control pong is missing")
	}
	if classifyDisconnect(readErr) != reasonReadTimeout {
		t.Fatalf("reason=%s err=%v", classifyDisconnect(readErr), readErr)
	}
	var netErr net.Error
	if !errors.As(readErr, &netErr) || !netErr.Timeout() {
		t.Fatalf("expected timeout error, got %v", readErr)
	}
	if elapsed < staleAfter/2 {
		t.Fatalf("timed out too quickly: %s", elapsed)
	}
	if elapsed > 4*staleAfter {
		t.Fatalf("timed out too slowly: %s", elapsed)
	}
}

func newControlPingConnector(
	t *testing.T,
	heartbeat, staleAfter time.Duration,
	replyPong bool,
) (*WebSocketConnector, *atomic.Int64, func()) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	var pings atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		connection.SetPingHandler(func(appData string) error {
			pings.Add(1)
			if !replyPong {
				return nil
			}
			err := connection.WriteControl(
				websocket.PongMessage, []byte(appData), time.Now().Add(time.Second),
			)
			if err == websocket.ErrCloseSent {
				return nil
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return nil
			}
			return err
		})
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	}))
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	connector := NewWebSocketConnector(ConnectorOptions{
		URLs: URLs{Gate: VenueURLs{
			SpotWS: wsURL, PerpetualWS: wsURL,
		}},
		HTTPClient: server.Client(), Heartbeat: heartbeat, StaleAfter: staleAfter,
	})
	return connector, &pings, server.Close
}

func TestBitgetAndOKXHeartbeatClearsExpiredWriteDeadline(t *testing.T) {
	t.Parallel()
	for _, venue := range []string{VenueBitget, VenueOKX} {
		t.Run(venue, func(t *testing.T) {
			t.Parallel()
			heartbeat := 40 * time.Millisecond
			connector, pings, closeServer := newTextHeartbeatConnector(t, venue, heartbeat, 5*time.Second, true)
			defer closeServer()
			connection := connectPrivateTextStream(t, connector, venue)
			defer connection.Close()
			stream := connection.(*websocketConnection)
			if err := stream.seedExpiredWriteDeadlineForTest(); err != nil {
				t.Fatalf("seed expired deadline: %v", err)
			}
			waitForTextPings(t, pings, 3, 2*time.Second)
		})
	}
}

func TestBitgetHeartbeatSurvivesTextPong(t *testing.T) {
	t.Parallel()
	heartbeat := 40 * time.Millisecond
	connector, pings, closeServer := newTextHeartbeatConnector(t, VenueBitget, heartbeat, time.Second, true)
	defer closeServer()
	connection := connectPrivateTextStream(t, connector, VenueBitget)
	defer connection.Close()
	waitForTextPings(t, pings, 3, 2*time.Second)
	done := make(chan error, 1)
	go func() {
		_, err := connection.Read()
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("connection died after text pongs: %v", err)
	case <-time.After(3 * heartbeat):
	}
}

func TestBitgetHeartbeatWithoutPongStillReadTimeout(t *testing.T) {
	t.Parallel()
	heartbeat := 40 * time.Millisecond
	staleAfter := 80 * time.Millisecond
	connector, _, closeServer := newTextHeartbeatConnector(t, VenueBitget, heartbeat, staleAfter, false)
	defer closeServer()
	connection := connectPrivateTextStream(t, connector, VenueBitget)
	defer connection.Close()
	started := time.Now()
	_, readErr := connection.Read()
	elapsed := time.Since(started)
	if readErr == nil {
		t.Fatal("expected read timeout when text pong is missing")
	}
	if classifyDisconnect(readErr) != reasonReadTimeout {
		t.Fatalf("reason=%s err=%v", classifyDisconnect(readErr), readErr)
	}
	if elapsed < staleAfter/2 {
		t.Fatalf("timed out too quickly: %s", elapsed)
	}
	if elapsed > 15*staleAfter {
		t.Fatalf("timed out too slowly: %s", elapsed)
	}
}

func TestHeartbeatWriteFailureKeepsHeartbeatReason(t *testing.T) {
	t.Parallel()
	heartbeat := 30 * time.Millisecond
	connector, _, closeServer := newTextHeartbeatConnector(t, VenueOKX, heartbeat, time.Second, true)
	defer closeServer()
	connection := connectPrivateTextStream(t, connector, VenueOKX)
	defer connection.Close()
	stream := connection.(*websocketConnection)
	_ = stream.closeUnderlyingForTest()
	time.Sleep(3 * heartbeat)
	_, readErr := connection.Read()
	if readErr == nil {
		t.Fatal("expected heartbeat write failure")
	}
	if classifyDisconnect(readErr) != reasonHeartbeatWriteFailed {
		t.Fatalf("reason=%s err=%v", classifyDisconnect(readErr), readErr)
	}
	var de *disconnectError
	if !errors.As(readErr, &de) || de.HeartbeatType != "text_ping" || de.Err == nil {
		t.Fatalf("disconnect error=%+v", de)
	}
}

func connectPrivateTextStream(t *testing.T, connector *WebSocketConnector, venue string) Connection {
	t.Helper()
	connection, err := connector.Connect(
		context.Background(),
		Key{Account: "acct", Venue: venue, Product: ProductPerpetual},
		Credentials{APIKey: "key", Secret: "secret", Passphrase: "pass"},
	)
	if err != nil {
		t.Fatalf("connect %s: %v", venue, err)
	}
	return connection
}

func waitForTextPings(t *testing.T, pings *atomic.Int64, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pings.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pings=%d, want >= %d", pings.Load(), want)
}

func newTextHeartbeatConnector(
	t *testing.T,
	venue string,
	heartbeat, staleAfter time.Duration,
	replyPong bool,
) (*WebSocketConnector, *atomic.Int64, func()) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	var pings atomic.Int64
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		authenticated := false
		for {
			_, payload, err := connection.ReadMessage()
			if err != nil {
				return
			}
			if strings.EqualFold(strings.TrimSpace(string(payload)), "ping") {
				pings.Add(1)
				if replyPong {
					if err := connection.WriteMessage(websocket.TextMessage, []byte("pong")); err != nil {
						return
					}
				}
				continue
			}
			if authenticated {
				continue
			}
			if err := connection.WriteJSON(map[string]any{"code": "0", "success": true}); err != nil {
				return
			}
			authenticated = true
			once.Do(func() {})
		}
	}))
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	urls := URLs{}
	switch venue {
	case VenueOKX:
		urls.OKX = VenueURLs{SpotWS: wsURL, PerpetualWS: wsURL}
	default:
		urls.Bitget = VenueURLs{SpotWS: wsURL, PerpetualWS: wsURL}
	}
	connector := NewWebSocketConnector(ConnectorOptions{
		URLs: urls, HTTPClient: server.Client(), Heartbeat: heartbeat, StaleAfter: staleAfter,
	})
	return connector, &pings, server.Close
}
