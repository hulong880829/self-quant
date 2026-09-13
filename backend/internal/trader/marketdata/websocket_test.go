package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebSocketConnectorPoolsAndUnsubscribesBySymbol(t *testing.T) {
	t.Parallel()

	var dials atomic.Int32
	accepted := make(chan *websocket.Conn, 1)
	requests := make(chan map[string]any, 8)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		dials.Add(1)
		accepted <- connection
		for {
			var message map[string]any
			if err := connection.ReadJSON(&message); err != nil {
				return
			}
			requests <- message
		}
	}))
	defer server.Close()

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	connector := NewWebSocketConnector()
	connector.endpoint = func(Key) (string, error) { return endpoint, nil }

	btcKey := Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "BTCUSDT"}
	ethKey := Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "ETHUSDT"}
	btc, err := connector.Connect(context.Background(), btcKey)
	if err != nil {
		t.Fatal(err)
	}
	defer btc.Close()
	physical := <-accepted

	eth, err := connector.Connect(context.Background(), ethKey)
	if err != nil {
		t.Fatal(err)
	}
	defer eth.Close()

	first := receiveWebSocketRequest(t, requests)
	second := receiveWebSocketRequest(t, requests)
	if first["method"] != "SUBSCRIBE" || second["method"] != "SUBSCRIBE" {
		t.Fatalf("initial methods = (%v, %v), want SUBSCRIBE", first["method"], second["method"])
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("physical dial count = %d, want 1", got)
	}

	if err := btc.Close(); err != nil {
		t.Fatal(err)
	}
	unsubscribe := receiveWebSocketRequest(t, requests)
	if unsubscribe["method"] != "UNSUBSCRIBE" {
		t.Fatalf("method = %v, want UNSUBSCRIBE", unsubscribe["method"])
	}
	params, ok := unsubscribe["params"].([]any)
	if !ok || len(params) != 1 || params[0] != "btcusdt@depth10@100ms" {
		t.Fatalf("unsubscribe params = %#v", unsubscribe["params"])
	}

	fixture := []byte(`{"stream":"ethusdt@depth10@100ms",` +
		`"data":{"bids":[["1","1"]],"asks":[["2","1"]]}}`)
	if err := physical.WriteMessage(websocket.TextMessage, fixture); err != nil {
		t.Fatal(err)
	}
	payload, err := readLogicalWithTimeout(t, eth)
	if err != nil {
		t.Fatalf("ETH logical read after BTC unsubscribe: %v", err)
	}
	if string(payload) != string(fixture) {
		t.Fatalf("ETH payload = %s, want %s", payload, fixture)
	}

	if err := physical.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readLogicalWithTimeout(t, eth); err == nil {
		t.Fatal("logical read after physical disconnect returned no error")
	}
}

func TestLogicalConnectionDeliveryNeverWaitsForConsumer(t *testing.T) {
	t.Parallel()
	connection := &logicalConnection{messages: make(chan websocketRead, 1)}
	delivered := make(chan struct{})
	go func() {
		for index := 0; index < 10_000; index++ {
			connection.deliver([]byte(strconv.Itoa(index)))
		}
		close(delivered)
	}()
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("logical delivery blocked on stalled consumer")
	}
	item := <-connection.messages
	if string(item.payload) != "9999" {
		t.Fatalf("buffered payload = %q, want latest", item.payload)
	}
}

func TestLighterLogicalDeliveryPreservesSequenceAndFailsOnOverflow(t *testing.T) {
	t.Parallel()
	connection := &logicalConnection{
		key:      Key{Venue: VenueLighter, Product: ProductPerpetual, Symbol: "BTC"},
		messages: make(chan websocketRead, 2),
	}
	if !connection.deliverSequenced([]byte("snapshot")) ||
		!connection.deliverSequenced([]byte("delta")) {
		t.Fatal("Lighter sequence frames were not queued")
	}
	if connection.deliverSequenced([]byte("overflow")) {
		t.Fatal("Lighter sequence overflow was silently accepted")
	}
	for _, want := range []string{"snapshot", "delta"} {
		item := <-connection.messages
		if string(item.payload) != want {
			t.Fatalf("queued frame = %q, want %q", item.payload, want)
		}
	}
}

