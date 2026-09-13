package trader

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

const (
	orderFillAverageTerminalTTL          = 10 * time.Minute
	orderFillAveragePruneScan            = 8
	orderFillAverageCoverageWarnInterval = 10 * time.Second
)

type orderFillAveragePolicy func(Order) bool

func hyperliquidArbitrageFillAverageEnabled(order Order) bool {
	return strings.EqualFold(strings.TrimSpace(order.Exchange), "hyperliquid") &&
		strings.TrimSpace(order.ArbitrageExecutionID) != ""
}

type orderFillAverageState struct {
	mu sync.Mutex

	refs      int
	terminal  bool
	expiresAt time.Time

	ensured          bool
	authoritativeQty decimal.Decimal
	fillQty          decimal.Decimal
	fillNotional     decimal.Decimal
	lastPricedQty    decimal.Decimal
	seenTradeIDs     map[string]struct{}
	seededPriced     bool
	averageFinalized bool
	lastCoverageWarn time.Time
}

type orderFillAveragePending struct {
	orderID                   string
	newTradeIDs               map[string]struct{}
	deltaFillQty              decimal.Decimal
	deltaFillNotional         decimal.Decimal
	candidateAuthoritativeQty decimal.Decimal
	candidateLastPricedQty    decimal.Decimal
	writeAverage              bool
}

type orderFillAverageHandle struct {
	orderID string
	state   *orderFillAverageState
}

type orderFillAverageTracker struct {
	mu     sync.Mutex
	byID   map[string]*orderFillAverageState
	now    func() time.Time
	policy orderFillAveragePolicy
}

func newOrderFillAverageTracker(policy orderFillAveragePolicy) *orderFillAverageTracker {
	if policy == nil {
		policy = func(Order) bool { return false }
	}
	return &orderFillAverageTracker{
		byID:   make(map[string]*orderFillAverageState),
		now:    time.Now,
		policy: policy,
	}
}

func (t *orderFillAverageTracker) enabled(order Order) bool {
	return t != nil && t.policy != nil && t.policy(order)
}

func (t *orderFillAverageTracker) Acquire(orderID string) orderFillAverageHandle {
	orderID = strings.TrimSpace(orderID)
	t.mu.Lock()
	t.pruneLocked(orderID)
	state := t.byID[orderID]
	if state == nil {
		state = &orderFillAverageState{seenTradeIDs: make(map[string]struct{})}
		t.byID[orderID] = state
	}
	state.refs++
	t.mu.Unlock()
	return orderFillAverageHandle{orderID: orderID, state: state}
}

func (t *orderFillAverageTracker) Prepare(
	handle orderFillAverageHandle,
	order Order,
	status string,
	authoritativeQty string,
	incoming []OrderFill,
	inserted []OrderFill,
) (orderFillAveragePending, string, bool) {
	state := handle.state
	t.ensureLocked(state, order)
	fills := incoming
	if state.seededPriced {
		fills = inserted
	}
	pending := orderFillAveragePending{
		orderID:                   handle.orderID,
		newTradeIDs:               make(map[string]struct{}),
		candidateAuthoritativeQty: parseDecimal(authoritativeQty),
		candidateLastPricedQty:    state.lastPricedQty,
	}
	for _, fill := range fills {
		observeFillLocked(state, &pending, fill)
	}
	candidateFillQty := state.fillQty.Add(pending.deltaFillQty)
	candidateNotional := state.fillNotional.Add(pending.deltaFillNotional)
	auth := pending.candidateAuthoritativeQty
	if !state.averageFinalized &&
		terminalStatus(status) &&
		auth.IsPositive() &&
		candidateFillQty.Equal(auth) &&
		candidateFillQty.IsPositive() {
		pending.writeAverage = true
		pending.candidateLastPricedQty = auth
		return pending, candidateNotional.Div(candidateFillQty).String(), true
	}
	if auth.IsPositive() && !candidateFillQty.Equal(auth) {
		warnFillCoverageLocked(state, handle.orderID, candidateFillQty, auth)
	}
	return pending, "", false
}

func (t *orderFillAverageTracker) Commit(handle orderFillAverageHandle, pending orderFillAveragePending) {
	if pending.orderID == "" || handle.state == nil {
		return
	}
	state := handle.state
	if state.seenTradeIDs == nil {
		state.seenTradeIDs = make(map[string]struct{})
	}
	for tradeID := range pending.newTradeIDs {
		state.seenTradeIDs[tradeID] = struct{}{}
	}
	state.fillQty = state.fillQty.Add(pending.deltaFillQty)
	state.fillNotional = state.fillNotional.Add(pending.deltaFillNotional)
	state.authoritativeQty = pending.candidateAuthoritativeQty
	if pending.writeAverage {
		state.lastPricedQty = pending.candidateLastPricedQty
		state.averageFinalized = true
	}
}

