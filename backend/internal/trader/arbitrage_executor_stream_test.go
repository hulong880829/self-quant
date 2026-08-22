package trader

import (
	"context"
	"testing"
	"time"

	"selfquant/backend/internal/trader/orderstream"
)

type executorStreamStore struct {
	arbitrageOrderStore
	update StreamUpdate
	result Order
}

func (s *executorStreamStore) ApplyStreamUpdate(
	_ context.Context,
	_ string,
	update StreamUpdate,
) (Order, error) {
	s.update = update
	return s.result, nil
}

func (s *executorStreamStore) AppendEvent(
	context.Context,
	string,
	string,
	map[string]any,
) error {
	return nil
}

func TestArbitrageExecutorAppliesCumulativeStreamFill(t *testing.T) {
	store := &executorStreamStore{result: Order{
		ID: "order-1", Status: "partially_filled", FilledQuantity: "0.5",
	}}
	executor := &ArbitrageExecutor{orders: store}
	eventAt := time.UnixMilli(1_700_000_000_123)
	updated, err := executor.applyOrderStreamUpdate(context.Background(), Order{
		ID: "order-1", VenueOrderID: "venue-1", FilledQuantity: "0.2",
	}, orderstream.Update{
		Type: orderstream.UpdateTrade, VenueOrderID: "venue-1",
		Status: orderstream.StatusPartiallyFilled, CumulativeFilled: "0.5",
		AveragePrice: "100", TradeID: "trade-7", EventTime: eventAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.FilledQuantity != "0.5" || store.update.Result.Status != "partially_filled" {
		t.Fatalf("updated=%+v stream=%+v", updated, store.update)
	}
	if len(store.update.Fills) != 1 || store.update.Fills[0].Quantity != "0.3" ||
		store.update.Fills[0].TradeID != "trade-7" || !store.update.EventAt.Equal(eventAt) {
		t.Fatalf("unexpected fill merge: %+v", store.update)
	}
}

func TestArbitrageExecutorMapsStreamNewToOpen(t *testing.T) {
	store := &executorStreamStore{result: Order{ID: "order-1", Status: "open"}}
	executor := &ArbitrageExecutor{orders: store}
	if _, err := executor.applyOrderStreamUpdate(context.Background(), Order{
		ID: "order-1", FilledQuantity: "0",
	}, orderstream.Update{Type: orderstream.UpdateOrder, Status: orderstream.StatusNew}); err != nil {
		t.Fatal(err)
	}
	if store.update.Result.Status != "open" {
		t.Fatalf("status=%s", store.update.Result.Status)
	}
}

func TestArbitrageExecutorConvertsVenueContractsToBase(t *testing.T) {
	store := &executorStreamStore{result: Order{
		ID: "order-1", Status: "partially_filled", FilledQuantity: "0.004",
	}}
	executor := &ArbitrageExecutor{orders: store}
	_, err := executor.applyOrderStreamUpdate(context.Background(), Order{
		ID: "order-1", VenueOrderID: "venue-1", FilledQuantity: "0.001",
	}, orderstream.Update{
		Type: orderstream.UpdateTrade, VenueOrderID: "venue-1",
		Status: orderstream.StatusPartiallyFilled, CumulativeFilled: "4",
		TradeID: "trade-8", LastPrice: "100",
	}, Instrument{
		Exchange: "okx", ContractType: "perpetual", ContractSize: "0.001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.update.Result.FilledQuantity != "0.004" ||
		len(store.update.Fills) != 1 || store.update.Fills[0].Quantity != "0.003" {
		t.Fatalf("unexpected converted update: %+v", store.update)
	}
}