func TestWebSocketConnectorReadDeadlineClosesSilentPool(t *testing.T) {
	t.Parallel()
	subscribed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		var subscription map[string]any
		if err := connection.ReadJSON(&subscription); err != nil {
			return
		}
		close(subscribed)
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	connector := NewWebSocketConnector(ConnectorOptions{
		Heartbeat: time.Hour,
		ReadWait:  40 * time.Millisecond,
		WriteWait: time.Second,
	})
	connector.endpoint = func(Key) (string, error) {
		return "ws" + strings.TrimPrefix(server.URL, "http"), nil
	}
	connection, err := connector.Connect(
		context.Background(),
		Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "BTCUSDT"},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	<-subscribed
	if _, err := readLogicalWithTimeout(t, connection); err == nil {
		t.Fatal("silent websocket did not fail its read deadline")
	}
}

func TestBinanceServerPingRenewsReadDeadline(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		var subscription map[string]any
		if err := connection.ReadJSON(&subscription); err != nil {
			return
		}
		for index := 0; index < 4; index++ {
			time.Sleep(30 * time.Millisecond)
			if err := connection.WriteControl(
				websocket.PingMessage, []byte("keepalive"), time.Now().Add(time.Second),
			); err != nil {
				return
			}
		}
		fixture := []byte(`{"stream":"btcusdt@depth10@100ms",` +
			`"data":{"bids":[["1","1"]],"asks":[["2","1"]]}}`)
		if err := connection.WriteMessage(websocket.TextMessage, fixture); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()

	connector := NewWebSocketConnector(ConnectorOptions{
		Heartbeat: time.Hour,
		ReadWait:  50 * time.Millisecond,
		WriteWait: time.Second,
	})
	connector.endpoint = func(Key) (string, error) {
		return "ws" + strings.TrimPrefix(server.URL, "http"), nil
	}
	connection, err := connector.Connect(
		context.Background(),
		Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "BTCUSDT"},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	payload, err := readLogicalWithTimeout(t, connection)
	if err != nil {
		t.Fatalf("logical read after server pings: %v", err)
	}
	if !strings.Contains(string(payload), `"stream":"btcusdt`) {
		t.Fatalf("unexpected payload: %s", payload)
	}
}

func TestSubscriptionRejectionFailsOnlyOneSymbol(t *testing.T) {
	t.Parallel()
	var dials atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		dials.Add(1)
		requests := make([]map[string]any, 0, 2)
		for len(requests) < 2 {
			var item map[string]any
			if err := connection.ReadJSON(&item); err != nil {
				return
			}
			requests = append(requests, item)
		}
		for _, item := range requests {
			params := item["params"].([]any)
			if strings.Contains(params[0].(string), "bad") {
				if err := connection.WriteJSON(map[string]any{
					"id": item["id"],
					"error": map[string]any{
						"code": -1121, "msg": "Invalid symbol",
					},
				}); err != nil {
					return
				}
			} else if err := connection.WriteJSON(map[string]any{
				"id": item["id"], "result": nil,
			}); err != nil {
				return
			}
		}
		fixture := []byte(`{"stream":"ethusdt@depth10@100ms",` +
			`"data":{"bids":[["1","1"]],"asks":[["2","1"]]}}`)
		if err := connection.WriteMessage(websocket.TextMessage, fixture); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()

	connector := NewWebSocketConnector(ConnectorOptions{
		Heartbeat: time.Hour, ReadWait: time.Second,
		WriteWait: time.Second, AckWait: time.Second,
	})
	connector.endpoint = func(Key) (string, error) {
		return "ws" + strings.TrimPrefix(server.URL, "http"), nil
	}
	bad, err := connector.Connect(
		context.Background(),
		Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "BADUSDT"},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	good, err := connector.Connect(
		context.Background(),
		Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "ETHUSDT"},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer good.Close()

	if _, err := readLogicalWithTimeout(t, bad); !errors.Is(err, ErrSubscriptionRejected) {
		t.Fatalf("bad symbol error = %v", err)
	}
	payload, err := readLogicalWithTimeout(t, good)
	if err != nil || !strings.Contains(string(payload), `"stream":"ethusdt`) {
		t.Fatalf("good symbol read = (%s, %v)", payload, err)
	}
	if dials.Load() != 1 {
		t.Fatalf("physical dials = %d, want 1", dials.Load())
	}
}

