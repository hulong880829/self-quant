package report

import (
	"math"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/shopspring/decimal"
)

const (
	minSharpeSamples     = 7
	minAnnualizedSamples = 7
	minAnnualized7DDays  = 7
	minAnnualized30DDays = 30
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
	returns := make([]parsedReturn, len(result))
	for index := range result {
		if result[index].Status == "recomputing" || result[index].Status == "invalid" {
			continue
		}
		returns[index] = parseReturnRate(result[index].ReturnRate)
	}
	cumulative := decimal.Zero
	nav := 1.0
	peak := 1.0
	maxDrawdown := 0.0
	for index := len(result) - 1; index >= 0; index-- {
		if pnl, err := decimal.NewFromString(result[index].PnLUSD); err == nil {
			cumulative = cumulative.Add(pnl)
		}
		result[index].AbsoluteReturn = cumulative.String()
		if parsed := returns[index]; parsed.ok {
			nav *= 1 + parsed.value
			if nav > peak {
				peak = nav
			} else if peak > 0 {
				drawdown := (nav - peak) / peak
				if drawdown < maxDrawdown {
					maxDrawdown = drawdown
				}
			}
		}
		result[index].MaxDrawdown = metricString(maxDrawdown)
		result[index].Annualized7D = annualizedWindow(returns, index, minAnnualized7DDays, minAnnualized7DDays)
		result[index].Annualized30D = annualizedWindow(returns, index, minAnnualized30DDays, minAnnualized30DDays)
		result[index].AnnualizedReturn = annualizedWindow(returns, index, len(returns)-index, minAnnualizedSamples)
		result[index].Sharpe = sharpeWindow(returns, index, len(returns)-index, minSharpeSamples)
	}
	return result
}

type parsedReturn struct {
	value float64
	ok    bool
}

func parseReturnRate(value string) parsedReturn {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return parsedReturn{}
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= -1 {
		return parsedReturn{}
	}
	return parsedReturn{value: parsed, ok: true}
}

func annualizedWindow(values []parsedReturn, start int, size int, minValid int) string {
	end := min(len(values), start+size)
	if start >= end || end-start < minValid {
		return ""
	}
	total := 0.0
	count := 0
	for _, value := range values[start:end] {
		if !value.ok {
			continue
		}
		total += math.Log1p(value.value)
		count++
	}
	if count < minValid {
		return ""
	}
	return metricString(math.Expm1(total / float64(count) * 365))
}

func sharpeWindow(values []parsedReturn, start int, size int, minValid int) string {
	end := min(len(values), start+size)
	if start >= end {
		return ""
	}
	valid := make([]float64, 0, end-start)
	for _, value := range values[start:end] {
		if value.ok {
			valid = append(valid, value.value)
		}
	}
	if len(valid) < minValid {
		return ""
	}
	mean := 0.0
	for _, value := range valid {
		mean += value
	}
	mean /= float64(len(valid))
	variance := 0.0
	for _, value := range valid {
		delta := value - mean
		variance += delta * delta
	}
	deviation := math.Sqrt(variance / float64(len(valid)-1))
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
