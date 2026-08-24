package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
			fixture: `{"channel":"futures.order_book","event":"all",` +
				`"result":{"contract":"BTC_USDT"}}`,
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
	bybitRequest, err := json.Marshal(subscriptionRequest(bybit, true))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(bybitRequest),
		`{"args":["orderbook.1.COTIUSDT"],"op":"subscribe"}`; got != want {
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
		product  string
		channel  string
		interval string
	}{
		{ProductSpot, "spot.order_book", "100ms"},
		{ProductPerpetual, "futures.order_book", "0"},
	} {
		request := subscriptionRequest(
			Key{Venue: VenueGate, Product: test.product, Symbol: "BTC_USDT"}, true,
		).(map[string]any)
		if request["channel"] != test.channel || request["event"] != "subscribe" {
			t.Fatalf("Gate %s request = %#v", test.product, request)
		}
		payload, ok := request["payload"].([]string)
		if !ok || len(payload) != 3 ||
			payload[0] != "BTC_USDT" || payload[1] != "10" || payload[2] != test.interval {
			t.Fatalf("Gate %s payload = %#v", test.product, request["payload"])
		}
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
