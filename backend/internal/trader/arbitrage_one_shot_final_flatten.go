package trader

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

const (
	oneShotFinalFlattenIdempotencyRole   = "final-flatten"
	oneShotFinalFlattenStartedEvent      = "one_shot_final_flatten_started"
	oneShotFinalFlattenLegSubmittedEvent = "one_shot_final_flatten_leg_submitted"
	oneShotFinalFlattenLegTerminalEvent  = "one_shot_final_flatten_leg_terminal"
	oneShotFinalFlattenRetryEvent        = "one_shot_final_flatten_retry"
	oneShotFinalFlattenCompletedEvent    = "one_shot_final_flatten_completed"
	oneShotFinalFlattenUnexecutableEvent = "one_shot_final_flatten_unexecutable_dust"
)

type OwnedFlattenLeg struct {
	Leg          ArbitrageLeg
	LegName      string
	Side         string
	BaseQuantity decimal.Decimal
	ReduceOnly   bool
	OrderType    string
}

type OneShotFinalFlattenClaim struct {
	Execution    ArbitrageExecution
	LegAQuantity decimal.Decimal
	LegBQuantity decimal.Decimal
	LegANotional decimal.Decimal
	LegBNotional decimal.Decimal
	ReconciledAt time.Time
}

type oneShotFinalFlattenFinalizeResult struct {
	Combination ArbitrageCombination
	Execution   ArbitrageExecution
	Applied     bool
}

func planOwnedFlattenLegs(
	combination ArbitrageCombination,
	instrumentA, instrumentB Instrument,
) []OwnedFlattenLeg {
	legs := make([]OwnedFlattenLeg, 0, 2)
	if leg, ok := planOwnedFlattenLeg(combination, instrumentA, "a", combination.LegA); ok {
		legs = append(legs, leg)
	}
	if leg, ok := planOwnedFlattenLeg(combination, instrumentB, "b", combination.LegB); ok {
		legs = append(legs, leg)
	}
	return legs
}

func planOwnedFlattenLeg(
	combination ArbitrageCombination,
	instrument Instrument,
	legName string,
	leg ArbitrageLeg,
) (OwnedFlattenLeg, bool) {
	if !closeFlattenOwnedVenueReducible(combination, legName) {
		return OwnedFlattenLeg{}, false
	}
	placed := closeFlattenOwnedPlaceBase(combination, instrument, legName)
	qty := placed.Abs()
	if !qty.IsPositive() {
		return OwnedFlattenLeg{}, false
	}
	return OwnedFlattenLeg{
		Leg:          leg,
		LegName:      strings.ToLower(strings.TrimSpace(legName)),
		Side:         closeFlattenSide(placed),
		BaseQuantity: qty,
		ReduceOnly:   true,
		OrderType:    "market",
	}, true
}

func oneShotFinalFlattenLegCap(combination ArbitrageCombination, legName string) decimal.Decimal {
	owned, venue, baseline := closeFlattenOwnedLegValues(combination, legName)
	if owned.IsZero() {
		return decimal.Zero
	}
	if !closeFlattenOwnedVenueReducible(combination, legName) {
		return decimal.Zero
	}
	return decimal.Min(owned.Abs(), venue.Sub(baseline).Abs())
}

func oneShotFinalFlattenPlannedQuantities(legs []OwnedFlattenLeg) (qtyA, qtyB decimal.Decimal) {
	for _, leg := range legs {
		switch strings.ToLower(strings.TrimSpace(leg.LegName)) {
		case "b":
			qtyB = leg.BaseQuantity
		default:
			qtyA = leg.BaseQuantity
		}
	}
	return qtyA, qtyB
}

func oneShotFinalFlattenDirection(combination ArbitrageCombination, qtyA, qtyB decimal.Decimal) string {
	ownedA := parseDecimal(combination.LegABasePosition)
	ownedB := parseDecimal(combination.LegBBasePosition)
	if qtyA.IsPositive() {
		if closeFlattenSide(ownedA) == "sell" {
			return "bid"
		}
		return "ask"
	}
	if qtyB.IsPositive() {
		if closeFlattenSide(ownedB) == "buy" {
			return "bid"
		}
		return "ask"
	}
	return ""
}

