package orderstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
