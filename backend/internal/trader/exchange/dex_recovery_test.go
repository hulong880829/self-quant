package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

const dexRecoveryTestKey = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

func TestHyperliquidResolveOrderUsesOrderStatus(t *testing.T) {
	var statusCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Type string `json:"type"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Type != "orderStatus" {
			t.Fatalf("unexpected info type %q", payload.Type)
		}
		statusCalls.Add(1)
		_, _ = writer.Write([]byte(`{"status":"unknown"}`))
	}))
	defer server.Close()

	resolution, err := newHyperliquid(server.Client(), server.URL).(OrderResolver).ResolveOrder(
		context.Background(), Credentials{SigningAddress: "0x1111111111111111111111111111111111111111"},
		QueryRequest{
			Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTC"},
			ClientOrderID: "missing",
		},
	)
	if err != nil || !resolution.ConfirmedAbsent || resolution.Found ||
		statusCalls.Load() != 1 {
		t.Fatalf("resolution=%+v status=%d err=%v", resolution, statusCalls.Load(), err)
	}
}

func TestHyperliquidResolveOrderFindsExactOrderStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Type string `json:"type"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Type != "orderStatus" {
			t.Fatalf("unexpected info type %q", payload.Type)
		}
		_, _ = writer.Write([]byte(`{
			"status":"order",
			"order":{
				"order":{"coin":"xyz:ZHIPU","oid":99,"cloid":"` + hyperliquidCloid("hip3-1") + `","sz":"1","origSz":"1"},
				"status":"open",
				"statusTimestamp":1700000000000
			}
		}`))
	}))
	defer server.Close()

	resolution, err := newHyperliquid(server.Client(), server.URL).(OrderResolver).ResolveOrder(
		context.Background(), Credentials{SigningAddress: "0x1111111111111111111111111111111111111111"},
		QueryRequest{
			Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "xyz:ZHIPU"},
			ClientOrderID: "hip3-1",
		},
	)
	if err != nil || !resolution.Found || !resolution.Active ||
		resolution.Result.VenueOrderID != "99" {
		t.Fatalf("resolution=%+v err=%v", resolution, err)
	}
}

func TestHyperliquidPlacementPreflightReturnsExistingOrder(t *testing.T) {
	var exchangeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/exchange" {
			exchangeCalls.Add(1)
			t.Fatal("preflight should prevent a duplicate order submission")
		}
		_, _ = writer.Write([]byte(`{
			"status":"order",
			"order":{
				"order":{"coin":"BTC","oid":42,"cloid":"` + hyperliquidCloid("client-1") + `","sz":"1","origSz":"1"},
				"status":"open",
				"statusTimestamp":1700000000000
			}
		}`))
	}))
	defer server.Close()
	result, err := newHyperliquid(server.Client(), server.URL).PlaceOrder(
		context.Background(), Credentials{
			APIKey:    "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
			APISecret: dexRecoveryTestKey, SigningAddress: "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
		},
		OrderRequest{
			Instrument: Instrument{
				ContractType: "perpetual", ExchangeSymbol: "BTC",
				QuantityStep: "0.001",
			},
			ClientOrderID: "client-1", Side: "buy", OrderType: "limit",
			Quantity: "1", Price: "100",
		},
	)
	if err != nil || result.VenueOrderID != "42" || exchangeCalls.Load() != 0 {
		t.Fatalf("result=%+v exchangeCalls=%d err=%v", result, exchangeCalls.Load(), err)
	}
}

