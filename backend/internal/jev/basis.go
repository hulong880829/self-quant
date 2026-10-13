package jev

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/config"
)

const (
	minuteTableName        = "crypto_bbo_minute"
	basisBucket            = 10 * time.Minute
	minBasisSamples        = 12
	minBasisDates          = 4
	minPairCoverage        = 0.80
	dateConfidenceDenom    = 5.0
	basisCacheModelVersion = BasisModelVersion
)

type TenMinuteBBO struct {
	Key    MarketKey
	Bucket time.Time
	Bid    decimal.Decimal
	Ask    decimal.Decimal
}

func (b TenMinuteBBO) Mid() decimal.Decimal {
	if !b.Bid.IsPositive() || !b.Ask.IsPositive() {
		return decimal.Zero
	}
	return b.Bid.Add(b.Ask).Div(decimal.NewFromInt(2))
}

type BasisForecast struct {
	ExpectedBasisReturn24h decimal.Decimal
	BasisReturnP10         decimal.Decimal
	BasisProfitProbability decimal.Decimal
	Confidence             decimal.Decimal
	SampleCount            int
	SelectedDayCount       int
	PairCoverage           decimal.Decimal
	MatchBandBPS           decimal.Decimal
	MatchedReturns         []decimal.Decimal
	CalculatedAt           time.Time
	ModelVersion           string
	CurrentMidBasisBPS     *decimal.Decimal
	BasisChange1hBPS       *decimal.Decimal
	BasisChange8hBPS       *decimal.Decimal
	Insufficient           bool
	OK                     bool
}

type cachedForecast struct {
	forecast BasisForecast
	err      error
}

type BasisCache struct {
	cfg         config.Jev
	quotes      QuoteSource
	mu          sync.Mutex
	lastRefresh time.Time
	refreshing  bool
	items       map[string]cachedForecast
	queryCount  int
}

func NewBasisCache(cfg config.Jev, quotes QuoteSource) *BasisCache {
	return &BasisCache{cfg: cfg, quotes: quotes, items: map[string]cachedForecast{}}
}

func ForecastCacheKey(long, short MarketKey, lookback, horizon time.Duration, version string) string {
	return long.QuoteKey() + "\x1e" + short.QuoteKey() + "\x1e" +
		lookback.String() + "\x1e" + horizon.String() + "\x1e" + version
}

