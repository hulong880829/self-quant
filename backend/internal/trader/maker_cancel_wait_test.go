package trader

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
	"selfquant/backend/internal/trader/orderstream"
)

func TestCompleteExecutionRejectsAnyNonTerminalHedge(t *testing.T) {
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61753", OwnerUsername: "admin",
	}
	execution := &ArbitrageExecution{ID: "exec-hedge", Status: "hedging"}
	store := &executorFailStore{
		combo: combo,
		orders: []Order{
			{ID: "maker", Status: "filled", ArbitrageExecutionID: "exec-hedge"},
			{ID: "hedge-1", Status: "canceled", ArbitrageExecutionID: "exec-hedge"},
			{ID: "hedge-2", Status: "pending", ArbitrageExecutionID: "exec-hedge"},
		},
	}
	executor := NewArbitrageExecutor(
		store, &postOnlyOrderStore{memoryStore: newMemoryStore()}, nil, nil, nil, nil, nil, "",
		time.Second, 0, 0, 0, time.Second, nil,
	)
	err := executor.completeExecution(context.Background(), combo, execution, parseDecimal("100"))
	if !errors.Is(err, ErrArbitrageReconciling) {
		t.Fatalf("err=%v", err)
	}
	if execution.Status != "reconciling" {
		t.Fatalf("status=%s", execution.Status)
	}
}

func TestRestoreMakerOpenSyncsExecutionAndCombination(t *testing.T) {
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61754", RuntimeState: "maker_canceling",
	}
	store := &executorFailStore{combo: combo}
	executor := NewArbitrageExecutor(
		store, &postOnlyOrderStore{memoryStore: newMemoryStore()}, nil, nil, nil, nil, nil, "",
		time.Second, 0, 0, 0, time.Second, nil,
	)
	execution := &ArbitrageExecution{ID: "exec-restore", Status: "maker_canceling"}
	restored := executor.restoreMakerOpen(context.Background(), combo, execution)
	if execution.Status != "maker_open" || restored.RuntimeState != "maker_open" ||
		store.combo.RuntimeState != "maker_open" {
		t.Fatalf("execution=%+v combo=%+v", execution, store.combo)
	}
}

func TestObserveMakerKeepsObservingWhenCancelNotSent(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61755", OwnerUsername: "admin",
		Status: "running", ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		LegA: ArbitrageLeg{TradingAccountID: 1, InstrumentID: 101, Exchange: "hyperliquid"},
		LegB: ArbitrageLeg{TradingAccountID: 2, InstrumentID: 202, Exchange: "okx"},
	}
	store := &executorFailStore{combo: combo}
	adapter := &countingCancelAdapter{
		err:      errors.New("invalid hyperliquid order id"),
		allowGet: true,
	}
	intent := Order{
		ID: "maker-stay", Status: "open", Quantity: "1", FilledQuantity: "0",
		AveragePrice: "0", ArbitrageExecutionID: "exec-stay",
	}
	orderStore.mu.Lock()
	orderStore.orders[intent.ID] = intent
	orderStore.mu.Unlock()
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service, nil, nil, nil, nil, "",
		20*time.Millisecond, 0, 0, 0, time.Hour, nil,
	)
	executor.streamAudit = time.Hour
	execution := &ArbitrageExecution{ID: "exec-stay", Status: "maker_open"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := executor.observeMaker(
			ctx, combo, execution, combo.LegA,
			Instrument{ID: 101, Exchange: "hyperliquid", QuantityStep: "1", PriceTick: "0.1"},
			Credentials{TradingAccountID: 1, Exchange: "hyperliquid"},
			adapter, intent, "buy", "100", "ask", orderObservation{}, nil, nil,
		)
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		status := store.execution.Status
		runtime := store.combo.RuntimeState
		store.mu.Unlock()
		if adapter.cancelCalls() >= 1 && status == "maker_open" && runtime == "maker_open" {
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("observeMaker did not return")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	store.mu.Lock()
	status := store.execution.Status
	runtime := store.combo.RuntimeState
	store.mu.Unlock()
	t.Fatalf("did not restore maker_open; execution=%s combo=%s cancels=%d",
		status, runtime, adapter.cancelCalls())
}

type failingStreamStore struct {
	*postOnlyOrderStore
	mu            sync.Mutex
	failRemaining int
}

func (s *failingStreamStore) ApplyStreamUpdate(
	ctx context.Context, orderID string, update StreamUpdate,
) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failRemaining > 0 {
		s.failRemaining--
		return Order{}, errors.New("persist failed")
	}
	return s.postOnlyOrderStore.ApplyStreamUpdate(ctx, orderID, update)
}

type cancelStreamRead struct {
	payload []byte
	err     error
}

type cancelStreamConn struct {
	reads chan cancelStreamRead
	done  chan struct{}
	once  sync.Once
}

