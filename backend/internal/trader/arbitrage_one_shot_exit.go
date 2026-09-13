package trader

import (
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/marketdata"
)

const (
	oneShotExitSampleInterval = time.Second
	oneShotExitWindowSize     = 30
	oneShotExitMinHits        = 18
	oneShotExitBBOStale       = 2 * time.Second
	oneShotExitBasisMaxAge    = 15 * time.Minute
)

type oneShotExitBasis struct {
	CombinationID      string
	Version            int64
	ExitAnnualizedRate decimal.Decimal
	OwnedA             decimal.Decimal
	OwnedB             decimal.Decimal
	AvgA               decimal.Decimal
	AvgB               decimal.Decimal
	ComboA             decimal.Decimal
	ComboB             decimal.Decimal
	BaselineA          decimal.Decimal
	BaselineB          decimal.Decimal
	BaselineCapturedAt time.Time
	Realized           decimal.Decimal
	Funding            decimal.Decimal
	IncurredFee        decimal.Decimal
	Exposure           decimal.Decimal
	Holding            decimal.Decimal
	ExposureUpdatedAt  time.Time
	ThresholdHours     float64
	CloseFeeRateA      decimal.Decimal
	CloseFeeRateB      decimal.Decimal
	ExecutionMode      string
	MakerLeg           string
	LegA               ArbitrageLeg
	LegB               ArbitrageLeg
	CalculatedAt       time.Time
}

type oneShotExitState struct {
	basis      oneShotExitBasis
	hits       []bool
	lastSample time.Time
	stale      bool
}

type oneShotExitQuote struct {
	BidA, AskA, BidB, AskB decimal.Decimal
	ReceivedA              time.Time
	ReceivedB              time.Time
}

type oneShotExitSampleResult struct {
	hit   bool
	fault bool
}

func oneShotAnnualizedWaitingExit(item ArbitrageCombination) bool {
	return strings.EqualFold(strings.TrimSpace(item.Status), "running") &&
		strings.EqualFold(strings.TrimSpace(item.RunMode), "one_shot") &&
		item.OneShotPhase == "waiting_exit" &&
		item.ExitPolicy == "annualized" &&
		!item.PositionUncertain
}

func closeFeeRole(executionMode, makerLeg, thisLeg string) string {
	if strings.EqualFold(strings.TrimSpace(executionMode), "simultaneous_market") {
		return "taker"
	}
	if strings.EqualFold(strings.TrimSpace(thisLeg), strings.TrimSpace(makerLeg)) {
		return "maker"
	}
	return "taker"
}

func closeFeeCostRate(
	leg ArbitrageLeg,
	executionMode, makerLeg, thisLeg string,
	accounts map[int64]accountFeeSnapshot,
) (decimal.Decimal, bool) {
	snapshot, ok := accounts[leg.TradingAccountID]
	if !ok {
		return decimal.Zero, false
	}
	stored, ok := accountFeeRate(
		snapshot, closeFeeRole(executionMode, makerLeg, thisLeg), leg.ContractType,
	)
	if !ok {
		return decimal.Zero, false
	}
	return tradingFeeCostRate(leg.Exchange, stored), true
}

func ownedPairedQuantity(ownedA, ownedB decimal.Decimal) decimal.Decimal {
	if ownedA.IsZero() || ownedB.IsZero() || ownedA.Sign() == ownedB.Sign() {
		return decimal.Zero
	}
	return decimal.Min(ownedA.Abs(), ownedB.Abs())
}

