package ranking

import (
	"context"
	"testing"
	"time"

	"selfquant/backend/internal/funding"
)

type fakeHistoryStore struct {
	quotes []Quote
	err    error
}

func (s *fakeHistoryStore) Query(
	context.Context,
	time.Time,
	time.Time,
	[]string,
	[]string,
) ([]Quote, error) {
	return s.quotes, s.err
}

func (s *fakeHistoryStore) Close() error { return nil }

func TestEngineRanksBestDirectionAndFundingUntilExit(t *testing.T) {
	now := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	points := syntheticPairPoints(now, 24*time.Hour, 28)
	quotes := make([]Quote, 0, len(points)*2)
	for _, point := range points {
		quotes = append(quotes, point.Long, point.Short)
	}
	nextRate := 0.0003
	rates := []funding.Rate{
		{
			Exchange: "binance", ExchangeSymbol: "BTCUSDT", GlobalSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Rate: 0.0001, IntervalHours: 8,
			FundingTime: now.Add(30 * time.Minute), PositionNotionalUSD: 5_000_000,
			Turnover24hUSD: 20_000_000, SourceUpdatedAt: now,
		},
		{
			Exchange: "okx", ExchangeSymbol: "BTC-USDT-SWAP", GlobalSymbol: "BTCUSDT",
			BaseAsset: "BTC", QuoteAsset: "USDT", Rate: 0.0002, NextRate: &nextRate,
			IntervalHours: 8, FundingTime: now.Add(30 * time.Minute),
			PositionNotionalUSD: 4_000_000, Turnover24hUSD: 30_000_000,
			SourceUpdatedAt: now,
		},
	}
	store := NewSnapshotStore()
	engine := NewEngine(&fakeHistoryStore{quotes: quotes}, store, 20*time.Minute)
	if err := engine.Refresh(context.Background(), []Period{Period1h}, rates, now); err != nil {
		t.Fatal(err)
	}
	snapshot := store.Get(Period1h)
	if len(snapshot.Items) != 1 {
		t.Fatalf("items=%d", len(snapshot.Items))
	}
	item := snapshot.Items[0]
	if item.Long.Exchange != "binance" || item.Short.Exchange != "okx" {
		t.Fatalf("direction=%s/%s", item.Long.Exchange, item.Short.Exchange)
	}
	if item.MinPositionNotionalUSD != 4_000_000 || item.Rank != 1 {
		t.Fatalf("item=%+v", item)
	}
	if item.ProfitProbability < 0 || item.ProfitProbability > 1 {
		t.Fatalf("profit probability=%f", item.ProfitProbability)
	}
}

func TestProjectedFundingRespectsSettlementBoundary(t *testing.T) {
	now := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	rate := funding.Rate{Rate: 0.001, IntervalHours: 8, FundingTime: now.Add(time.Hour)}
	if value := projectedFunding(rate, now, now.Add(30*time.Minute)); value != 0 {
		t.Fatalf("before settlement=%f", value)
	}
	if value := projectedFunding(rate, now, now.Add(2*time.Hour)); value != 0.001 {
		t.Fatalf("after settlement=%f", value)
	}
}

func TestEngineKeepsPreviousSnapshotStaleWhenBBOIsUnavailable(t *testing.T) {
	now := time.Now().UTC()
	store := NewSnapshotStore()
	store.Replace(Period1h, []Opportunity{{GlobalSymbol: "BTCUSDT"}}, now.Add(-time.Minute))
	engine := NewEngine(&fakeHistoryStore{}, store, 20*time.Minute)
	err := engine.Refresh(context.Background(), []Period{Period1h}, []funding.Rate{{
		Exchange: "binance", GlobalSymbol: "BTCUSDT", IntervalHours: 8,
	}}, now)
	if err == nil {
		t.Fatal("expected unavailable BBO error")
	}
	snapshot := store.Get(Period1h)
	if !snapshot.Stale || len(snapshot.Items) != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}
