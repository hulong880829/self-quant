package report

import (
	"math"
	"strconv"
	"sync/atomic"

	"github.com/shopspring/decimal"
)

type Metrics struct {
	sampleRuns       atomic.Uint64
	sampleFailures   atomic.Uint64
	finalizeRuns     atomic.Uint64
	finalizeFailures atomic.Uint64
	snapshotsWritten atomic.Uint64
}

type MetricsSnapshot struct {
	SampleRuns       uint64
	SampleFailures   uint64
	FinalizeRuns     uint64
	FinalizeFailures uint64
	SnapshotsWritten uint64
}

func (m *Metrics) RecordSample(success bool) {
	m.sampleRuns.Add(1)
	if !success {
		m.sampleFailures.Add(1)
	}
}

func (m *Metrics) RecordFinalize(success bool, written int) {
	m.finalizeRuns.Add(1)
	if !success {
		m.finalizeFailures.Add(1)
	}
	if written > 0 {
		m.snapshotsWritten.Add(uint64(written))
	}
}

func (m *Metrics) Snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		SampleRuns:       m.sampleRuns.Load(),
		SampleFailures:   m.sampleFailures.Load(),
		FinalizeRuns:     m.finalizeRuns.Load(),
		FinalizeFailures: m.finalizeFailures.Load(),
		SnapshotsWritten: m.snapshotsWritten.Load(),
	}
}

// CalculatePerformanceMetrics enriches newest-first daily rows without
// converting persisted monetary values away from decimal strings.
func CalculatePerformanceMetrics(rows []DailySnapshot) []DailySnapshot {
	result := append([]DailySnapshot(nil), rows...)
	if len(result) == 0 {
		return result
	}
	returns := make([]float64, len(result))
	for index := range result {
		returns[index], _ = strconv.ParseFloat(result[index].ReturnRate, 64)
	}
	cumulative := decimal.Zero
	peak := decimal.Zero
	maxDrawdown := 0.0
	for index := len(result) - 1; index >= 0; index-- {
		if pnl, err := decimal.NewFromString(result[index].PnLUSD); err == nil {
			cumulative = cumulative.Add(pnl)
		}
		result[index].AbsoluteReturn = cumulative.String()
		aum, err := decimal.NewFromString(result[index].ClosingEquityUSD)
		if err == nil {
			if peak.IsZero() || aum.GreaterThan(peak) {
				peak = aum
			} else if !peak.IsZero() {
				drawdown, _ := aum.Sub(peak).Div(peak).Float64()
				if drawdown < maxDrawdown {
					maxDrawdown = drawdown
				}
			}
		}
		result[index].MaxDrawdown = metricString(maxDrawdown)
		result[index].Annualized7D = annualizedWindow(returns, index, 7)
		result[index].Annualized30D = annualizedWindow(returns, index, 30)
		result[index].AnnualizedReturn = annualizedWindow(returns, index, len(returns)-index)
		result[index].Sharpe = sharpeWindow(returns, index, len(returns)-index)
	}
	return result
}

func annualizedWindow(values []float64, start int, size int) string {
	end := min(len(values), start+size)
	if start >= end {
		return ""
	}
	total := 0.0
	count := 0
	for _, value := range values[start:end] {
		if math.IsNaN(value) || math.IsInf(value, 0) || value <= -1 {
			continue
		}
		total += math.Log1p(value)
		count++
	}
	if count == 0 {
		return ""
	}
	return metricString(math.Expm1(total / float64(count) * 365))
}

func sharpeWindow(values []float64, start int, size int) string {
	end := min(len(values), start+size)
	count := end - start
	if count < 2 {
		return ""
	}
	mean := 0.0
	for _, value := range values[start:end] {
		mean += value
	}
	mean /= float64(count)
	variance := 0.0
	for _, value := range values[start:end] {
		delta := value - mean
		variance += delta * delta
	}
	deviation := math.Sqrt(variance / float64(count-1))
	if deviation == 0 {
		return ""
	}
	return metricString(mean / deviation * math.Sqrt(365))
}

func metricString(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return ""
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}
