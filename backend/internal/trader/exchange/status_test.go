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
	"time"

	"github.com/shopspring/decimal"
)

func TestBybitGetOrderUsesRealtimeOnly(t *testing.T) {
	historyCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v5/order/history" {
			historyCalls++
		}
		result := map[string]any{"list": []any{}}
		if request.URL.Path == "/v5/order/realtime" {
			result["list"] = []map[string]string{{
				"orderId": "venue-1", "orderStatus": "New",
			}}
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"retCode": 0, "result": result})
	}))
	defer server.Close()
	result, err := newBybit(server.Client(), server.URL).GetOrder(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret"},
		QueryRequest{
			Instrument: Instrument{
				Exchange: "bybit", ContractType: "perpetual",
				ExchangeSymbol: "BTCUSDT",
			},
			VenueOrderID: "venue-1",
		},
	)
	if err != nil || result.Status != "open" || historyCalls != 0 {
		t.Fatalf("result=%+v historyCalls=%d err=%v", result, historyCalls, err)
	}
}

func TestBybitGetOrderEmptyRealtimeIsNotFound(t *testing.T) {
	historyCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v5/order/history" {
			historyCalls++
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"retCode": 0, "result": map[string]any{"list": []any{}},
		})
	}))
	defer server.Close()
	_, err := newBybit(server.Client(), server.URL).GetOrder(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret"},
		QueryRequest{
			Instrument: Instrument{
				Exchange: "bybit", ContractType: "perpetual",
				ExchangeSymbol: "BTCUSDT",
			},
			VenueOrderID: "venue-1",
		},
	)
	if !errors.Is(err, ErrOrderNotFound) || historyCalls != 0 {
		t.Fatalf("err=%v historyCalls=%d", err, historyCalls)
	}
}

func TestParseBybitPartiallyFilledCanceledIsTerminalWithFill(t *testing.T) {
	result, err := parseBybitOrder([]byte(`{
		"retCode":0,
		"result":{"list":[{
			"orderId":"venue-1","orderStatus":"PartiallyFilledCanceled",
			"cumExecQty":"0.4","avgPrice":"100",
			"rejectReason":"EC_NoError","cancelType":"CancelByUser"
		}]}
	}`), false)
	if err != nil || result.Status != "canceled" ||
		result.FilledQuantity != "0.4" ||
		result.ErrorCode != "" ||
		result.ErrorMessage != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestParseBinanceSpotAverageAndLargeOrderID(t *testing.T) {
	result, err := parseBinanceOrder([]byte(`{
		"orderId":9223372036854775807,
		"status":"FILLED",
		"executedQty":"2",
		"cummulativeQuoteQty":"201",
		"avgPrice":"0"
	}`))
	if err != nil || result.VenueOrderID != "9223372036854775807" ||
		result.AveragePrice != "100.5" || result.Status != "filled" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestGateLookupUsesClientText(t *testing.T) {
	var lastPath string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		lastPath = request.URL.Path
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"id": 11, "status": "finished", "finish_as": "filled", "size": 1, "left": 0, "fill_price": "100",
		})
	}))
	defer server.Close()
	adapter := newGate(server.Client(), server.URL)
	creds := Credentials{APIKey: "key", APISecret: "secret"}
	instrument := Instrument{ContractType: "perpetual", BaseAsset: "BTC", QuoteAsset: "USDT", ContractSize: "0.001"}

	if _, err := adapter.GetOrder(context.Background(), creds, QueryRequest{
		Instrument: instrument, ClientOrderID: "sqabcdefghijklmnop",
	}); err != nil {
		t.Fatal(err)
	}
	if lastPath != "/api/v4/futures/usdt/orders/t-sqabcdefghijklmnop" {
		t.Fatalf("client lookup path=%s", lastPath)
	}

	if _, err := adapter.GetOrder(context.Background(), creds, QueryRequest{
		Instrument: instrument, VenueOrderID: "venue-9", ClientOrderID: "sqabcdefghijklmnop",
	}); err != nil {
		t.Fatal(err)
	}
	if lastPath != "/api/v4/futures/usdt/orders/venue-9" {
		t.Fatalf("venue lookup path=%s", lastPath)
	}
}

