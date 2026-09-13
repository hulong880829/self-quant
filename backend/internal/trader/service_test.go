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

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
)

type memoryStore struct {
	mu       sync.Mutex
	orders   map[string]Order
	keys     map[string]string
	events   []string
	deferred map[string]time.Time
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

func (m *memoryStore) getByIdempotencyKey(key string) (Order, bool) {
	if m == nil {
		return Order{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.keys[key]
	if !ok {
		return Order{}, false
	}
	return m.orders[id], true
}

func (m *memoryStore) snapshotOrders() []Order {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	items := make([]Order, 0, len(m.orders))
	for _, order := range m.orders {
		items = append(items, order)
	}
	return items
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

func (m *memoryStore) DeferReconcile(_ context.Context, orderID string, next time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deferred == nil {
		m.deferred = map[string]time.Time{}
	}
	m.deferred[orderID] = next
	return nil
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
	order.AbsenceConfirmations = 0
	order.UpdatedAt = time.Now().UTC()
	m.orders[orderID] = order
	return order, nil
}

func (m *memoryStore) AppendEvent(
	_ context.Context,
	_ string,
	eventType string,
	_ map[string]any,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, eventType)
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

func (s stubCredentials) Meta(ctx context.Context, token string, id int64) (AccountMeta, error) {
	item, err := s.Get(ctx, token, id)
	if err != nil {
		return AccountMeta{}, err
	}
	return AccountMeta{
		TradingAccountID: item.TradingAccountID, ProductName: item.ProductName,
		Exchange: item.Exchange, AccountName: item.AccountName, CredentialKind: item.CredentialKind,
		AccountIndex: item.AccountIndex, APIKeyIndex: item.APIKeyIndex,
	}, nil
}

type stubAdapter struct {
	place           exchange.Result
	placeResults    []exchange.Result
	query           exchange.Result
	err             error
	placeErrors     []error
	queryErr        error
	positionMode    string
	positionModeErr error
	profile         exchange.AccountProfileResult
	profileErr      error
	calls           int
	getCalls        int
	cancelCalls     int
	requests        []exchange.OrderRequest
	leverageSets    int
	leverageResult  exchange.LeverageApplyResult
	setLeverageErr  error
	bbo             exchange.BBO
	bboErr          error
	bboCalls        int
}

func (s *stubAdapter) GetBBO(context.Context, exchange.Instrument) (exchange.BBO, error) {
	s.bboCalls++
	if s.bboErr != nil {
		return exchange.BBO{}, s.bboErr
	}
	if strings.TrimSpace(s.bbo.BidPrice) != "" || strings.TrimSpace(s.bbo.AskPrice) != "" {
		return s.bbo, nil
	}
	return exchange.BBO{
		BidPrice: "99.9", AskPrice: "100.1", Timestamp: time.Now().UTC(),
	}, nil
}

func (s *stubAdapter) GetPositionMode(
	context.Context,
	exchange.Credentials,
	exchange.Instrument,
) (string, error) {
	if s.positionModeErr != nil {
		return "", s.positionModeErr
	}
	if s.positionMode != "" {
		return s.positionMode, nil
	}
	return exchange.PositionModeOneWay, nil
}

func (s *stubAdapter) ApplyAccountProfile(
	context.Context,
	exchange.Credentials,
	exchange.AccountProfileRequest,
) (exchange.AccountProfileResult, error) {
	return s.profile, s.profileErr
}

func (s *stubAdapter) PlaceOrder(
	_ context.Context,
	_ exchange.Credentials,
	request exchange.OrderRequest,
) (exchange.Result, error) {
	index := s.calls
	s.calls++
	s.requests = append(s.requests, request)
	if index < len(s.placeResults) {
		var err error
		if index < len(s.placeErrors) {
			err = s.placeErrors[index]
		}
		return s.placeResults[index], err
	}
	return s.place, s.err
}

func (s *stubAdapter) GetOrder(context.Context, exchange.Credentials, exchange.QueryRequest) (exchange.Result, error) {
	s.getCalls++
	if s.queryErr != nil {
		return s.query, s.queryErr
	}
	if s.query.Status != "" {
		return s.query, nil
	}
	return s.place, s.err
}

func (s *stubAdapter) CancelOrder(ctx context.Context, credentials exchange.Credentials, request exchange.CancelRequest) (exchange.Result, error) {
	return s.CancelAndGetOrder(ctx, credentials, request)
}

func (s *stubAdapter) CancelAndGetOrder(context.Context, exchange.Credentials, exchange.CancelRequest) (exchange.Result, error) {
	s.cancelCalls++
	return exchange.Result{Status: "canceled", VenueOrderID: "1"}, nil
}

func (s *stubAdapter) SetLeverage(
	context.Context,
	exchange.Credentials,
	exchange.Instrument,
	decimal.Decimal,
) (exchange.LeverageApplyResult, error) {
	s.leverageSets++
	if s.setLeverageErr != nil {
		return exchange.LeverageApplyResult{}, s.setLeverageErr
	}
	return s.leverageResult, nil
}

func testService(adapter *stubAdapter) (*Service, *memoryStore) {
	store := newMemoryStore()
	service := NewService(
		store,
		stubCatalog{item: testInstrument()},
		stubCredentials{owner: "admin", item: Credentials{
			TradingAccountID: 3, ProductName: "Funding Arb", Exchange: "binance", AccountName: "main",
		}},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"binance": adapter}),
		time.Second,
		nil,
	)
	return service, store
}

func testInstrument() Instrument {
	return Instrument{
		ID: 7, Exchange: "binance", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		PriceTick: "0.1", QuantityStep: "0.001", ContractSize: "1",
		MinQuantity: "0.001", MinQuantityStatus: exchange.ConstraintKnown,
		MaxQuantity: "1000", MaxQuantityStatus: exchange.ConstraintKnown,
		MinNotional: "0.01", MinNotionalStatus: exchange.ConstraintKnown,
		MarketQuantityStep:       "0.001",
		MarketQuantityStepStatus: exchange.ConstraintKnown,
		MarketMinQuantity:        "0.001",
		MarketMinQuantityStatus:  exchange.ConstraintKnown,
		MarketMaxQuantity:        "1000",
		MarketMaxQuantityStatus:  exchange.ConstraintKnown,
		MarketMinNotional:        "0.01",
		MarketMinNotionalStatus:  exchange.ConstraintKnown,
	}
}

func TestNormalizeAndValidateOrder(t *testing.T) {
	input, err := normalizePlaceInput(PlaceOrderInput{
		Token: "tok", TradingAccountID: 1, InstrumentID: 2, Side: "BUY",
		OrderType: "limit", Quantity: "0.010", Price: "100.0", IdempotencyKey: "k1",
	})
	if err != nil || input.Side != "buy" || input.Quantity != "0.01" {
		t.Fatalf("input=%+v err=%v", input, err)
	}
	rules := Instrument{
		QuantityStep: "0.001", PriceTick: "0.1",
		MinQuantityStatus: exchange.ConstraintNotApplicable,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotionalStatus: exchange.ConstraintNotApplicable,
	}
	if err := validateInstrumentRules(rules, "limit", "0.01", "100.0"); err != nil {
		t.Fatal(err)
	}
	if err := validateInstrumentRules(rules, "limit", "0.0015", "100.0"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err=%v", err)
	}
	if _, err := normalizePlaceInput(PlaceOrderInput{
		Token: "tok", TradingAccountID: 1, InstrumentID: 2, Side: "buy",
		OrderType: "market", Quantity: "1", Price: "10", IdempotencyKey: "k1",
	}); err != ErrInvalidArgument {
		t.Fatalf("err=%v", err)
	}
}

func TestPrepareOrderUsesOrderTypeSpecificRules(t *testing.T) {
	instrument := testInstrument()
	instrument.QuantityStep = "0.01"
	instrument.MinQuantity = "0.01"
	instrument.MaxQuantity = "10"
	instrument.MarketQuantityStep = "0.1"
	instrument.MarketMinQuantity = "0.2"
	instrument.MarketMaxQuantity = "2"
	instrument.MarketMinNotional = "5"

	quantity, price, err := prepareOrder(
		instrument, "market", "0.2", "", "25",
	)
	if err != nil || quantity != "0.2" || price != "" {
		t.Fatalf("quantity=%q price=%q err=%v", quantity, price, err)
	}
	if _, _, err := prepareOrder(
		instrument, "market", "0.21", "", "25",
	); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("market step err=%v", err)
	}
	instrument.MarketMinQuantity = "0.3"
	if _, _, err := prepareOrder(
		instrument, "market", "0.2", "", "25",
	); !errors.Is(err, ErrInvalidArgument) || !errors.Is(err, ErrOrderBelowMinimum) {
		t.Fatalf("market minimum quantity err=%v", err)
	}
	instrument.MarketMinQuantity = "0.2"
	if _, _, err := prepareOrder(
		instrument, "market", "0.2", "", "20",
	); !errors.Is(err, ErrInvalidArgument) || !errors.Is(err, ErrOrderBelowMinimum) {
		t.Fatalf("market notional err=%v", err)
	}
	instrument.MarketMaxQuantityStatus = exchange.ConstraintUnknown
	if _, _, err := prepareOrder(
		instrument, "market", "0.2", "", "25",
	); err != ErrInstrumentUnavailable {
		t.Fatalf("unknown rule err=%v", err)
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
		stubCatalog{item: testInstrument()},
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
		stubCatalog{item: testInstrument()},
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

func TestPlaceOrderPersistsOriginalVenueError(t *testing.T) {
	adapter := &stubAdapter{
		place: exchange.Result{
			Status:       "rejected",
			ErrorCode:    "51008",
			ErrorMessage: "Order failed. Insufficient account balance.",
		},
		err: exchange.ErrRejected,
	}
	service, _ := testService(adapter)
	order, err := service.PlaceOrder(context.Background(), PlaceOrderInput{
		Token: "tok", TradingAccountID: 3, InstrumentID: 7, Side: "buy",
		OrderType: "market", Quantity: "0.001", IdempotencyKey: "original-venue-error",
	})
	if !errors.Is(err, ErrVenueRejected) {
		t.Fatalf("err=%v", err)
	}
	if order.ErrorCode != "51008" ||
		order.ErrorMessage != "Order failed. Insufficient account balance." {
		t.Fatalf("order=%+v", order)
	}
}

func TestPlaceOrderPersistsRejectedWithoutVenueOrderIDAsTerminal(t *testing.T) {
	adapter := &stubAdapter{
		place: exchange.Result{
			Status: "rejected", FilledQuantity: "0",
			ErrorMessage: "validation error: field=Price code=significant_figures",
		},
		err: exchange.ErrRejected,
	}
	service, _ := testService(adapter)
	order, err := service.PlaceOrder(context.Background(), PlaceOrderInput{
		Token: "tok", TradingAccountID: 3, InstrumentID: 7, Side: "buy",
		OrderType: "limit", Quantity: "0.001", Price: "100",
		IdempotencyKey: "hyperliquid-validation-reject",
	})
	if !errors.Is(err, ErrVenueRejected) ||
		order.Status != "rejected" ||
		order.FilledQuantity != "0" ||
		order.VenueOrderID != "" ||
		order.ErrorCode != "venue_rejected" ||
		order.ErrorMessage != "validation error: field=Price code=significant_figures" {
		t.Fatalf("order=%+v err=%v", order, err)
	}
}

func TestPlaceOrderRejectedResultWritesRejectEvent(t *testing.T) {
	adapter := &stubAdapter{
		place: exchange.Result{
			Status:       "rejected",
			ErrorCode:    "ORDER_POC_IMMEDIATE",
			ErrorMessage: "order price 0.143 while counter price 0.142",
			Raw:          map[string]any{"label": "ORDER_POC_IMMEDIATE"},
		},
		err: exchange.ErrRejected,
	}
	service, store := testService(adapter)
	order, err := service.PlaceOrder(context.Background(), PlaceOrderInput{
		Token: "tok", TradingAccountID: 3, InstrumentID: 7, Side: "buy",
		OrderType: "limit", Quantity: "0.001", Price: "100",
		IdempotencyKey: "post-only-reject-audit",
	})
	if !errors.Is(err, ErrVenueRejected) ||
		order.Status != "rejected" ||
		order.ErrorCode != "ORDER_POC_IMMEDIATE" ||
		order.ErrorMessage != "order price 0.143 while counter price 0.142" {
		t.Fatalf("order=%+v err=%v", order, err)
	}
	store.mu.Lock()
	events := append([]string(nil), store.events...)
	store.mu.Unlock()
	if len(events) != 2 || events[0] != "submitted" || events[1] != "reject" {
		t.Fatalf("events=%v, want [submitted reject]", events)
	}
}

func TestPlaceOrderAsterCapacityLimitPersistsRejectedWithoutRecover(t *testing.T) {
	adapter := &stubAdapter{
		place: exchange.Result{
			Status:         "rejected",
			ErrorCode:      "-5018",
			FilledQuantity: "0",
			ErrorMessage:   "ReduceOnly Order is rejected.",
		},
		err: exchange.ErrRejected,
	}
	service, store := testService(adapter)
	order, err := service.PlaceOrder(context.Background(), PlaceOrderInput{
		Token: "tok", TradingAccountID: 3, InstrumentID: 7, Side: "buy",
		OrderType: "market", Quantity: "0.001", IdempotencyKey: "aster-5018-reject",
	})
	if !errors.Is(err, ErrVenueRejected) {
		t.Fatalf("err=%v", err)
	}
	if order.Status != "rejected" ||
		order.FilledQuantity != "0" ||
		order.ErrorCode != "-5018" ||
		order.ErrorMessage != "ReduceOnly Order is rejected." ||
		order.VenueOrderID != "" {
		t.Fatalf("order=%+v", order)
	}
	if adapter.getCalls != 0 || adapter.cancelCalls != 0 || adapter.calls != 1 {
		t.Fatalf("place=%d get=%d cancel=%d", adapter.calls, adapter.getCalls, adapter.cancelCalls)
	}
	store.mu.Lock()
	events := append([]string(nil), store.events...)
	store.mu.Unlock()
	if len(events) != 2 || events[0] != "submitted" || events[1] != "reject" {
		t.Fatalf("events=%v, want [submitted reject]", events)
	}
	for _, eventType := range events {
		if eventType == "reconcile_absent" || eventType == "reconcile_uncertain" {
			t.Fatalf("events=%v", events)
		}
	}
	if order.ErrorCode == errorConfirmedAbsentAfterUncertainSubmit {
		t.Fatalf("order=%+v", order)
	}
}

func TestGetOrderQueryRejectedDoesNotTerminalize(t *testing.T) {
	adapter := &stubAdapter{
		query: exchange.Result{
			Status: "rejected", ErrorCode: "51000", ErrorMessage: "Parameter sz error",
		},
		queryErr: exchange.ErrRejected,
	}
	service, store := testService(adapter)
	order, _, err := store.CreateIntent(context.Background(), Order{
		ID: "ord-rejected-query", IdempotencyKey: "query-reject-key",
		OwnerUsername: "admin", TradingAccountID: 3, InstrumentID: 7,
		Status: "open", ClientOrderID: "sqqueryreject",
	})
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	order.Status = "open"
	store.orders[order.ID] = order
	store.mu.Unlock()
	updated, err := service.GetOrder(context.Background(), "tok", order.ID)
	if !errors.Is(err, ErrVenueRejected) {
		t.Fatalf("err=%v", err)
	}
	if updated.Status != "unknown" || updated.ErrorCode != "51000" {
		t.Fatalf("updated=%+v", updated)
	}
}

func TestGetOrderQueryNotFoundDoesNotReject(t *testing.T) {
	adapter := &stubAdapter{
		query:    exchange.Result{Status: "unknown"},
		queryErr: exchange.ErrOrderNotFound,
	}
	service, store := testService(adapter)
	order, _, err := store.CreateIntent(context.Background(), Order{
		ID: "ord-not-found-query", IdempotencyKey: "query-not-found-key",
		OwnerUsername: "admin", TradingAccountID: 3, InstrumentID: 7,
		Status: "open", ClientOrderID: "sqquerynotfound",
	})
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	order.Status = "open"
	store.orders[order.ID] = order
	store.mu.Unlock()
	updated, err := service.GetOrder(context.Background(), "tok", order.ID)
	if !errors.Is(err, ErrVenueRejected) {
		t.Fatalf("err=%v", err)
	}
	if updated.Status == "rejected" {
		t.Fatalf("updated=%+v", updated)
	}
}

func TestPlaceOrderRecoverQueryRejectedDoesNotTerminalize(t *testing.T) {
	adapter := &stubAdapter{
		place:    exchange.Result{Status: "unknown", ErrorCode: "timeout"},
		err:      exchange.ErrUncertain,
		query:    exchange.Result{Status: "rejected", ErrorCode: "51000"},
		queryErr: exchange.ErrRejected,
	}
	service, _ := testService(adapter)
	order, err := service.PlaceOrder(context.Background(), PlaceOrderInput{
		Token: "tok", TradingAccountID: 3, InstrumentID: 7, Side: "buy",
		OrderType: "limit", Quantity: "0.001", Price: "100",
		IdempotencyKey: "recover-query-reject",
	})
	if err == nil || order.Status == "rejected" {
		t.Fatalf("order=%+v err=%v", order, err)
	}
}

func TestPlaceOrderRecoverQueryNotFoundDoesNotReject(t *testing.T) {
	adapter := &stubAdapter{
		place:    exchange.Result{Status: "unknown"},
		err:      exchange.ErrUncertain,
		query:    exchange.Result{Status: "unknown"},
		queryErr: exchange.ErrOrderNotFound,
	}
	service, _ := testService(adapter)
	order, err := service.PlaceOrder(context.Background(), PlaceOrderInput{
		Token: "tok", TradingAccountID: 3, InstrumentID: 7, Side: "buy",
		OrderType: "limit", Quantity: "0.001", Price: "100",
		IdempotencyKey: "recover-query-absent",
	})
	if err == nil || order.Status == "rejected" {
		t.Fatalf("order=%+v err=%v", order, err)
	}
}

func TestGetOrderPersistsPartialUncertainResultAsUnknown(t *testing.T) {
	adapter := &stubAdapter{
		query: exchange.Result{
			Status:       "rejected",
			VenueOrderID: "venue-42",
			ErrorCode:    "40010",
			ErrorMessage: "request timed out",
		},
		queryErr: exchange.ErrUncertain,
	}
	service, store := testService(adapter)
	order, _, err := store.CreateIntent(context.Background(), Order{
		ID: "ord-uncertain-query", IdempotencyKey: "query-uncertain-key",
		OwnerUsername: "admin", TradingAccountID: 3, InstrumentID: 7,
		Status: "open", ClientOrderID: "sqqueryuncertain",
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := service.GetOrder(context.Background(), "tok", order.ID)
	if !errors.Is(err, ErrVenueUncertain) {
		t.Fatalf("err=%v", err)
	}
	if updated.Status != "unknown" || updated.VenueOrderID != "venue-42" ||
		updated.ErrorCode != "40010" || updated.ErrorMessage != "request timed out" {
		t.Fatalf("order=%+v", updated)
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

func TestGetOrderReturnsKnownStateAndPropagatesVenueFailure(t *testing.T) {
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
	if !errors.Is(err, ErrVenueUncertain) || order.Status != "open" || adapter.getCalls != 1 {
		t.Fatalf("order=%+v calls=%d err=%v", order, adapter.getCalls, err)
	}
}

type metaOnlyCredentials struct {
	owner string
	meta  AccountMeta
	gets  int
}

func (c *metaOnlyCredentials) Owner(context.Context, string) (string, error) {
	return c.owner, nil
}

func (c *metaOnlyCredentials) Meta(context.Context, string, int64) (AccountMeta, error) {
	return c.meta, nil
}

func (c *metaOnlyCredentials) Get(context.Context, string, int64) (Credentials, error) {
	c.gets++
	return Credentials{}, errors.New("private key must not be loaded")
}

func TestListInstrumentsDoesNotLoadPrivateKeys(t *testing.T) {
	credentials := &metaOnlyCredentials{
		owner: "admin",
		meta:  AccountMeta{TradingAccountID: 24, Exchange: "hyperliquid", AccountName: "hulong-hy"},
	}
	service := NewService(
		newMemoryStore(),
		stubCatalog{item: Instrument{
			ID: 9, Exchange: "hyperliquid", ContractType: "perpetual",
			ExchangeSymbol: "BTC", BaseAsset: "BTC", QuoteAsset: "USDC",
		}},
		credentials,
		exchange.NewTestRegistry(map[string]exchange.Adapter{}),
		time.Second,
		nil,
	)
	items, err := service.ListInstruments(context.Background(), "tok", 24, "perpetual")
	if err != nil || len(items) != 1 || items[0].Exchange != "hyperliquid" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if credentials.gets != 0 {
		t.Fatal("ListInstruments loaded private keys")
	}
	capabilities, err := service.GetVenueCapabilities(context.Background(), "tok", 24)
	if err != nil || len(capabilities.Products) != 1 || capabilities.Products[0] != "perpetual" {
		t.Fatalf("capabilities=%+v err=%v", capabilities, err)
	}
	if credentials.gets != 0 {
		t.Fatal("GetVenueCapabilities loaded private keys")
	}
}
