package trader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"selfquant/backend/internal/trader/exchange"
)

type memoryStore struct {
	mu     sync.Mutex
	orders map[string]Order
	keys   map[string]string
}

type failingOrderStore struct {
	*memoryStore
	createErr error
	updateErr error
}

func (s *failingOrderStore) CreateIntent(
	ctx context.Context,
	order Order,
) (Order, bool, error) {
	if s.createErr != nil {
		return Order{}, false, s.createErr
	}
	return s.memoryStore.CreateIntent(ctx, order)
}

func (s *failingOrderStore) UpdateResult(
	ctx context.Context,
	orderID string,
	result VenueResult,
) (Order, error) {
	if s.updateErr != nil {
		return Order{}, s.updateErr
	}
	return s.memoryStore.UpdateResult(ctx, orderID, result)
}

func newMemoryStore() *memoryStore {
	return &memoryStore{orders: map[string]Order{}, keys: map[string]string{}}
}

func (m *memoryStore) CreateIntent(_ context.Context, order Order) (Order, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.keys[order.IdempotencyKey]; ok {
		return m.orders[id], false, nil
	}
	if order.ID == "" {
		order.ID = "ord-" + order.IdempotencyKey
	}
	if order.ClientOrderID == "" {
		order.ClientOrderID = "sq" + order.IdempotencyKey[:8]
	}
	order.Status = "pending"
	order.CreatedAt = time.Now().UTC()
	order.UpdatedAt = order.CreatedAt
	m.orders[order.ID] = order
	m.keys[order.IdempotencyKey] = order.ID
	return order, true, nil
}

func (m *memoryStore) GetByOwner(_ context.Context, owner, orderID string) (Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.orders[orderID]
	if !ok || order.OwnerUsername != owner {
		return Order{}, ErrNotFound
	}
	return order, nil
}

func (m *memoryStore) ListByOwnerAccount(
	_ context.Context,
	owner string,
	accountID int64,
	view string,
	_ int,
	_ string,
) ([]Order, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Order, 0)
	for _, order := range m.orders {
		isTerminal := terminalStatus(order.Status)
		if order.OwnerUsername == owner && order.TradingAccountID == accountID &&
			((view == "history" && isTerminal) || (view != "history" && !isTerminal)) {
			result = append(result, order)
		}
	}
	return result, "", nil
}

func (m *memoryStore) UpdateResult(_ context.Context, orderID string, result VenueResult) (Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	order, ok := m.orders[orderID]
	if !ok {
		return Order{}, ErrNotFound
	}
	if result.VenueOrderID != "" {
		order.VenueOrderID = result.VenueOrderID
	}
	order.Status = result.Status
	if result.FilledQuantity != "" {
		order.FilledQuantity = result.FilledQuantity
	}
	if result.AveragePrice != "" {
		order.AveragePrice = result.AveragePrice
	}
	order.ErrorCode = result.ErrorCode
	order.ErrorMessage = result.ErrorMessage
	order.UpdatedAt = time.Now().UTC()
	m.orders[orderID] = order
	return order, nil
}

func (m *memoryStore) AppendEvent(context.Context, string, string, map[string]any) error {
	return nil
}

type stubCatalog struct {
	item Instrument
}

func (s stubCatalog) List(context.Context, string, string) ([]Instrument, error) {
	return []Instrument{s.item}, nil
}

func (s stubCatalog) Get(_ context.Context, id int64) (Instrument, error) {
	if id != s.item.ID {
		return Instrument{}, ErrInstrumentUnavailable
	}
	return s.item, nil
}

type stubCredentials struct {
	owner string
	item  Credentials
	err   error
}

func (s stubCredentials) Owner(context.Context, string) (string, error) {
	return s.owner, s.err
}

func (s stubCredentials) Get(context.Context, string, int64) (Credentials, error) {
	return s.item, s.err
}

type stubAdapter struct {
	place    exchange.Result
	query    exchange.Result
	err      error
	queryErr error
	calls    int
	getCalls int
}

func (s *stubAdapter) GetBBO(context.Context, exchange.Instrument) (exchange.BBO, error) {
	return exchange.BBO{
		BidPrice: "99.9", AskPrice: "100.1", Timestamp: time.Now().UTC(),
	}, nil
}

func (s *stubAdapter) PlaceOrder(context.Context, exchange.Credentials, exchange.OrderRequest) (exchange.Result, error) {
	s.calls++
	return s.place, s.err
}

func (s *stubAdapter) GetOrder(context.Context, exchange.Credentials, exchange.QueryRequest) (exchange.Result, error) {
	s.getCalls++
	if s.queryErr != nil {
		return exchange.Result{}, s.queryErr
	}
	if s.query.Status != "" {
		return s.query, nil
	}
	return s.place, s.err
}

