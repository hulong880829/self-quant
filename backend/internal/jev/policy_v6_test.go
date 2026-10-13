package jev

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestEnsureActiveRunIsolatesStrategyVersions(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	v6, err := store.EnsureActiveRun(context.Background(), now, decimal.NewFromInt(10000), StrategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	v7, err := store.EnsureActiveRun(context.Background(), now, decimal.NewFromInt(1), "JEVStrategyV7")
	if err != nil {
		t.Fatal(err)
	}
	if v6.ID == v7.ID {
		t.Fatal("V6 and V7 must be different runs")
	}
	if !v6.Equity.Equal(decimal.NewFromInt(10000)) || !v7.InitialEquity.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("v6=%+v v7=%+v", v6, v7)
	}
	again, err := store.EnsureActiveRun(context.Background(), now, decimal.NewFromInt(3), StrategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != v6.ID || !again.Equity.Equal(decimal.NewFromInt(10000)) {
		t.Fatalf("V6 equity mutated %+v", again)
	}
}

func TestRequiredDepthUSDIgnoresNotional(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.MinDepthUSD = decimal.NewFromInt(5000)
	cfg.PositionNotionalUSDT = decimal.NewFromInt(100)
	if !RequiredDepthUSD(cfg).Equal(decimal.NewFromInt(5000)) {
		t.Fatalf("%s", RequiredDepthUSD(cfg))
	}
}

func TestConservativeNetAllowsNegativeP10(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.MinExpectedNetReturn24h = decimal.RequireFromString("0.002")
	cfg.MinConservativeNetReturn24h = decimal.RequireFromString("0.0005")
	cfg.MinNetProfitProbability = decimal.RequireFromString("0.60")
	cfg.MinBasisForecastConfidence = decimal.RequireFromString("0.60")
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	samples := make([]decimal.Decimal, 0, 12)
	for i := 0; i < 10; i++ {
		samples = append(samples, decimal.RequireFromString("0.001"))
	}
	samples = append(samples, decimal.RequireFromString("-0.002"), decimal.RequireFromString("-0.002"))
	candidate := ApplyLocalReturns(cfg, Candidate{
		FundingReturn24h:          decimal.RequireFromString("0.005"),
		ExpectedBasisReturn24h:    decimalPtr(decimal.RequireFromString("-0.001")),
		BasisReturnP10:            decimalPtr(decimal.RequireFromString("-0.002")),
		BasisForecastConfidence:   decimalPtr(decimal.NewFromInt(1)),
		BasisSampleCount:          intPtr(12),
		BasisForecastCalculatedAt: timePtr(now),
		MatchedBasisReturns:       samples,
		LongQuote:                 freshQuote("binance", "BTCUSDT", ProductSpot, now, "100"),
		ShortQuote:                freshQuote("okx", "BTCUSDT", ProductPerpetual, now, "101"),
	})
	if !candidate.ConservativeNetReturn24h.Equal(decimal.RequireFromString("0.0018")) {
		t.Fatalf("conservative=%s", candidate.ConservativeNetReturn24h)
	}
	if !EntryFilterOK(cfg, candidate, time.Minute) {
		t.Fatal("negative P10 with enough funding must pass")
	}
}

func TestEntryFilterRejectsLowNetProfitProbability(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.MinExpectedNetReturn24h = decimal.Zero
	cfg.MinConservativeNetReturn24h = decimal.Zero
	cfg.MinNetProfitProbability = decimal.RequireFromString("0.60")
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	samples := []decimal.Decimal{
		decimal.RequireFromString("-0.01"), decimal.RequireFromString("-0.01"),
		decimal.RequireFromString("0.01"),
	}
	candidate := ApplyLocalReturns(cfg, Candidate{
		FundingReturn24h:          decimal.RequireFromString("0.004"),
		ExpectedBasisReturn24h:    decimalPtr(decimal.RequireFromString("0.002")),
		BasisReturnP10:            decimalPtr(decimal.RequireFromString("-0.01")),
		BasisForecastConfidence:   decimalPtr(decimal.NewFromInt(1)),
		BasisSampleCount:          intPtr(3),
		BasisForecastCalculatedAt: timePtr(now),
		MatchedBasisReturns:       samples,
		LongQuote:                 freshQuote("binance", "BTCUSDT", ProductSpot, now, "100"),
		ShortQuote:                freshQuote("okx", "BTCUSDT", ProductPerpetual, now, "101"),
	})
	if EntryFilterOK(cfg, candidate, time.Minute) {
		t.Fatal("low net profit probability must not enter")
	}
}

func TestNetIfExitUsesBidAskAndExitCost(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	position := Position{
		SharedBaseQty: decimal.NewFromInt(1), LongQty: decimal.NewFromInt(1), ShortQty: decimal.NewFromInt(1).Neg(),
		LongEntryPrice: decimal.NewFromInt(100), ShortEntryPrice: decimal.NewFromInt(101),
		FundingPnL: decimal.RequireFromString("0.2"), CostPaid: decimal.RequireFromString("0.12"),
	}
	long := freshQuote("binance", "BTCUSDT", ProductSpot, now, "102")
	short := freshQuote("okx", "BTCUSDT", ProductPerpetual, now, "100")
	net, ok := NetIfExit(cfg, position, long, short, now)
	if !ok {
		t.Fatal("expected net")
	}
	fill, err := ExitFill(cfg, now, position, long, short)
	if err != nil {
		t.Fatal(err)
	}
	want := position.RealizedSpreadPnL.Add(exitSpreadPnL(position, fill)).Add(position.FundingPnL).Sub(position.CostPaid).Sub(fill.Cost)
	if !net.Equal(want) {
		t.Fatalf("net=%s want=%s", net, want)
	}
}

func TestMissingBBODoesNotFakeNetOrFill(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	position := Position{SharedBaseQty: decimal.NewFromInt(1), LongQty: decimal.NewFromInt(1), ShortQty: decimal.NewFromInt(1).Neg()}
	if _, ok := NetIfExit(cfg, position, Quote{}, Quote{}, now); ok {
		t.Fatal("missing bbo must not invent net")
	}
	if _, err := ExitFill(cfg, now, position, Quote{}, Quote{}); err == nil {
		t.Fatal("missing bbo must not fill")
	}
}

func TestMissingBBOTimeoutPendingThenFill(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.MissingBBOForceAfter = 15 * time.Minute
	opened := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	position := Position{
		Status: PositionOpen, SharedBaseQty: decimal.NewFromInt(1),
		LongQty: decimal.NewFromInt(1), ShortQty: decimal.NewFromInt(1).Neg(),
		LongEntryPrice: decimal.NewFromInt(100), ShortEntryPrice: decimal.NewFromInt(101),
		OpenedAt: opened, LongProduct: ProductSpot, ShortProduct: ProductPerpetual,
	}
	now := opened.Add(time.Minute)
	position = ApplyExitState(cfg, position, decimal.NewFromInt(1), Quote{}, Quote{}, now)
	if position.BBOMissingSince == nil || position.ExitPendingReason != "" {
		t.Fatalf("first miss %+v", position)
	}
	later := now.Add(16 * time.Minute)
	position = ApplyExitState(cfg, position, decimal.NewFromInt(1), Quote{}, Quote{}, later)
	if position.ExitPendingReason != MandatoryExitPending {
		t.Fatalf("timeout %+v", position)
	}
	if ShouldFlattenNow(cfg, position, Quote{}, Quote{}, later) {
		t.Fatal("must wait for bbo")
	}
	fresh := later.Add(time.Second)
	long := freshQuote("binance", "BTCUSDT", ProductSpot, fresh, "100")
	short := freshQuote("okx", "BTCUSDT", ProductPerpetual, fresh, "101")
	if !ShouldFlattenNow(cfg, position, long, short, fresh) {
		t.Fatal("recovered bbo must flatten")
	}
}

func TestLocalTakeProfitNeedsThreeConfirmsWithoutModel(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.TakeProfitPadUSDT = decimal.RequireFromString("0.05")
	cfg.ExitConfirmCycles = 3
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	settled := now.Add(-time.Hour)
	position := Position{
		Status: PositionOpen, SharedBaseQty: decimal.NewFromInt(1),
		LongQty: decimal.NewFromInt(1), ShortQty: decimal.NewFromInt(1).Neg(),
		LongEntryPrice: decimal.NewFromInt(100), ShortEntryPrice: decimal.NewFromInt(101),
		OpenedAt: now.Add(-2 * time.Hour), ShortProduct: ProductPerpetual,
		FundingCheckedThroughShort: &settled, FundingPnL: decimal.RequireFromString("1"),
	}
	long := freshQuote("binance", "BTCUSDT", ProductSpot, now, "102")
	short := freshQuote("okx", "BTCUSDT", ProductPerpetual, now, "100")
	for i := 1; i <= 3; i++ {
		tick := now.Add(time.Duration(i) * time.Minute)
		long.TS, short.TS = tick, tick
		position = ApplyExitState(cfg, position, decimal.RequireFromString("0.01"), long, short, tick)
		if i < 3 && ShouldFlattenNow(cfg, position, long, short, tick) {
			t.Fatalf("flatten too early at %d", i)
		}
	}
	if position.ExitConfirmCount != 3 || !ShouldFlattenNow(cfg, position, long, short, now.Add(3*time.Minute)) {
		t.Fatalf("confirm=%d", position.ExitConfirmCount)
	}
}

func TestDecisionWithoutPositionKeepsStrategyVersion(t *testing.T) {
	store := NewMemoryStore()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	run, err := store.EnsureActiveRun(context.Background(), now, decimal.NewFromInt(10000), StrategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.InsertDecision(context.Background(), Decision{
		RunID: run.ID, CandidateKey: "single:binance:BTCUSDT:BTCUSDT", Kind: KindSingle,
		Decision: DecisionHold, PolicyStatus: PolicyRejected, ExecutionStatus: ExecNotApplicable,
		PromptVersion: PromptVersion, PolicyVersion: PolicyVersion, DecisionAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.StrategyVersion != StrategyVersion {
		t.Fatalf("version=%s", saved.StrategyVersion)
	}
}

func TestFailureRawResponseIsJSON(t *testing.T) {
	raw := FailureRawResponse("parse_error", "not { json")
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["truncated"] != true || payload["parse_error_type"] != "parse_error" {
		t.Fatalf("%v", payload)
	}
}

func TestParseTypeSafeNormalizesWithinTolerance(t *testing.T) {
	raw := []byte(`{
		"model": "jev-1.13.0",
		"answers": {
			"decision": {
				"type": "choice",
				"choice": "hold",
				"probabilities": {"hold": 0.50, "enter": 0.30, "exit": 0.21},
				"confidence": 0.5
			}
		}
	}`)
	answer, err := parseTypeSafeResponse(raw, "x", decimal.RequireFromString("0.02"))
	if err != nil {
		t.Fatal(err)
	}
	sum := answer.EnterProbability.Add(answer.ExitProbability)
	hold := decimal.NewFromInt(1).Sub(sum)
	if !hold.Add(sum).Equal(decimal.NewFromInt(1)) {
		t.Fatalf("normalized %+v", answer)
	}
}

func TestConfidenceTwelveSamplesNotZeroFromDensity(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.BasisLookback = 7 * 24 * time.Hour
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	longKey := NewCanonicalMarketKey("binance", "BTCUSDT", ProductSpot)
	shortKey := NewCanonicalMarketKey("okx", "BTCUSDT", ProductPerpetual)
	until := now.UTC().Truncate(basisBucket)
	start := until.Add(-cfg.BasisLookback)
	longSeries := map[time.Time]TenMinuteBBO{}
	shortSeries := map[time.Time]TenMinuteBBO{}
	matchedDays := 0
	for bucket := start; bucket.Before(until); bucket = bucket.Add(basisBucket) {
		longMid := decimal.NewFromInt(100)
		shortMid := decimal.NewFromInt(100)
		if matchedDays < 4 && bucket.Hour() == 0 && bucket.Minute() == 0 {
			shortMid = decimal.NewFromInt(102)
			matchedDays++
		}
		longSeries[bucket] = TenMinuteBBO{Key: longKey, Bucket: bucket, Bid: longMid, Ask: longMid}
		shortSeries[bucket] = TenMinuteBBO{Key: shortKey, Bucket: bucket, Bid: shortMid, Ask: shortMid}
	}
	forecast := ForecastBasis(cfg, now, longSeries, shortSeries)
	if forecast.Confidence.IsZero() && forecast.SampleCount >= 12 {
		t.Fatalf("12 samples must not yield zero confidence from 864 density: %+v", forecast)
	}
}

func TestMatchBandWidensOnce(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.BasisMatchBandBPS = decimal.NewFromInt(20)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	longKey := NewCanonicalMarketKey("binance", "BTCUSDT", ProductSpot)
	shortKey := NewCanonicalMarketKey("okx", "BTCUSDT", ProductPerpetual)
	until := now.UTC().Truncate(basisBucket)
	start := until.Add(-7 * 24 * time.Hour)
	longSeries := map[time.Time]TenMinuteBBO{}
	shortSeries := map[time.Time]TenMinuteBBO{}
	for bucket := start; bucket.Before(until); bucket = bucket.Add(basisBucket) {
		longSeries[bucket] = TenMinuteBBO{Key: longKey, Bucket: bucket, Bid: decimal.NewFromInt(100), Ask: decimal.NewFromInt(100)}
		offset := decimal.RequireFromString("1.003")
		if bucket.Equal(until.Add(-basisBucket)) {
			offset = decimal.RequireFromString("1.002")
		}
		short := decimal.NewFromInt(100).Mul(offset)
		shortSeries[bucket] = TenMinuteBBO{Key: shortKey, Bucket: bucket, Bid: short, Ask: short}
	}
	forecast := ForecastBasis(cfg, now, longSeries, shortSeries)
	if !forecast.OK {
		t.Fatalf("%+v", forecast)
	}
	if !forecast.MatchBandBPS.Equal(decimal.NewFromInt(20)) && !forecast.MatchBandBPS.Equal(decimal.NewFromInt(40)) {
		t.Fatalf("band=%s", forecast.MatchBandBPS)
	}
}

func TestForecastRejectsIncompleteHorizonAsFutureLeak(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	until := now.UTC().Truncate(basisBucket)
	longKey := NewCanonicalMarketKey("binance", "BTCUSDT", ProductSpot)
	shortKey := NewCanonicalMarketKey("okx", "BTCUSDT", ProductPerpetual)
	longSeries := map[time.Time]TenMinuteBBO{}
	shortSeries := map[time.Time]TenMinuteBBO{}
	for bucket := until.Add(-30 * time.Minute); !bucket.After(until); bucket = bucket.Add(basisBucket) {
		longSeries[bucket] = TenMinuteBBO{Key: longKey, Bucket: bucket, Bid: decimal.NewFromInt(100), Ask: decimal.NewFromInt(100)}
		shortSeries[bucket] = TenMinuteBBO{Key: shortKey, Bucket: bucket, Bid: decimal.NewFromInt(101), Ask: decimal.NewFromInt(101)}
	}
	forecast := ForecastBasis(cfg, now, longSeries, shortSeries)
	if forecast.SampleCount != 0 {
		t.Fatalf("incomplete 24h horizon must not leak, samples=%d", forecast.SampleCount)
	}
}