func TestParseGateFuturesAcceptsStringSizeAndLeft(t *testing.T) {
	instrument := Instrument{ContractType: "perpetual", BaseAsset: "HYPE", QuoteAsset: "USDT", ContractSize: "1"}
	result, err := parseGateFutures([]byte(`{
		"id": 99, "status": "finished", "finish_as": "filled",
		"size": "-1", "left": "0", "fill_price": "40.1"
	}`), instrument)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "filled" || result.VenueOrderID != "99" || result.FilledQuantity != "1" {
		t.Fatalf("result=%+v", result)
	}

	numeric, err := parseGateFutures([]byte(`{
		"id": "88", "status": "finished", "finish_as": "ioc",
		"size": 10, "left": 4, "fill_price": "1"
	}`), instrument)
	if err != nil || numeric.Status != "canceled" || numeric.FilledQuantity != "6" {
		t.Fatalf("numeric=%+v err=%v", numeric, err)
	}
}

func TestParseBitgetOrderUsesUTAOrderInfoFields(t *testing.T) {
	result, err := parseBitgetOrder([]byte(`{
		"code":"00000","msg":"success",
		"data":{
			"orderId":"1475103830940282880",
			"orderStatus":"filled",
			"cumExecQty":"0.5",
			"avgPrice":"40.12"
		}
	}`), bitgetCallQuery)
	if err != nil || result.Status != "filled" || result.VenueOrderID != "1475103830940282880" ||
		result.FilledQuantity != "0.5" || result.AveragePrice != "40.12" {
		t.Fatalf("uta query=%+v err=%v", result, err)
	}

	ack, err := parseBitgetOrder([]byte(`{
		"code":"00000","data":{"orderId":"1475103830940282880","clientOid":"sqabc"}
	}`), bitgetCallPlace)
	if err != nil || ack.Status != "pending" || ack.VenueOrderID != "1475103830940282880" {
		t.Fatalf("place ack=%+v err=%v", ack, err)
	}

	legacy, err := parseBitgetOrder([]byte(`{
		"code":"00000","data":{"orderId":"bg-1","status":"filled","baseVolume":"1","priceAvg":"2"}
	}`), bitgetCallQuery)
	if err != nil || legacy.Status != "filled" || legacy.FilledQuantity != "1" || legacy.AveragePrice != "2" {
		t.Fatalf("legacy=%+v err=%v", legacy, err)
	}
}

