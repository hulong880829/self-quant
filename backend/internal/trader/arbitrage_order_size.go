package trader

import (
	"crypto/rand"
	"encoding/binary"
	"strings"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

const (
	arbitrageMinOpenNotional    = 20
	arbitrageMaxOrderNotional   = 1000
	arbitrageRandomNotionalMax  = 20
	arbitrageMakerDepthFraction = "0.05"
)

type arbitrageNotionalRand interface {
	RandomNotional() decimal.Decimal
}

type cryptoNotionalRand struct{}

func (cryptoNotionalRand) RandomNotional() decimal.Decimal {
	var buf [2]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return decimal.Zero
	}
	cents := int(binary.BigEndian.Uint16(buf[:])) % (arbitrageRandomNotionalMax * 100)
	return decimal.NewFromInt(int64(cents)).Div(decimal.NewFromInt(100))
}

type fixedNotionalRand struct {
	value decimal.Decimal
}

func (r fixedNotionalRand) RandomNotional() decimal.Decimal {
	return r.value
}

type arbitrageSizePlan struct {
	Direction          string
	Effect             string
	ReduceOnly         bool
	RequestedNotional  decimal.Decimal
	TargetBaseQuantity decimal.Decimal
	RandomNotional     decimal.Decimal
	HedgeCapNotional   decimal.Decimal
	LegANotional       decimal.Decimal
	LegBNotional       decimal.Decimal
	LastClip           bool
}

func combinationLooksFlat(item ArbitrageCombination) bool {
	return !parseDecimal(item.LegABasePosition).Abs().IsPositive() &&
		!parseDecimal(item.LegBBasePosition).Abs().IsPositive() &&
		parseDecimal(item.CarryBaseQuantity).IsZero()
}
func remainingDirectionNotional(
	legABase, legBBase, target, midA, midB decimal.Decimal,
) decimal.Decimal {
	if !target.IsPositive() {
		return decimal.Zero
	}
	usedA := legABase.Abs().Mul(midA)
	usedB := legBBase.Abs().Mul(midB)
	used := decimal.Max(usedA, usedB)
	remaining := target.Sub(used)
	if remaining.IsNegative() {
		return decimal.Zero
	}
	return remaining
}

func closeableNotional(
	legABase, legBBase, priceA, priceB decimal.Decimal,
) decimal.Decimal {
	if !priceA.IsPositive() || !priceB.IsPositive() {
		return decimal.Zero
	}
	availableA := decimal.Zero
	if legABase.IsPositive() {
		availableA = legABase
	} else if legABase.IsNegative() {
		availableA = legABase.Abs()
	}
	availableB := decimal.Zero
	if legBBase.IsNegative() {
		availableB = legBBase.Abs()
	} else if legBBase.IsPositive() {
		availableB = legBBase
	}
	if !availableA.IsPositive() || !availableB.IsPositive() {
		return decimal.Zero
	}
	base := decimal.Min(availableA, availableB)
	return decimal.Max(base.Mul(priceA), base.Mul(priceB))
}

func topOfBookNotional(
	instrument Instrument,
	price, rawQuantity string,
) (decimal.Decimal, bool) {
	px := parsePositiveDecimal(price)
	if !px.IsPositive() || strings.TrimSpace(rawQuantity) == "" {
		return decimal.Zero, false
	}
	base, err := exchange.FromVenueQuantity(toVenueInstrument(instrument), rawQuantity)
	if err != nil {
		return decimal.Zero, false
	}
	qty := parsePositiveDecimal(base)
	if !qty.IsPositive() {
		return decimal.Zero, false
	}
	return qty.Mul(px), true
}

func bboPriceForSide(bbo marketdata.BBO, side string) string {
	if strings.EqualFold(strings.TrimSpace(side), "sell") {
		return bbo.BidPrice
	}
	return bbo.AskPrice
}

func bboQuantityForSide(bbo marketdata.BBO, side string) string {
	if strings.EqualFold(strings.TrimSpace(side), "sell") {
		return bbo.BidQuantity
	}
	return bbo.AskQuantity
}

