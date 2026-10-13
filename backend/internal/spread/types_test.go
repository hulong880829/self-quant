package spread

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestNormalizeRequestAndRange(t *testing.T) {
	request, err := NormalizeRequest(HistoryRequest{
		Venue: "Binance", BaseAsset: "btc", QuoteAsset: "usdt", Range: "24h",
		Now: time.Date(2026, 8, 22, 7, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Venue != "binance" || request.BaseAsset != "BTC" ||
		request.QuoteAsset != "USDT" || CanonicalSymbol(request.BaseAsset, request.QuoteAsset) != "BTCUSDT" {
		t.Fatalf("normalized=%+v", request)
	}
	if request.Range.ExpectedBuckets() != 1440 || request.Range.Resolution() != time.Minute {
		t.Fatalf("range=%s expected=%d resolution=%s",
			request.Range, request.Range.ExpectedBuckets(), request.Range.Resolution())
	}
	if _, err := NormalizeRequest(HistoryRequest{
		Venue: "binance;drop", BaseAsset: "BTC", QuoteAsset: "USDT", Range: "24h",
	}); err == nil {
		t.Fatal("invalid venue was accepted")
	}
	if _, err := ParseRange("2h"); err == nil {
		t.Fatal("invalid range was accepted")
	}
}

func TestNormalizeRequestCompareVenue(t *testing.T) {
	request, err := NormalizeRequest(HistoryRequest{
		Venue: "Binance", CompareVenue: "OKX", BaseAsset: "btc", QuoteAsset: "usdt", Range: "24h",
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Venue != "binance" || request.CompareVenue != "okx" {
		t.Fatalf("normalized=%+v", request)
	}
	if _, err := NormalizeRequest(HistoryRequest{
		Venue: "binance", CompareVenue: "binance", BaseAsset: "BTC", QuoteAsset: "USDT", Range: "24h",
	}); err == nil {
		t.Fatal("same compare venue was accepted")
	}
	if _, err := NormalizeRequest(HistoryRequest{
		Venue: "binance", CompareVenue: "okx;drop", BaseAsset: "BTC", QuoteAsset: "USDT", Range: "24h",
	}); err == nil {
		t.Fatal("invalid compare venue was accepted")
	}
}

func TestNormalizeRequestAcceptsChineseAsset(t *testing.T) {
	request, err := NormalizeRequest(HistoryRequest{
		Venue: "aster", BaseAsset: "龙虾", QuoteAsset: "usdt", Range: "24h",
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.BaseAsset != "龙虾" || request.QuoteAsset != "USDT" ||
		request.VenueCanonicalSymbol != "龙虾USDT" ||
		CanonicalSymbol(request.BaseAsset, request.QuoteAsset) != "龙虾USDT" {
		t.Fatalf("normalized=%+v", request)
	}

	explicit, err := NormalizeRequest(HistoryRequest{
		Venue: "aster", CompareVenue: "bitget", BaseAsset: "龙虾", QuoteAsset: "USDT",
		VenueCanonicalSymbol: "龙虾USDT", CompareVenueCanonicalSymbol: "龙虾USDT",
		Range: "24h",
	})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.VenueCanonicalSymbol != "龙虾USDT" ||
		explicit.CompareVenueCanonicalSymbol != "龙虾USDT" {
		t.Fatalf("explicit=%+v", explicit)
	}
}

func TestNormalizeRequestAcceptsSingleCharacterAssets(t *testing.T) {
	for _, tc := range []struct {
		base, canonical string
	}{
		{base: "T", canonical: "TUSDT"},
		{base: "1", canonical: "1USDT"},
	} {
		request, err := NormalizeRequest(HistoryRequest{
			Venue: "bybit", BaseAsset: tc.base, QuoteAsset: "USDT", Range: "24h",
		})
		if err != nil {
			t.Fatalf("base=%s err=%v", tc.base, err)
		}
		if request.BaseAsset != tc.base || request.QuoteAsset != "USDT" ||
			request.VenueCanonicalSymbol != tc.canonical ||
			CanonicalSymbol(request.BaseAsset, request.QuoteAsset) != tc.canonical {
			t.Fatalf("normalized=%+v want canonical=%s", request, tc.canonical)
		}
	}
	if _, err := NormalizeRequest(HistoryRequest{
		Venue: "bybit", BaseAsset: "T/", QuoteAsset: "USDT", Range: "24h",
	}); err == nil {
		t.Fatal("invalid asset was accepted")
	}
}

func TestValidAssetAndCanonicalSymbol(t *testing.T) {
	for _, valid := range []string{"T", "1", "A", "BTC", "1000PEPE", "龙虾"} {
		if !ValidAsset(valid) {
			t.Fatalf("valid asset was rejected: %q", valid)
		}
	}
	if !ValidCanonicalSymbol("BTCUSDT") || !ValidCanonicalSymbol("龙虾USDT") ||
		!ValidCanonicalSymbol("TUSDT") || !ValidCanonicalSymbol("1USDT") {
		t.Fatal("latin or chinese canonical symbols were rejected")
	}
	if ValidCanonicalSymbol("A") || ValidCanonicalSymbol("1") {
		t.Fatal("too-short canonical symbol was accepted")
	}
	if ValidAsset(strings.Repeat("龙", 17)) || ValidCanonicalSymbol(strings.Repeat("A", 33)) {
		t.Fatal("too-long token was accepted")
	}
	for _, invalid := range []string{
		"", "-", "T/", "T USDT", "BT C", "BT/C", "BT;C", "BT\nC", "BT\x00C",
		string([]byte{'B', 0xff, 'C'}),
	} {
		if ValidAsset(invalid) || ValidCanonicalSymbol(invalid) {
			t.Fatalf("invalid token was accepted: %q", invalid)
		}
	}
}

func TestBuildHistoryScaleAndCoverage(t *testing.T) {
	now := time.Date(2026, 8, 22, 7, 0, 0, 0, time.UTC)
	request := HistoryRequest{
		Venue: "binance", BaseAsset: "BTC", QuoteAsset: "USDT", Range: Range1h, Now: now,
	}
	history := buildHistory(request, []bucketRow{
		{
			Bucket: now.Add(-10 * time.Second), SpotAskRaw: 10000, SpotScale: 2,
			PerpAskRaw: 10012, PerpScale: 2, Samples: 2,
		},
		{
			Bucket: now.Add(-5 * time.Second), SpotAskRaw: 1000, SpotScale: 1,
			PerpAskRaw: 990, PerpScale: 1, Samples: 2,
		},
		{
			Bucket: now, SpotAskRaw: 0, SpotScale: 2, PerpAskRaw: 10100, PerpScale: 2,
			Samples: 1,
		},
	})
	if history.Availability != AvailabilityAvailable || len(history.Points) != 2 {
		t.Fatalf("history=%+v", history)
	}
	if !history.Points[0].SpreadBps.Equal(decimal.RequireFromString("12")) {
		t.Fatalf("first spread=%s", history.Points[0].SpreadBps)
	}
	if !history.Points[1].SpreadBps.Equal(decimal.RequireFromString("-100")) {
		t.Fatalf("second spread=%s", history.Points[1].SpreadBps)
	}
	if !history.Summary.CurrentBps.Equal(decimal.RequireFromString("-100")) ||
		!history.Summary.MinBps.Equal(decimal.RequireFromString("-100")) ||
		!history.Summary.MaxBps.Equal(decimal.RequireFromString("12")) {
		t.Fatalf("summary=%+v", history.Summary)
	}
	if history.Summary.Coverage.GreaterThanOrEqual(decimal.NewFromInt(1)) {
		t.Fatalf("coverage=%s", history.Summary.Coverage)
	}
}

func TestBuildHistoryUnavailableWithoutSpot(t *testing.T) {
	now := time.Date(2026, 8, 22, 7, 0, 0, 0, time.UTC)
	history := buildHistory(HistoryRequest{
		Venue: "okx", BaseAsset: "ETH", QuoteAsset: "USDT", Range: Range1h, Now: now,
	}, []bucketRow{{
		Bucket: now, SpotAskRaw: 0, SpotScale: 2, PerpAskRaw: 400000, PerpScale: 2,
		Samples: 1,
	}})
	if history.Availability != AvailabilityUnavailable || len(history.Points) != 0 {
		t.Fatalf("history=%+v", history)
	}
}

func TestBuildHistoryAvailableWhenLegsDoNotOverlap(t *testing.T) {
	now := time.Date(2026, 8, 22, 7, 0, 0, 0, time.UTC)
	history := buildHistory(HistoryRequest{
		Venue: "binance", CompareVenue: "ibkr", BaseAsset: "AAPL", QuoteAsset: "USDT",
		Range: Range1h, Now: now,
	}, []bucketRow{{
		Bucket: now, SpotAskRaw: 19000, SpotScale: 2, PerpAskRaw: 0, PerpScale: 2,
	}})
	if history.Availability != AvailabilityAvailable || len(history.Points) != 0 ||
		!history.Summary.Coverage.IsZero() {
		t.Fatalf("history=%+v", history)
	}
}

func TestNormalizeIdentityRequestRejectsBadMultiplierAndAcceptsExchangeSymbol(t *testing.T) {
	_, err := NormalizeRequest(HistoryRequest{
		Venue: "binance", CompareVenue: "ibkr", BaseAsset: "AAPL", QuoteAsset: "USDT",
		VenueCanonicalSymbol: "AAPLUSDT", CompareVenueCanonicalSymbol: "AAPL",
		VenueExchangeSymbol: "AAPLUSDT", CompareExchangeSymbol: "265598",
		VenueProduct: "perpetual", CompareVenueProduct: "equity",
		IdentityMode: "venue_symbol", PriceMultiplier: decimal.RequireFromString("-1"),
		Range: Range1h,
	})
	if err == nil {
		t.Fatal("negative multiplier was accepted")
	}
	request, err := NormalizeRequest(HistoryRequest{
		Venue: "okx", CompareVenue: "ibkr", BaseAsset: "AAPL", QuoteAsset: "USDT",
		VenueCanonicalSymbol: "AAPLUSDT", CompareVenueCanonicalSymbol: "USDJPY",
		VenueExchangeSymbol: "AAPL-USDT-SWAP", CompareExchangeSymbol: "15016062",
		VenueProduct: "perpetual", CompareVenueProduct: "equity",
		IdentityMode: "venue_symbol", Range: Range24h,
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.VenueExchangeSymbol != "AAPL-USDT-SWAP" || request.PriceMultiplier.String() != "1" {
		t.Fatalf("request=%+v", request)
	}
	if _, err := NormalizeRequest(HistoryRequest{
		Venue: "okx", CompareVenue: "binance", BaseAsset: "AAPL", QuoteAsset: "USDT",
		VenueCanonicalSymbol: "AAPL-USDT-SWAP", Range: Range1h,
	}); err == nil {
		t.Fatal("default mode accepted a dashed canonical symbol")
	}
}

func TestNormalizeRequestPreservesExchangeSymbolCase(t *testing.T) {
	if !ValidIdentitySymbol("xyz:GME") {
		t.Fatal("xyz:GME was rejected as an identity symbol")
	}
	request, err := NormalizeRequest(HistoryRequest{
		Venue: "hyperliquid", CompareVenue: "ibkr", BaseAsset: "GME", QuoteAsset: "USDC",
		VenueCanonicalSymbol: "XYZGMEUSDC", CompareVenueCanonicalSymbol: "GME",
		VenueExchangeSymbol: "  xyz:GME  ", CompareExchangeSymbol: "36285627",
		VenueProduct: "perpetual", CompareVenueProduct: "equity",
		IdentityMode: "venue_symbol", Range: Range24h,
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.VenueExchangeSymbol != "xyz:GME" || request.CompareExchangeSymbol != "36285627" {
		t.Fatalf("exchange symbols=%q %q", request.VenueExchangeSymbol, request.CompareExchangeSymbol)
	}
	if request.VenueCanonicalSymbol != "XYZGMEUSDC" || request.CompareVenueCanonicalSymbol != "GME" {
		t.Fatalf("canonical symbols=%q %q", request.VenueCanonicalSymbol, request.CompareVenueCanonicalSymbol)
	}

	t.Run("mixed case compare symbol", func(t *testing.T) {
		mixed, err := NormalizeRequest(HistoryRequest{
			Venue: "hyperliquid", CompareVenue: "ibkr", BaseAsset: "GME", QuoteAsset: "USDC",
			VenueCanonicalSymbol: "XYZGMEUSDC", CompareVenueCanonicalSymbol: "GME",
			VenueExchangeSymbol: "xyz:GME", CompareExchangeSymbol: "abc:TsLa",
			VenueProduct: "perpetual", CompareVenueProduct: "equity",
			IdentityMode: "venue_symbol", Range: Range24h,
		})
		if err != nil {
			t.Fatal(err)
		}
		if mixed.VenueExchangeSymbol != "xyz:GME" || mixed.CompareExchangeSymbol != "abc:TsLa" {
			t.Fatalf("exchange symbols=%q %q", mixed.VenueExchangeSymbol, mixed.CompareExchangeSymbol)
		}
	})
}
