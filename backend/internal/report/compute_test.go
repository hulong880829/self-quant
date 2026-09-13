package report

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestComputeDailyRedemptionUsesModifiedDietz(t *testing.T) {
	location := PeriodLocation()
	start := wall(location, 2026, 8, 29, 9, 0, 0)
	end := wall(location, 2026, 8, 30, 9, 0, 0)
	opening := decimal.RequireFromString("7174.1791")
	closing := decimal.RequireFromString("5525.3778")
	flow := PeriodCashFlow{
		OccurredAt: wall(location, 2026, 8, 29, 18, 17, 0),
		Amount:     decimal.RequireFromString("-1500"),
		FlowType:   "redemption",
	}
	got := ComputeDaily(opening, closing, []PeriodCashFlow{flow}, start, end)
	if got.CashFlowCount != 1 || !got.HasReturn || got.Invalid {
		t.Fatalf("computation=%+v", got)
	}
	if !got.PnL.Equal(decimal.RequireFromString("-148.8013")) {
		t.Fatalf("pnl=%s", got.PnL)
	}
	if !got.NetCashFlow.Equal(decimal.RequireFromString("-1500")) {
		t.Fatalf("net=%s", got.NetCashFlow)
	}
	if !got.RedemptionUSD.Equal(decimal.RequireFromString("-1500")) || !got.SubscriptionUSD.IsZero() {
		t.Fatalf("subscription=%s redemption=%s", got.SubscriptionUSD, got.RedemptionUSD)
	}
	if !got.HasSimple {
		t.Fatal("simple return missing")
	}
	simplePct := got.SimpleReturn.Mul(decimal.NewFromInt(100))
	if simplePct.Abs().Sub(decimal.RequireFromString("2.07")).Abs().GreaterThan(decimal.RequireFromString("0.01")) {
		t.Fatalf("simple percent=%s", simplePct)
	}
	dietzPct := got.ReturnRate.Mul(decimal.NewFromInt(100))
	if dietzPct.Abs().Sub(decimal.RequireFromString("2.38")).Abs().GreaterThan(decimal.RequireFromString("0.01")) {
		t.Fatalf("dietz percent=%s", dietzPct)
	}
}

func TestComputeDailyIgnoresSubscriptionAsProfit(t *testing.T) {
	location := PeriodLocation()
	start := wall(location, 2026, 8, 20, 9, 0, 0)
	end := wall(location, 2026, 8, 21, 9, 0, 0)
	opening := decimal.NewFromInt(1000)
	closing := decimal.NewFromInt(1500)
	flow := PeriodCashFlow{
		OccurredAt: wall(location, 2026, 8, 20, 12, 0, 0),
		Amount:     decimal.NewFromInt(500),
		FlowType:   "subscription",
	}
	got := ComputeDaily(opening, closing, []PeriodCashFlow{flow}, start, end)
	if !got.PnL.IsZero() {
		t.Fatalf("subscription must not create pnl: %s", got.PnL)
	}
}

func TestComputeDailyMultipleFlowsAndZeroDenominator(t *testing.T) {
	location := PeriodLocation()
	start := wall(location, 2026, 8, 30, 9, 0, 0)
	end := wall(location, 2026, 8, 31, 9, 0, 0)
	flows := []PeriodCashFlow{
		{
			OccurredAt: wall(location, 2026, 8, 30, 10, 0, 0),
			Amount:     decimal.NewFromInt(100),
			FlowType:   "subscription",
		},
		{
			OccurredAt: wall(location, 2026, 8, 30, 18, 0, 0),
			Amount:     decimal.NewFromInt(-40),
			FlowType:   "redemption",
		},
	}
	got := ComputeDaily(decimal.NewFromInt(200), decimal.NewFromInt(270), flows, start, end)
	if got.CashFlowCount != 2 || !got.PnL.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("multi flow computation=%+v pnl=%s", got, got.PnL)
	}
	invalid := ComputeDaily(decimal.Zero, decimal.NewFromInt(10), nil, start, end)
	if !invalid.Invalid || invalid.HasReturn {
		t.Fatalf("zero opening without flows should be invalid: %+v", invalid)
	}
}
