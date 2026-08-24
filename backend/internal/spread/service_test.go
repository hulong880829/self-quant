package spread

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type fakeStore struct {
	calls   atomic.Int32
	delay   time.Duration
	history History
	err     error
}

func (s *fakeStore) QueryHistory(ctx context.Context, _ HistoryRequest) (History, error) {
	s.calls.Add(1)
	if s.delay > 0 {
		select {
		case <-ctx.Done():
			return History{}, ErrTimeout
		case <-time.After(s.delay):
		}
	}
	if s.err != nil {
		return History{}, s.err
	}
	return s.history, nil
}

func (s *fakeStore) Ping(context.Context) error { return nil }
func (s *fakeStore) Close() error               { return nil }

func TestServiceCachesAndMergesInflight(t *testing.T) {
	store := &fakeStore{
		delay: 20 * time.Millisecond,
		history: History{
			Venue: "binance", BaseAsset: "BTC", QuoteAsset: "USDT",
			CanonicalSymbol: "BTCUSDT", Range: Range1h, Availability: AvailabilityAvailable,
			AsOf: time.Date(2026, 8, 22, 7, 0, 0, 0, time.UTC),
		},
	}
	service := NewService(store, time.Minute, 2)
	ctx := context.Background()
	request := HistoryRequest{Venue: "binance", BaseAsset: "BTC", QuoteAsset: "USDT", Range: Range1h}
	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := service.GetHistory(ctx, request)
			errCh <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	if store.calls.Load() != 1 {
		t.Fatalf("calls=%d", store.calls.Load())
	}
	if _, err := service.GetHistory(ctx, request); err != nil {
		t.Fatal(err)
	}
	if store.calls.Load() != 1 {
		t.Fatalf("cache miss calls=%d", store.calls.Load())
	}
}

func TestServiceRejectsInvalidVenue(t *testing.T) {
	service := NewService(&fakeStore{}, time.Minute, 1)
	if _, err := service.GetHistory(context.Background(), HistoryRequest{
		Venue: "binance/okx", BaseAsset: "BTC", QuoteAsset: "USDT", Range: Range1h,
	}); err == nil {
		t.Fatal("cross-venue request was accepted")
	}
}

func TestServiceCacheKeyIncludesCompareVenue(t *testing.T) {
	store := &fakeStore{history: History{Venue: "binance", CompareVenue: "okx"}}
	service := NewService(store, time.Minute, 1)
	ctx := context.Background()
	if _, err := service.GetHistory(ctx, HistoryRequest{
		Venue: "binance", BaseAsset: "BTC", QuoteAsset: "USDT", Range: Range1h,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetHistory(ctx, HistoryRequest{
		Venue: "binance", CompareVenue: "okx", BaseAsset: "BTC", QuoteAsset: "USDT", Range: Range1h,
	}); err != nil {
		t.Fatal(err)
	}
	if store.calls.Load() != 2 {
		t.Fatalf("calls=%d", store.calls.Load())
	}
}
