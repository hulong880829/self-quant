package trader

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/orderstream"
)

func TestArbitrageFillAverageCompensateMemoryHitSkipsGetOrder(t *testing.T) {
	t.Parallel()
	order := sampleMissingAverageOrder("binance-1", "binance", 1, 10)
	store := &stubMissingAverageStore{orders: []missingArbitrageAverageOrder{order}}
	adapter := &countingGetOrderAdapter{}
	compensator := newTestFillAverageCompensator(
		t, store,
		stubRecentAverages{items: []orderstream.RecentFillAverage{matchingFillAverage(order, "11.5", "2")}},
		map[string]exchange.Adapter{"binance": adapter},
		map[int64]Instrument{10: binancePerp(10)},
		nil,
	)
	compensator.Compensate(context.Background(), []string{order.CombinationID})
	if adapter.getOrderCalls() != 0 {
		t.Fatalf("GetOrder calls=%d", adapter.getOrderCalls())
	}
	updates := store.updates()
	if len(updates) != 1 || updates[0].id != order.ID || updates[0].average != "11.5" {
		t.Fatalf("updates=%+v", updates)
	}
}

func TestArbitrageFillAverageCompensateNilManagerSkipsMemory(t *testing.T) {
	t.Parallel()
	order := sampleMissingAverageOrder("binance-nil", "binance", 1, 10)
	store := &stubMissingAverageStore{orders: []missingArbitrageAverageOrder{order}}
	adapter := &countingGetOrderAdapter{results: map[string]exchange.Result{
		order.ClientOrderID: {AveragePrice: "9", FilledQuantity: "2", VenueOrderID: order.VenueOrderID},
	}}
	var streams *orderstream.Manager
	compensator := newTestFillAverageCompensator(
		t, store, streams,
		map[string]exchange.Adapter{"binance": adapter},
		map[int64]Instrument{10: binancePerp(10)},
		nil,
	)
	compensator.Compensate(context.Background(), []string{order.CombinationID})
	if adapter.getOrderCalls() != 1 {
		t.Fatalf("GetOrder calls=%d", adapter.getOrderCalls())
	}
	if len(store.updates()) != 1 {
		t.Fatalf("updates=%+v", store.updates())
	}
}

func TestArbitrageFillAverageCompensateMemoryQuantityMismatchUsesGetOrder(t *testing.T) {
	t.Parallel()
	order := sampleMissingAverageOrder("binance-partial", "binance", 1, 10)
	store := &stubMissingAverageStore{orders: []missingArbitrageAverageOrder{order}}
	adapter := &countingGetOrderAdapter{results: map[string]exchange.Result{
		order.ClientOrderID: {AveragePrice: "12", FilledQuantity: "2", VenueOrderID: order.VenueOrderID},
	}}
	compensator := newTestFillAverageCompensator(
		t, store,
		stubRecentAverages{items: []orderstream.RecentFillAverage{matchingFillAverage(order, "11.5", "1")}},
		map[string]exchange.Adapter{"binance": adapter},
		map[int64]Instrument{10: binancePerp(10)},
		nil,
	)
	compensator.Compensate(context.Background(), []string{order.CombinationID})
	if adapter.getOrderCalls() != 1 {
		t.Fatalf("GetOrder calls=%d", adapter.getOrderCalls())
	}
	updates := store.updates()
	if len(updates) != 1 || updates[0].average != "12" {
		t.Fatalf("updates=%+v", updates)
	}
}

