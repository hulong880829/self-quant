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

func TestBybitGetOrderFallsBackToHistory(t *testing.T) {
	historyCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		result := map[string]any{"list": []any{}}
		if request.URL.Path == "/v5/order/history" {
			historyCalls++
			result["list"] = []map[string]string{{
				"orderId": "venue-1", "orderStatus": "Filled",
				"cumExecQty": "2", "avgPrice": "100",
			}}
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"retCode": 0, "result": result})
	}))
	defer server.Close()
	result, err := newBybit(server.Client(), server.URL).GetOrder(
		context.Background(),
		Credentials{APIKey: "key", APISecret: "secret"},
		QueryRequest{
			Instrument:   Instrument{Exchange: "bybit", ContractType: "perpetual"},
			VenueOrderID: "venue-1",
		},
	)
	if err != nil || result.Status != "filled" || historyCalls != 1 {
		t.Fatalf("result=%+v historyCalls=%d err=%v", result, historyCalls, err)
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
	if lastPath != "/api/v4/futures/usdt/orders/t-sqabcdefghijklmn" {
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
	if err != nil || numeric.Status != "partially_filled" || numeric.FilledQuantity != "6" {
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
	}`), false)
	if err != nil || result.Status != "filled" || result.VenueOrderID != "1475103830940282880" ||
		result.FilledQuantity != "0.5" || result.AveragePrice != "40.12" {
		t.Fatalf("uta query=%+v err=%v", result, err)
	}

	ack, err := parseBitgetOrder([]byte(`{
		"code":"00000","data":{"orderId":"1475103830940282880","clientOid":"sqabc"}
	}`), true)
	if err != nil || ack.Status != "pending" || ack.VenueOrderID != "1475103830940282880" {
		t.Fatalf("place ack=%+v err=%v", ack, err)
	}

	legacy, err := parseBitgetOrder([]byte(`{
		"code":"00000","data":{"orderId":"bg-1","status":"filled","baseVolume":"1","priceAvg":"2"}
	}`), false)
	if err != nil || legacy.Status != "filled" || legacy.FilledQuantity != "1" || legacy.AveragePrice != "2" {
		t.Fatalf("legacy=%+v err=%v", legacy, err)
	}
}

func TestBitgetMarketPerpetualUsesIOCAndPosSide(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		body = string(raw)
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"code": "00000", "data": map[string]string{"orderId": "bg-1", "status": "filled"},
		})
	}))
	defer server.Close()
	_, err := newBitget(server.Client(), server.URL).PlaceOrder(context.Background(), Credentials{
		APIKey: "key", APISecret: "secret", Passphrase: "phrase",
	}, OrderRequest{
		Instrument:    Instrument{ContractType: "perpetual", ExchangeSymbol: "HYPEUSDT", SettleAsset: "USDT"},
		ClientOrderID: "clid", Side: "sell", OrderType: "market", Quantity: "0.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"orderType":"market"`, `"timeInForce":"ioc"`, `"posSide":"short"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body=%s missing %s", body, want)
		}
	}

	_, err = newBitget(server.Client(), server.URL).PlaceOrder(context.Background(), Credentials{
		APIKey: "key", APISecret: "secret", Passphrase: "phrase",
	}, OrderRequest{
		Instrument:    Instrument{ContractType: "spot", ExchangeSymbol: "HYPEUSDT"},
		ClientOrderID: "clid", Side: "sell", OrderType: "market", Quantity: "0.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, `"posSide"`) {
		t.Fatalf("spot should omit posSide: %s", body)
	}
}

func TestGateIOCStatusUsesFillAmount(t *testing.T) {
	if status := gateFuturesStatus("finished", "ioc", 10, 0); status != "filled" {
		t.Fatalf("full IOC status=%s", status)
	}
	if status := gateFuturesStatus("finished", "ioc", 10, 4); status != "partially_filled" {
		t.Fatalf("partial IOC status=%s", status)
	}
	if status := gateFuturesStatus("finished", "ioc", 10, 10); status != "canceled" {
		t.Fatalf("empty IOC status=%s", status)
	}
}
