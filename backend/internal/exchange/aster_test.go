package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseAsterInstrumentsKeepsNativeQuoteAndMultiplier(t *testing.T) {
	var payload asterExchangeInfo
	mustJSON(t, `{
		"symbols":[
			{
				"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT",
				"status":"TRADING","contractType":"PERPETUAL","contractSize":1,
				"filters":[
					{"filterType":"PRICE_FILTER","tickSize":"0.1"},
					{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001"},
					{"filterType":"MIN_NOTIONAL","notional":"5"}
				]
			},
			{
				"symbol":"BTCUSDC","baseAsset":"BTC","quoteAsset":"USDC","marginAsset":"USDC",
				"status":"TRADING","contractType":"PERPETUAL",
				"filters":[{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001"}]
			},
			{
				"symbol":"BTCDATED","baseAsset":"BTC","quoteAsset":"USDT",
				"status":"TRADING","contractType":"CURRENT_QUARTER"
			}
		]
	}`, &payload)
	instruments := parseAsterInstruments(payload)
	if len(instruments) != 2 {
		t.Fatalf("instruments=%+v", instruments)
	}
	if instruments[0].QuoteAsset != "USDT" || instruments[0].ContractSize != 1 ||
		instruments[0].GlobalSymbol != "BTCUSDT" {
		t.Fatalf("usdt=%+v", instruments[0])
	}
	if instruments[1].QuoteAsset != "USDC" || instruments[1].ContractSize != 1 ||
		instruments[1].GlobalSymbol != "BTCUSDC" {
		t.Fatalf("usdc=%+v", instruments[1])
	}
}

func TestParseAsterInstrumentsReadsMarketLotSizeAndMaxQty(t *testing.T) {
	var payload asterExchangeInfo
	mustJSON(t, `{
		"symbols":[{
			"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT",
			"status":"TRADING","contractType":"PERPETUAL","contractSize":1,
			"filters":[
				{"filterType":"PRICE_FILTER","tickSize":"0.1"},
				{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"1000"},
				{"filterType":"MARKET_LOT_SIZE","stepSize":"0.001","minQty":"0.001","maxQty":"120"},
				{"filterType":"MIN_NOTIONAL","notional":"5"}
			]
		}]
	}`, &payload)
	item := parseAsterInstruments(payload)[0]
	if item.MaxQuantity != "1000" || item.MaxQuantityStatus != ConstraintKnown {
		t.Fatalf("limit max=%+v", item)
	}
	if item.MarketQuantityStep != "0.001" || item.MarketQuantityStepStatus != ConstraintKnown ||
		item.MarketMinQuantity != "0.001" || item.MarketMinQuantityStatus != ConstraintKnown ||
		item.MarketMaxQuantity != "120" || item.MarketMaxQuantityStatus != ConstraintKnown ||
		item.MarketMinNotional != "5" || item.MarketMinNotionalStatus != ConstraintKnown {
		t.Fatalf("market rules=%+v", item)
	}
}

