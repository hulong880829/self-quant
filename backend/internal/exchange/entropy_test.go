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
)

func TestEntropySupportedContractTypesExcludeSpot(t *testing.T) {
	adapter := NewEntropy(time.Second)
	if adapter.Name() != "entropy" {
		t.Fatalf("name=%q", adapter.Name())
	}
	got := AdapterContractTypes(adapter)
	if len(got) != 1 || got[0] != ContractTypePerpetual {
		t.Fatalf("contract types=%v", got)
	}
	if _, err := adapter.SyncInstruments(context.Background(), ContractTypeSpot); err == nil {
		t.Fatal("spot sync must fail")
	}
}

func TestHyperliquidPerpDexesExcludeEntropy(t *testing.T) {
	for _, dex := range hyperliquidPerpDexes {
		if dex == entropyDex {
			t.Fatal("hyperliquidPerpDexes must not include io")
		}
	}
}

func TestEntropyInstrumentsKeepIOPrefixAndUSDC(t *testing.T) {
	var meta hyperliquidMeta
	mustJSON(t, `{"universe":[
		{"name":"ANTH","isDelisted":false,"szDecimals":2},
		{"name":"io:SNDK","isDelisted":false,"szDecimals":4}
	]}`, &meta)
	instruments := stampEntropyInstruments(parseHyperliquidInstruments(meta, entropyDex))
	if len(instruments) != 2 {
		t.Fatalf("instruments=%+v", instruments)
	}
	first, second := instruments[0], instruments[1]
	if first.Exchange != "entropy" || first.ExchangeSymbol != "io:ANTH" ||
		first.BaseAsset != "ANTH" || first.QuoteAsset != "USDC" ||
		first.GlobalSymbol != "ANTHUSDC" || first.IntervalHours != 1 ||
		first.ContractType != ContractTypePerpetual {
		t.Fatalf("anth=%+v", first)
	}
	if second.ExchangeSymbol != "io:SNDK" || second.BaseAsset != "SNDK" {
		t.Fatalf("sndk=%+v", second)
	}
	var metadata map[string]any
	if err := json.Unmarshal(first.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["dex"] != entropyDex {
		t.Fatalf("metadata=%s", first.Metadata)
	}
}

func TestEntropyCurrentAlignsContextByOriginalIndex(t *testing.T) {
	var meta hyperliquidMeta
	mustJSON(t, `{"universe":[
		{"name":"ANTH","isDelisted":false},
		{"name":"GONE","isDelisted":true},
		{"name":"SNDK","isDelisted":false}
	]}`, &meta)
	contexts := []hyperliquidContext{
		{Funding: "0.0001", MarkPx: "10", OraclePx: "10", OpenInterest: "1", DayNtlVlm: "100", PrevDayPx: "10"},
		{Funding: "0.9999", MarkPx: "1", OraclePx: "1", OpenInterest: "1", DayNtlVlm: "1", PrevDayPx: "1"},
		{Funding: "0.0002", MarkPx: "20", OraclePx: "20", OpenInterest: "2", DayNtlVlm: "200", PrevDayPx: "20"},
	}
	now := time.Date(2026, 9, 8, 12, 20, 0, 0, time.UTC)
	rates, err := parseHyperliquidCurrent(meta, contexts, now, entropyDex)
	if err != nil {
		t.Fatal(err)
	}
	rates = stampEntropyRates(rates)
	if len(rates) != 2 {
		t.Fatalf("rates=%+v", rates)
	}
	if rates[0].Exchange != "entropy" || rates[0].ExchangeSymbol != "io:ANTH" ||
		rates[0].Rate != 0.0001 || rates[0].NextRate != nil || rates[0].Settled ||
		rates[0].IntervalHours != 1 {
		t.Fatalf("anth rate=%+v", rates[0])
	}
	if rates[1].ExchangeSymbol != "io:SNDK" || rates[1].Rate != 0.0002 {
		t.Fatalf("sndk rate=%+v", rates[1])
	}
	wantFunding := time.Date(2026, 9, 8, 13, 0, 0, 0, time.UTC)
	if !rates[0].FundingTime.Equal(wantFunding) {
		t.Fatalf("funding time=%v", rates[0].FundingTime)
	}
}

func TestEntropyHTTPUsesDexIOAndRejectsEmptyCatalog(t *testing.T) {
	var metaBody string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		metaBody = string(body)
		if !strings.Contains(string(body), `"dex":"io"`) {
			http.Error(writer, "missing dex", http.StatusBadRequest)
			return
		}
		_, _ = writer.Write([]byte(`[{"universe":[]},[]]`))
	}))
	defer server.Close()
	adapter := &Entropy{client: newClient(server.URL, 3*time.Second)}
	if _, err := adapter.SyncInstruments(context.Background(), ContractTypePerpetual); err == nil {
		t.Fatal("empty catalog must fail")
	}
	if !strings.Contains(metaBody, `"dex":"io"`) {
		t.Fatalf("body=%s", metaBody)
	}
	if _, err := adapter.FetchCurrent(context.Background(), nil); err == nil {
		t.Fatal("empty current catalog must fail")
	}
}