func (s *stubAdapter) CancelOrder(context.Context, exchange.Credentials, exchange.CancelRequest) (exchange.Result, error) {
	return exchange.Result{Status: "canceled", VenueOrderID: "1"}, nil
}

func testService(adapter *stubAdapter) (*Service, *memoryStore) {
	store := newMemoryStore()
	service := NewService(
		store,
		stubCatalog{item: Instrument{
			ID: 7, Exchange: "binance", ContractType: "perpetual",
			ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
			PriceTick: "0.1", QuantityStep: "0.001", ContractSize: "1",
		}},
		stubCredentials{owner: "admin", item: Credentials{
			TradingAccountID: 3, ProductName: "Funding Arb", Exchange: "binance", AccountName: "main",
		}},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"binance": adapter}),
		time.Second,
		nil,
	)
	return service, store
}

func TestNormalizeAndValidateOrder(t *testing.T) {
	input, err := normalizePlaceInput(PlaceOrderInput{
		Token: "tok", TradingAccountID: 1, InstrumentID: 2, Side: "BUY",
		OrderType: "limit", Quantity: "0.010", Price: "100.0", IdempotencyKey: "k1",
	})
	if err != nil || input.Side != "buy" || input.Quantity != "0.01" {
		t.Fatalf("input=%+v err=%v", input, err)
	}
	if err := validateInstrumentRules(Instrument{QuantityStep: "0.001", PriceTick: "0.1"}, "limit", "0.01", "100.0"); err != nil {
		t.Fatal(err)
	}
	if err := validateInstrumentRules(Instrument{QuantityStep: "0.001", PriceTick: "0.1"}, "limit", "0.0015", "100.0"); err != ErrInvalidArgument {
		t.Fatalf("err=%v", err)
	}
	if _, err := normalizePlaceInput(PlaceOrderInput{
		Token: "tok", TradingAccountID: 1, InstrumentID: 2, Side: "buy",
		OrderType: "market", Quantity: "1", Price: "10", IdempotencyKey: "k1",
	}); err != ErrInvalidArgument {
		t.Fatalf("err=%v", err)
	}
}

func TestPlaceOrderIdempotentAndConflict(t *testing.T) {
	adapter := &stubAdapter{place: exchange.Result{VenueOrderID: "v1", Status: "open"}}
	service, _ := testService(adapter)
	first, err := service.PlaceOrder(context.Background(), PlaceOrderInput{
		Token: "tok", TradingAccountID: 3, InstrumentID: 7, Side: "buy",
		OrderType: "limit", Quantity: "0.001", Price: "100.0", IdempotencyKey: "same-key",
	})
	if err != nil || first.Status != "open" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := service.PlaceOrder(context.Background(), PlaceOrderInput{
		Token: "tok", TradingAccountID: 3, InstrumentID: 7, Side: "buy",
		OrderType: "limit", Quantity: "0.001", Price: "100.0", IdempotencyKey: "same-key",
	})
	if err != nil || second.ID != first.ID || adapter.calls != 1 {
		t.Fatalf("second=%+v calls=%d err=%v", second, adapter.calls, err)
	}
	if _, err := service.PlaceOrder(context.Background(), PlaceOrderInput{
		Token: "tok", TradingAccountID: 3, InstrumentID: 7, Side: "sell",
		OrderType: "limit", Quantity: "0.001", Price: "100.0", IdempotencyKey: "same-key",
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err=%v", err)
	}
}

func TestPlaceOrderDoesNotRetryOnTimeout(t *testing.T) {
	adapter := &stubAdapter{err: exchange.ErrUncertain, place: exchange.Result{}}
	service, _ := testService(adapter)
	order, err := service.PlaceOrder(context.Background(), PlaceOrderInput{
		Token: "tok", TradingAccountID: 3, InstrumentID: 7, Side: "buy",
		OrderType: "market", Quantity: "0.001", IdempotencyKey: "timeout-key",
	})
	if !errors.Is(err, ErrVenueUncertain) || order.Status != "unknown" || adapter.calls != 1 {
		t.Fatalf("order=%+v calls=%d err=%v", order, adapter.calls, err)
	}
}

func TestPlaceOrderClassifiesAndLogsPersistenceFailure(t *testing.T) {
	var logs bytes.Buffer
	store := &failingOrderStore{
		memoryStore: newMemoryStore(),
		createErr:   errors.New("database insert broke"),
	}
	service := NewService(
		store,
		stubCatalog{item: Instrument{
			ID: 7, Exchange: "binance", ContractType: "perpetual",
			ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
			PriceTick: "0.1", QuantityStep: "0.001", ContractSize: "1",
		}},
		stubCredentials{owner: "admin", item: Credentials{
			TradingAccountID: 3, ProductName: "Funding Arb", Exchange: "binance",
		}},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"binance": &stubAdapter{}}),
		time.Second,
		slog.New(slog.NewTextHandler(&logs, nil)),
	)
	_, err := service.PlaceOrder(context.Background(), PlaceOrderInput{
		Token: "tok", TradingAccountID: 3, InstrumentID: 7, Side: "buy",
		OrderType: "market", Quantity: "0.001", IdempotencyKey: "db-error-key",
	})
	if !errors.Is(err, ErrPersistence) {
		t.Fatalf("err=%v", err)
	}
	output := logs.String()
	if !strings.Contains(output, "error_class=persistence") ||
		!strings.Contains(output, "database insert broke") {
		t.Fatalf("log=%s", output)
	}
}

