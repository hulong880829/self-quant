package report

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestReportDateShanghaiNineAMBoundary(t *testing.T) {
	location := PeriodLocation()
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{
			name: "before cutoff stays on calendar day",
			at:   wall(location, 2026, 8, 29, 8, 59, 59),
			want: "2026-08-29",
		},
		{
			name: "exactly 09:00 belongs to next report date",
			at:   wall(location, 2026, 8, 29, 9, 0, 0),
			want: "2026-08-30",
		},
		{
			name: "evening redemption belongs to next report date",
			at:   wall(location, 2026, 8, 29, 18, 17, 0),
			want: "2026-08-30",
		},
		{
			name: "utc evening still maps through shanghai",
			at:   time.Date(2026, 8, 29, 10, 17, 0, 0, time.UTC),
			want: "2026-08-30",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := ReportDate(test.at); got != test.want {
				t.Fatalf("ReportDate(%s)=%s want %s", test.at.Format(time.RFC3339), got, test.want)
			}
		})
	}
}

func TestPeriodBoundsAreHalfOpenShanghaiWindows(t *testing.T) {
	start, end, err := PeriodBounds("2026-08-30")
	if err != nil {
		t.Fatal(err)
	}
	location := PeriodLocation()
	wantStart := wall(location, 2026, 8, 29, 9, 0, 0)
	wantEnd := wall(location, 2026, 8, 30, 9, 0, 0)
	if !start.Equal(wantStart) || !end.Equal(wantEnd) {
		t.Fatalf("bounds=%s %s", start, end)
	}
	at := wall(location, 2026, 8, 29, 18, 17, 0)
	if at.Before(start) || !at.Before(end) {
		t.Fatal("example redemption must sit inside the 08-30 window")
	}
}

func TestDietzWeightUsesRemainingSeconds(t *testing.T) {
	location := PeriodLocation()
	start := wall(location, 2026, 8, 29, 9, 0, 0)
	end := wall(location, 2026, 8, 30, 9, 0, 0)
	at := wall(location, 2026, 8, 29, 18, 17, 0)
	got := DietzWeight(at, start, end)
	want := decimal.NewFromInt(52980).Div(decimal.NewFromInt(86400))
	if !got.Equal(want) {
		t.Fatalf("weight=%s want %s", got, want)
	}
	if !DietzWeight(start, start, end).Equal(decimal.NewFromInt(1)) {
		t.Fatal("flow at period start should have weight 1")
	}
	if !DietzWeight(end, start, end).IsZero() {
		t.Fatal("flow at period end should have weight 0")
	}
}

func wall(location *time.Location, year int, month time.Month, day, hour, minute, second int) time.Time {
	return time.Date(year, month, day, hour, minute, second, 0, location)
}
