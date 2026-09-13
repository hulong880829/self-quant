package account

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"selfquant/backend/internal/account/portfolio"
)

func TestFeeIsStaleUsesShanghaiCutoff(t *testing.T) {
	loc, err := time.LoadLocation(shanghaiLocationName)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, loc)
	fresh := time.Date(2026, 8, 29, 8, 0, 0, 0, loc)
	stale := time.Date(2026, 8, 28, 7, 0, 0, 0, loc)
	if feeIsStale(fresh, now, loc) {
		t.Fatal("recent success should not be stale")
	}
	if !feeIsStale(stale, now, loc) {
		t.Fatal("success older than 30h before today's 08:00 should be stale")
	}
}

func TestMergeFeeRatesKeepsPreviousSuccessOnFailure(t *testing.T) {
	maker, taker := "0.001", "0.002"
	existing := FeeRatesRecord{
		SpotStatus: portfolio.FeeStatusOK, SpotMaker: &maker, SpotTaker: &taker,
		ContractStatus: portfolio.FeeStatusUnknown,
		SyncStatus:     FeeSyncOK,
	}
	merged := mergeFeeRates(
		TradingAccountRecord{ID: 7, Exchange: "binance"},
		existing,
		portfolio.AccountFeeRates{
			Spot:     portfolio.MarketFee{Status: portfolio.FeeStatusUnknown},
			Contract: portfolio.MarketFee{Status: portfolio.FeeStatusOK, Maker: "0.0002", Taker: "0.0004"},
			Source:   "venue",
		},
		errors.New("upstream status 400: bad"),
		time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC),
	)
	if derefFee(merged.SpotMaker) != "0.001" || merged.SpotStatus != portfolio.FeeStatusOK {
		t.Fatalf("spot overwritten: %+v", merged)
	}
	if derefFee(merged.ContractMaker) != "0.0002" || merged.SyncStatus != FeeSyncFailed {
		t.Fatalf("contract/status=%+v", merged)
	}
}

func TestSanitizeFeeErrorStripsSecrets(t *testing.T) {
	got := sanitizeFeeError(errors.New("signature=abcd api-secret=xyz failed"))
	if strings.Contains(got, "abcd") || strings.Contains(strings.ToLower(got), "secret") && strings.Contains(got, "xyz") {
		t.Fatalf("leaked %q", got)
	}
}

func TestFeeCacheLoadAllAndGet(t *testing.T) {
	loc, err := time.LoadLocation(shanghaiLocationName)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, loc)
	updated := time.Date(2026, 8, 30, 8, 5, 0, 0, loc)
	cache := newFeeCache(func() time.Time { return now }, loc)
	maker := "0.001"
	cache.loadAll([]FeeRatesRecord{{
		ID: 3, Exchange: "okx", SpotStatus: portfolio.FeeStatusOK,
		SpotMaker: &maker, SpotTaker: &maker,
		ContractStatus: portfolio.FeeStatusUnsupported,
		UpdatedAt:      &updated, SyncStatus: FeeSyncOK,
	}})
	fees, ok := cache.get(3)
	if !ok || fees.Spot.Maker != "0.001" || fees.Contract.Status != portfolio.FeeStatusUnsupported {
		t.Fatalf("fees=%+v ok=%v", fees, ok)
	}
	if len(fees.UnsupportedMarkets) != 1 || fees.UnsupportedMarkets[0] != portfolio.FeeMarketContract {
		t.Fatalf("unsupported=%v", fees.UnsupportedMarkets)
	}
}

func TestAccountLockSerializesBindAndDaily(t *testing.T) {
	store := &memoryFeeStore{rates: map[int64]FeeRatesRecord{}}
	venues := &stubFeeVenue{delay: 50 * time.Millisecond, rates: portfolio.AccountFeeRates{
		Spot:     portfolio.MarketFee{Status: portfolio.FeeStatusOK, Maker: "0.001", Taker: "0.001"},
		Contract: portfolio.MarketFee{Status: portfolio.FeeStatusOK, Maker: "0.0002", Taker: "0.0004"},
		Source:   "venue",
	}}
	syncer := NewFeeSync(store, venues, func(TradingAccountRecord) (TradingCredentials, error) {
		return TradingCredentials{APIKey: "k", APISecret: "s"}, nil
	}, nil, nil, func() time.Time { return time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC) })
	store.account = TradingAccountRecord{ID: 9, Exchange: "binance"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go syncer.Run(ctx)
	first := syncer.Enqueue(9, true)
	second := syncer.Enqueue(9, false)
	<-first
	<-second
	venues.mu.Lock()
	calls := venues.calls
	venues.mu.Unlock()
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
	store.mu.Lock()
	status := store.rates[9].SyncStatus
	store.mu.Unlock()
	if status != FeeSyncOK {
		t.Fatalf("stored status=%s", status)
	}
}

