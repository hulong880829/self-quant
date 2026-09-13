package report

import (
	"time"

	"github.com/shopspring/decimal"
)

type PeriodCashFlow struct {
	OccurredAt time.Time
	Amount     decimal.Decimal
	FlowType   string
}

type DailyComputation struct {
	NetCashFlow     decimal.Decimal
	SubscriptionUSD decimal.Decimal
	RedemptionUSD   decimal.Decimal
	CashFlowCount   int
	PnL             decimal.Decimal
	ReturnRate      decimal.Decimal
	SimpleReturn    decimal.Decimal
	HasReturn       bool
	HasSimple       bool
	Invalid         bool
}

func ComputeDaily(
	opening decimal.Decimal,
	closing decimal.Decimal,
	flows []PeriodCashFlow,
	start time.Time,
	end time.Time,
) DailyComputation {
	result := DailyComputation{}
	weighted := decimal.Zero
	for _, flow := range flows {
		result.CashFlowCount++
		result.NetCashFlow = result.NetCashFlow.Add(flow.Amount)
		switch flow.FlowType {
		case "subscription", "deposit":
			result.SubscriptionUSD = result.SubscriptionUSD.Add(flow.Amount)
		case "redemption", "withdrawal":
			result.RedemptionUSD = result.RedemptionUSD.Add(flow.Amount)
		}
		weighted = weighted.Add(flow.Amount.Mul(DietzWeight(flow.OccurredAt, start, end)))
	}
	result.PnL = closing.Sub(opening).Sub(result.NetCashFlow)
	if !opening.IsZero() {
		result.SimpleReturn = result.PnL.Div(opening)
		result.HasSimple = true
	}
	denominator := opening.Add(weighted)
	if !denominator.IsPositive() {
		result.Invalid = true
		return result
	}
	result.ReturnRate = result.PnL.Div(denominator)
	result.HasReturn = true
	return result
}
