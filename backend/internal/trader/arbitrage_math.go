package trader

import (
	"math/big"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

var tenThousand = decimal.NewFromInt(10_000)

func calculateArbitrageSpreads(
	legABid, legAAsk, legBBid, legBAsk decimal.Decimal,
) (askSpread, bidSpread decimal.Decimal, ok bool) {
	if !legABid.IsPositive() || !legAAsk.IsPositive() ||
		!legBBid.IsPositive() || !legBAsk.IsPositive() {
		return decimal.Zero, decimal.Zero, false
	}
	askSpread = legBAsk.Div(legAAsk).Sub(decimal.NewFromInt(1)).Mul(tenThousand)
	bidSpread = legBBid.Div(legABid).Sub(decimal.NewFromInt(1)).Mul(tenThousand)
	return askSpread, bidSpread, true
}

func executableArbitrageSpreads(
	combination ArbitrageCombination,
	legABid, legAAsk, legBBid, legBAsk decimal.Decimal,
) (open, close decimal.Decimal, ok bool) {
	if strings.EqualFold(strings.TrimSpace(combination.ExecutionMode), "simultaneous_market") {
		if !legABid.IsPositive() || !legAAsk.IsPositive() ||
			!legBBid.IsPositive() || !legBAsk.IsPositive() {
			return decimal.Zero, decimal.Zero, false
		}
		open = legBBid.Div(legAAsk).Sub(decimal.NewFromInt(1)).Mul(tenThousand)
		close = legBAsk.Div(legABid).Sub(decimal.NewFromInt(1)).Mul(tenThousand)
		return open, close, true
	}
	quoteAsk, quoteBid, ok := calculateArbitrageSpreads(legABid, legAAsk, legBBid, legBAsk)
	if !ok {
		return decimal.Zero, decimal.Zero, false
	}
	if strings.EqualFold(strings.TrimSpace(combination.MakerLeg), "a") {
		return quoteBid, quoteAsk, true
	}
	return quoteAsk, quoteBid, true
}

func arbitrageTriggered(
	askSpread, bidSpread, askThreshold, bidThreshold decimal.Decimal,
) string {
	if askSpread.GreaterThanOrEqual(askThreshold) {
		return "ask"
	}
	if bidSpread.LessThanOrEqual(bidThreshold) {
		return "bid"
	}
	return ""
}

func selectArbitragePositionDirection(
	openSpread, closeSpread, openThreshold, closeThreshold, position, target decimal.Decimal,
) string {
	if position.LessThan(target) && openSpread.GreaterThanOrEqual(openThreshold) {
		return "ask"
	}
	if closeSpread.LessThanOrEqual(closeThreshold) {
		return "bid"
	}
	return ""
}

func arbitragePositionOverTarget(notional, target decimal.Decimal) bool {
	return target.IsPositive() && notional.Abs().GreaterThanOrEqual(target)
}

type arbitragePositionPlan struct {
	Direction         string
	Effect            string
	ReduceOnly        bool
	RequestedNotional decimal.Decimal
}

func planArbitragePosition(
	venueNotional, target, order decimal.Decimal,
	direction string,
) (arbitragePositionPlan, bool) {
	if !target.IsPositive() || !order.IsPositive() {
		return arbitragePositionPlan{}, false
	}
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case "ask":
		size := decimal.Min(order, target.Sub(venueNotional))
		if !size.IsPositive() {
			return arbitragePositionPlan{}, false
		}
		return arbitragePositionPlan{
			Direction: "ask", Effect: "open", ReduceOnly: false, RequestedNotional: size,
		}, true
	case "bid":
		if !venueNotional.IsPositive() {
			return arbitragePositionPlan{}, false
		}
		size := decimal.Min(order, venueNotional)
		if !size.IsPositive() {
			return arbitragePositionPlan{}, false
		}
		return arbitragePositionPlan{
			Direction: "bid", Effect: "close", ReduceOnly: true, RequestedNotional: size,
		}, true
	default:
		return arbitragePositionPlan{}, false
	}
}