func TestSubscriptionAckTimeoutFailsOnlyPendingSymbol(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		requests := make([]map[string]any, 0, 2)
		for len(requests) < 2 {
			var item map[string]any
			if err := connection.ReadJSON(&item); err != nil {
				return
			}
			requests = append(requests, item)
		}
		for _, item := range requests {
			params := item["params"].([]any)
			if strings.Contains(params[0].(string), "eth") {
				if err := connection.WriteJSON(map[string]any{
					"id": item["id"], "result": nil,
				}); err != nil {
					return
				}
			}
		}
		time.Sleep(80 * time.Millisecond)
		fixture := []byte(`{"stream":"ethusdt@depth10@100ms",` +
			`"data":{"bids":[["1","1"]],"asks":[["2","1"]]}}`)
		if err := connection.WriteMessage(websocket.TextMessage, fixture); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()

	connector := NewWebSocketConnector(ConnectorOptions{
		Heartbeat: time.Hour, ReadWait: time.Second,
		WriteWait: time.Second, AckWait: 30 * time.Millisecond,
	})
	connector.endpoint = func(Key) (string, error) {
		return "ws" + strings.TrimPrefix(server.URL, "http"), nil
	}
	timedOut, err := connector.Connect(
		context.Background(),
		Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "NOACKUSDT"},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer timedOut.Close()
	good, err := connector.Connect(
		context.Background(),
		Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "ETHUSDT"},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer good.Close()

	if _, err := readLogicalWithTimeout(t, timedOut); !errors.Is(
		err, ErrSubscriptionAckTimeout,
	) {
		t.Fatalf("pending symbol error = %v", err)
	}
	payload, err := readLogicalWithTimeout(t, good)
	if err != nil || !strings.Contains(string(payload), `"stream":"ethusdt`) {
		t.Fatalf("acknowledged symbol read = (%s, %v)", payload, err)
	}
}

func TestWebSocketProtocolSupportsEveryVenue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key     Key
		fixture string
	}{
		{
			key:     Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "BTCUSDT"},
			fixture: `{"stream":"btcusdt@depth10@100ms","data":{"bids":[["1"]],"asks":[["2"]]}}`,
		},
		{
			key:     Key{Venue: VenueOKX, Product: ProductPerpetual, Symbol: "BTC-USDT-SWAP"},
			fixture: `{"arg":{"channel":"books5","instId":"BTC-USDT-SWAP"},"data":[]}`,
		},
		{
			key:     Key{Venue: VenueBybit, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			fixture: `{"topic":"orderbook.1.BTCUSDT","data":{"s":"BTCUSDT"}}`,
		},
		{
			key:     Key{Venue: VenueBitget, Product: ProductSpot, Symbol: "BTCUSDT"},
			fixture: `{"arg":{"channel":"books15","instId":"BTCUSDT"}}`,
		},
		{
			key: Key{Venue: VenueGate, Product: ProductPerpetual, Symbol: "BTC_USDT"},
			fixture: `{"channel":"futures.book_ticker","event":"update",` +
				`"result":{"s":"BTC_USDT"}}`,
		},
		{
			key: Key{Venue: VenueHyperliquid, Product: ProductPerpetual, Symbol: "BTC"},
			fixture: `{"channel":"bbo","data":{"coin":"BTC","time":1724328000001,` +
				`"bbo":[{"px":"1","sz":"1"},{"px":"2","sz":"1"}]}}`,
		},
		{
			key: Key{Venue: VenueAster, Product: ProductPerpetual, Symbol: "BTCUSDT"},
			fixture: `{"e":"bookTicker","s":"BTCUSDT","b":"1","B":"1",` +
				`"a":"2","A":"1","E":1724328000001}`,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.key.Venue, func(t *testing.T) {
			t.Parallel()
			if _, err := websocketEndpoint(test.key); err != nil {
				t.Fatalf("endpoint: %v", err)
			}
			for _, subscribe := range []bool{true, false} {
				if request := subscriptionRequest(test.key, subscribe); request == nil {
					t.Fatalf("subscription request is nil (subscribe=%v)", subscribe)
				}
			}
			symbols, err := messageSymbols(test.key.Venue, []byte(test.fixture))
			if err != nil {
				t.Fatalf("demux: %v", err)
			}
			if len(symbols) != 1 || symbols[0] != test.key.Symbol {
				t.Fatalf("demux symbols = %v, want %q", symbols, test.key.Symbol)
			}
		})
	}
}

