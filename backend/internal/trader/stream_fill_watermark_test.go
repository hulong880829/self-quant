package trader

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"selfquant/backend/internal/trader/orderstream"
)

func TestStreamFillSkipsVenueWatermark(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		exchange   string
		updateType orderstream.UpdateType
		cumulative string
		wantSkip   bool
	}{
		{"hl trade empty", "hyperliquid", orderstream.UpdateTrade, "", true},
		{"hl trade with cumulative", "hyperliquid", orderstream.UpdateTrade, "47", true},
		{"hl order snapshot", "hyperliquid", orderstream.UpdateOrder, "47", false},
		{"bitget trade no cumulative", "bitget", orderstream.UpdateTrade, "", true},
		{"bitget trade with cumulative", "bitget", orderstream.UpdateTrade, "243", false},
		{"bitget order snapshot", "bitget", orderstream.UpdateOrder, "243", false},
		{"bitget order empty cumulative", "bitget", orderstream.UpdateOrder, "", false},
		{"binance trade no cumulative", "binance", orderstream.UpdateTrade, "", false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := streamFillSkipsVenueWatermark(test.exchange, test.updateType, test.cumulative)
			if got != test.wantSkip {
				t.Fatalf("skip=%v want=%v", got, test.wantSkip)
			}
		})
	}
}