func (t *orderFillAverageTracker) Abort(orderFillAveragePending) {}

func (t *orderFillAverageTracker) Release(
	handle orderFillAverageHandle,
	markTerminal bool,
	expiresAt time.Time,
) {
	if handle.state == nil || handle.orderID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := handle.state
	if state.refs > 0 {
		state.refs--
	}
	if markTerminal {
		state.terminal = true
		state.expiresAt = expiresAt
	}
	if t.canPruneLocked(handle.orderID, state, t.now()) {
		delete(t.byID, handle.orderID)
	}
}

func (t *orderFillAverageTracker) shouldMarkTerminal(handle orderFillAverageHandle, status string) bool {
	return handle.state != nil && terminalStatus(status)
}

func (t *orderFillAverageTracker) ensureLocked(state *orderFillAverageState, order Order) {
	if state.ensured {
		return
	}
	state.ensured = true
	filled := parseDecimal(order.FilledQuantity)
	average := parseDecimal(order.AveragePrice)
	state.authoritativeQty = filled
	if state.seenTradeIDs == nil {
		state.seenTradeIDs = make(map[string]struct{})
	}
	if average.IsPositive() {
		state.fillQty = filled
		state.fillNotional = filled.Mul(average)
		state.lastPricedQty = filled
		state.seededPriced = true
		state.averageFinalized = true
		return
	}
	state.fillQty = decimal.Zero
	state.fillNotional = decimal.Zero
	state.lastPricedQty = decimal.Zero
	state.seededPriced = false
	state.averageFinalized = false
}

func observeFillLocked(state *orderFillAverageState, pending *orderFillAveragePending, fill OrderFill) {
	tradeID := strings.TrimSpace(fill.TradeID)
	if tradeID == "" {
		return
	}
	if _, seen := state.seenTradeIDs[tradeID]; seen {
		return
	}
	if _, seen := pending.newTradeIDs[tradeID]; seen {
		return
	}
	quantity, err := decimal.NewFromString(strings.TrimSpace(fill.Quantity))
	if err != nil || !quantity.IsPositive() {
		return
	}
	price, err := decimal.NewFromString(strings.TrimSpace(fill.Price))
	if err != nil || !price.IsPositive() {
		return
	}
	pending.newTradeIDs[tradeID] = struct{}{}
	pending.deltaFillQty = pending.deltaFillQty.Add(quantity)
	pending.deltaFillNotional = pending.deltaFillNotional.Add(quantity.Mul(price))
}

func warnFillCoverageLocked(
	state *orderFillAverageState,
	orderID string,
	fillQty, authoritativeQty decimal.Decimal,
) {
	now := time.Now()
	if !state.lastCoverageWarn.IsZero() &&
		now.Sub(state.lastCoverageWarn) < orderFillAverageCoverageWarnInterval {
		return
	}
	state.lastCoverageWarn = now
	event := "hyperliquid_fill_coverage_incomplete"
	if fillQty.GreaterThan(authoritativeQty) {
		event = "hyperliquid_fill_quantity_ahead_of_order"
	}
	slog.Warn(
		event,
		slog.String("order_id", orderID),
		slog.String("fill_qty", fillQty.String()),
		slog.String("authoritative_qty", authoritativeQty.String()),
	)
}

func (t *orderFillAverageTracker) pruneLocked(currentID string) {
	now := t.now()
	if state := t.byID[currentID]; t.canPruneLocked(currentID, state, now) {
		delete(t.byID, currentID)
	}
	scanned := 0
	for id, state := range t.byID {
		if id == currentID {
			continue
		}
		scanned++
		if t.canPruneLocked(id, state, now) {
			delete(t.byID, id)
		}
		if scanned >= orderFillAveragePruneScan {
			return
		}
	}
}

func (t *orderFillAverageTracker) canPruneLocked(
	orderID string,
	state *orderFillAverageState,
	now time.Time,
) bool {
	if state == nil || !state.terminal || state.expiresAt.IsZero() || state.refs != 0 {
		return false
	}
	if now.Before(state.expiresAt) {
		return false
	}
	return t.byID[orderID] == state
}
