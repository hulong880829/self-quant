package trader

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/marketdata"
)

func oneShotFinalFlattenCombo(legA, legB string) ArbitrageCombination {
	item := ownedTinyCloseCombo(legA, legB)
	item.Status = "running"
	item.RuntimeState = "monitoring"
	item.RunMode = "one_shot"
	item.OneShotPhase = "exiting"
	return item
}

func oneShotFinalFlattenBBO() marketdata.BBO {
	return marketdata.BBO{
		BidPrice: "0.214", AskPrice: "0.214",
		BidQuantity: "100000", AskQuantity: "100000",
		ReceiveTimestamp: time.Now().UTC(),
	}
}

func oneShotFinalFlattenExecution(combo ArbitrageCombination, qtyA, qtyB decimal.Decimal) ArbitrageExecution {
	price := decimal.RequireFromString("0.214")
	notionalA, notionalB := decimal.Zero, decimal.Zero
	if qtyA.IsPositive() {
		notionalA = qtyA.Mul(price)
	}
	if qtyB.IsPositive() {
		notionalB = qtyB.Mul(price)
	}
	return ArbitrageExecution{
		ID: "flatten-88-80", CombinationID: combo.ID,
		Direction:      oneShotFinalFlattenDirection(combo, qtyA, qtyB),
		Status:         "claimed",
		TriggerLegABid: "0.214", TriggerLegAAsk: "0.214",
		TriggerLegBBid: "0.214", TriggerLegBAsk: "0.214",
		TargetBaseQuantity: decimal.Max(qtyA, qtyB).String(),
		RequestedNotional:  decimal.Max(notionalA, notionalB).String(),
		PositionEffect:     "close", ReduceOnly: true, LastCloseClip: true,
		LegAFilledQuantity: "0", LegBFilledQuantity: "0", DeltaNotional: "0",
	}
}

func TestOneShotFinalFlattenReconcileReadyRequiresVenueAndSnapshot(t *testing.T) {
	combo := oneShotFinalFlattenCombo("0", "0")
	combo.LegAVenueBasePosition = "0"
	combo.LegBVenueBasePosition = "0"
	instrumentA := flattenTestCatalog()[101]
	instrumentB := flattenTestCatalog()[202]
	orderTime := time.Now().UTC()
	orders := []Order{{
		ArbitrageExecutionID: "flatten-88-80",
		IdempotencyKey:       oneShotFinalFlattenIdempotencyKey("flatten-88-80", "a"),
		UpdatedAt:            orderTime,
		Status:               "filled",
	}}
	combo.LastPositionReconciledAt = orderTime.Add(-time.Second)
	if oneShotFinalFlattenReconcileReady(combo, orders, "flatten-88-80", instrumentA, instrumentB) {
		t.Fatal("stale auditor snapshot must not mark exited")
	}
	combo.LastPositionReconciledAt = orderTime.Add(time.Second)
	combo.LegAVenueBasePosition = "88"
	if oneShotFinalFlattenReconcileReady(combo, orders, "flatten-88-80", instrumentA, instrumentB) {
		t.Fatal("venue leftover must not mark exited")
	}
	combo.LegAVenueBasePosition = "0"
	if !oneShotFinalFlattenReconcileReady(combo, orders, "flatten-88-80", instrumentA, instrumentB) {
		t.Fatal("ledger and venue-baseline flat should be ready")
	}
}

func TestPlanOwnedFlattenLegsIndependentQuantities(t *testing.T) {
	combo := oneShotFinalFlattenCombo("88", "-80")
	instrumentA := flattenTestCatalog()[101]
	instrumentB := flattenTestCatalog()[202]
	legs := planOwnedFlattenLegs(combo, instrumentA, instrumentB)
	if len(legs) != 2 {
		t.Fatalf("legs=%d", len(legs))
	}
	qtyA, qtyB := oneShotFinalFlattenPlannedQuantities(legs)
	if !qtyA.Equal(decimal.NewFromInt(88)) || !qtyB.Equal(decimal.NewFromInt(80)) {
		t.Fatalf("qtyA=%s qtyB=%s", qtyA, qtyB)
	}
	if legs[0].Side != "sell" || legs[1].Side != "buy" {
		t.Fatalf("sides=%s %s", legs[0].Side, legs[1].Side)
	}
	if !legs[0].ReduceOnly || legs[0].OrderType != "market" {
		t.Fatalf("leg=%+v", legs[0])
	}
}

