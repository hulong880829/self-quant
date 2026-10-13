package jev

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

type fakeMarket struct {
	rates    []Rate
	history  map[string][]HistoryPoint
	histErrs map[string]error
}

func (f *fakeMarket) ListRates(context.Context) ([]Rate, error) { return f.rates, nil }
func (f *fakeMarket) ListHistory(_ context.Context, exchange, symbol string, _ int) ([]HistoryPoint, error) {
	key := rateMapKey(exchange, symbol)
	if f.histErrs != nil {
		if err := f.histErrs[key]; err != nil {
			return nil, err
		}
	}
	return f.history[key], nil
}

type fakeQuotes struct {
	latest map[string]Quote
	series []TenMinuteBBO
	mids   map[string]struct {
		mid decimal.Decimal
		ts  time.Time
		ok  bool
	}
	midBefore   func(key MarketKey, settledAt time.Time) (decimal.Decimal, time.Time, bool, error)
	minuteCalls atomic.Int32
}

func (f *fakeQuotes) Latest(context.Context, []MarketKey) (map[string]Quote, error) {
	return f.latest, nil
}
func (f *fakeQuotes) MidBefore(_ context.Context, key MarketKey, settledAt time.Time, _ time.Duration) (decimal.Decimal, time.Time, bool, error) {
	if f.midBefore != nil {
		return f.midBefore(key, settledAt)
	}
	item, ok := f.mids[key.QuoteKey()]
	if !ok {
		return decimal.Zero, time.Time{}, false, nil
	}
	return item.mid, item.ts, item.ok && SettlementMidValid(settledAt, item.ts, time.Minute), nil
}

func (f *fakeQuotes) TenMinuteBBOs(_ context.Context, keys []MarketKey, from, untilExclusive time.Time) ([]TenMinuteBBO, error) {
	f.minuteCalls.Add(1)
	if f.series != nil {
		return f.series, nil
	}
	return synthesizeTenMinute(keys, from, untilExclusive, f.latest), nil
}

func synthesizeTenMinute(keys []MarketKey, from, untilExclusive time.Time, latest map[string]Quote) []TenMinuteBBO {
	out := make([]TenMinuteBBO, 0)
	start := from.UTC().Truncate(10 * time.Minute)
	if start.Before(from) {
		start = start.Add(10 * time.Minute)
	}
	for _, key := range keys {
		canonical := NewCanonicalMarketKey(key.Venue, key.Symbol, key.Product)
		quote, ok := latest[canonical.QuoteKey()]
		if !ok {
			continue
		}
		bid, ask := quote.Bid, quote.Ask
		if !bid.IsPositive() || !ask.IsPositive() {
			continue
		}
		for bucket := start; bucket.Before(untilExclusive); bucket = bucket.Add(10 * time.Minute) {
			out = append(out, TenMinuteBBO{Key: canonical, Bucket: bucket, Bid: bid, Ask: ask})
		}
	}
	return out
}

type clockQuotes struct {
	latest   map[string]Quote
	calls    atomic.Int32
	onLatest func(call int)
}

func (f *clockQuotes) Latest(context.Context, []MarketKey) (map[string]Quote, error) {
	call := int(f.calls.Add(1))
	if f.onLatest != nil {
		f.onLatest(call)
	}
	return f.latest, nil
}

func (f *clockQuotes) MidBefore(context.Context, MarketKey, time.Time, time.Duration) (decimal.Decimal, time.Time, bool, error) {
	return decimal.Zero, time.Time{}, false, nil
}

func (f *clockQuotes) TenMinuteBBOs(_ context.Context, keys []MarketKey, from, untilExclusive time.Time) ([]TenMinuteBBO, error) {
	return synthesizeTenMinute(keys, from, untilExclusive, f.latest), nil
}

type fakeSpots struct {
	items map[string]SpotInstrument
}

func (f *fakeSpots) ListSpots(context.Context) (map[string]SpotInstrument, error) {
	return f.items, nil
}

type scriptedModel struct {
	calls atomic.Int32
	fn    func(call int, request DecisionRequest) (ModelAnswer, error)
	abort atomic.Bool
}

func (m *scriptedModel) ResetCycle()   { m.abort.Store(false) }
func (m *scriptedModel) Aborted() bool { return m.abort.Load() }
func (m *scriptedModel) Decide(_ context.Context, request DecisionRequest) (ModelAnswer, error) {
	call := int(m.calls.Add(1))
	answer, err := m.fn(call, request)
	if errors.Is(err, ErrUnauthorized) {
		m.abort.Store(true)
	}
	return answer, err
}

