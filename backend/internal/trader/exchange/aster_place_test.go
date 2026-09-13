package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestAsterPlaceOrderRejectsCapacityLimitAsTerminal(t *testing.T) {
	const message = "ReduceOnly Order is rejected."
	stats := &asterPlaceCallStats{}
	server := newAsterPlaceServer(t, stats, func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"code":-5018,"msg":"` + message + `"}`))
	})
	defer server.Close()

	result, err := newAster(server.Client(), server.URL).PlaceOrder(
		context.Background(), asterPlaceCredentials(), asterPlaceRequest(),
	)
	if !errors.Is(err, ErrRejected) || errors.Is(err, ErrUncertain) {
		t.Fatalf("err=%v", err)
	}
	if result.Status != "rejected" ||
		result.ErrorCode != "-5018" ||
		result.FilledQuantity != "0" ||
		result.VenueOrderID != "" ||
		result.ErrorMessage != message {
		t.Fatalf("result=%+v", result)
	}
	if stats.posts.Load() != 1 || stats.getsAfterPost.Load() != 0 || stats.deletes.Load() != 0 {
		t.Fatalf("posts=%d getsAfterPost=%d deletes=%d",
			stats.posts.Load(), stats.getsAfterPost.Load(), stats.deletes.Load())
	}
}

func TestAsterPlaceOrderKeepsOtherRejectedCodesUnknown(t *testing.T) {
	stats := &asterPlaceCallStats{}
	server := newAsterPlaceServer(t, stats, func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"code":-1111,"msg":"Precision is over the maximum defined for this asset."}`))
	})
	defer server.Close()

	result, err := newAster(server.Client(), server.URL).PlaceOrder(
		context.Background(), asterPlaceCredentials(), asterPlaceRequest(),
	)
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err=%v", err)
	}
	if result.Status != "unknown" ||
		result.ErrorCode != "-1111" ||
		terminalOrderStatus(result.Status) {
		t.Fatalf("result=%+v", result)
	}
}

func TestAsterPlaceOrderHTTP500WithCapacityCodeStaysUncertain(t *testing.T) {
	stats := &asterPlaceCallStats{}
	server := newAsterPlaceServer(t, stats, func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(`{"code":-5018,"msg":"ReduceOnly Order is rejected."}`))
	})
	defer server.Close()

	result, err := newAster(server.Client(), server.URL).PlaceOrder(
		context.Background(), asterPlaceCredentials(), asterPlaceRequest(),
	)
	if !errors.Is(err, ErrUncertain) || errors.Is(err, ErrRejected) {
		t.Fatalf("err=%v", err)
	}
	if result.Status == "rejected" || terminalOrderStatus(result.Status) {
		t.Fatalf("result=%+v", result)
	}
}

func TestAsterPlaceOrderTimeoutStaysUncertain(t *testing.T) {
	stats := &asterPlaceCallStats{}
	server := newAsterPlaceServer(t, stats, func(writer http.ResponseWriter, request *http.Request) {
		time.Sleep(150 * time.Millisecond)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"orderId":1,"status":"NEW","executedQty":"0"}`))
	})
	defer server.Close()

	client := server.Client()
	client.Timeout = 40 * time.Millisecond
	result, err := newAster(client, server.URL).PlaceOrder(
		context.Background(), asterPlaceCredentials(), asterPlaceRequest(),
	)
	if err == nil || result.Status == "rejected" || terminalOrderStatus(result.Status) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if errors.Is(err, ErrRejected) && result.ErrorCode == "-5018" {
		t.Fatalf("timeout classified as -5018 rejected: result=%+v err=%v", result, err)
	}
}

func TestAsterPlaceOrderBadJSONIsNotTerminalRejected(t *testing.T) {
	stats := &asterPlaceCallStats{}
	server := newAsterPlaceServer(t, stats, func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{not-json`))
	})
	defer server.Close()

	result, err := newAster(server.Client(), server.URL).PlaceOrder(
		context.Background(), asterPlaceCredentials(), asterPlaceRequest(),
	)
	if err == nil || result.Status == "rejected" || terminalOrderStatus(result.Status) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestAsterGetOrderNotFoundCodesStayUnknown(t *testing.T) {
	tests := []struct {
		name string
		code int
		msg  string
	}{
		{name: "order-does-not-exist", code: -2013, msg: "Order does not exist."},
		{name: "unknown-order-sent", code: -2011, msg: "Unknown order sent."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/fapi/v1/order" || request.Method != http.MethodGet {
					t.Fatalf("unexpected %s %s", request.Method, request.URL.Path)
				}
				writer.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(writer).Encode(map[string]any{
					"code": test.code, "msg": test.msg,
				})
			}))
			defer server.Close()
			result, err := newAster(server.Client(), server.URL).GetOrder(
				context.Background(), asterPlaceCredentials(), QueryRequest{
					Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
					ClientOrderID: "client-1",
				},
			)
			if !errors.Is(err, ErrOrderNotFound) || errors.Is(err, ErrRejected) {
				t.Fatalf("err=%v", err)
			}
			if result.Status != "unknown" || result.ErrorCode != strconv.Itoa(test.code) {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

type asterPlaceCallStats struct {
	posts         atomic.Int32
	getsAfterPost atomic.Int32
	deletes       atomic.Int32
	posted        atomic.Bool
}

func newAsterPlaceServer(
	t *testing.T,
	stats *asterPlaceCallStats,
	onPost func(http.ResponseWriter, *http.Request),
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/fapi/v1/positionSide/dual":
			_, _ = writer.Write([]byte(`{"dualSidePosition":false}`))
		case request.URL.Path == "/fapi/v1/order" && request.Method == http.MethodGet:
			if stats.posted.Load() {
				stats.getsAfterPost.Add(1)
			}
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"code":-2013,"msg":"Order does not exist."}`))
		case request.URL.Path == "/fapi/v1/order" && request.Method == http.MethodPost:
			stats.posted.Store(true)
			stats.posts.Add(1)
			onPost(writer, request)
		case request.Method == http.MethodDelete:
			stats.deletes.Add(1)
			_, _ = io.Copy(io.Discard, request.Body)
			http.Error(writer, "cancel must not run", http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected %s %s", request.Method, request.URL.Path)
		}
	}))
}

func asterPlaceCredentials() Credentials {
	return Credentials{APIKey: "key", APISecret: "secret"}
}

func asterPlaceRequest() OrderRequest {
	return OrderRequest{
		Instrument: Instrument{
			ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
			QuantityStep: "0.001",
		},
		ClientOrderID: "client-1", Side: "buy", OrderType: "limit",
		Quantity: "0.001", Price: "100",
	}
}