func oneShotFinalFlattenRemainingBelowThreshold(
	combination ArbitrageCombination,
	bboA, bboB marketdata.BBO,
) bool {
	threshold := decimal.NewFromInt(arbitrageMinOpenNotional)
	ownedA := parseDecimal(combination.LegABasePosition)
	ownedB := parseDecimal(combination.LegBBasePosition)
	if ownedA.IsZero() && ownedB.IsZero() {
		return false
	}
	if !ownedA.IsZero() {
		price := closeFlattenOwnedExecutablePrice(ownedA, bboA)
		if !price.IsPositive() || !ownedA.Abs().Mul(price).LessThan(threshold) {
			return false
		}
	}
	if !ownedB.IsZero() {
		price := closeFlattenOwnedExecutablePrice(ownedB, bboB)
		if !price.IsPositive() || !ownedB.Abs().Mul(price).LessThan(threshold) {
			return false
		}
	}
	return true
}

func oneShotFinalFlattenRemainingBelowThresholdFromExecution(
	combination ArbitrageCombination,
	execution ArbitrageExecution,
) bool {
	threshold := decimal.NewFromInt(arbitrageMinOpenNotional)
	ownedA := parseDecimal(combination.LegABasePosition)
	ownedB := parseDecimal(combination.LegBBasePosition)
	if ownedA.IsZero() && ownedB.IsZero() {
		return false
	}
	if !ownedA.IsZero() {
		price := oneShotFinalFlattenTriggerPrice(execution, ownedA, "a")
		if !price.IsPositive() || !ownedA.Abs().Mul(price).LessThan(threshold) {
			return false
		}
	}
	if !ownedB.IsZero() {
		price := oneShotFinalFlattenTriggerPrice(execution, ownedB, "b")
		if !price.IsPositive() || !ownedB.Abs().Mul(price).LessThan(threshold) {
			return false
		}
	}
	return true
}

func oneShotFinalFlattenTriggerPrice(
	execution ArbitrageExecution,
	owned decimal.Decimal,
	legName string,
) decimal.Decimal {
	if owned.IsZero() {
		return decimal.Zero
	}
	legB := strings.EqualFold(strings.TrimSpace(legName), "b")
	if owned.IsPositive() {
		if legB {
			return parsePositiveDecimal(execution.TriggerLegBBid)
		}
		return parsePositiveDecimal(execution.TriggerLegABid)
	}
	if legB {
		return parsePositiveDecimal(execution.TriggerLegBAsk)
	}
	return parsePositiveDecimal(execution.TriggerLegAAsk)
}

func oneShotFinalFlattenReady(
	combination ArbitrageCombination,
	instrumentA, instrumentB Instrument,
	bboA, bboB marketdata.BBO,
	now time.Time,
) bool {
	if !strings.EqualFold(strings.TrimSpace(combination.RunMode), "one_shot") {
		return false
	}
	if !oneShotRunningExiting(combination) {
		return false
	}
	if combination.PositionUncertain {
		return false
	}
	if combination.RuntimeState == "manual_intervention" {
		return false
	}
	if !closeFlattenBothPerpetual(combination.LegA.ContractType, combination.LegB.ContractType) {
		return false
	}
	if !circuitOpenBaselineComplete(combination) {
		return false
	}
	if !closeFlattenSnapshotFresh(combination.LastPositionReconciledAt, now) {
		return false
	}
	if !oneShotFinalFlattenRemainingBelowThreshold(combination, bboA, bboB) {
		return false
	}
	planned := planOwnedFlattenLegs(combination, instrumentA, instrumentB)
	if len(planned) == 0 {
		return false
	}
	for _, leg := range planned {
		if !closeFlattenOwnedVenueReducible(combination, leg.LegName) {
			return false
		}
	}
	return true
}

