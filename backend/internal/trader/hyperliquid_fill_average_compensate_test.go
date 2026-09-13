package trader

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
)

func TestMissingAverageSinceUsesEarliestCreatedMinusMinute(t *testing.T) {
	t.Parallel()
	later := time.Date(2026, 9, 11, 12, 5, 0, 0, time.UTC)
	earlier := later.Add(-5 * time.Minute)
	got := missingAverageSince([]missingHyperliquidAverageOrder{
		{CreatedAt: later},
		{CreatedAt: earlier},
	})
	want := earlier.Add(-time.Minute)
	if !got.Equal(want) {
		t.Fatalf("since=%s want=%s", got, want)
	}
}

func TestAggregateHyperliquidFillsMatchesOidAndDedupesTid(t *testing.T) {
	t.Parallel()
	instrument := Instrument{Exchange: "hyperliquid", ContractType: "perpetual"}
	qty, notional, ok := aggregateHyperliquidFills([]exchange.Fill{
		{TradeID: "1", VenueOrderID: "49", Quantity: "2", Price: "100"},
		{TradeID: "1", VenueOrderID: "49", Quantity: "2", Price: "100"},
		{TradeID: "2", VenueOrderID: "49", Quantity: "3", Price: "110"},
		{TradeID: "3", VenueOrderID: "50", Quantity: "9", Price: "90"},
		{TradeID: "", VenueOrderID: "49", Quantity: "1", Price: "120"},
	}, "49", instrument)
	if !ok || !qty.Equal(decimal.NewFromInt(5)) || !notional.Equal(decimal.NewFromInt(530)) {
		t.Fatalf("qty=%s notional=%s ok=%v", qty, notional, ok)
	}
}

func TestHyperliquidAverageCompensateWritesOnlyWhenQuantityMatches(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	store := &stubAverageCompensateStore{orders: []missingHyperliquidAverageOrder{
		{
			ID: "match", CombinationID: "combo-1", OwnerUsername: "admin",
			TradingAccountID: 1, InstrumentID: 10, VenueOrderID: "49",
			FilledQuantity: "5", CreatedAt: created,
		},
		{
			ID: "mismatch", CombinationID: "combo-1", OwnerUsername: "admin",
			TradingAccountID: 1, InstrumentID: 10, VenueOrderID: "50",
			FilledQuantity: "4", CreatedAt: created.Add(time.Minute),
		},
		{
			ID: "no-oid", CombinationID: "combo-1", OwnerUsername: "admin",
			TradingAccountID: 1, InstrumentID: 10, VenueOrderID: "",
			FilledQuantity: "1", CreatedAt: created,
		},
	}}
	reader := &compensateFillReader{fills: []exchange.Fill{
		{TradeID: "t1", VenueOrderID: "49", Quantity: "2", Price: "100"},
		{TradeID: "t2", VenueOrderID: "49", Quantity: "3", Price: "110"},
		{TradeID: "t3", VenueOrderID: "50", Quantity: "1", Price: "90"},
	}}
	compensator := newTestAverageCompensator(t, store, reader, map[int64]Instrument{
		10: {ID: 10, Exchange: "hyperliquid", ContractType: "perpetual", ExchangeSymbol: "BTC"},
	})
	compensator.Compensate(context.Background(), []string{"combo-1"})
	if reader.calls() != 1 {
		t.Fatalf("list fills calls=%d", reader.calls())
	}
	if !reader.lastSince().Equal(created.Add(-time.Minute)) {
		t.Fatalf("since=%s", reader.lastSince())
	}
	updates := store.updates()
	if len(updates) != 1 || updates[0].id != "match" || updates[0].average != "106" ||
		updates[0].expected != "5" {
		t.Fatalf("updates=%+v", updates)
	}
}

