package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExactResolversRejectBatchRecoveryEndpoints(t *testing.T) {
	forbidden := []string{
		"allOrders", "openOrders", "orders_timerange", "historicalOrders",
		"frontendOpenOrders", "history-orders", "unfilled-orders",
	}
	assertAllowed := func(t *testing.T, rawURL, body string) {
		t.Helper()
		combined := rawURL + " " + body
		for _, token := range forbidden {
			if strings.Contains(combined, token) {
				t.Fatalf("forbidden recovery endpoint %q in %s", token, combined)
			}
		}
	}

	t.Run("binance", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assertAllowed(t, request.URL.Path, "")
			if request.URL.Path != "/papi/v1/um/order" {
				t.Fatalf("path=%s", request.URL.Path)
			}
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"code":-2013,"msg":"Order does not exist."}`))
		}))
		defer server.Close()
		resolution, err := newBinance(server.Client(), server.URL).(OrderResolver).ResolveOrder(
			context.Background(), Credentials{APIKey: "k", APISecret: "s"},
			QueryRequest{
				Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
				ClientOrderID: "missing",
			},
		)
		if err != nil || !resolution.ConfirmedAbsent {
			t.Fatalf("resolution=%+v err=%v", resolution, err)
		}
	})

	t.Run("bitget", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assertAllowed(t, request.URL.Path, "")
			if !strings.Contains(request.URL.Path, "/api/v3/trade/order-info") {
				t.Fatalf("path=%s", request.URL.Path)
			}
			_, _ = writer.Write([]byte(`{"code":"25204","msg":"Order does not exist"}`))
		}))
		defer server.Close()
		resolution, err := newBitget(server.Client(), server.URL).(OrderResolver).ResolveOrder(
			context.Background(), Credentials{APIKey: "k", APISecret: "s", Passphrase: "p"},
			QueryRequest{
				Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
				ClientOrderID: "missing",
			},
		)
		if err != nil || !resolution.ConfirmedAbsent {
			t.Fatalf("resolution=%+v err=%v", resolution, err)
		}
	})

	t.Run("okx", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assertAllowed(t, request.URL.Path, "")
			if request.URL.Path != "/api/v5/trade/order" {
				t.Fatalf("path=%s", request.URL.Path)
			}
			_, _ = writer.Write([]byte(`{"code":"51603","msg":"Order does not exist","data":[]}`))
		}))
		defer server.Close()
		resolution, err := newOKX(server.Client(), server.URL).(OrderResolver).ResolveOrder(
			context.Background(), Credentials{APIKey: "k", APISecret: "s", Passphrase: "p"},
			QueryRequest{
				Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP"},
				ClientOrderID: "missing",
			},
		)
		if err != nil || !resolution.ConfirmedAbsent {
			t.Fatalf("resolution=%+v err=%v", resolution, err)
		}
	})

	t.Run("aster", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assertAllowed(t, request.URL.Path, "")
			if request.URL.Path != "/fapi/v1/order" {
				t.Fatalf("path=%s", request.URL.Path)
			}
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"code":-2013,"msg":"Order does not exist."}`))
		}))
		defer server.Close()
		resolution, err := newAster(server.Client(), server.URL).(OrderResolver).ResolveOrder(
			context.Background(), Credentials{APIKey: "k", APISecret: "s"},
			QueryRequest{
				Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
				ClientOrderID: "missing",
			},
		)
		if err != nil || !resolution.ConfirmedAbsent {
			t.Fatalf("resolution=%+v err=%v", resolution, err)
		}
	})

	t.Run("gate", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			assertAllowed(t, request.URL.Path, "")
			if !strings.Contains(request.URL.Path, "/api/v4/futures/usdt/orders/") {
				t.Fatalf("path=%s", request.URL.Path)
			}
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"label":"ORDER_NOT_FOUND","message":"order not found"}`))
		}))
		defer server.Close()
		resolution, err := newGate(server.Client(), server.URL).(OrderResolver).ResolveOrder(
			context.Background(), Credentials{APIKey: "k", APISecret: "s"},
			QueryRequest{
				Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTC_USDT", SettleAsset: "USDT"},
				ClientOrderID: "missing",
			},
		)
		if err != nil || !resolution.ConfirmedAbsent {
			t.Fatalf("resolution=%+v err=%v", resolution, err)
		}
	})

	t.Run("hyperliquid", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			assertAllowed(t, request.URL.Path, string(body))
			var payload struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Type != "orderStatus" {
				t.Fatalf("type=%s", payload.Type)
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
	})
}

func TestBybitResolveVenueOrderUsesRealtimeThenExactHistory(t *testing.T) {
	var realtime, history int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		combined := request.URL.String() + " " + string(body)
		if strings.Contains(combined, "cursor") {
			t.Fatalf("cursor forbidden: %s", combined)
		}
		switch request.URL.Path {
		case "/v5/order/realtime":
			realtime++
			if request.URL.Query().Get("orderId") != "venue-1" {
				t.Fatalf("realtime query=%s", request.URL.RawQuery)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"retCode": 0, "result": map[string]any{"list": []any{}},
			})
		case "/v5/order/history":
			history++
			if request.URL.Query().Get("orderId") != "venue-1" ||
				request.URL.Query().Get("cursor") != "" {
				t.Fatalf("history query=%s", request.URL.RawQuery)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"retCode": 0, "result": map[string]any{"list": []any{}},
			})
		default:
			t.Fatalf("path=%s", request.URL.Path)
		}
	}))
	defer server.Close()
	adapter := newBybit(server.Client(), server.URL)
	_, err := adapter.GetOrder(
		context.Background(), Credentials{APIKey: "k", APISecret: "s"},
		QueryRequest{
			Instrument:   Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
			VenueOrderID: "venue-1",
		},
	)
	if !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("get err=%v", err)
	}
	resolution, err := adapter.(OrderResolver).ResolveOrder(
		context.Background(), Credentials{APIKey: "k", APISecret: "s"},
		QueryRequest{
			Instrument:   Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
			VenueOrderID: "venue-1",
		},
	)
	if err != nil || !resolution.ConfirmedAbsent || realtime != 1 || history != 1 {
		t.Fatalf("resolution=%+v realtime=%d history=%d err=%v", resolution, realtime, history, err)
	}
}

func TestOKXEmptyQueryDataIsUncertainNotAbsent(t *testing.T) {
	result, err := parseOKXOrder([]byte(`{"code":"0","data":[]}`), Instrument{
		ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
	}, false)
	if !errors.Is(err, ErrUncertain) || result.Status != "unknown" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