func TestArbitrageFillAverageCompensateHyperliquidMissUsesFillReader(t *testing.T) {
	t.Parallel()
	hl := sampleMissingAverageOrder("hl-1", "hyperliquid", 1, 10)
	cex := sampleMissingAverageOrder("binance-miss", "binance", 2, 20)
	store := &stubMissingAverageStore{orders: []missingArbitrageAverageOrder{hl, cex}}
	hlStore := &stubAverageCompensateStore{}
	reader := &compensateFillReader{fills: []exchange.Fill{
		{TradeID: "t1", VenueOrderID: hl.VenueOrderID, Quantity: "2", Price: "100"},
	}}
	adapter := &countingGetOrderAdapter{results: map[string]exchange.Result{
		cex.ClientOrderID: {AveragePrice: "8", FilledQuantity: "2", VenueOrderID: cex.VenueOrderID},
	}}
	hyperliquid := newTestAverageCompensator(t, hlStore, reader, map[int64]Instrument{
		10: {ID: 10, Exchange: "hyperliquid", ContractType: "perpetual", ExchangeSymbol: "BTC"},
	})
	compensator := newTestFillAverageCompensator(
		t, store, nil,
		map[string]exchange.Adapter{"binance": adapter, "hyperliquid": reader},
		map[int64]Instrument{
			10: {ID: 10, Exchange: "hyperliquid", ContractType: "perpetual", ExchangeSymbol: "BTC"},
			20: binancePerp(20),
		},
		hyperliquid,
	)
	compensator.Compensate(context.Background(), []string{hl.CombinationID})
	if store.listCalls != 1 {
		t.Fatalf("generic list calls=%d", store.listCalls)
	}
	if hlStore.missingListCalls() != 0 {
		t.Fatalf("hyperliquid ListMissing calls=%d", hlStore.missingListCalls())
	}
	if reader.getOrderCalls() != 0 {
		t.Fatalf("hyperliquid GetOrder calls=%d", reader.getOrderCalls())
	}
	if reader.calls() != 1 {
		t.Fatalf("list fills calls=%d", reader.calls())
	}
	if adapter.getOrderCalls() != 1 {
		t.Fatalf("binance GetOrder calls=%d", adapter.getOrderCalls())
	}
	if len(hlStore.updates()) != 0 {
		t.Fatalf("hyperliquid store wrote %+v", hlStore.updates())
	}
	updates := store.updates()
	if len(updates) != 2 {
		t.Fatalf("updates=%+v", updates)
	}
}

func TestArbitrageFillAverageCompensateRESTEmptyOrMismatchDoesNotWrite(t *testing.T) {
	t.Parallel()
	empty := sampleMissingAverageOrder("empty", "binance", 1, 10)
	mismatch := sampleMissingAverageOrder("mismatch", "binance", 2, 20)
	ok := sampleMissingAverageOrder("ok", "binance", 3, 30)
	store := &stubMissingAverageStore{orders: []missingArbitrageAverageOrder{empty, mismatch, ok}}
	adapter := &countingGetOrderAdapter{results: map[string]exchange.Result{
		empty.ClientOrderID:    {AveragePrice: "0", FilledQuantity: "2", VenueOrderID: empty.VenueOrderID},
		mismatch.ClientOrderID: {AveragePrice: "11", FilledQuantity: "1", VenueOrderID: mismatch.VenueOrderID},
		ok.ClientOrderID:       {AveragePrice: "13", FilledQuantity: "2", VenueOrderID: ok.VenueOrderID},
	}}
	compensator := newTestFillAverageCompensator(
		t, store, nil,
		map[string]exchange.Adapter{"binance": adapter},
		map[int64]Instrument{10: binancePerp(10), 20: binancePerp(20), 30: binancePerp(30)},
		nil,
	)
	compensator.Compensate(context.Background(), []string{"combo"})
	if adapter.getOrderCalls() != 3 {
		t.Fatalf("GetOrder calls=%d", adapter.getOrderCalls())
	}
	updates := store.updates()
	if len(updates) != 1 || updates[0].id != ok.ID || updates[0].average != "13" {
		t.Fatalf("updates=%+v", updates)
	}
}

func TestArbitrageFillAverageCompensateRESTErrorDoesNotStopBatch(t *testing.T) {
	t.Parallel()
	fail := sampleMissingAverageOrder("fail", "binance", 1, 10)
	ok := sampleMissingAverageOrder("ok", "binance", 2, 20)
	store := &stubMissingAverageStore{orders: []missingArbitrageAverageOrder{fail, ok}}
	adapter := &countingGetOrderAdapter{
		errByClient: map[string]error{fail.ClientOrderID: errors.New("timeout")},
		results: map[string]exchange.Result{
			ok.ClientOrderID: {AveragePrice: "14", FilledQuantity: "2", VenueOrderID: ok.VenueOrderID},
		},
	}
	compensator := newTestFillAverageCompensator(
		t, store, nil,
		map[string]exchange.Adapter{"binance": adapter},
		map[int64]Instrument{10: binancePerp(10), 20: binancePerp(20)},
		nil,
	)
	compensator.Compensate(context.Background(), []string{"combo"})
	updates := store.updates()
	if len(updates) != 1 || updates[0].id != ok.ID {
		t.Fatalf("updates=%+v", updates)
	}
}