func oneShotFinalFlattenClaimShape(
	combination ArbitrageCombination,
	execution ArbitrageExecution,
) bool {
	if !oneShotRunningExiting(combination) {
		return false
	}
	return execution.LastCloseClip &&
		execution.ReduceOnly &&
		strings.EqualFold(strings.TrimSpace(execution.PositionEffect), "close")
}

func oneShotFinalFlattenIdempotencyKey(executionID, legName string) string {
	return fmt.Sprintf(
		"arb:%s:%s:%s:0",
		strings.TrimSpace(executionID),
		strings.ToLower(strings.TrimSpace(legName)),
		oneShotFinalFlattenIdempotencyRole,
	)
}

func hasOneShotFinalFlattenIdempotency(orders []Order, executionID string) bool {
	needle := ":" + oneShotFinalFlattenIdempotencyRole + ":"
	for _, order := range orders {
		if strings.TrimSpace(order.ArbitrageExecutionID) != strings.TrimSpace(executionID) {
			continue
		}
		if strings.Contains(order.IdempotencyKey, needle) {
			return true
		}
	}
	return false
}

func hasMakerOrHedgeRole(orders []Order, executionID string) bool {
	for _, order := range orders {
		if strings.TrimSpace(order.ArbitrageExecutionID) != strings.TrimSpace(executionID) {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(order.ArbitrageRole))
		if role == "maker" || role == "hedge" {
			return true
		}
	}
	return false
}

func executionOrdersFor(orders []Order, executionID string) []Order {
	matched := make([]Order, 0)
	for _, order := range orders {
		if strings.TrimSpace(order.ArbitrageExecutionID) == strings.TrimSpace(executionID) {
			matched = append(matched, order)
		}
	}
	return matched
}

func isOneShotFinalFlattenExecution(
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	orders []Order,
	bboA, bboB marketdata.BBO,
) bool {
	related := executionOrdersFor(orders, execution.ID)
	if hasOneShotFinalFlattenIdempotency(related, execution.ID) {
		return true
	}
	if hasMakerOrHedgeRole(related, execution.ID) {
		return false
	}
	if len(related) > 0 {
		return false
	}
	if !oneShotFinalFlattenClaimShape(combination, execution) {
		return false
	}
	if bboA.BidPrice == "" && bboA.AskPrice == "" && bboB.BidPrice == "" && bboB.AskPrice == "" {
		return oneShotFinalFlattenRemainingBelowThresholdFromExecution(combination, execution)
	}
	return oneShotFinalFlattenRemainingBelowThreshold(combination, bboA, bboB)
}

func closeFlattenPlaceRequest(
	instrument Instrument,
	base decimal.Decimal,
) (exchange.OrderRequest, bool) {
	quantity := closeFlattenQuantity(instrument, base)
	if !quantity.IsPositive() {
		return exchange.OrderRequest{}, false
	}
	venueInstrument := toVenueInstrument(instrument)
	if _, err := exchange.ToVenueQuantity(venueInstrument, quantity.String()); err != nil {
		return exchange.OrderRequest{}, false
	}
	return exchange.OrderRequest{
		Instrument: venueInstrument,
		Side:       closeFlattenSide(base),
		OrderType:  "market",
		Quantity:   quantity.String(),
		ReduceOnly: true,
	}, true
}