func oneShotExitQuantities(
	basis oneShotExitBasis,
) (qtyA, qtyB, ownedQ, venueQ decimal.Decimal, ok bool) {
	combo := ArbitrageCombination{
		LegABasePosition:              basis.ComboA.String(),
		LegBBasePosition:              basis.ComboB.String(),
		LegAVenueBaselineBasePosition: basis.BaselineA.String(),
		LegBVenueBaselineBasePosition: basis.BaselineB.String(),
		VenueBaselineCapturedAt:       basis.BaselineCapturedAt,
	}
	venueQ = arbitrageCloseableBase(combo)
	ownedQ = ownedPairedQuantity(basis.OwnedA, basis.OwnedB)
	comboOwnedQ := ownedPairedQuantity(basis.ComboA, basis.ComboB)
	costDirection, costOK := arbitragePairedCloseDirection(basis.OwnedA, basis.OwnedB)
	comboDirection, comboOK := arbitragePairedCloseDirection(basis.ComboA, basis.ComboB)
	venueDirection, venueOK := arbitrageVenueReduceDirection(combo)
	if !ownedQ.IsPositive() || !comboOwnedQ.IsPositive() || !venueQ.IsPositive() ||
		!costOK || !comboOK || !venueOK ||
		costDirection != comboDirection || comboDirection != venueDirection {
		return decimal.Zero, decimal.Zero, ownedQ, venueQ, false
	}
	q := decimal.Min(ownedQ, comboOwnedQ, venueQ)
	switch comboDirection {
	case "bid":
		return q, q.Neg(), ownedQ, venueQ, true
	case "ask":
		return q.Neg(), q, ownedQ, venueQ, true
	default:
		return decimal.Zero, decimal.Zero, ownedQ, venueQ, false
	}
}

func executableClosePrice(qty, bid, ask decimal.Decimal) (decimal.Decimal, bool) {
	if qty.IsZero() {
		return decimal.Zero, true
	}
	if qty.IsPositive() {
		if !bid.IsPositive() {
			return decimal.Zero, false
		}
		return bid, true
	}
	if !ask.IsPositive() {
		return decimal.Zero, false
	}
	return ask, true
}

func quoteStale(received, now time.Time) bool {
	return received.IsZero() || now.Sub(received) > oneShotExitBBOStale
}

func appendExitHit(hits []bool, hit bool) []bool {
	hits = append(append([]bool{}, hits...), hit)
	if len(hits) > oneShotExitWindowSize {
		hits = hits[len(hits)-oneShotExitWindowSize:]
	}
	return hits
}

func oneShotExitWindowReady(hits []bool) bool {
	if len(hits) < oneShotExitWindowSize {
		return false
	}
	matched := 0
	for _, hit := range hits {
		if hit {
			matched++
		}
	}
	return matched >= oneShotExitMinHits && hits[len(hits)-1]
}

func evaluateOneShotExitSample(
	basis oneShotExitBasis,
	quote oneShotExitQuote,
	now time.Time,
) oneShotExitSampleResult {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if quoteStale(quote.ReceivedA, now) || quoteStale(quote.ReceivedB, now) {
		return oneShotExitSampleResult{fault: true}
	}
	qtyA, qtyB, _, _, ok := oneShotExitQuantities(basis)
	if !ok || !basis.AvgA.IsPositive() || !basis.AvgB.IsPositive() {
		return oneShotExitSampleResult{fault: true}
	}
	pxA, okA := executableClosePrice(qtyA, quote.BidA, quote.AskA)
	pxB, okB := executableClosePrice(qtyB, quote.BidB, quote.AskB)
	if !okA || !okB || !pxA.IsPositive() || !pxB.IsPositive() {
		return oneShotExitSampleResult{fault: true}
	}
	elapsed := decimal.Zero
	if !basis.ExposureUpdatedAt.IsZero() && now.After(basis.ExposureUpdatedAt) {
		elapsed = decimal.NewFromFloat(now.Sub(basis.ExposureUpdatedAt).Seconds())
	}
	q := qtyA.Abs()
	currentPairedNotional := q.Mul(basis.AvgA.Add(basis.AvgB)).Div(decimal.NewFromInt(2))
	holding := basis.Holding.Add(elapsed)
	exposure := basis.Exposure.Add(currentPairedNotional.Mul(elapsed))
	closePnl := qtyA.Mul(pxA.Sub(basis.AvgA)).Add(qtyB.Mul(pxB.Sub(basis.AvgB)))
	closeFee := qtyA.Abs().Mul(pxA).Mul(basis.CloseFeeRateA).
		Add(qtyB.Abs().Mul(pxB).Mul(basis.CloseFeeRateB))
	exitProfit := basis.Realized.Add(basis.Funding).Sub(basis.IncurredFee).
		Add(closePnl).Sub(closeFee)
	annualized, ok := calculateCombinedAnnualized(
		exitProfit, exposure, holding, basis.ThresholdHours,
	)
	if !ok {
		return oneShotExitSampleResult{fault: true}
	}
	return oneShotExitSampleResult{
		hit: annualized.GreaterThanOrEqual(basis.ExitAnnualizedRate) &&
			basis.ExitAnnualizedRate.IsPositive(),
	}
}

