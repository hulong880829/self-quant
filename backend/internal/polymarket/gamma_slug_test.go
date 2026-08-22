package polymarket

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestHourlySlugCandidates(t *testing.T) {
	windowStart := time.Date(2026, time.August, 8, 7, 0, 0, 0, easternLocation)
	got := hourlySlugCandidates("BTC", windowStart)
	want := "bitcoin-up-or-down-august-8-2026-7am-et"
	if !slices.Contains(got, want) {
		t.Fatalf("candidates=%v, want %q", got, want)
	}
}

func TestHourlyWindowStartUsesEasternTime(t *testing.T) {
	now := time.Date(2026, time.August, 8, 11, 45, 0, 0, time.UTC)
	got := hourlyWindowStart(now, 0)
	if got.Location() != easternLocation || got.Hour() != 7 || got.Minute() != 0 {
		t.Fatalf("window start=%v", got)
	}
}

func TestParsePeriodFromHumanHourlySlug(t *testing.T) {
	slug := "bitcoin-up-or-down-august-8-2026-7am-et"
	if got := parsePeriodFromSlug(slug); got != "1h" {
		t.Fatalf("period=%q", got)
	}
}

func TestParsePeriodChineseHourly(t *testing.T) {
	if got := parsePeriod("BTC每小时上涨或下跌"); got != "1h" {
		t.Fatalf("period=%q", got)
	}
}

func TestIntervalWindowFromSlug1h(t *testing.T) {
	start, end, ok := intervalWindowFromSlug("btc-updown-1h-1786186800", "1h")
	if !ok {
		t.Fatal("expected 1h slug window")
	}
	if end.Sub(start) != time.Hour {
		t.Fatalf("duration=%s", end.Sub(start))
	}
}

func TestParsePeriodDoesNotTreat4hAs1h(t *testing.T) {
	if got := parsePeriodFromSlug("btc-updown-4h-1786176000"); got != "4h" {
		t.Fatalf("period=%q", got)
	}
}

func TestNormalizeHourlyGammaMarketUsesEventStartTime(t *testing.T) {
	source := gammaMarket{
		ID:             "3378327",
		ConditionID:    "0xb629",
		Slug:           "bitcoin-up-or-down-august-8-2026-7am-et",
		Question:       "Bitcoin Up or Down - August 8, 7AM ET",
		StartDate:      "2026-08-06T11:00:15Z",
		EventStartTime: "2026-08-08T11:00:00Z",
		EndDate:        "2026-08-08T12:00:00Z",
		Outcomes:       json.RawMessage(`"[\"Up\",\"Down\"]"`),
		ClobTokenIDs:   json.RawMessage(`["11","22"]`),
		Active:         true,
	}
	market, ok := normalizeGammaMarket(source)
	if !ok {
		t.Fatal("expected normalized hourly market")
	}
	if market.Asset != "BTC" || market.Period != "1h" {
		t.Fatalf("market=%+v", market)
	}
	if market.WindowEnd.Sub(market.WindowStart) != time.Hour {
		t.Fatalf("window=%s", market.WindowEnd.Sub(market.WindowStart))
	}
	if got := market.WindowStart.Format(time.RFC3339); got != "2026-08-08T11:00:00Z" {
		t.Fatalf("window start=%s", got)
	}
}