func TestHyperliquidResolveOrderDoesNotScanHistoricalOrders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Type string `json:"type"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Type != "orderStatus" {
			t.Fatalf("unexpected info type %q", payload.Type)
		}
		_, _ = writer.Write([]byte(`{"status":"unknown"}`))
	}))
	defer server.Close()
	resolution, err := newHyperliquid(server.Client(), server.URL).(OrderResolver).ResolveOrder(
		context.Background(), Credentials{SigningAddress: "0x1111111111111111111111111111111111111111"},
		QueryRequest{
			Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTC"},
			ClientOrderID: "missing",
		},
	)
	if err != nil || !resolution.ConfirmedAbsent {
		t.Fatalf("resolution=%+v err=%v", resolution, err)
	}
}

func TestAsterResolveOrderUsesExactOrderLookup(t *testing.T) {
	var orderCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/fapi/v1/order" {
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
		orderCalls.Add(1)
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"code":-2013,"msg":"Order does not exist."}`))
	}))
	defer server.Close()

	adapter := newAster(server.Client(), server.URL).(*asterAdapter)
	resolution, err := adapter.ResolveOrder(
		context.Background(), Credentials{APIKey: "key", APISecret: "secret"},
		QueryRequest{
			Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
			ClientOrderID: "missing",
		},
	)
	if err != nil || !resolution.ConfirmedAbsent || orderCalls.Load() != 1 {
		t.Fatalf("resolution=%+v calls=%d err=%v", resolution, orderCalls.Load(), err)
	}
}

func TestLighterResolveOrderUsesInactiveThenActiveOrders(t *testing.T) {
	accountIndex, apiKeyIndex := int64(7), int32(2)
	clientIndex := lighterClientOrderIndex("client-1")
	var inactiveCalls, activeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") == "" {
			t.Fatal("missing Lighter auth token")
		}
		if request.URL.Query().Get("account_index") != "7" ||
			request.URL.Query().Get("market_id") != "12" {
			t.Fatalf("query=%s", request.URL.RawQuery)
		}
		switch request.URL.Path {
		case "/api/v1/accountInactiveOrders":
			inactiveCalls.Add(1)
			_, _ = writer.Write([]byte(`{"code":200,"orders":[]}`))
		case "/api/v1/accountActiveOrders":
			activeCalls.Add(1)
			_, _ = writer.Write([]byte(`{"code":200,"orders":[{` +
				`"order_index":42,"client_order_index":` + strconv.FormatInt(clientIndex, 10) + `,` +
				`"order_id":"42","initial_base_amount":"1","remaining_base_amount":"1",` +
				`"filled_base_amount":"0","filled_quote_amount":"0","status":"open"}]}`))
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	instrument := Instrument{
		ContractType: "perpetual", ExchangeSymbol: "BTC",
		Metadata: map[string]any{
			"market_id": "12", "supported_size_decimals": "4",
			"supported_price_decimals": "2",
		},
	}
	resolution, err := newLighter(server.Client(), server.URL).(OrderResolver).ResolveOrder(
		context.Background(), Credentials{
			APISecret: dexRecoveryTestKey, AccountIndex: &accountIndex, APIKeyIndex: &apiKeyIndex,
		},
		QueryRequest{Instrument: instrument, ClientOrderID: "client-1"},
	)
	if err != nil || !resolution.Found || !resolution.Active ||
		resolution.Result.VenueOrderID != "42" ||
		inactiveCalls.Load() != 1 || activeCalls.Load() != 1 {
		t.Fatalf(
			"resolution=%+v inactive=%d active=%d err=%v",
			resolution, inactiveCalls.Load(), activeCalls.Load(), err,
		)
	}
}

