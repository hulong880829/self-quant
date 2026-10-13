package jev

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/config"
)

func TestForecastReplayExcludesRoundTripCost(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.PaperRoundTripCostBPS = decimal.NewFromInt(12)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	longKey := NewCanonicalMarketKey("binance", "BTCUSDT", ProductSpot)
	shortKey := NewCanonicalMarketKey("binance", "BTCUSDT", ProductPerpetual)
	longSeries, shortSeries := constantSeries(longKey, shortKey, now, cfg.BasisLookback, decimal.NewFromInt(100), decimal.NewFromInt(101))
	forecast := ForecastBasis(cfg, now, longSeries, shortSeries)
	if !forecast.OK {
		t.Fatalf("forecast %+v", forecast)
	}
	if !forecast.ExpectedBasisReturn24h.IsZero() {
		t.Fatalf("flat mids must produce zero basis return, got %s", forecast.ExpectedBasisReturn24h)
	}
	if forecast.ExpectedBasisReturn24h.Equal(decimal.RequireFromString("-0.0012")) {
		t.Fatal("replay deducted round-trip cost")
	}
}

func TestBasisProfitProbabilityIgnoresFundingAndCost(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	longKey := NewCanonicalMarketKey("binance", "ETHUSDT", ProductPerpetual)
	shortKey := NewCanonicalMarketKey("okx", "ETHUSDT", ProductPerpetual)
	longSeries := midSeries(longKey, now, cfg.BasisLookback, func(time.Time) decimal.Decimal {
		return decimal.NewFromInt(100)
	})
	until := now.UTC().Truncate(basisBucket)
	shortSeries := midSeries(shortKey, now, cfg.BasisLookback, func(bucket time.Time) decimal.Decimal {
		hours := until.Sub(bucket).Hours()
		return decimal.NewFromInt(100).Add(decimal.NewFromFloat(hours / 24000))
	})
	forecast := ForecastBasis(cfg, now, longSeries, shortSeries)
	if !forecast.OK || len(forecast.MatchedReturns) == 0 {
		t.Fatalf("profit probability %+v", forecast)
	}
	for _, item := range forecast.MatchedReturns {
		if !item.IsPositive() {
			t.Fatalf("basis sample %s", item)
		}
	}
}

func TestAskBidWidthDoesNotChangeMidForecast(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	longKey := NewCanonicalMarketKey("binance", "BTCUSDT", ProductSpot)
	shortKey := NewCanonicalMarketKey("okx", "BTCUSDT", ProductPerpetual)
	narrowLong, narrowShort := widthSeries(longKey, shortKey, now, cfg.BasisLookback, decimal.NewFromInt(100), decimal.NewFromInt(101), decimal.RequireFromString("0.1"))
	wideLong, wideShort := widthSeries(longKey, shortKey, now, cfg.BasisLookback, decimal.NewFromInt(100), decimal.NewFromInt(101), decimal.RequireFromString("2"))
	narrow := ForecastBasis(cfg, now, narrowLong, narrowShort)
	wide := ForecastBasis(cfg, now, wideLong, wideShort)
	if !narrow.OK || !wide.OK {
		t.Fatalf("narrow=%+v wide=%+v", narrow, wide)
	}
	if !narrow.ExpectedBasisReturn24h.Equal(wide.ExpectedBasisReturn24h) ||
		!decimalOrZero(narrow.CurrentMidBasisBPS).Equal(decimalOrZero(wide.CurrentMidBasisBPS)) {
		t.Fatalf("width changed mid forecast narrow=%+v wide=%+v", narrow, wide)
	}
}

func TestConfidenceDoesNotReachOneFromOverlapAlone(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.BasisLookback = 4 * 24 * time.Hour
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	longKey := NewCanonicalMarketKey("binance", "BTCUSDT", ProductSpot)
	shortKey := NewCanonicalMarketKey("binance", "BTCUSDT", ProductPerpetual)
	longSeries, shortSeries := constantSeries(longKey, shortKey, now, cfg.BasisLookback, decimal.NewFromInt(100), decimal.NewFromInt(100))
	forecast := ForecastBasis(cfg, now, longSeries, shortSeries)
	if !forecast.OK {
		t.Fatalf("%+v", forecast)
	}
	if forecast.SampleCount < minBasisSamples {
		t.Fatalf("samples=%d", forecast.SampleCount)
	}
	if forecast.SelectedDayCount != 4 {
		t.Fatalf("days=%d", forecast.SelectedDayCount)
	}
	if !forecast.Confidence.Equal(decimal.RequireFromString("0.8")) {
		t.Fatalf("confidence=%s samples=%d", forecast.Confidence, forecast.SampleCount)
	}
}

