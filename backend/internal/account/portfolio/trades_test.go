package portfolio

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTradeHTTPRetriesRateLimit(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		calls++
		if calls == 1 {
			writer.Header().Set("Retry-After", "0")
			http.Error(writer, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = fmt.Fprint(writer, `{"ok":true}`)
	}))
	defer server.Close()
	var target struct {
		OK bool `json:"ok"`
	}
	if err := getJSON(context.Background(), server.Client(), server.URL, nil, &target); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || !target.OK {
		t.Fatalf("calls=%d target=%+v", calls, target)
	}
}

func TestTradeAdaptersNormalizeSignedFixtures(t *testing.T) {
	tests := []struct {
		name    string
		adapter func(*http.Client, string) Adapter
		handler func(*testing.T, http.ResponseWriter, *http.Request)
	}{
		{"binance", newBinance, binanceTradeFixture},
		{"okx", newOKX, okxTradeFixture},
		{"bitget", newBitget, bitgetTradeFixture},
		{"bybit", newBybit, bybitTradeFixture},
		{"gate", newGate, gateTradeFixture},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(
				writer http.ResponseWriter,
				request *http.Request,
			) {
				test.handler(t, writer, request)
			}))
			defer server.Close()
			adapter := test.adapter(server.Client(), server.URL).(TradeAdapter)
			fills, err := adapter.TradeFills(context.Background(), Credentials{
				APIKey: "key", APISecret: "secret", Passphrase: "pass",
			}, TradeQuery{
				Since: time.Unix(1_700_000_000, 0),
				Until: time.Unix(1_700_003_600, 0),
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(fills) == 0 {
				t.Fatal("normalized fill missing")
			}
			fill := fills[0]
			if fill.ExternalTradeID == "" || fill.Symbol == "" ||
				fill.Price != "10" || fill.Quantity != "2" ||
				fill.QuoteNotionalUSD != "20" || fill.Side != "buy" {
				t.Fatalf("fill=%+v", fill)
			}
		})
	}
}

func binanceTradeFixture(t *testing.T, writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-MBX-APIKEY") != "key" ||
		request.URL.Query().Get("signature") == "" ||
		request.URL.Query().Get("startTime") == "" {
		t.Error("binance trade request was not signed or bounded")
	}
	writeFixture(writer, `[{"id":1,"orderId":2,"symbol":"BTCUSDT","side":"BUY",`+
		`"price":"10","qty":"2","quoteQty":"20","commission":"0.01",`+
		`"commissionAsset":"USDT","time":1700000100000}]`)
}

func okxTradeFixture(t *testing.T, writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("OK-ACCESS-SIGN") == "" ||
		request.URL.Path != "/api/v5/trade/fills-history" {
		t.Error("okx fill request was not signed")
	}
	writeFixture(writer, `{"code":"0","data":[{"tradeId":"1","ordId":"2",`+
		`"instId":"BTC-USDT","side":"buy","fillPx":"10","fillSz":"2",`+
		`"fillFee":"-0.01","fillFeeCcy":"USDT","ts":"1700000100000"}]}`)
}

func bitgetTradeFixture(t *testing.T, writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("ACCESS-SIGN") == "" ||
		request.URL.Path != "/api/v3/trade/fills" {
		t.Error("bitget fill request was not signed")
	}
	writeFixture(writer, `{"code":"00000","data":{"list":[{"tradeId":"1",`+
		`"orderId":"2","symbol":"BTCUSDT","side":"buy","price":"10","size":"2",`+
		`"amount":"20","fee":"0.01","feeCoin":"USDT","cTime":"1700000100000"}]}}`)
}

func bybitTradeFixture(t *testing.T, writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-BAPI-SIGN") == "" ||
		request.URL.Path != "/v5/execution/list" {
		t.Error("bybit execution request was not signed")
	}
	writeFixture(writer, `{"retCode":0,"result":{"list":[{"execId":"1",`+
		`"orderId":"2","symbol":"BTCUSDT","side":"Buy","execPrice":"10",`+
		`"execQty":"2","execValue":"20","execFee":"0.01",`+
		`"feeCurrency":"USDT","execTime":"1700000100000"}]}}`)
}

func gateTradeFixture(t *testing.T, writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("SIGN") == "" || request.Header.Get("KEY") != "key" {
		t.Error("gate trade request was not signed")
	}
	if strings.Contains(request.URL.Path, "/futures/") &&
		strings.Contains(request.URL.Path, "/btc/") {
		writer.WriteHeader(http.StatusBadRequest)
		writeFixture(writer, `{"label":"USER_NOT_FOUND",`+
			`"message":"please transfer funds first to create futures account"}`)
		return
	}
	writeFixture(writer, `[{"id":"1","order_id":"2","currency_pair":"BTC_USDT",`+
		`"contract":"BTC_USDT","side":"buy","price":"10","amount":"2","size":"2",`+
		`"fee":"0.01","fee_currency":"USDT","create_time":"1700000100"}]`)
}
