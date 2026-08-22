package funding

import (
	"context"
	"errors"
	"testing"
	"time"

	"selfquant/backend/internal/exchange"
)

func TestNormalizeHistoryIntervals(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	rates := []exchange.FundingRate{
		{FundingTime: base.Add(12 * time.Hour)},
		{FundingTime: base},
		{FundingTime: base.Add(4 * time.Hour)},
	}
	normalizeHistoryIntervals(rates, 8)
	if rates[0].IntervalHours != 4 ||
		rates[1].IntervalHours != 8 ||
		rates[2].IntervalHours != 8 {
		t.Fatalf("intervals=%v,%v,%v",
			rates[0].IntervalHours, rates[1].IntervalHours, rates[2].IntervalHours)
	}
}

func TestWaitContextCanBeCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := waitContext(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancelled wait did not return promptly")
	}
}

func TestInstrumentRefreshMustSucceedAndContainInstruments(t *testing.T) {
	instruments := []exchange.Instrument{{ExchangeSymbol: "BTCUSDT"}}
	if !authoritativeInstrumentRefresh(instruments, nil) {
		t.Fatal("successful non-empty catalog was rejected")
	}
	if authoritativeInstrumentRefresh(nil, nil) {
		t.Fatal("empty catalog must not trigger deactivation")
	}
	if authoritativeInstrumentRefresh(instruments, errors.New("exchange unavailable")) {
		t.Fatal("failed exchange request must not trigger deactivation")
	}
}

func TestHistorySinceBackfillsPartialWindow(t *testing.T) {
	now := time.Date(2026, time.August, 7, 0, 0, 0, 0, time.UTC)
	interval := 8 * time.Hour
	watermark := now.Add(-interval)
	partialStart := now.Add(-8 * 24 * time.Hour)
	if got := historySince(now, watermark, partialStart, interval, true); !got.Equal(now.AddDate(-1, 0, 0)) {
		t.Fatalf("partial bootstrap starts at %v", got)
	}
	fullStart := now.AddDate(-1, 0, 0)
	if got := historySince(now, watermark, fullStart, interval, true); !got.Equal(watermark.Add(-2 * interval)) {
		t.Fatalf("full bootstrap starts at %v", got)
	}
	if got := historySince(now, watermark, partialStart, interval, false); !got.Equal(watermark.Add(-2 * interval)) {
		t.Fatalf("incremental sync starts at %v", got)
	}
}