func TestPlaceOrderLogsVenueFailureWithoutSecrets(t *testing.T) {
	var logs bytes.Buffer
	adapter := &stubAdapter{
		err: fmt.Errorf("%w: api-key=secret-value", exchange.ErrRejected),
	}
	store := newMemoryStore()
	service := NewService(
		store,
		stubCatalog{item: Instrument{
			ID: 7, Exchange: "binance", ContractType: "perpetual",
			ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
			PriceTick: "0.1", QuantityStep: "0.001", ContractSize: "1",
		}},
		stubCredentials{owner: "admin", item: Credentials{
			TradingAccountID: 3, ProductName: "Funding Arb", Exchange: "binance",
		}},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"binance": adapter}),
		time.Second,
		slog.New(slog.NewTextHandler(&logs, nil)),
	)
	order, err := service.PlaceOrder(context.Background(), PlaceOrderInput{
		Token: "tok", TradingAccountID: 3, InstrumentID: 7, Side: "buy",
		OrderType: "market", Quantity: "0.001", IdempotencyKey: "venue-error-key",
	})
	if !errors.Is(err, ErrVenueRejected) || order.Status != "rejected" {
		t.Fatalf("order=%+v err=%v", order, err)
	}
	output := logs.String()
	if !strings.Contains(output, "error_class=venue_rejected") ||
		strings.Contains(output, "secret-value") {
		t.Fatalf("log=%s", output)
	}
}

func TestGetOrderReconcilesOpenAndSkipsTerminal(t *testing.T) {
	adapter := &stubAdapter{query: exchange.Result{
		VenueOrderID: "v9", Status: "filled", FilledQuantity: "0.001", AveragePrice: "100.1",
	}}
	service, store := testService(adapter)
	_, _, err := store.CreateIntent(context.Background(), Order{
		ID: "ord-open", IdempotencyKey: "open-key", OwnerUsername: "admin",
		TradingAccountID: 3, InstrumentID: 7, Status: "open", ClientOrderID: "sqopen01",
	})
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	open := store.orders["ord-open"]
	open.Status = "open"
	store.orders["ord-open"] = open
	store.mu.Unlock()

	updated, err := service.GetOrder(context.Background(), "tok", "ord-open")
	if err != nil || updated.Status != "filled" || adapter.getCalls != 1 {
		t.Fatalf("updated=%+v calls=%d err=%v", updated, adapter.getCalls, err)
	}

	_, _, err = store.CreateIntent(context.Background(), Order{
		ID: "ord-done", IdempotencyKey: "done-key", OwnerUsername: "admin",
		TradingAccountID: 3, InstrumentID: 7, Status: "canceled",
	})
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	done := store.orders["ord-done"]
	done.Status = "canceled"
	store.orders["ord-done"] = done
	store.mu.Unlock()
	terminal, err := service.GetOrder(context.Background(), "tok", "ord-done")
	if err != nil || terminal.Status != "canceled" || adapter.getCalls != 1 {
		t.Fatalf("terminal=%+v calls=%d err=%v", terminal, adapter.getCalls, err)
	}
}

func TestGetOrderReturnsKnownOnVenueFailure(t *testing.T) {
	adapter := &stubAdapter{queryErr: errors.New("secret passphrase leaked")}
	service, store := testService(adapter)
	_, _, err := store.CreateIntent(context.Background(), Order{
		ID: "ord-live", IdempotencyKey: "live-key", OwnerUsername: "admin",
		TradingAccountID: 3, InstrumentID: 7, Status: "open",
	})
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	live := store.orders["ord-live"]
	live.Status = "open"
	store.orders["ord-live"] = live
	store.mu.Unlock()
	order, err := service.GetOrder(context.Background(), "tok", "ord-live")
	if err != nil || order.Status != "open" || adapter.getCalls != 1 {
		t.Fatalf("order=%+v calls=%d err=%v", order, adapter.getCalls, err)
	}
}