func TestPlanOwnedFlattenLegsSingleLeg(t *testing.T) {
	combo := oneShotFinalFlattenCombo("8", "0")
	instrumentA := flattenTestCatalog()[101]
	instrumentB := flattenTestCatalog()[202]
	legs := planOwnedFlattenLegs(combo, instrumentA, instrumentB)
	if len(legs) != 1 || legs[0].LegName != "a" || legs[0].Side != "sell" ||
		!legs[0].BaseQuantity.Equal(decimal.NewFromInt(8)) {
		t.Fatalf("legs=%+v", legs)
	}
}

func TestOneShotFinalFlattenSidesFollowOwnedSign(t *testing.T) {
	combo := oneShotFinalFlattenCombo("-8", "10")
	instrumentA := flattenTestCatalog()[101]
	instrumentB := flattenTestCatalog()[202]
	legs := planOwnedFlattenLegs(combo, instrumentA, instrumentB)
	qtyA, qtyB := oneShotFinalFlattenPlannedQuantities(legs)
	if oneShotFinalFlattenDirection(combo, qtyA, qtyB) != "ask" {
		t.Fatalf("direction=%s", oneShotFinalFlattenDirection(combo, qtyA, qtyB))
	}
	byName := map[string]OwnedFlattenLeg{}
	for _, leg := range legs {
		byName[leg.LegName] = leg
	}
	if byName["a"].Side != "buy" || byName["b"].Side != "sell" {
		t.Fatalf("legs=%+v", legs)
	}
}

func TestValidateOneShotFinalFlattenClaimFFAcceptsIndependentQty(t *testing.T) {
	combo := oneShotFinalFlattenCombo("88", "-80")
	qtyA, qtyB := decimal.NewFromInt(88), decimal.NewFromInt(80)
	execution := oneShotFinalFlattenExecution(combo, qtyA, qtyB)
	claim := OneShotFinalFlattenClaim{
		Execution: execution, LegAQuantity: qtyA, LegBQuantity: qtyB,
		LegANotional: qtyA.Mul(decimal.RequireFromString("0.214")),
		LegBNotional: qtyB.Mul(decimal.RequireFromString("0.214")),
		ReconciledAt: combo.LastPositionReconciledAt,
	}
	if !validateOneShotFinalFlattenClaim(combo, claim, time.Now().UTC()) {
		t.Fatal("FF 88/80 claim should be accepted")
	}
	claim.Execution.TargetBaseQuantity = "80"
	if validateOneShotFinalFlattenClaim(combo, claim, time.Now().UTC()) {
		t.Fatal("targetBase != max(planned) must be rejected")
	}
}