func TestBitgetOneWayPayloads(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		body = string(raw)
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"code": "00000", "data": map[string]string{"orderId": "bg-1", "status": "filled"},
		})
	}))
	defer server.Close()
	adapter := newBitget(server.Client(), server.URL)
	creds := Credentials{APIKey: "key", APISecret: "secret", Passphrase: "phrase"}
	if _, err := adapter.PlaceOrder(context.Background(), creds, OrderRequest{
		Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "HYPEUSDT", SettleAsset: "USDT"},
		ClientOrderID: "clid", Side: "sell", OrderType: "market", Quantity: "0.5",
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"orderType":"market"`, `"marginMode":"crossed"`, `"reduceOnly":"no"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body=%s missing %s", body, want)
		}
	}
	for _, banned := range []string{`"posSide"`, `"timeInForce"`} {
		if strings.Contains(body, banned) {
			t.Fatalf("body=%s unexpectedly contains %s", body, banned)
		}
	}
	if _, err := adapter.PlaceOrder(context.Background(), creds, OrderRequest{
		Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "HYPEUSDT", SettleAsset: "USDT"},
		ClientOrderID: "clid", Side: "buy", OrderType: "limit", Quantity: "0.5", Price: "1",
		ReduceOnly: true,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"reduceOnly":"yes"`) || strings.Contains(body, `"posSide"`) {
		t.Fatalf("reduce-only body=%s", body)
	}
	if _, err := adapter.PlaceOrder(context.Background(), creds, OrderRequest{
		Instrument:    Instrument{ContractType: "spot", ExchangeSymbol: "HYPEUSDT"},
		ClientOrderID: "clid", Side: "sell", OrderType: "market", Quantity: "0.5",
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, `"posSide"`) || strings.Contains(body, `"marginMode"`) ||
		strings.Contains(body, `"reduceOnly"`) {
		t.Fatalf("spot should omit futures fields: %s", body)
	}
}

func TestGateIOCStatusUsesFillAmount(t *testing.T) {
	if status := gateFuturesStatus("finished", "ioc", decimal.NewFromInt(10), decimal.Zero); status != "filled" {
		t.Fatalf("full IOC status=%s", status)
	}
	if status := gateFuturesStatus("finished", "ioc", decimal.NewFromInt(10), decimal.NewFromInt(4)); status != "canceled" {
		t.Fatalf("partial IOC status=%s", status)
	}
	if status := gateFuturesStatus("finished", "ioc", decimal.NewFromInt(10), decimal.NewFromInt(10)); status != "canceled" {
		t.Fatalf("empty IOC status=%s", status)
	}
}

func TestParseGateSpotUsesBaseFillAndPreservesLargeOrderID(t *testing.T) {
	result, err := parseGateSpot([]byte(`{
		"id": 9223372036854775807,
		"status": "closed",
		"finish_as": "filled",
		"filled_amount": "12.5",
		"filled_total": "1.25",
		"avg_deal_price": "0.1"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.VenueOrderID != "9223372036854775807" ||
		result.FilledQuantity != "12.5" || result.AveragePrice != "0.1" ||
		result.Status != "filled" {
		t.Fatalf("result=%+v", result)
	}
}

func TestPartialIOCResultsAreTerminalAcrossVenues(t *testing.T) {
	instrument := Instrument{
		ContractType: "perpetual", ContractSize: "1",
		BaseAsset: "BTC", QuoteAsset: "USDT",
	}
	tests := []struct {
		name  string
		parse func() (Result, error)
	}{
		{
			name: "binance",
			parse: func() (Result, error) {
				return parseBinanceOrder([]byte(`{
					"orderId":1,"status":"EXPIRED","executedQty":"0.4","avgPrice":"100"
				}`))
			},
		},
		{
			name: "okx",
			parse: func() (Result, error) {
				return parseOKXOrder([]byte(`{
					"code":"0","data":[{"ordId":"2","state":"canceled","accFillSz":"0.4","avgPx":"100"}]
				}`), instrument, false)
			},
		},
		{
			name: "bybit",
			parse: func() (Result, error) {
				return parseBybitOrder([]byte(`{
					"retCode":0,"result":{"list":[{"orderId":"3",
					"orderStatus":"PartiallyFilledCanceled","cumExecQty":"0.4","avgPrice":"100"}]}
				}`), false)
			},
		},
		{
			name: "bitget",
			parse: func() (Result, error) {
				return parseBitgetOrder([]byte(`{
					"code":"00000","data":{"orderId":"4","orderStatus":"canceled",
					"cumExecQty":"0.4","avgPrice":"100"}
				}`), bitgetCallQuery)
			},
		},
		{
			name: "gate",
			parse: func() (Result, error) {
				return parseGateFutures([]byte(`{
					"id":"5","status":"finished","finish_as":"ioc",
					"size":"1","left":"0.6","fill_price":"100"
				}`), instrument)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := test.parse()
			if err != nil {
				t.Fatal(err)
			}
			if !terminalOrderStatus(result.Status) ||
				result.FilledQuantity != "0.4" {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestGateOrderNotFoundHasDedicatedClassification(t *testing.T) {
	result, err := gateRequestError(
		[]byte(`{"label":"ORDER_NOT_FOUND","message":"order not found"}`),
		errors.New("http 404"),
	)
	if !errors.Is(err, ErrOrderNotFound) || errors.Is(err, ErrRejected) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.Status != "unknown" || result.ErrorCode != "" {
		t.Fatalf("result=%+v", result)
	}
}

func TestBinanceOrderNotFoundHasDedicatedClassification(t *testing.T) {
	result, err := binanceRequestError(
		[]byte(`{"code":-2013,"msg":"Order does not exist."}`),
		errors.New("http 400"),
	)
	if !errors.Is(err, ErrOrderNotFound) || errors.Is(err, ErrRejected) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.Status != "unknown" || result.ErrorCode != "-2013" {
		t.Fatalf("result=%+v", result)
	}
}

func TestBinanceUnknownOrderSentIsUncertain(t *testing.T) {
	result, err := binanceRequestError(
		[]byte(`{"code":-2011,"msg":"Unknown order sent."}`),
		errors.New("http 400"),
	)
	if !errors.Is(err, ErrUncertain) || errors.Is(err, ErrRejected) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.Status != "unknown" || result.ErrorCode != "-2011" {
		t.Fatalf("result=%+v", result)
	}
}

func TestBinanceResolveOrderUsesExactOrderLookup(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.URL.Path != "/papi/v1/um/order" {
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
		if request.URL.Query().Get("origClientOrderId") != "client-1" {
			t.Fatalf("query=%s", request.URL.RawQuery)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"orderId": "venue-1", "clientOrderId": "client-1",
			"status": "CANCELED", "executedQty": "0",
		})
	}))
	defer server.Close()
	resolver := newBinance(server.Client(), server.URL).(OrderResolver)
	resolution, err := resolver.ResolveOrder(
		context.Background(), Credentials{APIKey: "key", APISecret: "secret"},
		QueryRequest{
			Instrument: Instrument{
				ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
			},
			ClientOrderID: "client-1",
		},
	)
	if err != nil || !resolution.Found || resolution.Active ||
		resolution.Result.Status != "canceled" || calls != 1 {
		t.Fatalf("resolution=%+v calls=%d err=%v", resolution, calls, err)
	}
}

func TestBybitResolveOrderKeepsExpectedZeroFillTerminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v5/order/history":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"retCode": 0,
				"result": map[string]any{"list": []map[string]any{{
					"orderId": "venue-1", "orderStatus": "Cancelled",
					"cumExecQty": "0", "rejectReason": "EC_NoImmediateQtyToFill",
				}}},
			})
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	resolver := newBybit(server.Client(), server.URL).(OrderResolver)
	resolution, err := resolver.ResolveOrder(
		context.Background(), Credentials{APIKey: "key", APISecret: "secret"},
		QueryRequest{
			Instrument: Instrument{
				ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
			},
			VenueOrderID: "venue-1",
			CreatedAt:    time.Now().UTC().Add(-time.Minute),
		},
	)
	if err != nil || !resolution.Found || resolution.Active ||
		resolution.Result.Status != "canceled" ||
		resolution.Result.FilledQuantity != "0" ||
		resolution.Result.ErrorCode != "EC_NoImmediateQtyToFill" {
		t.Fatalf("resolution=%+v err=%v", resolution, err)
	}
}

