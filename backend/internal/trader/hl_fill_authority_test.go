package trader

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"selfquant/backend/internal/trader/orderstream"
)

type mergeStreamStore struct {
	arbitrageOrderStore
	mu    sync.Mutex
	order Order
	fills []OrderFill
}

func (s *mergeStreamStore) ApplyStreamUpdate(
	_ context.Context, _ string, update StreamUpdate,
) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inserted := make([]OrderFill, 0, len(update.Fills))
	for _, fill := range update.Fills {
		if fill.TradeID != "" && streamStoreHasTrade(s.fills, fill.TradeID) {
			continue
		}
		inserted = append(inserted, fill)
	}
	s.fills = append(s.fills, inserted...)
	merged, err := mergeOrderUpdate(
		s.order, update.Result, update.EventAt, inserted, update.SkipVenueWatermark,
	)
	if err != nil {
		return Order{}, err
	}
	s.order = merged
	return merged, nil
}

func (s *mergeStreamStore) AppendEvent(context.Context, string, string, map[string]any) error {
	return nil
}

func streamStoreHasTrade(fills []OrderFill, tradeID string) bool {
	for _, fill := range fills {
		if fill.TradeID == tradeID {
			return true
		}
	}
	return false
}

func (s *mergeStreamStore) snapshot() (Order, []OrderFill) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.order, append([]OrderFill(nil), s.fills...)
}