func TestValidateOneShotFinalFlattenClaimRejects(t *testing.T) {
	combo := oneShotFinalFlattenCombo("88", "-80")
	qtyA, qtyB := decimal.NewFromInt(88), decimal.NewFromInt(80)
	base := OneShotFinalFlattenClaim{
		Execution:    oneShotFinalFlattenExecution(combo, qtyA, qtyB),
		LegAQuantity: qtyA, LegBQuantity: qtyB,
		LegANotional: qtyA.Mul(decimal.RequireFromString("0.214")),
		LegBNotional: qtyB.Mul(decimal.RequireFromString("0.214")),
		ReconciledAt: combo.LastPositionReconciledAt,
	}
	over := base
	over.LegAQuantity = decimal.NewFromInt(89)
	over.Execution.TargetBaseQuantity = "89"
	over.Execution.RequestedNotional = over.LegAQuantity.Mul(decimal.RequireFromString("0.214")).String()
	if validateOneShotFinalFlattenClaim(combo, over, time.Now().UTC()) {
		t.Fatal("qty above cap must be rejected")
	}
	stale := base
	stale.ReconciledAt = combo.LastPositionReconciledAt.Add(-time.Second)
	if validateOneShotFinalFlattenClaim(combo, stale, time.Now().UTC()) {
		t.Fatal("snapshot mismatch must be rejected")
	}
	old := combo
	old.LastPositionReconciledAt = time.Now().UTC().Add(-3 * time.Minute)
	staleSnap := base
	staleSnap.ReconciledAt = old.LastPositionReconciledAt
	if validateOneShotFinalFlattenClaim(old, staleSnap, time.Now().UTC()) {
		t.Fatal("stale snapshot must be rejected")
	}
	uncertain := combo
	uncertain.PositionUncertain = true
	if validateOneShotFinalFlattenClaim(uncertain, base, time.Now().UTC()) {
		t.Fatal("uncertain combo must be rejected")
	}
	spread := combo
	spread.RunMode = "spread"
	spread.OneShotPhase = ""
	if validateOneShotFinalFlattenClaim(spread, base, time.Now().UTC()) {
		t.Fatal("spread combo must be rejected")
	}
	single := oneShotFinalFlattenCombo("8", "0")
	singleClaim := OneShotFinalFlattenClaim{
		Execution:    oneShotFinalFlattenExecution(single, decimal.NewFromInt(8), decimal.Zero),
		LegAQuantity: decimal.NewFromInt(8),
		LegANotional: decimal.NewFromInt(8).Mul(decimal.RequireFromString("0.214")),
		ReconciledAt: single.LastPositionReconciledAt,
	}
	if !validateOneShotFinalFlattenClaim(single, singleClaim, time.Now().UTC()) {
		t.Fatal("unpaired +8 must claim")
	}
}

func TestIsOneShotFinalFlattenExecutionRecoverOrder(t *testing.T) {
	combo := oneShotFinalFlattenCombo("88", "-80")
	execution := oneShotFinalFlattenExecution(
		combo, decimal.NewFromInt(88), decimal.NewFromInt(80),
	)
	execution.MakerOrderID = "uuid-a"
	execution.HedgeOrderID = "uuid-b"
	flattenOrders := []Order{{
		ID: "uuid-a", ArbitrageExecutionID: execution.ID, ArbitrageRole: "market",
		IdempotencyKey: oneShotFinalFlattenIdempotencyKey(execution.ID, "a"),
	}}
	if !isOneShotFinalFlattenExecution(combo, execution, flattenOrders, marketdata.BBO{}, marketdata.BBO{}) {
		t.Fatal("final-flatten key must win even when IDs sit in maker/hedge columns")
	}
	makerOrders := []Order{{
		ID: "uuid-a", ArbitrageExecutionID: execution.ID, ArbitrageRole: "maker",
		IdempotencyKey: "arb:" + execution.ID + ":a:maker:0",
	}}
	if isOneShotFinalFlattenExecution(combo, execution, makerOrders, marketdata.BBO{}, marketdata.BBO{}) {
		t.Fatal("maker role must stay on the old path")
	}
	plainMarket := []Order{{
		ID: "uuid-a", ArbitrageExecutionID: execution.ID, ArbitrageRole: "market",
		IdempotencyKey: "arb:" + execution.ID + ":a:market:0",
	}}
	if isOneShotFinalFlattenExecution(combo, execution, plainMarket, marketdata.BBO{}, marketdata.BBO{}) {
		t.Fatal("plain market without final-flatten must not take flatten path")
	}
	if !isOneShotFinalFlattenExecution(combo, execution, nil, marketdata.BBO{}, marketdata.BBO{}) {
		t.Fatal("no-order flatten claim shape under $20 must take flatten path")
	}
}

