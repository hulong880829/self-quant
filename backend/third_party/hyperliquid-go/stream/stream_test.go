package stream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeWSServer is a minimal Hyperliquid-shaped WS endpoint backed by
// httptest. It echoes subscribe acks and exposes a Push channel for tests
// to inject server-originated messages.
type fakeWSServer struct {
	*httptest.Server
	Push     chan []byte // messages to push to the live connection
	Received chan []byte // last message the server saw from a client
}

func newFakeWS(t *testing.T) *fakeWSServer {
	t.Helper()
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	fws := &fakeWSServer{
		Push:     make(chan []byte, 16),
		Received: make(chan []byte, 16),
	}
	fws.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade failed: %v", err)
			return
		}
		// outbound serialises writes to the websocket: ack frames produced
		// by the reader and Push frames produced by the writer both flow
		// through this channel so the gorilla connection only sees one
		// writer at a time.
		outbound := make(chan []byte, 32)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					return
				}
				select {
				case fws.Received <- msg:
				default:
				}
				var cmd struct {
					Method string `json:"method"`
				}
				if err := json.Unmarshal(msg, &cmd); err == nil && cmd.Method == "subscribe" {
					outbound <- []byte(`{"channel":"subscriptionResponse","data":{}}`)
				}
			}
		}()
		for {
			select {
			case <-done:
				return
			case payload := <-fws.Push:
				if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
					return
				}
			case payload := <-outbound:
				if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
					return
				}
			}
		}
	}))
	return fws
}

// streamForURL builds a Client targeted at the test server's URL. The
// httptest server speaks plain ws://, so we cannot route through New
// (which rewrites the scheme to wss).
func streamForURL(t *testing.T, httpURL string) *Client {
	t.Helper()
	ws := "ws" + strings.TrimPrefix(httpURL, "http") + "/ws"
	return &Client{
		url:             ws,
		subscriptions:   make(map[subKey]map[int]*subscriptionCallback),
		pendingRequests: make(map[int]*pendingRequest),
		reconnectWait:   time.Millisecond,
		logger:          nopLogger{},
	}
}

func TestStream_ConnectIdempotentAndClose(t *testing.T) {
	srv := newFakeWS(t)
	defer srv.Close()

	s := streamForURL(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := s.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !s.connected.Load() {
		t.Errorf("connected flag not set")
	}
	if err := s.Connect(ctx); err != nil {
		t.Errorf("Connect should be idempotent: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if s.connected.Load() {
		t.Errorf("connected flag still set after Close")
	}
	// Close again — must not panic.
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	// Connect after Close should fail.
	if err := s.Connect(ctx); err == nil {
		t.Errorf("Connect after Close should fail")
	}
}

func TestStream_CloseWithoutConnect(t *testing.T) {
	srv := newFakeWS(t)
	defer srv.Close()
	s := streamForURL(t, srv.URL)
	if err := s.Close(); err != nil {
		t.Errorf("Close without Connect: %v", err)
	}
}

func TestStream_SubscribeDispatchAndUnsubscribe(t *testing.T) {
	srv := newFakeWS(t)
	defer srv.Close()

	s := streamForURL(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var got atomic.Int32
	done := make(chan struct{}, 1)
	sub, err := s.Subscribe(Trades("BTC"), func(m WSMessage) {
		if m.Channel != "trades" {
			t.Errorf("dispatched channel = %s", m.Channel)
		}
		got.Add(1)
		select {
		case done <- struct{}{}:
		default:
		}
	})
	if err != nil || sub == nil {
		t.Fatalf("Subscribe: sub=%v err=%v", sub, err)
	}

	// Push a matching trade message from the server.
	srv.Push <- []byte(`{"channel":"trades","data":{"coin":"BTC"}}`)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("callback not invoked")
	}
	if got.Load() != 1 {
		t.Errorf("dispatch count = %d", got.Load())
	}

	// Mismatched coin — must not invoke the callback.
	srv.Push <- []byte(`{"channel":"trades","data":{"coin":"ETH"}}`)
	time.Sleep(100 * time.Millisecond)
	if got.Load() != 1 {
		t.Errorf("ETH trade should not match BTC subscription: count=%d", got.Load())
	}

	if err := sub.Close(); err != nil {
		t.Errorf("sub.Close: %v", err)
	}
	// Idempotent: a second Close is a no-op.
	if err := sub.Close(); err != nil {
		t.Errorf("sub.Close (second): %v", err)
	}
}

func TestStream_SubscribeNilCallback(t *testing.T) {
	srv := newFakeWS(t)
	defer srv.Close()
	s := streamForURL(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if _, err := s.Subscribe(AllMids(), nil); err == nil {
		t.Errorf("nil callback should error")
	}
}

func TestStream_MultipleSubscribersSameKey(t *testing.T) {
	srv := newFakeWS(t)
	defer srv.Close()

	s := streamForURL(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var wg sync.WaitGroup
	wg.Add(2)
	id1, err := s.Subscribe(AllMids(), func(WSMessage) { wg.Done() })
	if err != nil {
		t.Fatalf("Subscribe 1: %v", err)
	}
	id2, err := s.Subscribe(AllMids(), func(WSMessage) { wg.Done() })
	if err != nil {
		t.Fatalf("Subscribe 2: %v", err)
	}
	if id1 == id2 {
		t.Errorf("IDs must be unique")
	}

	srv.Push <- []byte(`{"channel":"allMids","data":{}}`)
	doneAll := make(chan struct{})
	go func() { wg.Wait(); close(doneAll) }()
	select {
	case <-doneAll:
	case <-time.After(time.Second):
		t.Fatalf("both callbacks should fire")
	}
}

func TestStream_PostNotConnected(t *testing.T) {
	srv := newFakeWS(t)
	defer srv.Close()
	s := streamForURL(t, srv.URL)
	if _, err := s.Post("info", map[string]any{}, 10*time.Millisecond); err == nil {
		t.Errorf("Post should require Connect first")
	}
}

func TestStream_SubscribeNotConnected(t *testing.T) {
	srv := newFakeWS(t)
	defer srv.Close()
	s := streamForURL(t, srv.URL)
	if _, err := s.Subscribe(AllMids(), func(WSMessage) {}); err == nil {
		t.Errorf("Subscribe without Connect should fail")
	}
}

func TestNewStreamInvalidURL(t *testing.T) {
	if _, err := New("://nope"); err == nil {
		t.Errorf("invalid URL should error")
	}
}

func TestStream_SetLoggerNil(t *testing.T) {
	srv := newFakeWS(t)
	defer srv.Close()
	s := streamForURL(t, srv.URL)
	s.SetLogger(nil)
	if _, ok := s.logger.(nopLogger); !ok {
		t.Errorf("nil logger must revert to nopLogger")
	}
}