func (c *BasisCache) Lookup(long, short MarketKey) (BasisForecast, bool) {
	if c == nil {
		return BasisForecast{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[ForecastCacheKey(long, short, c.cfg.BasisLookback, c.cfg.BasisForecastHorizon, basisCacheModelVersion)]
	if !ok || item.err != nil || !item.forecast.OK {
		return BasisForecast{}, false
	}
	return item.forecast, true
}

func (c *BasisCache) LookupAny(long, short MarketKey) (cachedForecast, bool) {
	if c == nil {
		return cachedForecast{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[ForecastCacheKey(long, short, c.cfg.BasisLookback, c.cfg.BasisForecastHorizon, basisCacheModelVersion)]
	return item, ok
}

func (c *BasisCache) QueryCount() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queryCount
}

func (c *BasisCache) MaybeRefresh(ctx context.Context, now time.Time, candidates []Candidate, open []Position) error {
	if c == nil || c.quotes == nil {
		return nil
	}
	c.mu.Lock()
	due := c.lastRefresh.IsZero() || now.Sub(c.lastRefresh) >= c.cfg.BasisRefreshInterval
	if !due || c.refreshing {
		c.mu.Unlock()
		return nil
	}
	c.refreshing = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.refreshing = false
		c.mu.Unlock()
	}()
	keys := collectForecastKeys(candidates, open)
	from := now.Add(-c.cfg.BasisLookback)
	until := now.Truncate(basisBucket)
	series, err := c.quotes.TenMinuteBBOs(ctx, keys, from, until)
	c.mu.Lock()
	c.queryCount++
	c.mu.Unlock()
	if err != nil {
		return err
	}
	byKey := indexTenMinute(series)
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[string]struct{}{}
	refreshOne := func(long, short MarketKey) {
		if !long.CanonicalOK() || !short.CanonicalOK() {
			return
		}
		key := ForecastCacheKey(long, short, c.cfg.BasisLookback, c.cfg.BasisForecastHorizon, basisCacheModelVersion)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		forecast := ForecastBasis(c.cfg, now, byKey[long.QuoteKey()], byKey[short.QuoteKey()])
		c.items[key] = cachedForecast{forecast: forecast}
	}
	for _, candidate := range candidates {
		refreshOne(candidate.LongKey(), candidate.ShortKey())
	}
	for _, position := range open {
		refreshOne(position.LongKey(), position.ShortKey())
	}
	c.lastRefresh = now
	return nil
}

func collectForecastKeys(candidates []Candidate, open []Position) []MarketKey {
	seen := map[string]MarketKey{}
	add := func(key MarketKey) {
		if !key.CanonicalOK() {
			return
		}
		seen[key.QuoteKey()] = key
	}
	for _, candidate := range candidates {
		add(candidate.LongKey())
		add(candidate.ShortKey())
	}
	for _, position := range open {
		add(position.LongKey())
		add(position.ShortKey())
	}
	out := make([]MarketKey, 0, len(seen))
	for _, key := range seen {
		out = append(out, key)
	}
	return out
}

func indexTenMinute(items []TenMinuteBBO) map[string]map[time.Time]TenMinuteBBO {
	out := map[string]map[time.Time]TenMinuteBBO{}
	for _, item := range items {
		key := item.Key.QuoteKey()
		if out[key] == nil {
			out[key] = map[time.Time]TenMinuteBBO{}
		}
		out[key][item.Bucket.UTC()] = item
	}
	return out
}

func ForecastBasis(cfg config.Jev, now time.Time, longSeries, shortSeries map[time.Time]TenMinuteBBO) BasisForecast {
	horizon := cfg.BasisForecastHorizon
	if horizon <= 0 {
		horizon = 24 * time.Hour
	}
	lookback := cfg.BasisLookback
	if lookback <= 0 {
		lookback = 7 * 24 * time.Hour
	}
	until := now.UTC().Truncate(basisBucket)
	start := until.Add(-lookback)
	theoretical := int(lookback / basisBucket)
	if theoretical <= 0 {
		theoretical = 1
	}
	paired := 0
	samples := make([]sample, 0)
	var latestPair *time.Time
	for bucket := start; bucket.Before(until); bucket = bucket.Add(basisBucket) {
		long, longOK := longSeries[bucket]
		short, shortOK := shortSeries[bucket]
		if !longOK || !shortOK || !long.Mid().IsPositive() || !short.Mid().IsPositive() {
			continue
		}
		paired++
		if latestPair == nil || bucket.After(*latestPair) {
			copyBucket := bucket
			latestPair = &copyBucket
		}
		endBucket := bucket.Add(horizon)
		if !endBucket.Before(until) {
			continue
		}
		longEnd, longEndOK := longSeries[endBucket]
		shortEnd, shortEndOK := shortSeries[endBucket]
		if !longEndOK || !shortEndOK || !longEnd.Mid().IsPositive() || !shortEnd.Mid().IsPositive() {
			continue
		}
		startBPS := midBasisBPSFromMids(long.Mid(), short.Mid())
		if startBPS == nil {
			continue
		}
		samples = append(samples, sample{
			startBPS: *startBPS,
			ret:      basisReturn(long.Mid(), longEnd.Mid(), short.Mid(), shortEnd.Mid()),
			day:      bucket.UTC().Format("2006-01-02"),
		})
	}
	coverage := decimal.NewFromInt(int64(paired)).Div(decimal.NewFromInt(int64(theoretical)))
	forecast := BasisForecast{
		PairCoverage: coverage,
		CalculatedAt: now.UTC(),
		ModelVersion: basisCacheModelVersion,
	}
	if latestPair != nil {
		long := longSeries[*latestPair]
		short := shortSeries[*latestPair]
		forecast.CurrentMidBasisBPS = midBasisBPSFromMids(long.Mid(), short.Mid())
		forecast.BasisChange1hBPS = basisChangeAt(longSeries, shortSeries, *latestPair, time.Hour, forecast.CurrentMidBasisBPS)
		forecast.BasisChange8hBPS = basisChangeAt(longSeries, shortSeries, *latestPair, 8*time.Hour, forecast.CurrentMidBasisBPS)
	}
	if coverage.LessThan(decimal.RequireFromString(fmt.Sprintf("%g", minPairCoverage))) || forecast.CurrentMidBasisBPS == nil {
		forecast.Insufficient = true
		return forecast
	}
	band := cfg.BasisMatchBandBPS
	if !band.IsPositive() {
		band = decimal.NewFromInt(20)
	}
	matched, usedBand := matchBasisSamples(*forecast.CurrentMidBasisBPS, samples, band)
	forecast.MatchBandBPS = usedBand
	forecast.MatchedReturns = matchedReturns(matched)
	forecast.SampleCount = len(matched)
	forecast.SelectedDayCount = uniqueDays(matched)
	target := cfg.BasisConfidenceSampleTarget
	if target <= 0 {
		target = 48
	}
	sampleConf := decimal.NewFromInt(int64(forecast.SampleCount)).Div(decimal.NewFromInt(int64(target)))
	if sampleConf.GreaterThan(decimal.NewFromInt(1)) {
		sampleConf = decimal.NewFromInt(1)
	}
	dateConf := decimal.NewFromInt(int64(forecast.SelectedDayCount)).Div(decimal.NewFromInt(int64(dateConfidenceDenom)))
	if dateConf.GreaterThan(decimal.NewFromInt(1)) {
		dateConf = decimal.NewFromInt(1)
	}
	forecast.Confidence = sampleConf.Mul(dateConf)
	if forecast.SampleCount < minBasisSamples || forecast.SelectedDayCount < minBasisDates {
		forecast.Insufficient = true
		return forecast
	}
	sorted := append([]decimal.Decimal(nil), forecast.MatchedReturns...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].LessThan(sorted[j]) })
	forecast.BasisReturnP10 = percentile(sorted, 0.10)
	trimmed := trimP5P95(sorted)
	forecast.ExpectedBasisReturn24h = meanDecimal(trimmed)
	forecast.OK = true
	return forecast
}