func newCancelStreamConn() *cancelStreamConn {
	return &cancelStreamConn{reads: make(chan cancelStreamRead, 8), done: make(chan struct{})}
}

func (c *cancelStreamConn) Read() ([]byte, error) {
	select {
	case <-c.done:
		return nil, io.EOF
	case result := <-c.reads:
		return result.payload, result.err
	}
}

func (c *cancelStreamConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

type cancelStreamConnector struct {
	mu          sync.Mutex
	connections []*cancelStreamConn
	calls       int
	called      chan int
}

func (c *cancelStreamConnector) Connect(
	context.Context, orderstream.Key, orderstream.Credentials,
) (orderstream.Connection, error) {
	c.mu.Lock()
	index := c.calls
	c.calls++
	c.mu.Unlock()
	select {
	case c.called <- index + 1:
	default:
	}
	if index >= len(c.connections) {
		return nil, errors.New("no fake connection")
	}
	return c.connections[index], nil
}

func startHLCancelWatch(t *testing.T, extra ...*cancelStreamConn) (
	*orderstream.Subscription, *orderstream.OrderWatch, *cancelStreamConn,
) {
	t.Helper()
	connection := newCancelStreamConn()
	connections := []*cancelStreamConn{connection}
	connections = append(connections, extra...)
	connector := &cancelStreamConnector{
		connections: connections, called: make(chan int, 4),
	}
	manager, err := orderstream.New(orderstream.Options{
		Connector: connector, ReconnectInitial: time.Millisecond,
		ReconnectMax: 2 * time.Millisecond, StaleAfter: time.Second,
		IdleTimeout: -1,
		Jitter:      func(time.Duration) time.Duration { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	subscription, err := manager.Subscribe(
		context.Background(),
		orderstream.Key{Account: "1", Venue: orderstream.VenueHyperliquid, Product: orderstream.ProductPerpetual},
		orderstream.Credentials{Secret: "agent-key", SigningAddress: "0xuser"},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subscription.Close() })
	select {
	case got := <-connector.called:
		if got != 1 {
			t.Fatalf("connection call=%d", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream connect")
	}
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for {
		select {
		case update, ok := <-subscription.Updates():
			if !ok {
				t.Fatal("subscription closed before connected")
			}
			if update.Status == orderstream.StatusConnected {
				if !subscription.Healthy() {
					t.Fatal("connected subscription is not healthy")
				}
				watch, watchErr := subscription.WatchOrder(context.Background(), "0xclient")
				if watchErr != nil {
					t.Fatal(watchErr)
				}
				t.Cleanup(func() { _ = watch.Close() })
				return subscription, watch, connection
			}
		case <-timeout.C:
			t.Fatal("timed out waiting for connected")
		}
	}
}

func seedOpenMaker(t *testing.T, store *postOnlyOrderStore, id string) Order {
	t.Helper()
	order := Order{
		ID: id, IdempotencyKey: id, OwnerUsername: "admin",
		TradingAccountID: 1, Exchange: "hyperliquid", Quantity: "1",
		FilledQuantity: "0", AveragePrice: "0", Status: "open",
		ClientOrderID: "0xclient", ArbitrageExecutionID: "exec-wait",
	}
	store.mu.Lock()
	store.orders[order.ID] = order
	store.mu.Unlock()
	return order
}

func awaitCancelHarness(t *testing.T, timeout time.Duration, orders arbitrageOrderStore) (
	*ArbitrageExecutor, *executorFailStore, *ArbitrageExecution, ArbitrageCombination, *countingCancelAdapter,
) {
	t.Helper()
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61756", OwnerUsername: "admin",
		RuntimeState: "maker_canceling",
	}
	store := &executorFailStore{combo: combo}
	adapter := &countingCancelAdapter{}
	service := NewService(orders, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orders, service, nil, nil, nil, nil, "",
		15*time.Millisecond, 0, 0, 0, timeout, nil,
	)
	execution := &ArbitrageExecution{ID: "exec-wait", Status: "maker_canceling"}
	return executor, store, execution, combo, adapter
}

func testMakerCancelConfirm(
	adapter exchange.Adapter,
	observation orderObservation,
	deadline time.Duration,
) makerCancelConfirm {
	if deadline <= 0 {
		deadline = time.Hour
	}
	return makerCancelConfirm{
		adapter:    adapter,
		generation: snapshotCancelGeneration(observation),
		deadline:   time.Now().Add(deadline),
	}
}

func TestAwaitCancelTerminalGraceElapsedConfirmsByRESTOnce(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	order := seedOpenMaker(t, orderStore, "maker-timeout")
	subscription, watch, _ := startHLCancelWatch(t)
	executor, store, execution, combo, adapter := awaitCancelHarness(
		t, 60*time.Millisecond, orderStore,
	)
	adapter.allowGet = true
	observation := orderObservation{subscription: subscription, watch: watch}
	_, err := executor.awaitCancelTerminal(
		context.Background(), combo, execution,
		Instrument{ID: 101, Exchange: "hyperliquid", QuantityStep: "1"},
		order, observation, nil,
		testMakerCancelConfirm(adapter, observation, 80*time.Millisecond), nil,
	)
	if !errors.Is(err, ErrArbitrageReconciling) {
		t.Fatalf("err=%v", err)
	}
	if execution.Status != "reconciling" || store.combo.RuntimeState != "reconciling" {
		t.Fatalf("execution=%+v combo=%+v", execution, store.combo)
	}
	if adapter.getCalls() != 1 || adapter.cancelCalls() != 0 {
		t.Fatalf("gets=%d cancels=%d", adapter.getCalls(), adapter.cancelCalls())
	}
	orderStore.mu.Lock()
	_, deferred := orderStore.deferred[order.ID]
	orderStore.mu.Unlock()
	if !deferred {
		t.Fatal("expected DeferReconcile")
	}
}

func TestAwaitCancelTerminalUnhealthyConfirmsByRESTOnce(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	order := seedOpenMaker(t, orderStore, "maker-unhealthy")
	subscription, watch, connection := startHLCancelWatch(t)
	executor, store, execution, combo, adapter := awaitCancelHarness(
		t, time.Hour, orderStore,
	)
	adapter.allowGet = true
	observation := orderObservation{subscription: subscription, watch: watch}
	done := make(chan error, 1)
	go func() {
		_, err := executor.awaitCancelTerminal(
			context.Background(), combo, execution,
			Instrument{ID: 101, Exchange: "hyperliquid", QuantityStep: "1"},
			order, observation, nil,
			testMakerCancelConfirm(adapter, observation, time.Hour), nil,
		)
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	connection.reads <- cancelStreamRead{err: errors.New("socket lost")}
	select {
	case err := <-done:
		if !errors.Is(err, ErrArbitrageReconciling) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not park after stream became unhealthy")
	}
	if execution.Status != "reconciling" || store.combo.RuntimeState != "reconciling" {
		t.Fatalf("execution=%+v combo=%+v", execution, store.combo)
	}
	if adapter.getCalls() != 1 || adapter.cancelCalls() != 0 {
		t.Fatalf("gets=%d cancels=%d", adapter.getCalls(), adapter.cancelCalls())
	}
}

func TestAwaitCancelTerminalContextDoneParksWithoutSecondCancel(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	order := seedOpenMaker(t, orderStore, "maker-ctx")
	subscription, watch, _ := startHLCancelWatch(t)
	executor, store, execution, combo, adapter := awaitCancelHarness(
		t, time.Hour, orderStore,
	)
	observation := orderObservation{subscription: subscription, watch: watch}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := executor.awaitCancelTerminal(
			ctx, combo, execution,
			Instrument{ID: 101, Exchange: "hyperliquid", QuantityStep: "1"},
			order, observation, nil,
			testMakerCancelConfirm(adapter, observation, time.Hour), nil,
		)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrArbitrageReconciling) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not park on ctx.Done")
	}
	if execution.Status != "reconciling" || store.combo.RuntimeState != "reconciling" {
		t.Fatalf("execution=%+v combo=%+v", execution, store.combo)
	}
	if adapter.cancelCalls() != 0 || adapter.getCalls() != 0 {
		t.Fatalf("cancels=%d gets=%d", adapter.cancelCalls(), adapter.getCalls())
	}
}

func TestAwaitCancelTerminalFilledPersistsBeforeReturn(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	order := seedOpenMaker(t, orderStore, "maker-filled")
	subscription, watch, connection := startHLCancelWatch(t)
	executor, store, execution, combo, adapter := awaitCancelHarness(
		t, time.Hour, orderStore,
	)
	observation := orderObservation{subscription: subscription, watch: watch}
	fills := 0
	done := make(chan struct {
		order Order
		err   error
	}, 1)
	go func() {
		updated, err := executor.awaitCancelTerminal(
			context.Background(), combo, execution,
			Instrument{ID: 101, Exchange: "hyperliquid", QuantityStep: "1"},
			order, observation,
			func(current Order) error {
				if parseDecimal(current.FilledQuantity).IsPositive() {
					fills++
				}
				return nil
			},
			testMakerCancelConfirm(adapter, observation, time.Hour), nil,
		)
		done <- struct {
			order Order
			err   error
		}{updated, err}
	}()
	connection.reads <- cancelStreamRead{payload: []byte(`{"channel":"orderUpdates","data":[{
		"order":{"oid":49,"cloid":"0xclient","origSz":"1","sz":"0","filledSz":"1"},
		"status":"filled","statusTimestamp":1700000000002
	}]}`)}
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("err=%v", result.err)
		}
		if result.order.Status != "filled" || fills != 1 {
			t.Fatalf("order=%+v fills=%d", result.order, fills)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for filled persist")
	}
	if execution.Status == "reconciling" || store.combo.RuntimeState == "reconciling" {
		t.Fatalf("parked unexpectedly execution=%+v combo=%+v", execution, store.combo)
	}
	if adapter.getCalls() != 0 {
		t.Fatalf("GetOrder calls=%d", adapter.getCalls())
	}
}

func TestAwaitCancelTerminalCanceledPersistsBeforeReturn(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	order := seedOpenMaker(t, orderStore, "maker-canceled")
	subscription, watch, connection := startHLCancelWatch(t)
	executor, _, execution, combo, adapter := awaitCancelHarness(t, time.Hour, orderStore)
	observation := orderObservation{subscription: subscription, watch: watch}
	done := make(chan struct {
		order Order
		err   error
	}, 1)
	go func() {
		updated, err := executor.awaitCancelTerminal(
			context.Background(), combo, execution,
			Instrument{ID: 101, Exchange: "hyperliquid", QuantityStep: "1"},
			order, observation, nil,
			testMakerCancelConfirm(adapter, observation, time.Hour), nil,
		)
		done <- struct {
			order Order
			err   error
		}{updated, err}
	}()
	connection.reads <- cancelStreamRead{payload: []byte(`{"channel":"orderUpdates","data":[{
		"order":{"oid":49,"cloid":"0xclient","origSz":"1","sz":"1"},
		"status":"canceled","statusTimestamp":1700000000003
	}]}`)}
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("err=%v", result.err)
		}
		if result.order.Status != "canceled" {
			t.Fatalf("order=%+v", result.order)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for canceled persist")
	}
	if adapter.getCalls() != 0 {
		t.Fatalf("GetOrder calls=%d", adapter.getCalls())
	}
}

func TestAwaitCancelTerminalPartialThenCanceledUsesFinalQuantity(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	order := seedOpenMaker(t, orderStore, "maker-partial-cancel")
	order.Quantity = "100"
	orderStore.mu.Lock()
	orderStore.orders[order.ID] = order
	orderStore.mu.Unlock()
	subscription, watch, connection := startHLCancelWatch(t)
	executor, _, execution, combo, adapter := awaitCancelHarness(t, time.Hour, orderStore)
	observation := orderObservation{subscription: subscription, watch: watch}
	var hedgeQty []string
	done := make(chan struct {
		order Order
		err   error
	}, 1)
	go func() {
		updated, err := executor.awaitCancelTerminal(
			context.Background(), combo, execution,
			Instrument{ID: 101, Exchange: "hyperliquid", QuantityStep: "1"},
			order, observation,
			func(current Order) error {
				hedgeQty = append(hedgeQty, current.FilledQuantity)
				return nil
			},
			testMakerCancelConfirm(adapter, observation, time.Hour), nil,
		)
		done <- struct {
			order Order
			err   error
		}{updated, err}
	}()
	connection.reads <- cancelStreamRead{payload: []byte(`{"channel":"orderUpdates","data":[{
		"order":{"oid":49,"cloid":"0xclient","origSz":"100","sz":"70","filledSz":"30"},
		"status":"open","statusTimestamp":1700000000001
	}]}`)}
	connection.reads <- cancelStreamRead{payload: []byte(`{"channel":"orderUpdates","data":[{
		"order":{"oid":49,"cloid":"0xclient","origSz":"100","sz":"70","filledSz":"30"},
		"status":"canceled","statusTimestamp":1700000000002
	}]}`)}
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("err=%v", result.err)
		}
		if result.order.Status != "canceled" || result.order.FilledQuantity != "30" {
			t.Fatalf("order=%+v", result.order)
		}
		if len(hedgeQty) != 1 || hedgeQty[0] != "30" {
			t.Fatalf("hedgeQty=%v", hedgeQty)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for canceled after partial")
	}
	if adapter.getCalls() != 0 {
		t.Fatalf("GetOrder calls=%d", adapter.getCalls())
	}
}