func oneShotFinalFlattenOrderIntent(
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	leg OwnedFlattenLeg,
	instrument Instrument,
) Order {
	intent := Order{
		IdempotencyKey:   oneShotFinalFlattenIdempotencyKey(execution.ID, leg.LegName),
		OwnerUsername:    combination.OwnerUsername,
		TradingAccountID: leg.Leg.TradingAccountID, ProductName: leg.Leg.ProductName,
		Exchange: leg.Leg.Exchange, InstrumentID: instrument.ID,
		ContractType: instrument.ContractType, ExchangeSymbol: instrument.ExchangeSymbol,
		BaseAsset: instrument.BaseAsset, QuoteAsset: instrument.QuoteAsset,
		Side: leg.Side, OrderType: "market", Quantity: leg.BaseQuantity.String(),
		RequestFingerprint: requestFingerprint(
			leg.Leg.TradingAccountID, instrument.ID, leg.Side, "market",
			leg.BaseQuantity.String(), "", "reduce_only",
		),
		ArbitrageExecutionID: execution.ID, ArbitrageLeg: leg.LegName,
		ArbitrageRole: "market", ReduceOnly: true,
	}
	return intent
}

func validateOneShotFinalFlattenClaim(
	combination ArbitrageCombination,
	claim OneShotFinalFlattenClaim,
	now time.Time,
) bool {
	execution := claim.Execution
	if !oneShotFinalFlattenClaimShape(combination, execution) {
		return false
	}
	if combination.PositionUncertain ||
		combination.RuntimeState == "manual_intervention" {
		return false
	}
	if !closeFlattenBothPerpetual(combination.LegA.ContractType, combination.LegB.ContractType) {
		return false
	}
	if !circuitOpenBaselineComplete(combination) {
		return false
	}
	if !closeFlattenSnapshotFresh(combination.LastPositionReconciledAt, now) {
		return false
	}
	if !combination.LastPositionReconciledAt.Equal(claim.ReconciledAt) {
		return false
	}
	if !oneShotFinalFlattenRemainingBelowThresholdFromExecution(combination, execution) {
		return false
	}
	qtyA := claim.LegAQuantity
	qtyB := claim.LegBQuantity
	if qtyA.IsNegative() || qtyB.IsNegative() {
		return false
	}
	if !qtyA.IsPositive() && !qtyB.IsPositive() {
		return false
	}
	if qtyA.IsPositive() {
		if !closeFlattenOwnedVenueReducible(combination, "a") {
			return false
		}
		if qtyA.GreaterThan(oneShotFinalFlattenLegCap(combination, "a")) {
			return false
		}
	}
	if qtyB.IsPositive() {
		if !closeFlattenOwnedVenueReducible(combination, "b") {
			return false
		}
		if qtyB.GreaterThan(oneShotFinalFlattenLegCap(combination, "b")) {
			return false
		}
	}
	target := parsePositiveDecimal(execution.TargetBaseQuantity)
	if !target.Equal(decimal.Max(qtyA, qtyB)) {
		return false
	}
	requested := parsePositiveDecimal(execution.RequestedNotional)
	if !requested.Equal(decimal.Max(claim.LegANotional, claim.LegBNotional)) {
		return false
	}
	if !requested.IsPositive() || !target.IsPositive() {
		return false
	}
	return true
}

func oneShotFinalFlattenVenueDeltaFlat(
	combination ArbitrageCombination,
	instrumentA, instrumentB Instrument,
) bool {
	deltaA := parseDecimal(combination.LegAVenueBasePosition).
		Sub(parseDecimal(combination.LegAVenueBaselineBasePosition))
	deltaB := parseDecimal(combination.LegBVenueBasePosition).
		Sub(parseDecimal(combination.LegBVenueBaselineBasePosition))
	return positionDifferenceWithinMarketStep(deltaA, closeFlattenQuantityStep(instrumentA)) &&
		positionDifferenceWithinMarketStep(deltaB, closeFlattenQuantityStep(instrumentB))
}

