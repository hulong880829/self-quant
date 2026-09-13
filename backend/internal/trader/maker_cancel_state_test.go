package trader

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/orderstream"
)

type countingCancelAdapter struct {
	mu       sync.Mutex
	calls    int
	gets     int
	places   int
	result   exchange.Result
	err      error
	place    exchange.Result
	placeErr error
	allowGet bool
	get      exchange.Result
	getErr   error
	liveGet  exchange.Result
	onCancel func()
}

func (a *countingCancelAdapter) GetBBO(context.Context, exchange.Instrument) (exchange.BBO, error) {
	return exchange.BBO{}, nil
}

func (a *countingCancelAdapter) PlaceOrder(
	context.Context, exchange.Credentials, exchange.OrderRequest,
) (exchange.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.places++
	if a.placeErr != nil || a.place.Status != "" {
		return a.place, a.placeErr
	}
	return exchange.Result{}, errors.New("PlaceOrder must not run")
}

func (a *countingCancelAdapter) GetOrder(
	context.Context, exchange.Credentials, exchange.QueryRequest,
) (exchange.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gets++
	if a.getErr != nil {
		return exchange.Result{}, a.getErr
	}
	if a.allowGet {
		if a.calls == 0 && a.liveGet.Status != "" {
			return a.liveGet, nil
		}
		if a.get.Status != "" {
			return a.get, nil
		}
		return exchange.Result{Status: "open", FilledQuantity: "0"}, nil
	}
	return exchange.Result{}, errors.New("GetOrder must not be used on the cancel path")
}

func (a *countingCancelAdapter) CancelOrder(
	ctx context.Context, credentials exchange.Credentials, request exchange.CancelRequest,
) (exchange.Result, error) {
	a.mu.Lock()
	a.calls++
	onCancel := a.onCancel
	result, err := a.result, a.err
	a.mu.Unlock()
	if onCancel != nil {
		onCancel()
	}
	return result, err
}

func (a *countingCancelAdapter) CancelAndGetOrder(
	ctx context.Context, credentials exchange.Credentials, request exchange.CancelRequest,
) (exchange.Result, error) {
	return a.CancelOrder(ctx, credentials, request)
}

func (a *countingCancelAdapter) cancelCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func (a *countingCancelAdapter) getCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gets
}

func (a *countingCancelAdapter) placeCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.places
}

func TestObserveMakerAmbiguousCancelParksReconcilingWithoutSecondCancel(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61750", OwnerUsername: "admin",
		Status: "running", ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		LegA: ArbitrageLeg{TradingAccountID: 1, InstrumentID: 101, Exchange: "hyperliquid"},
		LegB: ArbitrageLeg{TradingAccountID: 2, InstrumentID: 202, Exchange: "okx"},
	}
	store := &executorFailStore{combo: combo}
	adapter := &countingCancelAdapter{
		result:   exchange.Result{Status: "pending", LocalCommandAck: true},
		err:      exchange.ErrAmbiguousCancel,
		allowGet: true,
		get:      exchange.Result{Status: "open", FilledQuantity: "0"},
	}
	intent, _, err := orderStore.CreateIntent(context.Background(), Order{
		ID: "maker-1", IdempotencyKey: "amb-cancel", OwnerUsername: "admin",
		TradingAccountID: 1, Exchange: "hyperliquid", Quantity: "1",
		FilledQuantity: "0", AveragePrice: "0", Status: "open",
		ArbitrageExecutionID: "exec-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	intent.Status = "open"
	orderStore.mu.Lock()
	orderStore.orders[intent.ID] = intent
	orderStore.mu.Unlock()
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service, nil, nil, nil, nil, "",
		time.Hour, 0, 0, 0, 50*time.Millisecond, nil,
	)
	execution := &ArbitrageExecution{ID: "exec-1", Status: "maker_open"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	order, _, observeErr := executor.observeMaker(
		ctx, combo, execution, combo.LegA,
		Instrument{ID: 101, Exchange: "hyperliquid", QuantityStep: "1", PriceTick: "0.1"},
		Credentials{TradingAccountID: 1, Exchange: "hyperliquid"},
		adapter, intent, "buy", "100", "ask", orderObservation{}, nil, nil,
	)
	if !errors.Is(observeErr, ErrArbitrageReconciling) {
		t.Fatalf("observeErr=%v order=%+v", observeErr, order)
	}
	if adapter.cancelCalls() != 1 {
		t.Fatalf("cancel calls=%d", adapter.cancelCalls())
	}
	if adapter.getCalls() != 1 {
		t.Fatalf("GetOrder calls=%d", adapter.getCalls())
	}
	if execution.Status != "reconciling" || store.combo.RuntimeState != "reconciling" {
		t.Fatalf("execution=%+v combo=%+v", execution, store.combo)
	}
	orderStore.mu.Lock()
	_, deferred := orderStore.deferred[intent.ID]
	orderStore.mu.Unlock()
	if !deferred {
		t.Fatal("expected DeferReconcile")
	}
}

func TestObserveMakerRejectedCancelParksExistingMaker(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61751", OwnerUsername: "admin",
		Status: "running", ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		LegA: ArbitrageLeg{TradingAccountID: 1, InstrumentID: 101, Exchange: "hyperliquid"},
		LegB: ArbitrageLeg{TradingAccountID: 2, InstrumentID: 202, Exchange: "okx"},
	}
	store := &executorFailStore{combo: combo}
	adapter := &countingCancelAdapter{
		result: exchange.Result{Status: "pending", LocalCommandAck: true},
		err:    exchange.ErrRejected,
	}
	intent := Order{
		ID: "maker-2", IdempotencyKey: "rej-cancel", Status: "open",
		Quantity: "1", FilledQuantity: "0", AveragePrice: "0",
		ArbitrageExecutionID: "exec-2",
	}
	orderStore.mu.Lock()
	orderStore.orders[intent.ID] = intent
	orderStore.mu.Unlock()
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service, nil, nil, nil, nil, "",
		time.Hour, 0, 0, 0, time.Second, nil,
	)
	execution := &ArbitrageExecution{ID: "exec-2", Status: "maker_open"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, observeErr := executor.observeMaker(
		ctx, combo, execution, combo.LegA,
		Instrument{ID: 101, Exchange: "hyperliquid"},
		Credentials{TradingAccountID: 1}, adapter, intent, "buy", "100", "ask",
		orderObservation{}, nil, nil,
	)
	if !errors.Is(observeErr, ErrArbitrageReconciling) {
		t.Fatalf("observeErr=%v", observeErr)
	}
	if adapter.cancelCalls() != 1 {
		t.Fatalf("cancel calls=%d", adapter.cancelCalls())
	}
	if adapter.getCalls() != 0 {
		t.Fatalf("GetOrder calls=%d", adapter.getCalls())
	}
	if execution.Status != "reconciling" {
		t.Fatalf("status=%s", execution.Status)
	}
}