func TestEntropyHTTPRejectsMalformedAndAPIErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"not":"an array"}`))
	}))
	defer server.Close()
	adapter := &Entropy{client: newClient(server.URL, 3*time.Second)}
	if _, err := adapter.SyncInstruments(context.Background(), ContractTypePerpetual); err == nil {
		t.Fatal("malformed catalog must fail")
	}

	failing := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unavailable", http.StatusInternalServerError)
	}))
	defer failing.Close()
	failingAdapter := &Entropy{client: newClient(failing.URL, 3*time.Second)}
	if _, err := failingAdapter.FetchCurrent(context.Background(), nil); err == nil {
		t.Fatal("api error must fail")
	}
}

func TestEntropyHistoryUsesQualifiedCoinAndSettledRates(t *testing.T) {
	var coin string
	var startTime int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Type      string `json:"type"`
			Coin      string `json:"coin"`
			StartTime int64  `json:"startTime"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode: %v", err)
			return
		}
		coin = payload.Coin
		startTime = payload.StartTime
		_ = json.NewEncoder(writer).Encode([]hyperliquidHistory{
			{Coin: "io:ANTH", FundingRate: "0.0003", Time: 1720000000000},
			{Coin: "io:ANTH", FundingRate: "0.0003", Time: 1720000000000},
		})
	}))
	defer server.Close()
	adapter := &Entropy{client: newClient(server.URL, 3*time.Second)}
	since := time.UnixMilli(1710000000000)
	rates, err := adapter.FetchHistory(context.Background(), Instrument{
		ExchangeSymbol: "io:ANTH", IntervalHours: 1,
	}, since, 10)
	if err != nil {
		t.Fatal(err)
	}
	if coin != "io:ANTH" {
		t.Fatalf("history coin=%q", coin)
	}
	if startTime != since.UnixMilli() {
		t.Fatalf("startTime=%d", startTime)
	}
	if len(rates) != 2 {
		t.Fatalf("duplicate timestamps should be returned for upsert: %+v", rates)
	}
	for _, rate := range rates {
		if rate.Exchange != "entropy" || !rate.Settled || rate.IntervalHours != 1 ||
			rate.Rate != 0.0003 || rate.ExchangeSymbol != "io:ANTH" {
			t.Fatalf("rate=%+v", rate)
		}
	}
}

func TestEntropyHistoryAdvancesCursorAndRejectsStuckPages(t *testing.T) {
	var pages int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			StartTime int64 `json:"startTime"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode: %v", err)
			return
		}
		pages++
		page := make([]hyperliquidHistory, 500)
		base := payload.StartTime
		if pages == 1 {
			for i := range page {
				page[i] = hyperliquidHistory{FundingRate: "0.0001", Time: base + int64(i)}
			}
			_ = json.NewEncoder(writer).Encode(page)
			return
		}
		for i := range page {
			page[i] = hyperliquidHistory{FundingRate: "0.0002", Time: base - 1}
		}
		_ = json.NewEncoder(writer).Encode(page)
	}))
	defer server.Close()
	adapter := &Entropy{client: newClient(server.URL, 3*time.Second)}
	_, err := adapter.FetchHistory(context.Background(), Instrument{
		ExchangeSymbol: "io:ANTH",
	}, time.UnixMilli(1_700_000_000_000), 0)
	if err == nil || !strings.Contains(err.Error(), "cursor did not advance") {
		t.Fatalf("stuck cursor error=%v", err)
	}
	if pages != 2 {
		t.Fatalf("pages=%d", pages)
	}
}

func TestEntropySyncInstrumentsStampsExchange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Type string `json:"type"`
			Dex  string `json:"dex"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode: %v", err)
			http.Error(writer, "bad", http.StatusBadRequest)
			return
		}
		if payload.Type != "metaAndAssetCtxs" || payload.Dex != entropyDex {
			http.Error(writer, "unexpected", http.StatusBadRequest)
			return
		}
		_, _ = writer.Write([]byte(`[{"universe":[{"name":"ANTH","isDelisted":false,"szDecimals":2}]},[{"funding":"0.0004","markPx":"12"}]]`))
	}))
	defer server.Close()
	adapter := &Entropy{client: newClient(server.URL, 3*time.Second)}
	instruments, err := adapter.SyncInstruments(context.Background(), ContractTypePerpetual)
	if err != nil {
		t.Fatal(err)
	}
	if len(instruments) != 1 || instruments[0].Exchange != "entropy" ||
		instruments[0].ExchangeSymbol != "io:ANTH" {
		t.Fatalf("instruments=%+v", instruments)
	}
	rates, err := adapter.FetchCurrent(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 1 || rates[0].Exchange != "entropy" || rates[0].Rate != 0.0004 ||
		rates[0].NextRate != nil {
		t.Fatalf("rates=%+v err=%v", rates, err)
	}
}

func TestEntropyDoesNotTreatHTTPErrorAsEmptySuccess(t *testing.T) {
	adapter := &Entropy{client: newClient("http://127.0.0.1:1", time.Millisecond)}
	instruments, err := adapter.SyncInstruments(context.Background(), ContractTypePerpetual)
	if err == nil || instruments != nil {
		t.Fatalf("instruments=%v err=%v", instruments, err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