func TestHyperliquidAverageCompensateGroupFailureDoesNotStopOthers(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	store := &stubAverageCompensateStore{orders: []missingHyperliquidAverageOrder{
		{
			ID: "fail", CombinationID: "combo-a", OwnerUsername: "admin",
			TradingAccountID: 1, InstrumentID: 10, VenueOrderID: "49",
			FilledQuantity: "1", CreatedAt: created,
		},
		{
			ID: "ok", CombinationID: "combo-b", OwnerUsername: "admin",
			TradingAccountID: 2, InstrumentID: 20, VenueOrderID: "51",
			FilledQuantity: "1", CreatedAt: created,
		},
	}}
	reader := &compensateFillReader{
		fills: []exchange.Fill{
			{TradeID: "t1", VenueOrderID: "51", Quantity: "1", Price: "100"},
		},
		errBySymbol: map[string]error{"FAIL": errors.New("rest failed")},
	}
	compensator := newTestAverageCompensator(t, store, reader, map[int64]Instrument{
		10: {ID: 10, Exchange: "hyperliquid", ContractType: "perpetual", ExchangeSymbol: "FAIL"},
		20: {ID: 20, Exchange: "hyperliquid", ContractType: "perpetual", ExchangeSymbol: "BTC"},
	})
	compensator.Compensate(context.Background(), []string{"combo-a", "combo-b"})
	if reader.calls() != 2 {
		t.Fatalf("list fills calls=%d", reader.calls())
	}
	updates := store.updates()
	if len(updates) != 1 || updates[0].id != "ok" {
		t.Fatalf("updates=%+v", updates)
	}
}

func TestHyperliquidAverageCompensateRESTFailureDoesNotWrite(t *testing.T) {
	t.Parallel()
	store := &stubAverageCompensateStore{orders: []missingHyperliquidAverageOrder{{
		ID: "order", CombinationID: "combo-1", OwnerUsername: "admin",
		TradingAccountID: 1, InstrumentID: 10, VenueOrderID: "49",
		FilledQuantity: "1", CreatedAt: time.Now().UTC(),
	}}}
	reader := &compensateFillReader{errBySymbol: map[string]error{"BTC": errors.New("timeout")}}
	compensator := newTestAverageCompensator(t, store, reader, map[int64]Instrument{
		10: {ID: 10, Exchange: "hyperliquid", ContractType: "perpetual", ExchangeSymbol: "BTC"},
	})
	compensator.Compensate(context.Background(), []string{"combo-1"})
	if len(store.updates()) != 0 {
		t.Fatalf("updates=%+v", store.updates())
	}
}

func TestPlanArbitragePositionMetricsWriteBlocksMissingAverage(t *testing.T) {
	t.Parallel()
	incomplete := planArbitragePositionMetricsWrite(
		arbitrageReplay{coverageComplete: false}, true, true,
	)
	if incomplete.fillOK || incomplete.writeAnnualized ||
		incomplete.quality != metricsQualityPartial || !incomplete.fundingHistoryComplete {
		t.Fatalf("incomplete=%+v", incomplete)
	}
	complete := planArbitragePositionMetricsWrite(
		arbitrageReplay{coverageComplete: true}, true, true,
	)
	if !complete.fillOK || !complete.writeAnnualized ||
		complete.quality != metricsQualityComplete || !complete.fundingHistoryComplete {
		t.Fatalf("complete=%+v", complete)
	}
}

func TestLoadArbitrageReplayStrictCoverageKeepsZeroAverageIncomplete(t *testing.T) {
	t.Parallel()
	at := time.Unix(1_700_000_000, 0).UTC()
	queryer := stubReplayQuerier{rows: []stubReplayOrderRow{
		{id: "priced", leg: "a", side: "buy", qty: "2", price: "100", at: at},
		{id: "missing", leg: "b", side: "sell", qty: "2", price: "0", at: at.Add(time.Second)},
	}}
	replay, err := loadArbitrageReplay(context.Background(), queryer, "combo", true)
	if err != nil {
		t.Fatal(err)
	}
	if replay.coverageComplete || len(replay.fills) != 1 || replay.fills[0].orderID != "priced" {
		t.Fatalf("replay=%+v", replay)
	}
}

