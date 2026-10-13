package jev

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/config"
)

func TestMissingBasisForecastEmptyReason(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	cfg := testJevConfig(t)
	rate := sampleRate("binance", "BTCUSDT", "BTC", "0.002")
	rate.SourceUpdatedAt = now
	quotes, spots := singleMarket(now, "binance", "BTC", "BTCUSDT")
	set := BuildEntryCandidates(cfg, CandidateInputs{
		Now: now, Rates: []Rate{rate}, Quotes: quotes, Spots: spots,
		Forecast: func(MarketKey, MarketKey) (BasisForecast, bool) { return BasisForecast{}, false },
	})
	if len(set.Single) != 0 || set.SingleEmptyReason != BasisHistoryInsufficient {
		t.Fatalf("single=%d reason=%s stats=%+v", len(set.Single), set.SingleEmptyReason, set.SingleStats)
	}
	if set.SingleStats.DepthPassed == 0 || set.SingleStats.BasisForecastPassed != 0 {
		t.Fatalf("funnel %+v", set.SingleStats)
	}
}

func TestBuildEntryCandidatesRequiresSpot(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	cfg := testJevConfig(t)
	rate := sampleRate("binance", "BTCUSDT", "BTC", "0.002")
	rate.SourceUpdatedAt = now
	quotes := map[string]Quote{
		quoteKey("binance", "BTCUSDT", ProductPerpetual): freshQuote("binance", "BTCUSDT", ProductPerpetual, now, "50000"),
	}
	set := BuildEntryCandidates(cfg, CandidateInputs{Now: now, Rates: []Rate{rate}, Quotes: quotes})
	if len(set.Single) != 0 || set.SingleEmptyReason != "missing_spot_instrument" {
		t.Fatalf("single=%d reason=%s stats=%+v", len(set.Single), set.SingleEmptyReason, set.SingleStats)
	}
	quotes[quoteKey("binance", "BTCUSDT", ProductSpot)] = freshQuote("binance", "BTCUSDT", ProductSpot, now, "49990")
	spots := map[string]SpotInstrument{
		spotKey("binance", "BTC", "USDT"): {Exchange: "binance", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT"},
	}
	set = BuildEntryCandidates(cfg, CandidateInputs{Now: now, Rates: []Rate{rate}, Quotes: quotes, Spots: spots})
	if len(set.Single) != 1 || set.Single[0].LongProduct != ProductSpot {
		t.Fatalf("%+v", set.Single)
	}
}

func TestCrossCandidatesReorientByCumulative24h(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	cfg := testJevConfig(t)
	low := sampleRate("binance", "ETHUSDT", "ETH", "0.0002")
	high := sampleRate("okx", "ETHUSDT", "ETH", "0.0020")
	low.SourceUpdatedAt = now
	high.SourceUpdatedAt = now
	quotes := map[string]Quote{
		quoteKey("binance", "ETHUSDT", ProductPerpetual): freshQuote("binance", "ETHUSDT", ProductPerpetual, now, "2000"),
		quoteKey("okx", "ETHUSDT", ProductPerpetual):     freshQuote("okx", "ETHUSDT", ProductPerpetual, now, "2001"),
	}
	set := BuildEntryCandidates(cfg, CandidateInputs{
		Now: now, Rates: []Rate{low, high}, Quotes: quotes, Forecast: okForecast(now),
	})
	if len(set.Cross) != 1 {
		t.Fatalf("cross=%d reason=%s", len(set.Cross), set.CrossEmptyReason)
	}
	got := set.Cross[0]
	if got.LongExchange != "binance" || got.ShortExchange != "okx" {
		t.Fatalf("orientation %+v", got)
	}
}

func TestTop20DoesNotDropManagedPositions(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Now().UTC()
	position := Position{ID: 7, Status: PositionOpen, BaseAsset: "SOL", OpenedAt: now.Add(-time.Hour)}
	if ShouldForceExit(cfg, position, now) {
		t.Fatal("fresh position must not force-exit")
	}
	if !ShouldForceExit(cfg, position, now.Add(cfg.MaxHoldDuration)) {
		t.Fatal("max hold must force-exit even if out of top 20")
	}
}

func TestIncompleteHistoryDoesNotBlockSingleCandidate(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	cfg := testJevConfig(t)
	rate := sampleRate("binance", "BTCUSDT", "BTC", "0.002")
	rate.SourceUpdatedAt = now
	rate.History24hComplete = false
	quotes, spots := singleMarket(now, "binance", "BTC", "BTCUSDT")
	set := BuildEntryCandidates(cfg, CandidateInputs{
		Now: now, Rates: []Rate{rate}, Quotes: quotes, Spots: spots, Forecast: okForecast(now),
	})
	if len(set.Single) != 1 || set.SingleEmptyReason != "" {
		t.Fatalf("single=%d reason=%s stats=%+v", len(set.Single), set.SingleEmptyReason, set.SingleStats)
	}
	if set.SingleStats.HistoryComplete != 0 {
		t.Fatalf("history complete should stay observational, stats=%+v", set.SingleStats)
	}
}

func TestIncompleteHistoryDoesNotBlockCrossCandidate(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	cfg := testJevConfig(t)
	low := sampleRate("binance", "ETHUSDT", "ETH", "0.0002")
	high := sampleRate("okx", "ETHUSDT", "ETH", "0.0020")
	low.SourceUpdatedAt = now
	high.SourceUpdatedAt = now
	low.History24hComplete = false
	high.History24hComplete = false
	quotes := map[string]Quote{
		quoteKey("binance", "ETHUSDT", ProductPerpetual): freshQuote("binance", "ETHUSDT", ProductPerpetual, now, "2000"),
		quoteKey("okx", "ETHUSDT", ProductPerpetual):     freshQuote("okx", "ETHUSDT", ProductPerpetual, now, "2001"),
	}
	set := BuildEntryCandidates(cfg, CandidateInputs{
		Now: now, Rates: []Rate{low, high}, Quotes: quotes, Forecast: okForecast(now),
	})
	if len(set.Cross) != 1 || set.CrossEmptyReason != "" {
		t.Fatalf("cross=%d reason=%s stats=%+v", len(set.Cross), set.CrossEmptyReason, set.CrossStats)
	}
	if set.CrossStats.HistoryComplete != 0 {
		t.Fatalf("history complete should stay observational, stats=%+v", set.CrossStats)
	}
	got := set.Cross[0]
	if got.LongExchange != "binance" || got.ShortExchange != "okx" {
		t.Fatalf("orientation %+v", got)
	}
}

func TestUnsupportedContractEmptyReason(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	cfg := testJevConfig(t)
	rate := sampleRate("hyperliquid", "TSLA", "TSLA", "0.01")
	rate.SourceUpdatedAt = now
	rate.VenueContractType = VenueHIP3
	set := BuildEntryCandidates(cfg, CandidateInputs{Now: now, Rates: []Rate{rate}})
	if set.SingleEmptyReason != "no_supported_contract" || set.SingleStats.ContractPassed != 0 {
		t.Fatalf("reason=%s stats=%+v", set.SingleEmptyReason, set.SingleStats)
	}
}

func TestNegativeRateRejectedWithoutCompleteness(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	cfg := testJevConfig(t)
	incomplete := sampleRate("binance", "BTCUSDT", "BTC", "-0.001")
	incomplete.SourceUpdatedAt = now
	incomplete.History24hComplete = false
	set := BuildEntryCandidates(cfg, CandidateInputs{Now: now, Rates: []Rate{incomplete}})
	if set.SingleEmptyReason != "no_positive_24h_rate" {
		t.Fatalf("incomplete reason=%s stats=%+v", set.SingleEmptyReason, set.SingleStats)
	}
	rate := sampleRate("binance", "BTCUSDT", "BTC", "-0.001")
	rate.SourceUpdatedAt = now
	set = BuildEntryCandidates(cfg, CandidateInputs{Now: now, Rates: []Rate{rate}})
	if set.SingleEmptyReason != "no_positive_24h_rate" || set.SingleStats.Positive24hPassed != 0 {
		t.Fatalf("reason=%s stats=%+v", set.SingleEmptyReason, set.SingleStats)
	}
}

func TestCrossNegativeLegsWithPositiveEdge(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	cfg := testJevConfig(t)
	low := sampleRate("binance", "ETHUSDT", "ETH", "-0.0020")
	high := sampleRate("okx", "ETHUSDT", "ETH", "-0.0002")
	low.SourceUpdatedAt = now
	high.SourceUpdatedAt = now
	quotes := map[string]Quote{
		quoteKey("binance", "ETHUSDT", ProductPerpetual): freshQuote("binance", "ETHUSDT", ProductPerpetual, now, "2000"),
		quoteKey("okx", "ETHUSDT", ProductPerpetual):     freshQuote("okx", "ETHUSDT", ProductPerpetual, now, "2001"),
	}
	set := BuildEntryCandidates(cfg, CandidateInputs{
		Now: now, Rates: []Rate{low, high}, Quotes: quotes, Forecast: okForecast(now),
	})
	if len(set.Cross) != 1 || set.CrossEmptyReason != "" {
		t.Fatalf("cross=%d reason=%s stats=%+v", len(set.Cross), set.CrossEmptyReason, set.CrossStats)
	}
	if set.Cross[0].LongExchange != "binance" || set.Cross[0].ShortExchange != "okx" {
		t.Fatalf("orientation %+v", set.Cross[0])
	}
	if set.CrossStats.PositiveEdgePassed != 1 || set.CrossStats.Final != 1 {
		t.Fatalf("funnel %+v", set.CrossStats)
	}
}

func TestCrossEqualRatesNoPositiveEdge(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	cfg := testJevConfig(t)
	left := sampleRate("binance", "ETHUSDT", "ETH", "0.001")
	right := sampleRate("okx", "ETHUSDT", "ETH", "0.001")
	left.SourceUpdatedAt = now
	right.SourceUpdatedAt = now
	set := BuildEntryCandidates(cfg, CandidateInputs{Now: now, Rates: []Rate{left, right}})
	if set.CrossEmptyReason != "no_positive_24h_edge" || set.CrossStats.PositiveEdgePassed != 0 {
		t.Fatalf("reason=%s stats=%+v", set.CrossEmptyReason, set.CrossStats)
	}
}

func TestSingleBBOUnavailableVsDepth(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	cfg := testJevConfig(t)
	cfg.MinDepthUSD = decimal.NewFromInt(100000)
	rate := sampleRate("binance", "BTCUSDT", "BTC", "0.002")
	rate.SourceUpdatedAt = now
	spots := map[string]SpotInstrument{
		spotKey("binance", "BTC", "USDT"): {Exchange: "binance", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT"},
	}
	set := BuildEntryCandidates(cfg, CandidateInputs{Now: now, Rates: []Rate{rate}, Quotes: map[string]Quote{}, Spots: spots})
	if set.SingleEmptyReason != "bbo_unavailable" {
		t.Fatalf("missing quote reason=%s stats=%+v", set.SingleEmptyReason, set.SingleStats)
	}
	thin := freshQuote("binance", "BTCUSDT", ProductPerpetual, now, "50000")
	thin.BidQty = decimal.RequireFromString("0.001")
	thin.AskQty = decimal.RequireFromString("0.001")
	spotThin := thin
	spotThin.Key = MarketKey{Venue: "binance", Symbol: "BTCUSDT", Product: ProductSpot, Exchange: "binance"}
	quotes := map[string]Quote{
		quoteKey("binance", "BTCUSDT", ProductPerpetual): thin,
		quoteKey("binance", "BTCUSDT", ProductSpot):      spotThin,
	}
	set = BuildEntryCandidates(cfg, CandidateInputs{Now: now, Rates: []Rate{rate}, Quotes: quotes, Spots: spots})
	if set.SingleEmptyReason != "bbo_depth_insufficient" || set.SingleStats.BBOFreshPassed == 0 {
		t.Fatalf("thin depth reason=%s stats=%+v", set.SingleEmptyReason, set.SingleStats)
	}
}

func TestCapEntryCandidatesPrefersHigherNetReturn(t *testing.T) {
	singleLow := Candidate{Key: "single:a", Kind: KindSingle, ExpectedNetReturn24h: decimal.RequireFromString("0.001")}
	crossHigh := Candidate{Key: "cross:b", Kind: KindCross, ExpectedNetReturn24h: decimal.RequireFromString("0.009")}
	got := capEntryCandidates([]Candidate{singleLow, crossHigh}, 1)
	if len(got) != 1 || got[0].Key != "cross:b" {
		t.Fatalf("%+v", got)
	}
}

func TestAllocateOnePerBaseAndPortfolioRejected(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.MaxOpenPositions = 2
	open := []Position{{Status: PositionOpen, BaseAsset: "BTC"}}
	enters := []Decision{
		{CandidateKey: "single:okx:ETHUSDT", Kind: KindSingle, Decision: DecisionEnter, BaseAsset: "ETH", EnterProbability: decimal.RequireFromString("0.9"), Confidence: decimal.RequireFromString("0.8"), ExpectedNetReturn24h: decimalPtr(decimal.RequireFromString("0.01"))},
		{CandidateKey: "single:binance:ETHUSDT", Kind: KindSingle, Decision: DecisionEnter, BaseAsset: "ETH", EnterProbability: decimal.RequireFromString("0.8"), Confidence: decimal.RequireFromString("0.7"), ExpectedNetReturn24h: decimalPtr(decimal.RequireFromString("0.009"))},
		{CandidateKey: "single:binance:SOLUSDT", Kind: KindSingle, Decision: DecisionEnter, BaseAsset: "SOL", EnterProbability: decimal.RequireFromString("0.7"), Confidence: decimal.RequireFromString("0.6"), ExpectedNetReturn24h: decimalPtr(decimal.RequireFromString("0.008"))},
	}
	got := AllocateEnters(cfg, open, enters, nil)
	if len(got) != 3 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].ExecutionStatus != ExecPending || got[0].CandidateKey != "single:okx:ETHUSDT" {
		t.Fatalf("winner %+v", got[0])
	}
	if got[1].ExecutionStatus != ExecPortfolioRej || got[2].ExecutionStatus != ExecPortfolioRej {
		t.Fatalf("losers %+v %+v", got[1], got[2])
	}
}

func TestRankEnterDecisionsPrefersNetReturn(t *testing.T) {
	ranked := RankEnterDecisions([]Decision{
		{CandidateKey: "high-p", Decision: DecisionEnter, EnterProbability: decimal.RequireFromString("0.9"), Confidence: decimal.RequireFromString("0.9"), ExpectedNetReturn24h: decimalPtr(decimal.RequireFromString("0.001"))},
		{CandidateKey: "high-net", Decision: DecisionEnter, EnterProbability: decimal.RequireFromString("0.2"), Confidence: decimal.RequireFromString("0.2"), ExpectedNetReturn24h: decimalPtr(decimal.RequireFromString("0.01"))},
	})
	if ranked[0].CandidateKey != "high-net" {
		t.Fatalf("%+v", ranked)
	}
}

func TestShouldHonorEnterRequiresPositiveNetReturn(t *testing.T) {
	cfg := testJevConfig(t)
	positive := honorEnterAnswer("0.001", "0.60", "0.50")
	positive.ExpectedNetReturn24h = decimalPtr(decimal.Zero)
	if ShouldHonorEnter(cfg, positive) {
		t.Fatal("zero net return must not honor enter")
	}
	positive.ExpectedNetReturn24h = decimalPtr(decimal.RequireFromString("-0.001"))
	if ShouldHonorEnter(cfg, positive) {
		t.Fatal("negative net return must not honor enter")
	}
	if !ShouldHonorEnter(cfg, honorEnterAnswer("0.001", "0.60", "0.50")) {
		t.Fatal("positive net return at thresholds must honor enter")
	}
}

func TestEnterRejectReasonProbabilityAndConfidence(t *testing.T) {
	cfg := testJevConfig(t)
	if got := EnterRejectReason(cfg, honorEnterAnswer("0.001", "0.59", "0.80")); got != "enter_probability_below_minimum" {
		t.Fatalf("prob reason=%s", got)
	}
	if got := EnterRejectReason(cfg, honorEnterAnswer("0.001", "0.80", "0.49")); got != "enter_confidence_below_minimum" {
		t.Fatalf("conf reason=%s", got)
	}
	if got := EnterRejectReason(cfg, honorEnterAnswer("0.001", "0.60", "0.50")); got != "" {
		t.Fatalf("accept reason=%s", got)
	}
}

func honorEnterAnswer(net, prob, conf string) ModelAnswer {
	return ModelAnswer{
		Decision: DecisionEnter, ExpectedNetReturn24h: decimalPtr(decimal.RequireFromString(net)),
		EnterProbability: decimal.RequireFromString(prob), Confidence: decimal.RequireFromString(conf),
	}
}

func TestAllocateKeepsRejectedNonPositiveEnter(t *testing.T) {
	cfg := testJevConfig(t)
	got := AllocateEnters(cfg, nil, []Decision{{
		CandidateKey: "single:binance:BTCUSDT:BTCUSDT", Kind: KindSingle, Decision: DecisionEnter,
		PolicyStatus: PolicyRejected, ExecutionStatus: ExecNotApplicable, BaseAsset: "BTC",
		ErrorMessage: "non_positive_expected_net_return_24h", ExpectedNetReturn24h: decimalPtr(decimal.RequireFromString("-0.001")),
	}}, nil)
	if len(got) != 1 || got[0].PolicyStatus != PolicyRejected || got[0].ExecutionStatus != ExecNotApplicable {
		t.Fatalf("%+v", got)
	}
}

func TestNonPositiveExpectedNetReturnEmptyReason(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	cfg := testJevConfig(t)
	rate := sampleRate("binance", "BTCUSDT", "BTC", "0.001")
	rate.SourceUpdatedAt = now
	quotes := map[string]Quote{
		quoteKey("binance", "BTCUSDT", ProductPerpetual): freshQuote("binance", "BTCUSDT", ProductPerpetual, now, "50000"),
		quoteKey("binance", "BTCUSDT", ProductSpot):      freshQuote("binance", "BTCUSDT", ProductSpot, now, "49990"),
	}
	spots := map[string]SpotInstrument{
		spotKey("binance", "BTC", "USDT"): {Exchange: "binance", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT"},
	}
	set := BuildEntryCandidates(cfg, CandidateInputs{
		Now: now, Rates: []Rate{rate}, Quotes: quotes, Spots: spots, Forecast: okForecast(now),
	})
	if len(set.Single) != 0 || set.SingleEmptyReason != "non_positive_expected_net_return_24h" {
		t.Fatalf("single=%d reason=%s stats=%+v", len(set.Single), set.SingleEmptyReason, set.SingleStats)
	}
	if set.SingleStats.DepthPassed == 0 || set.SingleStats.Final != 0 {
		t.Fatalf("funnel %+v", set.SingleStats)
	}
}

func TestFinalCandidatesSortedByExpectedNetReturn(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	cfg := testJevConfig(t)
	low := sampleRate("binance", "BTCUSDT", "BTC", "0.002")
	high := sampleRate("okx", "ETHUSDT", "ETH", "0.005")
	low.SourceUpdatedAt = now
	high.SourceUpdatedAt = now
	quotes, spots := singleMarket(now, "binance", "BTC", "BTCUSDT")
	q, s := singleMarket(now, "okx", "ETH", "ETHUSDT")
	for k, v := range q {
		quotes[k] = v
	}
	for k, v := range s {
		spots[k] = v
	}
	set := BuildEntryCandidates(cfg, CandidateInputs{
		Now: now, Rates: []Rate{low, high}, Quotes: quotes, Spots: spots, Forecast: okForecast(now),
	})
	if len(set.Single) != 2 {
		t.Fatalf("single=%d", len(set.Single))
	}
	if set.Single[0].BaseAsset != "ETH" || !set.Single[0].ExpectedNetReturn24h.GreaterThan(set.Single[1].ExpectedNetReturn24h) {
		t.Fatalf("%+v", set.Single)
	}
}

func quoteKey(exchange, symbol, product string) string {
	return NormalizeMarketKey(exchange, symbol, product).QuoteKey()
}

func sampleRate(exchange, symbol, base, cumulative string) Rate {
	complete := true
	return Rate{
		Exchange: exchange, ExchangeSymbol: symbol, GlobalSymbol: symbol, BaseAsset: base, QuoteAsset: "USDT",
		FundingRate: decimal.RequireFromString("0.0001"), Cumulative24h: decimal.RequireFromString(cumulative),
		Turnover24hUSD: decimal.NewFromInt(5_000_000), History24hComplete: complete, VenueContractType: VenuePerpetual,
	}
}

func freshQuote(exchange, symbol, product string, now time.Time, price string) Quote {
	px := decimal.RequireFromString(price)
	return Quote{
		Key: MarketKey{Venue: exchange, Symbol: symbol, Product: product, Exchange: exchange},
		Bid: px, Ask: px.Add(decimal.NewFromInt(1)), BidQty: decimal.NewFromInt(10), AskQty: decimal.NewFromInt(10),
		TS: now, Available: true,
	}
}

func testJevConfig(t *testing.T) config.Jev {
	t.Helper()
	return config.Jev{
		Enabled: true, TypeSafeAPIKey: "test-key", TypeSafeBaseURL: "https://api.typesafe.ai/v1/systemone",
		TypeSafeModel: "gpt-5.4", TypeSafeTimeout: 3 * time.Second, CycleInterval: time.Minute,
		FillWait: 5 * time.Second, BBOMaxAge: time.Minute, FundingPriceMaxAge: time.Minute,
		RateMaxAge: 15 * time.Minute, MinTurnoverUSD: decimal.NewFromInt(1_000_000),
		MinDepthUSD: decimal.NewFromInt(1), PositionNotionalUSDT: decimal.NewFromInt(100),
		PaperRoundTripCostBPS: decimal.NewFromInt(12), PaperInitialEquity: decimal.NewFromInt(10000),
		MinExitProbability: decimal.RequireFromString("0.6"), MaxHoldDuration: 72 * time.Hour,
		MaxConcurrentDecisions: 2, MaxEntryDecisionsPerCycle: 8, MaxOpenPositions: 8, USDTUSDCOneToOne: true,
		BasisLookback: 7 * 24 * time.Hour, BasisForecastHorizon: 24 * time.Hour,
		BasisRefreshInterval: 10 * time.Minute, BasisCacheMaxAge: 20 * time.Minute,
		MinExpectedNetReturn24h: decimal.Zero, MinConservativeNetReturn24h: decimal.Zero,
		MinNetProfitProbability: decimal.Zero, MinBasisForecastConfidence: decimal.Zero,
		MinEnterProbability: decimal.RequireFromString("0.60"), MinEnterConfidence: decimal.RequireFromString("0.50"),
		BasisMatchBandBPS: decimal.NewFromInt(20), BasisConfidenceSampleTarget: 48,
		StopLossNetUSDT: decimal.NewFromInt(-1), TakeProfitPadUSDT: decimal.RequireFromString("0.05"),
		ExitConfirmCycles: 3, MissingBBOForceAfter: 15 * time.Minute,
		ProbabilitySumTolerance: decimal.RequireFromString("0.02"),
	}
}

func okForecast(now time.Time) func(long, short MarketKey) (BasisForecast, bool) {
	return func(MarketKey, MarketKey) (BasisForecast, bool) {
		return BasisForecast{
			OK: true, ExpectedBasisReturn24h: decimal.Zero, BasisReturnP10: decimal.Zero,
			BasisProfitProbability: decimal.Zero, Confidence: decimal.NewFromInt(1),
			SampleCount: 20, CalculatedAt: now, ModelVersion: BasisModelVersion,
		}, true
	}
}
