package trader

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/marketdata"
)

func testOneShotExitBasis(now time.Time) oneShotExitBasis {
	return oneShotExitBasis{
		CombinationID:      "combo-1",
		Version:            3,
		ExitAnnualizedRate: decimal.RequireFromString("0.10"),
		OwnedA:             decimal.NewFromInt(1),
		OwnedB:             decimal.NewFromInt(-1),
		AvgA:               decimal.NewFromInt(100),
		AvgB:               decimal.NewFromInt(101),
		ComboA:             decimal.NewFromInt(1),
		ComboB:             decimal.NewFromInt(-1),
		Realized:           decimal.NewFromInt(1),
		Exposure:           decimal.NewFromInt(10).Mul(decimal.NewFromInt(3600)),
		Holding:            decimal.NewFromInt(3600),
		ExposureUpdatedAt:  now,
		ThresholdHours:     8,
		CloseFeeRateA:      decimal.RequireFromString("0.0002"),
		CloseFeeRateB:      decimal.RequireFromString("0.0004"),
		ExecutionMode:      "maker_then_hedge",
		MakerLeg:           "a",
		LegA: ArbitrageLeg{
			Exchange: "binance", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
		},
		LegB: ArbitrageLeg{
			Exchange: "okx", ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
		},
		CalculatedAt: now,
	}
}

func favorableExitQuote(now time.Time) oneShotExitQuote {
	return oneShotExitQuote{
		BidA: decimal.NewFromInt(110), AskA: decimal.NewFromInt(112),
		BidB: decimal.NewFromInt(88), AskB: decimal.NewFromInt(90),
		ReceivedA: now, ReceivedB: now,
	}
}

func adverseExitQuote(now time.Time) oneShotExitQuote {
	return oneShotExitQuote{
		BidA: decimal.NewFromInt(90), AskA: decimal.NewFromInt(92),
		BidB: decimal.NewFromInt(108), AskB: decimal.NewFromInt(110),
		ReceivedA: now, ReceivedB: now,
	}
}

func TestOneShotExitQuantitiesUseBidAskBySign(t *testing.T) {
	basis := testOneShotExitBasis(time.Now().UTC())
	qtyA, qtyB, ownedQ, venueQ, ok := oneShotExitQuantities(basis)
	if !ok || !ownedQ.Equal(decimal.NewFromInt(1)) || !venueQ.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("owned=%s venue=%s ok=%v", ownedQ, venueQ, ok)
	}
	if !qtyA.Equal(decimal.NewFromInt(1)) || !qtyB.Equal(decimal.NewFromInt(-1)) {
		t.Fatalf("qty=%s/%s", qtyA, qtyB)
	}
	pxA, okA := executableClosePrice(qtyA, decimal.NewFromInt(110), decimal.NewFromInt(112))
	pxB, okB := executableClosePrice(qtyB, decimal.NewFromInt(88), decimal.NewFromInt(90))
	if !okA || !okB || !pxA.Equal(decimal.NewFromInt(110)) || !pxB.Equal(decimal.NewFromInt(90)) {
		t.Fatalf("prices=%s/%s", pxA, pxB)
	}

	basis.OwnedA, basis.OwnedB = decimal.NewFromInt(-1), decimal.NewFromInt(1)
	basis.ComboA, basis.ComboB = decimal.NewFromInt(-1), decimal.NewFromInt(1)
	qtyA, qtyB, _, _, ok = oneShotExitQuantities(basis)
	if !ok || !qtyA.Equal(decimal.NewFromInt(-1)) || !qtyB.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("reversed qty=%s/%s ok=%v", qtyA, qtyB, ok)
	}
	pxA, okA = executableClosePrice(qtyA, decimal.NewFromInt(99), decimal.NewFromInt(101))
	pxB, okB = executableClosePrice(qtyB, decimal.NewFromInt(100), decimal.NewFromInt(102))
	if !okA || !okB || !pxA.Equal(decimal.NewFromInt(101)) || !pxB.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("reversed prices=%s/%s", pxA, pxB)
	}
}

