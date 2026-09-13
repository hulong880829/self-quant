package trader

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"selfquant/backend/internal/trader/exchange"
)

type reconcileMemoryStore struct {
	orders         []Order
	updates        int
	recomputes     int
	failures       int
	lastResult     VenueResult
	finalizeCalls  int
	lastFinalizeID string
}

func (s *reconcileMemoryStore) LeaseDueOrders(context.Context, int, time.Duration) ([]Order, error) {
	return s.orders, nil
}
func (s *reconcileMemoryStore) MarkReconcileFailure(_ context.Context, id string, _ time.Time) error {
	s.failures++
	for index, order := range s.orders {
		if order.ID != id {
			continue
		}
		order.AbsenceConfirmations = 0
		s.orders[index] = order
		return nil
	}
	return nil
}
func (s *reconcileMemoryStore) ConfirmOrderAbsence(
	_ context.Context, id string, expectedUpdatedAt time.Time, _ time.Time,
) (Order, bool, error) {
	for index, order := range s.orders {
		if order.ID != id {
			continue
		}
		if !order.UpdatedAt.Equal(expectedUpdatedAt) {
			return order, false, nil
		}
		if strings.TrimSpace(order.VenueOrderID) != "" {
			return order, false, nil
		}
		filled := strings.TrimSpace(order.FilledQuantity)
		if filled != "" && filled != "0" {
			return order, false, nil
		}
		order.AbsenceConfirmations++
		converted := order.AbsenceConfirmations >= confirmedAbsentRejectAfter
		if converted {
			order.Status = "rejected"
			order.FilledQuantity = "0"
			order.ErrorCode = errorConfirmedAbsentAfterUncertainSubmit
			order.ReconcileFailures = 0
			s.updates++
			s.lastResult = VenueResult{
				Status: "rejected", FilledQuantity: "0",
				ErrorCode: errorConfirmedAbsentAfterUncertainSubmit,
			}
		}
		s.orders[index] = order
		return order, converted, nil
	}
	return Order{}, false, nil
}
func (s *reconcileMemoryStore) DeferReconcile(context.Context, string, time.Time) error {
	return nil
}
func (s *reconcileMemoryStore) UpdateResultWithFillDelta(
	_ context.Context, id string, result VenueResult,
) (Order, bool, error) {
	s.updates++
	s.lastResult = result
	for _, order := range s.orders {
		if order.ID == id {
			filledChanged := !parseDecimal(order.FilledQuantity).Equal(parseDecimal(result.FilledQuantity))
			return order, filledChanged, nil
		}
	}
	return Order{}, false, nil
}

type uncertainReconcileAdapter struct{}

func (uncertainReconcileAdapter) GetBBO(
	context.Context, exchange.Instrument,
) (exchange.BBO, error) {
	return exchange.BBO{}, nil
}
func (uncertainReconcileAdapter) PlaceOrder(
	context.Context, exchange.Credentials, exchange.OrderRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}
func (uncertainReconcileAdapter) GetOrder(
	context.Context, exchange.Credentials, exchange.QueryRequest,
) (exchange.Result, error) {
	return exchange.Result{
		VenueOrderID: "venue-42", Status: "rejected",
		FilledQuantity: "0.4", AveragePrice: "100",
		ErrorCode: "40010", ErrorMessage: "timeout",
	}, exchange.ErrUncertain
}
func (uncertainReconcileAdapter) CancelOrder(
	context.Context, exchange.Credentials, exchange.CancelRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}
func (uncertainReconcileAdapter) CancelAndGetOrder(
	ctx context.Context, credentials exchange.Credentials, request exchange.CancelRequest,
) (exchange.Result, error) {
	return uncertainReconcileAdapter{}.CancelOrder(ctx, credentials, request)
}
func (s *reconcileMemoryStore) AppendEvent(context.Context, string, string, map[string]any) error {
	return nil
}
func (s *reconcileMemoryStore) RecomputeArbitrageBasePositionsForExecution(
	context.Context, string,
) (ArbitrageCombination, error) {
	s.recomputes++
	return ArbitrageCombination{}, nil
}

func (s *reconcileMemoryStore) FinalizeConfirmedAbsentZeroFillExecution(
	_ context.Context, executionID string,
) (confirmedAbsentFinalizeResult, error) {
	s.finalizeCalls++
	s.lastFinalizeID = executionID
	return confirmedAbsentFinalizeSkipped, nil
}