func TestApplyHyperliquidUserFillsRecordTradesWithoutChangingCumulative(t *testing.T) {
	store := &mergeStreamStore{order: Order{
		ID: "order-1", Exchange: "hyperliquid", Status: "filled",
		Quantity: "100", FilledQuantity: "47", AveragePrice: "1",
	}}
	executor := &ArbitrageExecutor{orders: store}
	for i, trade := range []struct {
		id, qty string
	}{{"t1", "47"}, {"t2", "12"}} {
		updated, err := executor.applyOrderStreamUpdate(context.Background(), store.order, orderstream.Update{
			Type: orderstream.UpdateTrade, TradeID: trade.id, LastFilled: trade.qty,
			LastPrice: "1", EventTime: time.UnixMilli(int64(i + 1)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if updated.FilledQuantity != "47" || updated.Status != "filled" {
			t.Fatalf("after %s: %+v", trade.id, updated)
		}
	}
	order, fills := store.snapshot()
	if order.FilledQuantity != "47" || len(fills) != 2 ||
		fills[0].TradeID != "t1" || fills[1].TradeID != "t2" {
		t.Fatalf("order=%+v fills=%+v", order, fills)
	}
}

func TestApplyHyperliquidIOCPlaceThenUserFillKeeps47And92(t *testing.T) {
	for _, qty := range []string{"47", "92"} {
		store := &mergeStreamStore{order: Order{
			ID: "ioc-" + qty, Exchange: "hyperliquid", Status: "filled",
			Quantity: "100", FilledQuantity: qty, AveragePrice: "1",
		}}
		executor := &ArbitrageExecutor{orders: store}
		updated, err := executor.applyOrderStreamUpdate(context.Background(), store.order, orderstream.Update{
			Type: orderstream.UpdateTrade, TradeID: "fill-" + qty, LastFilled: qty,
			LastPrice: "1", EventTime: time.UnixMilli(2),
		})
		if err != nil {
			t.Fatal(err)
		}
		if updated.FilledQuantity != qty || updated.Status != "filled" {
			t.Fatalf("qty=%s updated=%+v", qty, updated)
		}
		_, fills := store.snapshot()
		if len(fills) != 1 || fills[0].Quantity != qty {
			t.Fatalf("qty=%s fills=%+v", qty, fills)
		}
	}
}

func TestApplyHyperliquidPartialUserFillDoesNotTerminateMaker(t *testing.T) {
	store := &mergeStreamStore{order: Order{
		ID: "maker", Exchange: "hyperliquid", Status: "open",
		Quantity: "100", FilledQuantity: "0", AveragePrice: "0",
	}}
	executor := &ArbitrageExecutor{orders: store}
	updated, err := executor.applyOrderStreamUpdate(context.Background(), store.order, orderstream.Update{
		Type: orderstream.UpdateTrade, TradeID: "partial", LastFilled: "10",
		LastPrice: "1", EventTime: time.UnixMilli(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "open" || updated.FilledQuantity != "0" {
		t.Fatalf("partial userFill must not terminate maker: %+v", updated)
	}
	if terminalStatus(updated.Status) {
		t.Fatal("maker must remain non-terminal")
	}
}

func TestApplyHyperliquidTerminalSnapshotFills47ThenUserFillDoesNotAdd(t *testing.T) {
	store := &mergeStreamStore{order: Order{
		ID: "maker", Exchange: "hyperliquid", Status: "open",
		Quantity: "100", FilledQuantity: "0", AveragePrice: "0",
	}}
	executor := &ArbitrageExecutor{orders: store}
	updated, err := executor.applyOrderStreamUpdate(context.Background(), store.order, orderstream.Update{
		Type: orderstream.UpdateOrder, Status: orderstream.StatusFilled,
		CumulativeFilled: "47", EventTime: time.UnixMilli(3),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.FilledQuantity != "47" || updated.Status != "filled" {
		t.Fatalf("snapshot=%+v", updated)
	}
	updated, err = executor.applyOrderStreamUpdate(context.Background(), updated, orderstream.Update{
		Type: orderstream.UpdateTrade, TradeID: "late", LastFilled: "47",
		LastPrice: "1", EventTime: time.UnixMilli(4),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.FilledQuantity != "47" {
		t.Fatalf("userFill must not add to snapshot: %+v", updated)
	}
}

func TestApplyHyperliquidTerminalSnapshotWithoutCumulativeIsUnknown(t *testing.T) {
	store := &mergeStreamStore{order: Order{
		ID: "maker", Exchange: "hyperliquid", Status: "open",
		Quantity: "40", FilledQuantity: "0", AveragePrice: "0",
	}}
	executor := &ArbitrageExecutor{orders: store}
	_, err := executor.applyOrderStreamUpdate(context.Background(), store.order, orderstream.Update{
		Type: orderstream.UpdateOrder, Status: orderstream.StatusFilled,
		EventTime: time.UnixMilli(5),
	})
	if !errors.Is(err, errHyperliquidFillUnknown) {
		t.Fatalf("err=%v", err)
	}
	order, _ := store.snapshot()
	if order.Status != "open" || order.FilledQuantity != "0" {
		t.Fatalf("must not persist guessed fill: %+v", order)
	}
}

func TestAwaitCancelTerminalUnknownFillParksReconciling(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	order := seedOpenMaker(t, orderStore, "maker-unknown-fill")
	subscription, watch, connection := startHLCancelWatch(t)
	executor, store, execution, combo, adapter := awaitCancelHarness(t, time.Hour, orderStore)
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
		"order":{"oid":49,"cloid":"0xclient","origSz":"40"},
		"status":"filled","statusTimestamp":1700000000005
	}]}`)}
	select {
	case err := <-done:
		if !errors.Is(err, ErrArbitrageReconciling) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not park on unknown terminal fill")
	}
	if execution.Status != "reconciling" || store.combo.RuntimeState != "reconciling" {
		t.Fatalf("execution=%+v combo=%+v", execution, store.combo)
	}
	if adapter.getCalls() != 1 {
		t.Fatalf("GetOrder calls=%d", adapter.getCalls())
	}
}

func TestObserveMakerHedgesOnlyTerminalSnapshotQuantity(t *testing.T) {
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61758", OwnerUsername: "admin",
		Status: "running", ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		LegA: ArbitrageLeg{TradingAccountID: 1, InstrumentID: 101, Exchange: "hyperliquid"},
		LegB: ArbitrageLeg{TradingAccountID: 2, InstrumentID: 202, Exchange: "okx"},
	}
	store := &executorFailStore{combo: combo}
	intent := seedOpenMaker(t, orderStore, "maker-hedge-once")
	intent.Quantity = "100"
	orderStore.mu.Lock()
	orderStore.orders[intent.ID] = intent
	orderStore.mu.Unlock()
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service, nil, nil, nil, nil, "",
		time.Hour, 0, 0, 0, time.Hour, nil,
	)
	executor.streamAudit = time.Hour
	execution := &ArbitrageExecution{ID: "exec-wait", Status: "maker_open"}
	subscription, watch, connection := startHLCancelWatch(t)
	var hedgeQty []string
	done := make(chan struct {
		order Order
		err   error
	}, 1)
	go func() {
		updated, _, err := executor.observeMaker(
			context.Background(), combo, execution, combo.LegA,
			Instrument{ID: 101, Exchange: "hyperliquid", QuantityStep: "1", PriceTick: "0.1"},
			Credentials{TradingAccountID: 1, Exchange: "hyperliquid"},
			&countingCancelAdapter{}, intent, "buy", "100", "ask",
			orderObservation{subscription: subscription, watch: watch},
			func(current Order) error {
				hedgeQty = append(hedgeQty, current.FilledQuantity)
				return nil
			},
			nil,
		)
		done <- struct {
			order Order
			err   error
		}{updated, err}
	}()
	connection.reads <- cancelStreamRead{payload: []byte(`{"channel":"userFills","data":{"fills":[{
		"oid":49,"tid":1,"cloid":"0xclient","sz":"10","px":"1","time":1700000000001
	}]}}`)}
	connection.reads <- cancelStreamRead{payload: []byte(`{"channel":"orderUpdates","data":[{
		"order":{"oid":49,"cloid":"0xclient","origSz":"100","sz":"53","filledSz":"47"},
		"status":"filled","statusTimestamp":1700000000002
	}]}`)}
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("err=%v", result.err)
		}
		if result.order.FilledQuantity != "47" || result.order.Status != "filled" {
			t.Fatalf("order=%+v", result.order)
		}
		if len(hedgeQty) != 1 || hedgeQty[0] != "47" {
			t.Fatalf("hedgeQty=%v", hedgeQty)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for terminal snapshot")
	}
}