func arbitrageCloseableBase(combination ArbitrageCombination) decimal.Decimal {
	baselineA, baselineB := decimal.Zero, decimal.Zero
	if !combination.VenueBaselineCapturedAt.IsZero() &&
		!combination.VenueBaselineCapturedAt.Equal(time.Unix(0, 0).UTC()) {
		baselineA = parseDecimal(combination.LegAVenueBaselineBasePosition)
		baselineB = parseDecimal(combination.LegBVenueBaselineBasePosition)
	}
	legAAvailable := baselineA.Add(parseDecimal(combination.LegABasePosition))
	legBAvailable := baselineB.Add(parseDecimal(combination.LegBBasePosition)).Neg()
	switch {
	case legAAvailable.IsPositive() && legBAvailable.IsPositive():
		return decimal.Min(legAAvailable, legBAvailable)
	case legAAvailable.IsNegative() && legBAvailable.IsNegative():
		return decimal.Min(legAAvailable.Abs(), legBAvailable.Abs())
	default:
		return decimal.Zero
	}
}

func arbitrageOwnedCloseableBase(combination ArbitrageCombination) decimal.Decimal {
	return ownedPairedQuantity(
		parseDecimal(combination.LegABasePosition),
		parseDecimal(combination.LegBBasePosition),
	)
}

func arbitrageOwnedCloseDirection(
	combination ArbitrageCombination,
) (string, bool) {
	return arbitragePairedCloseDirection(
		parseDecimal(combination.LegABasePosition),
		parseDecimal(combination.LegBBasePosition),
	)
}

func arbitrageVenueReduceDirection(
	combination ArbitrageCombination,
) (string, bool) {
	baselineA, baselineB := decimal.Zero, decimal.Zero
	if !combination.VenueBaselineCapturedAt.IsZero() &&
		!combination.VenueBaselineCapturedAt.Equal(time.Unix(0, 0).UTC()) {
		baselineA = parseDecimal(combination.LegAVenueBaselineBasePosition)
		baselineB = parseDecimal(combination.LegBVenueBaselineBasePosition)
	}
	return arbitragePairedCloseDirection(
		baselineA.Add(parseDecimal(combination.LegABasePosition)),
		baselineB.Add(parseDecimal(combination.LegBBasePosition)),
	)
}

func arbitragePairedCloseDirection(legA, legB decimal.Decimal) (string, bool) {
	switch {
	case legA.IsPositive() && legB.IsNegative():
		return "bid", true
	case legA.IsNegative() && legB.IsPositive():
		return "ask", true
	default:
		return "", false
	}
}

func arbitrageFlattening(combination ArbitrageCombination) bool {
	return strings.EqualFold(strings.TrimSpace(combination.Status), "closing") ||
		(strings.EqualFold(strings.TrimSpace(combination.Status), "running") &&
			strings.EqualFold(strings.TrimSpace(combination.RunMode), "one_shot") &&
			strings.EqualFold(strings.TrimSpace(combination.OneShotPhase), "exiting"))
}

func instrumentOrderStep(instrument Instrument, orderType string) decimal.Decimal {
	if strings.EqualFold(strings.TrimSpace(orderType), "market") {
		return ruleDecimal(
			instrument.MarketQuantityStep,
			instrument.MarketQuantityStepStatus,
		)
	}
	return parsePositiveDecimal(instrument.QuantityStep)
}

func commonQuantityStep(a, b decimal.Decimal) decimal.Decimal {
	if !a.IsPositive() || !b.IsPositive() {
		return decimal.Zero
	}
	exponent := a.Exponent()
	if b.Exponent() < exponent {
		exponent = b.Exponent()
	}
	scaled := func(value decimal.Decimal) *big.Int {
		result := new(big.Int).Set(value.Coefficient())
		if shift := int64(value.Exponent() - exponent); shift > 0 {
			result.Mul(result, new(big.Int).Exp(big.NewInt(10), big.NewInt(shift), nil))
		}
		return result.Abs(result)
	}
	aInt, bInt := scaled(a), scaled(b)
	gcd := new(big.Int).GCD(nil, nil, aInt, bInt)
	if gcd.Sign() == 0 {
		return decimal.Zero
	}
	lcm := new(big.Int).Mul(aInt, bInt)
	lcm.Div(lcm, gcd)
	return decimal.NewFromBigInt(lcm, exponent)
}

