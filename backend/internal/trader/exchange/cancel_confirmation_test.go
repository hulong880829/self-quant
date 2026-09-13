package exchange

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCancelAcknowledgementRequiresTerminalQuery(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		adapter func(*httptest.Server) Adapter
		request CancelRequest
	}{
		{
			name: "binance",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				status := "CANCELED"
				if request.Method == http.MethodGet {
					status = "NEW"
				}
				_, _ = writer.Write([]byte(`{"orderId":42,"status":"` + status + `","executedQty":"0"}`))
			},
			adapter: func(server *httptest.Server) Adapter {
				return newBinance(server.Client(), server.URL)
			},
			request: CancelRequest{
				Instrument:   Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
				VenueOrderID: "42",
			},
		},
		{
			name: "bybit",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if request.URL.Path == "/v5/order/cancel" {
					_, _ = writer.Write([]byte(`{"retCode":0,"result":{"orderId":"42"}}`))
					return
				}
				_, _ = writer.Write([]byte(`{"retCode":0,"result":{"list":[{"orderId":"42","orderStatus":"New","cumExecQty":"0"}]}}`))
			},
			adapter: func(server *httptest.Server) Adapter {
				return newBybit(server.Client(), server.URL)
			},
			request: CancelRequest{
				Instrument:   Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
				VenueOrderID: "42",
			},
		},
		{
			name: "bitget",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if request.URL.Path == "/api/v3/trade/cancel-order" {
					_, _ = writer.Write([]byte(`{"code":"00000","data":{"orderId":"42"}}`))
					return
				}
				_, _ = writer.Write([]byte(`{"code":"00000","data":{"orderId":"42","orderStatus":"live","cumExecQty":"0"}}`))
			},
			adapter: func(server *httptest.Server) Adapter {
				return newBitget(server.Client(), server.URL)
			},
			request: CancelRequest{
				Instrument:   Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
				VenueOrderID: "42",
			},
		},
		{
			name: "gate",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				status := "cancelled"
				if request.Method == http.MethodGet {
					status = "open"
				}
				_, _ = writer.Write([]byte(`{"id":"42","status":"` + status + `","filled_amount":"0"}`))
			},
			adapter: func(server *httptest.Server) Adapter {
				return newGate(server.Client(), server.URL)
			},
			request: CancelRequest{
				Instrument: Instrument{
					ContractType: "spot", ExchangeSymbol: "BTC_USDT",
					BaseAsset: "BTC", QuoteAsset: "USDT",
				},
				VenueOrderID: "42",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(
				writer http.ResponseWriter, request *http.Request,
			) {
				requests.Add(1)
				test.handler(writer, request)
			}))
			defer server.Close()

			result, err := test.adapter(server).CancelAndGetOrder(
				context.Background(),
				Credentials{APIKey: "key", APISecret: "secret", Passphrase: "pass"},
				test.request,
			)
			if !errors.Is(err, ErrUncertain) || terminalOrderStatus(result.Status) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if requests.Load() != 2 {
				t.Fatalf("requests=%d, want cancel plus query", requests.Load())
			}
		})
	}
}

func TestBitgetCancel25204QueriesOrderImmediately(t *testing.T) {
	tests := []struct {
		name       string
		queryBody  string
		wantStatus string
		wantErr    error
	}{
		{
			name:       "filled",
			queryBody:  `{"code":"00000","data":{"orderId":"42","orderStatus":"filled","cumExecQty":"531","avgPrice":"1.2"}}`,
			wantStatus: "filled",
		},
		{
			name:       "still live",
			queryBody:  `{"code":"00000","data":{"orderId":"42","orderStatus":"live","cumExecQty":"0"}}`,
			wantStatus: "open",
			wantErr:    ErrUncertain,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(
				writer http.ResponseWriter, request *http.Request,
			) {
				requests.Add(1)
				writer.Header().Set("Content-Type", "application/json")
				if request.Method == http.MethodPost &&
					request.URL.Path == "/api/v3/trade/cancel-order" {
					_, _ = writer.Write([]byte(`{"code":"25204","msg":"Order does not exist"}`))
					return
				}
				if request.Method != http.MethodGet ||
					!strings.Contains(request.URL.Path, "/api/v3/trade/order-info") {
					t.Errorf("unexpected %s %s", request.Method, request.URL.Path)
				}
				_, _ = writer.Write([]byte(test.queryBody))
			}))
			defer server.Close()

			result, err := newBitget(server.Client(), server.URL).CancelAndGetOrder(
				context.Background(),
				Credentials{APIKey: "key", APISecret: "secret", Passphrase: "pass"},
				CancelRequest{
					Instrument:   Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
					VenueOrderID: "42",
				},
			)
			if test.wantErr == nil {
				if err != nil || result.Status != test.wantStatus ||
					result.FilledQuantity != "531" {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			} else if !errors.Is(err, test.wantErr) || result.Status != test.wantStatus ||
				terminalOrderStatus(result.Status) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if requests.Load() != 2 {
				t.Fatalf("requests=%d, want cancel plus query", requests.Load())
			}
		})
	}
}