func TestOneShotExitQuantitiesRejectUnpairedAndDirectionMismatch(t *testing.T) {
	basis := testOneShotExitBasis(time.Now().UTC())
	basis.OwnedB = decimal.NewFromInt(2)
	if _, _, _, _, ok := oneShotExitQuantities(basis); ok {
		t.Fatal("same-sign inventory must be invalid")
	}
	basis = testOneShotExitBasis(time.Now().UTC())
	basis.ComboA = decimal.Zero
	if _, _, _, _, ok := oneShotExitQuantities(basis); ok {
		t.Fatal("zero closeable must be invalid")
	}
	basis = testOneShotExitBasis(time.Now().UTC())
	basis.BaselineA = decimal.NewFromInt(10)
	basis.BaselineB = decimal.NewFromInt(-7)
	basis.ComboA = decimal.NewFromInt(-2)
	basis.ComboB = decimal.NewFromInt(1)
	basis.BaselineCapturedAt = time.Now().UTC()
	_, _, ownedQ, venueQ, ok := oneShotExitQuantities(basis)
	if ok || !ownedQ.Equal(decimal.NewFromInt(1)) || !venueQ.Equal(decimal.NewFromInt(6)) {
		t.Fatalf("direction mismatch owned=%s venue=%s ok=%v", ownedQ, venueQ, ok)
	}

	basis = testOneShotExitBasis(time.Now().UTC())
	basis.BaselineA = decimal.NewFromInt(10)
	basis.BaselineB = decimal.NewFromInt(-7)
	basis.BaselineCapturedAt = time.Now().UTC()
	qtyA, qtyB, ownedQ, venueQ, ok := oneShotExitQuantities(basis)
	if !ok || !qtyA.Equal(decimal.NewFromInt(1)) ||
		!qtyB.Equal(decimal.NewFromInt(-1)) ||
		!ownedQ.Equal(decimal.NewFromInt(1)) ||
		!venueQ.Equal(decimal.NewFromInt(8)) {
		t.Fatalf("same-direction baseline qty=%s/%s owned=%s venue=%s ok=%v",
			qtyA, qtyB, ownedQ, venueQ, ok)
	}
}

func TestEvaluateOneShotExitSampleFavorableAndAdverse(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	basis := testOneShotExitBasis(now)
	if got := evaluateOneShotExitSample(basis, favorableExitQuote(now), now); got.fault || !got.hit {
		t.Fatalf("favorable=%+v", got)
	}
	if got := evaluateOneShotExitSample(basis, adverseExitQuote(now), now); got.fault || got.hit {
		t.Fatalf("adverse=%+v", got)
	}
}

func TestEvaluateOneShotExitSampleDeductsCloseFees(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	basis := testOneShotExitBasis(now)
	basis.Realized = decimal.Zero
	basis.ExitAnnualizedRate = decimal.RequireFromString("0.0000001")
	basis.CloseFeeRateA = decimal.RequireFromString("0.2")
	basis.CloseFeeRateB = decimal.RequireFromString("0.2")
	if got := evaluateOneShotExitSample(basis, favorableExitQuote(now), now); got.fault || got.hit {
		t.Fatalf("high close fee should miss: %+v", got)
	}
	basis.CloseFeeRateA = decimal.Zero
	basis.CloseFeeRateB = decimal.Zero
	if got := evaluateOneShotExitSample(basis, favorableExitQuote(now), now); got.fault || !got.hit {
		t.Fatalf("zero fee should hit: %+v", got)
	}
}