func arbitrageOrderTypes(combination ArbitrageCombination) (string, string) {
	if combination.ExecutionMode == "simultaneous_market" {
		orderTypeA, orderTypeB := "market", "market"
		if combination.LegA.ContractType == "spot" {
			orderTypeA = "limit"
		}
		if combination.LegB.ContractType == "spot" {
			orderTypeB = "limit"
		}
		return orderTypeA, orderTypeB
	}
	return "limit", "limit"
}

func arbitrageExecutableBase(
	combination ArbitrageCombination,
	instrumentA, instrumentB Instrument,
	bboA, bboB marketdata.BBO,
	direction, effect string,
	notional decimal.Decimal,
	lastClip bool,
) (decimal.Decimal, string, bool) {
	aSide, bSide, ok := arbitrageLegSides(direction)
	if !ok || !notional.IsPositive() {
		return decimal.Zero, "invalid close size", false
	}
	priceA := parsePositiveDecimal(bboA.AskPrice)
	if aSide == "sell" {
		priceA = parsePositiveDecimal(bboA.BidPrice)
	}
	priceB := parsePositiveDecimal(bboB.AskPrice)
	if bSide == "sell" {
		priceB = parsePositiveDecimal(bboB.BidPrice)
	}
	if !priceA.IsPositive() || !priceB.IsPositive() {
		return decimal.Zero, "missing bbo price", false
	}
	orderTypeA, orderTypeB := arbitrageOrderTypes(combination)
	step := commonQuantityStep(
		instrumentOrderStep(instrumentA, orderTypeA),
		instrumentOrderStep(instrumentB, orderTypeB),
	)
	if !step.IsPositive() {
		return decimal.Zero, "quantity step is invalid", false
	}
	quantity := floorToStep(
		decimal.Min(notional.Div(priceA), notional.Div(priceB)),
		step,
	)
	if strings.EqualFold(direction, "bid") {
		quantity = floorToStep(
			decimal.Min(quantity, arbitrageCloseableBase(combination)),
			step,
		)
	}
	if !quantity.IsPositive() {
		return decimal.Zero, "quantity floored to zero", false
	}
	closing := strings.EqualFold(strings.TrimSpace(effect), "close")
	if err := prepareSizedOrder(
		instrumentA, orderTypeA, quantity.String(),
		preflightPrice(instrumentA, orderTypeA, priceA), priceA.String(), effect, closing, lastClip,
	); err != nil {
		return decimal.Zero, err.Error(), false
	}
	if err := prepareSizedOrder(
		instrumentB, orderTypeB, quantity.String(),
		preflightPrice(instrumentB, orderTypeB, priceB), priceB.String(), effect, closing, lastClip,
	); err != nil {
		return decimal.Zero, err.Error(), false
	}
	if _, err := exchange.ToVenueQuantity(toVenueInstrument(instrumentA), quantity.String()); err != nil {
		return decimal.Zero, err.Error(), false
	}
	if _, err := exchange.ToVenueQuantity(toVenueInstrument(instrumentB), quantity.String()); err != nil {
		return decimal.Zero, err.Error(), false
	}
	return quantity, "", true
}

func prepareSizedOrder(
	instrument Instrument,
	orderType, quantity, price, referencePrice, effect string,
	reduceOnly, lastClip bool,
) error {
	_, _, err := prepareArbitrageOrder(
		instrument, orderType, quantity, price, referencePrice,
		ArbitrageExecution{
			PositionEffect: effect, ReduceOnly: reduceOnly, LastCloseClip: lastClip,
		},
	)
	return err
}