func TestLowSelectedDayCountKeepsConfidenceLow(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	longKey := NewCanonicalMarketKey("binance", "BTCUSDT", ProductSpot)
	shortKey := NewCanonicalMarketKey("okx", "BTCUSDT", ProductPerpetual)
	until := now.UTC().Truncate(basisBucket)
	start := until.Add(-3 * 24 * time.Hour)
	longSeries := map[time.Time]TenMinuteBBO{}
	shortSeries := map[time.Time]TenMinuteBBO{}
	for bucket := start; bucket.Before(until); bucket = bucket.Add(basisBucket) {
		longSeries[bucket] = TenMinuteBBO{Key: longKey, Bucket: bucket, Bid: decimal.NewFromInt(99), Ask: decimal.NewFromInt(101)}
		shortSeries[bucket] = TenMinuteBBO{Key: shortKey, Bucket: bucket, Bid: decimal.NewFromInt(99), Ask: decimal.NewFromInt(101)}
	}
	forecast := ForecastBasis(cfg, now, longSeries, shortSeries)
	if forecast.OK {
		t.Fatalf("3-day coverage must be insufficient: %+v", forecast)
	}
	if forecast.SelectedDayCount >= minBasisDates {
		t.Fatalf("days=%d", forecast.SelectedDayCount)
	}
	if !forecast.Confidence.LessThan(decimal.NewFromInt(1)) {
		t.Fatalf("confidence=%s", forecast.Confidence)
	}
}

func TestP10UsesSameSelectedWindowSet(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	longKey := NewCanonicalMarketKey("binance", "BTCUSDT", ProductSpot)
	shortKey := NewCanonicalMarketKey("okx", "BTCUSDT", ProductPerpetual)
	longSeries := midSeries(longKey, now, cfg.BasisLookback, func(time.Time) decimal.Decimal { return decimal.NewFromInt(100) })
	shortSeries := midSeries(shortKey, now, cfg.BasisLookback, func(bucket time.Time) decimal.Decimal {
		if bucket.UTC().Hour() < 6 {
			return decimal.NewFromInt(98)
		}
		return decimal.NewFromInt(100)
	})
	forecast := ForecastBasis(cfg, now, longSeries, shortSeries)
	if !forecast.OK {
		t.Fatalf("%+v", forecast)
	}
	if forecast.BasisReturnP10.GreaterThan(forecast.ExpectedBasisReturn24h) &&
		forecast.ExpectedBasisReturn24h.IsPositive() {
		t.Fatalf("p10=%s mean=%s", forecast.BasisReturnP10, forecast.ExpectedBasisReturn24h)
	}
}

func TestReversalUsesDifferentCacheKey(t *testing.T) {
	long := NewCanonicalMarketKey("binance", "BTCUSDT", ProductSpot)
	short := NewCanonicalMarketKey("okx", "BTCUSDT", ProductPerpetual)
	if !orderedCacheKeyDiffersOnReversal(long, short) {
		t.Fatal("reversed legs must not share a cache key")
	}
}

func TestRefreshIntervalDoesNotIssuePerCandidateQueries(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	quotes := &fakeQuotes{latest: map[string]Quote{
		NewCanonicalMarketKey("binance", "BTCUSDT", ProductSpot).QuoteKey():      freshQuote("binance", "BTCUSDT", ProductSpot, now, "100"),
		NewCanonicalMarketKey("binance", "BTCUSDT", ProductPerpetual).QuoteKey(): freshQuote("binance", "BTCUSDT", ProductPerpetual, now, "101"),
		NewCanonicalMarketKey("okx", "ETHUSDT", ProductPerpetual).QuoteKey():     freshQuote("okx", "ETHUSDT", ProductPerpetual, now, "200"),
		NewCanonicalMarketKey("binance", "ETHUSDT", ProductPerpetual).QuoteKey(): freshQuote("binance", "ETHUSDT", ProductPerpetual, now, "201"),
	}}
	cache := NewBasisCache(cfg, quotes)
	first := []Candidate{{
		LongExchange: "binance", LongCanonicalSymbol: "BTCUSDT", LongProduct: ProductSpot,
		ShortExchange: "binance", ShortCanonicalSymbol: "BTCUSDT", ShortProduct: ProductPerpetual,
	}}
	if err := cache.MaybeRefresh(context.Background(), now, first, nil); err != nil {
		t.Fatal(err)
	}
	second := append(first, Candidate{
		LongExchange: "binance", LongCanonicalSymbol: "ETHUSDT", LongProduct: ProductPerpetual,
		ShortExchange: "okx", ShortCanonicalSymbol: "ETHUSDT", ShortProduct: ProductPerpetual,
	})
	if err := cache.MaybeRefresh(context.Background(), now.Add(time.Minute), second, nil); err != nil {
		t.Fatal(err)
	}
	if quotes.minuteCalls.Load() != 1 || cache.QueryCount() != 1 {
		t.Fatalf("queries=%d cache=%d", quotes.minuteCalls.Load(), cache.QueryCount())
	}
}