type reconcileCredentials struct {
	mu    sync.Mutex
	calls int
}

func (p *reconcileCredentials) GetInternal(context.Context, string, string, int64) (Credentials, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return Credentials{Exchange: "binance", APIKey: "key", APISecret: "secret"}, nil
}

type reconcileCatalog struct{ instrument Instrument }

func (c reconcileCatalog) List(context.Context, string, string) ([]Instrument, error) {
	return []Instrument{c.instrument}, nil
}
func (c reconcileCatalog) Get(context.Context, int64) (Instrument, error) {
	return c.instrument, nil
}

func TestReconcilerReusesCredentialsPerAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"orderId":11,"status":"FILLED","executedQty":"1","avgPrice":"100"}`))
	}))
	defer server.Close()
	instrument := Instrument{
		ID: 7, Exchange: "binance", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
	}
	store := &reconcileMemoryStore{orders: []Order{
		{ID: "one", OwnerUsername: "alice", TradingAccountID: 3, Exchange: "binance", InstrumentID: 7, Quantity: "1", ArbitrageExecutionID: "exec-1"},
		{ID: "two", OwnerUsername: "alice", TradingAccountID: 3, Exchange: "binance", InstrumentID: 7, Quantity: "1"},
	}}
	provider := &reconcileCredentials{}
	registry := exchange.NewRegistry(server.Client(), map[string]string{"binance": server.URL})
	reconciler := NewReconciler(
		store, reconcileCatalog{instrument}, provider, registry, "token",
		time.Second, time.Second, 20, 2, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	reconciler.runOnce(context.Background())
	if provider.calls != 1 {
		t.Fatalf("credential calls=%d", provider.calls)
	}
	if store.updates != 2 {
		t.Fatalf("updates=%d", store.updates)
	}
	if store.recomputes != 1 {
		t.Fatalf("recomputes=%d", store.recomputes)
	}
}

func TestReconcilerDoesNotPlaceOrders(t *testing.T) {
	instrument := Instrument{
		ID: 7, Exchange: "binance", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
	}
	store := &reconcileMemoryStore{orders: []Order{{
		ID: "one", OwnerUsername: "alice", TradingAccountID: 3,
		Exchange: "binance", InstrumentID: 7, Quantity: "1",
		ArbitrageExecutionID: "exec-1",
	}}}
	adapter := &countingPlaceReconcileAdapter{Adapter: uncertainReconcileAdapter{}}
	reconciler := NewReconciler(
		store, reconcileCatalog{instrument}, &reconcileCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{"binance": adapter}),
		"token", time.Second, time.Second, 20, 1,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	reconciler.runOnce(context.Background())
	if adapter.places != 0 {
		t.Fatalf("PlaceOrder calls=%d", adapter.places)
	}
	if store.recomputes != 1 {
		t.Fatalf("recomputes=%d", store.recomputes)
	}
}

type countingPlaceReconcileAdapter struct {
	exchange.Adapter
	places int
}

func (a *countingPlaceReconcileAdapter) PlaceOrder(
	context.Context, exchange.Credentials, exchange.OrderRequest,
) (exchange.Result, error) {
	a.places++
	return a.Adapter.PlaceOrder(context.Background(), exchange.Credentials{}, exchange.OrderRequest{})
}

func TestReconcilerPersistsPartialUncertainResultBeforeRetry(t *testing.T) {
	instrument := Instrument{
		ID: 7, Exchange: "binance", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
	}
	store := &reconcileMemoryStore{orders: []Order{{
		ID: "one", OwnerUsername: "alice", TradingAccountID: 3,
		Exchange: "binance", InstrumentID: 7, Quantity: "1",
		ArbitrageExecutionID: "exec-1",
	}}}
	reconciler := NewReconciler(
		store, reconcileCatalog{instrument}, &reconcileCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"binance": uncertainReconcileAdapter{},
		}),
		"token", time.Second, time.Second, 20, 1,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	reconciler.runOnce(context.Background())
	if store.updates != 1 || store.failures != 1 || store.recomputes != 1 {
		t.Fatalf(
			"updates=%d failures=%d recomputes=%d",
			store.updates, store.failures, store.recomputes,
		)
	}
	if store.lastResult.Status != "unknown" ||
		store.lastResult.FilledQuantity != "0.4" ||
		store.lastResult.VenueOrderID != "venue-42" {
		t.Fatalf("result=%+v", store.lastResult)
	}
}

type confirmedAbsentReconcileAdapter struct{}

func (confirmedAbsentReconcileAdapter) GetBBO(
	context.Context, exchange.Instrument,
) (exchange.BBO, error) {
	return exchange.BBO{}, nil
}
func (confirmedAbsentReconcileAdapter) PlaceOrder(
	context.Context, exchange.Credentials, exchange.OrderRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}
func (confirmedAbsentReconcileAdapter) GetOrder(
	context.Context, exchange.Credentials, exchange.QueryRequest,
) (exchange.Result, error) {
	return exchange.Result{}, exchange.ErrOrderNotFound
}
func (confirmedAbsentReconcileAdapter) CancelOrder(
	context.Context, exchange.Credentials, exchange.CancelRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}
func (confirmedAbsentReconcileAdapter) CancelAndGetOrder(
	ctx context.Context, credentials exchange.Credentials, request exchange.CancelRequest,
) (exchange.Result, error) {
	return confirmedAbsentReconcileAdapter{}.CancelOrder(ctx, credentials, request)
}
func (confirmedAbsentReconcileAdapter) ResolveOrder(
	context.Context, exchange.Credentials, exchange.QueryRequest,
) (exchange.OrderResolution, error) {
	return exchange.OrderResolution{ConfirmedAbsent: true}, nil
}

func runConfirmedAbsentReconcile(t *testing.T, order Order) *reconcileMemoryStore {
	t.Helper()
	instrument := Instrument{
		ID: 7, Exchange: "binance", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
	}
	store := &reconcileMemoryStore{orders: []Order{order}}
	reconciler := NewReconciler(
		store, reconcileCatalog{instrument}, &reconcileCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"binance": confirmedAbsentReconcileAdapter{},
		}),
		"token", time.Second, time.Second, 20, 1,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	reconciler.runOnce(context.Background())
	return store
}

func TestReconcilerConfirmedAbsentRejectsUncertainSubmitAfterRetries(t *testing.T) {
	base := Order{
		ID: "ghost", OwnerUsername: "alice", TradingAccountID: 3,
		Exchange: "binance", InstrumentID: 7, Quantity: "1",
		Status: "pending", FilledQuantity: "0", ArbitrageExecutionID: "exec-1",
	}
	first := runConfirmedAbsentReconcile(t, base)
	if first.failures != 0 || first.updates != 0 || first.finalizeCalls != 0 {
		t.Fatalf("first updates=%d failures=%d finalize=%d", first.updates, first.failures, first.finalizeCalls)
	}
	second := base
	second.AbsenceConfirmations = 1
	mid := runConfirmedAbsentReconcile(t, second)
	if mid.failures != 0 || mid.updates != 0 {
		t.Fatalf("second updates=%d failures=%d", mid.updates, mid.failures)
	}
	third := base
	third.AbsenceConfirmations = confirmedAbsentRejectAfter - 1
	done := runConfirmedAbsentReconcile(t, third)
	if done.failures != 0 || done.updates != 1 {
		t.Fatalf("third updates=%d failures=%d", done.updates, done.failures)
	}
	if done.finalizeCalls != 1 || done.lastFinalizeID != "exec-1" {
		t.Fatalf("finalizeCalls=%d id=%s", done.finalizeCalls, done.lastFinalizeID)
	}
	if done.recomputes != 0 {
		t.Fatalf("confirmed-absent zero fill should not recompute: recomputes=%d", done.recomputes)
	}
	if done.lastResult.Status != "rejected" ||
		done.lastResult.FilledQuantity != "0" ||
		done.lastResult.ErrorCode != errorConfirmedAbsentAfterUncertainSubmit {
		t.Fatalf("result=%+v", done.lastResult)
	}
}

func TestReconcilerConfirmedAbsentDoesNotRejectKnownOrders(t *testing.T) {
	base := Order{
		ID: "known", OwnerUsername: "alice", TradingAccountID: 3,
		Exchange: "binance", InstrumentID: 7, Quantity: "1",
		Status: "pending", AbsenceConfirmations: confirmedAbsentRejectAfter - 1,
		ArbitrageExecutionID: "exec-1",
	}
	withVenue := base
	withVenue.VenueOrderID = "oid-9"
	venue := runConfirmedAbsentReconcile(t, withVenue)
	if venue.failures != 1 || venue.updates != 0 {
		t.Fatalf("venue id updates=%d failures=%d result=%+v", venue.updates, venue.failures, venue.lastResult)
	}
	withFill := base
	withFill.FilledQuantity = "0.4"
	filledQty := runConfirmedAbsentReconcile(t, withFill)
	if filledQty.failures != 0 || filledQty.updates != 0 {
		t.Fatalf("fill updates=%d failures=%d result=%+v", filledQty.updates, filledQty.failures, filledQty.lastResult)
	}
	terminal := base
	terminal.Status = "filled"
	terminal.FilledQuantity = "1"
	kept := runConfirmedAbsentReconcile(t, terminal)
	if kept.failures != 0 || kept.updates != 1 || kept.finalizeCalls != 0 {
		t.Fatalf("filled updates=%d failures=%d finalize=%d", kept.updates, kept.failures, kept.finalizeCalls)
	}
	if kept.lastResult.Status != "filled" || kept.lastResult.FilledQuantity != "1" {
		t.Fatalf("filled result=%+v", kept.lastResult)
	}
}

type filledReconcileAdapter struct {
	status string
	filled string
}

func (filledReconcileAdapter) GetBBO(
	context.Context, exchange.Instrument,
) (exchange.BBO, error) {
	return exchange.BBO{}, nil
}
func (filledReconcileAdapter) PlaceOrder(
	context.Context, exchange.Credentials, exchange.OrderRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}
func (a filledReconcileAdapter) GetOrder(
	context.Context, exchange.Credentials, exchange.QueryRequest,
) (exchange.Result, error) {
	return exchange.Result{
		VenueOrderID: "venue-42", Status: a.status,
		FilledQuantity: a.filled, AveragePrice: "100",
	}, nil
}
func (filledReconcileAdapter) CancelOrder(
	context.Context, exchange.Credentials, exchange.CancelRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}
func (filledReconcileAdapter) CancelAndGetOrder(
	ctx context.Context, credentials exchange.Credentials, request exchange.CancelRequest,
) (exchange.Result, error) {
	return filledReconcileAdapter{}.CancelOrder(ctx, credentials, request)
}

func TestReconcilerRecomputesOnlyWhenFilledChanges(t *testing.T) {
	instrument := Instrument{
		ID: 7, Exchange: "binance", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
	}
	unchanged := &reconcileMemoryStore{orders: []Order{{
		ID: "one", OwnerUsername: "alice", TradingAccountID: 3,
		Exchange: "binance", InstrumentID: 7, Quantity: "1",
		Status: "partially_filled", FilledQuantity: "0.4",
		ArbitrageExecutionID: "exec-1",
	}}}
	NewReconciler(
		unchanged, reconcileCatalog{instrument}, &reconcileCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"binance": filledReconcileAdapter{status: "filled", filled: "0.4"},
		}),
		"token", time.Second, time.Second, 20, 1,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	).runOnce(context.Background())
	if unchanged.updates != 1 || unchanged.recomputes != 0 {
		t.Fatalf("unchanged updates=%d recomputes=%d", unchanged.updates, unchanged.recomputes)
	}

	changed := &reconcileMemoryStore{orders: []Order{{
		ID: "one", OwnerUsername: "alice", TradingAccountID: 3,
		Exchange: "binance", InstrumentID: 7, Quantity: "1",
		Status: "partially_filled", FilledQuantity: "0.4",
		ArbitrageExecutionID: "exec-1",
	}}}
	NewReconciler(
		changed, reconcileCatalog{instrument}, &reconcileCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"binance": filledReconcileAdapter{status: "filled", filled: "0.5"},
		}),
		"token", time.Second, time.Second, 20, 1,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	).runOnce(context.Background())
	if changed.updates != 1 || changed.recomputes != 1 {
		t.Fatalf("changed updates=%d recomputes=%d", changed.updates, changed.recomputes)
	}
}

type timeoutReconcileAdapter struct{}

func (timeoutReconcileAdapter) GetBBO(context.Context, exchange.Instrument) (exchange.BBO, error) {
	return exchange.BBO{}, nil
}
func (timeoutReconcileAdapter) PlaceOrder(
	context.Context, exchange.Credentials, exchange.OrderRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}
func (timeoutReconcileAdapter) GetOrder(
	context.Context, exchange.Credentials, exchange.QueryRequest,
) (exchange.Result, error) {
	return exchange.Result{}, exchange.ErrUncertain
}
func (timeoutReconcileAdapter) CancelOrder(
	context.Context, exchange.Credentials, exchange.CancelRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}
func (timeoutReconcileAdapter) CancelAndGetOrder(
	ctx context.Context, credentials exchange.Credentials, request exchange.CancelRequest,
) (exchange.Result, error) {
	return timeoutReconcileAdapter{}.CancelOrder(ctx, credentials, request)
}

type wssDuringRESTAdapter struct {
	store *reconcileMemoryStore
}

func (wssDuringRESTAdapter) GetBBO(context.Context, exchange.Instrument) (exchange.BBO, error) {
	return exchange.BBO{}, nil
}
func (wssDuringRESTAdapter) PlaceOrder(
	context.Context, exchange.Credentials, exchange.OrderRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}
func (a wssDuringRESTAdapter) GetOrder(
	context.Context, exchange.Credentials, exchange.QueryRequest,
) (exchange.Result, error) {
	if len(a.store.orders) > 0 {
		order := a.store.orders[0]
		order.Status = "filled"
		order.FilledQuantity = "1"
		order.UpdatedAt = order.UpdatedAt.Add(time.Second)
		a.store.orders[0] = order
	}
	return exchange.Result{}, exchange.ErrOrderNotFound
}
func (wssDuringRESTAdapter) CancelOrder(
	context.Context, exchange.Credentials, exchange.CancelRequest,
) (exchange.Result, error) {
	return exchange.Result{}, nil
}
func (wssDuringRESTAdapter) CancelAndGetOrder(
	ctx context.Context, credentials exchange.Credentials, request exchange.CancelRequest,
) (exchange.Result, error) {
	return wssDuringRESTAdapter{}.CancelOrder(ctx, credentials, request)
}
func (wssDuringRESTAdapter) ResolveOrder(
	context.Context, exchange.Credentials, exchange.QueryRequest,
) (exchange.OrderResolution, error) {
	return exchange.OrderResolution{ConfirmedAbsent: true}, nil
}

func TestReconcilerTimeoutResetsAbsenceThenDoesNotReject(t *testing.T) {
	instrument := Instrument{
		ID: 7, Exchange: "binance", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
	}
	store := &reconcileMemoryStore{orders: []Order{{
		ID: "ghost", OwnerUsername: "alice", TradingAccountID: 3,
		Exchange: "binance", InstrumentID: 7, Quantity: "1",
		Status: "pending", FilledQuantity: "0", AbsenceConfirmations: 2,
	}}}
	timeoutReconciler := NewReconciler(
		store, reconcileCatalog{instrument}, &reconcileCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"binance": timeoutReconcileAdapter{},
		}),
		"token", time.Second, time.Second, 20, 1,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	timeoutReconciler.runOnce(context.Background())
	if store.failures != 1 || store.orders[0].AbsenceConfirmations != 0 ||
		store.orders[0].Status != "pending" {
		t.Fatalf("timeout order=%+v failures=%d", store.orders[0], store.failures)
	}
	absentReconciler := NewReconciler(
		store, reconcileCatalog{instrument}, &reconcileCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"binance": confirmedAbsentReconcileAdapter{},
		}),
		"token", time.Second, time.Second, 20, 1,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	absentReconciler.runOnce(context.Background())
	absentReconciler.runOnce(context.Background())
	if store.orders[0].AbsenceConfirmations != 2 || store.orders[0].Status != "pending" ||
		store.updates != 0 {
		t.Fatalf("after two absences order=%+v updates=%d", store.orders[0], store.updates)
	}
}

func TestReconcilerDoesNotCountAbsenceWhenStreamFillsFirst(t *testing.T) {
	instrument := Instrument{
		ID: 7, Exchange: "binance", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
	}
	updatedAt := time.Unix(1_700_000_000, 0).UTC()
	store := &reconcileMemoryStore{orders: []Order{{
		ID: "ghost", OwnerUsername: "alice", TradingAccountID: 3,
		Exchange: "binance", InstrumentID: 7, Quantity: "1",
		Status: "pending", FilledQuantity: "0", AbsenceConfirmations: 2,
		UpdatedAt: updatedAt,
	}}}
	reconciler := NewReconciler(
		store, reconcileCatalog{instrument}, &reconcileCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"binance": wssDuringRESTAdapter{store: store},
		}),
		"token", time.Second, time.Second, 20, 1,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	reconciler.runOnce(context.Background())
	if store.orders[0].AbsenceConfirmations != 2 || store.orders[0].Status != "filled" ||
		store.updates != 0 {
		t.Fatalf("order=%+v updates=%d", store.orders[0], store.updates)
	}
}
