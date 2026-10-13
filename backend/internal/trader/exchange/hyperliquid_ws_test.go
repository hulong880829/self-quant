package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
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
	adapter, _ := newHyperliquidTestAdapterCapture(t, actionPayload, exchangeCalls, upgrades, nil)
	return adapter
}

type hyperliquidWSRecorder struct {
	mu        sync.Mutex
	infoTypes []string
	orders    []capturedHLOrderWire
}

type capturedHLOrderWire struct {
	Asset      int    `json:"a"`
	IsBuy      bool   `json:"b"`
	LimitPx    string `json:"p"`
	Size       string `json:"s"`
	ReduceOnly bool   `json:"r"`
	OrderType  struct {
		Limit *struct {
			Tif string `json:"tif"`
		} `json:"limit"`
	} `json:"t"`
}

func (r *hyperliquidWSRecorder) recordInfo(typ string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.infoTypes = append(r.infoTypes, typ)
	r.mu.Unlock()
}

func (r *hyperliquidWSRecorder) recordAction(raw []byte) {
	if r == nil {
		return
	}
	var envelope struct {
		Method  string `json:"method"`
		Request struct {
			Type    string `json:"type"`
			Payload struct {
				Action struct {
					Orders []capturedHLOrderWire `json:"orders"`
				} `json:"action"`
			} `json:"payload"`
		} `json:"request"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return
	}
	if envelope.Method != "post" || envelope.Request.Type != "action" {
		return
	}
	r.mu.Lock()
	r.orders = append(r.orders, envelope.Request.Payload.Action.Orders...)
	r.mu.Unlock()
}

func (r *hyperliquidWSRecorder) snapshot() ([]string, []capturedHLOrderWire) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.infoTypes...), append([]capturedHLOrderWire(nil), r.orders...)
}

func newHyperliquidTestAdapterCapture(
	t *testing.T,
	actionPayload string,
	exchangeCalls *atomic.Int32,
	upgrades *atomic.Int32,
	recorder *hyperliquidWSRecorder,
) (*hyperliquidAdapter, *httptest.Server) {
	t.Helper()
	server := newHyperliquidActionWSServer(t, actionPayload, exchangeCalls, upgrades, recorder)
	adapter := newHyperliquid(server.Client(), server.URL).(*hyperliquidAdapter)
	t.Cleanup(server.Close)
	t.Cleanup(func() { _ = adapter.Close() })
	return adapter, server
}

func newHyperliquidActionWSServer(
	t *testing.T,
	actionPayload string,
	exchangeCalls *atomic.Int32,
	upgrades *atomic.Int32,
	recorder *hyperliquidWSRecorder,
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
				recorder.recordAction(raw)
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
			recorder.recordInfo(payload.Type)
			switch payload.Type {
			case "allMids":
				_, _ = writer.Write([]byte(`{"SKR":"0.018003"}`))
			case "orderStatus":
				_, _ = writer.Write([]byte(`{"status":"unknown"}`))
			case "clearinghouseState":
				_, _ = writer.Write([]byte(`{"marginSummary":{"accountValue":"1000"},"withdrawable":"1000","assetPositions":[]}`))
			case "meta":
				_, _ = writer.Write([]byte(`{"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":50},{"name":"HLFILL1","szDecimals":2,"maxLeverage":20},{"name":"HLFILL2","szDecimals":2,"maxLeverage":20},{"name":"SKR","szDecimals":0,"maxLeverage":20}]}`))
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

func TestHyperliquidPlaceOrderSKRMarketProtectivePrice(t *testing.T) {
	cases := []struct {
		name       string
		side       string
		reduceOnly bool
		wantBuy    bool
		wantPx     string
	}{
		{name: "market buy", side: "buy", wantBuy: true, wantPx: "0.018903"},
		{name: "market sell", side: "sell", wantBuy: false, wantPx: "0.017103"},
		{name: "reduce-only close long", side: "sell", reduceOnly: true, wantBuy: false, wantPx: "0.017103"},
		{name: "reduce-only close short", side: "buy", reduceOnly: true, wantBuy: true, wantPx: "0.018903"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var exchangeCalls atomic.Int32
			rec := &hyperliquidWSRecorder{}
			payload := `{"status":"ok","response":{"type":"order","data":{"statuses":[{"filled":{"oid":7,"avgPx":"0.018003","totalSz":"1"}}]}}}`
			adapter, _ := newHyperliquidTestAdapterCapture(t, payload, &exchangeCalls, nil, rec)
			result, err := adapter.PlaceOrder(
				context.Background(),
				hyperliquidTestCredentials(),
				OrderRequest{
					Instrument: Instrument{
						ContractType: "perpetual", ExchangeSymbol: "SKR",
						QuantityStep: "1",
					},
					ClientOrderID: "client-skr-" + tc.name, Side: tc.side, OrderType: "market",
					Quantity: "1", ReduceOnly: tc.reduceOnly,
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
			infoTypes, orders := rec.snapshot()
			assertHyperliquidPricingUsesAllMidsOnly(t, infoTypes)
			if len(orders) != 1 {
				t.Fatalf("ws orders=%+v", orders)
			}
			order := orders[0]
			if order.Asset < 3 {
				t.Fatalf("SKR asset id=%d, want >= 3", order.Asset)
			}
			if order.IsBuy != tc.wantBuy || order.LimitPx != tc.wantPx || order.Size != "1" {
				t.Fatalf("order=%+v want isBuy=%v px=%s", order, tc.wantBuy, tc.wantPx)
			}
			if order.ReduceOnly != tc.reduceOnly {
				t.Fatalf("reduceOnly=%v want %v", order.ReduceOnly, tc.reduceOnly)
			}
			if order.OrderType.Limit == nil || order.OrderType.Limit.Tif != "Ioc" {
				t.Fatalf("tif=%+v, want Ioc", order.OrderType)
			}
		})
	}
}

func assertHyperliquidPricingUsesAllMidsOnly(t *testing.T, types []string) {
	t.Helper()
	allowed := map[string]bool{
		"allMids": true, "meta": true, "spotMeta": true, "outcomeMeta": true,
		"orderStatus": true, "clearinghouseState": true,
	}
	sawAllMids := false
	for _, typ := range types {
		if typ == "allMids" {
			sawAllMids = true
		}
		if typ == "l2Book" {
			t.Fatal("pricing path queried l2Book")
		}
		if !allowed[typ] {
			t.Fatalf("unexpected /info type %q; pricing must use AllMids only", typ)
		}
	}
	if !sawAllMids {
		t.Fatalf("expected allMids, got %v", types)
	}
}