func TestArbitrageFillAverageCompensateGroupsByAccount(t *testing.T) {
	t.Parallel()
	sameA := sampleMissingAverageOrder("a1", "binance", 1, 10)
	sameB := sampleMissingAverageOrder("a2", "binance", 1, 20)
	other := sampleMissingAverageOrder("b1", "binance", 2, 30)
	third := sampleMissingAverageOrder("c1", "binance", 3, 40)
	store := &stubMissingAverageStore{orders: []missingArbitrageAverageOrder{sameA, sameB, other, third}}
	adapter := &countingGetOrderAdapter{
		delay: 40 * time.Millisecond,
		results: map[string]exchange.Result{
			sameA.ClientOrderID: {AveragePrice: "10", FilledQuantity: "2", VenueOrderID: sameA.VenueOrderID},
			sameB.ClientOrderID: {AveragePrice: "11", FilledQuantity: "2", VenueOrderID: sameB.VenueOrderID},
			other.ClientOrderID: {AveragePrice: "12", FilledQuantity: "2", VenueOrderID: other.VenueOrderID},
			third.ClientOrderID: {AveragePrice: "13", FilledQuantity: "2", VenueOrderID: third.VenueOrderID},
		},
	}
	catalog := &countingAverageCatalog{items: map[int64]Instrument{
		10: binancePerp(10), 20: binancePerp(20), 30: binancePerp(30), 40: binancePerp(40),
	}}
	compensator := NewArbitrageFillAverageCompensator(
		store, nil, catalog, stubAverageCompensateCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"binance": adapter}),
		nil, "token", time.Second, slog.Default(),
	)
	compensator.Compensate(context.Background(), []string{"combo"})
	if adapter.getOrderCalls() != 4 {
		t.Fatalf("GetOrder calls=%d", adapter.getOrderCalls())
	}
	if max := adapter.maxInflight.Load(); max != 2 {
		t.Fatalf("max inflight=%d", max)
	}
	if overlappingGets(adapter.callsForClients(sameA.ClientOrderID, sameB.ClientOrderID)) {
		t.Fatal("same-account GetOrder overlapped")
	}
	if got := catalog.ids(); len(got) != 4 {
		t.Fatalf("catalog gets=%v", got)
	}
	if len(store.updates()) != 4 {
		t.Fatalf("updates=%+v", store.updates())
	}
}

func TestArbitrageFillAverageCompensateCachesInstrumentPerAccount(t *testing.T) {
	t.Parallel()
	first := sampleMissingAverageOrder("same-a", "binance", 1, 10)
	second := sampleMissingAverageOrder("same-b", "binance", 1, 10)
	store := &stubMissingAverageStore{orders: []missingArbitrageAverageOrder{first, second}}
	adapter := &countingGetOrderAdapter{results: map[string]exchange.Result{
		first.ClientOrderID:  {AveragePrice: "10", FilledQuantity: "2", VenueOrderID: first.VenueOrderID},
		second.ClientOrderID: {AveragePrice: "11", FilledQuantity: "2", VenueOrderID: second.VenueOrderID},
	}}
	catalog := &countingAverageCatalog{items: map[int64]Instrument{10: binancePerp(10)}}
	compensator := NewArbitrageFillAverageCompensator(
		store, nil, catalog, stubAverageCompensateCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"binance": adapter}),
		nil, "token", time.Second, slog.Default(),
	)
	compensator.Compensate(context.Background(), []string{"combo"})
	if got := catalog.ids(); len(got) != 1 || got[0] != 10 {
		t.Fatalf("catalog gets=%v", got)
	}
	if adapter.getOrderCalls() != 2 {
		t.Fatalf("GetOrder calls=%d", adapter.getOrderCalls())
	}
}

func TestArbitrageFillAverageCompensateRejectedFilledCanCAS(t *testing.T) {
	t.Parallel()
	order := sampleMissingAverageOrder("rejected", "binance", 1, 10)
	order.ID = "rejected-filled"
	store := &stubMissingAverageStore{orders: []missingArbitrageAverageOrder{order}}
	compensator := newTestFillAverageCompensator(
		t, store,
		stubRecentAverages{items: []orderstream.RecentFillAverage{matchingFillAverage(order, "7.5", "2")}},
		map[string]exchange.Adapter{"binance": &countingGetOrderAdapter{}},
		map[int64]Instrument{10: binancePerp(10)},
		nil,
	)
	compensator.Compensate(context.Background(), []string{order.CombinationID})
	updates := store.updates()
	if len(updates) != 1 || updates[0].id != "rejected-filled" {
		t.Fatalf("updates=%+v", updates)
	}
}

