package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Simon-Busch/hyperliquid-go/stream"
	"github.com/gorilla/websocket"
)

func TestHyperliquidPlaceOrderUsesWSSNotRESTExchange(t *testing.T) {
	var exchangeCalls atomic.Int32
	payload := `{"status":"ok","response":{"type":"order","data":{"statuses":[{"filled":{"oid":7,"avgPx":"100.5","totalSz":"1"}}]}}}`
	adapter := newHyperliquidTestAdapter(t, payload, &exchangeCalls, nil)
	result, err := adapter.PlaceOrder(
		context.Background(),
		hyperliquidTestCredentials(),
		OrderRequest{
			Instrument: Instrument{
				ContractType: "perpetual", ExchangeSymbol: "BTC",
				QuantityStep: "0.001",
			},
			ClientOrderID: "client-wss-1", Side: "buy", OrderType: "limit",
			TimeInForce: "IOC", Quantity: "1", Price: "100",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "filled" || result.VenueOrderID != "7" {
		t.Fatalf("result=%+v", result)
	}
	if exchangeCalls.Load() != 0 {
		t.Fatalf("REST /exchange called %d times", exchangeCalls.Load())
	}
}

func TestHyperliquidPlaceOrderRestingStaysOpen(t *testing.T) {
	payload := `{"status":"ok","response":{"type":"order","data":{"statuses":[{"resting":{"oid":42,"cid":"x","status":"open"}}]}}}`
	adapter := newHyperliquidTestAdapter(t, payload, nil, nil)
	result, err := adapter.PlaceOrder(
		context.Background(),
		hyperliquidTestCredentials(),
		OrderRequest{
			Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTC"},
			ClientOrderID: "client-resting", Side: "buy", OrderType: "limit",
			TimeInForce: "GTC", Quantity: "1", Price: "100",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "open" || result.VenueOrderID != "42" {
		t.Fatalf("result=%+v", result)
	}
}

func TestHyperliquidPlaceOrderIOCNoMatchIsCanceled(t *testing.T) {
	payload := `{"status":"ok","response":{"type":"order","data":{"statuses":[{"error":"Order could not immediately match against any resting orders."}]}}}`
	adapter := newHyperliquidTestAdapter(t, payload, nil, nil)
	result, err := adapter.PlaceOrder(
		context.Background(),
		hyperliquidTestCredentials(),
		OrderRequest{
			Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTC"},
			ClientOrderID: "client-ioc", Side: "sell", OrderType: "limit",
			TimeInForce: "IOC", Quantity: "1", Price: "100",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "canceled" || result.FilledQuantity != "0" {
		t.Fatalf("result=%+v", result)
	}
}

func TestHyperliquidPlaceOrderRejectedStaysRejected(t *testing.T) {
	payload := `{"status":"ok","response":{"type":"order","data":{"statuses":[{"error":"Insufficient margin"}]}}}`
	adapter := newHyperliquidTestAdapter(t, payload, nil, nil)
	_, err := adapter.PlaceOrder(
		context.Background(),
		hyperliquidTestCredentials(),
		OrderRequest{
			Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTC"},
			ClientOrderID: "client-rej", Side: "buy", OrderType: "limit",
			TimeInForce: "GTC", Quantity: "1", Price: "100",
		},
	)
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err=%v, want ErrRejected", err)
	}
}

func TestHyperliquidPlaceOrderTimeoutIsUncertainWithoutRESTRetry(t *testing.T) {
	var exchangeCalls atomic.Int32
	adapter := newHyperliquidTestAdapter(t, "", &exchangeCalls, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := adapter.PlaceOrder(
		ctx,
		hyperliquidTestCredentials(),
		OrderRequest{
			Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTC"},
			ClientOrderID: "client-timeout", Side: "buy", OrderType: "limit",
			TimeInForce: "IOC", Quantity: "1", Price: "100",
		},
	)
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("err=%v, want ErrUncertain", err)
	}
	if exchangeCalls.Load() != 0 {
		t.Fatalf("timeout retried REST /exchange %d times", exchangeCalls.Load())
	}
}

func TestHyperliquidWarmOrderTransportReusesConnection(t *testing.T) {
	var upgrades atomic.Int32
	adapter := newHyperliquidTestAdapter(t, `{"status":"ok","response":{"type":"order","data":{"statuses":[{"resting":{"oid":1,"cid":"x","status":"open"}}]}}}`, nil, &upgrades)
	instrument := Instrument{ContractType: "perpetual", ExchangeSymbol: "BTC"}
	ctx := context.Background()
	if err := adapter.WarmOrderTransport(ctx, hyperliquidTestCredentials(), instrument); err != nil {
		t.Fatal(err)
	}
	if err := adapter.WarmOrderTransport(ctx, hyperliquidTestCredentials(), instrument); err != nil {
		t.Fatal(err)
	}
	if upgrades.Load() != 1 {
		t.Fatalf("upgrades=%d, want 1 reused connection", upgrades.Load())
	}
}

func TestHyperliquidClientsIsolateVaultAndReuseIdentity(t *testing.T) {
	adapter := newHyperliquidTestAdapter(t, `{"status":"ok","response":{"type":"order","data":{"statuses":[]}}}`, nil, nil)
	base := hyperliquidTestCredentials()
	first, release, err := adapter.client(base, "BTC")
	if err != nil {
		t.Fatal(err)
	}
	release()
	again, release, err := adapter.client(base, "BTC")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if first != again {
		t.Fatal("same credentials did not reuse client")
	}
	vaulted := base
	vaulted.VaultAddress = "0x1111111111111111111111111111111111111111"
	other, release, err := adapter.client(vaulted, "BTC")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if other == first {
		t.Fatal("different vault shared a client")
	}
}

func TestHyperliquidMetadataRefreshKeepsOldClientOnFailure(t *testing.T) {
	adapter := newHyperliquidTestAdapter(t, `{"status":"ok","response":{"type":"order","data":{"statuses":[]}}}`, nil, nil)
	first, release, err := adapter.client(hyperliquidTestCredentials(), "BTC")
	if err != nil {
		t.Fatal(err)
	}
	oldStream := first.Stream
	release()
	adapter.base = "://bad"
	_, _, err = adapter.client(hyperliquidTestCredentials(), "ETH")
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("missing symbol after failed refresh err=%v", err)
	}
	again, release, err := adapter.client(hyperliquidTestCredentials(), "BTC")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if again != first || again.Stream != oldStream {
		t.Fatal("failed refresh replaced the healthy client")
	}
}

func TestHyperliquidMetadataRefreshClosesOldStreamAfterSwap(t *testing.T) {
	adapter := newHyperliquidTestAdapter(t, `{"status":"ok","response":{"type":"order","data":{"statuses":[]}}}`, nil, nil)
	first, release, err := adapter.client(hyperliquidTestCredentials(), "BTC")
	if err != nil {
		t.Fatal(err)
	}
	oldStream := first.Stream
	release()
	_, _, err = adapter.client(hyperliquidTestCredentials(), "ETH")
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err=%v", err)
	}
	if err := oldStream.Connect(context.Background()); !errors.Is(err, stream.ErrNotConnected) {
		t.Fatalf("old stream after successful replacement Connect err=%v", err)
	}
}

func newHyperliquidTestAdapter(
	t *testing.T,
	actionPayload string,
	exchangeCalls *atomic.Int32,
	upgrades *atomic.Int32,
) *hyperliquidAdapter {
	t.Helper()
	server := newHyperliquidActionWSServer(t, actionPayload, exchangeCalls, upgrades)
	adapter := newHyperliquid(server.Client(), server.URL).(*hyperliquidAdapter)
	t.Cleanup(server.Close)
	t.Cleanup(func() { _ = adapter.Close() })
	return adapter
}

func newHyperliquidActionWSServer(
	t *testing.T,
	actionPayload string,
	exchangeCalls *atomic.Int32,
	upgrades *atomic.Int32,
) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/ws":
			if upgrades != nil {
				upgrades.Add(1)
			}
			conn, err := upgrader.Upgrade(writer, request, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				_, raw, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var envelope struct {
					Method string `json:"method"`
					ID     int    `json:"id"`
				}
				if json.Unmarshal(raw, &envelope) != nil || envelope.Method != "post" {
					continue
				}
				if actionPayload == "" {
					continue
				}
				reply, _ := json.Marshal(map[string]any{
					"channel": "post",
					"data": map[string]any{
						"id": envelope.ID,
						"response": map[string]any{
							"type":    "action",
							"payload": json.RawMessage(actionPayload),
						},
					},
				})
				if err := conn.WriteMessage(websocket.TextMessage, reply); err != nil {
					return
				}
			}
		case "/exchange":
			if exchangeCalls != nil {
				exchangeCalls.Add(1)
			}
			http.Error(writer, "REST exchange must not be used for placement", http.StatusInternalServerError)
		case "/info":
			var payload struct {
				Type string `json:"type"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				http.Error(writer, "invalid", http.StatusBadRequest)
				return
			}
			switch payload.Type {
			case "orderStatus":
				_, _ = writer.Write([]byte(`{"status":"unknown"}`))
			case "clearinghouseState":
				_, _ = writer.Write([]byte(`{"marginSummary":{"accountValue":"1000"},"withdrawable":"1000","assetPositions":[]}`))
			case "meta":
				_, _ = writer.Write([]byte(`{"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":50}]}`))
			case "spotMeta":
				_, _ = writer.Write([]byte(`{"universe":[],"tokens":[]}`))
			case "outcomeMeta":
				_, _ = writer.Write([]byte(`{}`))
			default:
				_, _ = writer.Write([]byte(`{}`))
			}
		default:
			http.NotFound(writer, request)
		}
	}))
}