func matchBasisSamples(current decimal.Decimal, samples []sample, band decimal.Decimal) ([]sample, decimal.Decimal) {
	matched := filterBand(current, samples, band)
	if len(matched) >= minBasisSamples && uniqueDays(matched) >= minBasisDates {
		return matched, band
	}
	wide := band.Mul(decimal.NewFromInt(2))
	return filterBand(current, samples, wide), wide
}

type sample struct {
	startBPS decimal.Decimal
	ret      decimal.Decimal
	day      string
}

func filterBand(current decimal.Decimal, samples []sample, band decimal.Decimal) []sample {
	out := make([]sample, 0, len(samples))
	for _, item := range samples {
		delta := item.startBPS.Sub(current).Abs()
		if delta.LessThanOrEqual(band) {
			out = append(out, item)
		}
	}
	return out
}

func matchedReturns(items []sample) []decimal.Decimal {
	out := make([]decimal.Decimal, 0, len(items))
	for _, item := range items {
		out = append(out, item.ret)
	}
	return out
}

func uniqueDays(items []sample) int {
	set := map[string]struct{}{}
	for _, item := range items {
		set[item.day] = struct{}{}
	}
	return len(set)
}

func basisReturn(long0, long1, short0, short1 decimal.Decimal) decimal.Decimal {
	return long1.Div(long0).Sub(decimal.NewFromInt(1)).Add(decimal.NewFromInt(1).Sub(short1.Div(short0)))
}

func midBasisBPSFromMids(longMid, shortMid decimal.Decimal) *decimal.Decimal {
	ratio := MidBasisRatio(longMid, shortMid)
	if ratio == nil {
		return nil
	}
	bps := ratio.Mul(decimal.NewFromInt(10000))
	return &bps
}

