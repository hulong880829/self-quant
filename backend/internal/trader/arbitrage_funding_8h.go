package trader

import (
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const (
	funding8hWindow           = 8 * time.Hour
	funding8hGapSlack         = 5 * time.Minute
	funding8hStaleSlack       = 20 * time.Minute
	funding8hExitInterval     = time.Minute
	funding8hExitQueryTimeout = 5 * time.Second
)

type funding8hExitRow struct {
	CombinationID  string
	Version        int64
	EntryDirection string
	Floor          string
	Leg            string
	ContractType   string
	FundingTime    *time.Time
	FundingRate    *string
	IntervalHours  *string
}

type funding8hPoint struct {
	At            time.Time
	Rate          decimal.Decimal
	IntervalHours decimal.Decimal
}

type funding8hEvalResult struct {
	OK       bool
	Trigger  bool
	Observed decimal.Decimal
	CumA     decimal.Decimal
	CumB     decimal.Decimal
}

func funding8hWindows(now time.Time) (time.Time, time.Time) {
	end := now.UTC()
	return end.Add(-funding8hWindow), end
}

func groupFunding8hExitRows(rows []funding8hExitRow) (
	combos map[string]*funding8hCombo,
	duplicates map[string]struct{},
) {
	combos = make(map[string]*funding8hCombo)
	duplicates = make(map[string]struct{})
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		combo := combos[row.CombinationID]
		if combo == nil {
			combo = &funding8hCombo{
				ID:             row.CombinationID,
				Version:        row.Version,
				EntryDirection: row.EntryDirection,
				Floor:          row.Floor,
				LegAType:       "",
				LegBType:       "",
			}
			combos[row.CombinationID] = combo
		}
		switch strings.ToLower(strings.TrimSpace(row.Leg)) {
		case "a":
			combo.LegAType = row.ContractType
		case "b":
			combo.LegBType = row.ContractType
		}
		if row.FundingTime == nil || row.FundingRate == nil || row.IntervalHours == nil {
			continue
		}
		key := row.CombinationID + "\x00" + strings.ToLower(row.Leg) + "\x00" + row.FundingTime.UTC().Format(time.RFC3339Nano)
		if _, exists := seen[key]; exists {
			duplicates[row.CombinationID] = struct{}{}
			continue
		}
		seen[key] = struct{}{}
		rate, rateErr := decimal.NewFromString(strings.TrimSpace(*row.FundingRate))
		interval, intervalErr := decimal.NewFromString(strings.TrimSpace(*row.IntervalHours))
		if rateErr != nil || intervalErr != nil {
			combo.Invalid = true
			continue
		}
		point := funding8hPoint{At: row.FundingTime.UTC(), Rate: rate, IntervalHours: interval}
		switch strings.ToLower(strings.TrimSpace(row.Leg)) {
		case "a":
			combo.LegA = append(combo.LegA, point)
		case "b":
			combo.LegB = append(combo.LegB, point)
		}
	}
	return combos, duplicates
}

type funding8hCombo struct {
	ID             string
	Version        int64
	EntryDirection string
	Floor          string
	LegAType       string
	LegBType       string
	LegA           []funding8hPoint
	LegB           []funding8hPoint
	Invalid        bool
}

func evaluateFunding8hExit(
	combo funding8hCombo,
	windowStart, windowEnd time.Time,
) funding8hEvalResult {
	if combo.Invalid {
		return funding8hEvalResult{}
	}
	floor, err := decimal.NewFromString(strings.TrimSpace(combo.Floor))
	if err != nil {
		return funding8hEvalResult{}
	}
	cumA, okA := funding8hLegCumulative(combo.LegAType, combo.LegA, windowStart, windowEnd)
	cumB, okB := funding8hLegCumulative(combo.LegBType, combo.LegB, windowStart, windowEnd)
	if !okA || !okB {
		return funding8hEvalResult{}
	}
	var net decimal.Decimal
	switch strings.ToLower(strings.TrimSpace(combo.EntryDirection)) {
	case "ask":
		net = cumB.Sub(cumA)
	case "bid":
		net = cumA.Sub(cumB)
	default:
		return funding8hEvalResult{}
	}
	observed := net.Mul(decimal.NewFromInt(3)).Mul(decimal.NewFromInt(365))
	return funding8hEvalResult{
		OK: true, Trigger: observed.LessThan(floor),
		Observed: observed, CumA: cumA, CumB: cumB,
	}
}

func funding8hLegCumulative(
	contractType string,
	points []funding8hPoint,
	windowStart, windowEnd time.Time,
) (decimal.Decimal, bool) {
	if strings.EqualFold(strings.TrimSpace(contractType), "spot") {
		return decimal.Zero, true
	}
	if !strings.EqualFold(strings.TrimSpace(contractType), "perpetual") {
		return decimal.Zero, false
	}
	var anchor *funding8hPoint
	window := make([]funding8hPoint, 0, len(points))
	for _, point := range points {
		if point.IntervalHours.LessThanOrEqual(decimal.Zero) {
			return decimal.Zero, false
		}
		at := point.At.UTC()
		if !at.After(windowStart) {
			copied := point
			if anchor == nil || at.After(anchor.At) {
				anchor = &copied
			}
			continue
		}
		if at.After(windowEnd) {
			continue
		}
		window = append(window, point)
	}
	if anchor == nil || len(window) == 0 {
		return decimal.Zero, false
	}
	ordered := append([]funding8hPoint{*anchor}, window...)
	sortFunding8hPoints(ordered)
	for i := 1; i < len(ordered); i++ {
		prev := ordered[i-1]
		curr := ordered[i]
		limit := prev.At.Add(intervalHoursDuration(prev.IntervalHours)).Add(funding8hGapSlack)
		if curr.At.After(limit) {
			return decimal.Zero, false
		}
	}
	last := ordered[len(ordered)-1]
	if windowEnd.Sub(last.At) > intervalHoursDuration(last.IntervalHours)+funding8hStaleSlack {
		return decimal.Zero, false
	}
	sum := decimal.Zero
	for _, point := range window {
		sum = sum.Add(point.Rate)
	}
	return sum, true
}

func sortFunding8hPoints(points []funding8hPoint) {
	sort.Slice(points, func(i, j int) bool {
		return points[i].At.Before(points[j].At)
	})
}

func intervalHoursDuration(hours decimal.Decimal) time.Duration {
	seconds := hours.Mul(decimal.NewFromInt(3600))
	return time.Duration(seconds.IntPart()) * time.Second
}