func TestAwaitCancelTerminalPersistFailureConfirmsByRESTOnce(t *testing.T) {
	orderStore := &failingStreamStore{
		postOnlyOrderStore: &postOnlyOrderStore{memoryStore: newMemoryStore()},
		failRemaining:      2,
	}
	order := seedOpenMaker(t, orderStore.postOnlyOrderStore, "maker-persist-fail")
	subscription, watch, connection := startHLCancelWatch(t)
	executor, store, execution, combo, adapter := awaitCancelHarness(
		t, time.Hour, orderStore,
	)
	adapter.allowGet = true
	observation := orderObservation{subscription: subscription, watch: watch}
	done := make(chan error, 1)
	go func() {
		_, err := executor.awaitCancelTerminal(
			context.Background(), combo, execution,
			Instrument{ID: 101, Exchange: "hyperliquid", QuantityStep: "1"},
			order, observation, nil,
			testMakerCancelConfirm(adapter, observation, time.Hour), nil,
		)
		done <- err
	}()
	connection.reads <- cancelStreamRead{payload: []byte(`{"channel":"orderUpdates","data":[{
		"order":{"oid":49,"cloid":"0xclient","origSz":"1","sz":"0","filledSz":"1"},
		"status":"filled","statusTimestamp":1700000000004
	}]}`)}
	select {
	case err := <-done:
		if !errors.Is(err, ErrArbitrageReconciling) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not park after persist failures")
	}
	if execution.Status != "reconciling" || store.combo.RuntimeState != "reconciling" {
		t.Fatalf("execution=%+v combo=%+v", execution, store.combo)
	}
	if adapter.getCalls() != 1 {
		t.Fatalf("GetOrder calls=%d", adapter.getCalls())
	}
	orderStore.memoryStore.mu.Lock()
	stored := orderStore.orders[order.ID]
	orderStore.memoryStore.mu.Unlock()
	if stored.Status == "filled" {
		t.Fatal("failed persist must not complete the maker")
	}
}

