package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestLighterHourlyFromEightHourUsesDecimalDivision(t *testing.T) {
	hourly, err := lighterHourlyFromEightHour(0.000096)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(hourly-0.000012) > 1e-15 {
		t.Fatalf("hourly=%v", hourly)
	}
	if math.Abs(hourly*24*365-0.10512) > 1e-12 {
		t.Fatalf("annualized=%v would be 8x off if current was stored raw", hourly*24*365)
	}
}

func TestLighterSettledHourlyRateConvertsPercentAndSign(t *testing.T) {
	hourly, err := lighterSettledHourlyRate("0.0012", "long")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(hourly-0.000012) > 1e-15 {
		t.Fatalf("hourly=%v", hourly)
	}
	short, err := lighterSettledHourlyRate("0.0022", "short")
	if err != nil || short >= 0 {
		t.Fatalf("short=%v err=%v", short, err)
	}
}

func TestLighterCurrentMatchesLatestSettledMagnitude(t *testing.T) {
	hourly, err := lighterHourlyFromEightHour(9.6e-5)
	if err != nil {
		t.Fatal(err)
	}
	settled, err := lighterSettledHourlyRate("0.0012", "long")
	if err != nil {
		t.Fatal(err)
	}
	if hourly == 0 || settled/hourly < 0.5 || settled/hourly > 2 {
		t.Fatalf("current=%v settled=%v must share magnitude", hourly, settled)
	}
	if math.Abs((hourly*24*365)/(settled*24*365)-1) > 0.01 {
		t.Fatalf("annualized current=%v settled=%v", hourly*24*365, settled*24*365)
	}
}

func TestParseLighterInstrumentsWritesMultiplierAndMarketID(t *testing.T) {
	var payload lighterOrderBookDetails
	mustJSON(t, `{
		"code":200,
		"order_book_details":[
			{
				"symbol":"BTC","market_id":1,"market_type":"perp","status":"active",
				"multiplier":"1.000000000000000000","supported_size_decimals":4,
				"supported_price_decimals":2,
				"min_base_amount":"0.0001","min_quote_amount":"10"
			},
			{"symbol":"OLD","market_id":9,"market_type":"perp","status":"inactive","multiplier":"1"}
		]
	}`, &payload)
	instruments := parseLighterInstruments(payload)
	if len(instruments) != 1 {
		t.Fatalf("instruments=%+v", instruments)
	}
	got := instruments[0]
	if got.ExchangeSymbol != "BTC" || got.QuoteAsset != "USDC" || got.ContractSize != 1 ||
		got.IntervalHours != 1 || got.PriceTick != 0.01 ||
		got.MarketQuantityStep != "0.0001" ||
		got.MarketMinQuantity != "0.0001" ||
		got.MarketMinNotional != "10" {
		t.Fatalf("instrument=%+v", got)
	}
	var metadata map[string]any
	if err := json.Unmarshal(got.Metadata, &metadata); err != nil || metadata["market_id"] != "1" {
		t.Fatalf("metadata=%s err=%v", got.Metadata, err)
	}
	if metadata["supported_price_decimals"] != float64(2) ||
		metadata["supported_size_decimals"] != float64(4) {
		t.Fatalf("metadata=%s", got.Metadata)
	}
}

