package main

import (
	"strings"
	"testing"
	"time"

	"selfquant/backend/internal/funding"
)

func TestFormatIBNegativeCacheExpiryOmitsActiveRows(t *testing.T) {
	previous := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	next := previous.Add(-time.Second)
	text := formatIBNegativeCacheExpiry([]funding.IBNegativeCacheExpiry{{
		Symbol: "AAPL", Status: "not_found", PreviousNextVerify: previous, NextVerify: next,
	}, {
		Symbol: "MSFT", Status: "ambiguous", PreviousNextVerify: previous, NextVerify: next,
	}})
	if !strings.Contains(text, "expired 2 ib negative resolutions") {
		t.Fatalf("text=%s", text)
	}
	if strings.Contains(text, "status=active") {
		t.Fatalf("active row leaked: %s", text)
	}
	if !strings.Contains(text, "symbol=AAPL status=not_found") || !strings.Contains(text, "symbol=MSFT status=ambiguous") {
		t.Fatalf("text=%s", text)
	}
}