func TestArbitrageFillAverageCompensateCASFalseStopsWithoutFallback(t *testing.T) {
	t.Parallel()
	order := sampleMissingAverageOrder("cas-false", "binance", 1, 10)
	hl := sampleMissingAverageOrder("hl-cas", "hyperliquid", 1, 10)
	store := &stubMissingAverageStore{
		orders:     []missingArbitrageAverageOrder{order, hl},
		casFalseID: map[string]bool{order.ID: true, hl.ID: true},
	}
	hlStore := &stubAverageCompensateStore{}
	reader := &compensateFillReader{fills: []exchange.Fill{
		{TradeID: "t1", VenueOrderID: hl.VenueOrderID, Quantity: "2", Price: "100"},
	}}
	adapter := &countingGetOrderAdapter{}
	hyperliquid := newTestAverageCompensator(t, hlStore, reader, map[int64]Instrument{
		10: {ID: 10, Exchange: "hyperliquid", ContractType: "perpetual", ExchangeSymbol: "BTC"},
	})
	compensator := newTestFillAverageCompensator(
		t, store,
		stubRecentAverages{items: []orderstream.RecentFillAverage{
			matchingFillAverage(order, "11.5", "2"),
			matchingFillAverage(hl, "100", "2"),
		}},
		map[string]exchange.Adapter{"binance": adapter, "hyperliquid": reader},
		map[int64]Instrument{
			10: {ID: 10, Exchange: "hyperliquid", ContractType: "perpetual", ExchangeSymbol: "BTC"},
		},
		hyperliquid,
	)
	compensator.Compensate(context.Background(), []string{"combo"})
	if adapter.getOrderCalls() != 0 {
		t.Fatalf("GetOrder calls=%d", adapter.getOrderCalls())
	}
	if reader.calls() != 0 {
		t.Fatalf("list fills calls=%d", reader.calls())
	}
	if len(store.updates()) != 0 {
		t.Fatalf("updates=%+v", store.updates())
	}
}

func TestArbitrageFillAverageCompensateOKXUsesSamePath(t *testing.T) {
	t.Parallel()
	order := sampleMissingAverageOrder("okx-1", "okx", 4, 50)
	store := &stubMissingAverageStore{orders: []missingArbitrageAverageOrder{order}}
	adapter := &countingGetOrderAdapter{}
	compensator := newTestFillAverageCompensator(
		t, store,
		stubRecentAverages{items: []orderstream.RecentFillAverage{matchingFillAverage(order, "101", "2")}},
		map[string]exchange.Adapter{"okx": adapter},
		map[int64]Instrument{50: {
			ID: 50, Exchange: "okx", ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
			ContractSize: "1",
		}},
		nil,
	)
	compensator.Compensate(context.Background(), []string{order.CombinationID})
	if adapter.getOrderCalls() != 0 {
		t.Fatalf("GetOrder calls=%d", adapter.getOrderCalls())
	}
	if len(store.updates()) != 1 || store.updates()[0].average != "101" {
		t.Fatalf("updates=%+v", store.updates())
	}
}

func TestArbitrageFillAverageCompensateTrySetErrorDoesNotFallback(t *testing.T) {
	t.Parallel()
	order := sampleMissingAverageOrder("err", "binance", 1, 10)
	store := &stubMissingAverageStore{
		orders: []missingArbitrageAverageOrder{order},
		tryErr: errors.New("db down"),
	}
	adapter := &countingGetOrderAdapter{}
	compensator := newTestFillAverageCompensator(
		t, store,
		stubRecentAverages{items: []orderstream.RecentFillAverage{matchingFillAverage(order, "11.5", "2")}},
		map[string]exchange.Adapter{"binance": adapter},
		map[int64]Instrument{10: binancePerp(10)},
		nil,
	)
	compensator.Compensate(context.Background(), []string{order.CombinationID})
	if adapter.getOrderCalls() != 0 {
		t.Fatalf("GetOrder calls=%d", adapter.getOrderCalls())
	}
	if len(store.updates()) != 0 {
		t.Fatalf("updates=%+v", store.updates())
	}
}