func TestApplyOrderStreamUpdateSkipsWatermarkForBitgetFillsWithoutCumulative(t *testing.T) {
	store := &executorStreamStore{result: Order{
		ID: "order-1", Status: "open", FilledQuantity: "0",
	}}
	executor := &ArbitrageExecutor{orders: store}
	_, err := executor.applyOrderStreamUpdate(context.Background(), Order{
		ID: "order-1", Exchange: "bitget", VenueOrderID: "1",
		Quantity: "810", FilledQuantity: "0",
	}, orderstream.Update{
		Type: orderstream.UpdateTrade, Status: orderstream.StatusFilled,
		TradeID: "t1", LastFilled: "243", EventTime: time.UnixMilli(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !store.update.SkipVenueWatermark {
		t.Fatal("bitget fill without cumulative must skip venue watermark")
	}
}

func TestApplyOrderStreamUpdateBitgetOrderSnapshotDoesNotSkipWatermark(t *testing.T) {
	store := &executorStreamStore{result: Order{ID: "order-1", Status: "open"}}
	executor := &ArbitrageExecutor{orders: store}
	_, err := executor.applyOrderStreamUpdate(context.Background(), Order{
		ID: "order-1", Exchange: "bitget", Quantity: "810", FilledQuantity: "0",
	}, orderstream.Update{
		Type: orderstream.UpdateOrder, Status: orderstream.StatusCanceled,
		CumulativeFilled: "243", EventTime: time.UnixMilli(3),
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.update.SkipVenueWatermark {
		t.Fatal("bitget order snapshot must advance venue watermark")
	}
	if store.update.Result.FilledQuantity != "243" {
		t.Fatalf("snapshot=%+v", store.update)
	}
}

func TestApplyBitgetFillWithoutCumulativeDoesNotChangeOrder(t *testing.T) {
	store := &mergeStreamStore{order: Order{
		ID: "maker", Exchange: "bitget", Status: "open",
		Quantity: "810", FilledQuantity: "0", AveragePrice: "0",
	}}
	executor := &ArbitrageExecutor{orders: store}
	updated, err := executor.applyOrderStreamUpdate(context.Background(), store.order, orderstream.Update{
		Type: orderstream.UpdateTrade, Status: orderstream.StatusFilled,
		TradeID: "fill-243", LastFilled: "243", LastPrice: "0.061565",
		EventTime: time.UnixMilli(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "open" || updated.FilledQuantity != "0" {
		t.Fatalf("terminal fill without cumulative must not finish maker: %+v", updated)
	}
	if terminalStatus(updated.Status) {
		t.Fatal("maker must remain non-terminal")
	}
	_, fills := store.snapshot()
	if len(fills) != 1 || fills[0].TradeID != "fill-243" || fills[0].Quantity != "243" {
		t.Fatalf("fills=%+v", fills)
	}
}

func TestApplyBitgetOrderThenFillKeeps243ThenCanceled(t *testing.T) {
	store := &mergeStreamStore{order: Order{
		ID: "maker", Exchange: "bitget", Status: "open",
		Quantity: "810", FilledQuantity: "0", AveragePrice: "0",
	}}
	executor := &ArbitrageExecutor{orders: store}
	updated, err := executor.applyOrderStreamUpdate(context.Background(), store.order, orderstream.Update{
		Type: orderstream.UpdateOrder, Status: orderstream.StatusPartiallyFilled,
		CumulativeFilled: "243", AveragePrice: "0.061565",
		EventTime: time.UnixMilli(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.FilledQuantity != "243" {
		t.Fatalf("order topic=%+v", updated)
	}
	updated, err = executor.applyOrderStreamUpdate(context.Background(), updated, orderstream.Update{
		Type: orderstream.UpdateTrade, TradeID: "fill-243", LastFilled: "243",
		LastPrice: "0.061565", EventTime: time.UnixMilli(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.FilledQuantity != "243" {
		t.Fatalf("fill must not add to order topic: %+v", updated)
	}
	updated, err = executor.applyOrderStreamUpdate(context.Background(), updated, orderstream.Update{
		Type: orderstream.UpdateOrder, Status: orderstream.StatusCanceled,
		CumulativeFilled: "243", AveragePrice: "0.061565",
		EventTime: time.UnixMilli(3),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "canceled" || updated.FilledQuantity != "243" {
		t.Fatalf("canceled snapshot=%+v", updated)
	}
	_, fills := store.snapshot()
	if len(fills) != 1 || fills[0].Quantity != "243" {
		t.Fatalf("fills=%+v", fills)
	}
}

func TestApplyBitgetFillThenOrderSnapshotAdvancesCumulative(t *testing.T) {
	store := &mergeStreamStore{order: Order{
		ID: "maker", Exchange: "bitget", Status: "open",
		Quantity: "810", FilledQuantity: "0", AveragePrice: "0",
	}}
	executor := &ArbitrageExecutor{orders: store}
	updated, err := executor.applyOrderStreamUpdate(context.Background(), store.order, orderstream.Update{
		Type: orderstream.UpdateTrade, Status: orderstream.StatusFilled,
		TradeID: "fill-243", LastFilled: "243", LastPrice: "0.061565",
		EventTime: time.UnixMilli(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "open" || updated.FilledQuantity != "0" {
		t.Fatalf("fill first=%+v", updated)
	}
	updated, err = executor.applyOrderStreamUpdate(context.Background(), updated, orderstream.Update{
		Type: orderstream.UpdateOrder, Status: orderstream.StatusCanceled,
		CumulativeFilled: "243", AveragePrice: "0.061565",
		EventTime: time.UnixMilli(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "canceled" || updated.FilledQuantity != "243" {
		t.Fatalf("snapshot after fill=%+v", updated)
	}
}

func TestApplyBitgetTradeWithCumulativeAdvancesOrder(t *testing.T) {
	store := &mergeStreamStore{order: Order{
		ID: "maker", Exchange: "bitget", Status: "open",
		Quantity: "810", FilledQuantity: "0", AveragePrice: "0",
	}}
	executor := &ArbitrageExecutor{orders: store}
	updated, err := executor.applyOrderStreamUpdate(context.Background(), store.order, orderstream.Update{
		Type: orderstream.UpdateTrade, Status: orderstream.StatusPartiallyFilled,
		TradeID: "fill-243", LastFilled: "243", CumulativeFilled: "243",
		AveragePrice: "0.061565", LastPrice: "0.061565",
		EventTime: time.UnixMilli(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.FilledQuantity != "243" {
		t.Fatalf("trade with cumulative=%+v", updated)
	}
	_, fills := store.snapshot()
	if len(fills) != 1 || fills[0].Quantity != "243" {
		t.Fatalf("fills=%+v", fills)
	}
}

func TestApplyBitgetDuplicateTradeIDDoesNotInsertTwice(t *testing.T) {
	store := &mergeStreamStore{order: Order{
		ID: "maker", Exchange: "bitget", Status: "open",
		Quantity: "810", FilledQuantity: "243", AveragePrice: "0.061565",
	}}
	executor := &ArbitrageExecutor{orders: store}
	update := orderstream.Update{
		Type: orderstream.UpdateTrade, TradeID: "fill-243", LastFilled: "243",
		LastPrice: "0.061565", EventTime: time.UnixMilli(2),
	}
	if _, err := executor.applyOrderStreamUpdate(context.Background(), store.order, update); err != nil {
		t.Fatal(err)
	}
	updated, err := executor.applyOrderStreamUpdate(context.Background(), store.order, update)
	if err != nil {
		t.Fatal(err)
	}
	if updated.FilledQuantity != "243" || updated.Status != "open" {
		t.Fatalf("duplicate trade=%+v", updated)
	}
	_, fills := store.snapshot()
	if len(fills) != 1 || fills[0].TradeID != "fill-243" {
		t.Fatalf("fills=%+v", fills)
	}
}

func TestReconcilerApplyPrivateStreamUpdateUsesStreamFillHelper(t *testing.T) {
	store := &executorStreamStore{result: Order{
		ID: "order-1", Status: "open", FilledQuantity: "0",
	}}
	reconciler := NewReconciler(
		&reconcileMemoryStore{}, reconcileCatalog{instrument: Instrument{
			ID: 1, Exchange: "bitget", QuantityStep: "1",
		}}, nil, nil, "", time.Second, time.Second, 1, 1,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	instrument := Instrument{ID: 1, Exchange: "bitget", QuantityStep: "1"}
	order := Order{
		ID: "order-1", Exchange: "bitget", Quantity: "810", FilledQuantity: "0",
	}
	cases := []struct {
		name       string
		update     orderstream.Update
		wantSkip   bool
		wantFilled string
	}{
		{
			name: "bitget fill without cumulative",
			update: orderstream.Update{
				Type: orderstream.UpdateTrade, Status: orderstream.StatusFilled,
				TradeID: "t1", LastFilled: "243", EventTime: time.UnixMilli(2),
			},
			wantSkip: true,
		},
		{
			name: "bitget trade with cumulative",
			update: orderstream.Update{
				Type: orderstream.UpdateTrade, LastFilled: "243",
				CumulativeFilled: "243", EventTime: time.UnixMilli(3),
			},
			wantSkip:   false,
			wantFilled: "243",
		},
		{
			name: "bitget order snapshot",
			update: orderstream.Update{
				Type: orderstream.UpdateOrder, Status: orderstream.StatusCanceled,
				CumulativeFilled: "243", EventTime: time.UnixMilli(4),
			},
			wantSkip:   false,
			wantFilled: "243",
		},
		{
			name: "hyperliquid trade",
			update: orderstream.Update{
				Type: orderstream.UpdateTrade, TradeID: "hl", LastFilled: "10",
				EventTime: time.UnixMilli(5),
			},
			wantSkip: true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			order.Exchange = "bitget"
			if test.name == "hyperliquid trade" {
				order.Exchange = "hyperliquid"
			}
			if _, err := reconciler.applyPrivateStreamUpdate(
				context.Background(), store, instrument, order, test.update,
			); err != nil {
				t.Fatal(err)
			}
			want := streamFillSkipsVenueWatermark(
				order.Exchange, test.update.Type, test.update.CumulativeFilled,
			)
			if store.update.SkipVenueWatermark != test.wantSkip ||
				store.update.SkipVenueWatermark != want {
				t.Fatalf("skip=%v want=%v helper=%v",
					store.update.SkipVenueWatermark, test.wantSkip, want)
			}
			if test.wantFilled != "" && store.update.Result.FilledQuantity != test.wantFilled {
				t.Fatalf("filled=%s want=%s", store.update.Result.FilledQuantity, test.wantFilled)
			}
		})
	}
}
