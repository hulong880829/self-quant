package exchange

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestBinanceFixture(t *testing.T) {
	var payload []binancePremium
	mustJSON(t, `[{"symbol":"BTCUSDT","lastFundingRate":"0.0001","nextFundingTime":1720000000000}]`, &payload)
	rates, err := parseBinanceCurrent(payload, nil)
	if err != nil || len(rates) != 1 || rates[0].Rate != 0.0001 {
		t.Fatalf("parse binance: rates=%+v err=%v", rates, err)
	}
}

func TestOKXFixture(t *testing.T) {
	var payload okxEnvelope[okxFunding]
	mustJSON(t, `{"code":"0","data":[{"instId":"BTC-USDT-SWAP","fundingRate":"0.0002","nextFundingTime":"1720000000000"}]}`, &payload)
	rates, err := parseOKXFunding(payload.Data, false, nil)
	if err != nil || len(rates) != 1 || rates[0].ExchangeSymbol != "BTC-USDT-SWAP" {
		t.Fatalf("parse okx: rates=%+v err=%v", rates, err)
	}
}

func TestBybitFixture(t *testing.T) {
	var payload bybitEnvelope[bybitTicker]
	mustJSON(t, `{"retCode":0,"result":{"list":[{"symbol":"BTCUSDT","fundingRate":"0.0003","nextFundingTime":"1720000000000"}]}}`, &payload)
	rates, err := parseBybitCurrent(payload.Result.List, nil)
	if err != nil || len(rates) != 1 || rates[0].Rate != 0.0003 {
		t.Fatalf("parse bybit: rates=%+v err=%v", rates, err)
	}
}

func TestBitgetFixture(t *testing.T) {
	var payload bitgetEnvelope[[]bitgetTicker]
	mustJSON(t, `{"code":"00000","data":[{"symbol":"BTCUSDT","fundingRate":"0.0004","nextSettleTime":"1720000000000"}]}`, &payload)
	rates, err := parseBitgetCurrent(payload.Data, nil)
	if err != nil || len(rates) != 1 || rates[0].Rate != 0.0004 {
		t.Fatalf("parse bitget: rates=%+v err=%v", rates, err)
	}
}

func TestGateFixture(t *testing.T) {
	var payload []gateContract
	mustJSON(t, `[{"name":"BTC_USDT","in_delisting":false,"funding_rate":"0.0005","funding_next_apply":1720000000,"funding_interval":28800}]`, &payload)
	rates, err := parseGateCurrent(payload, nil)
	if err != nil || len(rates) != 1 || rates[0].IntervalHours != 8 {
		t.Fatalf("parse gate: rates=%+v err=%v", rates, err)
	}
	instruments := parseGateInstruments(payload)
	if len(instruments) != 1 || instruments[0].GlobalSymbol != "BTCUSDT" {
		t.Fatalf("parse gate instrument: %+v", instruments)
	}
}

func TestHyperliquidFixture(t *testing.T) {
	var meta hyperliquidMeta
	var contexts []hyperliquidContext
	mustJSON(t, `{"universe":[{"name":"BTC","isDelisted":false}]}`, &meta)
	mustJSON(t, `[{"funding":"0.0006"}]`, &contexts)
	rates, err := parseHyperliquidCurrent(meta, contexts, time.Unix(1720000000, 0))
	if err != nil || len(rates) != 1 || rates[0].Rate != 0.0006 {
		t.Fatalf("parse hyperliquid: rates=%+v err=%v", rates, err)
	}
	if got := parseHyperliquidInstruments(meta)[0].GlobalSymbol; got != "BTCUSDC" {
		t.Fatalf("global symbol = %q", got)
	}
}

func TestGlobalSymbolPreservesQuote(t *testing.T) {
	if got := GlobalSymbol("btc", "usdt"); got != "BTCUSDT" {
		t.Fatalf("got %q", got)
	}
}

func TestHTTPClientRetriesRateLimit(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(writer, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	var response struct {
		OK bool `json:"ok"`
	}
	if err := newClient(server.URL, 3*time.Second).get(
		context.Background(), "/", nil, &response,
	); err != nil {
		t.Fatal(err)
	}
	if !response.OK || calls.Load() != 3 {
		t.Fatalf("response=%+v calls=%d", response, calls.Load())
	}
}

func TestBinanceHistoryPagination(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		call := calls.Add(1)
		count := 1000
		if call == 2 {
			count = 1
		}
		items := make([]binanceHistory, 0, count)
		start := time.Now().Add(-24 * time.Hour).UnixMilli()
		for index := 0; index < count; index++ {
			items = append(items, binanceHistory{
				Symbol: "BTCUSDT", FundingRate: "0.000100000000000001",
				FundingTime: start + int64(index),
			})
		}
		_ = json.NewEncoder(writer).Encode(items)
	}))
	defer server.Close()
	adapter := &Binance{client: newClient(server.URL, 3*time.Second)}
	rates, err := adapter.FetchHistory(context.Background(), Instrument{
		ExchangeSymbol: "BTCUSDT", IntervalHours: 8,
	}, time.Now().Add(-48*time.Hour), 1001)
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 1001 || calls.Load() != 2 {
		t.Fatalf("rates=%d calls=%d", len(rates), calls.Load())
	}
}

func TestMissingFundingRateReturnsError(t *testing.T) {
	_, err := parseBinanceCurrent([]binancePremium{{
		Symbol: "BTCUSDT", LastFundingRate: "not-a-number",
	}}, nil)
	if err == nil {
		t.Fatal("expected malformed precision field to fail")
	}
}

func TestLiveAdapters(t *testing.T) {
	if os.Getenv("LIVE_EXCHANGE_TESTS") != "1" {
		t.Skip("LIVE_EXCHANGE_TESTS is not enabled")
	}
	adapters := []Adapter{
		NewBinance(15 * time.Second), NewOKX(15 * time.Second),
		NewBybit(15 * time.Second), NewBitget(15 * time.Second),
		NewGate(15 * time.Second), NewHyperliquid(15 * time.Second),
	}
	for _, adapter := range adapters {
		t.Run(adapter.Name(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			instruments, err := adapter.SyncInstruments(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(instruments) == 0 {
				t.Fatal("no active instruments")
			}
			sample := instruments[:min(3, len(instruments))]
			current, err := adapter.FetchCurrent(ctx, sample)
			if err != nil {
				t.Fatal(err)
			}
			if len(current) == 0 {
				t.Fatal("no current funding rates")
			}
			if current[0].LastPrice == 0 && current[0].MarkPrice == 0 {
				t.Fatalf("current snapshot has no market price: %+v", current[0])
			}
			if current[0].FundingTime.IsZero() {
				t.Fatal("current snapshot has no next funding time")
			}
			history, err := adapter.FetchHistory(
				ctx, instruments[0], time.Now().Add(-48*time.Hour), 2,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(history) == 0 {
				t.Fatal("no funding history")
			}
		})
	}
}

func mustJSON(t *testing.T, fixture string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(fixture), target); err != nil {
		t.Fatal(err)
	}
}