func TestMaybeRunDailyShanghai0800IsIdempotent(t *testing.T) {
	loc, err := time.LoadLocation(shanghaiLocationName)
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryFeeStore{
		rates: map[int64]FeeRatesRecord{},
		accounts: []TradingAccountRecord{
			{ID: 1, Exchange: "binance"},
			{ID: 2, Exchange: "polymarket"},
		},
	}
	syncer := NewFeeSync(store, &stubFeeVenue{}, func(TradingAccountRecord) (TradingCredentials, error) {
		return TradingCredentials{}, nil
	}, nil, nil, func() time.Time {
		return time.Date(2026, 8, 30, 8, 0, 0, 0, loc)
	})
	ctx := context.Background()
	syncer.maybeRunDaily(ctx, time.Date(2026, 8, 30, 7, 59, 0, 0, loc))
	store.mu.Lock()
	if store.lockCalls != 0 {
		t.Fatalf("7:59 should not claim lock, calls=%d", store.lockCalls)
	}
	store.mu.Unlock()

	eight := time.Date(2026, 8, 30, 8, 0, 0, 0, loc)
	syncer.maybeRunDaily(ctx, eight)
	syncer.maybeRunDaily(ctx, eight.Add(15*time.Second))
	store.mu.Lock()
	lockCalls := store.lockCalls
	day := store.lastLockDay
	store.mu.Unlock()
	if lockCalls != 1 {
		t.Fatalf("lockCalls=%d want 1", lockCalls)
	}
	if day != "2026-08-30" {
		t.Fatalf("day=%s", day)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(syncer.low) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(syncer.low) != 1 {
		t.Fatalf("queued=%d want 1 (polymarket skipped)", len(syncer.low))
	}
}

func TestIsRetryableFeeError(t *testing.T) {
	if !isRetryableFeeError(errors.New("upstream status 429: too many requests")) {
		t.Fatal("429 should retry")
	}
	if isRetryableFeeError(errors.New("upstream status 400: invalid symbol")) {
		t.Fatal("400 should not retry")
	}
}

type memoryFeeStore struct {
	mu          sync.Mutex
	account     TradingAccountRecord
	accounts    []TradingAccountRecord
	rates       map[int64]FeeRatesRecord
	lockCalls   int
	lastLockDay string
}

func (m *memoryFeeStore) GetByID(context.Context, int64) (TradingAccountRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.account, nil
}
func (m *memoryFeeStore) GetFeeRates(_ context.Context, id int64) (FeeRatesRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if record, ok := m.rates[id]; ok {
		return record, nil
	}
	return FeeRatesRecord{ID: id}, nil
}
func (m *memoryFeeStore) UpdateFeeRates(_ context.Context, record FeeRatesRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rates[record.ID] = record
	return nil
}
func (m *memoryFeeStore) ListFeeCacheRows(context.Context) ([]FeeRatesRecord, error) { return nil, nil }
func (m *memoryFeeStore) ListFeeSyncAccounts(context.Context) ([]TradingAccountRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]TradingAccountRecord(nil), m.accounts...), nil
}
func (m *memoryFeeStore) ListPendingFeeSyncIDs(context.Context) ([]int64, error) { return nil, nil }
func (m *memoryFeeStore) TryDailyFeeSyncLock(_ context.Context, day string) (func(), bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lockCalls++
	m.lastLockDay = day
	return func() {}, true, nil
}

type stubFeeVenue struct {
	delay time.Duration
	rates portfolio.AccountFeeRates
	mu    sync.Mutex
	calls int
}

func (s *stubFeeVenue) AccountFeeRates(context.Context, string, portfolio.Credentials) (portfolio.AccountFeeRates, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	time.Sleep(s.delay)
	return s.rates, nil
}
