package trader

import (
	"sync"
	"testing"
	"time"
)

func TestHyperliquidArbitrageFillAverageEnabled(t *testing.T) {
	t.Parallel()
	if hyperliquidArbitrageFillAverageEnabled(Order{Exchange: "hyperliquid"}) {
		t.Fatal("manual hyperliquid order should be disabled")
	}
	if hyperliquidArbitrageFillAverageEnabled(Order{
		Exchange: "binance", ArbitrageExecutionID: "arb",
	}) {
		t.Fatal("non-hyperliquid arbitrage should be disabled")
	}
	if !hyperliquidArbitrageFillAverageEnabled(Order{
		Exchange: "Hyperliquid", ArbitrageExecutionID: "arb",
	}) {
		t.Fatal("hyperliquid arbitrage should be enabled")
	}
}

func TestSkipWatermarkMergeLeavesQuantityForAverageOverlay(t *testing.T) {
	t.Parallel()
	watermark := time.UnixMilli(1_700_000_000_000)
	current := Order{
		ID: "order-skip", Exchange: "hyperliquid", ArbitrageExecutionID: "arb",
		Status: "open", Quantity: "10", FilledQuantity: "10", AveragePrice: "0",
		LastVenueEventAt: watermark, ErrorCode: "open",
	}
	merged, err := mergeOrderUpdate(current, VenueResult{Status: "filled"}, watermark, []OrderFill{
		{TradeID: "t1", Quantity: "4", Price: "100"},
		{TradeID: "t2", Quantity: "6", Price: "110"},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if merged.FilledQuantity != "10" || merged.Status != "open" ||
		merged.AveragePrice != "0" || merged.ErrorCode != "open" {
		t.Fatalf("skip merge changed order: %+v", merged)
	}
	tracker := newOrderFillAverageTracker(hyperliquidArbitrageFillAverageEnabled)
	if _, wrote := applyFillAverage(t, tracker, current, merged.FilledQuantity, []OrderFill{
		{TradeID: "t1", Quantity: "4", Price: "100"},
		{TradeID: "t2", Quantity: "6", Price: "110"},
	}, nil, "open", false); wrote {
		t.Fatal("open order must not persist average")
	}
	avg, wrote := applyFillAverage(t, tracker, current, merged.FilledQuantity, nil, nil, "filled", true)
	if !wrote || avg != "106" {
		t.Fatalf("terminal overlay average=%q wrote=%v", avg, wrote)
	}
}

func TestOrderFillAveragePartialThenComplete(t *testing.T) {
	t.Parallel()
	tracker := newOrderFillAverageTracker(hyperliquidArbitrageFillAverageEnabled)
	order := Order{
		ID: "order-partial", Exchange: "hyperliquid", ArbitrageExecutionID: "arb",
		FilledQuantity: "4", AveragePrice: "0", Status: "partially_filled",
	}
	if _, wrote := applyFillAverage(t, tracker, order, "4", []OrderFill{
		{TradeID: "t1", Quantity: "4", Price: "100"},
	}, nil, "partially_filled", false); wrote {
		t.Fatal("open coverage must not persist average")
	}
	if _, _, _, present := tracker.lifecycle("order-partial"); !present {
		t.Fatal("must keep byID after first average")
	}
	order.FilledQuantity = "10"
	avg, wrote := applyFillAverage(t, tracker, order, "10", []OrderFill{
		{TradeID: "t2", Quantity: "6", Price: "110"},
	}, nil, "filled", true)
	if !wrote || avg != "106" {
		t.Fatalf("second average=%q wrote=%v", avg, wrote)
	}
}

func TestOrderFillAverageDuplicateTradeID(t *testing.T) {
	t.Parallel()
	tracker := newOrderFillAverageTracker(hyperliquidArbitrageFillAverageEnabled)
	order := Order{
		ID: "order-dup", Exchange: "hyperliquid", ArbitrageExecutionID: "arb",
		FilledQuantity: "4", Status: "open",
	}
	fills := []OrderFill{{TradeID: "t1", Quantity: "4", Price: "100"}}
	if _, wrote := applyFillAverage(t, tracker, order, "4", fills, nil, "filled", true); !wrote {
		t.Fatal("first fill should write")
	}
	if _, wrote := applyFillAverage(t, tracker, order, "4", fills, nil, "filled", true); wrote {
		t.Fatal("duplicate tid must not rewrite")
	}
}

func TestOrderFillAverageZeroAverageRebuildsFromIncoming(t *testing.T) {
	t.Parallel()
	tracker := newOrderFillAverageTracker(hyperliquidArbitrageFillAverageEnabled)
	order := Order{
		ID: "order-rebuild", Exchange: "hyperliquid", ArbitrageExecutionID: "arb",
		FilledQuantity: "10", AveragePrice: "0", Status: "filled",
	}
	avg, wrote := applyFillAverage(t, tracker, order, "10", []OrderFill{
		{TradeID: "a", Quantity: "4", Price: "100"},
		{TradeID: "b", Quantity: "6", Price: "110"},
	}, nil, "filled", true)
	if !wrote || avg != "106" {
		t.Fatalf("rebuild average=%q wrote=%v", avg, wrote)
	}
}

func TestOrderFillAverageSeededIgnoresIncomingSnapshot(t *testing.T) {
	t.Parallel()
	tracker := newOrderFillAverageTracker(hyperliquidArbitrageFillAverageEnabled)
	order := Order{
		ID: "order-seeded", Exchange: "hyperliquid", ArbitrageExecutionID: "arb",
		FilledQuantity: "10", AveragePrice: "100", Status: "open",
	}
	if _, wrote := applyFillAverage(t, tracker, order, "10", []OrderFill{
		{TradeID: "snap-1", Quantity: "4", Price: "90"},
		{TradeID: "snap-2", Quantity: "6", Price: "110"},
	}, nil, "open", false); wrote {
		t.Fatal("seeded snapshot incoming must not write")
	}
	if _, wrote := applyFillAverage(t, tracker, order, "16", nil, []OrderFill{
		{TradeID: "new", Quantity: "6", Price: "110"},
	}, "filled", true); wrote {
		t.Fatal("seeded finalized average must not rewrite")
	}
}

func TestOrderFillAverageIncompleteAndAhead(t *testing.T) {
	t.Parallel()
	tracker := newOrderFillAverageTracker(hyperliquidArbitrageFillAverageEnabled)
	order := Order{
		ID: "order-gap", Exchange: "hyperliquid", ArbitrageExecutionID: "arb",
		FilledQuantity: "10", Status: "open",
	}
	if _, wrote := applyFillAverage(t, tracker, order, "10", []OrderFill{
		{TradeID: "t1", Quantity: "4", Price: "100"},
	}, nil, "open", false); wrote {
		t.Fatal("incomplete must not write")
	}
	order = Order{
		ID: "order-ahead", Exchange: "hyperliquid", ArbitrageExecutionID: "arb",
		FilledQuantity: "4", Status: "open",
	}
	if _, wrote := applyFillAverage(t, tracker, order, "4", []OrderFill{
		{TradeID: "t1", Quantity: "4", Price: "100"},
		{TradeID: "t2", Quantity: "6", Price: "110"},
	}, nil, "open", false); wrote {
		t.Fatal("ahead must not write")
	}
}

func TestOrderFillAverageTerminalThenLateFills(t *testing.T) {
	t.Parallel()
	tracker := newOrderFillAverageTracker(hyperliquidArbitrageFillAverageEnabled)
	order := Order{
		ID: "order-late", Exchange: "hyperliquid", ArbitrageExecutionID: "arb",
		FilledQuantity: "10", AveragePrice: "0", Status: "filled",
	}
	if _, wrote := applyFillAverage(t, tracker, order, "10", nil, nil, "filled", true); wrote {
		t.Fatal("terminal without fills must keep average 0")
	}
	if _, wrote := applyFillAverage(t, tracker, order, "10", []OrderFill{
		{TradeID: "t1", Quantity: "4", Price: "100"},
	}, nil, "filled", true); wrote {
		t.Fatal("partial late fills must keep average 0")
	}
	avg, wrote := applyFillAverage(t, tracker, order, "10", []OrderFill{
		{TradeID: "t2", Quantity: "6", Price: "110"},
	}, nil, "filled", true)
	if !wrote || avg != "106" {
		t.Fatalf("complete late fills average=%q wrote=%v", avg, wrote)
	}
	if _, wrote := applyFillAverage(t, tracker, order, "10", []OrderFill{
		{TradeID: "t3", Quantity: "1", Price: "120"},
	}, nil, "filled", true); wrote {
		t.Fatal("finalized average must not rewrite")
	}
}

func TestOrderFillAverageAbortAllowsRetry(t *testing.T) {
	t.Parallel()
	tracker := newOrderFillAverageTracker(hyperliquidArbitrageFillAverageEnabled)
	order := Order{
		ID: "order-abort", Exchange: "hyperliquid", ArbitrageExecutionID: "arb",
		FilledQuantity: "4", Status: "open",
	}
	handle := tracker.Acquire(order.ID)
	handle.state.mu.Lock()
	pending, avg, wrote := tracker.Prepare(handle, order, "filled", "4", []OrderFill{
		{TradeID: "t1", Quantity: "4", Price: "100"},
	}, nil)
	if !wrote || avg != "100" {
		t.Fatalf("prepare=%q wrote=%v", avg, wrote)
	}
	tracker.Abort(pending)
	handle.state.mu.Unlock()
	tracker.Release(handle, false, time.Time{})

	avg, wrote = applyFillAverage(t, tracker, order, "4", []OrderFill{
		{TradeID: "t1", Quantity: "4", Price: "100"},
	}, nil, "filled", true)
	if !wrote || avg != "100" {
		t.Fatalf("retry after abort average=%q wrote=%v", avg, wrote)
	}
}

func TestOrderFillAverageCommitDoesNotMarkTerminal(t *testing.T) {
	t.Parallel()
	tracker := newOrderFillAverageTracker(hyperliquidArbitrageFillAverageEnabled)
	order := Order{
		ID: "order-life", Exchange: "hyperliquid", ArbitrageExecutionID: "arb",
		FilledQuantity: "4", Status: "filled",
	}
	handle := tracker.Acquire(order.ID)
	handle.state.mu.Lock()
	pending, _, wrote := tracker.Prepare(handle, order, "filled", "4", []OrderFill{
		{TradeID: "t1", Quantity: "4", Price: "100"},
	}, nil)
	if !wrote {
		t.Fatal("expected write")
	}
	tracker.Commit(handle, pending)
	handle.state.mu.Unlock()
	_, terminal, _, present := tracker.lifecycle(order.ID)
	if !present || terminal {
		t.Fatalf("terminal after commit=%v present=%v", terminal, present)
	}
	expires := tracker.now().Add(orderFillAverageTerminalTTL)
	tracker.Release(handle, true, expires)
	_, terminal, _, present = tracker.lifecycle(order.ID)
	if !present || !terminal {
		t.Fatalf("expected terminal retained, terminal=%v present=%v", terminal, present)
	}
}

func TestOrderFillAveragePruneRequiresExpiredZeroRefs(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	tracker := newOrderFillAverageTracker(hyperliquidArbitrageFillAverageEnabled)
	tracker.now = func() time.Time { return now }
	order := Order{
		ID: "order-prune", Exchange: "hyperliquid", ArbitrageExecutionID: "arb",
		FilledQuantity: "4", Status: "filled",
	}
	applyFillAverage(t, tracker, order, "4", []OrderFill{
		{TradeID: "t1", Quantity: "4", Price: "100"},
	}, nil, "filled", true)
	if _, _, _, present := tracker.lifecycle(order.ID); !present {
		t.Fatal("must not prune immediately")
	}
	held := tracker.Acquire(order.ID)
	now = now.Add(orderFillAverageTerminalTTL + time.Second)
	other := tracker.Acquire("other")
	if _, _, _, present := tracker.lifecycle(order.ID); !present {
		t.Fatal("must not prune while refs>0")
	}
	tracker.Release(other, false, time.Time{})
	tracker.Release(held, false, time.Time{})
	if _, _, _, present := tracker.lifecycle(order.ID); present {
		t.Fatal("expired zero-ref state should prune")
	}
}

func TestOrderFillAverageConcurrentAcquireSamePointer(t *testing.T) {
	t.Parallel()
	tracker := newOrderFillAverageTracker(hyperliquidArbitrageFillAverageEnabled)
	order := Order{
		ID: "order-race", Exchange: "hyperliquid", ArbitrageExecutionID: "arb",
		FilledQuantity: "4", Status: "open",
	}
	first := tracker.Acquire(order.ID)
	first.state.mu.Lock()
	started := make(chan struct{})
	got := make(chan *orderFillAverageState, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		second := tracker.Acquire(order.ID)
		close(started)
		second.state.mu.Lock()
		got <- second.state
		second.state.mu.Unlock()
		tracker.Release(second, false, time.Time{})
	}()
	<-started
	if tracker.lookup(order.ID) != first.state {
		t.Fatal("second acquire must keep same pointer")
	}
	pending, _, _ := tracker.Prepare(first, order, "filled", "4", []OrderFill{
		{TradeID: "t1", Quantity: "4", Price: "100"},
	}, nil)
	tracker.Commit(first, pending)
	first.state.mu.Unlock()
	secondState := <-got
	if secondState != first.state {
		t.Fatal("overlapping acquire used a different state")
	}
	tracker.Release(first, false, time.Time{})
	wg.Wait()
}

func applyFillAverage(
	t *testing.T,
	tracker *orderFillAverageTracker,
	order Order,
	authoritative string,
	incoming, inserted []OrderFill,
	status string,
	mark bool,
) (string, bool) {
	t.Helper()
	handle := tracker.Acquire(order.ID)
	handle.state.mu.Lock()
	pending, avg, wrote := tracker.Prepare(handle, order, status, authoritative, incoming, inserted)
	tracker.Commit(handle, pending)
	markTerminal := mark && tracker.shouldMarkTerminal(handle, status)
	handle.state.mu.Unlock()
	expires := time.Time{}
	if markTerminal {
		expires = tracker.now().Add(orderFillAverageTerminalTTL)
	}
	tracker.Release(handle, markTerminal, expires)
	return avg, wrote
}

func (t *orderFillAverageTracker) lifecycle(
	orderID string,
) (refs int, terminal bool, expiresAt time.Time, present bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.byID[orderID]
	if state == nil {
		return 0, false, time.Time{}, false
	}
	return state.refs, state.terminal, state.expiresAt, true
}

func (t *orderFillAverageTracker) lookup(orderID string) *orderFillAverageState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byID[orderID]
}
