package exchange

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBinanceSpotAndPerpetualOrders(t *testing.T) {
	var lastPath, lastQuery, lastMethod string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		lastPath, lastMethod = request.URL.Path, request.Method
		lastQuery = request.URL.RawQuery
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"orderId": 11, "status": "NEW", "executedQty": "0", "avgPrice": "0",
		})
	}))
	defer server.Close()
	adapter := newBinance(server.Client(), server.URL)
	creds := Credentials{APIKey: "key", APISecret: "secret"}
	spot, err := adapter.PlaceOrder(context.Background(), creds, OrderRequest{
		Instrument:    Instrument{ContractType: "spot", ExchangeSymbol: "BTCUSDT"},
		ClientOrderID: "sqabc", Side: "buy", OrderType: "limit", Quantity: "0.01", Price: "100",
	})
	if err != nil || lastPath != "/papi/v1/margin/order" || lastMethod != http.MethodPost {
		t.Fatalf("spot=%+v path=%s method=%s err=%v", spot, lastPath, lastMethod, err)
	}
	if !strings.Contains(lastQuery, "sideEffectType=NO_SIDE_EFFECT") || !strings.Contains(lastQuery, "signature=") {
		t.Fatalf("query=%s", lastQuery)
	}
	perp, err := adapter.PlaceOrder(context.Background(), creds, OrderRequest{
		Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
		ClientOrderID: "sqabc", Side: "sell", OrderType: "market", Quantity: "0.01",
	})
	if err != nil || lastPath != "/papi/v1/um/order" || strings.Contains(lastQuery, "price=") {
		t.Fatalf("perp=%+v path=%s query=%s err=%v", perp, lastPath, lastQuery, err)
	}
}

func TestOKXBybitBitgetGatePlaceBodies(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		seen = append(seen, request.Method+" "+request.URL.Path+" "+string(body))
		switch {
		case strings.Contains(request.URL.Path, "/api/v5/"):
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"code": "0", "data": []map[string]string{{"ordId": "okx-1", "sCode": "0", "state": "live"}},
			})
		case strings.Contains(request.URL.Path, "/v5/order"):
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"retCode": 0, "result": map[string]string{"orderId": "bybit-1", "orderStatus": "New"},
			})
		case strings.Contains(request.URL.Path, "/api/v3/"):
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"code": "00000", "data": map[string]string{"orderId": "bg-1", "status": "live"},
			})
		default:
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": "gate-1", "status": "open"})
		}
	}))
	defer server.Close()
	creds := Credentials{APIKey: "key", APISecret: "secret", Passphrase: "phrase"}
	okx := newOKX(server.Client(), server.URL)
	if _, err := okx.PlaceOrder(context.Background(), creds, OrderRequest{
		Instrument:    Instrument{ContractType: "spot", BaseAsset: "BTC", QuoteAsset: "USDT"},
		ClientOrderID: "clid", Side: "buy", OrderType: "limit", Quantity: "0.01", Price: "100",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := okx.PlaceOrder(context.Background(), creds, OrderRequest{
		Instrument:    Instrument{ContractType: "perpetual", BaseAsset: "BTC", QuoteAsset: "USDT"},
		ClientOrderID: "clid", Side: "buy", OrderType: "market", Quantity: "1",
	}); err != nil {
		t.Fatal(err)
	}
	bybit := newBybit(server.Client(), server.URL)
	if _, err := bybit.PlaceOrder(context.Background(), creds, OrderRequest{
		Instrument:    Instrument{ContractType: "spot", ExchangeSymbol: "BTCUSDT"},
		ClientOrderID: "clid", Side: "buy", OrderType: "market", Quantity: "0.01",
	}); err != nil {
		t.Fatal(err)
	}
	bitget := newBitget(server.Client(), server.URL)
	if _, err := bitget.PlaceOrder(context.Background(), creds, OrderRequest{
		Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT", SettleAsset: "USDT"},
		ClientOrderID: "clid", Side: "sell", OrderType: "limit", Quantity: "0.01", Price: "100",
	}); err != nil {
		t.Fatal(err)
	}
	gate := newGate(server.Client(), server.URL)
	if _, err := gate.PlaceOrder(context.Background(), creds, OrderRequest{
		Instrument:    Instrument{ContractType: "spot", BaseAsset: "BTC", QuoteAsset: "USDT"},
		ClientOrderID: "clid1234567890ab", Side: "buy", OrderType: "market", Quantity: "0.01",
	}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(seen, "\n")
	for _, want := range []string{
		`"tdMode":"cash"`, `"tdMode":"cross"`, `"instId":"BTC-USDT-SWAP"`,
		`"category":"spot"`, `"marketUnit":"baseCoin"`, `"category":"USDT-FUTURES"`,
		`"account":"unified"`, `"text":"t-clid1234567890ab"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %s", want, joined)
		}
	}
}

func TestAdaptersRejectVenueErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"code":-2010,"msg":"insufficient"}`))
	}))
	defer server.Close()
	_, err := newBinance(server.Client(), server.URL).PlaceOrder(context.Background(), Credentials{APIKey: "k", APISecret: "s"}, OrderRequest{
		Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
		ClientOrderID: "id", Side: "buy", OrderType: "market", Quantity: "1",
	})
	if err == nil {
		t.Fatal("expected rejection")
	}
}