func preflightPrice(
	instrument Instrument,
	orderType string,
	reference decimal.Decimal,
) string {
	if orderType != "limit" {
		return ""
	}
	tick := parsePositiveDecimal(instrument.PriceTick)
	if !tick.IsPositive() {
		return ""
	}
	return floorToStep(reference, tick).String()
}

func arbitragePositionNotionals(
	combination ArbitrageCombination,
	legAMid, legBMid decimal.Decimal,
) (venueNotional, comboNotional decimal.Decimal, baselineCaptured bool) {
	if !legAMid.IsPositive() || !legBMid.IsPositive() {
		return decimal.Zero, decimal.Zero, false
	}
	baselineCaptured = !combination.VenueBaselineCapturedAt.IsZero() &&
		!combination.VenueBaselineCapturedAt.Equal(time.Unix(0, 0).UTC())
	baselineA, baselineB := decimal.Zero, decimal.Zero
	if baselineCaptured {
		baselineA = parseDecimal(combination.LegAVenueBaselineBasePosition)
		baselineB = parseDecimal(combination.LegBVenueBaselineBasePosition)
	}
	comboA := parseDecimal(combination.LegABasePosition)
	comboB := parseDecimal(combination.LegBBasePosition)
	venueNotional = pairedAskNotional(
		baselineA.Add(comboA).Mul(legAMid),
		baselineB.Add(comboB).Neg().Mul(legBMid),
	)
	comboNotional = pairedAskNotional(
		comboA.Mul(legAMid),
		comboB.Neg().Mul(legBMid),
	)
	return venueNotional, comboNotional, baselineCaptured
}

func pairedAskNotional(legA, legB decimal.Decimal) decimal.Decimal {
	switch {
	case legA.IsPositive() && legB.IsPositive():
		return decimal.Min(legA, legB)
	case legA.IsNegative() && legB.IsNegative():
		return decimal.Min(legA.Abs(), legB.Abs()).Neg()
	default:
		return decimal.Zero
	}
}

func capSpotReduceQuantity(
	quantity, comboBase decimal.Decimal,
	side string,
	step decimal.Decimal,
) decimal.Decimal {
	available := decimal.Zero
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "sell":
		if comboBase.IsPositive() {
			available = comboBase
		}
	case "buy":
		if comboBase.IsNegative() {
			available = comboBase.Abs()
		}
	}
	if !available.IsPositive() {
		return decimal.Zero
	}
	return floorToStep(decimal.Min(quantity, available), step)
}

func signedPositionDelta(direction string, fillA, fillB, mark decimal.Decimal) decimal.Decimal {
	completed := decimal.Min(fillA, fillB).Mul(mark)
	if !completed.IsPositive() {
		return decimal.Zero
	}
	if strings.EqualFold(strings.TrimSpace(direction), "bid") {
		return completed.Neg()
	}
	return completed
}

func executionNotional(_ ArbitrageCombination, execution ArbitrageExecution) decimal.Decimal {
	return parsePositiveDecimal(execution.RequestedNotional)
}

func executionHasExposure(execution ArbitrageExecution) bool {
	if parseDecimal(execution.LegAFilledQuantity).IsPositive() ||
		parseDecimal(execution.LegBFilledQuantity).IsPositive() {
		return true
	}
	switch execution.Status {
	case "maker_open", "maker_canceling":
		return execution.MakerOrderID != ""
	case "hedging":
		return execution.MakerOrderID != "" || execution.HedgeOrderID != ""
	case "reconciling":
		return execution.MakerOrderID != "" || execution.HedgeOrderID != ""
	default:
		return false
	}
}

func arbitrageBackoff(failures int) decimal.Decimal {
	if failures <= 0 {
		return decimal.NewFromInt(2)
	}
	seconds := 2 << (failures - 1)
	if seconds > 60 {
		seconds = 60
	}
	return decimal.NewFromInt(int64(seconds))
}

