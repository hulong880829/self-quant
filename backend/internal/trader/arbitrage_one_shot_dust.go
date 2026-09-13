package trader

import (
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/marketdata"
)

type oneShotDustVerdict string

const (
	oneShotDustAccepted oneShotDustVerdict = "accepted"
	oneShotDustRetry    oneShotDustVerdict = "retry"
	oneShotDustUnsafe   oneShotDustVerdict = "unsafe"
)

type oneShotDustInput struct {
	Combination      ArbitrageCombination
	Remaining        decimal.Decimal
	Phase            string
	InstrumentA      Instrument
	InstrumentB      Instrument
	InstrumentErr    error
	PriceA           decimal.Decimal
	PriceB           decimal.Decimal
	LiveOrder        bool
	Unreconciled     bool
	LiveExecution    bool
	GateErr          error
	ExecutionRunning bool
}

func evaluateOneShotDust(in oneShotDustInput) oneShotDustVerdict {
	combo := in.Combination
	if !strings.EqualFold(strings.TrimSpace(combo.RunMode), "one_shot") ||
		!strings.EqualFold(strings.TrimSpace(combo.Status), "running") {
		return oneShotDustRetry
	}
	phase := strings.TrimSpace(combo.OneShotPhase)
	if phase != in.Phase {
		return oneShotDustRetry
	}
	if combo.RuntimeState == "manual_intervention" {
		return oneShotDustRetry
	}
	if combo.PositionUncertain || combo.CircuitOpen || in.ExecutionRunning {
		return oneShotDustRetry
	}
	if in.GateErr != nil {
		return oneShotDustRetry
	}
	if in.LiveOrder || in.LiveExecution || in.Unreconciled {
		return oneShotDustRetry
	}
	if combo.LastPositionReconciledAt.IsZero() ||
		combo.LastPositionReconciledAt.Equal(time.Unix(0, 0).UTC()) {
		return oneShotDustRetry
	}
	if !closeFlattenBothPerpetual(combo.LegA.ContractType, combo.LegB.ContractType) {
		if phase == "exiting" {
			return oneShotDustUnsafe
		}
		return oneShotDustRetry
	}
	if in.InstrumentErr != nil {
		return oneShotDustRetry
	}
	legA := parseDecimal(combo.LegABasePosition)
	legB := parseDecimal(combo.LegBBasePosition)
	carry := parseDecimal(combo.CarryBaseQuantity)
	if !carry.Equal(legA.Add(legB)) {
		return oneShotDustUnsafe
	}
	switch phase {
	case "building_target":
		return evaluateBuildingOneShotDust(in, legA, legB, carry)
	case "exiting":
		return evaluateExitingOneShotDust(in, legA, legB, carry)
	default:
		return oneShotDustRetry
	}
}

func evaluateBuildingOneShotDust(
	in oneShotDustInput,
	legA, legB, carry decimal.Decimal,
) oneShotDustVerdict {
	if in.Remaining.GreaterThanOrEqual(decimal.NewFromInt(arbitrageMinOpenNotional)) {
		return oneShotDustRetry
	}
	if legA.IsZero() || legB.IsZero() || legA.Sign() == legB.Sign() {
		return oneShotDustUnsafe
	}
	if carry.IsZero() {
		return oneShotDustAccepted
	}
	if !in.PriceA.IsPositive() || !in.PriceB.IsPositive() {
		return oneShotDustRetry
	}
	residual := carry.Abs()
	residualNotional := decimal.Max(residual.Mul(in.PriceA), residual.Mul(in.PriceB))
	if residualNotional.GreaterThanOrEqual(decimal.NewFromInt(arbitrageMinOpenNotional)) {
		return oneShotDustUnsafe
	}
	_, eligibleA, errA := executableHedgeQuantity(residual, in.PriceA, in.InstrumentA, false)
	_, eligibleB, errB := executableHedgeQuantity(residual, in.PriceB, in.InstrumentB, false)
	if errA != nil || errB != nil {
		return oneShotDustRetry
	}
	if eligibleA || eligibleB {
		return oneShotDustUnsafe
	}
	if hedgeQuantityDustReason(residual, in.PriceA, in.InstrumentA) == "" &&
		hedgeQuantityDustReason(residual, in.PriceB, in.InstrumentB) == "" {
		return oneShotDustRetry
	}
	return oneShotDustAccepted
}

func evaluateExitingOneShotDust(
	in oneShotDustInput,
	legA, legB, carry decimal.Decimal,
) oneShotDustVerdict {
	aNZ := !legA.IsZero()
	bNZ := !legB.IsZero()
	if aNZ == bNZ || carry.IsZero() {
		return oneShotDustUnsafe
	}
	residual := legA.Abs()
	price := in.PriceA
	instrument := in.InstrumentA
	if bNZ {
		residual = legB.Abs()
		price = in.PriceB
		instrument = in.InstrumentB
	}
	if !price.IsPositive() {
		return oneShotDustRetry
	}
	residualNotional := residual.Mul(price)
	if residualNotional.GreaterThanOrEqual(decimal.NewFromInt(arbitrageMinOpenNotional)) {
		return oneShotDustUnsafe
	}
	_, eligible, err := executableHedgeQuantity(residual, price, instrument, false)
	if err != nil {
		return oneShotDustRetry
	}
	if eligible {
		return oneShotDustUnsafe
	}
	if hedgeQuantityDustReason(residual, price, instrument) == "" {
		return oneShotDustRetry
	}
	return oneShotDustAccepted
}

func oneShotLeftoverResidual(
	combo ArbitrageCombination,
	bboA, bboB marketdata.BBO,
) (qty, notional decimal.Decimal, single bool) {
	legA := parseDecimal(combo.LegABasePosition)
	legB := parseDecimal(combo.LegBBasePosition)
	aNZ := !legA.IsZero()
	bNZ := !legB.IsZero()
	if aNZ == bNZ {
		return decimal.Zero, decimal.Zero, false
	}
	if aNZ {
		price := closeFlattenOwnedExecutablePrice(legA, bboA)
		return legA.Abs(), legA.Abs().Mul(price), true
	}
	price := closeFlattenOwnedExecutablePrice(legB, bboB)
	return legB.Abs(), legB.Abs().Mul(price), true
}

func oneShotRunningExiting(combo ArbitrageCombination) bool {
	return strings.EqualFold(strings.TrimSpace(combo.Status), "running") &&
		strings.EqualFold(strings.TrimSpace(combo.RunMode), "one_shot") &&
		strings.EqualFold(strings.TrimSpace(combo.OneShotPhase), "exiting")
}

func carryClosePrices(carry decimal.Decimal, bboA, bboB marketdata.BBO) (decimal.Decimal, decimal.Decimal) {
	side := carryHedgeSide(carry)
	return parsePositiveDecimal(bboPriceForSide(bboA, side)),
		parsePositiveDecimal(bboPriceForSide(bboB, side))
}