func TestArbitrageSchedulerOneShotFinalFlattenClaimsIndependentLegs(t *testing.T) {
	item := oneShotFinalFlattenCombo("88", "-80")
	item.LegA = ArbitrageLeg{
		TradingAccountID: 1, InstrumentID: 101,
		Exchange: "bybit", ContractType: "perpetual", ExchangeSymbol: "USELESSUSDT",
	}
	item.LegB = ArbitrageLeg{
		TradingAccountID: 2, InstrumentID: 202,
		Exchange: "okx", ContractType: "perpetual", ExchangeSymbol: "USELESSUSDT",
	}
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := oneShotFinalFlattenBBO()
	px := decimal.RequireFromString("0.214")
	if scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	) {
		t.Fatal("flatten claim should not close the combination")
	}
	if store.claims != 0 {
		t.Fatalf("generic claims=%d", store.claims)
	}
	select {
	case execution := <-runner.executed:
		if execution.TargetBaseQuantity != "88" || !execution.LastCloseClip {
			t.Fatalf("execution=%+v", execution)
		}
		requested := decimal.RequireFromString("88").Mul(px)
		if parsePositiveDecimal(execution.RequestedNotional).Cmp(requested) != 0 {
			t.Fatalf("requested=%s want=%s", execution.RequestedNotional, requested)
		}
		if !store.lastFlattenClaim.LegAQuantity.Equal(decimal.NewFromInt(88)) ||
			!store.lastFlattenClaim.LegBQuantity.Equal(decimal.NewFromInt(80)) {
			t.Fatalf("claim=%+v", store.lastFlattenClaim)
		}
	case <-time.After(time.Second):
		t.Fatal("expected independent flatten claim")
	}
}

func TestArbitrageSchedulerPairedAboveMinOpenStillMakerHedge(t *testing.T) {
	item := oneShotFinalFlattenCombo("200", "-200")
	item.LegA = ArbitrageLeg{
		TradingAccountID: 1, InstrumentID: 101,
		Exchange: "bybit", ContractType: "perpetual", ExchangeSymbol: "USELESSUSDT",
	}
	item.LegB = ArbitrageLeg{
		TradingAccountID: 2, InstrumentID: 202,
		Exchange: "okx", ContractType: "perpetual", ExchangeSymbol: "USELESSUSDT",
	}
	store := &dryRunStore{claim: true, item: item}
	runner := &schedulerRunner{executed: make(chan ArbitrageExecution, 1)}
	scheduler := newTestArbitrageScheduler(
		store, nil, runner, time.Second, time.Millisecond, time.Second,
		10, 2, 2, 2, false, nil,
	)
	runtime := &arbitrageRuntime{combination: item}
	bbo := oneShotFinalFlattenBBO()
	px := decimal.RequireFromString("0.214")
	scheduler.flattenOrFinalizeClosing(
		context.Background(), runtime, bbo, bbo, px, px, px, px,
	)
	if store.flattenClaims != 0 {
		t.Fatalf("flattenClaims=%d", store.flattenClaims)
	}
	select {
	case execution := <-runner.executed:
		if execution.LastCloseClip && execution.TargetBaseQuantity == "200" {
			return
		}
		if store.claims == 0 {
			t.Fatalf("expected generic maker-hedge claim execution=%+v", execution)
		}
	case <-time.After(time.Second):
		if store.claims == 0 {
			t.Fatal("paired leftover at or above $20 must use Maker→Hedge")
		}
	}
}

func TestEvaluateExitingOneShotDustFiveDollarRetries(t *testing.T) {
	combo := oneShotFinalFlattenCombo("0", "-23.364")
	instrument := defaultSizingInstrument()
	verdict := evaluateOneShotDust(oneShotDustInput{
		Combination: combo, Phase: "exiting",
		InstrumentA: instrument, InstrumentB: instrument,
		PriceA: decimal.RequireFromString("0.214"),
		PriceB: decimal.RequireFromString("0.214"),
	})
	if verdict != oneShotDustRetry {
		t.Fatalf("verdict=%s", verdict)
	}
}