func TestParseLighterCurrentFiltersComparisonVenues(t *testing.T) {
	var payload lighterFundingRates
	mustJSON(t, `{
		"code":200,
		"funding_rates":[
			{"market_id":1,"exchange":"binance","symbol":"BTC","rate":0.0001},
			{"market_id":1,"exchange":"lighter","symbol":"BTC","rate":0.00008},
			{"market_id":1,"exchange":"hyperliquid","symbol":"BTC","rate":0.00009}
		]
	}`, &payload)
	now := time.Date(2026, 8, 27, 15, 20, 0, 0, time.UTC)
	rates, err := parseLighterCurrent(payload, lighterOrderBookDetails{}, now)
	if err != nil || len(rates) != 1 {
		t.Fatalf("rates=%+v err=%v", rates, err)
	}
	if rates[0].Exchange != "lighter" || rates[0].IntervalHours != 1 ||
		math.Abs(rates[0].Rate-0.00001) > 1e-15 {
		t.Fatalf("rate=%+v", rates[0])
	}
	if !rates[0].FundingTime.Equal(time.Date(2026, 8, 27, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("next funding=%v", rates[0].FundingTime)
	}
}

func lighterHistoryInstrument(t *testing.T) Instrument {
	t.Helper()
	metadata, err := json.Marshal(map[string]any{"market_id": "0"})
	if err != nil {
		t.Fatal(err)
	}
	return Instrument{ExchangeSymbol: "ETH", Metadata: metadata, IntervalHours: 1}
}

func newLighterHTTPAdapter(t *testing.T, baseURL string) *Lighter {
	t.Helper()
	adapter := NewLighter(2*time.Second, baseURL)
	adapter.client.limiter.interval = 0
	return adapter
}

func TestLighterNewSetsGlobalRequestInterval(t *testing.T) {
	if lighterRequestInterval != time.Second {
		t.Fatalf("lighterRequestInterval=%s want 1s", lighterRequestInterval)
	}
	adapter := NewLighter(time.Second, "http://example.invalid")
	if adapter.client.limiter == nil {
		t.Fatal("missing limiter")
	}
	if adapter.client.limiter.interval != lighterRequestInterval {
		t.Fatalf("interval=%s want %s", adapter.client.limiter.interval, lighterRequestInterval)
	}
}

func TestLighterFetchCurrentAndHistoryShareLimiter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/funding-rates":
			_, _ = writer.Write([]byte(`{"code":200,"funding_rates":[
				{"market_id":0,"exchange":"lighter","symbol":"ETH","rate":0.00008}
			]}`))
		case "/api/v1/orderBookDetails":
			_, _ = writer.Write([]byte(`{"code":200,"order_book_details":[
				{"symbol":"ETH","market_id":0,"market_type":"perp","status":"active",
				 "multiplier":"1","mark_price":"1","index_price":"1","open_interest":"1"}
			]}`))
		case "/api/v1/fundings":
			_, _ = writer.Write([]byte(`{"code":200,"resolution":"1h","fundings":[
				{"timestamp":1700000000,"value":"1","rate":"0.0012","direction":"long"}
			]}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	adapter := newLighterHTTPAdapter(t, server.URL)
	limiter := adapter.client.limiter
	if limiter == nil {
		t.Fatal("missing limiter")
	}
	if _, err := adapter.FetchCurrent(context.Background(), []Instrument{lighterHistoryInstrument(t)}); err != nil {
		t.Fatal(err)
	}
	nextAfterCurrent := limiter.next
	if _, err := adapter.FetchHistory(
		context.Background(), lighterHistoryInstrument(t), time.Unix(1700000000, 0).UTC(), 10,
	); err != nil {
		t.Fatal(err)
	}
	if adapter.client.limiter != limiter {
		t.Fatal("FetchCurrent and FetchHistory must use the same limiter")
	}
	if !adapter.client.limiter.next.After(nextAfterCurrent) {
		t.Fatal("FetchHistory did not advance the shared limiter")
	}
}

func TestLighterLimiterWaitCanceledImmediately(t *testing.T) {
	adapter := NewLighter(time.Second, "http://example.invalid")
	adapter.client.limiter.interval = time.Hour
	if err := adapter.client.limiter.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := adapter.client.limiter.wait(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("canceled wait took %s", elapsed)
	}
}

func TestLighterHistoryRecentStopsAfterCrossingSince(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/fundings" {
			http.NotFound(writer, request)
			return
		}
		pages++
		if pages > 1 {
			http.Error(writer, `{"code":400,"message":"invalid timestamps: end_timestamp must be greater than start_timestamp"}`, http.StatusBadRequest)
			return
		}
		start, _ := strconv.ParseInt(request.URL.Query().Get("start_timestamp"), 10, 64)
		if start != 1700000000 {
			t.Errorf("start_timestamp=%d", start)
		}
		_, _ = writer.Write([]byte(`{"code":200,"resolution":"1h","fundings":[
			{"timestamp":1699996400,"value":"1","rate":"0.0012","direction":"long"},
			{"timestamp":1700000000,"value":"1","rate":"0.0012","direction":"long"},
			{"timestamp":1700003600,"value":"1","rate":"0.0016","direction":"short"}
		]}`))
	}))
	t.Cleanup(server.Close)
	history, err := newLighterHTTPAdapter(t, server.URL).FetchHistory(
		context.Background(), lighterHistoryInstrument(t), time.Unix(1700000000, 0).UTC(), 9000,
	)
	if err != nil {
		t.Fatal(err)
	}
	if pages != 1 {
		t.Fatalf("pages=%d want 1", pages)
	}
	if len(history) != 2 {
		t.Fatalf("history=%d want 2 after filtering before since", len(history))
	}
	if !history[0].FundingTime.Equal(time.Unix(1700000000, 0).UTC()) ||
		!history[1].FundingTime.Equal(time.Unix(1700003600, 0).UTC()) ||
		history[1].Rate >= 0 {
		t.Fatalf("history=%+v", history)
	}
}

func TestLighterHistoryPagesBackwardAndDedupes(t *testing.T) {
	var starts, ends []int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/fundings" {
			http.NotFound(writer, request)
			return
		}
		start, _ := strconv.ParseInt(request.URL.Query().Get("start_timestamp"), 10, 64)
		end, _ := strconv.ParseInt(request.URL.Query().Get("end_timestamp"), 10, 64)
		starts = append(starts, start)
		ends = append(ends, end)
		countBack, _ := strconv.Atoi(request.URL.Query().Get("count_back"))
		if countBack != lighterHistoryPageSize && len(starts) == 1 {
			t.Errorf("count_back=%d want=%d", countBack, lighterHistoryPageSize)
		}
		switch len(starts) {
		case 1:
			_, _ = writer.Write([]byte(`{"code":200,"resolution":"1h","fundings":[
				{"timestamp":1700021600,"value":"1","rate":"0.0012","direction":"long"},
				{"timestamp":1700018000,"value":"1","rate":"0.0012","direction":"long"},
				{"timestamp":1700014400,"value":"1","rate":"0.0012","direction":"long"}
			]}`))
		case 2:
			_, _ = writer.Write([]byte(`{"code":200,"resolution":"1h","fundings":[
				{"timestamp":1700014400,"value":"1","rate":"0.0012","direction":"long"},
				{"timestamp":1700010800,"value":"1","rate":"0.0012","direction":"long"},
				{"timestamp":1700007200,"value":"1","rate":"0.0012","direction":"long"},
				{"timestamp":1700003600,"value":"1","rate":"0.0016","direction":"short"},
				{"timestamp":1700000000,"value":"1","rate":"0.0012","direction":"long"}
			]}`))
		default:
			_, _ = writer.Write([]byte(`{"code":200,"resolution":"1h","fundings":[]}`))
		}
	}))
	t.Cleanup(server.Close)
	history, err := newLighterHTTPAdapter(t, server.URL).FetchHistory(
		context.Background(), lighterHistoryInstrument(t), time.Unix(1700000000, 0).UTC(), 9000,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(starts) != 2 || starts[0] != 1700000000 || starts[1] != 1700000000 {
		t.Fatalf("starts=%v", starts)
	}
	if len(ends) != 2 || ends[1] != 1700014399 || ends[1] >= ends[0] {
		t.Fatalf("ends=%v", ends)
	}
	if len(history) != 7 {
		t.Fatalf("history=%d", len(history))
	}
	for i := 1; i < len(history); i++ {
		if !history[i].FundingTime.After(history[i-1].FundingTime) {
			t.Fatalf("unsorted history=%+v", history)
		}
	}
	if history[0].FundingTime.Unix() != 1700000000 || history[6].FundingTime.Unix() != 1700021600 {
		t.Fatalf("history range=%v %v", history[0].FundingTime, history[6].FundingTime)
	}
	if history[1].Rate >= 0 {
		t.Fatalf("short rate=%v", history[1].Rate)
	}
}

func TestLighterHistoryStopsWhenPageDoesNotAdvance(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		pages++
		if pages > 4 {
			http.Error(writer, "loop", http.StatusInternalServerError)
			return
		}
		_, _ = writer.Write([]byte(`{"code":200,"resolution":"1h","fundings":[
			{"timestamp":1700007200,"value":"1","rate":"0.0012","direction":"long"},
			{"timestamp":1700010800,"value":"1","rate":"0.0012","direction":"long"}
		]}`))
	}))
	t.Cleanup(server.Close)
	history, err := newLighterHTTPAdapter(t, server.URL).FetchHistory(
		context.Background(), lighterHistoryInstrument(t), time.Unix(1700000000, 0).UTC(), 9000,
	)
	if err != nil {
		t.Fatal(err)
	}
	if pages > 3 {
		t.Fatalf("pages=%d did not terminate", pages)
	}
	if len(history) != 2 {
		t.Fatalf("history=%d pages=%d", len(history), pages)
	}
}

func TestLighterHistoryStopsWhenCursorReachesLowerBound(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		pages++
		end, _ := strconv.ParseInt(request.URL.Query().Get("end_timestamp"), 10, 64)
		if end <= 1700000000 {
			t.Errorf("requested end=%d after reaching lower bound", end)
		}
		_, _ = writer.Write([]byte(`{"code":200,"resolution":"1h","fundings":[
			{"timestamp":1700000000,"value":"1","rate":"0.0012","direction":"long"}
		]}`))
	}))
	t.Cleanup(server.Close)
	history, err := newLighterHTTPAdapter(t, server.URL).FetchHistory(
		context.Background(), lighterHistoryInstrument(t), time.Unix(1700000000, 0).UTC(), 9000,
	)
	if err != nil || pages != 1 || len(history) != 1 {
		t.Fatalf("history=%d pages=%d err=%v", len(history), pages, err)
	}
}

func TestLighterHistoryHTTPAndDecodeErrorsAreFailed(t *testing.T) {
	t.Run("400", func(t *testing.T) {
		assertLighterHistoryFailed(t, func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, "invalid timestamps", http.StatusBadRequest)
		})
	})
	t.Run("429", func(t *testing.T) {
		assertLighterHistoryFailed(t, func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, "rate limited", http.StatusTooManyRequests)
		})
	})
	t.Run("500", func(t *testing.T) {
		assertLighterHistoryFailed(t, func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, "unavailable", http.StatusInternalServerError)
		})
	})
	t.Run("bad json", func(t *testing.T) {
		assertLighterHistoryFailed(t, func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`{not-json`))
		})
	})
}

func TestLighterHistorySecondPageErrorDiscardsPartial(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		pages++
		if pages == 1 {
			_, _ = writer.Write([]byte(`{"code":200,"resolution":"1h","fundings":[
				{"timestamp":1700014400,"value":"1","rate":"0.0012","direction":"long"},
				{"timestamp":1700010800,"value":"1","rate":"0.0012","direction":"long"}
			]}`))
			return
		}
		http.Error(writer, "invalid timestamps", http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	_, err := newLighterHTTPAdapter(t, server.URL).FetchHistory(
		context.Background(), lighterHistoryInstrument(t), time.Unix(1700000000, 0).UTC(), 9000,
	)
	if err == nil || errors.Is(err, ErrNoSettledHistory) {
		t.Fatalf("partial page must fail err=%v", err)
	}
	if pages != 2 {
		t.Fatalf("pages=%d", pages)
	}
}

func assertLighterHistoryFailed(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	_, err := newLighterHTTPAdapter(t, server.URL).FetchHistory(
		context.Background(), lighterHistoryInstrument(t), time.Unix(1700000000, 0).UTC(), 10,
	)
	if err == nil || errors.Is(err, ErrNoSettledHistory) {
		t.Fatalf("error=%v", err)
	}
}

func TestLighterEmptyHistoryReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"code":200,"resolution":"1h","fundings":[]}`))
	}))
	t.Cleanup(server.Close)
	metadata, _ := json.Marshal(map[string]any{"market_id": "0"})
	_, err := newLighterHTTPAdapter(t, server.URL).FetchHistory(
		context.Background(),
		Instrument{ExchangeSymbol: "ETH", Metadata: metadata},
		time.Now().Add(-24*time.Hour),
		10,
	)
	if !errors.Is(err, ErrNoSettledHistory) {
		t.Fatalf("empty history error=%v", err)
	}
}