func TestAwaitCancelTerminalRESTNotFoundParksReconciling(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	order := seedOpenMaker(t, orderStore, "maker-rest-missing")
	executor, store, execution, combo, adapter := awaitCancelHarness(t, time.Hour, orderStore)
	adapter.allowGet = true
	adapter.getErr = exchange.ErrOrderNotFound
	_, err := executor.awaitCancelTerminal(
		context.Background(), combo, execution,
		Instrument{ID: 101, Exchange: "hyperliquid", QuantityStep: "1"},
		order, orderObservation{}, nil,
		testMakerCancelConfirm(adapter, orderObservation{}, time.Hour), nil,
	)
	if !errors.Is(err, ErrArbitrageReconciling) {
		t.Fatalf("err=%v", err)
	}
	if adapter.getCalls() != 1 || adapter.cancelCalls() != 0 {
		t.Fatalf("gets=%d cancels=%d", adapter.getCalls(), adapter.cancelCalls())
	}
	if execution.Status != "reconciling" || store.combo.RuntimeState != "reconciling" {
		t.Fatalf("execution=%+v combo=%+v", execution, store.combo)
	}
}

func TestAwaitCancelTerminalRESTCanceledZeroFill(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	order := seedOpenMaker(t, orderStore, "maker-rest-canceled")
	executor, store, execution, combo, adapter := awaitCancelHarness(t, time.Hour, orderStore)
	adapter.allowGet = true
	adapter.get = exchange.Result{Status: "canceled", FilledQuantity: "0"}
	updated, err := executor.awaitCancelTerminal(
		context.Background(), combo, execution,
		Instrument{ID: 101, Exchange: "hyperliquid", QuantityStep: "1"},
		order, orderObservation{}, nil,
		testMakerCancelConfirm(adapter, orderObservation{}, time.Hour), nil,
	)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if updated.Status != "canceled" || updated.FilledQuantity != "0" {
		t.Fatalf("order=%+v", updated)
	}
	if adapter.getCalls() != 1 {
		t.Fatalf("GetOrder calls=%d", adapter.getCalls())
	}
	if execution.Status == "reconciling" || store.combo.RuntimeState == "reconciling" {
		t.Fatalf("parked unexpectedly execution=%+v combo=%+v", execution, store.combo)
	}
}