func TestOneShotFinalFlattenShouldDustRequiresFreshReconcile(t *testing.T) {
	combo := oneShotFinalFlattenCombo("8", "0")
	instrumentA := flattenTestCatalog()[101]
	instrumentB := flattenTestCatalog()[202]
	orderTime := time.Now().UTC()
	orders := []Order{{
		ArbitrageExecutionID: "flatten-88-80",
		IdempotencyKey:       oneShotFinalFlattenIdempotencyKey("flatten-88-80", "a"),
		UpdatedAt:            orderTime,
		Status:               "rejected",
		ErrorMessage:         "minimum notional",
	}}
	combo.LastPositionReconciledAt = orderTime.Add(-time.Minute)
	if oneShotFinalFlattenShouldDust(combo, instrumentA, instrumentB, orders, "flatten-88-80", true) {
		t.Fatal("stale snapshot must not dust")
	}
	combo.LastPositionReconciledAt = orderTime.Add(time.Second)
	if !oneShotFinalFlattenShouldDust(combo, instrumentA, instrumentB, orders, "flatten-88-80", true) {
		t.Fatal("explicit min-notional with fresh reconcile should dust")
	}
	if oneShotFinalFlattenShouldDust(combo, instrumentA, instrumentB, orders, "flatten-88-80", false) {
		t.Fatal("generic reject must not dust while qty still steps")
	}
}

func TestOneShotFinalFlattenUnexecutableClassifier(t *testing.T) {
	if oneShotFinalFlattenUnexecutable(ErrVenueRejected) {
		t.Fatal("generic venue rejected is not dust")
	}
	if !oneShotFinalFlattenUnexecutable(ErrOrderBelowMinimum) {
		t.Fatal("min notional must be unexecutable")
	}
	if !oneShotFinalFlattenUnexecutable(errors.New("quantity precision invalid")) {
		t.Fatal("precision reject must be unexecutable")
	}
}

func TestFinalizeOneShotFinalFlattenTurnoverOnce(t *testing.T) {
	combo := oneShotFinalFlattenCombo("88", "-80")
	execution := oneShotFinalFlattenExecution(
		combo, decimal.NewFromInt(88), decimal.NewFromInt(80),
	)
	orders := []Order{
		{
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a", Side: "sell",
			IdempotencyKey: oneShotFinalFlattenIdempotencyKey(execution.ID, "a"),
			Status:         "filled", FilledQuantity: "88", AveragePrice: "0.214",
			UpdatedAt: time.Now().UTC(),
		},
		{
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "b", Side: "buy",
			IdempotencyKey: oneShotFinalFlattenIdempotencyKey(execution.ID, "b"),
			Status:         "filled", FilledQuantity: "80", AveragePrice: "0.214",
			UpdatedAt: time.Now().UTC(),
		},
	}
	store := &executorFailStore{combo: combo, execution: execution, orders: orders}
	first, err := store.FinalizeOneShotFinalFlattenExecution(context.Background(), execution.ID)
	if err != nil || !first.Applied {
		t.Fatalf("first finalize applied=%v err=%v", first.Applied, err)
	}
	turnover := parseDecimal(first.Combination.CumulativeTurnoverNotional)
	want := decimal.RequireFromString("88").Mul(decimal.RequireFromString("0.214")).
		Add(decimal.RequireFromString("80").Mul(decimal.RequireFromString("0.214")))
	if !turnover.Equal(want) {
		t.Fatalf("turnover=%s want=%s", turnover, want)
	}
	second, err := store.FinalizeOneShotFinalFlattenExecution(context.Background(), execution.ID)
	if err != nil || second.Applied {
		t.Fatalf("second finalize applied=%v err=%v", second.Applied, err)
	}
	if parseDecimal(second.Combination.CumulativeTurnoverNotional).Cmp(turnover) != 0 {
		t.Fatalf("turnover changed %s -> %s", turnover, second.Combination.CumulativeTurnoverNotional)
	}
}

func TestArbitrageExecutorOneShotFinalFlattenPersistsIndependentMarkets(t *testing.T) {
	combo := oneShotFinalFlattenCombo("88", "-80")
	execution := oneShotFinalFlattenExecution(
		combo, decimal.NewFromInt(88), decimal.NewFromInt(80),
	)
	store := &closeOrderStore{executorFailStore: &executorFailStore{
		combo: combo, execution: execution, activeExecution: &execution,
	}}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, nil, "0.214", "0.214")
	executor.Execute(context.Background(), combo, execution, oneShotFinalFlattenBBO(), oneShotFinalFlattenBBO(), nil)
	orders, err := store.ListArbitrageOrders(context.Background(), combo.OwnerUsername, combo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 2 {
		t.Fatalf("orders=%d events=%v", len(orders), store.eventTypes)
	}
	byLeg := map[string]Order{}
	for _, order := range orders {
		byLeg[order.ArbitrageLeg] = order
		if order.ArbitrageRole != "market" || !order.ReduceOnly ||
			!strings.Contains(order.IdempotencyKey, ":final-flatten:") {
			t.Fatalf("order=%+v", order)
		}
	}
	if byLeg["a"].Side != "sell" || byLeg["a"].Quantity != "88" ||
		byLeg["b"].Side != "buy" || byLeg["b"].Quantity != "80" {
		t.Fatalf("orders=%+v", orders)
	}
	if store.combo.Status != "running" || store.combo.OneShotPhase != "exiting" {
		t.Fatalf("combo=%+v", store.combo)
	}
	if store.exited {
		t.Fatal("must not mark exited before venue reconcile")
	}
}