func arbitrageLegSides(direction string) (legA, legB string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case "ask":
		return "buy", "sell", true
	case "bid":
		return "sell", "buy", true
	default:
		return "", "", false
	}
}

func baseQuantityForNotional(notional, price, step decimal.Decimal) decimal.Decimal {
	if !notional.IsPositive() || !price.IsPositive() {
		return decimal.Zero
	}
	return floorToStep(notional.Div(price), step)
}

func deltaNotional(
	legABaseFilled, legBBaseFilled, markPrice decimal.Decimal,
) decimal.Decimal {
	if !markPrice.IsPositive() {
		return decimal.Zero
	}
	return legABaseFilled.Sub(legBBaseFilled).Abs().Mul(markPrice)
}

func opportunityStillValid(
	combination ArbitrageCombination,
	legA, legB marketdata.BBO,
	direction string,
) bool {
	if strings.EqualFold(strings.TrimSpace(combination.RunMode), "one_shot") {
		return true
	}
	open, close, ok := executableArbitrageSpreads(
		combination,
		parsePositiveDecimal(legA.BidPrice),
		parsePositiveDecimal(legA.AskPrice),
		parsePositiveDecimal(legB.BidPrice),
		parsePositiveDecimal(legB.AskPrice),
	)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case "ask":
		return open.GreaterThanOrEqual(parseDecimal(combination.AskThresholdBps))
	case "bid":
		return close.LessThanOrEqual(parseDecimal(combination.BidThresholdBps))
	default:
		return false
	}
}

func makerQuantityWithinCarryBudget(
	requested, carry decimal.Decimal,
	side string,
	roundBaseCap, step decimal.Decimal,
) decimal.Decimal {
	requested = floorToStep(decimal.Min(requested, roundBaseCap), step)
	if !requested.IsPositive() || carry.IsZero() {
		return requested
	}
	makerSign := decimal.NewFromInt(1)
	if strings.EqualFold(strings.TrimSpace(side), "sell") {
		makerSign = makerSign.Neg()
	}
	if carry.Sign() != makerSign.Sign() {
		return requested
	}
	headroom := roundBaseCap.Sub(carry.Abs())
	if !headroom.IsPositive() {
		return decimal.Zero
	}
	return floorToStep(decimal.Min(requested, headroom), step)
}

func makerPositionSign(side string) int {
	if strings.EqualFold(strings.TrimSpace(side), "sell") {
		return -1
	}
	return 1
}

func carrySameSignAsMaker(carry decimal.Decimal, makerSide string) bool {
	return !carry.IsZero() && carry.Sign() == makerPositionSign(makerSide)
}

func expectedCarryAfterMaker(carry, makerQty decimal.Decimal, makerSide string) decimal.Decimal {
	if strings.EqualFold(strings.TrimSpace(makerSide), "sell") {
		return carry.Sub(makerQty)
	}
	return carry.Add(makerQty)
}

func carryHedgeSide(carry decimal.Decimal) string {
	if carry.IsNegative() {
		return "buy"
	}
	return "sell"
}

const arbitrageUnpairedLegsManualReason = "combination-owned legs are not a paired opposite position"

type closeFlattenTinyOwnedInput struct {
	Combination    ArbitrageCombination
	InstrumentA    Instrument
	InstrumentB    Instrument
	BBOA           marketdata.BBO
	BBOB           marketdata.BBO
	Now            time.Time
	LatestOrderAt  time.Time
	RequireOrders  bool
	SignalBBOStale time.Duration
}

func closeFlattenQuantityStep(instrument Instrument) decimal.Decimal {
	step := instrumentOrderStep(instrument, "market")
	if !step.IsPositive() {
		step = parsePositiveDecimal(instrument.QuantityStep)
	}
	return step
}

func positionDifferenceWithinMarketStep(difference, step decimal.Decimal) bool {
	if !step.IsPositive() {
		return false
	}
	return floorToStep(difference.Abs(), step).IsZero()
}

