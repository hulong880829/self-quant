package report

import (
	"fmt"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

const (
	PeriodRuleVersion = 1
	periodTimezone    = "Asia/Shanghai"
	periodHour        = 9
)

var (
	periodLocationOnce sync.Once
	periodLocation     *time.Location
)

func PeriodLocation() *time.Location {
	periodLocationOnce.Do(func() {
		location, err := time.LoadLocation(periodTimezone)
		if err != nil {
			location = time.FixedZone("CST", 8*3600)
		}
		periodLocation = location
	})
	return periodLocation
}

func ReportDate(occurredAt time.Time) string {
	local := occurredAt.In(PeriodLocation())
	cutoff := time.Date(local.Year(), local.Month(), local.Day(), periodHour, 0, 0, 0, PeriodLocation())
	if local.Before(cutoff) {
		return local.Format(time.DateOnly)
	}
	return local.AddDate(0, 0, 1).Format(time.DateOnly)
}

func PeriodBounds(reportDate string) (time.Time, time.Time, error) {
	day, err := time.ParseInLocation(time.DateOnly, reportDate, PeriodLocation())
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid report date %q", reportDate)
	}
	end := time.Date(day.Year(), day.Month(), day.Day(), periodHour, 0, 0, 0, PeriodLocation())
	return end.AddDate(0, 0, -1), end, nil
}

func DietzWeight(occurredAt, start, end time.Time) decimal.Decimal {
	duration := end.Sub(start)
	if duration <= 0 {
		return decimal.Zero
	}
	remaining := end.Sub(occurredAt)
	if remaining < 0 {
		remaining = 0
	}
	if remaining > duration {
		remaining = duration
	}
	seconds := decimal.NewFromInt(int64(duration / time.Second))
	if seconds.IsZero() {
		return decimal.Zero
	}
	return decimal.NewFromInt(int64(remaining / time.Second)).Div(seconds)
}

func DefaultIdempotencyKey(flowType string, occurredAt time.Time, amount string) string {
	return fmt.Sprintf("%s|%s|%s", flowType, occurredAt.UTC().Truncate(time.Second).Format(time.RFC3339), amount)
}