func sizeArbitrageExecution(
	combination ArbitrageCombination,
	instrumentA, instrumentB Instrument,
	bboA, bboB marketdata.BBO,
	direction, effect string,
	remaining decimal.Decimal,
	closeable decimal.Decimal,
	rng arbitrageNotionalRand,
) (arbitrageSizePlan, string, bool) {
	aSide, bSide, ok := arbitrageLegSides(direction)
	if !ok {
		return arbitrageSizePlan{}, "invalid direction", false
	}
	priceA := parsePositiveDecimal(bboPriceForSide(bboA, aSide))
	priceB := parsePositiveDecimal(bboPriceForSide(bboB, bSide))
	if !priceA.IsPositive() || !priceB.IsPositive() {
		return arbitrageSizePlan{}, "missing bbo price", false
	}
	closing := strings.EqualFold(strings.TrimSpace(effect), "close")
	minOpen := decimal.NewFromInt(arbitrageMinOpenNotional)
	maxOrder := decimal.NewFromInt(arbitrageMaxOrderNotional)
	lastClip := arbitrageFlattening(combination) &&
		closing && closeable.IsPositive() && closeable.LessThan(minOpen)
	var raw, hedgeCap, random decimal.Decimal
	if lastClip {
		raw = closeable
	} else {
		if rng == nil {
			rng = cryptoNotionalRand{}
		}
		legATop, okA := topOfBookNotional(instrumentA, bboPriceForSide(bboA, aSide), bboQuantityForSide(bboA, aSide))
		legBTop, okB := topOfBookNotional(instrumentB, bboPriceForSide(bboB, bSide), bboQuantityForSide(bboB, bSide))
		if !okA || !okB {
			return arbitrageSizePlan{}, "missing bbo quantity", false
		}
		maker := strings.EqualFold(strings.TrimSpace(combination.ExecutionMode), "maker_then_hedge")
		random = rng.RandomNotional()
		if random.IsNegative() {
			random = decimal.Zero
		}
		maxRandom := decimal.NewFromInt(arbitrageRandomNotionalMax)
		if !random.LessThan(maxRandom) {
			random = maxRandom.Sub(decimal.RequireFromString("0.01"))
		}
		fraction := decimal.RequireFromString(arbitrageMakerDepthFraction)
		if maker {
			makerTop, hedgeTop := legATop, legBTop
			if strings.EqualFold(strings.TrimSpace(combination.MakerLeg), "b") {
				makerTop, hedgeTop = legBTop, legATop
			}
			hedgeCap = hedgeTop
			raw = makerTop.Mul(fraction).Add(random)
			if hedgeCap.LessThan(raw) {
				raw = hedgeCap
			}
		} else {
			hedgeCap = decimal.Min(legATop, legBTop)
			raw = decimal.Min(legATop, legBTop).Mul(fraction).Add(random)
			raw = decimal.Min(raw, legATop, legBTop)
		}
		if !closing && hedgeCap.LessThan(minOpen) {
			return arbitrageSizePlan{}, "hedge cap below minimum open notional", false
		}
		if closing && hedgeCap.LessThan(minOpen) {
			return arbitrageSizePlan{}, "hedge cap below minimum close notional", false
		}
		if raw.LessThan(minOpen) {
			raw = minOpen
		}
	}
	bounded := raw
	if bounded.GreaterThan(maxOrder) {
		bounded = maxOrder
	}
	if closing {
		if !closeable.IsPositive() {
			return arbitrageSizePlan{}, "closeable notional is zero", false
		}
		bounded = decimal.Min(bounded, closeable)
	} else {
		if remaining.LessThan(minOpen) {
			return arbitrageSizePlan{}, "remaining below minimum open notional", false
		}
		bounded = decimal.Min(bounded, remaining)
	}
	if !bounded.IsPositive() {
		return arbitrageSizePlan{}, "bounded notional is not positive", false
	}
	finalBase, reason, ok := arbitrageExecutableBase(
		combination, instrumentA, instrumentB, bboA, bboB, direction, effect, bounded, lastClip,
	)
	if !ok {
		if reason == "" {
			reason = "close size is unexecutable"
		}
		return arbitrageSizePlan{}, reason, false
	}
	if closing {
		closeableBase := arbitrageCloseableBase(combination)
		if arbitrageFlattening(combination) {
			closeableBase = decimal.Min(
				closeableBase,
				arbitrageOwnedCloseableBase(combination),
			)
		}
		finalBase = decimal.Min(finalBase, closeableBase)
		step := commonQuantityStep(
			instrumentOrderStep(instrumentA, arbitrageOrderType(combination, instrumentA)),
			instrumentOrderStep(instrumentB, arbitrageOrderType(combination, instrumentB)),
		)
		finalBase = floorToStep(finalBase, step)
		if !finalBase.IsPositive() {
			return arbitrageSizePlan{}, "close quantity floored to zero", false
		}
	}
	capPrice := decimal.Max(priceA, priceB)
	if !closing {
		maxBase := remaining.Div(capPrice)
		if finalBase.GreaterThan(maxBase) {
			step := commonQuantityStep(
				instrumentOrderStep(instrumentA, arbitrageOrderType(combination, instrumentA)),
				instrumentOrderStep(instrumentB, arbitrageOrderType(combination, instrumentB)),
			)
			finalBase = floorToStep(maxBase, step)
		}
	}
	if !finalBase.IsPositive() {
		return arbitrageSizePlan{}, "sized quantity is not positive", false
	}
	legANotional := finalBase.Mul(priceA)
	legBNotional := finalBase.Mul(priceB)
	requested := decimal.Max(legANotional, legBNotional)
	if !closing && (requested.GreaterThan(remaining) || remaining.LessThan(minOpen)) {
		return arbitrageSizePlan{}, "requested exceeds remaining open notional", false
	}
	if closing && requested.GreaterThan(closeable) {
		return arbitrageSizePlan{}, "requested exceeds closeable notional", false
	}
	return arbitrageSizePlan{
		Direction:          direction,
		Effect:             effect,
		ReduceOnly:         closing,
		RequestedNotional:  requested,
		TargetBaseQuantity: finalBase,
		RandomNotional:     random,
		HedgeCapNotional:   hedgeCap,
		LegANotional:       legANotional,
		LegBNotional:       legBNotional,
		LastClip:           lastClip,
	}, "", true
}

func arbitrageOrderType(combination ArbitrageCombination, instrument Instrument) string {
	orderTypeA, orderTypeB := arbitrageOrderTypes(combination)
	if instrument.ID == 0 {
		if strings.EqualFold(instrument.ExchangeSymbol, combination.LegA.ExchangeSymbol) {
			return orderTypeA
		}
		return orderTypeB
	}
	if instrument.ID == combination.LegA.InstrumentID {
		return orderTypeA
	}
	return orderTypeB
}
