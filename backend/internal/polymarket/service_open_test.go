package polymarket

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestFilterLivePositions(t *testing.T) {
	positions := []Position{
		{ID: "live", CurrentPrice: "0.47"},
		{ID: "redeemable", CurrentPrice: "1", Redeemable: true},
		{ID: "zero", CurrentPrice: "0"},
		{ID: "invalid", CurrentPrice: "not-a-price"},
	}
	filtered := filterLivePositions(positions)
	if len(filtered) != 1 || filtered[0].ID != "live" {
		t.Fatalf("filtered=%+v", filtered)
	}
}

func TestApplyGammaOpenPriceSetsSnapshotOpen(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	market := Market{
		ID: "m1", Asset: "BTC", Period: "1h", Active: true,
		WindowStart: now.Add(-time.Minute), WindowEnd: now.Add(time.Hour),
		GammaOpenPrice: "64967.36",
	}
	store.ReplaceMarkets([]Market{market})

	service := &Service{
		snapshots:      store,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		openPersisted:  make(map[string]bool),
		lastPersisted:  make(map[string]time.Time),
		openCandidates: make(map[string]chainlinkOpenCandidate),
	}
	service.applyGammaOpenPrice(context.Background(), market)

	snapshot, ok := store.Get(market.ID)
	if !ok {
		t.Fatal("snapshot missing")
	}
	if snapshot.OpenPrice != "64967.36" {
		t.Fatalf("open=%q", snapshot.OpenPrice)
	}
	if snapshot.ChainlinkPrice != "" {
		t.Fatalf("gamma open used as chainlink=%q", snapshot.ChainlinkPrice)
	}
}

func TestApplyGammaOpenPriceDoesNotOverwriteExistingOpen(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	market := Market{
		ID: "m2", Asset: "BTC", Period: "4h", Active: true,
		WindowStart: now.Add(-time.Minute), WindowEnd: now.Add(4 * time.Hour),
		GammaOpenPrice: "65000",
	}
	store.ReplaceMarkets([]Market{market})
	store.Update(Snapshot{
		Market: market, OpenPrice: "64900", ChainlinkPrice: "64950",
	})

	service := &Service{
		snapshots:      store,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		openPersisted:  make(map[string]bool),
		lastPersisted:  make(map[string]time.Time),
		openCandidates: make(map[string]chainlinkOpenCandidate),
	}
	service.applyGammaOpenPrice(context.Background(), market)

	snapshot, _ := store.Get(market.ID)
	if snapshot.OpenPrice != "64900" {
		t.Fatalf("open overwritten=%q", snapshot.OpenPrice)
	}
	if snapshot.ChainlinkPrice != "64950" {
		t.Fatalf("chainlink overwritten=%q", snapshot.ChainlinkPrice)
	}
}

func TestListMarketsActiveOnlyFiltersExpiredWindow(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	store.ReplaceMarkets([]Market{
		{
			ID: "live", Asset: "BTC", Period: "5m", Active: true,
			WindowStart: now.Add(-2 * time.Minute), WindowEnd: now.Add(3 * time.Minute),
		},
		{
			ID: "expired", Asset: "BTC", Period: "5m", Active: true,
			WindowStart: now.Add(-10 * time.Minute), WindowEnd: now.Add(-time.Minute),
		},
	})
	markets, _ := store.ListMarkets("BTC", "5m", true)
	if len(markets) != 1 || markets[0].ID != "live" {
		t.Fatalf("markets=%+v", markets)
	}
}

func newOpenTestService(store *SnapshotStore) *Service {
	return &Service{
		snapshots:      store,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		openPersisted:  make(map[string]bool),
		lastPersisted:  make(map[string]time.Time),
		openCandidates: make(map[string]chainlinkOpenCandidate),
	}
}

func TestRecordChainlinkOpenUsesClosestBoundaryTick(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	windowStart := now.Add(-30 * time.Second)
	market := Market{
		ID: "m5m", Asset: "BTC", Period: "5m", Active: true,
		WindowStart: windowStart, WindowEnd: now.Add(4*time.Minute + 30*time.Second),
	}
	store.ReplaceMarkets([]Market{market})
	service := newOpenTestService(store)
	ctx := context.Background()

	service.RecordChainlinkPrice(ctx, "BTC", "100.00", windowStart.Add(3*time.Second))
	service.RecordChainlinkPrice(ctx, "BTC", "101.00", windowStart.Add(500*time.Millisecond))
	service.RecordChainlinkPrice(ctx, "BTC", "102.00", windowStart.Add(4*time.Second))

	snapshot, _ := store.Get(market.ID)
	if snapshot.OpenPrice != "101.00" {
		t.Fatalf("open=%q want closest boundary tick", snapshot.OpenPrice)
	}

	service.RecordChainlinkPrice(ctx, "BTC", "200.00", windowStart.Add(10*time.Second))
	snapshot, _ = store.Get(market.ID)
	if snapshot.OpenPrice != "101.00" {
		t.Fatalf("open changed after lock window=%q", snapshot.OpenPrice)
	}
}

func TestOpenCandidateRestoresAfterSnapshotWipe(t *testing.T) {
	store := NewSnapshotStore()
	now := time.Now().UTC()
	windowStart := now.Add(-10 * time.Second)
	market := Market{
		ID: "m-restore", Asset: "BTC", Period: "5m", Active: true,
		WindowStart: windowStart, WindowEnd: now.Add(4*time.Minute + 50*time.Second),
	}
	store.ReplaceMarkets([]Market{market})
	service := newOpenTestService(store)
	ctx := context.Background()

	service.RecordChainlinkPrice(ctx, "BTC", "101.00", windowStart.Add(500*time.Millisecond))
	service.priceMu.Lock()
	service.openCandidates[market.ID] = chainlinkOpenCandidate{
		distance: 500 * time.Millisecond, price: "101.00",
		observedAt: windowStart.Add(500 * time.Millisecond),
	}
	service.priceMu.Unlock()

	store.Update(Snapshot{Market: market, ChainlinkPrice: "102.00"})

	service.RecordChainlinkPrice(ctx, "BTC", "103.00", windowStart.Add(3*time.Second))

	snapshot, _ := store.Get(market.ID)
	if snapshot.OpenPrice != "101.00" {
		t.Fatalf("open=%q want restored candidate", snapshot.OpenPrice)
	}
}

func TestForgetMarketPriceState(t *testing.T) {
	service := newOpenTestService(NewSnapshotStore())
	marketID := "market-forget"
	service.openPersisted[marketID] = true
	service.lastPersisted[marketID] = time.Now().UTC()
	service.openCandidates[marketID] = chainlinkOpenCandidate{
		distance: time.Second, price: "100", observedAt: time.Now().UTC(),
	}

	service.forgetMarketPriceState([]string{marketID})

	if service.openPersisted[marketID] {
		t.Fatal("openPersisted not cleared")
	}
	if _, ok := service.lastPersisted[marketID]; ok {
		t.Fatal("lastPersisted not cleared")
	}
	if _, ok := service.openCandidates[marketID]; ok {
		t.Fatal("openCandidates not cleared")
	}
}