func oneShotExitQuoteFromBBO(
	bboA, bboB marketdata.BBO,
	now time.Time,
) (oneShotExitQuote, bool) {
	bidA, errBidA := decimal.NewFromString(bboA.BidPrice)
	askA, errAskA := decimal.NewFromString(bboA.AskPrice)
	bidB, errBidB := decimal.NewFromString(bboB.BidPrice)
	askB, errAskB := decimal.NewFromString(bboB.AskPrice)
	if errBidA != nil || errAskA != nil || errBidB != nil || errAskB != nil {
		return oneShotExitQuote{}, false
	}
	if !bidA.IsPositive() || !askA.IsPositive() || !bidB.IsPositive() || !askB.IsPositive() {
		return oneShotExitQuote{}, false
	}
	if quoteStale(bboA.ReceiveTimestamp, now) || quoteStale(bboB.ReceiveTimestamp, now) {
		return oneShotExitQuote{}, false
	}
	return oneShotExitQuote{
		BidA: bidA, AskA: askA, BidB: bidB, AskB: askB,
		ReceivedA: bboA.ReceiveTimestamp, ReceivedB: bboB.ReceiveTimestamp,
	}, true
}

func buildOneShotExitBasis(
	updated ArbitrageCombination,
	ownedA, ownedB arbitrageLegCost,
	realized, funding, incurredFee, exposure, holding decimal.Decimal,
	thresholdHours float64,
	accounts map[int64]accountFeeSnapshot,
	now time.Time,
) (*oneShotExitBasis, bool) {
	if !oneShotAnnualizedWaitingExit(updated) {
		return nil, false
	}
	exitRate := parseDecimal(updated.ExitAnnualizedRate)
	if !exitRate.IsPositive() || !ownedA.average.IsPositive() || !ownedB.average.IsPositive() {
		return nil, false
	}
	rateA, okA := closeFeeCostRate(
		updated.LegA, updated.ExecutionMode, updated.MakerLeg, "a", accounts,
	)
	rateB, okB := closeFeeCostRate(
		updated.LegB, updated.ExecutionMode, updated.MakerLeg, "b", accounts,
	)
	if !okA || !okB {
		return nil, false
	}
	capturedAt := updated.VenueBaselineCapturedAt
	if capturedAt.Equal(time.Unix(0, 0).UTC()) {
		capturedAt = time.Time{}
	}
	return &oneShotExitBasis{
		CombinationID:      updated.ID,
		Version:            updated.Version,
		ExitAnnualizedRate: exitRate,
		OwnedA:             ownedA.position,
		OwnedB:             ownedB.position,
		AvgA:               ownedA.average,
		AvgB:               ownedB.average,
		ComboA:             parseDecimal(updated.LegABasePosition),
		ComboB:             parseDecimal(updated.LegBBasePosition),
		BaselineA:          parseDecimal(updated.LegAVenueBaselineBasePosition),
		BaselineB:          parseDecimal(updated.LegBVenueBaselineBasePosition),
		BaselineCapturedAt: capturedAt,
		Realized:           realized,
		Funding:            funding,
		IncurredFee:        incurredFee,
		Exposure:           exposure,
		Holding:            holding,
		ExposureUpdatedAt:  now,
		ThresholdHours:     thresholdHours,
		CloseFeeRateA:      rateA,
		CloseFeeRateB:      rateB,
		ExecutionMode:      updated.ExecutionMode,
		MakerLeg:           updated.MakerLeg,
		LegA:               updated.LegA,
		LegB:               updated.LegB,
		CalculatedAt:       now,
	}, true
}
