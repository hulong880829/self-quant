package jev

import (
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/config"
)

func RequiredDepthUSD(cfg config.Jev) decimal.Decimal {
	return cfg.MinDepthUSD
}

func RoundTripCostRate(costBPS decimal.Decimal) decimal.Decimal {
	if !costBPS.IsPositive() {
		return decimal.Zero
	}
	return costBPS.Div(decimal.NewFromInt(10000))
}

func MidBasisRatio(longMid, shortMid decimal.Decimal) *decimal.Decimal {
	if !longMid.IsPositive() || !shortMid.IsPositive() {
		return nil
	}
	value := shortMid.Div(longMid).Sub(decimal.NewFromInt(1))
	return &value
}

func MidBasisBPS(long, short Quote) *decimal.Decimal {
	ratio := MidBasisRatio(long.Mid(), short.Mid())
	if ratio == nil {
		return nil
	}
	bps := ratio.Mul(decimal.NewFromInt(10000))
	return &bps
}

func BasisChangeSinceEntryBPS(current, entry *decimal.Decimal) *decimal.Decimal {
	if current == nil || entry == nil {
		return nil
	}
	value := current.Sub(*entry)
	return &value
}

func BasisContribution(raw, confidence decimal.Decimal) decimal.Decimal {
	if raw.IsPositive() {
		return raw.Mul(confidence)
	}
	return raw
}

func ApplyLocalReturns(cfg config.Jev, candidate Candidate) Candidate {
	costRate := RoundTripCostRate(cfg.PaperRoundTripCostBPS)
	rawBasis := decimal.Zero
	if candidate.ExpectedBasisReturn24h != nil {
		rawBasis = *candidate.ExpectedBasisReturn24h
	}
	confidence := decimal.Zero
	if candidate.BasisForecastConfidence != nil {
		confidence = *candidate.BasisForecastConfidence
	}
	contribution := BasisContribution(rawBasis, confidence)
	gross := candidate.FundingReturn24h.Add(contribution)
	net := gross.Sub(costRate)
	candidate.ExpectedNetReturn24h = net
	p10 := decimalOrZero(candidate.BasisReturnP10)
	candidate.ConservativeNetReturn24h = candidate.FundingReturn24h.Add(p10).Sub(costRate)
	candidate.BasisProfitProbability = decimalPtr(netProfitProbability(candidate.FundingReturn24h, costRate, candidate.MatchedBasisReturns))
	horizonHours := decimal.NewFromFloat(cfg.MaxHoldDuration.Hours())
	candidate.HoldingHorizonNetReturn = candidate.FundingReturn24h.Mul(horizonHours).
		Div(decimal.NewFromInt(24)).
		Add(contribution).
		Sub(costRate)
	if gross.IsPositive() && costRate.IsPositive() {
		minutes := decimal.NewFromInt(24 * 60).Mul(costRate).Div(gross)
		candidate.ExpectedPaybackMinutes = &minutes
	} else {
		candidate.ExpectedPaybackMinutes = nil
	}
	candidate.CurrentMidBasisBPS = MidBasisBPS(candidate.LongQuote, candidate.ShortQuote)
	return candidate
}

func EntryFilterOK(cfg config.Jev, candidate Candidate, forecastAge time.Duration) bool {
	if candidate.BasisSampleCount == nil || candidate.ExpectedBasisReturn24h == nil ||
		candidate.BasisForecastConfidence == nil || candidate.BasisForecastCalculatedAt == nil ||
		candidate.BasisProfitProbability == nil {
		return false
	}
	if forecastAge > cfg.BasisCacheMaxAge {
		return false
	}
	if !candidate.FundingReturn24h.IsPositive() {
		return false
	}
	if candidate.ExpectedNetReturn24h.LessThan(cfg.MinExpectedNetReturn24h) {
		return false
	}
	if candidate.ConservativeNetReturn24h.LessThan(cfg.MinConservativeNetReturn24h) {
		return false
	}
	if candidate.BasisProfitProbability.LessThan(cfg.MinNetProfitProbability) {
		return false
	}
	if candidate.BasisForecastConfidence.LessThan(cfg.MinBasisForecastConfidence) {
		return false
	}
	if candidate.ExpectedPaybackMinutes == nil {
		return false
	}
	maxHoldMinutes := decimal.NewFromFloat(cfg.MaxHoldDuration.Minutes())
	if candidate.ExpectedPaybackMinutes.GreaterThan(maxHoldMinutes) {
		return false
	}
	return candidate.HoldingHorizonNetReturn.IsPositive()
}

func netProfitProbability(funding, costRate decimal.Decimal, samples []decimal.Decimal) decimal.Decimal {
	if len(samples) == 0 {
		return decimal.Zero
	}
	positive := 0
	for _, item := range samples {
		if funding.Add(item).Sub(costRate).IsPositive() {
			positive++
		}
	}
	return decimal.NewFromInt(int64(positive)).Div(decimal.NewFromInt(int64(len(samples))))
}

func directionalDepthOK(long, short Quote, required decimal.Decimal) bool {
	if !long.Ask.IsPositive() || !long.AskQty.IsPositive() || !short.Bid.IsPositive() || !short.BidQty.IsPositive() {
		return false
	}
	longAskNotional := long.Ask.Mul(long.AskQty)
	shortBidNotional := short.Bid.Mul(short.BidQty)
	return !longAskNotional.LessThan(required) && !shortBidNotional.LessThan(required)
}

func fillQtyOK(long, short Quote, qty decimal.Decimal, enter bool) bool {
	if !qty.IsPositive() {
		return false
	}
	if enter {
		return !long.AskQty.LessThan(qty) && !short.BidQty.LessThan(qty)
	}
	return !long.BidQty.LessThan(qty) && !short.AskQty.LessThan(qty)
}

func decimalPtr(value decimal.Decimal) *decimal.Decimal { return &value }

func intPtr(value int) *int { return &value }

func stringPtr(value string) *string { return &value }

func timePtr(value time.Time) *time.Time { return &value }

func decimalOrZero(value *decimal.Decimal) decimal.Decimal {
	if value == nil {
		return decimal.Zero
	}
	return *value
}