func TestLighterConnectorResolvesCachesAndRoutesMarketID(t *testing.T) {
	t.Parallel()
	var metadataCalls atomic.Int32
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/orderBookDetails" {
			metadataCalls.Add(1)
			if request.URL.Query().Get("filter") != "perp" {
				t.Errorf("metadata filter = %q", request.URL.Query().Get("filter"))
			}
			_, _ = writer.Write([]byte(`{"code":200,"order_book_details":[` +
				`{"symbol":"BTC","market_id":1,"market_type":"perp","status":"active"},` +
				`{"symbol":"ETH","market_id":2,"market_type":"perp","status":"active"}]}`))
			return
		}
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		accepted <- connection
	}))
	defer server.Close()

	connector := NewWebSocketConnector(ConnectorOptions{
		HTTPClient: server.Client(), LighterRESTURL: server.URL,
		Heartbeat: time.Hour, ReadWait: time.Second, AckWait: 30 * time.Millisecond,
	})
	connector.endpoint = func(Key) (string, error) {
		return "ws" + strings.TrimPrefix(server.URL, "http") + "/stream", nil
	}
	btcKey := Key{Venue: VenueLighter, Product: ProductPerpetual, Symbol: "BTC"}
	ethKey := Key{Venue: VenueLighter, Product: ProductPerpetual, Symbol: "ETHUSDC"}
	btc, err := connector.Connect(context.Background(), btcKey)
	if err != nil {
		t.Fatal(err)
	}
	defer btc.Close()
	physical := <-accepted
	var request map[string]any
	if err := physical.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	if request["type"] != "subscribe" || request["channel"] != "order_book/1" {
		t.Fatalf("BTC subscription = %#v", request)
	}

	eth, err := connector.Connect(context.Background(), ethKey)
	if err != nil {
		t.Fatal(err)
	}
	defer eth.Close()
	if err := physical.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	if request["type"] != "subscribe" || request["channel"] != "order_book/2" {
		t.Fatalf("ETH subscription = %#v", request)
	}
	if got := metadataCalls.Load(); got != 1 {
		t.Fatalf("metadata calls = %d, want cached single request", got)
	}

	btcSnapshot := []byte(`{"type":"subscribed/order_book","channel":"order_book:1",` +
		`"timestamp":1724328000001,"order_book":{"nonce":100,"begin_nonce":0,` +
		`"bids":[{"price":"100","size":"1"}],"asks":[{"price":"101","size":"1"}]}}`)
	if err := physical.WriteMessage(websocket.TextMessage, btcSnapshot); err != nil {
		t.Fatal(err)
	}
	payload, err := readLogicalWithTimeout(t, btc)
	if err != nil || string(payload) != string(btcSnapshot) {
		t.Fatalf("BTC routed payload = (%s, %v)", payload, err)
	}

	ethSnapshot := []byte(`{"type":"subscribed/order_book","channel":"order_book:2",` +
		`"timestamp":1724328000002,"order_book":{"nonce":200,"begin_nonce":0,` +
		`"bids":[{"price":"200","size":"1"}],"asks":[{"price":"201","size":"1"}]}}`)
	if err := physical.WriteMessage(websocket.TextMessage, ethSnapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := readLogicalWithTimeout(t, eth); err != nil {
		t.Fatalf("ETH routed payload: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	btcDelta := []byte(`{"type":"update/order_book","channel":"order_book:1",` +
		`"timestamp":1724328000003,"order_book":{"nonce":101,"begin_nonce":100,` +
		`"bids":[{"price":"100.5","size":"1"}],"asks":[]}}`)
	if err := physical.WriteMessage(websocket.TextMessage, btcDelta); err != nil {
		t.Fatal(err)
	}
	if _, err := readLogicalWithTimeout(t, btc); err != nil {
		t.Fatalf("BTC read after implicit snapshot ACK: %v", err)
	}

	if err := btc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := physical.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	if request["type"] != "unsubscribe" || request["channel"] != "order_book/1" {
		t.Fatalf("BTC unsubscribe = %#v", request)
	}
}

func TestDEXHeartbeatProtocols(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		venue     string
		symbol    string
		heartbeat map[string]string
	}{
		{
			venue: VenueHyperliquid, symbol: "BTC",
			heartbeat: map[string]string{"method": "ping"},
		},
		{
			venue: VenueLighter, symbol: "BTC",
			heartbeat: map[string]string{"type": "ping"},
		},
	} {
		test := test
		t.Run(test.venue, func(t *testing.T) {
			t.Parallel()
			messages := make(chan map[string]any, 4)
			server := httptest.NewServer(http.HandlerFunc(func(
				writer http.ResponseWriter,
				request *http.Request,
			) {
				if request.URL.Path == "/api/v1/orderBookDetails" {
					_, _ = writer.Write([]byte(
						`{"code":200,"order_book_details":[{"symbol":"BTC",` +
							`"market_id":1,"market_type":"perp","status":"active"}]}`,
					))
					return
				}
				connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
				if err != nil {
					return
				}
				defer connection.Close()
				for {
					var message map[string]any
					if err := connection.ReadJSON(&message); err != nil {
						return
					}
					messages <- message
				}
			}))
			defer server.Close()

			connector := NewWebSocketConnector(ConnectorOptions{
				HTTPClient: server.Client(), LighterRESTURL: server.URL,
				Heartbeat: 10 * time.Millisecond, ReadWait: time.Second,
				AckWait: time.Second,
			})
			connector.endpoint = func(Key) (string, error) {
				return "ws" + strings.TrimPrefix(server.URL, "http") + "/stream", nil
			}
			connection, err := connector.Connect(context.Background(), Key{
				Venue: test.venue, Product: ProductPerpetual, Symbol: test.symbol,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			_ = receiveWebSocketRequest(t, messages)
			heartbeat := receiveWebSocketRequest(t, messages)
			for field, want := range test.heartbeat {
				if heartbeat[field] != want {
					t.Fatalf("heartbeat = %#v, want %s=%s", heartbeat, field, want)
				}
			}
		})
	}
}

func TestAsterHeartbeatUsesRFC6455Ping(t *testing.T) {
	t.Parallel()
	pings := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		connection.SetPingHandler(func(message string) error {
			select {
			case pings <- struct{}{}:
			default:
			}
			return connection.WriteControl(
				websocket.PongMessage, []byte(message), time.Now().Add(time.Second),
			)
		})
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	connector := NewWebSocketConnector(ConnectorOptions{
		Heartbeat: 10 * time.Millisecond, ReadWait: time.Second, AckWait: time.Second,
	})
	connector.endpoint = func(Key) (string, error) {
		return "ws" + strings.TrimPrefix(server.URL, "http"), nil
	}
	connection, err := connector.Connect(context.Background(), Key{
		Venue: VenueAster, Product: ProductPerpetual, Symbol: "BTCUSDT",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	select {
	case <-pings:
	case <-time.After(time.Second):
		t.Fatal("Aster client did not send RFC6455 ping")
	}
}

func TestAsterConnectionRotatesProactively(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	connector := NewWebSocketConnector(ConnectorOptions{
		Heartbeat: time.Hour, ReadWait: time.Second, AckWait: time.Second,
		AsterConnectionMaxAge: 25 * time.Millisecond,
	})
	connector.endpoint = func(Key) (string, error) {
		return "ws" + strings.TrimPrefix(server.URL, "http"), nil
	}
	connection, err := connector.Connect(context.Background(), Key{
		Venue: VenueAster, Product: ProductPerpetual, Symbol: "BTCUSDT",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := readLogicalWithTimeout(t, connection); !errors.Is(err, ErrConnectionRotation) {
		t.Fatalf("Aster rotation error = %v, want ErrConnectionRotation", err)
	}
}

func TestGateSubscriptionRejectionIsAControlError(t *testing.T) {
	t.Parallel()
	fixture := []byte(`{"id":17,"channel":"futures.book_ticker","event":"subscribe",` +
		`"error":{"code":2,"message":"Unknown channel futures.book_ticker"},` +
		`"result":{"status":"fail"}}`)
	control, handled, err := parseSubscriptionControl(VenueGate, fixture)
	if err != nil || !handled || control.err == nil ||
		!strings.Contains(control.err.Error(), "Unknown channel") {
		t.Fatalf("Gate rejection control = (%#v, %t, %v)", control, handled, err)
	}
}

func TestSnapshotOrderBookProtocols(t *testing.T) {
	t.Parallel()

	binance := Key{Venue: VenueBinance, Product: ProductSpot, Symbol: "BTCUSDT"}
	if endpoint, err := websocketEndpoint(binance); err != nil ||
		endpoint != "wss://stream.binance.com:9443/stream" {
		t.Fatalf("Binance endpoint = %q, %v", endpoint, err)
	}
	binanceRequest := subscriptionRequest(binance, true).(map[string]any)
	if got, want := binanceRequest["params"].([]string),
		[]string{"btcusdt@depth10@100ms"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("Binance params = %#v, want %#v", got, want)
	}

	okx := Key{Venue: VenueOKX, Product: ProductPerpetual, Symbol: "BTC-USDT-SWAP"}
	okxRequest, err := json.Marshal(subscriptionRequest(okx, true))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(okxRequest),
		`{"args":[{"channel":"books5","instId":"BTC-USDT-SWAP"}],"op":"subscribe"}`; got != want {
		t.Fatalf("OKX request = %s, want %s", got, want)
	}

	bybit := Key{Venue: VenueBybit, Product: ProductPerpetual, Symbol: "COTIUSDT"}
	if endpoint, err := websocketEndpoint(bybit); err != nil ||
		endpoint != "wss://stream.bybit.com/v5/public/linear" {
		t.Fatalf("Bybit endpoint = %q, %v", endpoint, err)
	}
	bybitValue, _ := subscriptionRequestWithID(bybit, true, 7)
	bybitRequest, err := json.Marshal(bybitValue)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(bybitRequest),
		`{"args":["orderbook.1.COTIUSDT"],"op":"subscribe","req_id":"7"}`; got != want {
		t.Fatalf("Bybit request = %s, want %s", got, want)
	}

	bitget := Key{Venue: VenueBitget, Product: ProductPerpetual, Symbol: "COTIUSDT"}
	if endpoint, err := websocketEndpoint(bitget); err != nil ||
		endpoint != "wss://ws.bitget.com/v2/ws/public" {
		t.Fatalf("Bitget endpoint = %q, %v", endpoint, err)
	}
	bitgetRequest, err := json.Marshal(subscriptionRequest(bitget, true))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(bitgetRequest),
		`{"args":[{"channel":"books15","instId":"COTIUSDT","instType":"USDT-FUTURES"}],"op":"subscribe"}`; got != want {
		t.Fatalf("Bitget request = %s, want %s", got, want)
	}

	for _, test := range []struct {
		product string
		channel string
		payload []string
	}{
		{ProductSpot, "spot.order_book", []string{"BTC_USDT", "10", "100ms"}},
		{ProductPerpetual, "futures.book_ticker", []string{"BTC_USDT"}},
	} {
		key := Key{Venue: VenueGate, Product: test.product, Symbol: "BTC_USDT"}
		if endpoint, err := websocketEndpoint(key); err != nil ||
			(test.product == ProductPerpetual &&
				endpoint != "wss://fx-ws.gateio.ws/v4/ws/usdt") {
			t.Fatalf("Gate %s endpoint = %q, %v", test.product, endpoint, err)
		}
		request := subscriptionRequest(
			key, true,
		).(map[string]any)
		if request["channel"] != test.channel || request["event"] != "subscribe" {
			t.Fatalf("Gate %s request = %#v", test.product, request)
		}
		payload, ok := request["payload"].([]string)
		if !ok || len(payload) != len(test.payload) {
			t.Fatalf("Gate %s payload = %#v", test.product, request["payload"])
		}
		for index := range payload {
			if payload[index] != test.payload[index] {
				t.Fatalf("Gate %s payload = %#v, want %#v", test.product, payload, test.payload)
			}
		}
	}

	hyperliquid := Key{
		Venue: VenueHyperliquid, Product: ProductPerpetual, Symbol: "BTC",
	}
	if endpoint, err := websocketEndpoint(hyperliquid); err != nil ||
		endpoint != "wss://api.hyperliquid.xyz/ws" {
		t.Fatalf("Hyperliquid endpoint = %q, %v", endpoint, err)
	}
	hyperliquidRequest := subscriptionRequest(hyperliquid, true).(map[string]any)
	if hyperliquidRequest["method"] != "subscribe" {
		t.Fatalf("Hyperliquid request = %#v", hyperliquidRequest)
	}
	hyperliquidSubscription := hyperliquidRequest["subscription"].(map[string]string)
	if hyperliquidSubscription["type"] != "bbo" ||
		hyperliquidSubscription["coin"] != "BTC" {
		t.Fatalf("Hyperliquid subscription = %#v", hyperliquidSubscription)
	}

	aster := Key{Venue: VenueAster, Product: ProductPerpetual, Symbol: "BTCUSDT"}
	if endpoint, err := websocketEndpoint(aster); err != nil ||
		endpoint != "wss://fstream.asterdex.com/ws" {
		t.Fatalf("Aster endpoint = %q, %v", endpoint, err)
	}
	asterRequest := subscriptionRequest(aster, true).(map[string]any)
	if asterRequest["method"] != "SUBSCRIBE" ||
		asterRequest["params"].([]string)[0] != "btcusdt@bookTicker" {
		t.Fatalf("Aster request = %#v", asterRequest)
	}

	lighter := Key{Venue: VenueLighter, Product: ProductPerpetual, Symbol: "BTC"}
	if endpoint, err := websocketEndpoint(lighter); err != nil ||
		endpoint != "wss://mainnet.zklighter.elliot.ai/stream" {
		t.Fatalf("Lighter endpoint = %q, %v", endpoint, err)
	}
}

func receiveWebSocketRequest(t *testing.T, requests <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for websocket request")
		return nil
	}
}

func readLogicalWithTimeout(t *testing.T, connection Connection) ([]byte, error) {
	t.Helper()
	type result struct {
		payload []byte
		err     error
	}
	results := make(chan result, 1)
	go func() {
		payload, err := connection.Read()
		results <- result{payload: payload, err: err}
	}()
	select {
	case result := <-results:
		return result.payload, result.err
	case <-time.After(time.Second):
		return nil, errors.New("timed out waiting for logical websocket read")
	}
}

func TestSubscriptionRequestsAreJSONEncodable(t *testing.T) {
	t.Parallel()
	for _, venue := range []string{VenueBinance, VenueOKX, VenueBybit, VenueBitget, VenueGate} {
		key := Key{Venue: venue, Product: ProductSpot, Symbol: "BTCUSDT"}
		if _, err := json.Marshal(subscriptionRequest(key, true)); err != nil {
			t.Fatalf("%s subscribe request: %v", venue, err)
		}
	}
}