func TestCompleteExecutionRejectsNonTerminalOrders(t *testing.T) {
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61752", OwnerUsername: "admin",
	}
	execution := &ArbitrageExecution{ID: "exec-3", Status: "maker_open"}
	store := &executorFailStore{
		combo: combo,
		orders: []Order{{
			ID: "live-maker", Status: "open", ArbitrageExecutionID: "exec-3",
		}},
	}
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	executor := NewArbitrageExecutor(
		store, orderStore, nil, nil, nil, nil, nil, "",
		time.Second, 0, 0, 0, time.Second, nil,
	)
	err := executor.completeExecution(context.Background(), combo, execution, parseDecimal("100"))
	if !errors.Is(err, ErrArbitrageReconciling) {
		t.Fatalf("err=%v", err)
	}
	if execution.Status != "reconciling" || store.combo.RuntimeState != "reconciling" {
		t.Fatalf("execution=%+v combo=%+v", execution, store.combo)
	}
}

func TestApplyOrderStreamUpdateSkipsWatermarkForHyperliquidFills(t *testing.T) {
	store := &executorStreamStore{result: Order{
		ID: "order-1", Status: "partially_filled", FilledQuantity: "1",
	}}
	executor := &ArbitrageExecutor{orders: store}
	_, err := executor.applyOrderStreamUpdate(context.Background(), Order{
		ID: "order-1", Exchange: "hyperliquid", VenueOrderID: "1",
		FilledQuantity: "1",
	}, orderstream.Update{
		Type: orderstream.UpdateTrade, TradeID: "t1", LastFilled: "1",
		EventTime: time.UnixMilli(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !store.update.SkipVenueWatermark {
		t.Fatal("hyperliquid userFills must skip venue watermark")
	}
}

func TestApplyOrderStreamUpdateOrderSnapshotDoesNotSkipWatermark(t *testing.T) {
	store := &executorStreamStore{result: Order{ID: "order-1", Status: "open"}}
	executor := &ArbitrageExecutor{orders: store}
	_, err := executor.applyOrderStreamUpdate(context.Background(), Order{
		ID: "order-1", Exchange: "hyperliquid",
	}, orderstream.Update{
		Type: orderstream.UpdateOrder, Status: orderstream.StatusNew,
		CumulativeFilled: "1", EventTime: time.UnixMilli(3),
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.update.SkipVenueWatermark {
		t.Fatal("orderUpdates must advance snapshot watermark")
	}
	if store.update.Result.FilledQuantity != "1" {
		t.Fatalf("snapshot=%+v", store.update)
	}
}

type localAckStore struct {
	*memoryStore
	got VenueResult
}

func (s *localAckStore) UpdateResult(ctx context.Context, orderID string, result VenueResult) (Order, error) {
	s.got = result
	return s.memoryStore.UpdateResult(ctx, orderID, result)
}

func TestPersistResultForwardsLocalCommandAck(t *testing.T) {
	store := &localAckStore{memoryStore: newMemoryStore()}
	order, _, err := store.CreateIntent(context.Background(), Order{
		IdempotencyKey: "local-ack", OwnerUsername: "admin", Quantity: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, nil, nil, nil, time.Second, nil)
	if _, err := service.persistResult(context.Background(), order.ID, "arbitrage_cancel", exchange.Result{
		Status: "pending", LocalCommandAck: true,
		Reference: exchange.VenueReference{ClientOrderID: "c1", Cloid: "0xabc"},
	}); err != nil {
		t.Fatal(err)
	}
	if !store.got.LocalCommandAck || store.got.Status != "pending" {
		t.Fatalf("got=%+v", store.got)
	}
}