func TestSnapshotCancelGenerationRecordsZero(t *testing.T) {
	subscription, watch, _ := startHLCancelWatch(t)
	observation := orderObservation{subscription: subscription, watch: watch}
	snap := snapshotCancelGeneration(observation)
	if !snap.recorded {
		t.Fatal("generation 0 must be recorded when the watcher is healthy")
	}
	if cancelGenerationChanged(snap, observation) {
		t.Fatal("unchanged generation 0 must not look like a reconnect")
	}
}

func TestRecoverWithoutMarketDataDoesNotPlaceHedge(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61757", OwnerUsername: "admin",
		Status: "running", ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 101, Exchange: "hyperliquid",
			ExchangeSymbol: "BTC", ContractType: "perpetual",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 202, Exchange: "okx",
			ExchangeSymbol: "BTC-USDT-SWAP", ContractType: "perpetual",
		},
	}
	store := &executorFailStore{combo: combo}
	adapter := &countingCancelAdapter{
		result:   exchange.Result{Status: "pending", LocalCommandAck: true},
		allowGet: true,
		get:      exchange.Result{Status: "canceled", FilledQuantity: "0"},
	}
	intent := Order{
		ID: "maker-recover", IdempotencyKey: "recover", OwnerUsername: "admin",
		TradingAccountID: 1, Exchange: "hyperliquid", Quantity: "1",
		FilledQuantity: "0", AveragePrice: "0", Status: "open",
		ArbitrageExecutionID: "exec-recover", ArbitrageLeg: "a", ArbitrageRole: "maker",
	}
	orderStore.mu.Lock()
	orderStore.orders[intent.ID] = intent
	orderStore.mu.Unlock()
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	catalog := silentRiskCatalog{
		101: Instrument{
			ID: 101, Exchange: "hyperliquid", ContractType: "perpetual",
			ExchangeSymbol: "BTC", QuantityStep: "1", PriceTick: "0.1",
		},
		202: Instrument{
			ID: 202, Exchange: "okx", ContractType: "perpetual",
			ExchangeSymbol: "BTC-USDT-SWAP", QuantityStep: "1", PriceTick: "0.1",
		},
	}
	executor := NewArbitrageExecutor(
		store, orderStore, service, catalog, silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"hyperliquid": adapter, "okx": adapter,
		}),
		nil, "", 20*time.Millisecond, 0, 0, 0, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "exec-recover", Status: "maker_canceling", MakerOrderID: intent.ID,
	}
	if err := executor.RecoverWithoutMarketData(context.Background(), combo, execution); err != nil {
		t.Fatal(err)
	}
	if adapter.placeCalls() != 0 {
		t.Fatalf("PlaceOrder calls=%d", adapter.placeCalls())
	}
	if adapter.cancelCalls() != 0 || adapter.getCalls() != 0 {
		t.Fatalf("cancels=%d gets=%d", adapter.cancelCalls(), adapter.getCalls())
	}
	if store.execution.Status != "reconciling" || store.combo.RuntimeState != "reconciling" {
		t.Fatalf("execution=%+v combo=%+v", store.execution, store.combo)
	}
}