func closeFlattenTinyOwnedContext(in closeFlattenTinyOwnedInput) bool {
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !strings.EqualFold(strings.TrimSpace(in.Combination.Status), "closing") {
		return false
	}
	if in.Combination.PositionUncertain {
		return false
	}
	if !closeFlattenBothPerpetual(in.Combination.LegA.ContractType, in.Combination.LegB.ContractType) {
		return false
	}
	if !circuitOpenBaselineComplete(in.Combination) {
		return false
	}
	if !positionDifferenceWithinMarketStep(
		parseDecimal(in.Combination.LegAPositionDifference),
		closeFlattenQuantityStep(in.InstrumentA),
	) || !positionDifferenceWithinMarketStep(
		parseDecimal(in.Combination.LegBPositionDifference),
		closeFlattenQuantityStep(in.InstrumentB),
	) {
		return false
	}
	return closeFlattenSnapshotFresh(in.Combination.LastPositionReconciledAt, now)
}

func closeFlattenOwnedExecutablePrice(owned decimal.Decimal, bbo marketdata.BBO) decimal.Decimal {
	if owned.IsZero() {
		return decimal.Zero
	}
	return parsePositiveDecimal(bboPriceForSide(bbo, closeFlattenSide(owned)))
}

func closeFlattenOwnedLegValues(
	combination ArbitrageCombination,
	legName string,
) (owned, venue, baseline decimal.Decimal) {
	if strings.EqualFold(strings.TrimSpace(legName), "b") {
		return parseDecimal(combination.LegBBasePosition),
			parseDecimal(combination.LegBVenueBasePosition),
			parseDecimal(combination.LegBVenueBaselineBasePosition)
	}
	return parseDecimal(combination.LegABasePosition),
		parseDecimal(combination.LegAVenueBasePosition),
		parseDecimal(combination.LegAVenueBaselineBasePosition)
}

func closeFlattenOwnedVenueReducible(combination ArbitrageCombination, legName string) bool {
	owned, venue, baseline := closeFlattenOwnedLegValues(combination, legName)
	if owned.IsZero() {
		return true
	}
	delta := venue.Sub(baseline)
	if delta.Sign() != owned.Sign() {
		return false
	}
	return delta.Abs().GreaterThanOrEqual(owned.Abs())
}

func closeFlattenOwnedPlaceBase(
	combination ArbitrageCombination,
	instrument Instrument,
	legName string,
) decimal.Decimal {
	owned, venue, baseline := closeFlattenOwnedLegValues(combination, legName)
	if owned.IsZero() {
		return decimal.Zero
	}
	unsigned := decimal.Min(owned.Abs(), venue.Sub(baseline).Abs())
	stepped := closeFlattenQuantity(instrument, unsigned)
	if !stepped.IsPositive() {
		return decimal.Zero
	}
	if owned.IsNegative() {
		return stepped.Neg()
	}
	return stepped
}

func closeFlattenOwnedBBOUnusable(
	owned decimal.Decimal,
	bbo marketdata.BBO,
	now time.Time,
	stale time.Duration,
) bool {
	if owned.IsZero() {
		return false
	}
	if bbo.Stale(now, stale) {
		return true
	}
	return !closeFlattenOwnedExecutablePrice(owned, bbo).IsPositive()
}

func closeFlattenTinyOwnedWaitingSnapshot(in closeFlattenTinyOwnedInput) bool {
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !closeFlattenTinyOwnedContext(in) {
		return false
	}
	if in.RequireOrders && !in.LatestOrderAt.IsZero() &&
		in.Combination.LastPositionReconciledAt.Before(in.LatestOrderAt) {
		return true
	}
	legA := parseDecimal(in.Combination.LegABasePosition)
	legB := parseDecimal(in.Combination.LegBBasePosition)
	return closeFlattenOwnedBBOUnusable(legA, in.BBOA, now, in.SignalBBOStale) ||
		closeFlattenOwnedBBOUnusable(legB, in.BBOB, now, in.SignalBBOStale)
}