func oneShotFinalFlattenReconcileReady(
	combination ArbitrageCombination,
	orders []Order,
	executionID string,
	instrumentA, instrumentB Instrument,
) bool {
	if !combinationLooksFlat(combination) {
		return false
	}
	if !circuitOpenBaselineComplete(combination) {
		return false
	}
	latest := time.Time{}
	needle := ":" + oneShotFinalFlattenIdempotencyRole + ":"
	for _, order := range orders {
		if executionID != "" &&
			strings.TrimSpace(order.ArbitrageExecutionID) != strings.TrimSpace(executionID) {
			continue
		}
		if !strings.Contains(order.IdempotencyKey, needle) {
			continue
		}
		if order.UpdatedAt.After(latest) {
			latest = order.UpdatedAt
		}
	}
	if !latest.IsZero() && combination.LastPositionReconciledAt.Before(latest) {
		return false
	}
	return oneShotFinalFlattenVenueDeltaFlat(combination, instrumentA, instrumentB)
}

func oneShotFinalFlattenUnexecutable(err error) bool {
	if err == nil {
		return false
	}
	if lastClipHedgeMinNotionalError(err) {
		return true
	}
	if errors.Is(err, exchange.ErrInvalidQuantity) {
		return true
	}
	lower := strings.ToLower(sanitizeError(err))
	for _, needle := range []string{
		"lot size", "step size", "quantity precision", "price precision",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func oneShotFinalFlattenFillNotional(order Order) decimal.Decimal {
	filled := parseDecimal(order.FilledQuantity)
	if !filled.IsPositive() {
		return decimal.Zero
	}
	price := parsePositiveDecimal(order.AveragePrice)
	if !price.IsPositive() {
		price = parsePositiveDecimal(order.Price)
	}
	if !price.IsPositive() {
		return decimal.Zero
	}
	return filled.Mul(price)
}

func oneShotFinalFlattenFillTotals(
	orders []Order,
	executionID string,
) (fillA, fillB, notional decimal.Decimal) {
	needle := ":" + oneShotFinalFlattenIdempotencyRole + ":"
	for _, order := range executionOrdersFor(orders, executionID) {
		if !strings.Contains(order.IdempotencyKey, needle) {
			continue
		}
		filled := parseDecimal(order.FilledQuantity)
		notional = notional.Add(oneShotFinalFlattenFillNotional(order))
		switch strings.ToLower(strings.TrimSpace(order.ArbitrageLeg)) {
		case "b":
			fillB = fillB.Add(filled)
		default:
			fillA = fillA.Add(filled)
		}
	}
	return fillA, fillB, notional
}

func oneShotFinalFlattenOrdersTerminal(orders []Order, executionID string) bool {
	needle := ":" + oneShotFinalFlattenIdempotencyRole + ":"
	for _, order := range executionOrdersFor(orders, executionID) {
		if !strings.Contains(order.IdempotencyKey, needle) {
			continue
		}
		if !terminalStatus(order.Status) {
			return false
		}
	}
	return true
}

func oneShotFinalFlattenLatestOrderAt(orders []Order, executionID string) time.Time {
	var latest time.Time
	for _, order := range executionOrdersFor(orders, executionID) {
		if order.UpdatedAt.After(latest) {
			latest = order.UpdatedAt
		}
	}
	return latest
}

func applyOneShotFinalFlattenLedger(
	combo ArbitrageCombination,
	orders []Order,
	executionID string,
) ArbitrageCombination {
	needle := ":" + oneShotFinalFlattenIdempotencyRole + ":"
	legA := parseDecimal(combo.LegABasePosition)
	legB := parseDecimal(combo.LegBBasePosition)
	for _, order := range executionOrdersFor(orders, executionID) {
		if !strings.Contains(order.IdempotencyKey, needle) {
			continue
		}
		filled := parseDecimal(order.FilledQuantity)
		if !filled.IsPositive() {
			continue
		}
		if strings.EqualFold(order.Side, "sell") {
			filled = filled.Neg()
		}
		if strings.EqualFold(strings.TrimSpace(order.ArbitrageLeg), "b") {
			legB = legB.Add(filled)
		} else {
			legA = legA.Add(filled)
		}
	}
	combo.LegABasePosition = legA.String()
	combo.LegBBasePosition = legB.String()
	combo.CarryBaseQuantity = legA.Add(legB).String()
	return combo
}