func TestArbitrageFillAverageCompensateRace(t *testing.T) {
	t.Parallel()
	order := sampleMissingAverageOrder("race", "binance", 1, 10)
	store := &stubMissingAverageStore{orders: []missingArbitrageAverageOrder{order}}
	adapter := &countingGetOrderAdapter{}
	compensator := newTestFillAverageCompensator(
		t, store,
		stubRecentAverages{items: []orderstream.RecentFillAverage{matchingFillAverage(order, "11.5", "2")}},
		map[string]exchange.Adapter{"binance": adapter},
		map[int64]Instrument{10: binancePerp(10)},
		nil,
	)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			compensator.Compensate(context.Background(), []string{order.CombinationID})
		}()
	}
	wg.Wait()
	if adapter.getOrderCalls() != 0 {
		t.Fatalf("GetOrder calls=%d", adapter.getOrderCalls())
	}
	if len(store.updates()) != 1 {
		t.Fatalf("updates=%+v", store.updates())
	}
}

func TestGroupMissingAverageByAccount(t *testing.T) {
	t.Parallel()
	groups := groupMissingAverageByAccount([]missingArbitrageAverageOrder{
		sampleMissingAverageOrder("a", "Binance", 1, 10),
		sampleMissingAverageOrder("b", "binance", 1, 20),
		sampleMissingAverageOrder("c", "okx", 1, 30),
	})
	if len(groups) != 2 {
		t.Fatalf("groups=%d", len(groups))
	}
	if len(groups[0].Orders) != 2 || groups[0].Exchange != "binance" {
		t.Fatalf("group0=%+v", groups[0])
	}
	if len(groups[1].Orders) != 1 || groups[1].Exchange != "okx" {
		t.Fatalf("group1=%+v", groups[1])
	}
}

func newTestFillAverageCompensator(
	t *testing.T,
	store missingAverageStore,
	streams recentAverageSource,
	adapters map[string]exchange.Adapter,
	instruments map[int64]Instrument,
	hyperliquid *HyperliquidArbitrageFillAverageCompensator,
) *ArbitrageFillAverageCompensator {
	t.Helper()
	return NewArbitrageFillAverageCompensator(
		store, streams, stubAverageCompensateCatalog{items: instruments},
		stubAverageCompensateCredentials{}, exchange.NewTestRegistry(adapters),
		hyperliquid, "token", time.Second, slog.Default(),
	)
}

func sampleMissingAverageOrder(id, venue string, accountID, instrumentID int64) missingArbitrageAverageOrder {
	return missingArbitrageAverageOrder{
		ID: id, CombinationID: "combo", OwnerUsername: "admin",
		TradingAccountID: accountID, Exchange: venue, ContractType: "perpetual",
		InstrumentID: instrumentID, ClientOrderID: id + "-c", VenueOrderID: id + "-v",
		FilledQuantity: "2", CreatedAt: time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
	}
}

func matchingFillAverage(order missingArbitrageAverageOrder, average, filled string) orderstream.RecentFillAverage {
	return orderstream.RecentFillAverage{
		Key: orderstream.Key{
			Account: strconv.FormatInt(order.TradingAccountID, 10),
			Venue:   order.Exchange,
			Product: order.ContractType,
		},
		ClientOrderID:    order.ClientOrderID,
		VenueOrderID:     order.VenueOrderID,
		CumulativeFilled: filled,
		AveragePrice:     average,
	}
}