func closeFlattenTinyOwnedPositions(in closeFlattenTinyOwnedInput) bool {
	if !closeFlattenTinyOwnedContext(in) {
		return false
	}
	if closeFlattenTinyOwnedWaitingSnapshot(in) {
		return false
	}
	legA := parseDecimal(in.Combination.LegABasePosition)
	legB := parseDecimal(in.Combination.LegBBasePosition)
	if legA.IsZero() && legB.IsZero() {
		return false
	}
	if !closeFlattenOwnedVenueReducible(in.Combination, "a") ||
		!closeFlattenOwnedVenueReducible(in.Combination, "b") {
		return false
	}
	threshold := decimal.NewFromInt(arbitrageMinOpenNotional)
	if !legA.IsZero() {
		price := closeFlattenOwnedExecutablePrice(legA, in.BBOA)
		if !legA.Abs().Mul(price).LessThan(threshold) {
			return false
		}
	}
	if !legB.IsZero() {
		price := closeFlattenOwnedExecutablePrice(legB, in.BBOB)
		if !legB.Abs().Mul(price).LessThan(threshold) {
			return false
		}
	}
	return true
}

func latestOrderUpdatedAt(orders []Order) time.Time {
	var latest time.Time
	for _, order := range orders {
		if order.UpdatedAt.After(latest) {
			latest = order.UpdatedAt
		}
	}
	return latest
}

func circuitOpenNotionalWithinTarget(comboNotional, target decimal.Decimal) bool {
	if !target.IsPositive() {
		return false
	}
	return comboNotional.Abs().LessThanOrEqual(target)
}

func closeFlattenBothPerpetual(contractA, contractB string) bool {
	return strings.EqualFold(strings.TrimSpace(contractA), "perpetual") &&
		strings.EqualFold(strings.TrimSpace(contractB), "perpetual")
}

func closeFlattenSnapshotFresh(at, now time.Time) bool {
	if at.IsZero() || at.Equal(time.Unix(0, 0).UTC()) {
		return false
	}
	age := now.Sub(at)
	return age >= 0 && age <= 2*time.Minute
}

func closeFlattenSide(base decimal.Decimal) string {
	if base.IsNegative() {
		return "buy"
	}
	return "sell"
}

func closeFlattenQuantity(instrument Instrument, base decimal.Decimal) decimal.Decimal {
	return floorToStep(base.Abs(), closeFlattenQuantityStep(instrument))
}

func circuitOpenBaselineComplete(combination ArbitrageCombination) bool {
	return !combination.VenueBaselineCapturedAt.IsZero() &&
		!combination.VenueBaselineCapturedAt.Equal(time.Unix(0, 0).UTC())
}

func circuitOpenCarryDust(
	carry decimal.Decimal,
	instrumentA, instrumentB Instrument,
	markA, markB decimal.Decimal,
) bool {
	if carry.IsZero() {
		return true
	}
	target := carry.Abs()
	_, eligibleA, errA := executableHedgeQuantity(target, markA, instrumentA, false)
	_, eligibleB, errB := executableHedgeQuantity(target, markB, instrumentB, false)
	if errA != nil || errB != nil {
		return false
	}
	return !eligibleA && !eligibleB
}

func circuitOpenLegsNotSameDirection(localA, localB decimal.Decimal) bool {
	if localA.IsZero() || localB.IsZero() {
		return true
	}
	return localA.Sign() != localB.Sign()
}

func circuitOpenOrderMatchesVenue(order Order, result exchange.Result) bool {
	if !terminalStatus(order.Status) || !terminalStatus(result.Status) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(order.Status), strings.TrimSpace(result.Status)) {
		return false
	}
	return parseDecimal(order.FilledQuantity).Equal(parseDecimal(result.FilledQuantity))
}

func signedFilledBase(order Order) decimal.Decimal {
	filled := parseDecimal(order.FilledQuantity)
	if !filled.IsPositive() {
		return decimal.Zero
	}
	if strings.EqualFold(strings.TrimSpace(order.Side), "sell") {
		return filled.Neg()
	}
	return filled
}
