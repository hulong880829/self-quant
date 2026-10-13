package trader

import (
	"strings"

	"github.com/shopspring/decimal"
)

const (
	lastCloseResidualRecoveredEvent         = "last_close_residual_recovered"
	lastCloseResidualAuditedReason          = "last_close_residual_audited"
	lastCloseRecoveryOrderNotTerminalEvent  = "last_close_recovery_order_not_terminal"
	lastCloseRecoveryFillMismatchEvent      = "last_close_recovery_fill_mismatch"
	lastCloseResidualDustEvent              = "closed_with_dust"
	lastCloseResidualFlattenIdempotencyRole = "last-close-residual-flatten"
)

func lastCloseResidualAuditUnlockEligible(
	status, runtimeState, currentError string,
	currentUncertain, circuitOpen, lastCloseResidualRecoveredLatest, zeroDifferences bool,
) bool {
	return strings.EqualFold(strings.TrimSpace(status), "closing") &&
		currentUncertain &&
		strings.EqualFold(strings.TrimSpace(runtimeState), "position_uncertain") &&
		currentError == ErrArbitrageUnsafeHedge.Error() &&
		!circuitOpen &&
		zeroDifferences &&
		lastCloseResidualRecoveredLatest
}

type lastCloseClipAllocatedFills struct {
	MakerFill          decimal.Decimal
	NormalHedgeFill    decimal.Decimal
	ResidualFill       decimal.Decimal
	AllocatedResidual  decimal.Decimal
	EffectiveHedgeFill decimal.Decimal
	FillA              decimal.Decimal
	FillB              decimal.Decimal
	Found              bool
}

func lastCloseClipClosingEligible(
	combination ArbitrageCombination,
	execution ArbitrageExecution,
) bool {
	return strings.EqualFold(strings.TrimSpace(combination.Status), "closing") &&
		execution.LastCloseClip &&
		execution.ReduceOnly &&
		strings.EqualFold(strings.TrimSpace(execution.PositionEffect), "close")
}

func lastCloseClipLockedResumeEligible(
	combination ArbitrageCombination,
	execution ArbitrageExecution,
) bool {
	if !lastCloseClipClosingEligible(combination, execution) ||
		!combination.PositionUncertain ||
		combination.CircuitOpen {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(combination.RuntimeState), "position_uncertain") {
		return false
	}
	if combination.ErrorMessage != ErrArbitrageUnsafeHedge.Error() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(execution.Status)) {
	case "hedging", "reconciling":
		return true
	default:
		return false
	}
}

func lastCloseClipMakerHedgeLegs(combination ArbitrageCombination) (makerLeg, hedgeLeg string) {
	if strings.EqualFold(strings.TrimSpace(combination.MakerLeg), "b") {
		return "b", "a"
	}
	return "a", "b"
}

func lastCloseClipHedgeSide(
	combination ArbitrageCombination,
	execution ArbitrageExecution,
) (string, bool) {
	legASide, legBSide, ok := arbitrageLegSides(execution.Direction)
	if !ok {
		return "", false
	}
	_, hedgeLeg := lastCloseClipMakerHedgeLegs(combination)
	if hedgeLeg == "a" {
		return legASide, true
	}
	return legBSide, true
}

func aggregateSimultaneousMarketLastCloseFills(
	executionID string, orders []Order,
) (fillA, fillB decimal.Decimal, found bool) {
	executionID = strings.TrimSpace(executionID)
	for _, order := range orders {
		if strings.TrimSpace(order.ArbitrageExecutionID) != executionID {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(order.ArbitrageRole), "market") {
			continue
		}
		if !terminalStatus(order.Status) {
			continue
		}
		found = true
		filled := parseDecimal(order.FilledQuantity)
		if !filled.IsPositive() {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(order.ArbitrageLeg)) {
		case "a":
			fillA = fillA.Add(filled)
		case "b":
			fillB = fillB.Add(filled)
		}
	}
	return fillA, fillB, found
}

