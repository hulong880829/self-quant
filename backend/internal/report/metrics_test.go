package report

import "testing"

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
	if result[1].MaxDrawdown == "" || result[0].Annualized7D == "" {
		t.Fatalf("metrics were not populated: %+v", result)
	}
	if rows[0].AbsoluteReturn != "" {
		t.Fatal("input rows were mutated")
	}
}