func TestGateResolveOrderUsesExactOrderLookup(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/api/v4/futures/usdt/orders/venue-1" {
				t.Fatalf("unexpected path %s", request.URL.Path)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"id": "venue-1", "text": "t-client-1", "status": "finished",
				"finish_as": "ioc", "size": "10", "left": "4", "fill_price": "100",
			})
		}))
		defer server.Close()
		resolver := newGate(server.Client(), server.URL).(OrderResolver)
		resolution, err := resolver.ResolveOrder(
			context.Background(), Credentials{APIKey: "key", APISecret: "secret"},
			QueryRequest{
				Instrument: Instrument{
					ContractType: "perpetual", BaseAsset: "BTC", QuoteAsset: "USDT",
					ContractSize: "1",
				},
				VenueOrderID: "venue-1", ClientOrderID: "client-1",
			},
		)
		if err != nil || !resolution.Found || resolution.Active ||
			resolution.Result.Status != "canceled" ||
			resolution.Result.FilledQuantity != "6" {
			t.Fatalf("resolution=%+v err=%v", resolution, err)
		}
	})

	t.Run("not found is confirmed absent", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(writer).Encode(map[string]string{
				"label": "ORDER_NOT_FOUND", "message": "order not found",
			})
		}))
		defer server.Close()
		resolver := newGate(server.Client(), server.URL).(OrderResolver)
		resolution, err := resolver.ResolveOrder(
			context.Background(), Credentials{APIKey: "key", APISecret: "secret"},
			QueryRequest{
				Instrument: Instrument{
					ContractType: "perpetual", BaseAsset: "BTC", QuoteAsset: "USDT",
					ContractSize: "1",
				},
				VenueOrderID: "missing",
			},
		)
		if err != nil || !resolution.ConfirmedAbsent || resolution.Found {
			t.Fatalf("resolution=%+v err=%v", resolution, err)
		}
	})

	t.Run("closed is not confirmed absent", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(writer).Encode(map[string]string{
				"label": "ORDER_CLOSED", "message": "order finished",
			})
		}))
		defer server.Close()
		resolver := newGate(server.Client(), server.URL).(OrderResolver)
		resolution, err := resolver.ResolveOrder(
			context.Background(), Credentials{APIKey: "key", APISecret: "secret"},
			QueryRequest{
				Instrument: Instrument{
					ContractType: "perpetual", BaseAsset: "BTC", QuoteAsset: "USDT",
				},
				VenueOrderID: "closed",
			},
		)
		if !errors.Is(err, ErrUncertain) || resolution.ConfirmedAbsent {
			t.Fatalf("resolution=%+v err=%v", resolution, err)
		}
	})
}

func TestGateResolveOrderDoesNotTreatQueryFailureAsAbsent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(writer).Encode(map[string]string{
			"label": "INVALID_KEY", "message": "invalid api key",
		})
	}))
	defer server.Close()
	resolver := newGate(server.Client(), server.URL).(OrderResolver)
	resolution, err := resolver.ResolveOrder(
		context.Background(), Credentials{APIKey: "key", APISecret: "secret"},
		QueryRequest{
			Instrument: Instrument{
				ContractType: "perpetual", BaseAsset: "BTC", QuoteAsset: "USDT",
			},
			VenueOrderID: "target",
		},
	)
	if err == nil || resolution.ConfirmedAbsent {
		t.Fatalf("resolution=%+v err=%v", resolution, err)
	}
}