type compensateFillCall struct {
	instrument  exchange.Instrument
	since       time.Time
	credentials exchange.Credentials
}

type compensateFillReader struct {
	mu          sync.Mutex
	recorded    []compensateFillCall
	fills       []exchange.Fill
	errBySymbol map[string]error
}

func (*compensateFillReader) GetBBO(context.Context, exchange.Instrument) (exchange.BBO, error) {
	return exchange.BBO{}, nil
}

func (*compensateFillReader) PlaceOrder(
	context.Context, exchange.Credentials, exchange.OrderRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}

func (*compensateFillReader) GetOrder(
	context.Context, exchange.Credentials, exchange.QueryRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}

func (*compensateFillReader) CancelOrder(
	context.Context, exchange.Credentials, exchange.CancelRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}

func (r *compensateFillReader) CancelAndGetOrder(
	ctx context.Context, credentials exchange.Credentials, request exchange.CancelRequest,
) (exchange.Result, error) {
	return r.CancelOrder(ctx, credentials, request)
}

func (r *compensateFillReader) ListFills(
	_ context.Context,
	credentials exchange.Credentials,
	instrument exchange.Instrument,
	since time.Time,
) ([]exchange.Fill, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorded = append(r.recorded, compensateFillCall{
		instrument: instrument, since: since, credentials: credentials,
	})
	if err := r.errBySymbol[instrument.ExchangeSymbol]; err != nil {
		return nil, err
	}
	return r.fills, nil
}

func (r *compensateFillReader) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.recorded)
}

func (r *compensateFillReader) lastSince() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.recorded) == 0 {
		return time.Time{}
	}
	return r.recorded[len(r.recorded)-1].since
}

type stubAverageCompensateStore struct {
	mu      sync.Mutex
	orders  []missingHyperliquidAverageOrder
	written []averageCompensateUpdate
}

type averageCompensateUpdate struct {
	id, average, expected string
}

func (s *stubAverageCompensateStore) ListMissingHyperliquidArbitrageAverages(
	context.Context, []string,
) ([]missingHyperliquidAverageOrder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]missingHyperliquidAverageOrder, len(s.orders))
	copy(out, s.orders)
	return out, nil
}

func (s *stubAverageCompensateStore) TrySetHyperliquidArbitrageAverage(
	_ context.Context, id, average, expected string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, averageCompensateUpdate{
		id: id, average: average, expected: expected,
	})
	return nil
}

func (s *stubAverageCompensateStore) updates() []averageCompensateUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]averageCompensateUpdate, len(s.written))
	copy(out, s.written)
	return out
}

type stubAverageCompensateCatalog struct {
	items map[int64]Instrument
}

func (s stubAverageCompensateCatalog) List(context.Context, string, string) ([]Instrument, error) {
	return nil, nil
}

func (s stubAverageCompensateCatalog) Get(_ context.Context, id int64) (Instrument, error) {
	item, ok := s.items[id]
	if !ok {
		return Instrument{}, ErrInstrumentUnavailable
	}
	return item, nil
}

type stubAverageCompensateCredentials struct{}

func (stubAverageCompensateCredentials) GetInternal(
	_ context.Context, _, _ string, accountID int64,
) (Credentials, error) {
	return Credentials{TradingAccountID: accountID, Exchange: "hyperliquid"}, nil
}

func newTestAverageCompensator(
	t *testing.T,
	store hyperliquidAverageCompensateStore,
	reader *compensateFillReader,
	instruments map[int64]Instrument,
) *HyperliquidArbitrageFillAverageCompensator {
	t.Helper()
	return NewHyperliquidArbitrageFillAverageCompensator(
		store,
		stubAverageCompensateCatalog{items: instruments},
		stubAverageCompensateCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"hyperliquid": reader}),
		"token",
		time.Second,
		slog.Default(),
	)
}