func TestFinishMakerCancel51400ConfirmsByRESTFillThenHedgesOnce(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	order := seedOpenMaker(t, orderStore, "maker-51400-rest")
	order.Quantity = "100"
	orderStore.mu.Lock()
	orderStore.orders[order.ID] = order
	orderStore.mu.Unlock()
	subscription, watch, _ := startHLCancelWatch(t)
	executor, store, execution, combo, adapter := awaitCancelHarness(
		t, time.Hour, orderStore,
	)
	adapter.allowGet = true
	adapter.get = exchange.Result{
		Status: "filled", VenueOrderID: "49", FilledQuantity: "70", AveragePrice: "100",
	}
	observation := orderObservation{subscription: subscription, watch: watch}
	var hedgeQty []string
	updated, err := executor.finishMakerCancel(
		context.Background(), combo, execution,
		Instrument{ID: 101, Exchange: "hyperliquid", QuantityStep: "1"},
		order, exchange.ErrAmbiguousCancel, observation,
		func(current Order) error {
			if parseDecimal(current.FilledQuantity).IsPositive() {
				hedgeQty = append(hedgeQty, current.FilledQuantity)
			}
			return nil
		},
		testMakerCancelConfirm(adapter, observation, time.Hour),
	)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if updated.Status != "filled" || updated.FilledQuantity != "70" {
		t.Fatalf("order=%+v", updated)
	}
	if len(hedgeQty) != 1 || hedgeQty[0] != "70" {
		t.Fatalf("hedgeQty=%v", hedgeQty)
	}
	if execution.Status == "reconciling" || store.combo.RuntimeState == "reconciling" {
		t.Fatalf("parked unexpectedly execution=%+v combo=%+v", execution, store.combo)
	}
	if adapter.cancelCalls() != 0 || adapter.getCalls() != 1 || adapter.placeCalls() != 0 {
		t.Fatalf("cancels=%d gets=%d places=%d", adapter.cancelCalls(), adapter.getCalls(), adapter.placeCalls())
	}
}