func TestCloseFeeRoleAndCostRate(t *testing.T) {
	if got := closeFeeRole("maker_then_hedge", "a", "a"); got != "maker" {
		t.Fatalf("preferred=%s", got)
	}
	if got := closeFeeRole("maker_then_hedge", "a", "b"); got != "taker" {
		t.Fatalf("hedge=%s", got)
	}
	if got := closeFeeRole("simultaneous_market", "a", "a"); got != "taker" {
		t.Fatalf("market A=%s", got)
	}
	maker := "0.0002"
	taker := "0.0004"
	okxMaker := "-0.0002"
	okxTaker := "-0.0005"
	accounts := map[int64]accountFeeSnapshot{
		1: {exchange: "binance", contractMaker: &maker, contractTaker: &taker},
		2: {exchange: "okx", contractMaker: &okxMaker, contractTaker: &okxTaker},
	}
	legA := ArbitrageLeg{TradingAccountID: 1, Exchange: "binance", ContractType: "perpetual"}
	legB := ArbitrageLeg{TradingAccountID: 2, Exchange: "okx", ContractType: "perpetual"}
	rateA, okA := closeFeeCostRate(legA, "maker_then_hedge", "a", "a", accounts)
	rateB, okB := closeFeeCostRate(legB, "maker_then_hedge", "a", "b", accounts)
	if !okA || !okB || !rateA.Equal(decimal.RequireFromString("0.0002")) ||
		!rateB.Equal(decimal.RequireFromString("0.0005")) {
		t.Fatalf("maker/hedge rates=%s/%s", rateA, rateB)
	}
	rateA, okA = closeFeeCostRate(legA, "simultaneous_market", "a", "a", accounts)
	rateB, okB = closeFeeCostRate(legB, "simultaneous_market", "a", "b", accounts)
	if !okA || !okB || !rateA.Equal(decimal.RequireFromString("0.0004")) ||
		!rateB.Equal(decimal.RequireFromString("0.0005")) {
		t.Fatalf("market rates=%s/%s", rateA, rateB)
	}
}

func TestEvaluateOneShotExitSampleFaultsOnStaleQuote(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	basis := testOneShotExitBasis(now)
	quote := favorableExitQuote(now.Add(-3 * time.Second))
	if got := evaluateOneShotExitSample(basis, quote, now); !got.fault {
		t.Fatalf("stale quote=%+v", got)
	}
}

func TestOneShotExitWindowReadyRules(t *testing.T) {
	if oneShotExitWindowReady(nil) {
		t.Fatal("empty window")
	}
	hits := make([]bool, 29)
	for i := range hits {
		hits[i] = true
	}
	if oneShotExitWindowReady(hits) {
		t.Fatal("29 samples must not trigger")
	}
	hits = append(hits, true)
	if !oneShotExitWindowReady(hits) {
		t.Fatal("30 hits should trigger")
	}
	hits[29] = false
	if oneShotExitWindowReady(hits) {
		t.Fatal("latest miss must not trigger")
	}
	for i := 0; i < 12; i++ {
		hits[i] = false
	}
	hits[29] = true
	if !oneShotExitWindowReady(hits) {
		t.Fatal("18 hits including latest should trigger")
	}
}

func TestAdvanceExposureUsesCurrentPairedNotional(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	sampleAt := now.Add(time.Hour)
	basis := testOneShotExitBasis(now)
	basis.Realized = decimal.NewFromInt(1)
	basis.CloseFeeRateA = decimal.Zero
	basis.CloseFeeRateB = decimal.Zero
	basis.ExitAnnualizedRate = decimal.RequireFromString("0.5")
	got := evaluateOneShotExitSample(basis, favorableExitQuote(sampleAt), sampleAt)
	if got.fault {
		t.Fatalf("sample=%+v", got)
	}
	historical := basis.Exposure.Div(basis.Holding)
	if historical.GreaterThanOrEqual(decimal.NewFromInt(100)) {
		t.Fatalf("fixture average notional=%s", historical)
	}
	qtyA, _, _, _, ok := oneShotExitQuantities(basis)
	if !ok {
		t.Fatal("quantities")
	}
	current := qtyA.Abs().Mul(basis.AvgA.Add(basis.AvgB)).Div(decimal.NewFromInt(2))
	if !current.GreaterThan(historical) {
		t.Fatalf("current=%s historical=%s", current, historical)
	}
}