func TestAdapterBBOHTTPContracts(t *testing.T) {
	type testCase struct {
		name       string
		newAdapter func(*http.Client, string) Adapter
		instrument Instrument
		wantPath   string
		wantQuery  string
		response   string
	}
	tests := []testCase{
		{
			name: "binance spot", newAdapter: newBinance,
			instrument: Instrument{ContractType: "spot", ExchangeSymbol: "BTCUSDT"},
			wantPath:   "/api/v3/ticker/bookTicker", wantQuery: "symbol=BTCUSDT",
			response: `{"bidPrice":"100","askPrice":"101"}`,
		},
		{
			name: "binance perpetual", newAdapter: newBinance,
			instrument: Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
			wantPath:   "/fapi/v1/ticker/bookTicker", wantQuery: "symbol=BTCUSDT",
			response: `{"bidPrice":"100","askPrice":"101"}`,
		},
		{
			name: "okx spot", newAdapter: newOKX,
			instrument: Instrument{ContractType: "spot", BaseAsset: "BTC", QuoteAsset: "USDT"},
			wantPath:   "/api/v5/market/ticker", wantQuery: "instId=BTC-USDT",
			response: `{"code":"0","data":[{"bidPx":"100","askPx":"101","ts":"1700000000000"}]}`,
		},
		{
			name: "okx perpetual", newAdapter: newOKX,
			instrument: Instrument{ContractType: "perpetual", BaseAsset: "BTC", QuoteAsset: "USDT"},
			wantPath:   "/api/v5/market/ticker", wantQuery: "instId=BTC-USDT-SWAP",
			response: `{"code":"0","data":[{"bidPx":"100","askPx":"101","ts":"1700000000000"}]}`,
		},
		{
			name: "bybit spot", newAdapter: newBybit,
			instrument: Instrument{ContractType: "spot", ExchangeSymbol: "BTCUSDT"},
			wantPath:   "/v5/market/tickers", wantQuery: "category=spot&symbol=BTCUSDT",
			response: `{"retCode":0,"time":1700000000000,"result":{"list":[{"bid1Price":"100","ask1Price":"101"}]}}`,
		},
		{
			name: "bybit perpetual", newAdapter: newBybit,
			instrument: Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"},
			wantPath:   "/v5/market/tickers", wantQuery: "category=linear&symbol=BTCUSDT",
			response: `{"retCode":0,"time":1700000000000,"result":{"list":[{"bid1Price":"100","ask1Price":"101"}]}}`,
		},
		{
			name: "bitget spot", newAdapter: newBitget,
			instrument: Instrument{ContractType: "spot", ExchangeSymbol: "BTCUSDT"},
			wantPath:   "/api/v3/market/tickers", wantQuery: "category=SPOT&symbol=BTCUSDT",
			response: `{"code":"00000","requestTime":1700000000000,"data":[{"bid1Price":"100","ask1Price":"101"}]}`,
		},
		{
			name: "bitget perpetual", newAdapter: newBitget,
			instrument: Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT", SettleAsset: "USDT"},
			wantPath:   "/api/v3/market/tickers", wantQuery: "category=USDT-FUTURES&symbol=BTCUSDT",
			response: `{"code":"00000","requestTime":1700000000000,"data":[{"bid1Price":"100","ask1Price":"101"}]}`,
		},
		{
			name: "gate spot", newAdapter: newGate,
			instrument: Instrument{ContractType: "spot", BaseAsset: "BTC", QuoteAsset: "USDT"},
			wantPath:   "/api/v4/spot/order_book", wantQuery: "currency_pair=BTC_USDT&limit=1",
			response: `{"current":1700000000.5,"bids":[["100","1"]],"asks":[["101","1"]]}`,
		},
		{
			name: "gate perpetual", newAdapter: newGate,
			instrument: Instrument{ContractType: "perpetual", BaseAsset: "BTC", QuoteAsset: "USD", SettleAsset: "BTC"},
			wantPath:   "/api/v4/futures/btc/order_book", wantQuery: "contract=BTC_USD&limit=1",
			response: `{"current":1700000000.5,"bids":[{"p":"100","s":1}],"asks":[{"p":"101","s":1}]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.URL.Path != test.wantPath || request.URL.RawQuery != test.wantQuery {
					t.Errorf("request=%s %s?%s", request.Method, request.URL.Path, request.URL.RawQuery)
				}
				_, _ = writer.Write([]byte(test.response))
			}))
			defer server.Close()
			got, err := test.newAdapter(server.Client(), server.URL).GetBBO(context.Background(), test.instrument)
			if err != nil {
				t.Fatal(err)
			}
			if got.BidPrice != "100" || got.AskPrice != "101" || got.Timestamp.IsZero() {
				t.Fatalf("BBO=%+v", got)
			}
		})
	}
}

func TestAdapterOrderPolicyHTTPContracts(t *testing.T) {
	var requestTarget string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		requestTarget = request.URL.RawQuery + "\n" + string(body)
		switch {
		case strings.Contains(request.URL.Path, "/papi/"):
			_, _ = writer.Write([]byte(`{"orderId":1,"status":"NEW"}`))
		case strings.Contains(request.URL.Path, "/api/v5/"):
			_, _ = writer.Write([]byte(`{"code":"0","data":[{"ordId":"1","sCode":"0"}]}`))
		case strings.Contains(request.URL.Path, "/v5/"):
			_, _ = writer.Write([]byte(`{"retCode":0,"result":{"orderId":"1"}}`))
		case strings.Contains(request.URL.Path, "/api/v3/"):
			_, _ = writer.Write([]byte(`{"code":"00000","data":{"orderId":"1"}}`))
		case strings.Contains(request.URL.Path, "/spot/"):
			_, _ = writer.Write([]byte(`{"id":"1","status":"open"}`))
		default:
			_, _ = writer.Write([]byte(`{"id":1,"status":"open","size":1,"left":1}`))
		}
	}))
	defer server.Close()

	type testCase struct {
		name       string
		adapter    Adapter
		instrument Instrument
		policy     OrderRequest
		want       string
	}
	tests := []testCase{
		{"binance spot post-only", newBinance(server.Client(), server.URL), Instrument{ContractType: "spot", ExchangeSymbol: "BTCUSDT"}, OrderRequest{PostOnly: true}, "type=LIMIT_MAKER"},
		{"binance perpetual IOC", newBinance(server.Client(), server.URL), Instrument{ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"}, OrderRequest{TimeInForce: "IOC"}, "timeInForce=IOC"},
		{"okx post-only", newOKX(server.Client(), server.URL), Instrument{ContractType: "spot", BaseAsset: "BTC", QuoteAsset: "USDT"}, OrderRequest{PostOnly: true}, `"ordType":"post_only"`},
		{"okx IOC", newOKX(server.Client(), server.URL), Instrument{ContractType: "spot", BaseAsset: "BTC", QuoteAsset: "USDT"}, OrderRequest{TimeInForce: "IOC"}, `"ordType":"ioc"`},
		{"bybit post-only", newBybit(server.Client(), server.URL), Instrument{ContractType: "spot", ExchangeSymbol: "BTCUSDT"}, OrderRequest{PostOnly: true}, `"timeInForce":"PostOnly"`},
		{"bybit IOC", newBybit(server.Client(), server.URL), Instrument{ContractType: "spot", ExchangeSymbol: "BTCUSDT"}, OrderRequest{TimeInForce: "IOC"}, `"timeInForce":"IOC"`},
		{"bitget post-only", newBitget(server.Client(), server.URL), Instrument{ContractType: "spot", ExchangeSymbol: "BTCUSDT"}, OrderRequest{PostOnly: true}, `"timeInForce":"post_only"`},
		{"bitget IOC", newBitget(server.Client(), server.URL), Instrument{ContractType: "spot", ExchangeSymbol: "BTCUSDT"}, OrderRequest{TimeInForce: "IOC"}, `"timeInForce":"ioc"`},
		{"gate post-only", newGate(server.Client(), server.URL), Instrument{ContractType: "spot", BaseAsset: "BTC", QuoteAsset: "USDT"}, OrderRequest{PostOnly: true}, `"time_in_force":"poc"`},
		{"gate IOC", newGate(server.Client(), server.URL), Instrument{ContractType: "spot", BaseAsset: "BTC", QuoteAsset: "USDT"}, OrderRequest{TimeInForce: "IOC"}, `"time_in_force":"ioc"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := test.policy
			request.Instrument = test.instrument
			request.ClientOrderID = "policy-test"
			request.Side = "buy"
			request.OrderType = "limit"
			request.Quantity = "1"
			request.Price = "100"
			if _, err := test.adapter.PlaceOrder(context.Background(), Credentials{APIKey: "key", APISecret: "secret"}, request); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(requestTarget, test.want) {
				t.Fatalf("missing %q in %s", test.want, requestTarget)
			}
		})
	}
}
