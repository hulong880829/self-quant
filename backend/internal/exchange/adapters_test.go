package exchange

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
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
	mustJSON(t, `{"code":"0","data":[{"instId":"BTC-USDT-SWAP","fundingRate":"0.0002","fundingTime":"1720000000000","nextFundingTime":"1720014400000"}]}`, &payload)
	rates, err := parseOKXFunding(payload.Data, false, nil)
	if err != nil || len(rates) != 1 || rates[0].ExchangeSymbol != "BTC-USDT-SWAP" ||
		rates[0].IntervalHours != 4 {
		t.Fatalf("parse okx: rates=%+v err=%v", rates, err)
	}
	if rates[0].FundingTime.UnixMilli() != 1720000000000 {
		t.Fatalf("current funding should use fundingTime, got %v", rates[0].FundingTime)
	}
}

func TestOKXCurrentFundingUsesFundingTimeForNextSettlement(t *testing.T) {
	const imminent = int64(1720000000000)
	const following = int64(1720014400000)
	payload := okxEnvelope[okxFunding]{Data: []okxFunding{{
		InstID: "BEAT-USDT-SWAP", FundingRate: "0.0001",
		FundingTime: strconv.FormatInt(imminent, 10),
		NextFundingTime: strconv.FormatInt(following, 10),
		NextFundingRate: "0.0002",
	}}}
	rates, err := parseOKXFunding(payload.Data, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 1 {
		t.Fatalf("rates=%+v", rates)
	}
	rate := rates[0]
	if rate.IntervalHours != 4 {
		t.Fatalf("interval=%v", rate.IntervalHours)
	}
	if rate.FundingTime.UnixMilli() != imminent {
		t.Fatalf("funding time=%v want %d", rate.FundingTime, imminent)
	}
	if !rate.FundingTime.Before(milliseconds(following)) {
		t.Fatalf("imminent settlement must precede following settlement")
	}
	if rate.FundingTime.Add(4 * time.Hour).UnixMilli() != following {
		t.Fatalf("interval should match gap between fundingTime and nextFundingTime")
	}
}

func TestOKXMarketDataUsesQuoteTurnoverNotBaseVolume(t *testing.T) {
	rate := FundingRate{}
	applyOKXMarketData(&rate, okxTicker{
		Last: "65000", Open24h: "65000",
		Vol24h: "6363000", VolCcy24h: "63630",
	}, okxOpenInterest{OI: "100", OICcy: "1", OIUsd: "65000"})
	if rate.Volume24hBase != 63630 {
		t.Fatalf("volume base should use volCcy24h (coins), got %v", rate.Volume24hBase)
	}
	wantTurnover := 63630.0 * 65000.0
	if rate.Turnover24hUSD != wantTurnover {
		t.Fatalf("turnover=%v want %v", rate.Turnover24hUSD, wantTurnover)
	}
	if rate.OpenInterestNotionalUSD != 65000 {
		t.Fatalf("oi usd=%v", rate.OpenInterestNotionalUSD)
	}

	rate = FundingRate{}
	applyOKXMarketData(&rate, okxTicker{
		Last: "65000", VolCcy24h: "63630", VolCcyQuote24h: "4100000000",
	}, okxOpenInterest{})
	if rate.Turnover24hUSD != 4100000000 {
		t.Fatalf("prefer volCcyQuote24h when present: %v", rate.Turnover24hUSD)
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
	mustJSON(t, `[{"name":"BTC_USDT","in_delisting":false,"funding_rate":"0.0005","funding_next_apply":1720000000,"funding_interval":28800,"quanto_multiplier":"0.0001"}]`, &payload)
	rates, err := parseGateCurrent(payload, map[string]gateTicker{
		"BTC_USDT": {
			Contract: "BTC_USDT", TotalSize: "10000", MarkPrice: "60000",
		},
	})
	if err != nil || len(rates) != 1 || rates[0].IntervalHours != 8 {
		t.Fatalf("parse gate: rates=%+v err=%v", rates, err)
	}
	if rates[0].OpenInterestContracts != 10000 ||
		rates[0].OpenInterestBase != 1 ||
		rates[0].OpenInterestNotionalUSD != 60000 {
		t.Fatalf("gate open interest units: %+v", rates[0])
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

func TestSpotInstrumentFixtures(t *testing.T) {
	var binance binanceExchangeInfo
	mustJSON(t, `{"symbols":[{"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"USDT","status":"TRADING"}]}`, &binance)
	if items := parseBinanceSpotInstruments(binance); len(items) != 1 ||
		items[0].ContractType != ContractTypeSpot {
		t.Fatalf("binance spot: %+v", items)
	}

	var okx []okxInstrument
	mustJSON(t, `[{"instID":"BTC-USDT","instType":"SPOT","state":"live","baseCcy":"BTC","quoteCcy":"USDT"}]`, &okx)
	if items := parseOKXInstruments(okx, ContractTypeSpot); len(items) != 1 {
		t.Fatalf("okx spot: %+v", items)
	}

	var bybit []bybitInstrument
	mustJSON(t, `[{"symbol":"BTCUSDT","status":"Trading","baseCoin":"BTC","quoteCoin":"USDT"}]`, &bybit)
	if items := parseBybitInstruments(bybit, ContractTypeSpot); len(items) != 1 {
		t.Fatalf("bybit spot: %+v", items)
	}

	var bitget []bitgetSpotInstrument
	mustJSON(t, `[{"symbol":"BTCUSDT","baseCoin":"BTC","quoteCoin":"USDT","status":"online"}]`, &bitget)
	if items := parseBitgetSpotInstruments(bitget); len(items) != 1 {
		t.Fatalf("bitget spot: %+v", items)
	}

	var gate []gateSpotPair
	mustJSON(t, `[{"id":"BTC_USDT","base":"BTC","quote":"USDT","trade_status":"tradable"}]`, &gate)
	if items := parseGateSpotInstruments(gate); len(items) != 1 {
		t.Fatalf("gate spot: %+v", items)
	}

	var hyperliquid hyperliquidSpotMeta
	mustJSON(t, `{"tokens":[{"name":"BTC","index":1},{"name":"USDC","index":0}],"universe":[{"name":"BTC/USDC","tokens":[1,0]}]}`, &hyperliquid)
	if items := parseHyperliquidSpotInstruments(hyperliquid); len(items) != 1 ||
		items[0].GlobalSymbol != "BTCUSDC" {
		t.Fatalf("hyperliquid spot: %+v", items)
	}
}

func TestOKXPerpetualInstrumentUsesInstFamily(t *testing.T) {
	var items []okxInstrument
	mustJSON(t, `[{
		"instId":"BTC-USDT-SWAP",
		"instType":"SWAP",
		"instFamily":"BTC-USDT",
		"state":"live",
		"ctType":"linear",
		"baseCcy":"",
		"quoteCcy":"",
		"settleCcy":"USDT"
	}]`, &items)

	instruments := parseOKXInstruments(items, ContractTypePerpetual)
	if len(instruments) != 1 {
		t.Fatalf("okx perpetual: %+v", instruments)
	}
	instrument := instruments[0]
	if instrument.BaseAsset != "BTC" ||
		instrument.QuoteAsset != "USDT" ||
		instrument.GlobalSymbol != "BTCUSDT" {
		t.Fatalf("okx perpetual assets: %+v", instrument)
	}
}

func TestOKXPerpetualInstrumentRejectsMissingFamily(t *testing.T) {
	items := []okxInstrument{{
		InstID: "BTC-USDT-SWAP", InstType: "SWAP", State: "live", CtType: "linear",
	}}
	if instruments := parseOKXInstruments(items, ContractTypePerpetual); len(instruments) != 0 {
		t.Fatalf("expected invalid okx perpetual to be skipped: %+v", instruments)
	}
}

func TestPerpetualInstrumentFixturesRejectNonLiveMarkets(t *testing.T) {
	var binance binanceExchangeInfo
	mustJSON(t, `{"symbols":[{
		"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"USDT",
		"status":"BREAK","contractType":"PERPETUAL"
	}]}`, &binance)
	if instruments := parseBinanceInstruments(binance); len(instruments) != 0 {
		t.Fatalf("binance non-live: %+v", instruments)
	}

	var okx []okxInstrument
	mustJSON(t, `[{
		"instId":"BTC-USDT-SWAP","instType":"SWAP","instFamily":"BTC-USDT",
		"state":"suspend","ctType":"linear"
	}]`, &okx)
	if instruments := parseOKXInstruments(okx, ContractTypePerpetual); len(instruments) != 0 {
		t.Fatalf("okx non-live: %+v", instruments)
	}

	var bybit []bybitInstrument
	mustJSON(t, `[{
		"symbol":"BTCUSDT","contractType":"LinearPerpetual","status":"Settled",
		"baseCoin":"BTC","quoteCoin":"USDT"
	}]`, &bybit)
	if instruments := parseBybitInstruments(bybit, ContractTypePerpetual); len(instruments) != 0 {
		t.Fatalf("bybit non-live: %+v", instruments)
	}

	var bitget []bitgetInstrument
	mustJSON(t, `[{
		"symbol":"BTCUSDT","symbolStatus":"off","symbolType":"perpetual",
		"baseCoin":"BTC","quoteCoin":"USDT"
	}]`, &bitget)
	if instruments := parseBitgetInstruments(bitget); len(instruments) != 0 {
		t.Fatalf("bitget non-live: %+v", instruments)
	}

	var gate []gateContract
	mustJSON(t, `[{"name":"BTC_USDT","in_delisting":true}]`, &gate)
	if instruments := parseGateInstruments(gate); len(instruments) != 0 {
		t.Fatalf("gate non-live: %+v", instruments)
	}

	var hyperliquid hyperliquidMeta
	mustJSON(t, `{"universe":[{"name":"BTC","isDelisted":true}]}`, &hyperliquid)
	if instruments := parseHyperliquidInstruments(hyperliquid); len(instruments) != 0 {
		t.Fatalf("hyperliquid non-live: %+v", instruments)
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

func TestHTTPClientRetriesPOSTBody(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Type string `json:"type"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if payload.Type != "metaAndAssetCtxs" {
			t.Errorf("body type=%q", payload.Type)
		}
		if calls.Add(1) == 1 {
			http.Error(writer, "retry", http.StatusInternalServerError)
			return
		}
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	var response struct {
		OK bool `json:"ok"`
	}
	if err := newClient(server.URL, 3*time.Second).post(
		context.Background(), "/", `{"type":"metaAndAssetCtxs"}`, &response,
	); err != nil {
		t.Fatal(err)
	}
	if !response.OK || calls.Load() != 2 {
		t.Fatalf("response=%+v calls=%d", response, calls.Load())
	}
}

func TestRetryAfterParsing(t *testing.T) {
	if got := parseRetryAfter("2"); got != 2*time.Second {
		t.Fatalf("duration=%v", got)
	}
	if got := parseRetryAfter(strings.TrimSpace(" invalid ")); got != 0 {
		t.Fatalf("invalid duration=%v", got)
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

func TestGateHistoryUsesBoundedWindows(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		from, _ := strconv.ParseInt(request.URL.Query().Get("from"), 10, 64)
		to, _ := strconv.ParseInt(request.URL.Query().Get("to"), 10, 64)
		if request.URL.Query().Get("limit") != "100" || to-from > int64((30*24*time.Hour)/time.Second) {
			t.Errorf("query=%s", request.URL.RawQuery)
		}
		if time.Unix(from, 0).Before(time.Now().Add(-180 * 24 * time.Hour)) {
			t.Errorf("from exceeds Gate retention: %v", time.Unix(from, 0))
		}
		_ = json.NewEncoder(writer).Encode([]gateHistory{{Time: from, Rate: "0.0001"}})
	}))
	defer server.Close()
	adapter := &Gate{client: newClient(server.URL, 3*time.Second)}
	rates, err := adapter.FetchHistory(context.Background(), Instrument{
		ExchangeSymbol: "BTC_USDT", IntervalHours: 8,
	}, time.Now().Add(-365*24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 6 || calls.Load() != 6 {
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
			spot, err := adapter.SyncInstruments(ctx, ContractTypeSpot)
			if err != nil {
				t.Fatal(err)
			}
			if len(spot) == 0 {
				t.Fatal("no active spot instruments")
			}
			instruments, err := adapter.SyncInstruments(ctx, ContractTypePerpetual)
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

func TestBitgetPerpetualUsesSupportMarginCoins(t *testing.T) {
	var items []bitgetInstrument
	mustJSON(t, `[{
		"symbol":"BTCUSDT","symbolStatus":"normal","symbolType":"perpetual",
		"baseCoin":"BTC","quoteCoin":"USDT","sizeMultiplier":"0.001",
		"priceEndStep":"0.1","minTradeNum":"0.001",
		"supportMarginCoins":["USDT"]
	},{
		"symbol":"BTCUSD","symbolStatus":"normal","symbolType":"perpetual",
		"baseCoin":"BTC","quoteCoin":"USD","sizeMultiplier":"1",
		"priceEndStep":"0.1","minTradeNum":"1",
		"supportMarginCoins":["BTC"]
	}]`, &items)

	instruments := parseBitgetInstruments(items)
	if len(instruments) != 2 {
		t.Fatalf("instruments=%+v", instruments)
	}
	if instruments[0].SettleAsset != "USDT" {
		t.Fatalf("linear settle=%q", instruments[0].SettleAsset)
	}
	if instruments[1].SettleAsset != "BTC" {
		t.Fatalf("inverse settle=%q", instruments[1].SettleAsset)
	}
	var model string
	if err := json.Unmarshal(instruments[1].Metadata, &struct {
		ContractModel *string `json:"contractModel"`
	}{ContractModel: &model}); err != nil || model != "inverse" {
		t.Fatalf("inverse metadata=%s err=%v", instruments[1].Metadata, err)
	}
}

func mustJSON(t *testing.T, fixture string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(fixture), target); err != nil {
		t.Fatal(err)
	}
}