func TestPositionMetricsWorkerSamplesAndClosesOnce(t *testing.T) {
	keyA, keyB := metricsTestKeys(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	basis := testOneShotExitBasis(now)
	combo := metricsTestCombo()
	combo.RunMode = "one_shot"
	combo.OneShotPhase = "waiting_exit"
	combo.ExitPolicy = "annualized"
	combo.ExitAnnualizedRate = "0.10"
	combo.Version = 3
	current := now
	store := &stubPositionMetricsStore{
		items: []ArbitrageCombination{combo},
		persistResult: arbitragePositionMetricsPersistResult{
			Applied: true, ExitBasis: &basis, Combination: combo,
		},
		closeApplied: true,
	}
	worker := NewArbitragePositionMetricsWorker(store, stubBBOSource{
		now: func() time.Time { return current },
		values: map[marketdata.Key]marketdata.BBO{
			keyA: {BidPrice: "110", AskPrice: "112"},
			keyB: {BidPrice: "88", AskPrice: "90"},
		},
	}, slog.Default())
	worker.refreshAt(context.Background(), current)
	state, ok := worker.exitStateCopy("combo-1")
	if !ok || len(state.hits) != 0 {
		t.Fatalf("basis window=%+v ok=%v", state, ok)
	}
	for i := 0; i < oneShotExitWindowSize; i++ {
		current = current.Add(time.Second)
		worker.sampleExits(context.Background(), current)
	}
	if store.closeCalls.Load() != 1 || store.lastCloseVer != 3 {
		t.Fatalf("close calls=%d version=%d", store.closeCalls.Load(), store.lastCloseVer)
	}
	if _, ok := worker.exitStateCopy("combo-1"); ok {
		t.Fatal("exit state should clear after CAS")
	}
}

func TestPositionMetricsWorkerDoesNotCloseOnLatestMiss(t *testing.T) {
	keyA, keyB := metricsTestKeys(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	basis := testOneShotExitBasis(now)
	combo := metricsTestCombo()
	combo.RunMode = "one_shot"
	combo.OneShotPhase = "waiting_exit"
	combo.ExitPolicy = "annualized"
	current := now
	store := &stubPositionMetricsStore{
		items: []ArbitrageCombination{combo},
		persistResult: arbitragePositionMetricsPersistResult{
			Applied: true, ExitBasis: &basis, Combination: combo,
		},
		closeApplied: true,
	}
	values := map[marketdata.Key]marketdata.BBO{
		keyA: {BidPrice: "110", AskPrice: "112"},
		keyB: {BidPrice: "88", AskPrice: "90"},
	}
	worker := NewArbitragePositionMetricsWorker(store, stubBBOSource{
		now:    func() time.Time { return current },
		values: values,
	}, slog.Default())
	worker.refreshAt(context.Background(), current)
	for i := 0; i < 18; i++ {
		current = current.Add(time.Second)
		worker.sampleExits(context.Background(), current)
	}
	values[keyA] = marketdata.BBO{BidPrice: "90", AskPrice: "92"}
	values[keyB] = marketdata.BBO{BidPrice: "108", AskPrice: "110"}
	for i := 0; i < 12; i++ {
		current = current.Add(time.Second)
		worker.sampleExits(context.Background(), current)
	}
	if store.closeCalls.Load() != 0 {
		t.Fatalf("latest miss closed calls=%d", store.closeCalls.Load())
	}
	state, ok := worker.exitStateCopy("combo-1")
	if !ok || len(state.hits) != 30 || state.hits[29] {
		t.Fatalf("window=%+v ok=%v", state.hits, ok)
	}
}

func TestPositionMetricsWorkerResetsOnStaleBBO(t *testing.T) {
	keyA, keyB := metricsTestKeys(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	basis := testOneShotExitBasis(now)
	combo := metricsTestCombo()
	combo.RunMode = "one_shot"
	combo.OneShotPhase = "waiting_exit"
	combo.ExitPolicy = "annualized"
	store := &stubPositionMetricsStore{
		items: []ArbitrageCombination{combo},
		persistResult: arbitragePositionMetricsPersistResult{
			Applied: true, ExitBasis: &basis, Combination: combo,
		},
	}
	stale := now.Add(-3 * time.Second)
	worker := NewArbitragePositionMetricsWorker(store, stubBBOSource{
		now: func() time.Time { return stale },
		values: map[marketdata.Key]marketdata.BBO{
			keyA: {BidPrice: "110", AskPrice: "112"},
			keyB: {BidPrice: "88", AskPrice: "90"},
		},
	}, slog.Default())
	worker.refreshAt(context.Background(), now)
	current := now
	for i := 0; i < 5; i++ {
		current = current.Add(time.Second)
		worker.sampleExits(context.Background(), current)
	}
	state, ok := worker.exitStateCopy("combo-1")
	if !ok || len(state.hits) != 0 {
		t.Fatalf("stale BBO must reset window hits=%v ok=%v", state.hits, ok)
	}
	if store.closeCalls.Load() != 0 {
		t.Fatal("stale BBO must not CAS")
	}
}

func TestPositionMetricsWorkerAppliedFalseKeepsWindow(t *testing.T) {
	keyA, keyB := metricsTestKeys(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	basis := testOneShotExitBasis(now)
	combo := metricsTestCombo()
	combo.RunMode = "one_shot"
	combo.OneShotPhase = "waiting_exit"
	combo.ExitPolicy = "annualized"
	current := now
	store := &stubPositionMetricsStore{
		items: []ArbitrageCombination{combo},
		persistResult: arbitragePositionMetricsPersistResult{
			Applied: true, ExitBasis: &basis, Combination: combo,
		},
	}
	worker := NewArbitragePositionMetricsWorker(store, stubBBOSource{
		now: func() time.Time { return current },
		values: map[marketdata.Key]marketdata.BBO{
			keyA: {BidPrice: "110", AskPrice: "112"},
			keyB: {BidPrice: "88", AskPrice: "90"},
		},
	}, slog.Default())
	worker.refreshAt(context.Background(), current)
	current = current.Add(time.Second)
	worker.sampleExits(context.Background(), current)
	store.persistResult = arbitragePositionMetricsPersistResult{
		Applied: false, Combination: combo,
	}
	worker.refreshAt(context.Background(), current.Add(arbitragePositionMetricsInterval))
	state, ok := worker.exitStateCopy("combo-1")
	if !ok || len(state.hits) != 1 {
		t.Fatalf("guarded persist cleared window=%+v ok=%v", state, ok)
	}
}

func TestPositionMetricsWorkerDropsIneligibleExitState(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	basis := testOneShotExitBasis(now)
	combo := metricsTestCombo()
	combo.RunMode = "one_shot"
	combo.OneShotPhase = "waiting_exit"
	combo.ExitPolicy = "annualized"
	store := &stubPositionMetricsStore{
		items: []ArbitrageCombination{combo},
		persistResult: arbitragePositionMetricsPersistResult{
			Applied: true, ExitBasis: &basis, Combination: combo,
		},
	}
	worker := NewArbitragePositionMetricsWorker(store, stubBBOSource{}, slog.Default())
	worker.refreshAt(context.Background(), now)
	if _, ok := worker.exitStateCopy("combo-1"); !ok {
		t.Fatal("expected exit state")
	}
	combo.ExitPolicy = "time"
	store.items = []ArbitrageCombination{combo}
	store.persistResult = arbitragePositionMetricsPersistResult{Applied: true, Combination: combo}
	worker.refreshAt(context.Background(), now.Add(arbitragePositionMetricsInterval))
	if _, ok := worker.exitStateCopy("combo-1"); ok {
		t.Fatal("time-exit combo must drop exit state")
	}
}

func TestPositionMetricsWorkerStopsSamplingStaleBasis(t *testing.T) {
	keyA, keyB := metricsTestKeys(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	basis := testOneShotExitBasis(now.Add(-oneShotExitBasisMaxAge - time.Minute))
	combo := metricsTestCombo()
	combo.RunMode = "one_shot"
	combo.OneShotPhase = "waiting_exit"
	combo.ExitPolicy = "annualized"
	store := &stubPositionMetricsStore{
		items: []ArbitrageCombination{combo},
		persistResult: arbitragePositionMetricsPersistResult{
			Applied: true, ExitBasis: &basis, Combination: combo,
		},
		closeApplied: true,
	}
	current := now
	worker := NewArbitragePositionMetricsWorker(store, stubBBOSource{
		now: func() time.Time { return current },
		values: map[marketdata.Key]marketdata.BBO{
			keyA: {BidPrice: "110", AskPrice: "112"},
			keyB: {BidPrice: "88", AskPrice: "90"},
		},
	}, slog.Default())
	worker.refreshAt(context.Background(), now)
	worker.sampleExits(context.Background(), now)
	state, ok := worker.exitStateCopy("combo-1")
	if !ok || !state.stale || len(state.hits) != 0 {
		t.Fatalf("stale basis=%+v ok=%v", state, ok)
	}
	current = now.Add(time.Second)
	worker.sampleExits(context.Background(), current)
	if store.closeCalls.Load() != 0 {
		t.Fatal("stale basis must not CAS")
	}
}
