package trader

import (
	"context"
	"errors"
	"testing"
	"time"

	"selfquant/backend/internal/trader/exchange"
)

func TestMergeLocalCommandAckDoesNotOverrideVenueSnapshot(t *testing.T) {
	t.Parallel()
	watermark := time.UnixMilli(1_700_000_000_000)
	current := Order{
		VenueOrderID: "", Status: "open", Quantity: "2",
		FilledQuantity: "1", AveragePrice: "100", LastVenueEventAt: watermark,
		ErrorCode: "open",
	}
	merged, err := mergeOrderUpdate(current, VenueResult{
		VenueOrderID: "99", Status: "pending", FilledQuantity: "1",
		AveragePrice: "50", ErrorCode: "ack", ErrorMessage: "local",
		LocalCommandAck: true,
	}, time.Time{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Status != "open" || merged.AveragePrice != "100" ||
		merged.FilledQuantity != "1" || merged.ErrorCode != "open" ||
		merged.VenueOrderID != "99" {
		t.Fatalf("merged=%+v", merged)
	}
}

func TestMergeLocalCommandAckAcceptsLargerFillWithAverage(t *testing.T) {
	t.Parallel()
	watermark := time.UnixMilli(1_700_000_000_000)
	current := Order{
		Status: "open", Quantity: "3", FilledQuantity: "1", AveragePrice: "100",
		LastVenueEventAt: watermark,
	}
	merged, err := mergeOrderUpdate(current, VenueResult{
		Status: "pending", FilledQuantity: "2", AveragePrice: "110",
		LocalCommandAck: true,
	}, time.Time{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Status != "open" || merged.FilledQuantity != "2" ||
		merged.AveragePrice != "110" {
		t.Fatalf("merged=%+v", merged)
	}
}

func TestMergeUserFillsDoNotChangeCumulativeOrStatus(t *testing.T) {
	t.Parallel()
	watermark := time.UnixMilli(1_700_000_000_000)
	current := Order{
		Status: "open", Quantity: "10", FilledQuantity: "2", AveragePrice: "100",
		LastVenueEventAt: watermark, ErrorCode: "open",
	}
	sameMs := watermark.Add(time.Millisecond)
	merged, err := mergeOrderUpdate(current, VenueResult{Status: "filled"}, sameMs, []OrderFill{
		{TradeID: "a", Quantity: "1", Price: "101", ExecutedAt: sameMs},
		{TradeID: "b", Quantity: "1", Price: "102", ExecutedAt: sameMs},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if merged.FilledQuantity != "2" || merged.Status != "open" ||
		merged.AveragePrice != "100" || merged.ErrorCode != "open" {
		t.Fatalf("merged=%+v", merged)
	}
}

func TestMergeOlderUserFillDoesNotIncreaseCumulative(t *testing.T) {
	t.Parallel()
	watermark := time.UnixMilli(1_700_000_000_000)
	current := Order{
		Status: "open", Quantity: "10", FilledQuantity: "2", AveragePrice: "100",
		LastVenueEventAt: watermark,
	}
	merged, err := mergeOrderUpdate(current, VenueResult{}, watermark.Add(-time.Millisecond), []OrderFill{
		{TradeID: "old", Quantity: "1", Price: "90", ExecutedAt: watermark.Add(-time.Millisecond)},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if merged.FilledQuantity != "2" {
		t.Fatalf("filled=%s", merged.FilledQuantity)
	}
}

func TestMergeSnapshotThenLaterUserFillDoesNotIncreaseCumulative(t *testing.T) {
	t.Parallel()
	watermark := time.UnixMilli(1_700_000_000_000)
	current := Order{
		Status: "open", Quantity: "10", FilledQuantity: "2", AveragePrice: "100",
		LastVenueEventAt: watermark,
	}
	later := watermark.Add(2 * time.Millisecond)
	merged, err := mergeOrderUpdate(current, VenueResult{}, later, []OrderFill{
		{TradeID: "new", Quantity: "1", Price: "105", ExecutedAt: later},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if merged.FilledQuantity != "2" {
		t.Fatalf("filled=%s", merged.FilledQuantity)
	}
}

func TestMergeLocalCommandAckWithoutWatermarkMaySetPending(t *testing.T) {
	t.Parallel()
	current := Order{
		Status: "pending", Quantity: "1", FilledQuantity: "0", AveragePrice: "0",
	}
	merged, err := mergeOrderUpdate(current, VenueResult{
		Status: "pending", LocalCommandAck: true,
	}, time.Time{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Status != "pending" {
		t.Fatalf("status=%s", merged.Status)
	}
}

func TestMergeLocalCommandAckLargerFillWithoutAverageKeepsOld(t *testing.T) {
	t.Parallel()
	watermark := time.UnixMilli(1_700_000_000_000)
	current := Order{
		Status: "open", Quantity: "3", FilledQuantity: "1", AveragePrice: "100",
		LastVenueEventAt: watermark,
	}
	merged, err := mergeOrderUpdate(current, VenueResult{
		Status: "pending", FilledQuantity: "2", LocalCommandAck: true,
	}, time.Time{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if merged.FilledQuantity != "1" || merged.AveragePrice != "100" {
		t.Fatalf("merged=%+v", merged)
	}
}

func TestMergeUserFillAtWatermarkDoesNotIncrease(t *testing.T) {
	t.Parallel()
	watermark := time.UnixMilli(1_700_000_000_000)
	current := Order{
		Status: "open", Quantity: "10", FilledQuantity: "2", AveragePrice: "100",
		LastVenueEventAt: watermark,
	}
	merged, err := mergeOrderUpdate(current, VenueResult{Status: "open"}, watermark, []OrderFill{
		{TradeID: "dup", Quantity: "1", Price: "101", ExecutedAt: watermark},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if merged.FilledQuantity != "2" {
		t.Fatalf("filled=%s", merged.FilledQuantity)
	}
}

func TestMergeUserFillThenSnapshotDoesNotDouble(t *testing.T) {
	t.Parallel()
	eventAt := time.UnixMilli(1_700_000_000_001)
	current := Order{
		Status: "open", Quantity: "10", FilledQuantity: "0", AveragePrice: "0",
	}
	afterFill, err := mergeOrderUpdate(current, VenueResult{Status: "open"}, eventAt, []OrderFill{
		{TradeID: "t1", Quantity: "1", Price: "100", ExecutedAt: eventAt},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if afterFill.FilledQuantity != "0" || afterFill.Status != "open" {
		t.Fatalf("after fill=%+v", afterFill)
	}
	merged, err := mergeOrderUpdate(afterFill, VenueResult{
		Status: "open", FilledQuantity: "1", AveragePrice: "100",
	}, eventAt, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if merged.FilledQuantity != "1" {
		t.Fatalf("after snapshot=%s", merged.FilledQuantity)
	}
}

func TestMergeIOCPlaceThenUserFillKeepsCumulative(t *testing.T) {
	t.Parallel()
	for _, qty := range []string{"47", "92"} {
		current := Order{
			Status: "filled", Quantity: "100", FilledQuantity: qty, AveragePrice: "1",
		}
		merged, err := mergeOrderUpdate(current, VenueResult{Status: "filled"}, time.UnixMilli(2), []OrderFill{
			{TradeID: "fill-" + qty, Quantity: qty, Price: "1", ExecutedAt: time.UnixMilli(2)},
		}, true)
		if err != nil {
			t.Fatal(err)
		}
		if merged.FilledQuantity != qty || merged.Status != "filled" {
			t.Fatalf("qty=%s merged=%+v", qty, merged)
		}
	}
}

func TestMergeSnapshotThenUserFillAtSameTimeDoesNotDouble(t *testing.T) {
	t.Parallel()
	watermark := time.UnixMilli(1_700_000_000_000)
	current := Order{
		Status: "open", Quantity: "10", FilledQuantity: "2", AveragePrice: "100",
		LastVenueEventAt: watermark,
	}
	merged, err := mergeOrderUpdate(current, VenueResult{Status: "open"}, watermark, []OrderFill{
		{TradeID: "same", Quantity: "2", Price: "100", ExecutedAt: watermark},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if merged.FilledQuantity != "2" {
		t.Fatalf("filled=%s", merged.FilledQuantity)
	}
}

func TestErrorsIsCanceledIgnoresAmbiguousCancelText(t *testing.T) {
	t.Parallel()
	if errorsIsCanceled(errors.New("Order was never placed, already canceled, or filled.")) {
		t.Fatal("venue cancel text must not be treated as context.Canceled")
	}
	if errorsIsCanceled(exchange.ErrAmbiguousCancel) {
		t.Fatal("ErrAmbiguousCancel must not match errorsIsCanceled")
	}
	if !errorsIsCanceled(context.Canceled) {
		t.Fatal("context.Canceled must match errorsIsCanceled")
	}
}