func allocateLastCloseClipResidualFills(
	orders []Order,
	combination ArbitrageCombination,
	execution ArbitrageExecution,
) lastCloseClipAllocatedFills {
	makerLeg, hedgeLeg := lastCloseClipMakerHedgeLegs(combination)
	hedgeSide, sideOK := lastCloseClipHedgeSide(combination, execution)
	var out lastCloseClipAllocatedFills
	for _, order := range orders {
		if strings.TrimSpace(order.ArbitrageExecutionID) != strings.TrimSpace(execution.ID) {
			continue
		}
		out.Found = true
		filled := parseDecimal(order.FilledQuantity)
		if !filled.IsPositive() {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(order.ArbitrageRole))
		leg := strings.ToLower(strings.TrimSpace(order.ArbitrageLeg))
		side := strings.ToLower(strings.TrimSpace(order.Side))
		switch {
		case role == "maker" && leg == makerLeg:
			out.MakerFill = out.MakerFill.Add(filled)
		case role == "hedge" && leg == hedgeLeg && sideOK && side == hedgeSide:
			out.NormalHedgeFill = out.NormalHedgeFill.Add(filled)
		case role == "residual" && leg == hedgeLeg && sideOK && side == hedgeSide:
			out.ResidualFill = out.ResidualFill.Add(filled)
		}
	}
	gap := out.MakerFill.Sub(out.NormalHedgeFill)
	if gap.IsNegative() {
		gap = decimal.Zero
	}
	out.AllocatedResidual = decimal.Min(out.ResidualFill, gap)
	out.EffectiveHedgeFill = out.NormalHedgeFill.Add(out.AllocatedResidual)
	if makerLeg == "b" {
		out.FillA = out.EffectiveHedgeFill
		out.FillB = out.MakerFill
	} else {
		out.FillA = out.MakerFill
		out.FillB = out.EffectiveHedgeFill
	}
	return out
}

func lastCloseRecoveryMark(orders []Order, execution ArbitrageExecution) decimal.Decimal {
	var notionalA, qtyA, notionalB, qtyB decimal.Decimal
	for _, order := range orders {
		if strings.TrimSpace(order.ArbitrageExecutionID) != strings.TrimSpace(execution.ID) {
			continue
		}
		filled := parseDecimal(order.FilledQuantity)
		price := parsePositiveDecimal(order.AveragePrice)
		if !filled.IsPositive() || !price.IsPositive() {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(order.ArbitrageLeg)) {
		case "a":
			qtyA = qtyA.Add(filled)
			notionalA = notionalA.Add(filled.Mul(price))
		case "b":
			qtyB = qtyB.Add(filled)
			notionalB = notionalB.Add(filled.Mul(price))
		}
	}
	var vwapA, vwapB decimal.Decimal
	if qtyA.IsPositive() {
		vwapA = notionalA.Div(qtyA)
	}
	if qtyB.IsPositive() {
		vwapB = notionalB.Div(qtyB)
	}
	if vwapA.IsPositive() && vwapB.IsPositive() {
		return vwapA.Add(vwapB).Div(decimal.NewFromInt(2))
	}
	if vwapA.IsPositive() {
		return vwapA
	}
	if vwapB.IsPositive() {
		return vwapB
	}
	return lastCloseTriggerMark(execution)
}

func lastCloseTriggerMark(execution ArbitrageExecution) decimal.Decimal {
	midA := lastCloseTriggerLegMid(execution.TriggerLegABid, execution.TriggerLegAAsk)
	midB := lastCloseTriggerLegMid(execution.TriggerLegBBid, execution.TriggerLegBAsk)
	if midA.IsPositive() && midB.IsPositive() {
		return midA.Add(midB).Div(decimal.NewFromInt(2))
	}
	if midA.IsPositive() {
		return midA
	}
	return midB
}

func lastCloseTriggerLegMid(bid, ask string) decimal.Decimal {
	bidPrice := parsePositiveDecimal(bid)
	askPrice := parsePositiveDecimal(ask)
	if bidPrice.IsPositive() && askPrice.IsPositive() {
		return bidPrice.Add(askPrice).Div(decimal.NewFromInt(2))
	}
	if bidPrice.IsPositive() {
		return bidPrice
	}
	return askPrice
}

func (a lastCloseClipAllocatedFills) matched() bool {
	return a.MakerFill.IsPositive() && a.MakerFill.Equal(a.EffectiveHedgeFill)
}

func (a lastCloseClipAllocatedFills) residualShort() bool {
	return a.ResidualFill.IsPositive() && a.EffectiveHedgeFill.LessThan(a.MakerFill)
}

func lastCloseClipExecutionOrders(orders []Order, executionID string) []Order {
	id := strings.TrimSpace(executionID)
	if id == "" {
		return nil
	}
	matched := make([]Order, 0, len(orders))
	for _, order := range orders {
		if strings.TrimSpace(order.ArbitrageExecutionID) != id {
			continue
		}
		matched = append(matched, order)
	}
	return matched
}

func lastCloseClipOrdersBlocked(orders []Order) bool {
	for _, order := range orders {
		if !terminalStatus(order.Status) || order.ReconcileFailures > 0 {
			return true
		}
	}
	return false
}

func lastCloseClipOrdersAllReduceOnly(orders []Order) bool {
	if len(orders) == 0 {
		return false
	}
	for _, order := range orders {
		if !order.ReduceOnly {
			return false
		}
	}
	return true
}