func TestLighterPlacementPreflightReturnsExistingOrder(t *testing.T) {
	accountIndex, apiKeyIndex := int64(7), int32(2)
	clientIndex := lighterClientOrderIndex("client-1")
	var sendCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/accountInactiveOrders":
			_, _ = writer.Write([]byte(`{"code":200,"orders":[{` +
				`"order_index":42,"client_order_index":` + strconv.FormatInt(clientIndex, 10) + `,` +
				`"order_id":"42","initial_base_amount":"1","remaining_base_amount":"0",` +
				`"filled_base_amount":"1","filled_quote_amount":"100","status":"filled"}]}`))
		case "/api/v1/sendTx":
			sendCalls.Add(1)
			t.Fatal("preflight should prevent a duplicate order submission")
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	result, err := newLighter(server.Client(), server.URL).PlaceOrder(
		context.Background(), Credentials{
			APISecret: dexRecoveryTestKey, AccountIndex: &accountIndex, APIKeyIndex: &apiKeyIndex,
		},
		OrderRequest{
			Instrument: Instrument{
				ContractType: "perpetual", ExchangeSymbol: "BTC",
				Metadata: map[string]any{
					"market_id": "12", "supported_size_decimals": "4",
					"supported_price_decimals": "2",
				},
			},
			ClientOrderID: "client-1", Side: "buy", OrderType: "limit",
			Quantity: "1", Price: "100",
		},
	)
	if err != nil || result.VenueOrderID != "42" || sendCalls.Load() != 0 {
		t.Fatalf("result=%+v sendCalls=%d err=%v", result, sendCalls.Load(), err)
	}
}

func TestLighterResolveOrderFailsClosedAtInactiveLimit(t *testing.T) {
	accountIndex, apiKeyIndex := int64(7), int32(2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/accountInactiveOrders":
			orders := make([]map[string]any, 100)
			for index := range orders {
				orders[index] = map[string]any{
					"order_index": index + 1, "client_order_index": index + 1,
					"status": "canceled",
				}
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"code": 200, "orders": orders})
		case "/api/v1/accountActiveOrders":
			_, _ = writer.Write([]byte(`{"code":200,"orders":[]}`))
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	instrument := Instrument{
		ContractType: "perpetual", ExchangeSymbol: "BTC",
		Metadata: map[string]any{
			"market_id": "12", "supported_size_decimals": "4",
			"supported_price_decimals": "2",
		},
	}
	resolution, err := newLighter(server.Client(), server.URL).(OrderResolver).ResolveOrder(
		context.Background(), Credentials{
			APISecret: dexRecoveryTestKey, AccountIndex: &accountIndex, APIKeyIndex: &apiKeyIndex,
		},
		QueryRequest{Instrument: instrument, ClientOrderID: "missing"},
	)
	if !errors.Is(err, ErrUncertain) || resolution.ConfirmedAbsent {
		t.Fatalf("resolution=%+v err=%v", resolution, err)
	}
}

func TestAsterCancelRequiresTerminalRequery(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		status := "CANCELED"
		if request.Method == http.MethodGet {
			status = "NEW"
		}
		_, _ = writer.Write([]byte(
			`{"orderId":42,"clientOrderId":"client-1","status":"` + status + `","executedQty":"0"}`,
		))
	}))
	defer server.Close()
	result, err := newAster(server.Client(), server.URL).CancelAndGetOrder(
		context.Background(), Credentials{APIKey: "key", APISecret: "secret"},
		CancelRequest{
			Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
			ClientOrderID: "client-1", VenueOrderID: "42",
		},
	)
	if !errors.Is(err, ErrUncertain) || terminalOrderStatus(result.Status) ||
		requests.Load() != 2 {
		t.Fatalf("result=%+v requests=%d err=%v", result, requests.Load(), err)
	}
}