func TestArbitrageExecutorOneShotFinalFlattenSingleLegIntent(t *testing.T) {
	combo := oneShotFinalFlattenCombo("8", "0")
	execution := oneShotFinalFlattenExecution(combo, decimal.NewFromInt(8), decimal.Zero)
	store := &closeOrderStore{executorFailStore: &executorFailStore{
		combo: combo, execution: execution, activeExecution: &execution,
	}}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, nil, "0.214", "0.214")
	executor.Execute(context.Background(), combo, execution, oneShotFinalFlattenBBO(), oneShotFinalFlattenBBO(), nil)
	orders, err := store.ListArbitrageOrders(context.Background(), combo.OwnerUsername, combo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 || orders[0].ArbitrageLeg != "a" || orders[0].Side != "sell" ||
		orders[0].Quantity != "8" {
		t.Fatalf("orders=%+v", orders)
	}
	if len(adapterB.snapshot()) != 0 {
		t.Fatalf("leg b places=%d", len(adapterB.snapshot()))
	}
}

func TestArbitrageExecutorOneShotFinalFlattenPartialDoesNotReverse(t *testing.T) {
	combo := oneShotFinalFlattenCombo("88", "-80")
	execution := oneShotFinalFlattenExecution(
		combo, decimal.NewFromInt(88), decimal.NewFromInt(80),
	)
	store := &closeOrderStore{executorFailStore: &executorFailStore{
		combo: combo, execution: execution, activeExecution: &execution,
	}}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{err: fmtVenueRejected("bounce")}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, nil, "0.214", "0.214")
	executor.Execute(context.Background(), combo, execution, oneShotFinalFlattenBBO(), oneShotFinalFlattenBBO(), nil)
	if store.exitedWithDust {
		t.Fatal("generic venue reject must not dust")
	}
	if parseDecimal(store.combo.LegABasePosition).IsPositive() {
		t.Fatalf("successful sell should flatten A combo=%+v", store.combo)
	}
	if !parseDecimal(store.combo.LegBBasePosition).Equal(decimal.RequireFromString("-80")) {
		t.Fatalf("B must not reverse combo=%+v", store.combo)
	}
	if store.combo.OneShotPhase != "exiting" || store.combo.Status != "running" {
		t.Fatalf("combo=%+v", store.combo)
	}
}

func TestArbitrageExecutorOneShotFinalFlattenMinNotionalDustsWhenFresh(t *testing.T) {
	combo := oneShotFinalFlattenCombo("8", "0")
	combo.LastPositionReconciledAt = time.Now().UTC().Add(time.Minute)
	execution := oneShotFinalFlattenExecution(combo, decimal.NewFromInt(8), decimal.Zero)
	store := &closeOrderStore{executorFailStore: &executorFailStore{
		combo: combo, execution: execution, activeExecution: &execution,
	}}
	adapterA := &flattenVenueAdapter{err: ErrOrderBelowMinimum}
	adapterB := &flattenVenueAdapter{}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, nil, "0.214", "0.214")
	executor.Execute(context.Background(), combo, execution, oneShotFinalFlattenBBO(), oneShotFinalFlattenBBO(), nil)
	if !store.exitedWithDust {
		t.Fatalf("expected dust events=%v combo=%+v", store.eventTypes, store.combo)
	}
}