func TestEnsureActiveRunReusesSingleRun(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	first, err := store.EnsureActiveRun(context.Background(), now, decimal.NewFromInt(10000), StrategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.EnsureActiveRun(context.Background(), now, decimal.NewFromInt(1), StrategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || !second.InitialEquity.Equal(decimal.NewFromInt(10000)) {
		t.Fatalf("runs diverged %+v %+v", first, second)
	}
}

func TestPendingEnterFillFailedDoesNotStayPending(t *testing.T) {
	cfg := testJevConfig(t)
	store := NewMemoryStore()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	run, err := store.EnsureActiveRun(context.Background(), now, cfg.PaperInitialEquity, StrategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	decisionAt := now.Add(-30 * time.Second)
	started := now.Add(-1 * time.Second)
	deadline := started.Add(cfg.FillWait)
	_, err = store.InsertDecision(context.Background(), Decision{
		RunID: run.ID, CandidateKey: "cross:binance:ETHUSDT:okx:ETHUSDT", Kind: KindCross,
		Decision: DecisionEnter, PolicyStatus: PolicyAccepted, ExecutionStatus: ExecPending,
		PromptVersion: PromptVersion, PolicyVersion: PolicyVersion, DecisionAt: decisionAt,
		ExecutionStartedAt: &started, FillDeadline: &deadline, BaseAsset: "ETH", QuoteAsset: "USDT",
		LongCanonicalSymbol: "ETHUSDT", LongQuoteAsset: "USDT",
		ShortCanonicalSymbol: "ETHUSDT", ShortQuoteAsset: "USDT",
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(cfg, store, &fakeMarket{}, &fakeQuotes{latest: map[string]Quote{}}, &fakeSpots{}, nil, nil)
	engine.now = func() time.Time { return now }
	if err := engine.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListPendingDecisions(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("enter fill failure must not stay pending, pending=%d", len(pending))
	}
	decisions, _, err := store.ListDecisionsPage(context.Background(), run.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].ExecutionStatus != ExecFillFailed {
		t.Fatalf("%+v", decisions)
	}
	if decisions[0].ErrorMessage != "missing fresh two-leg bbo" {
		t.Fatalf("error=%q", decisions[0].ErrorMessage)
	}
	if decisions[0].FillDeadline == nil || !decisions[0].FillDeadline.Equal(deadline) {
		t.Fatalf("deadline mutated %+v want %v", decisions[0].FillDeadline, deadline)
	}
}

func TestUnauthorizedStopsRemainingModelCalls(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	rateA := sampleRate("binance", "BTCUSDT", "BTC", "0.002")
	rateB := sampleRate("okx", "ETHUSDT", "ETH", "0.0015")
	rateA.SourceUpdatedAt = now
	rateB.SourceUpdatedAt = now
	quotes := map[string]Quote{
		quoteKey("binance", "BTCUSDT", ProductPerpetual): freshQuote("binance", "BTCUSDT", ProductPerpetual, now, "50000"),
		quoteKey("binance", "BTCUSDT", ProductSpot):      freshQuote("binance", "BTCUSDT", ProductSpot, now, "49990"),
		quoteKey("okx", "ETHUSDT", ProductPerpetual):     freshQuote("okx", "ETHUSDT", ProductPerpetual, now, "2000"),
		quoteKey("okx", "ETHUSDT", ProductSpot):          freshQuote("okx", "ETHUSDT", ProductSpot, now, "1999"),
	}
	spots := map[string]SpotInstrument{
		spotKey("binance", "BTC", "USDT"): {Exchange: "binance", ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT"},
		spotKey("okx", "ETH", "USDT"):     {Exchange: "okx", ExchangeSymbol: "ETHUSDT", BaseAsset: "ETH", QuoteAsset: "USDT"},
	}
	model := &scriptedModel{fn: func(call int, _ DecisionRequest) (ModelAnswer, error) {
		if call == 1 {
			return ModelAnswer{}, ErrUnauthorized
		}
		t.Fatal("second model call must not happen after 401")
		return ModelAnswer{}, nil
	}}
	cfg.MaxConcurrentDecisions = 1
	store := NewMemoryStore()
	engine := NewEngine(cfg, store, &fakeMarket{rates: []Rate{rateA, rateB}}, &fakeQuotes{latest: quotes}, &fakeSpots{items: spots}, model, nil)
	engine.now = func() time.Time { return now }
	if err := engine.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if model.calls.Load() != 1 {
		t.Fatalf("calls=%d", model.calls.Load())
	}
}

func TestNoAPIKeyStillForceExits(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.TypeSafeAPIKey = ""
	cfg.MaxHoldDuration = time.Hour
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	run, _ := store.EnsureActiveRun(context.Background(), now, cfg.PaperInitialEquity, StrategyVersion)
	_, err := store.InsertPosition(context.Background(), Position{
		RunID: run.ID, Kind: KindCross, Status: PositionOpen, CandidateKey: "cross:binance:ETHUSDT:okx:ETHUSDT",
		BaseAsset: "ETH", QuoteAsset: "USDT", GlobalSymbol: "ETHUSDT",
		LongExchange: "binance", LongSymbol: "ETHUSDT", LongProduct: ProductPerpetual,
		LongCanonicalSymbol: "ETHUSDT", LongQuoteAsset: "USDT",
		ShortExchange: "okx", ShortSymbol: "ETHUSDT", ShortProduct: ProductPerpetual,
		ShortCanonicalSymbol: "ETHUSDT", ShortQuoteAsset: "USDT",
		SharedBaseQty: decimal.NewFromInt(1), LongQty: decimal.NewFromInt(1), ShortQty: decimal.NewFromInt(1).Neg(),
		LongEntryPrice: decimal.NewFromInt(2000), ShortEntryPrice: decimal.NewFromInt(2001),
		OpenedAt: now.Add(-2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	quotes := map[string]Quote{
		quoteKey("binance", "ETHUSDT", ProductPerpetual): freshQuote("binance", "ETHUSDT", ProductPerpetual, now, "2000"),
		quoteKey("okx", "ETHUSDT", ProductPerpetual):     freshQuote("okx", "ETHUSDT", ProductPerpetual, now, "2001"),
	}
	engine := NewEngine(cfg, store, &fakeMarket{}, &fakeQuotes{latest: quotes}, &fakeSpots{}, nil, nil)
	engine.now = func() time.Time { return now }
	if err := engine.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	open, err := store.ListOpenPositions(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("force exit should close position, open=%d", len(open))
	}
}

func TestPromptIncludesConfiguredCost(t *testing.T) {
	cfg := testJevConfig(t)
	prompt := BuildDecisionPrompt(cfg, Candidate{Key: "k", Kind: KindSingle, BaseAsset: "BTC"}, nil, time.Now().UTC(), false)
	if !containsAll(prompt, "12", "0.12", "DecisionPromptV4", "expected_net_return_24h", "current_mid_basis_bps") {
		t.Fatalf("prompt missing cost fields: %s", prompt)
	}
}

func TestIncompleteHistoryCanReachModel(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	rate := sampleRate("binance", "BTCUSDT", "BTC", "0.002")
	rate.SourceUpdatedAt = now
	rate.History24hComplete = false
	quotes, spots := singleMarket(now, "binance", "BTC", "BTCUSDT")
	model := &scriptedModel{fn: func(_ int, _ DecisionRequest) (ModelAnswer, error) {
		return ModelAnswer{Decision: DecisionHold, ModelActual: "m"}, nil
	}}
	store := NewMemoryStore()
	engine := NewEngine(cfg, store, &fakeMarket{rates: []Rate{rate}}, &fakeQuotes{latest: quotes}, &fakeSpots{items: spots}, model, nil)
	engine.now = func() time.Time { return now }
	var snap Snapshot
	engine.SetSnapshotHandler(func(s Snapshot) { snap = s })
	if err := engine.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if model.calls.Load() != 1 {
		t.Fatalf("calls=%d snap=%+v", model.calls.Load(), snap)
	}
	if snap.SingleFilter.Final == 0 || snap.SingleEntryEmptyReason != "" {
		t.Fatalf("incomplete history should produce a candidate, snap=%+v", snap)
	}
	if snap.ModelStatus != ModelStatusSuccess {
		t.Fatalf("model status=%s snap=%+v", snap.ModelStatus, snap)
	}
	if snap.SingleEntryEmptyReason == "history_incomplete" {
		t.Fatalf("history_incomplete must not appear, snap=%+v", snap)
	}
}

func TestEntryDecisionCapLimitsNewEntryCalls(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.MaxConcurrentDecisions = 2
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	rates, quotes, spots := manySingleMarkets(now, 9)
	model := &scriptedModel{fn: func(_ int, request DecisionRequest) (ModelAnswer, error) {
		if !request.AllowEnter {
			t.Fatal("entry-only fixture must not ask hold")
		}
		return ModelAnswer{Decision: DecisionHold, ModelActual: "m"}, nil
	}}
	store := NewMemoryStore()
	engine := NewEngine(cfg, store, &fakeMarket{rates: rates}, &fakeQuotes{latest: quotes}, &fakeSpots{items: spots}, model, nil)
	engine.now = func() time.Time { return now }
	var snap Snapshot
	engine.SetSnapshotHandler(func(s Snapshot) { snap = s })
	if err := engine.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if model.calls.Load() != 8 {
		t.Fatalf("calls=%d", model.calls.Load())
	}
	if snap.TypeSafeRequests != 8 || snap.EntryCandidateCount != 9 {
		t.Fatalf("requests=%d candidates=%d", snap.TypeSafeRequests, snap.EntryCandidateCount)
	}
}

func TestHoldJobsAreNotCapped(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	rates, quotes, spots := manySingleMarkets(now, 9)
	quotes[quoteKey("okx", "ETHUSDT", ProductPerpetual)] = freshQuote("okx", "ETHUSDT", ProductPerpetual, now, "2000")
	quotes[quoteKey("binance", "ETHUSDT", ProductPerpetual)] = freshQuote("binance", "ETHUSDT", ProductPerpetual, now, "2000")
	store := NewMemoryStore()
	run, err := store.EnsureActiveRun(context.Background(), now, cfg.PaperInitialEquity, StrategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.InsertPosition(context.Background(), Position{
		RunID: run.ID, Kind: KindCross, Status: PositionOpen, CandidateKey: "cross:binance:ETHUSDT:okx:ETHUSDT",
		BaseAsset: "ETH", QuoteAsset: "USDT", GlobalSymbol: "ETHUSDT",
		LongExchange: "binance", LongSymbol: "ETHUSDT", LongProduct: ProductPerpetual,
		ShortExchange: "okx", ShortSymbol: "ETHUSDT", ShortProduct: ProductPerpetual,
		SharedBaseQty: decimal.NewFromInt(1), LongQty: decimal.NewFromInt(1), ShortQty: decimal.NewFromInt(1).Neg(),
		LongEntryPrice: decimal.NewFromInt(2000), ShortEntryPrice: decimal.NewFromInt(2001),
		OpenedAt: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	var holdCalls, enterCalls atomic.Int32
	model := &scriptedModel{fn: func(_ int, request DecisionRequest) (ModelAnswer, error) {
		if request.AllowEnter {
			enterCalls.Add(1)
		} else {
			holdCalls.Add(1)
		}
		return ModelAnswer{Decision: DecisionHold, ModelActual: "m"}, nil
	}}
	engine := NewEngine(cfg, store, &fakeMarket{rates: rates}, &fakeQuotes{latest: quotes}, &fakeSpots{items: spots}, model, nil)
	engine.now = func() time.Time { return now }
	if err := engine.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if enterCalls.Load() != 8 {
		t.Fatalf("enter=%d", enterCalls.Load())
	}
	if holdCalls.Load() != 1 {
		t.Fatalf("hold=%d", holdCalls.Load())
	}
	if model.calls.Load() != 9 {
		t.Fatalf("total=%d", model.calls.Load())
	}
}

func TestZeroEntryCandidatesDoesNotCallModel(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	model := &scriptedModel{fn: func(_ int, _ DecisionRequest) (ModelAnswer, error) {
		t.Fatal("model must not be called without candidates")
		return ModelAnswer{}, nil
	}}
	store := NewMemoryStore()
	engine := NewEngine(cfg, store, &fakeMarket{}, &fakeQuotes{latest: map[string]Quote{}}, &fakeSpots{}, model, nil)
	engine.now = func() time.Time { return now }
	var snap Snapshot
	engine.SetSnapshotHandler(func(s Snapshot) { snap = s })
	if err := engine.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if model.calls.Load() != 0 || snap.ModelStatus != ModelStatusNotCalled || snap.TypeSafeRequests != 0 {
		t.Fatalf("calls=%d snap=%+v", model.calls.Load(), snap)
	}
	if snap.Model != "" {
		t.Fatalf("model should stay empty, got %q", snap.Model)
	}
	if snap.LastDecisionAt != nil {
		t.Fatalf("lastDecisionAt=%v", snap.LastDecisionAt)
	}
}

func TestTypeSafeErrorMarksDegraded(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	rate := sampleRate("binance", "BTCUSDT", "BTC", "0.002")
	rate.SourceUpdatedAt = now
	quotes, spots := singleMarket(now, "binance", "BTC", "BTCUSDT")
	model := &scriptedModel{fn: func(_ int, _ DecisionRequest) (ModelAnswer, error) {
		return ModelAnswer{}, errors.New("typesafe timeout")
	}}
	store := NewMemoryStore()
	engine := NewEngine(cfg, store, &fakeMarket{rates: []Rate{rate}}, &fakeQuotes{latest: quotes}, &fakeSpots{items: spots}, model, nil)
	engine.now = func() time.Time { return now }
	var snap Snapshot
	engine.SetSnapshotHandler(func(s Snapshot) { snap = s })
	if err := engine.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snap.Status != StatusDegraded || snap.ModelStatus != ModelStatusError || snap.TypeSafeError != 1 {
		t.Fatalf("%+v", snap)
	}
}

func TestLastSuccessfulModelActualWins(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.MaxConcurrentDecisions = 1
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	rateA := sampleRate("binance", "BTCUSDT", "BTC", "0.003")
	rateB := sampleRate("okx", "SOLUSDT", "SOL", "0.002")
	rateA.SourceUpdatedAt = now
	rateB.SourceUpdatedAt = now
	quotes := map[string]Quote{}
	spots := map[string]SpotInstrument{}
	q, s := singleMarket(now, "binance", "BTC", "BTCUSDT")
	for k, v := range q {
		quotes[k] = v
	}
	for k, v := range s {
		spots[k] = v
	}
	q, s = singleMarket(now, "okx", "SOL", "SOLUSDT")
	for k, v := range q {
		quotes[k] = v
	}
	for k, v := range s {
		spots[k] = v
	}
	model := &scriptedModel{fn: func(call int, _ DecisionRequest) (ModelAnswer, error) {
		name := "first"
		if call == 2 {
			name = "second"
		}
		return ModelAnswer{Decision: DecisionHold, ModelActual: name}, nil
	}}
	store := NewMemoryStore()
	engine := NewEngine(cfg, store, &fakeMarket{rates: []Rate{rateA, rateB}}, &fakeQuotes{latest: quotes}, &fakeSpots{items: spots}, model, nil)
	engine.now = func() time.Time { return now }
	var snap Snapshot
	engine.SetSnapshotHandler(func(s Snapshot) { snap = s })
	if err := engine.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snap.Model != "second" || snap.ModelStatus != ModelStatusSuccess {
		t.Fatalf("%+v", snap)
	}
}

func TestUnauthorizedStatusOutranksSuccess(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.MaxConcurrentDecisions = 1
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	rateA := sampleRate("binance", "BTCUSDT", "BTC", "0.003")
	rateB := sampleRate("okx", "SOLUSDT", "SOL", "0.002")
	rateA.SourceUpdatedAt = now
	rateB.SourceUpdatedAt = now
	quotes := map[string]Quote{}
	spots := map[string]SpotInstrument{}
	q, s := singleMarket(now, "binance", "BTC", "BTCUSDT")
	for k, v := range q {
		quotes[k] = v
	}
	for k, v := range s {
		spots[k] = v
	}
	q, s = singleMarket(now, "okx", "SOL", "SOLUSDT")
	for k, v := range q {
		quotes[k] = v
	}
	for k, v := range s {
		spots[k] = v
	}
	model := &scriptedModel{fn: func(call int, _ DecisionRequest) (ModelAnswer, error) {
		if call == 1 {
			return ModelAnswer{Decision: DecisionHold, ModelActual: "ok"}, nil
		}
		return ModelAnswer{}, ErrUnauthorized
	}}
	store := NewMemoryStore()
	engine := NewEngine(cfg, store, &fakeMarket{rates: []Rate{rateA, rateB}}, &fakeQuotes{latest: quotes}, &fakeSpots{items: spots}, model, nil)
	engine.now = func() time.Time { return now }
	var snap Snapshot
	engine.SetSnapshotHandler(func(s Snapshot) { snap = s })
	if err := engine.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snap.ModelStatus != ModelStatusUnauthorized || snap.Status != StatusDegraded {
		t.Fatalf("%+v", snap)
	}
	if snap.Model != "ok" {
		t.Fatalf("keep last success actual, got %q", snap.Model)
	}
}

func TestLockBusySkipsSnapshotAndTimestamps(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	run, err := store.EnsureActiveRun(context.Background(), now, cfg.PaperInitialEquity, StrategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	success := now.Add(-time.Minute)
	run.LastSuccessfulAt = &success
	if err := store.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	lease, ok, err := store.AcquireCycleLock(context.Background())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	defer func() { _ = lease.Release() }()
	engine := NewEngine(cfg, store, &fakeMarket{}, &fakeQuotes{latest: map[string]Quote{}}, &fakeSpots{}, nil, nil)
	engine.now = func() time.Time { return now }
	engine.SetSnapshotHandler(func(Snapshot) { t.Fatal("lock_busy must not publish") })
	if err := engine.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := store.ActiveRun(context.Background(), StrategyVersion)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.LastSuccessfulAt == nil || !got.LastSuccessfulAt.Equal(success) {
		t.Fatalf("lastSuccessfulAt %+v", got.LastSuccessfulAt)
	}
	snap, err := store.LatestSnapshot(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap != nil {
		t.Fatalf("snapshot published %+v", snap)
	}
	if got.SnapshotVersion != 0 {
		t.Fatalf("version=%d", got.SnapshotVersion)
	}
}

func TestEnterFillUsesQuoteTimeAfterCycleStart(t *testing.T) {
	cfg := testJevConfig(t)
	start := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	bboTS := start.Add(2 * time.Second)
	fillNow := start.Add(3 * time.Second)
	clock := start
	rate := sampleRate("binance", "BTCUSDT", "BTC", "0.002")
	rate.SourceUpdatedAt = start
	universeQuotes, spots := singleMarket(start, "binance", "BTC", "BTCUSDT")
	execQuotes, _ := singleMarket(bboTS, "binance", "BTC", "BTCUSDT")
	source := &clockQuotes{latest: universeQuotes}
	source.onLatest = func(call int) {
		if call >= 2 {
			clock = fillNow
			source.latest = execQuotes
		}
	}
	model := &scriptedModel{fn: func(_ int, _ DecisionRequest) (ModelAnswer, error) {
		return ModelAnswer{
			Decision: DecisionEnter, EnterProbability: decimal.RequireFromString("0.8"),
			Confidence: decimal.RequireFromString("0.7"), ModelActual: "m",
		}, nil
	}}
	store := NewMemoryStore()
	engine := NewEngine(cfg, store, &fakeMarket{rates: []Rate{rate}}, source, &fakeSpots{items: spots}, model, nil)
	engine.now = func() time.Time { return clock }
	if err := engine.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, err := store.ActiveRun(context.Background(), StrategyVersion)
	if err != nil || run == nil {
		t.Fatal(err)
	}
	open, err := store.ListOpenPositions(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("expected fill, open=%d", len(open))
	}
	if !open[0].OpenedAt.Equal(fillNow) {
		t.Fatalf("openedAt=%v fillNow=%v", open[0].OpenedAt, fillNow)
	}
}

func TestPendingExitRetryDoesNotExtendFillDeadline(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	run, err := store.EnsureActiveRun(context.Background(), now, cfg.PaperInitialEquity, StrategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	position, err := store.InsertPosition(context.Background(), Position{
		RunID: run.ID, Kind: KindCross, Status: PositionOpen, CandidateKey: "cross:binance:ETHUSDT:okx:ETHUSDT",
		BaseAsset: "ETH", QuoteAsset: "USDT", GlobalSymbol: "ETHUSDT",
		LongExchange: "binance", LongSymbol: "ETHUSDT", LongProduct: ProductPerpetual,
		ShortExchange: "okx", ShortSymbol: "ETHUSDT", ShortProduct: ProductPerpetual,
		SharedBaseQty: decimal.NewFromInt(1), LongQty: decimal.NewFromInt(1), ShortQty: decimal.NewFromInt(1).Neg(),
		LongEntryPrice: decimal.NewFromInt(2000), ShortEntryPrice: decimal.NewFromInt(2001),
		OpenedAt: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	started := now.Add(-2 * time.Second)
	deadline := started.Add(cfg.FillWait)
	_, err = store.InsertDecision(context.Background(), Decision{
		RunID: run.ID, PositionID: ptrInt(position.ID), CandidateKey: position.CandidateKey,
		Kind: KindCross, Decision: DecisionExit, PolicyStatus: PolicyAccepted, ExecutionStatus: ExecPending,
		PromptVersion: PromptVersion, PolicyVersion: PolicyVersion, DecisionAt: started,
		ExecutionStartedAt: &started, FillDeadline: &deadline, BaseAsset: "ETH",
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(cfg, store, &fakeMarket{}, &fakeQuotes{latest: map[string]Quote{}}, &fakeSpots{}, nil, nil)
	engine.now = func() time.Time { return now }
	if err := engine.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListPendingDecisions(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].FillDeadline == nil || !pending[0].FillDeadline.Equal(deadline) {
		t.Fatalf("deadline rewritten %+v", pending)
	}
	if pending[0].ExecutionStartedAt == nil || !pending[0].ExecutionStartedAt.Equal(started) {
		t.Fatalf("started rewritten %+v", pending[0].ExecutionStartedAt)
	}
}

func TestForecastFailureDoesNotBlockForceExit(t *testing.T) {
	cfg := testJevConfig(t)
	cfg.TypeSafeAPIKey = ""
	cfg.MaxHoldDuration = time.Hour
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	run, err := store.EnsureActiveRun(context.Background(), now, cfg.PaperInitialEquity, StrategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.InsertPosition(context.Background(), Position{
		RunID: run.ID, Kind: KindCross, Status: PositionOpen, CandidateKey: "cross:binance:ETHUSDT:okx:ETHUSDT",
		BaseAsset: "ETH", QuoteAsset: "USDT", GlobalSymbol: "ETHUSDT",
		LongExchange: "binance", LongSymbol: "ETHUSDT", LongProduct: ProductPerpetual,
		LongCanonicalSymbol: "ETHUSDT", LongQuoteAsset: "USDT",
		ShortExchange: "okx", ShortSymbol: "ETHUSDT", ShortProduct: ProductPerpetual,
		ShortCanonicalSymbol: "ETHUSDT", ShortQuoteAsset: "USDT",
		SharedBaseQty: decimal.NewFromInt(1), LongQty: decimal.NewFromInt(1), ShortQty: decimal.NewFromInt(1).Neg(),
		LongEntryPrice: decimal.NewFromInt(2000), ShortEntryPrice: decimal.NewFromInt(2001),
		OpenedAt: now.Add(-2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	quotes := map[string]Quote{
		quoteKey("binance", "ETHUSDT", ProductPerpetual): freshQuote("binance", "ETHUSDT", ProductPerpetual, now, "2000"),
		quoteKey("okx", "ETHUSDT", ProductPerpetual):     freshQuote("okx", "ETHUSDT", ProductPerpetual, now, "2001"),
	}
	engine := NewEngine(cfg, store, &fakeMarket{}, &fakeQuotes{latest: quotes, series: []TenMinuteBBO{}}, &fakeSpots{}, nil, nil)
	engine.now = func() time.Time { return now }
	if err := engine.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	open, err := store.ListOpenPositions(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("force exit must proceed without basis forecast, open=%d", len(open))
	}
}

func TestAskModelRejectsNonPositiveEnter(t *testing.T) {
	cfg := testJevConfig(t)
	store := NewMemoryStore()
	model := &scriptedModel{fn: func(_ int, _ DecisionRequest) (ModelAnswer, error) {
		return ModelAnswer{
			Decision: DecisionEnter, EnterProbability: decimal.RequireFromString("0.9"),
			Confidence: decimal.RequireFromString("0.8"), ModelActual: "m",
		}, nil
	}}
	engine := NewEngine(cfg, store, &fakeMarket{}, &fakeQuotes{latest: map[string]Quote{}}, &fakeSpots{}, model, nil)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	run, err := store.EnsureActiveRun(context.Background(), now, cfg.PaperInitialEquity, StrategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := engine.askModel(context.Background(), run, decisionJob{
		candidate:  Candidate{Key: "single:binance:BTCUSDT:BTCUSDT", Kind: KindSingle, BaseAsset: "BTC", Edge24h: decimal.RequireFromString("0.0001")},
		allowEnter: true,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if decision.PolicyStatus != PolicyRejected || decision.ExecutionStatus != ExecNotApplicable ||
		decision.ErrorMessage != "expected_net_return_below_minimum" || decision.Decision != DecisionEnter {
		t.Fatalf("%+v", decision)
	}
}

func TestNonPositiveCandidatesSkipModel(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	rate := sampleRate("binance", "BTCUSDT", "BTC", "0.001")
	rate.SourceUpdatedAt = now
	quotes, spots := singleMarket(now, "binance", "BTC", "BTCUSDT")
	model := &scriptedModel{fn: func(_ int, _ DecisionRequest) (ModelAnswer, error) {
		t.Fatal("model must not be called for non-positive net return")
		return ModelAnswer{}, nil
	}}
	store := NewMemoryStore()
	engine := NewEngine(cfg, store, &fakeMarket{rates: []Rate{rate}}, &fakeQuotes{latest: quotes}, &fakeSpots{items: spots}, model, nil)
	engine.now = func() time.Time { return now }
	var snap Snapshot
	engine.SetSnapshotHandler(func(s Snapshot) { snap = s })
	if err := engine.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if model.calls.Load() != 0 || snap.SingleFilter.Final != 0 || snap.SingleEntryEmptyReason != "non_positive_expected_net_return_24h" {
		t.Fatalf("calls=%d snap=%+v", model.calls.Load(), snap)
	}
	if snap.LastDecisionAt != nil {
		t.Fatalf("lastDecisionAt=%v", snap.LastDecisionAt)
	}
	if snap.LastMarketDataAt == nil || !snap.LastMarketDataAt.Equal(now) {
		t.Fatalf("lastMarketDataAt %+v", snap.LastMarketDataAt)
	}
}

func TestLastDecisionAtAdvancesOnlyWhenPersisted(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	rate := sampleRate("binance", "BTCUSDT", "BTC", "0.002")
	rate.SourceUpdatedAt = now
	quotes, spots := singleMarket(now, "binance", "BTC", "BTCUSDT")
	model := &scriptedModel{fn: func(_ int, _ DecisionRequest) (ModelAnswer, error) {
		return ModelAnswer{Decision: DecisionHold, ModelActual: "m"}, nil
	}}
	store := NewMemoryStore()
	engine := NewEngine(cfg, store, &fakeMarket{rates: []Rate{rate}}, &fakeQuotes{latest: quotes}, &fakeSpots{items: spots}, model, nil)
	engine.now = func() time.Time { return now }
	var snap Snapshot
	engine.SetSnapshotHandler(func(s Snapshot) { snap = s })
	if err := engine.RunCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snap.LastDecisionAt == nil || !snap.LastDecisionAt.Equal(now) {
		t.Fatalf("lastDecisionAt %+v", snap.LastDecisionAt)
	}
}

func TestAskModelRejectsLowEnterProbabilityAndConfidence(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	run, err := store.EnsureActiveRun(context.Background(), now, cfg.PaperInitialEquity, StrategyVersion)
	if err != nil {
		t.Fatal(err)
	}
	job := decisionJob{
		candidate: Candidate{
			Key: "single:binance:BTCUSDT:BTCUSDT", Kind: KindSingle, BaseAsset: "BTC",
			ExpectedNetReturn24h: decimal.RequireFromString("0.01"),
		},
		allowEnter: true,
	}
	lowProb := &scriptedModel{fn: func(_ int, _ DecisionRequest) (ModelAnswer, error) {
		return ModelAnswer{
			Decision: DecisionEnter, EnterProbability: decimal.RequireFromString("0.59"),
			Confidence: decimal.RequireFromString("0.80"), ModelActual: "m",
		}, nil
	}}
	engine := NewEngine(cfg, store, &fakeMarket{}, &fakeQuotes{}, &fakeSpots{}, lowProb, nil)
	decision, err := engine.askModel(context.Background(), run, job, now)
	if err != nil {
		t.Fatal(err)
	}
	if decision.ErrorMessage != "enter_probability_below_minimum" || decision.PolicyStatus != PolicyRejected {
		t.Fatalf("%+v", decision)
	}
	lowConf := &scriptedModel{fn: func(_ int, _ DecisionRequest) (ModelAnswer, error) {
		return ModelAnswer{
			Decision: DecisionEnter, EnterProbability: decimal.RequireFromString("0.80"),
			Confidence: decimal.RequireFromString("0.49"), ModelActual: "m",
		}, nil
	}}
	engine = NewEngine(cfg, store, &fakeMarket{}, &fakeQuotes{}, &fakeSpots{}, lowConf, nil)
	decision, err = engine.askModel(context.Background(), run, job, now)
	if err != nil {
		t.Fatal(err)
	}
	if decision.ErrorMessage != "enter_confidence_below_minimum" || decision.PolicyStatus != PolicyRejected {
		t.Fatalf("%+v", decision)
	}
	okModel := &scriptedModel{fn: func(_ int, _ DecisionRequest) (ModelAnswer, error) {
		return ModelAnswer{
			Decision: DecisionEnter, EnterProbability: decimal.RequireFromString("0.60"),
			Confidence: decimal.RequireFromString("0.50"), ModelActual: "m",
		}, nil
	}}
	engine = NewEngine(cfg, store, &fakeMarket{}, &fakeQuotes{}, &fakeSpots{}, okModel, nil)
	decision, err = engine.askModel(context.Background(), run, job, now)
	if err != nil {
		t.Fatal(err)
	}
	if decision.PolicyStatus != PolicySuggested || decision.ErrorMessage != "" {
		t.Fatalf("%+v", decision)
	}
}

func TestCatchupAppliesDESCHistoryAscending(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	opened := now.Add(-24 * time.Hour)
	t1 := opened.Add(8 * time.Hour)
	t2 := opened.Add(16 * time.Hour)
	position := sampleCatchupPosition(opened)
	engine := NewEngine(cfg, NewMemoryStore(), &fakeMarket{
		history: map[string][]HistoryPoint{
			rateMapKey("binance", "BTCUSDT"): {
				{SettledAt: t2, Rate: decimal.RequireFromString("0.002")},
				{SettledAt: t1, Rate: decimal.RequireFromString("0.001")},
			},
		},
	}, &fakeQuotes{midBefore: validSettlementMid}, &fakeSpots{}, nil, nil)
	var degraded []string
	got := engine.catchupFunding(context.Background(), position, now, &degraded)
	want := FundingPnLDelta(position.ShortQty, decimal.NewFromInt(100), decimal.RequireFromString("0.001")).
		Add(FundingPnLDelta(position.ShortQty, decimal.NewFromInt(100), decimal.RequireFromString("0.002")))
	if !got.FundingPnL.Equal(want) {
		t.Fatalf("pnl=%s want=%s (DESC-only would skip older)", got.FundingPnL, want)
	}
	if got.FundingCheckedThroughShort == nil || !got.FundingCheckedThroughShort.Equal(t2) {
		t.Fatalf("watermark=%v", got.FundingCheckedThroughShort)
	}
	if got.DegradedReason != "" || len(degraded) != 0 {
		t.Fatalf("degraded=%s extra=%v", got.DegradedReason, degraded)
	}
}

func TestCatchupStopsAtMissingBBOGap(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	opened := now.Add(-24 * time.Hour)
	t1 := opened.Add(4 * time.Hour)
	t2 := opened.Add(8 * time.Hour)
	t3 := opened.Add(12 * time.Hour)
	position := sampleCatchupPosition(opened)
	engine := NewEngine(cfg, NewMemoryStore(), &fakeMarket{
		history: map[string][]HistoryPoint{
			rateMapKey("binance", "BTCUSDT"): {
				{SettledAt: t3, Rate: decimal.RequireFromString("0.001")},
				{SettledAt: t2, Rate: decimal.RequireFromString("0.001")},
				{SettledAt: t1, Rate: decimal.RequireFromString("0.001")},
			},
		},
	}, &fakeQuotes{midBefore: func(_ MarketKey, settledAt time.Time) (decimal.Decimal, time.Time, bool, error) {
		if settledAt.Equal(t2) {
			return decimal.Zero, time.Time{}, false, nil
		}
		return validSettlementMid(MarketKey{}, settledAt)
	}}, &fakeSpots{}, nil, nil)
	var degraded []string
	got := engine.catchupFunding(context.Background(), position, now, &degraded)
	want := FundingPnLDelta(position.ShortQty, decimal.NewFromInt(100), decimal.RequireFromString("0.001"))
	if !got.FundingPnL.Equal(want) {
		t.Fatalf("pnl=%s want=%s", got.FundingPnL, want)
	}
	if got.FundingCheckedThroughShort == nil || !got.FundingCheckedThroughShort.Equal(t1) {
		t.Fatalf("watermark=%v want=%v", got.FundingCheckedThroughShort, t1)
	}
	if got.DegradedReason != AccountingIncomplete {
		t.Fatalf("reason=%s", got.DegradedReason)
	}
}

func TestCatchupIncompleteOneLegKeepsReasonUntilBothRecover(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	opened := now.Add(-24 * time.Hour)
	settled := opened.Add(8 * time.Hour)
	position := sampleCatchupPosition(opened)
	position.Kind = KindCross
	position.LongProduct = ProductPerpetual
	position.LongExchange = "okx"
	position.LongSymbol = "BTCUSDT"
	position.LongCanonicalSymbol = "BTCUSDT"
	position.FundingCheckedThroughLong = &opened
	history := map[string][]HistoryPoint{
		rateMapKey("binance", "BTCUSDT"): {{SettledAt: settled, Rate: decimal.RequireFromString("0.001")}},
		rateMapKey("okx", "BTCUSDT"):     {{SettledAt: settled, Rate: decimal.RequireFromString("0.001")}},
	}
	engine := NewEngine(cfg, NewMemoryStore(), &fakeMarket{
		history: history,
		histErrs: map[string]error{
			rateMapKey("okx", "BTCUSDT"): errors.New("funding history failed"),
		},
	}, &fakeQuotes{midBefore: validSettlementMid}, &fakeSpots{}, nil, nil)
	var degraded []string
	got := engine.catchupFunding(context.Background(), position, now, &degraded)
	if got.DegradedReason != AccountingIncomplete {
		t.Fatalf("reason=%s", got.DegradedReason)
	}
	if got.FundingCheckedThroughLong == nil || !got.FundingCheckedThroughLong.Equal(opened) {
		t.Fatalf("failed leg watermark moved %v", got.FundingCheckedThroughLong)
	}
	engine.market = &fakeMarket{history: history}
	degraded = nil
	got.DegradedReason = AccountingIncomplete
	got = engine.catchupFunding(context.Background(), got, now, &degraded)
	if got.DegradedReason != "" {
		t.Fatalf("recovered reason=%s", got.DegradedReason)
	}
}

func TestCatchupQueryErrorDoesNotAdvanceWatermark(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	opened := now.Add(-time.Hour)
	position := sampleCatchupPosition(opened)
	engine := NewEngine(cfg, NewMemoryStore(), &fakeMarket{
		histErrs: map[string]error{rateMapKey("binance", "BTCUSDT"): errors.New("boom")},
	}, &fakeQuotes{midBefore: validSettlementMid}, &fakeSpots{}, nil, nil)
	var degraded []string
	got := engine.catchupFunding(context.Background(), position, now, &degraded)
	if !got.FundingPnL.IsZero() {
		t.Fatalf("pnl=%s", got.FundingPnL)
	}
	if got.FundingCheckedThroughShort == nil || !got.FundingCheckedThroughShort.Equal(opened) {
		t.Fatalf("watermark=%v", got.FundingCheckedThroughShort)
	}
	if got.DegradedReason != AccountingIncomplete {
		t.Fatalf("reason=%s", got.DegradedReason)
	}
}

func TestCatchupOldFullPageDoesNotTruncate(t *testing.T) {
	cfg := testJevConfig(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	opened := now.Add(-time.Hour)
	points := make([]HistoryPoint, HistoryLimit)
	for i := range points {
		points[i] = HistoryPoint{SettledAt: opened.Add(-time.Duration(HistoryLimit-i) * time.Hour), Rate: decimal.RequireFromString("0.001")}
	}
	position := sampleCatchupPosition(opened)
	engine := NewEngine(cfg, NewMemoryStore(), &fakeMarket{
		history: map[string][]HistoryPoint{rateMapKey("binance", "BTCUSDT"): points},
	}, &fakeQuotes{midBefore: validSettlementMid}, &fakeSpots{}, nil, nil)
	var degraded []string
	got := engine.catchupFunding(context.Background(), position, now, &degraded)
	if !got.FundingPnL.IsZero() || got.DegradedReason != "" {
		t.Fatalf("pnl=%s reason=%s", got.FundingPnL, got.DegradedReason)
	}
}

func sampleCatchupPosition(opened time.Time) Position {
	return Position{
		Kind: KindSingle, Status: PositionOpen, CandidateKey: "single:binance:BTCUSDT:BTCUSDT",
		BaseAsset: "BTC", QuoteAsset: "USDT", GlobalSymbol: "BTCUSDT",
		LongExchange: "binance", LongSymbol: "BTCUSDT", LongProduct: ProductSpot,
		LongCanonicalSymbol: "BTCUSDT", LongQuoteAsset: "USDT",
		ShortExchange: "binance", ShortSymbol: "BTCUSDT", ShortProduct: ProductPerpetual,
		ShortCanonicalSymbol: "BTCUSDT", ShortQuoteAsset: "USDT",
		SharedBaseQty: decimal.NewFromInt(2), LongQty: decimal.NewFromInt(2), ShortQty: decimal.NewFromInt(2).Neg(),
		OpenedAt: opened, FundingCheckedThroughShort: timePtr(opened),
	}
}

func validSettlementMid(_ MarketKey, settledAt time.Time) (decimal.Decimal, time.Time, bool, error) {
	return decimal.NewFromInt(100), settledAt.Add(-30 * time.Second), true, nil
}

func singleMarket(now time.Time, exchange, base, symbol string) (map[string]Quote, map[string]SpotInstrument) {
	quotes := map[string]Quote{
		quoteKey(exchange, symbol, ProductPerpetual): freshQuote(exchange, symbol, ProductPerpetual, now, "100"),
		quoteKey(exchange, symbol, ProductSpot):      freshQuote(exchange, symbol, ProductSpot, now, "100"),
	}
	spots := map[string]SpotInstrument{
		spotKey(exchange, base, "USDT"): {Exchange: exchange, ExchangeSymbol: symbol, BaseAsset: base, QuoteAsset: "USDT"},
	}
	return quotes, spots
}

func manySingleMarkets(now time.Time, n int) ([]Rate, map[string]Quote, map[string]SpotInstrument) {
	bases := []string{"AAA", "BBB", "CCC", "DDD", "EEE", "FFF", "GGG", "HHH", "III", "JJJ"}
	if n > len(bases) {
		n = len(bases)
	}
	rates := make([]Rate, 0, n)
	quotes := map[string]Quote{}
	spots := map[string]SpotInstrument{}
	for i := 0; i < n; i++ {
		base := bases[i]
		symbol := base + "USDT"
		rate := sampleRate("binance", symbol, base, decimal.RequireFromString("0.002").Add(decimal.NewFromInt(int64(i)).Mul(decimal.RequireFromString("0.001"))).String())
		rate.SourceUpdatedAt = now
		rates = append(rates, rate)
		q, s := singleMarket(now, "binance", base, symbol)
		for k, v := range q {
			quotes[k] = v
		}
		for k, v := range s {
			spots[k] = v
		}
	}
	return rates, quotes, spots
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !containsString(value, part) {
			return false
		}
	}
	return true
}

func containsString(value, part string) bool {
	return len(value) >= len(part) && (value == part || len(part) == 0 ||
		(func() bool {
			for i := 0; i+len(part) <= len(value); i++ {
				if value[i:i+len(part)] == part {
					return true
				}
			}
			return false
		})())
}