func TestLighterLiveContract(t *testing.T) {
	if os.Getenv("LIVE_EXCHANGE_TESTS") != "1" && os.Getenv("LIGHTER_LIVE_CONTRACT") != "1" {
		t.Skip("LIGHTER_LIVE_CONTRACT is not enabled")
	}
	adapter := NewLighter(15*time.Second, "")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	instruments, err := adapter.SyncInstruments(ctx, ContractTypePerpetual)
	if err != nil || len(instruments) == 0 {
		t.Fatalf("live orderBookDetails failed: %v", err)
	}
	var eth Instrument
	for _, instrument := range instruments {
		if instrument.ExchangeSymbol == "ETH" {
			eth = instrument
			break
		}
	}
	if eth.ExchangeSymbol == "" {
		eth = instruments[0]
	}
	current, err := adapter.FetchCurrent(ctx, []Instrument{eth})
	if err != nil || len(current) != 1 {
		t.Fatalf("live funding-rates failed: %v", err)
	}
	history, err := adapter.FetchHistory(ctx, eth, time.Now().Add(-6*time.Hour), 8)
	if err != nil || len(history) == 0 {
		t.Fatalf("live fundings failed: %v", err)
	}
	latest := history[len(history)-1]
	if current[0].Rate == 0 || latest.Rate == 0 {
		t.Fatalf("zero rates current=%v settled=%v", current[0].Rate, latest.Rate)
	}
	if current[0].Rate*latest.Rate < 0 && math.Abs(current[0].Rate) > 1e-6 && math.Abs(latest.Rate) > 1e-6 {
		t.Fatalf("sign mismatch current=%v settled=%v", current[0].Rate, latest.Rate)
	}
	ratio := math.Abs(current[0].Rate / latest.Rate)
	if ratio < 0.2 || ratio > 5 {
		t.Fatalf("magnitude mismatch current=%v settled=%v", current[0].Rate, latest.Rate)
	}
}