func TestArbitrageExecutorOneShotFinalFlattenMinNotionalWaitsWithoutSnapshot(t *testing.T) {
	combo := oneShotFinalFlattenCombo("8", "0")
	combo.LastPositionReconciledAt = time.Now().UTC().Add(-time.Hour)
	execution := oneShotFinalFlattenExecution(combo, decimal.NewFromInt(8), decimal.Zero)
	store := &closeOrderStore{executorFailStore: &executorFailStore{
		combo: combo, execution: execution, activeExecution: &execution,
	}}
	adapterA := &flattenVenueAdapter{err: ErrOrderBelowMinimum}
	adapterB := &flattenVenueAdapter{}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, nil, "0.214", "0.214")
	executor.Execute(context.Background(), combo, execution, oneShotFinalFlattenBBO(), oneShotFinalFlattenBBO(), nil)
	if store.exitedWithDust || store.combo.OneShotPhase != "exiting" {
		t.Fatalf("combo=%+v dust=%v", store.combo, store.exitedWithDust)
	}
}

func fmtVenueRejected(message string) error {
	return errors.Join(ErrVenueRejected, errors.New(message))
}

func TestArbitrageExecutorRecoverWithoutMarketDataUsesFlattenPath(t *testing.T) {
	combo := oneShotFinalFlattenCombo("88", "-80")
	execution := oneShotFinalFlattenExecution(
		combo, decimal.NewFromInt(88), decimal.NewFromInt(80),
	)
	execution.MakerOrderID = "uuid-a"
	execution.HedgeOrderID = "uuid-b"
	orderA := Order{
		ID: "uuid-a", ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a",
		ArbitrageRole: "market", Side: "sell", Quantity: "88", Status: "filled",
		FilledQuantity: "88", AveragePrice: "0.214",
		IdempotencyKey: oneShotFinalFlattenIdempotencyKey(execution.ID, "a"),
		UpdatedAt:      time.Now().UTC(),
	}
	orderB := Order{
		ID: "uuid-b", ArbitrageExecutionID: execution.ID, ArbitrageLeg: "b", Side: "buy",
		ArbitrageRole: "market", Quantity: "80", Status: "filled",
		FilledQuantity: "80", AveragePrice: "0.214",
		IdempotencyKey: oneShotFinalFlattenIdempotencyKey(execution.ID, "b"),
		UpdatedAt:      time.Now().UTC(),
	}
	store := &closeOrderStore{executorFailStore: &executorFailStore{
		combo: combo, execution: execution, orders: []Order{orderA, orderB},
	}}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, nil, "0.214", "0.214")
	if err := executor.RecoverWithoutMarketData(context.Background(), combo, execution); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, eventType := range store.eventTypes {
		if eventType == oneShotFinalFlattenStartedEvent {
			found = true
		}
	}
	if !found {
		t.Fatalf("events=%v", store.eventTypes)
	}
}

func TestFinishOneShotFinalFlattenCompensatesThenMarksExited(t *testing.T) {
	combo := oneShotFinalFlattenCombo("0", "0")
	combo.LegAVenueBasePosition = "0"
	combo.LegBVenueBasePosition = "0"
	combo.LastPositionReconciledAt = time.Now().UTC().Add(time.Minute)
	execution := oneShotFinalFlattenExecution(
		combo, decimal.NewFromInt(88), decimal.NewFromInt(80),
	)
	execution.Status = "completed"
	orders := []Order{
		{
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a",
			IdempotencyKey: oneShotFinalFlattenIdempotencyKey(execution.ID, "a"),
			Status:         "filled", FilledQuantity: "88", AveragePrice: "0.214",
			UpdatedAt: time.Now().UTC(),
		},
		{
			ArbitrageExecutionID: execution.ID, ArbitrageLeg: "b",
			IdempotencyKey: oneShotFinalFlattenIdempotencyKey(execution.ID, "b"),
			Status:         "filled", FilledQuantity: "80", AveragePrice: "0.214",
			UpdatedAt: time.Now().UTC(),
		},
	}
	store := &executorFailStore{
		combo: combo, execution: execution, orders: orders, activeExecution: &execution,
	}
	compensator := &recordingFillAverageCompensator{}
	executor := &ArbitrageExecutor{store: store}
	executor.ConfigureFillAverageCompensator(compensator)
	catalog := flattenTestCatalog()
	if err := executor.finishOneShotFinalFlatten(
		context.Background(), store, combo, &execution,
		catalog[101], catalog[202], nil, false,
	); err != nil {
		t.Fatal(err)
	}
	if compensator.called || store.persistFinalCalls != 0 || !store.exited {
		t.Fatalf("compensate=%+v persist=%d exited=%v",
			compensator, store.persistFinalCalls, store.exited)
	}
	if store.combo.FinalMetricsStatus != finalMetricsPending {
		t.Fatalf("metrics=%s", store.combo.FinalMetricsStatus)
	}
}