func TestHyperliquidCancelRequiresTerminalRequery(t *testing.T) {
	var exchangeCalls, queryCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/exchange":
			exchangeCalls.Add(1)
			_, _ = writer.Write([]byte(
				`{"status":"ok","response":{"type":"cancel","data":{"statuses":["success"]}}}`,
			))
		case "/info":
			var payload struct {
				Type string `json:"type"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			switch payload.Type {
			case "meta":
				_, _ = writer.Write([]byte(
					`{"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":50}]}`,
				))
			case "spotMeta":
				_, _ = writer.Write([]byte(`{"universe":[],"tokens":[]}`))
			case "outcomeMeta":
				_, _ = writer.Write([]byte(`{}`))
			case "orderStatus":
				queryCalls.Add(1)
				_, _ = writer.Write([]byte(`{
					"status":"order",
					"order":{
						"order":{"coin":"BTC","oid":42,"cloid":"` + hyperliquidCloid("client-1") + `","sz":"1","origSz":"1"},
						"status":"open",
						"statusTimestamp":1700000000000
					}
				}`))
			default:
				t.Fatalf("unexpected info type %q", payload.Type)
			}
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	result, err := newHyperliquid(server.Client(), server.URL).CancelOrder(
		context.Background(), Credentials{
			APIKey:    "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
			APISecret: dexRecoveryTestKey, SigningAddress: "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
		},
		CancelRequest{
			Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTC"},
			ClientOrderID: "client-1", VenueOrderID: "42",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "pending" || !result.LocalCommandAck ||
		queryCalls.Load() != 0 || exchangeCalls.Load() != 1 {
		t.Fatalf(
			"result=%+v exchange=%d query=%d err=%v",
			result, exchangeCalls.Load(), queryCalls.Load(), err,
		)
	}
}

func TestLighterCancelRequiresTerminalRequery(t *testing.T) {
	accountIndex, apiKeyIndex := int64(7), int32(2)
	clientIndex := lighterClientOrderIndex("client-1")
	var sendCalls, activeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/nextNonce":
			_, _ = writer.Write([]byte(`{"code":200,"nonce":11}`))
		case "/api/v1/sendTx":
			sendCalls.Add(1)
			_, _ = writer.Write([]byte(`{"code":200,"tx_hash":"tx-1"}`))
		case "/api/v1/accountInactiveOrders":
			_, _ = writer.Write([]byte(`{"code":200,"orders":[]}`))
		case "/api/v1/accountActiveOrders":
			activeCalls.Add(1)
			_, _ = writer.Write([]byte(`{"code":200,"orders":[{` +
				`"order_index":42,"client_order_index":` + strconv.FormatInt(clientIndex, 10) + `,` +
				`"order_id":"42","initial_base_amount":"1","remaining_base_amount":"1",` +
				`"filled_base_amount":"0","filled_quote_amount":"0","status":"open"}]}`))
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	result, err := newLighter(server.Client(), server.URL).CancelAndGetOrder(
		context.Background(), Credentials{
			APISecret: dexRecoveryTestKey, AccountIndex: &accountIndex, APIKeyIndex: &apiKeyIndex,
		},
		CancelRequest{
			Instrument: Instrument{
				ContractType: "perpetual", ExchangeSymbol: "BTC",
				Metadata: map[string]any{
					"market_id": "12", "supported_size_decimals": "4",
					"supported_price_decimals": "2",
				},
			},
			ClientOrderID: "client-1", VenueOrderID: "42",
		},
	)
	if !errors.Is(err, ErrUncertain) || terminalOrderStatus(result.Status) ||
		sendCalls.Load() != 1 || activeCalls.Load() != 1 {
		t.Fatalf(
			"result=%+v send=%d active=%d err=%v",
			result, sendCalls.Load(), activeCalls.Load(), err,
		)
	}
}

func TestAsterPlacementPreflightReturnsExistingOrder(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/fapi/v1/positionSide/dual":
			_, _ = writer.Write([]byte(`{"dualSidePosition":false}`))
		case "/fapi/v1/order":
			if request.Method == http.MethodPost {
				posts.Add(1)
			}
			_, _ = writer.Write([]byte(
				`{"orderId":42,"clientOrderId":"client-1","status":"NEW","executedQty":"0"}`,
			))
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	result, err := newAster(server.Client(), server.URL).PlaceOrder(
		context.Background(), Credentials{APIKey: "key", APISecret: "secret"},
		OrderRequest{
			Instrument: Instrument{
				ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
				QuantityStep: "0.001",
			},
			ClientOrderID: "client-1", Side: "buy", OrderType: "limit",
			Quantity: "0.001", Price: "100",
		},
	)
	if err != nil || result.VenueOrderID != "42" || posts.Load() != 0 {
		t.Fatalf("result=%+v posts=%d err=%v", result, posts.Load(), err)
	}
}
