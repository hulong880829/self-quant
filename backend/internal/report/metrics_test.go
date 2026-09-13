package report

import (
	"strconv"
	"testing"
)

func TestCalculatePerformanceMetrics(t *testing.T) {
	rows := []DailySnapshot{
		{PnLUSD: "10", ClosingEquityUSD: "110", ReturnRate: "0.1"},
		{PnLUSD: "-5", ClosingEquityUSD: "100", ReturnRate: "-0.05"},
		{PnLUSD: "5", ClosingEquityUSD: "105", ReturnRate: "0.05"},
	}
	result := CalculatePerformanceMetrics(rows)
	if result[0].AbsoluteReturn != "10" {
		t.Fatalf("absolute return=%s", result[0].AbsoluteReturn)
	}
	if result[0].Annualized7D != "" || result[0].AnnualizedReturn != "" || result[0].Sharpe != "" {
		t.Fatalf("sparse samples must not invent annualized metrics: %+v", result[0])
	}
	if rows[0].AbsoluteReturn != "" {
		t.Fatal("input rows were mutated")
	}
}

func TestCalculatePerformanceMetricsDrawdownUsesNAVNotAUM(t *testing.T) {
	rows := []DailySnapshot{
		{PnLUSD: "-148.8013", ClosingEquityUSD: "5525.3778", ReturnRate: "-0.0238"},
		{PnLUSD: "10", ClosingEquityUSD: "7174.1791", ReturnRate: "0.0014"},
	}
	result := CalculatePerformanceMetrics(rows)
	drawdown, err := strconv.ParseFloat(result[0].MaxDrawdown, 64)
	if err != nil {
		t.Fatal(err)
	}
	if drawdown < -0.05 {
		t.Fatalf("max drawdown=%v followed AUM cash-flow drop", drawdown)
	}
	if result[0].AbsoluteReturn != "-138.8013" {
		t.Fatalf("cumulative pnl=%s", result[0].AbsoluteReturn)
	}
}

func TestCalculatePerformanceMetricsSkipsEmptyAndRecomputingReturns(t *testing.T) {
	rows := make([]DailySnapshot, 0, 7)
	for day := 0; day < 6; day++ {
		rows = append(rows, DailySnapshot{
			PnLUSD: "1", ClosingEquityUSD: "100", ReturnRate: "0.01", Status: "final",
		})
	}
	rows = append([]DailySnapshot{{
		PnLUSD: "0", ClosingEquityUSD: "100", ReturnRate: "0.50", Status: "recomputing",
	}}, rows...)
	result := CalculatePerformanceMetrics(rows)
	if result[0].Sharpe != "" || result[0].AnnualizedReturn != "" {
		t.Fatalf("recomputing/short windows must stay empty: %+v", result[0])
	}
}

func TestCalculatePerformanceMetricsRequiresSevenValidDays(t *testing.T) {
	rows := make([]DailySnapshot, 0, 7)
	for day := 0; day < 7; day++ {
		rows = append(rows, DailySnapshot{
			PnLUSD: "1", ClosingEquityUSD: "100",
			ReturnRate: strconv.FormatFloat(0.01+float64(day)*0.001, 'f', -1, 64),
			Status:     "final",
		})
	}
	result := CalculatePerformanceMetrics(rows)
	if result[0].Annualized7D == "" || result[0].AnnualizedReturn == "" || result[0].Sharpe == "" {
		t.Fatalf("seven valid days should populate metrics: %+v", result[0])
	}
	if result[6].Annualized7D != "" {
		t.Fatalf("oldest row does not have a trailing 7-day window: %+v", result[6])
	}
}