func binancePerp(id int64) Instrument {
	return Instrument{ID: id, Exchange: "binance", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT"}
}

type stubRecentAverages struct {
	items []orderstream.RecentFillAverage
}

func (s stubRecentAverages) LookupRecentFillAverage(
	key orderstream.Key, clientOrderID, venueOrderID string,
) (orderstream.RecentFillAverage, bool) {
	for _, item := range s.items {
		if item.Key.Account != key.Account {
			continue
		}
		if !strings.EqualFold(item.Key.Venue, key.Venue) {
			continue
		}
		if clientOrderID != "" && item.ClientOrderID == clientOrderID {
			return item, true
		}
		if venueOrderID != "" && item.VenueOrderID == venueOrderID {
			return item, true
		}
	}
	return orderstream.RecentFillAverage{}, false
}

type stubMissingAverageStore struct {
	mu         sync.Mutex
	orders     []missingArbitrageAverageOrder
	written    []averageCompensateUpdate
	listCalls  int
	casFalseID map[string]bool
	tryErr     error
	prices     map[string]string
}

func (s *stubMissingAverageStore) ListMissingArbitrageOrderAverages(
	context.Context, []string,
) ([]missingArbitrageAverageOrder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	out := make([]missingArbitrageAverageOrder, len(s.orders))
	copy(out, s.orders)
	return out, nil
}

func (s *stubMissingAverageStore) TrySetArbitrageOrderAverage(
	_ context.Context, id, average, expected string,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tryErr != nil {
		return false, s.tryErr
	}
	if s.casFalseID[id] {
		return false, nil
	}
	if s.prices == nil {
		s.prices = make(map[string]string)
	}
	if _, exists := s.prices[id]; exists {
		return false, nil
	}
	s.prices[id] = average
	s.written = append(s.written, averageCompensateUpdate{id: id, average: average, expected: expected})
	return true, nil
}

func (s *stubMissingAverageStore) updates() []averageCompensateUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]averageCompensateUpdate, len(s.written))
	copy(out, s.written)
	return out
}

type countingAverageCatalog struct {
	mu    sync.Mutex
	items map[int64]Instrument
	got   []int64
}

func (c *countingAverageCatalog) List(context.Context, string, string) ([]Instrument, error) {
	return nil, nil
}

func (c *countingAverageCatalog) Get(_ context.Context, id int64) (Instrument, error) {
	c.mu.Lock()
	c.got = append(c.got, id)
	c.mu.Unlock()
	item, ok := c.items[id]
	if !ok {
		return Instrument{}, ErrInstrumentUnavailable
	}
	return item, nil
}

func (c *countingAverageCatalog) ids() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]int64, len(c.got))
	copy(out, c.got)
	return out
}

type timedGetOrder struct {
	client     string
	instrument string
	start, end time.Time
}

type countingGetOrderAdapter struct {
	mu          sync.Mutex
	recorded    []timedGetOrder
	results     map[string]exchange.Result
	errByClient map[string]error
	delay       time.Duration
	inflight    atomic.Int32
	maxInflight atomic.Int32
}

func (*countingGetOrderAdapter) GetBBO(context.Context, exchange.Instrument) (exchange.BBO, error) {
	return exchange.BBO{}, nil
}

func (*countingGetOrderAdapter) PlaceOrder(
	context.Context, exchange.Credentials, exchange.OrderRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}

func (a *countingGetOrderAdapter) GetOrder(
	_ context.Context, credentials exchange.Credentials, request exchange.QueryRequest,
) (exchange.Result, error) {
	n := a.inflight.Add(1)
	for {
		current := a.maxInflight.Load()
		if n <= current || a.maxInflight.CompareAndSwap(current, n) {
			break
		}
	}
	start := time.Now()
	if a.delay > 0 {
		time.Sleep(a.delay)
	}
	end := time.Now()
	a.inflight.Add(-1)
	a.mu.Lock()
	a.recorded = append(a.recorded, timedGetOrder{
		client: request.ClientOrderID, instrument: request.Instrument.ExchangeSymbol,
		start: start, end: end,
	})
	a.mu.Unlock()
	if err := a.errByClient[request.ClientOrderID]; err != nil {
		return exchange.Result{}, err
	}
	if result, ok := a.results[request.ClientOrderID]; ok {
		return result, nil
	}
	return exchange.Result{}, nil
}

func (*countingGetOrderAdapter) CancelOrder(
	context.Context, exchange.Credentials, exchange.CancelRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}

func (a *countingGetOrderAdapter) CancelAndGetOrder(
	ctx context.Context, credentials exchange.Credentials, request exchange.CancelRequest,
) (exchange.Result, error) {
	return a.CancelOrder(ctx, credentials, request)
}

func (a *countingGetOrderAdapter) getOrderCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.recorded)
}

func (a *countingGetOrderAdapter) callsForClients(ids ...string) []timedGetOrder {
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]timedGetOrder, 0)
	for _, item := range a.recorded {
		if _, ok := want[item.client]; ok {
			out = append(out, item)
		}
	}
	return out
}

func overlappingGets(calls []timedGetOrder) bool {
	for i := 0; i < len(calls); i++ {
		for j := i + 1; j < len(calls); j++ {
			if calls[i].start.Before(calls[j].end) && calls[j].start.Before(calls[i].end) {
				return true
			}
		}
	}
	return false
}