func TestExecuteMakerThenHedgeOKX51400RESTFillHedgesOnce(t *testing.T) {
	market := newBinance2011TestMarket(t)
	makerInstrument := Instrument{
		ID: 101, Exchange: "okx", ContractType: "perpetual",
		ExchangeSymbol: "BTC-USDT-SWAP", BaseAsset: "BTC", QuoteAsset: "USDT",
		ContractSize: "1", PriceTick: "0.1", QuantityStep: "1",
		MinQuantityStatus: exchange.ConstraintNotApplicable,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotionalStatus: exchange.ConstraintNotApplicable,
	}
	hedgeInstrument := Instrument{
		ID: 202, Exchange: "binance", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		ContractSize: "1", PriceTick: "0.1", QuantityStep: "1",
		MinQuantityStatus: exchange.ConstraintNotApplicable,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotionalStatus: exchange.ConstraintNotApplicable,
	}
	combination := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61748", OwnerUsername: "admin",
		Status: "running", RuntimeState: "monitoring",
		ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		OrderNotional: "10000", CarryBaseQuantity: "0",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: makerInstrument.ID,
			ProductName: "ARBITRAGE", Exchange: "okx",
			ContractType: "perpetual", ExchangeSymbol: makerInstrument.ExchangeSymbol,
			BaseAsset: "BTC", QuoteAsset: "USDT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: hedgeInstrument.ID,
			ProductName: "ARBITRAGE", Exchange: "binance",
			ContractType: "perpetual", ExchangeSymbol: hedgeInstrument.ExchangeSymbol,
			BaseAsset: "BTC", QuoteAsset: "USDT",
		},
	}
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	baseStore := &executorFailStore{combo: combination, intentStore: orderStore}
	store := &makerHedgeStore{executorFailStore: baseStore, orderStore: orderStore.memoryStore}
	maker := &countingCancelAdapter{
		place: exchange.Result{Status: "open", VenueOrderID: "maker-1", FilledQuantity: "0"},
		result: exchange.Result{
			Status: "pending", LocalCommandAck: true, VenueOrderID: "maker-1",
		},
		err:      exchange.ErrAmbiguousCancel,
		allowGet: true,
		liveGet:  exchange.Result{Status: "open", VenueOrderID: "maker-1", FilledQuantity: "0"},
		get: exchange.Result{
			Status: "filled", VenueOrderID: "maker-1", FilledQuantity: "70", AveragePrice: "100",
		},
	}
	hedge := &stubAdapter{place: exchange.Result{
		VenueOrderID: "hedge-1", Status: "filled",
		FilledQuantity: "70", AveragePrice: "102",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{
			makerInstrument.ID: makerInstrument, hedgeInstrument.ID: hedgeInstrument,
		},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"okx": maker, "binance": hedge,
		}),
		market, "", time.Millisecond, 1, 0, 1, time.Second, nil,
	)
	executor.streamAudit = time.Hour
	execution := ArbitrageExecution{
		ID: "okx-51400-rest", CombinationID: combination.ID,
		Direction: "ask", RequestedNotional: "10000", TargetBaseQuantity: "100",
		TriggerLegABid: "100", TriggerLegAAsk: "100.1",
		TriggerLegBBid: "100", TriggerLegBAsk: "100.1",
	}
	if err := executor.executeMakerThenHedge(
		context.Background(), combination, &execution,
		marketdata.BBO{BidPrice: "100", AskPrice: "100.1"},
		marketdata.BBO{BidPrice: "100", AskPrice: "100.1"},
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if execution.Status != "completed" {
		t.Fatalf("execution=%+v", execution)
	}
	if maker.cancelCalls() != 1 || maker.placeCalls() != 1 {
		t.Fatalf("cancels=%d places=%d", maker.cancelCalls(), maker.placeCalls())
	}
	if maker.getCalls() < 1 {
		t.Fatalf("GetOrder calls=%d", maker.getCalls())
	}
	if hedge.calls != 1 {
		t.Fatalf("hedgePlaces=%d", hedge.calls)
	}
	if len(hedge.requests) != 1 || hedge.requests[0].Quantity != "70" {
		t.Fatalf("hedge requests=%+v", hedge.requests)
	}
}

func TestAwaitCancelTerminalWatcherClosedConfirmsByRESTOnce(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	order := seedOpenMaker(t, orderStore, "maker-watch-closed")
	subscription, watch, _ := startHLCancelWatch(t)
	executor, store, execution, combo, adapter := awaitCancelHarness(t, time.Hour, orderStore)
	adapter.allowGet = true
	observation := orderObservation{subscription: subscription, watch: watch}
	_ = watch.Close()
	_, err := executor.awaitCancelTerminal(
		context.Background(), combo, execution,
		Instrument{ID: 101, Exchange: "hyperliquid", QuantityStep: "1"},
		order, observation, nil,
		testMakerCancelConfirm(adapter, observation, time.Hour), nil,
	)
	if !errors.Is(err, ErrArbitrageReconciling) {
		t.Fatalf("err=%v", err)
	}
	if execution.Status != "reconciling" || store.combo.RuntimeState != "reconciling" {
		t.Fatalf("execution=%+v combo=%+v", execution, store.combo)
	}
	if adapter.getCalls() != 1 {
		t.Fatalf("GetOrder calls=%d", adapter.getCalls())
	}
}