func TestFinishOneShotFinalFlattenMarksExitedWithoutMetrics(t *testing.T) {
	combo := oneShotFinalFlattenCombo("0", "0")
	combo.LegAVenueBasePosition = "0"
	combo.LegBVenueBasePosition = "0"
	combo.LastPositionReconciledAt = time.Now().UTC().Add(time.Minute)
	execution := oneShotFinalFlattenExecution(
		combo, decimal.NewFromInt(88), decimal.NewFromInt(80),
	)
	execution.Status = "completed"
	orders := []Order{{
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a",
		IdempotencyKey: oneShotFinalFlattenIdempotencyKey(execution.ID, "a"),
		Status:         "filled", FilledQuantity: "88", AveragePrice: "0.214",
		UpdatedAt: time.Now().UTC(),
	}}
	store := &executorFailStore{
		combo: combo, execution: execution, orders: orders, activeExecution: &execution,
		persistFinalErr: ErrFinalMetricsNotReady,
	}
	compensator := &recordingFillAverageCompensator{}
	executor := &ArbitrageExecutor{store: store}
	executor.ConfigureFillAverageCompensator(compensator)
	catalog := flattenTestCatalog()
	if err := executor.finishOneShotFinalFlatten(
		context.Background(), store, combo, &execution,
		catalog[101], catalog[202], nil, false,
	); err != nil {
		t.Fatal(err)
	}
	if compensator.called || store.persistFinalCalls != 0 || !store.exited {
		t.Fatalf("compensate=%+v persist=%d exited=%v",
			compensator, store.persistFinalCalls, store.exited)
	}
	if store.closeFailures != 0 || store.combo.LastFailureKey == finalMetricsNotReadyKey {
		t.Fatalf("failure=%+v closeFailures=%d", store.combo, store.closeFailures)
	}
}

func TestFinishOneShotFinalFlattenDustCompensatesBeforePersist(t *testing.T) {
	combo := oneShotFinalFlattenCombo("8", "0")
	combo.LastPositionReconciledAt = time.Now().UTC().Add(time.Minute)
	execution := oneShotFinalFlattenExecution(combo, decimal.NewFromInt(8), decimal.Zero)
	execution.Status = "failed"
	orders := []Order{{
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: "a",
		IdempotencyKey: oneShotFinalFlattenIdempotencyKey(execution.ID, "a"),
		Status:         "rejected", ErrorMessage: "minimum notional",
		UpdatedAt: time.Now().UTC(),
	}}
	store := &executorFailStore{
		combo: combo, execution: execution, orders: orders, activeExecution: &execution,
	}
	compensator := &recordingFillAverageCompensator{}
	executor := &ArbitrageExecutor{store: store}
	executor.ConfigureFillAverageCompensator(compensator)
	catalog := flattenTestCatalog()
	if err := executor.finishOneShotFinalFlatten(
		context.Background(), store, combo, &execution,
		catalog[101], catalog[202], ErrOrderBelowMinimum, true,
	); err != nil {
		t.Fatal(err)
	}
	if compensator.called || store.persistFinalCalls != 0 || !store.exitedWithDust {
		t.Fatalf("compensate=%+v persist=%d dust=%v",
			compensator, store.persistFinalCalls, store.exitedWithDust)
	}
	if store.combo.FinalMetricsStatus != finalMetricsPartial || store.combo.FinalMetricsRetryFinite {
		t.Fatalf("metrics=%+v", store.combo)
	}
}