func TestParseAsterCurrentUsesDynamicInterval(t *testing.T) {
	rates, err := parseAsterCurrent([]asterPremium{{
		Symbol: "BTCUSDT", LastFundingRate: "0.0001",
		NextFundingTime: 1720000000000, MarkPrice: "60000", IndexPrice: "59990",
		Time: 1719990000000,
	}}, map[string]asterTicker{
		"BTCUSDT": {LastPrice: "60001", Volume: "10", QuoteVolume: "600000", PriceChangePercent: "1.5"},
	}, map[string]float64{"BTCUSDT": 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 1 || rates[0].Rate != 0.0001 || rates[0].IntervalHours != 4 ||
		rates[0].PriceChange24h != 0.015 || rates[0].Turnover24hUSD != 600000 {
		t.Fatalf("rate=%+v", rates[0])
	}
}

func newAsterHTTPAdapter(t *testing.T, baseURL string) *Aster {
	t.Helper()
	adapter := NewAster(2*time.Second, baseURL)
	adapter.oiClient.limiter.interval = 0
	return adapter
}

func TestAsterOpenInterestIntervalIsTenPerSecond(t *testing.T) {
	if asterOpenInterestInterval != 100*time.Millisecond {
		t.Fatalf("asterOpenInterestInterval=%s want 100ms", asterOpenInterestInterval)
	}
	adapter := NewAster(time.Second, "http://example.invalid")
	if adapter.oiClient.limiter == nil {
		t.Fatal("missing oi limiter")
	}
	if adapter.oiClient.limiter.interval != asterOpenInterestInterval {
		t.Fatalf("oi interval=%s want %s", adapter.oiClient.limiter.interval, asterOpenInterestInterval)
	}
	if adapter.client.limiter.interval == asterOpenInterestInterval {
		t.Fatal("generic client must keep a separate limiter")
	}
}

func TestApplyAsterOpenInterestMapping(t *testing.T) {
	t.Run("ten contracts", func(t *testing.T) {
		rate := FundingRate{ExchangeSymbol: "BTCUSDT", MarkPrice: 60000}
		if err := applyAsterOpenInterest(&rate, asterOpenInterest{
			Symbol: "BTCUSDT", OpenInterest: "10",
		}); err != nil {
			t.Fatal(err)
		}
		if rate.OpenInterestContracts != 10 || rate.OpenInterestBase != 10 ||
			rate.OpenInterestNotionalUSD != 600000 {
			t.Fatalf("rate=%+v", rate)
		}
	})
	t.Run("zero is valid", func(t *testing.T) {
		rate := FundingRate{ExchangeSymbol: "BTCUSDT", MarkPrice: 60000}
		if err := applyAsterOpenInterest(&rate, asterOpenInterest{
			Symbol: "BTCUSDT", OpenInterest: "0",
		}); err != nil {
			t.Fatal(err)
		}
		if rate.OpenInterestContracts != 0 || rate.OpenInterestNotionalUSD != 0 {
			t.Fatalf("rate=%+v", rate)
		}
	})
	t.Run("1000 prefix is not multiplied", func(t *testing.T) {
		rate := FundingRate{ExchangeSymbol: "1000SHIBUSDT", MarkPrice: 0.02}
		if err := applyAsterOpenInterest(&rate, asterOpenInterest{
			Symbol: "1000SHIBUSDT", OpenInterest: "10",
		}); err != nil {
			t.Fatal(err)
		}
		if rate.OpenInterestContracts != 10 || rate.OpenInterestBase != 10 ||
			rate.OpenInterestNotionalUSD != 0.2 {
			t.Fatalf("rate=%+v", rate)
		}
	})
	t.Run("invalid number", func(t *testing.T) {
		rate := FundingRate{ExchangeSymbol: "BTCUSDT", MarkPrice: 60000}
		if err := applyAsterOpenInterest(&rate, asterOpenInterest{
			Symbol: "BTCUSDT", OpenInterest: "abc",
		}); err == nil {
			t.Fatal("expected parse error")
		}
	})
	t.Run("negative", func(t *testing.T) {
		rate := FundingRate{ExchangeSymbol: "BTCUSDT", MarkPrice: 60000}
		if err := applyAsterOpenInterest(&rate, asterOpenInterest{
			Symbol: "BTCUSDT", OpenInterest: "-1",
		}); err == nil {
			t.Fatal("expected negative error")
		}
	})
	t.Run("symbol mismatch", func(t *testing.T) {
		rate := FundingRate{ExchangeSymbol: "BTCUSDT", MarkPrice: 60000}
		if err := applyAsterOpenInterest(&rate, asterOpenInterest{
			Symbol: "ETHUSDT", OpenInterest: "10",
		}); err == nil {
			t.Fatal("expected symbol mismatch")
		}
	})
	t.Run("mark price required", func(t *testing.T) {
		rate := FundingRate{ExchangeSymbol: "BTCUSDT", MarkPrice: 0}
		if err := applyAsterOpenInterest(&rate, asterOpenInterest{
			Symbol: "BTCUSDT", OpenInterest: "10",
		}); err == nil {
			t.Fatal("expected mark price error")
		}
	})
}

func TestAsterHTTPFixtureFetchesCurrentAndHistory(t *testing.T) {
	var oiSymbols []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/fapi/v3/exchangeInfo":
			_, _ = writer.Write([]byte(`{"symbols":[{"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT","status":"TRADING","contractType":"PERPETUAL","filters":[]}]}`))
		case "/fapi/v3/premiumIndex":
			_, _ = writer.Write([]byte(`[{"symbol":"BTCUSDT","lastFundingRate":"0.0002","nextFundingTime":1720000000000,"markPrice":"60000","indexPrice":"59990","time":1719990000000}]`))
		case "/fapi/v3/ticker/24hr":
			_, _ = writer.Write([]byte(`[{"symbol":"BTCUSDT","lastPrice":"60001","volume":"1","quoteVolume":"60000","priceChangePercent":"0"}]`))
		case "/fapi/v3/fundingInfo":
			_, _ = writer.Write([]byte(`[{"symbol":"BTCUSDT","fundingIntervalHours":8}]`))
		case "/fapi/v3/openInterest":
			if request.URL.Query().Get("symbol") != "BTCUSDT" {
				t.Errorf("openInterest symbol=%q", request.URL.Query().Get("symbol"))
			}
			oiSymbols = append(oiSymbols, request.URL.Query().Get("symbol"))
			_, _ = writer.Write([]byte(`{"symbol":"BTCUSDT","openInterest":"10","time":1719990000000}`))
		case "/fapi/v3/fundingRate":
			_, _ = writer.Write([]byte(`[{"symbol":"BTCUSDT","fundingRate":"0.00015","fundingTime":1719986400000}]`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	adapter := newAsterHTTPAdapter(t, server.URL)
	if adapter.Name() != "aster" {
		t.Fatalf("name=%s", adapter.Name())
	}
	ctx := context.Background()
	instruments, err := adapter.SyncInstruments(ctx, ContractTypePerpetual)
	if err != nil || len(instruments) != 1 {
		t.Fatalf("instruments=%+v err=%v", instruments, err)
	}
	if _, err := adapter.SyncInstruments(ctx, ContractTypeSpot); err == nil {
		t.Fatal("spot must be unsupported")
	}
	current, err := adapter.FetchCurrent(ctx, instruments)
	if err != nil || len(current) != 1 || current[0].Rate != 0.0002 {
		t.Fatalf("current=%+v err=%v", current, err)
	}
	if current[0].OpenInterestContracts != 10 || current[0].OpenInterestBase != 10 ||
		current[0].OpenInterestNotionalUSD != 600000 {
		t.Fatalf("open interest=%+v", current[0])
	}
	if len(oiSymbols) != 1 || oiSymbols[0] != "BTCUSDT" {
		t.Fatalf("oi symbols=%v", oiSymbols)
	}
	history, err := adapter.FetchHistory(ctx, instruments[0], time.Unix(0, 0), 10)
	if err != nil || len(history) != 1 || !history[0].Settled || history[0].Rate != 0.00015 {
		t.Fatalf("history=%+v err=%v", history, err)
	}
}

func TestAsterFetchCurrentRequestsOnlyWantedOpenInterest(t *testing.T) {
	var (
		mu        sync.Mutex
		oiSymbols []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/fapi/v3/premiumIndex":
			_, _ = writer.Write([]byte(`[
				{"symbol":"BTCUSDT","lastFundingRate":"0.0001","nextFundingTime":1720000000000,"markPrice":"60000","indexPrice":"59990","time":1719990000000},
				{"symbol":"ETHUSDT","lastFundingRate":"0.0002","nextFundingTime":1720000000000,"markPrice":"3000","indexPrice":"2990","time":1719990000000}
			]`))
		case "/fapi/v3/ticker/24hr":
			_, _ = writer.Write([]byte(`[
				{"symbol":"BTCUSDT","lastPrice":"60001","volume":"1","quoteVolume":"60000","priceChangePercent":"0"},
				{"symbol":"ETHUSDT","lastPrice":"3001","volume":"1","quoteVolume":"3000","priceChangePercent":"0"}
			]`))
		case "/fapi/v3/fundingInfo":
			_, _ = writer.Write([]byte(`[]`))
		case "/fapi/v3/openInterest":
			symbol := request.URL.Query().Get("symbol")
			mu.Lock()
			oiSymbols = append(oiSymbols, symbol)
			mu.Unlock()
			_, _ = writer.Write([]byte(`{"symbol":"` + symbol + `","openInterest":"10","time":1719990000000}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	current, err := newAsterHTTPAdapter(t, server.URL).FetchCurrent(context.Background(), []Instrument{
		{ExchangeSymbol: "BTCUSDT", IntervalHours: 8},
	})
	if err != nil || len(current) != 1 || current[0].ExchangeSymbol != "BTCUSDT" {
		t.Fatalf("current=%+v err=%v", current, err)
	}
	if len(oiSymbols) != 1 || oiSymbols[0] != "BTCUSDT" {
		t.Fatalf("oi symbols=%v", oiSymbols)
	}
}

func TestAsterFetchCurrentSkipsOpenInterestWhenInstrumentsEmpty(t *testing.T) {
	oiHits := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/fapi/v3/premiumIndex":
			_, _ = writer.Write([]byte(`[{"symbol":"BTCUSDT","lastFundingRate":"0.0001","nextFundingTime":1720000000000,"markPrice":"60000","indexPrice":"59990","time":1719990000000}]`))
		case "/fapi/v3/ticker/24hr":
			_, _ = writer.Write([]byte(`[{"symbol":"BTCUSDT","lastPrice":"60001","volume":"1","quoteVolume":"60000","priceChangePercent":"0"}]`))
		case "/fapi/v3/fundingInfo":
			_, _ = writer.Write([]byte(`[]`))
		case "/fapi/v3/openInterest":
			oiHits++
			http.NotFound(writer, request)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	current, err := newAsterHTTPAdapter(t, server.URL).FetchCurrent(context.Background(), nil)
	if err != nil || len(current) != 1 {
		t.Fatalf("current=%+v err=%v", current, err)
	}
	if oiHits != 0 {
		t.Fatalf("openInterest hits=%d", oiHits)
	}
}

func TestAsterFetchCurrentFailsWholeRoundOnOpenInterestError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/fapi/v3/premiumIndex":
			_, _ = writer.Write([]byte(`[
				{"symbol":"BTCUSDT","lastFundingRate":"0.0001","nextFundingTime":1720000000000,"markPrice":"60000","indexPrice":"59990","time":1719990000000},
				{"symbol":"ETHUSDT","lastFundingRate":"0.0002","nextFundingTime":1720000000000,"markPrice":"3000","indexPrice":"2990","time":1719990000000}
			]`))
		case "/fapi/v3/ticker/24hr":
			_, _ = writer.Write([]byte(`[
				{"symbol":"BTCUSDT","lastPrice":"60001","volume":"1","quoteVolume":"60000","priceChangePercent":"0"},
				{"symbol":"ETHUSDT","lastPrice":"3001","volume":"1","quoteVolume":"3000","priceChangePercent":"0"}
			]`))
		case "/fapi/v3/fundingInfo":
			_, _ = writer.Write([]byte(`[]`))
		case "/fapi/v3/openInterest":
			if request.URL.Query().Get("symbol") == "ETHUSDT" {
				http.Error(writer, "unavailable", http.StatusInternalServerError)
				return
			}
			_, _ = writer.Write([]byte(`{"symbol":"BTCUSDT","openInterest":"10","time":1719990000000}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	rates, err := newAsterHTTPAdapter(t, server.URL).FetchCurrent(context.Background(), []Instrument{
		{ExchangeSymbol: "BTCUSDT"}, {ExchangeSymbol: "ETHUSDT"},
	})
	if err == nil || rates != nil {
		t.Fatalf("partial openInterest must fail rates=%+v err=%v", rates, err)
	}
}

func TestAsterEmptyHistoryReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/fapi/v3/fundingRate" {
			_, _ = writer.Write([]byte(`[]`))
			return
		}
		http.NotFound(writer, request)
	}))
	t.Cleanup(server.Close)
	_, err := NewAster(2*time.Second, server.URL).FetchHistory(
		context.Background(),
		Instrument{ExchangeSymbol: "BTCUSDT", IntervalHours: 8},
		time.Now().Add(-24*time.Hour),
		10,
	)
	if !errors.Is(err, ErrNoSettledHistory) {
		t.Fatalf("empty history error=%v", err)
	}
}

func TestAsterLiveContract(t *testing.T) {
	if os.Getenv("LIVE_EXCHANGE_TESTS") != "1" && os.Getenv("ASTER_LIVE_CONTRACT") != "1" {
		t.Skip("ASTER_LIVE_CONTRACT is not enabled")
	}
	adapter := NewAster(15*time.Second, "")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	instruments, err := adapter.SyncInstruments(ctx, ContractTypePerpetual)
	if err != nil || len(instruments) == 0 {
		t.Fatalf("live exchangeInfo failed: instruments=%d err=%v", len(instruments), err)
	}
	sample := pickAsterLiveSample(instruments)
	current, err := adapter.FetchCurrent(ctx, sample)
	if err != nil || len(current) == 0 {
		t.Fatalf("live premiumIndex failed: %v", err)
	}
	if current[0].FundingTime.IsZero() || current[0].IntervalHours <= 0 {
		t.Fatalf("live current missing settlement fields: %+v", current[0])
	}
	for _, rate := range current {
		if rate.OpenInterestContracts < 0 || rate.OpenInterestBase < 0 ||
			rate.OpenInterestNotionalUSD < 0 ||
			!asterFinite(rate.OpenInterestContracts) ||
			!asterFinite(rate.OpenInterestBase) ||
			!asterFinite(rate.OpenInterestNotionalUSD) {
			t.Fatalf("live open interest invalid: %+v", rate)
		}
		if rate.OpenInterestBase > 0 && rate.MarkPrice > 0 {
			want := rate.OpenInterestBase * rate.MarkPrice
			if math.Abs(rate.OpenInterestNotionalUSD-want) > math.Max(1, math.Abs(want)*1e-9) {
				t.Fatalf("live notional=%v want %v rate=%+v", rate.OpenInterestNotionalUSD, want, rate)
			}
		}
	}
	history, err := adapter.FetchHistory(ctx, sample[0], time.Now().Add(-48*time.Hour), 4)
	if err != nil || len(history) == 0 {
		t.Fatalf("live fundingRate history failed: %v", err)
	}
	raw, _ := json.Marshal(map[string]any{
		"instruments": len(instruments),
		"current":     current[0],
		"history":     history[0],
	})
	t.Logf("aster live contract %s", raw)
}

func pickAsterLiveSample(instruments []Instrument) []Instrument {
	var btc, eth, prefix Instrument
	for _, instrument := range instruments {
		switch {
		case instrument.ExchangeSymbol == "BTCUSDT" && btc.ExchangeSymbol == "":
			btc = instrument
		case instrument.ExchangeSymbol == "ETHUSDT" && eth.ExchangeSymbol == "":
			eth = instrument
		case strings.HasPrefix(instrument.ExchangeSymbol, "1000") && prefix.ExchangeSymbol == "":
			prefix = instrument
		}
	}
	sample := make([]Instrument, 0, 3)
	if btc.ExchangeSymbol != "" {
		sample = append(sample, btc)
	}
	if eth.ExchangeSymbol != "" {
		sample = append(sample, eth)
	}
	if prefix.ExchangeSymbol != "" {
		sample = append(sample, prefix)
	}
	if len(sample) == 0 {
		return instruments[:min(3, len(instruments))]
	}
	return sample
}