func TestCancelMakerOnceGenerationChangeDuringHTTPImmediateREST(t *testing.T) {
	extra := newCancelStreamConn()
	subscription, watch, connection := startHLCancelWatch(t, extra)
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	order := seedOpenMaker(t, orderStore, "maker-gen-change")
	executor, store, execution, combo, adapter := awaitCancelHarness(t, time.Hour, orderStore)
	observation := orderObservation{subscription: subscription, watch: watch}
	snap := snapshotCancelGeneration(observation)
	if !snap.recorded {
		t.Fatal("expected recorded generation before cancel")
	}
	adapter.result = exchange.Result{Status: "pending", LocalCommandAck: true}
	adapter.allowGet = true
	adapter.onCancel = func() {
		connection.reads <- cancelStreamRead{err: errors.New("socket lost")}
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if subscription.Generation() != snap.value {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	_, err := executor.cancelMakerOnce(
		context.Background(), combo, execution,
		Credentials{TradingAccountID: 1, Exchange: "hyperliquid"},
		Instrument{ID: 101, Exchange: "hyperliquid", QuantityStep: "1"},
		adapter, order, observation, nil,
	)
	if !errors.Is(err, ErrArbitrageReconciling) {
		t.Fatalf("err=%v", err)
	}
	if adapter.cancelCalls() != 1 || adapter.getCalls() != 1 {
		t.Fatalf("cancels=%d gets=%d", adapter.cancelCalls(), adapter.getCalls())
	}
	if execution.Status != "reconciling" || store.combo.RuntimeState != "reconciling" {
		t.Fatalf("execution=%+v combo=%+v", execution, store.combo)
	}
}

func recoverWithoutMarketFixture(t *testing.T, status, orderStatus string) (
	*ArbitrageExecutor, *executorFailStore, *countingCancelAdapter, ArbitrageCombination, ArbitrageExecution,
) {
	t.Helper()
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61759", OwnerUsername: "admin",
		Status: "running", ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		RuntimeState: status,
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 101, Exchange: "hyperliquid",
			ExchangeSymbol: "BTC", ContractType: "perpetual",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 202, Exchange: "okx",
			ExchangeSymbol: "BTC-USDT-SWAP", ContractType: "perpetual",
		},
	}
	store := &executorFailStore{combo: combo}
	adapter := &countingCancelAdapter{
		result:   exchange.Result{Status: "pending", LocalCommandAck: true},
		allowGet: true,
		get:      exchange.Result{Status: "canceled", FilledQuantity: "0"},
	}
	intent := Order{
		ID: "maker-recover-2", IdempotencyKey: "recover-2", OwnerUsername: "admin",
		TradingAccountID: 1, Exchange: "hyperliquid", Quantity: "1",
		FilledQuantity: "0", AveragePrice: "0", Status: orderStatus,
		ArbitrageExecutionID: "exec-recover-2", ArbitrageLeg: "a", ArbitrageRole: "maker",
	}
	if orderStatus == "filled" {
		intent.FilledQuantity = "1"
		intent.AveragePrice = "100"
	}
	orderStore.mu.Lock()
	orderStore.orders[intent.ID] = intent
	orderStore.mu.Unlock()
	executor := NewArbitrageExecutor(
		store, orderStore, NewService(orderStore, nil, nil, nil, time.Second, nil),
		silentRiskCatalog{
			101: Instrument{
				ID: 101, Exchange: "hyperliquid", ContractType: "perpetual",
				ExchangeSymbol: "BTC", QuantityStep: "1", PriceTick: "0.1",
			},
			202: Instrument{
				ID: 202, Exchange: "okx", ContractType: "perpetual",
				ExchangeSymbol: "BTC-USDT-SWAP", QuantityStep: "1", PriceTick: "0.1",
			},
		},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"hyperliquid": adapter, "okx": adapter,
		}),
		nil, "", 20*time.Millisecond, 0, 0, 0, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "exec-recover-2", Status: status, MakerOrderID: intent.ID,
	}
	return executor, store, adapter, combo, execution
}

func TestRecoverWithoutMarketDataMakerOpenCancelsOnce(t *testing.T) {
	executor, store, adapter, combo, execution := recoverWithoutMarketFixture(t, "maker_open", "open")
	if err := executor.RecoverWithoutMarketData(context.Background(), combo, execution); err != nil {
		t.Fatal(err)
	}
	if adapter.cancelCalls() != 1 || adapter.getCalls() > 1 {
		t.Fatalf("cancels=%d gets=%d", adapter.cancelCalls(), adapter.getCalls())
	}
	if adapter.getCalls() != 1 {
		t.Fatalf("GetOrder calls=%d", adapter.getCalls())
	}
	if adapter.placeCalls() != 0 {
		t.Fatalf("PlaceOrder calls=%d", adapter.placeCalls())
	}
	if store.execution.Status != "reconciling" || store.combo.RuntimeState != "reconciling" {
		t.Fatalf("execution=%+v combo=%+v", store.execution, store.combo)
	}
}

func TestRecoverWithoutMarketDataLocalTerminalDoesNotTouchVenue(t *testing.T) {
	executor, store, adapter, combo, execution := recoverWithoutMarketFixture(t, "maker_open", "filled")
	if err := executor.RecoverWithoutMarketData(context.Background(), combo, execution); err != nil {
		t.Fatal(err)
	}
	if adapter.cancelCalls() != 0 || adapter.getCalls() != 0 || adapter.placeCalls() != 0 {
		t.Fatalf("cancels=%d gets=%d places=%d", adapter.cancelCalls(), adapter.getCalls(), adapter.placeCalls())
	}
	if store.execution.Status != "reconciling" {
		t.Fatalf("execution=%+v", store.execution)
	}
}