func TestTenMinuteBBOQueryAggregatesInClickHouse(t *testing.T) {
	query := tenMinuteBBOQuery("market_data")
	for _, part := range []string{
		"toStartOfInterval(bucket, INTERVAL 10 MINUTE)",
		"argMaxMerge(bbo_state)",
		"venue, canonical_symbol, product",
		"FROM market_data.crypto_bbo_minute",
	} {
		if !strings.Contains(query, part) {
			t.Fatalf("query missing %q: %s", part, query)
		}
	}
	if strings.Contains(query, "GROUP BY venue, canonical_symbol, product, bucket\n") &&
		!strings.Contains(query, "bucket_10m") {
		t.Fatal("minute rows must not be grouped only by raw minute bucket")
	}
}

func TestBasisChangeSinceEntryUsesMidNotFillPrices(t *testing.T) {
	entry := decimal.NewFromInt(10)
	current := decimal.NewFromInt(25)
	got := BasisChangeSinceEntryBPS(&current, &entry)
	if got == nil || !got.Equal(decimal.NewFromInt(15)) {
		t.Fatalf("%v", got)
	}
}

func constantSeries(
	longKey, shortKey MarketKey,
	now time.Time,
	lookback time.Duration,
	longMid, shortMid decimal.Decimal,
) (map[time.Time]TenMinuteBBO, map[time.Time]TenMinuteBBO) {
	return widthSeries(longKey, shortKey, now, lookback, longMid, shortMid, decimal.RequireFromString("0.5"))
}

func widthSeries(
	longKey, shortKey MarketKey,
	now time.Time,
	lookback time.Duration,
	longMid, shortMid, halfWidth decimal.Decimal,
) (map[time.Time]TenMinuteBBO, map[time.Time]TenMinuteBBO) {
	until := now.UTC().Truncate(basisBucket)
	start := until.Add(-lookback)
	long := map[time.Time]TenMinuteBBO{}
	short := map[time.Time]TenMinuteBBO{}
	for bucket := start; bucket.Before(until); bucket = bucket.Add(basisBucket) {
		long[bucket] = TenMinuteBBO{Key: longKey, Bucket: bucket, Bid: longMid.Sub(halfWidth), Ask: longMid.Add(halfWidth)}
		short[bucket] = TenMinuteBBO{Key: shortKey, Bucket: bucket, Bid: shortMid.Sub(halfWidth), Ask: shortMid.Add(halfWidth)}
	}
	return long, short
}

func midSeries(
	key MarketKey,
	now time.Time,
	lookback time.Duration,
	midFn func(time.Time) decimal.Decimal,
) map[time.Time]TenMinuteBBO {
	until := now.UTC().Truncate(basisBucket)
	start := until.Add(-lookback)
	out := map[time.Time]TenMinuteBBO{}
	for bucket := start; bucket.Before(until); bucket = bucket.Add(basisBucket) {
		mid := midFn(bucket)
		out[bucket] = TenMinuteBBO{
			Key: key, Bucket: bucket,
			Bid: mid.Sub(decimal.RequireFromString("0.5")),
			Ask: mid.Add(decimal.RequireFromString("0.5")),
		}
	}
	return out
}

func TestOpenQuotesBatchQueryCompletesWithinTimeout(t *testing.T) {
	t.Setenv("CLICKHOUSE_PASSWORD", envOr("CLICKHOUSE_PASSWORD", envOr("CLICKHOUSE_BBO_PASSWORD", "")))
	cfg, err := config.JevFromEnv()
	if err != nil || strings.TrimSpace(cfg.ClickHousePassword) == "" {
		t.Skip("clickhouse is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.QueryTimeout)
	defer cancel()
	quotes, err := OpenQuotes(ctx, cfg)
	if err != nil {
		t.Skipf("clickhouse unavailable: %v", err)
	}
	defer quotes.Close()
	now := time.Now().UTC()
	keys := make([]MarketKey, 0, 96)
	bases := []string{
		"AAA", "BBB", "CCC", "DDD", "EEE", "FFF", "GGG", "HHH",
		"III", "JJJ", "KKK", "LLL", "MMM", "NNN", "OOO", "PPP",
		"QQQ", "RRR", "SSS", "TTT", "UUU", "VVV", "WWW", "XXX",
		"YYY", "ZZZ", "A1", "B1", "C1", "D1", "E1", "F1",
		"G1", "H1", "I1", "J1", "K1", "L1", "M1", "N1",
		"O1", "P1", "Q1", "R1", "S1", "T1", "U1", "V1",
	}
	venues := []string{"binance", "okx"}
	products := []string{ProductSpot, ProductPerpetual}
	for _, base := range bases {
		for _, venue := range venues {
			for _, product := range products {
				keys = append(keys, NewCanonicalMarketKey(venue, base+"USDT", product))
				if len(keys) == 96 {
					break
				}
			}
			if len(keys) == 96 {
				break
			}
		}
		if len(keys) == 96 {
			break
		}
	}
	started := time.Now()
	_, err = quotes.TenMinuteBBOs(ctx, keys, now.Add(-7*24*time.Hour), now)
	if err != nil {
		t.Fatalf("batch query failed: %v", err)
	}
	if time.Since(started) > cfg.QueryTimeout {
		t.Fatalf("batch query exceeded %s", cfg.QueryTimeout)
	}
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