func basisChangeAt(
	longSeries, shortSeries map[time.Time]TenMinuteBBO,
	latest time.Time,
	ago time.Duration,
	current *decimal.Decimal,
) *decimal.Decimal {
	if current == nil {
		return nil
	}
	then := latest.Add(-ago)
	long, longOK := longSeries[then]
	short, shortOK := shortSeries[then]
	if !longOK || !shortOK {
		return nil
	}
	thenBPS := midBasisBPSFromMids(long.Mid(), short.Mid())
	if thenBPS == nil {
		return nil
	}
	value := current.Sub(*thenBPS)
	return &value
}

func trimP5P95(sorted []decimal.Decimal) []decimal.Decimal {
	if len(sorted) < 3 {
		return sorted
	}
	lo := percentile(sorted, 0.05)
	hi := percentile(sorted, 0.95)
	out := make([]decimal.Decimal, 0, len(sorted))
	for _, item := range sorted {
		if item.LessThan(lo) || item.GreaterThan(hi) {
			continue
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return sorted
	}
	return out
}

func percentile(sorted []decimal.Decimal, p float64) decimal.Decimal {
	if len(sorted) == 0 {
		return decimal.Zero
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	rank := p * float64(len(sorted)-1)
	lo := int(rank)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[lo]
	}
	weight := decimal.NewFromFloat(rank - float64(lo))
	return sorted[lo].Add(sorted[hi].Sub(sorted[lo]).Mul(weight))
}

func meanDecimal(values []decimal.Decimal) decimal.Decimal {
	if len(values) == 0 {
		return decimal.Zero
	}
	sum := decimal.Zero
	for _, item := range values {
		sum = sum.Add(item)
	}
	return sum.Div(decimal.NewFromInt(int64(len(values))))
}

func tenMinuteBBOQuery(database string) string {
	return fmt.Sprintf(`
SELECT
	venue, canonical_symbol, product,
	toStartOfInterval(bucket, INTERVAL 10 MINUTE) AS bucket_10m,
	tupleElement(argMaxMerge(bbo_state), 1) AS bid_raw,
	tupleElement(argMaxMerge(bbo_state), 2) AS bid_scale,
	tupleElement(argMaxMerge(bbo_state), 3) AS ask_raw,
	tupleElement(argMaxMerge(bbo_state), 4) AS ask_scale
FROM %s.%s
WHERE venue IN @venues
	AND canonical_symbol IN @symbols
	AND product IN @products
	AND bucket >= @from
	AND bucket < toStartOfInterval(@until, INTERVAL 10 MINUTE)
GROUP BY venue, canonical_symbol, product, bucket_10m`, database, minuteTableName)
}

func attachForecast(candidate Candidate, forecast BasisForecast) Candidate {
	if !forecast.OK {
		return candidate
	}
	candidate.ExpectedBasisReturn24h = decimalPtr(forecast.ExpectedBasisReturn24h)
	candidate.BasisReturnP10 = decimalPtr(forecast.BasisReturnP10)
	candidate.BasisProfitProbability = decimalPtr(forecast.BasisProfitProbability)
	candidate.BasisForecastConfidence = decimalPtr(forecast.Confidence)
	candidate.BasisSampleCount = intPtr(forecast.SampleCount)
	candidate.BasisForecastCalculatedAt = timePtr(forecast.CalculatedAt)
	candidate.BasisModelVersion = stringPtr(forecast.ModelVersion)
	if forecast.CurrentMidBasisBPS != nil {
		candidate.CurrentMidBasisBPS = forecast.CurrentMidBasisBPS
	}
	candidate.MatchBandBPS = decimalPtr(forecast.MatchBandBPS)
	candidate.MatchedBasisReturns = append([]decimal.Decimal(nil), forecast.MatchedReturns...)
	candidate.BasisChange1hBPS = forecast.BasisChange1hBPS
	candidate.BasisChange8hBPS = forecast.BasisChange8hBPS
	return candidate
}

func orderedCacheKeyDiffersOnReversal(long, short MarketKey) bool {
	return ForecastCacheKey(long, short, time.Hour, time.Hour, "v") !=
		ForecastCacheKey(short, long, time.Hour, time.Hour, "v")
}