func TestGateSpotNeverUsesQuoteFilledTotalAsBaseQuantity(t *testing.T) {
	result, err := parseGateSpot([]byte(
		`{"id":"42","status":"closed","filled_amount":"","filled_total":"250","avg_deal_price":"100"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if result.FilledQuantity != "" {
		t.Fatalf("filled quantity=%q, want empty base fill", result.FilledQuantity)
	}
}

func TestVenueTransientErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		call func() (Result, error)
		want error
	}{
		{
			name: "binance timeout",
			call: func() (Result, error) {
				return binanceRequestError(
					[]byte(`{"code":-1007,"msg":"Timeout waiting for response"}`),
					errors.New("upstream failure"),
				)
			},
			want: ErrUncertain,
		},
		{
			name: "binance overloaded",
			call: func() (Result, error) {
				return binanceRequestError(
					[]byte(`{"code":-1008,"msg":"Server is currently overloaded"}`),
					errors.New("upstream failure"),
				)
			},
			want: ErrRateLimited,
		},
		{
			name: "bybit transient",
			call: func() (Result, error) {
				return parseBybitOrder(
					[]byte(`{"retCode":10016,"retMsg":"Server error"}`), true,
				)
			},
			want: ErrUncertain,
		},
		{
			name: "bybit rate limit",
			call: func() (Result, error) {
				return parseBybitOrder(
					[]byte(`{"retCode":10006,"retMsg":"Too many visits"}`), true,
				)
			},
			want: ErrRateLimited,
		},
		{
			name: "bitget timeout",
			call: func() (Result, error) {
				return parseBitgetOrder(
					[]byte(`{"code":"40010","msg":"Request timed out"}`), bitgetCallPlace,
				)
			},
			want: ErrUncertain,
		},
		{
			name: "bitget query 25204 is not found",
			call: func() (Result, error) {
				return parseBitgetOrder(
					[]byte(`{"code":"25204","msg":"Order does not exist"}`), bitgetCallQuery,
				)
			},
			want: ErrOrderNotFound,
		},
		{
			name: "bitget place 25204 is uncertain",
			call: func() (Result, error) {
				return parseBitgetOrder(
					[]byte(`{"code":"25204","msg":"Order does not exist"}`), bitgetCallPlace,
				)
			},
			want: ErrUncertain,
		},
		{
			name: "bitget cancel 25204 is ambiguous",
			call: func() (Result, error) {
				return parseBitgetOrder(
					[]byte(`{"code":"25204","msg":"Order does not exist"}`), bitgetCallCancel,
				)
			},
			want: ErrAmbiguousCancel,
		},
		{
			name: "binance unknown order sent",
			call: func() (Result, error) {
				return binanceRequestError(
					[]byte(`{"code":-2011,"msg":"Unknown order sent."}`),
					errors.New("upstream failure"),
				)
			},
			want: ErrUncertain,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := test.call()
			if !errors.Is(err, test.want) || result.Status != "unknown" ||
				result.ErrorCode == "" || result.ErrorMessage == "" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestBinance2011QueriesOrderImmediately(t *testing.T) {
	const unknownBody = `{"code":-2011,"msg":"Unknown order sent."}`
	instrument := Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"}
	creds := Credentials{APIKey: "key", APISecret: "secret"}
	tests := []struct {
		name       string
		method     string
		queryBody  string
		queryCode  int
		wantStatus string
		wantFill   string
		wantErr    error
	}{
		{
			name:       "cancel filled",
			method:     http.MethodDelete,
			queryBody:  `{"orderId":42,"status":"FILLED","executedQty":"0.01","avgPrice":"100"}`,
			wantStatus: "filled",
			wantFill:   "0.01",
		},
		{
			name:       "cancel canceled zero fill",
			method:     http.MethodDelete,
			queryBody:  `{"orderId":42,"status":"CANCELED","executedQty":"0"}`,
			wantStatus: "canceled",
			wantFill:   "0",
		},
		{
			name:      "cancel query failed",
			method:    http.MethodDelete,
			queryBody: `{"code":-2013,"msg":"Order does not exist."}`,
			queryCode: http.StatusBadRequest,
			wantErr:   ErrUncertain,
		},
		{
			name:       "place still open",
			method:     http.MethodPost,
			queryBody:  `{"orderId":42,"status":"NEW","executedQty":"0"}`,
			wantStatus: "open",
			wantErr:    ErrUncertain,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(
				writer http.ResponseWriter, request *http.Request,
			) {
				requests.Add(1)
				writer.Header().Set("Content-Type", "application/json")
				if request.Method != http.MethodGet {
					if request.Method != test.method {
						t.Errorf("unexpected %s %s", request.Method, request.URL.Path)
					}
					writer.WriteHeader(http.StatusBadRequest)
					_, _ = writer.Write([]byte(unknownBody))
					return
				}
				if test.queryCode != 0 {
					writer.WriteHeader(test.queryCode)
				}
				_, _ = writer.Write([]byte(test.queryBody))
			}))
			defer server.Close()

			adapter := newBinance(server.Client(), server.URL)
			var result Result
			var err error
			if test.method == http.MethodPost {
				result, err = adapter.PlaceOrder(context.Background(), creds, OrderRequest{
					Instrument:    instrument,
					ClientOrderID: "clid-1",
					Side:          "buy",
					OrderType:     "limit",
					Quantity:      "0.01",
					Price:         "100",
				})
			} else {
				result, err = adapter.CancelAndGetOrder(context.Background(), creds, CancelRequest{
					Instrument:    instrument,
					ClientOrderID: "clid-1",
					VenueOrderID:  "42",
				})
			}
			if test.wantErr == nil {
				if err != nil || result.Status != test.wantStatus ||
					result.FilledQuantity != test.wantFill {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			} else if !errors.Is(err, test.wantErr) || errors.Is(err, ErrRejected) ||
				result.Status == "rejected" {
				t.Fatalf("result=%+v err=%v", result, err)
			} else if test.wantStatus != "" && result.Status != test.wantStatus {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if requests.Load() != 2 {
				t.Fatalf("requests=%d, want mutate plus query", requests.Load())
			}
		})
	}
}

func TestCancelOrderCommandOnlyDoesNotQuery(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		adapter func(*httptest.Server) Adapter
		request CancelRequest
		check   func(*testing.T, Result, error)
	}{
		{
			name: "binance trusted terminal",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodDelete {
					t.Errorf("unexpected %s %s", request.Method, request.URL.Path)
				}
				_, _ = writer.Write([]byte(
					`{"orderId":42,"status":"CANCELED","executedQty":"0.5","avgPrice":"100"}`,
				))
			},
			adapter: func(server *httptest.Server) Adapter {
				return newBinance(server.Client(), server.URL)
			},
			request: CancelRequest{
				Instrument:   Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
				VenueOrderID: "42",
			},
			check: func(t *testing.T, result Result, err error) {
				if err != nil || result.Status != "canceled" || result.LocalCommandAck ||
					result.FilledQuantity != "0.5" {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			},
		},
		{
			name: "binance minus 2011",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodDelete {
					t.Errorf("unexpected %s %s", request.Method, request.URL.Path)
				}
				_, _ = writer.Write([]byte(`{"code":-2011,"msg":"Unknown order sent."}`))
			},
			adapter: func(server *httptest.Server) Adapter {
				return newBinance(server.Client(), server.URL)
			},
			request: CancelRequest{
				Instrument:   Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
				VenueOrderID: "42",
			},
			check: func(t *testing.T, result Result, err error) {
				if !errors.Is(err, ErrUncertain) || result.Status != "unknown" ||
					result.ErrorCode != "-2011" || result.Status == "rejected" {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			},
		},
		{
			name: "bitget ack",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/api/v3/trade/cancel-order" {
					t.Errorf("unexpected %s %s", request.Method, request.URL.Path)
				}
				_, _ = writer.Write([]byte(`{"code":"00000","data":{"orderId":"42"}}`))
			},
			adapter: func(server *httptest.Server) Adapter {
				return newBitget(server.Client(), server.URL)
			},
			request: CancelRequest{
				Instrument:   Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
				VenueOrderID: "42",
			},
			check: func(t *testing.T, result Result, err error) {
				if err != nil || result.Status != "pending" || !result.LocalCommandAck {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			},
		},
		{
			name: "bitget 25204",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/api/v3/trade/cancel-order" {
					t.Errorf("unexpected %s %s", request.Method, request.URL.Path)
				}
				_, _ = writer.Write([]byte(`{"code":"25204","msg":"Order does not exist"}`))
			},
			adapter: func(server *httptest.Server) Adapter {
				return newBitget(server.Client(), server.URL)
			},
			request: CancelRequest{
				Instrument:   Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
				VenueOrderID: "42",
			},
			check: func(t *testing.T, result Result, err error) {
				if !errors.Is(err, ErrAmbiguousCancel) || result.Status != "unknown" {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			},
		},
		{
			name: "bybit ack",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/v5/order/cancel" {
					t.Errorf("unexpected %s", request.URL.Path)
				}
				_, _ = writer.Write([]byte(`{"retCode":0,"result":{"orderId":"42"}}`))
			},
			adapter: func(server *httptest.Server) Adapter {
				return newBybit(server.Client(), server.URL)
			},
			request: CancelRequest{
				Instrument:   Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
				VenueOrderID: "42",
			},
			check: func(t *testing.T, result Result, err error) {
				if err != nil || result.Status != "pending" || !result.LocalCommandAck {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(
				writer http.ResponseWriter, request *http.Request,
			) {
				requests.Add(1)
				writer.Header().Set("Content-Type", "application/json")
				test.handler(writer, request)
			}))
			defer server.Close()
			result, err := test.adapter(server).CancelOrder(
				context.Background(),
				Credentials{APIKey: "key", APISecret: "secret", Passphrase: "pass"},
				test.request,
			)
			test.check(t, result, err)
			if requests.Load() != 1 {
				t.Fatalf("requests=%d, want cancel only", requests.Load())
			}
		})
	}
}
