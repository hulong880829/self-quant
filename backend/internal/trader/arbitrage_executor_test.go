package trader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/account/portfolio"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

type executorFailStore struct {
	arbitrageStore
	mu                sync.Mutex
	execution         ArbitrageExecution
	executions        []ArbitrageExecution
	orders            []Order
	combo             ArbitrageCombination
	failures          int
	closeFailures     int
	cleared           int
	events            int
	eventTypes        []string
	eventPayloads     []map[string]any
	finalizeCalls     int
	finalizeResult    confirmedAbsentFinalizeResult
	finalizeErr       error
	activeExecution   *ArbitrageExecution
	prepareErr        error
	prepareCalls      int
	recomputeCalls    int
	getActiveCalls    int
	lastPrepare       PrepareArbitrageHedgeIntentInput
	fastPathMakerFill string
	intentStore       interface {
		CreateIntent(context.Context, Order) (Order, bool, error)
		AppendEvent(context.Context, string, string, map[string]any) error
		GetByOwner(context.Context, string, string) (Order, error)
	}
}

type makerHedgeStore struct {
	*executorFailStore
	orderStore *memoryStore
}

func (s *makerHedgeStore) RecomputeArbitrageBasePositions(
	_ context.Context,
	_ string,
) (ArbitrageCombination, error) {
	s.orderStore.mu.Lock()
	legA, legB := decimal.Zero, decimal.Zero
	hasFilled := false
	for _, order := range s.orderStore.orders {
		filled := parseDecimal(order.FilledQuantity)
		if !filled.IsPositive() && !filled.IsNegative() {
			continue
		}
		hasFilled = true
		if strings.EqualFold(order.Side, "sell") {
			filled = filled.Neg()
		}
		if order.ArbitrageLeg == "a" {
			legA = legA.Add(filled)
		} else if order.ArbitrageLeg == "b" {
			legB = legB.Add(filled)
		}
	}
	s.orderStore.mu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.recomputeCalls++
	if hasFilled {
		s.combo.LegABasePosition = legA.String()
		s.combo.LegBBasePosition = legB.String()
		s.combo.CarryBaseQuantity = legA.Add(legB).String()
	}
	return s.combo, nil
}

func (s *makerHedgeStore) RecomputeArbitrageBasePositionsForExecution(
	ctx context.Context,
	executionID string,
) (ArbitrageCombination, error) {
	combo, err := s.RecomputeArbitrageBasePositions(ctx, "")
	if err != nil {
		return combo, err
	}
	s.syncPairedExecutionFills(ctx, executionID)
	return combo, nil
}

func (s *makerHedgeStore) ListArbitrageOrders(
	context.Context, string, string,
) ([]Order, error) {
	s.mu.Lock()
	items := append([]Order(nil), s.orders...)
	s.mu.Unlock()
	if s.orderStore == nil {
		return items, nil
	}
	s.orderStore.mu.Lock()
	defer s.orderStore.mu.Unlock()
	for _, order := range s.orderStore.orders {
		items = append(items, order)
	}
	return items, nil
}

type countingSpotSnapshots struct {
	mu       sync.Mutex
	snapshot portfolio.Snapshot
	err      error
	calls    int
}

type postOnlyOrderStore struct {
	*memoryStore
}

func (s *postOnlyOrderStore) CreateArbitrageIntents(
	ctx context.Context,
	orders []Order,
) ([]Order, []bool, error) {
	stored := make([]Order, len(orders))
	created := make([]bool, len(orders))
	for index, order := range orders {
		item, inserted, err := s.CreateIntent(ctx, order)
		if err != nil {
			return nil, nil, err
		}
		stored[index], created[index] = item, inserted
	}
	return stored, created, nil
}

func (s *postOnlyOrderStore) ApplyStreamUpdate(
	ctx context.Context,
	orderID string,
	update StreamUpdate,
) (Order, error) {
	return s.UpdateResult(ctx, orderID, update.Result)
}

func (s *countingSpotSnapshots) Snapshot(
	context.Context, string, portfolio.Credentials,
) (portfolio.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.snapshot, s.err
}

func (s *executorFailStore) ClearArbitrageFailureIfUnchanged(
	_ context.Context, _ string, expected string,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.combo.ErrorMessage != expected || s.combo.PositionUncertain || s.combo.CircuitOpen {
		return ArbitrageCombination{}, ErrNotFound
	}
	s.cleared++
	s.combo.ErrorMessage = ""
	s.combo.ConsecutiveFailures = 0
	s.combo.RuntimeState = "monitoring"
	return s.combo, nil
}

func (s *executorFailStore) UpdateArbitrageExecution(
	_ context.Context, execution ArbitrageExecution,
) (ArbitrageExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if terminalArbitrageExecutionStatus(s.execution.Status) &&
		!terminalArbitrageExecutionStatus(execution.Status) {
		return ArbitrageExecution{}, ErrArbitrageExecutionTerminal
	}
	s.execution = execution
	for index := range s.executions {
		if s.executions[index].ID == execution.ID {
			if terminalArbitrageExecutionStatus(s.executions[index].Status) &&
				!terminalArbitrageExecutionStatus(execution.Status) {
				return ArbitrageExecution{}, ErrArbitrageExecutionTerminal
			}
			s.executions[index] = execution
			return execution, nil
		}
	}
	s.executions = append(s.executions, execution)
	return execution, nil
}

func (s *executorFailStore) RecordArbitrageFailure(
	_ context.Context, _, message string,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures++
	s.combo.ErrorMessage = message
	s.combo.ConsecutiveFailures++
	return s.combo, nil
}

func (s *executorFailStore) RecordArbitrageCloseFailure(
	_ context.Context, _, key, message string,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeFailures++
	s.combo.ErrorMessage = message
	s.combo.LastFailureKey = key
	s.combo.ConsecutiveFailures++
	if arbitrageFlattening(s.combo) && s.combo.Status == "running" {
		s.combo.RuntimeState = "backoff"
	} else {
		s.combo.RuntimeState = "closing"
	}
	s.combo.CircuitOpen = false
	s.combo.NextRetryAt = time.Now().UTC().Add(2 * time.Second)
	return s.combo, nil
}

func (s *executorFailStore) GetArbitrageCombinationByOwner(
	_ context.Context, _, _ string,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.combo, nil
}

func (s *executorFailStore) UpdateArbitrageCombinationRuntime(
	_ context.Context, item ArbitrageCombination,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.combo = item
	return item, nil
}

func (s *executorFailStore) FinalizeConfirmedAbsentZeroFillExecution(
	_ context.Context, _ string,
) (confirmedAbsentFinalizeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalizeCalls++
	if s.finalizeErr != nil {
		return confirmedAbsentFinalizeSkipped, s.finalizeErr
	}
	if s.finalizeResult == "" {
		return confirmedAbsentFinalizeSkipped, nil
	}
	return s.finalizeResult, nil
}

func (s *executorFailStore) GetActiveArbitrageExecution(
	_ context.Context, _ string,
) (ArbitrageExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getActiveCalls++
	if s.activeExecution != nil &&
		!terminalArbitrageExecutionStatus(s.activeExecution.Status) {
		return *s.activeExecution, nil
	}
	return ArbitrageExecution{}, ErrNotFound
}

func (s *executorFailStore) FailLastCloseClipUnbalanced(
	_ context.Context,
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	fillA, fillB, remaining string,
) (FailLastCloseClipUnbalancedResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.execution
	if current.ID == "" || current.ID == execution.ID {
		if current.ID != "" {
			execution = current
		} else {
			current = execution
		}
	}
	hasEvent := false
	for _, eventType := range s.eventTypes {
		if eventType == "last_close_clip_unbalanced" {
			hasEvent = true
			break
		}
	}
	combo := s.combo
	if combo.ID == "" {
		combo = combination
	}
	if current.Status == "failed" && hasEvent {
		return FailLastCloseClipUnbalancedResult{Combination: combo, Execution: current}, nil
	}
	if combo.Version != combination.Version ||
		(combo.Status != "running" && combo.Status != "closing") {
		return FailLastCloseClipUnbalancedResult{}, ErrArbitrageLastCloseClipUnbalancedConflict
	}
	if terminalArbitrageExecutionStatus(current.Status) {
		return FailLastCloseClipUnbalancedResult{}, ErrArbitrageLastCloseClipUnbalancedConflict
	}
	message := lastCloseClipUnbalancedErrorMessage(execution.ID, fillA, fillB, remaining)
	combo.RuntimeState = "manual_intervention"
	combo.ErrorMessage = message
	combo.CircuitOpen = true
	combo.Version++
	current.Status = "failed"
	current.ErrorMessage = message
	if current.ClosedAt.IsZero() || current.ClosedAt.Equal(time.Unix(0, 0).UTC()) {
		current.ClosedAt = time.Now().UTC()
	}
	s.combo = combo
	s.execution = current
	for index := range s.executions {
		if s.executions[index].ID == current.ID {
			s.executions[index] = current
			break
		}
	}
	if s.activeExecution != nil && s.activeExecution.ID == current.ID {
		copied := current
		s.activeExecution = &copied
	}
	if !hasEvent {
		s.events++
		s.eventTypes = append(s.eventTypes, "last_close_clip_unbalanced")
		s.eventPayloads = append(s.eventPayloads, lastCloseClipUnbalancedPayload(fillA, fillB, remaining))
	}
	return FailLastCloseClipUnbalancedResult{Combination: combo, Execution: current}, nil
}

func (s *executorFailStore) PrepareArbitrageHedgeIntent(
	ctx context.Context,
	in PrepareArbitrageHedgeIntentInput,
) (PrepareArbitrageHedgeIntentResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prepareCalls++
	s.lastPrepare = in
	if s.prepareErr != nil {
		return PrepareArbitrageHedgeIntentResult{}, s.prepareErr
	}
	execution := in.Execution
	if s.execution.ID == in.Execution.ID {
		execution.HedgeSequence = s.execution.HedgeSequence
		if s.execution.HedgeOrderID != "" {
			execution.HedgeOrderID = s.execution.HedgeOrderID
		}
		if s.execution.Status != "" {
			execution.Status = s.execution.Status
		}
	}
	if terminalArbitrageExecutionStatus(execution.Status) {
		return PrepareArbitrageHedgeIntentResult{}, ErrArbitrageExecutionTerminal
	}
	combo := s.combo
	if combo.ID == "" {
		combo = in.Combination
	}
	if in.FastPathAdmission {
		if s.activeExecution != nil && s.activeExecution.ID != "" &&
			s.activeExecution.ID != execution.ID {
			return PrepareArbitrageHedgeIntentResult{}, ErrArbitrageHedgeAdmissionConflict
		}
		switch execution.Status {
		case "maker_open", "maker_canceling", "hedging", "":
		default:
			return PrepareArbitrageHedgeIntentResult{}, ErrArbitrageHedgeAdmissionConflict
		}
		makerID := strings.TrimSpace(execution.MakerOrderID)
		if makerID == "" {
			return PrepareArbitrageHedgeIntentResult{}, ErrArbitrageHedgeAdmissionConflict
		}
		if s.intentStore != nil {
			maker, err := s.intentStore.GetByOwner(ctx, combo.OwnerUsername, makerID)
			if err != nil || maker.ArbitrageRole != "maker" ||
				maker.ArbitrageExecutionID != execution.ID ||
				!terminalStatus(maker.Status) {
				return PrepareArbitrageHedgeIntentResult{}, ErrArbitrageHedgeAdmissionConflict
			}
			if s.fastPathMakerFill != "" {
				if !parseDecimal(s.fastPathMakerFill).Equal(parseDecimal(in.ConfirmedMakerFilled)) {
					return PrepareArbitrageHedgeIntentResult{}, ErrArbitrageHedgeAdmissionConflict
				}
			} else if !parseDecimal(maker.FilledQuantity).Equal(parseDecimal(in.ConfirmedMakerFilled)) {
				return PrepareArbitrageHedgeIntentResult{}, ErrArbitrageHedgeAdmissionConflict
			}
		}
		if strings.TrimSpace(execution.HedgeOrderID) != "" {
			return PrepareArbitrageHedgeIntentResult{}, ErrArbitrageHedgeAdmissionConflict
		}
		hedgeFilled := parseDecimal(execution.LegBFilledQuantity)
		if strings.EqualFold(combo.MakerLeg, "b") {
			hedgeFilled = parseDecimal(execution.LegAFilledQuantity)
		}
		if !hedgeFilled.Equal(parseDecimal(in.ConfirmedHedgeFilled)) {
			return PrepareArbitrageHedgeIntentResult{}, ErrArbitrageHedgeAdmissionConflict
		}
	}
	if in.AggregateCarry {
		if s.activeExecution != nil && s.activeExecution.ID != "" &&
			s.activeExecution.ID != execution.ID {
			return PrepareArbitrageHedgeIntentResult{}, ErrArbitrageHedgeAdmissionConflict
		}
		expected := parseDecimal(in.ExpectedCarryQuantity)
		if strings.TrimSpace(in.ExpectedCarryQuantity) == "" ||
			!parseDecimal(combo.CarryBaseQuantity).Equal(expected) {
			return PrepareArbitrageHedgeIntentResult{}, ErrArbitrageHedgeAdmissionConflict
		}
		if !parseDecimal(in.Order.Quantity).IsPositive() ||
			parseDecimal(in.Order.Quantity).GreaterThan(expected.Abs()) {
			return PrepareArbitrageHedgeIntentResult{}, ErrArbitrageHedgeAdmissionConflict
		}
		makerID := strings.TrimSpace(execution.MakerOrderID)
		if makerID != "" && s.intentStore != nil {
			maker, err := s.intentStore.GetByOwner(ctx, combo.OwnerUsername, makerID)
			if err != nil || maker.ArbitrageRole != "maker" ||
				maker.ArbitrageExecutionID != execution.ID ||
				!terminalStatus(maker.Status) {
				return PrepareArbitrageHedgeIntentResult{}, ErrArbitrageHedgeAdmissionConflict
			}
		}
		_, keyFound := s.lookupHedgeIntentLocked(in.Order.IdempotencyKey)
		if !keyFound && strings.TrimSpace(execution.HedgeOrderID) != "" {
			if err := s.admitAggregateCarryHedgeReplacementLocked(
				ctx, combo.OwnerUsername, execution, in.Order.ArbitrageLeg,
			); err != nil {
				return PrepareArbitrageHedgeIntentResult{}, err
			}
		}
	}
	var stored Order
	created := false
	if s.intentStore != nil {
		item, inserted, err := s.intentStore.CreateIntent(ctx, in.Order)
		if err != nil {
			return PrepareArbitrageHedgeIntentResult{}, err
		}
		stored, created = item, inserted
		if created {
			_ = s.intentStore.AppendEvent(ctx, stored.ID, "intent", nil)
			_ = s.intentStore.AppendEvent(ctx, stored.ID, "submitted", nil)
		}
	} else {
		for _, existing := range s.orders {
			if existing.IdempotencyKey == in.Order.IdempotencyKey {
				stored = existing
				break
			}
		}
		if stored.ID == "" {
			stored = in.Order
			if stored.ID == "" {
				stored.ID = "hedge-" + stored.IdempotencyKey
			}
			stored.Status = "pending"
			s.orders = append(s.orders, stored)
			created = true
		}
	}
	if !created {
		if stored.ArbitrageExecutionID != execution.ID && stored.ArbitrageExecutionID != in.Execution.ID ||
			stored.ArbitrageRole != in.Order.ArbitrageRole ||
			stored.ArbitrageLeg != in.Order.ArbitrageLeg ||
			stored.RequestFingerprint != in.Order.RequestFingerprint {
			if stored.ArbitrageRole != "" && stored.ArbitrageRole != in.Order.ArbitrageRole {
				return PrepareArbitrageHedgeIntentResult{}, ErrIdempotencyConflict
			}
			if stored.ArbitrageLeg != "" && stored.ArbitrageLeg != in.Order.ArbitrageLeg {
				return PrepareArbitrageHedgeIntentResult{}, ErrIdempotencyConflict
			}
			if stored.RequestFingerprint != "" &&
				stored.RequestFingerprint != in.Order.RequestFingerprint {
				return PrepareArbitrageHedgeIntentResult{}, ErrIdempotencyConflict
			}
		}
	} else if execution.HedgeSequence != in.ExpectedSequence {
		return PrepareArbitrageHedgeIntentResult{}, ErrArbitrageHedgeSequence
	}
	previousStatus := execution.Status
	if created {
		execution.HedgeSequence++
	}
	execution.HedgeOrderID = stored.ID
	execution.Status = "hedging"
	execution.ErrorMessage = ""
	s.execution = execution
	updated := false
	for index := range s.executions {
		if s.executions[index].ID == execution.ID {
			s.executions[index] = execution
			updated = true
			break
		}
	}
	if !updated {
		s.executions = append(s.executions, execution)
	}
	protected := combo.PositionUncertain ||
		combo.RuntimeState == "position_uncertain" ||
		combo.RuntimeState == "closing" ||
		combo.RuntimeState == "manual_intervention" ||
		combo.Status == "closing" || combo.Status == "closed" || combo.Status == "failed"
	if !protected {
		combo.RuntimeState = "hedging"
		combo.ErrorMessage = ""
	}
	s.combo = combo
	if created && previousStatus != "hedging" {
		s.events++
		s.eventTypes = append(s.eventTypes, "hedge_started")
	}
	return PrepareArbitrageHedgeIntentResult{
		Order: stored, Created: created, Execution: execution, Combination: combo,
	}, nil
}

func (s *executorFailStore) lookupHedgeIntentLocked(key string) (Order, bool) {
	for _, existing := range s.orders {
		if existing.IdempotencyKey == key {
			return existing, true
		}
	}
	store := memoryStoreFromIntent(s.intentStore)
	if store == nil {
		return Order{}, false
	}
	return store.getByIdempotencyKey(key)
}

func (s *executorFailStore) findOrderLocked(
	ctx context.Context,
	owner, orderID string,
) (Order, bool) {
	for _, order := range s.orders {
		if order.ID == orderID {
			return order, true
		}
	}
	if s.intentStore != nil && owner != "" {
		order, err := s.intentStore.GetByOwner(ctx, owner, orderID)
		if err == nil {
			return order, true
		}
	}
	for _, order := range snapshotIntentOrders(s.intentStore) {
		if order.ID == orderID {
			return order, true
		}
	}
	return Order{}, false
}

func (s *executorFailStore) hasLiveHedgeLocked(executionID string) bool {
	seen := map[string]struct{}{}
	live := func(order Order) bool {
		if order.ID == "" {
			return false
		}
		if _, ok := seen[order.ID]; ok {
			return false
		}
		seen[order.ID] = struct{}{}
		return order.ArbitrageExecutionID == executionID &&
			isArbitrageCarryHedgeRole(order.ArbitrageRole) &&
			!terminalStatus(order.Status)
	}
	for _, order := range s.orders {
		if live(order) {
			return true
		}
	}
	for _, order := range snapshotIntentOrders(s.intentStore) {
		if live(order) {
			return true
		}
	}
	return false
}

func (s *executorFailStore) admitAggregateCarryHedgeReplacementLocked(
	ctx context.Context,
	owner string,
	execution ArbitrageExecution,
	hedgeLeg string,
) error {
	previous, ok := s.findOrderLocked(ctx, owner, strings.TrimSpace(execution.HedgeOrderID))
	if !ok ||
		previous.ArbitrageExecutionID != execution.ID ||
		!isArbitrageCarryHedgeRole(previous.ArbitrageRole) ||
		previous.ArbitrageLeg != hedgeLeg ||
		!terminalStatus(previous.Status) ||
		previous.ReconcileFailures != 0 {
		return ErrArbitrageHedgeAdmissionConflict
	}
	if s.hasLiveHedgeLocked(execution.ID) {
		return ErrArbitrageHedgeAdmissionConflict
	}
	return nil
}

func memoryStoreFromIntent(store any) *memoryStore {
	switch item := store.(type) {
	case *memoryStore:
		return item
	case *postOnlyOrderStore:
		if item == nil {
			return nil
		}
		return item.memoryStore
	case *failingOrderStore:
		if item == nil {
			return nil
		}
		return item.memoryStore
	default:
		return nil
	}
}

func snapshotIntentOrders(store any) []Order {
	memory := memoryStoreFromIntent(store)
	if memory == nil {
		return nil
	}
	return memory.snapshotOrders()
}

func (s *executorFailStore) ListArbitrageOrders(
	context.Context, string, string,
) ([]Order, error) {
	s.mu.Lock()
	items := append([]Order(nil), s.orders...)
	intent := s.intentStore
	s.mu.Unlock()
	for _, order := range snapshotIntentOrders(intent) {
		items = append(items, order)
	}
	return items, nil
}

func (s *executorFailStore) ListArbitrageExecutions(
	context.Context, string, int,
) ([]ArbitrageExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ArbitrageExecution(nil), s.executions...), nil
}

func (s *executorFailStore) AppendArbitrageEvent(
	_ context.Context, _, _, eventType string, payload map[string]any,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events++
	s.eventTypes = append(s.eventTypes, eventType)
	s.eventPayloads = append(s.eventPayloads, payload)
	return nil
}

func (s *executorFailStore) RecomputeArbitrageBasePositions(
	_ context.Context, _ string,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recomputeCalls++
	return s.combo, nil
}

func (s *executorFailStore) RecomputeArbitrageBasePositionsForExecution(
	ctx context.Context, executionID string,
) (ArbitrageCombination, error) {
	combo, err := s.RecomputeArbitrageBasePositions(ctx, "")
	if err != nil {
		return combo, err
	}
	s.syncPairedExecutionFills(ctx, executionID)
	return combo, nil
}

func (s *executorFailStore) syncPairedExecutionFills(ctx context.Context, executionID string) {
	if strings.TrimSpace(executionID) == "" {
		return
	}
	orders, err := s.ListArbitrageOrders(ctx, "", "")
	if err != nil {
		return
	}
	legA, legB, found := pairedArbitrageFillTotals(orders, executionID)
	if !found {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	apply := func(item *ArbitrageExecution) {
		if item == nil || item.ID != executionID {
			return
		}
		item.LegAFilledQuantity = legA.String()
		item.LegBFilledQuantity = legB.String()
	}
	apply(&s.execution)
	apply(s.activeExecution)
	for index := range s.executions {
		apply(&s.executions[index])
	}
}

func (s *executorFailStore) UpdateArbitragePositionFromBase(
	_ context.Context, _, _, _ string,
) (ArbitrageCombination, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.combo, nil
}

type silentRiskCatalog map[int64]Instrument

func (s silentRiskCatalog) List(context.Context, string, string) ([]Instrument, error) {
	return nil, nil
}

func (s silentRiskCatalog) Get(_ context.Context, id int64) (Instrument, error) {
	item, ok := s[id]
	if !ok {
		return Instrument{}, ErrInstrumentUnavailable
	}
	return item, nil
}

type silentRiskCredentials struct{}

func (silentRiskCredentials) GetInternal(context.Context, string, string, int64) (Credentials, error) {
	return Credentials{APIKey: "k", APISecret: "s"}, nil
}

type unusedVenueAdapter struct{}

func (unusedVenueAdapter) GetBBO(context.Context, exchange.Instrument) (exchange.BBO, error) {
	return exchange.BBO{}, errors.New("unused adapter")
}

func (unusedVenueAdapter) PlaceOrder(context.Context, exchange.Credentials, exchange.OrderRequest) (exchange.Result, error) {
	return exchange.Result{}, errors.New("unused adapter")
}

func (unusedVenueAdapter) GetOrder(context.Context, exchange.Credentials, exchange.QueryRequest) (exchange.Result, error) {
	return exchange.Result{}, errors.New("unused adapter")
}

func (unusedVenueAdapter) CancelOrder(context.Context, exchange.Credentials, exchange.CancelRequest) (exchange.Result, error) {
	return exchange.Result{}, errors.New("unused adapter")
}

func (unusedVenueAdapter) CancelAndGetOrder(ctx context.Context, credentials exchange.Credentials, request exchange.CancelRequest) (exchange.Result, error) {
	return unusedVenueAdapter{}.CancelOrder(ctx, credentials, request)
}

type closeOrderStore struct {
	*executorFailStore
}

func (s *closeOrderStore) CreateIntent(
	_ context.Context, order Order,
) (Order, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if order.ID == "" {
		order.ID = fmt.Sprintf("close-order-%d", len(s.orders)+1)
	}
	if order.ClientOrderID == "" {
		order.ClientOrderID = order.ID
	}
	if order.Status == "" {
		order.Status = "pending"
	}
	s.orders = append(s.orders, order)
	return order, true, nil
}

func (s *closeOrderStore) CreateArbitrageIntents(
	_ context.Context, orders []Order,
) ([]Order, []bool, error) {
	return orders, make([]bool, len(orders)), nil
}

func (s *closeOrderStore) GetByOwner(
	_ context.Context, _, id string,
) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, order := range s.orders {
		if order.ID == id {
			return order, nil
		}
	}
	return Order{}, ErrNotFound
}

func (s *closeOrderStore) ListByOwnerAccount(
	context.Context, string, int64, string, int, string,
) ([]Order, string, error) {
	return nil, "", nil
}

func (s *closeOrderStore) UpdateResult(
	_ context.Context, id string, result VenueResult,
) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.orders {
		if s.orders[index].ID != id {
			continue
		}
		if result.VenueOrderID != "" {
			s.orders[index].VenueOrderID = result.VenueOrderID
		}
		s.orders[index].Status = result.Status
		if result.FilledQuantity != "" {
			s.orders[index].FilledQuantity = result.FilledQuantity
		}
		if result.AveragePrice != "" {
			s.orders[index].AveragePrice = result.AveragePrice
		}
		s.orders[index].ErrorCode = result.ErrorCode
		s.orders[index].ErrorMessage = result.ErrorMessage
		return s.orders[index], nil
	}
	return Order{}, ErrNotFound
}

func (s *closeOrderStore) AppendEvent(
	context.Context, string, string, map[string]any,
) error {
	return nil
}

func (s *closeOrderStore) ApplyStreamUpdate(
	_ context.Context, id string, _ StreamUpdate,
) (Order, error) {
	return s.GetByOwner(context.Background(), "", id)
}

type closeVenueAdapter struct {
	mu           sync.Mutex
	cancelResult exchange.Result
	queryResult  exchange.Result
	cancelErr    error
	queryErr     error
	cancelCalls  int
	queryCalls   int
	placeCalls   int
}

func (a *closeVenueAdapter) GetBBO(
	context.Context, exchange.Instrument,
) (exchange.BBO, error) {
	return exchange.BBO{}, errors.New("unused adapter")
}

func (a *closeVenueAdapter) PlaceOrder(
	context.Context, exchange.Credentials, exchange.OrderRequest,
) (exchange.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.placeCalls++
	return exchange.Result{}, errors.New("close must not place orders")
}

func (a *closeVenueAdapter) GetOrder(
	context.Context, exchange.Credentials, exchange.QueryRequest,
) (exchange.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queryCalls++
	return a.queryResult, a.queryErr
}

func (a *closeVenueAdapter) CancelOrder(
	context.Context, exchange.Credentials, exchange.CancelRequest,
) (exchange.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancelCalls++
	return a.cancelResult, a.cancelErr
}

func (a *closeVenueAdapter) CancelAndGetOrder(
	ctx context.Context, credentials exchange.Credentials, request exchange.CancelRequest,
) (exchange.Result, error) {
	return a.CancelOrder(ctx, credentials, request)
}

type flattenVenueAdapter struct {
	mu       sync.Mutex
	requests []exchange.OrderRequest
	err      error
}

func (a *flattenVenueAdapter) GetBBO(
	context.Context, exchange.Instrument,
) (exchange.BBO, error) {
	return exchange.BBO{}, errors.New("unused adapter")
}

func (a *flattenVenueAdapter) PlaceOrder(
	_ context.Context, _ exchange.Credentials, request exchange.OrderRequest,
) (exchange.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, request)
	if a.err != nil {
		return exchange.Result{}, a.err
	}
	return exchange.Result{
		Status: "filled", FilledQuantity: request.Quantity, AveragePrice: "100",
		VenueOrderID: "venue-" + request.ClientOrderID,
	}, nil
}

func (a *flattenVenueAdapter) GetOrder(
	context.Context, exchange.Credentials, exchange.QueryRequest,
) (exchange.Result, error) {
	return exchange.Result{}, errors.New("unused adapter")
}

func (a *flattenVenueAdapter) CancelOrder(
	context.Context, exchange.Credentials, exchange.CancelRequest,
) (exchange.Result, error) {
	return exchange.Result{}, errors.New("unused adapter")
}

func (a *flattenVenueAdapter) CancelAndGetOrder(
	ctx context.Context, credentials exchange.Credentials, request exchange.CancelRequest,
) (exchange.Result, error) {
	return a.CancelOrder(ctx, credentials, request)
}

func (a *flattenVenueAdapter) snapshot() []exchange.OrderRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]exchange.OrderRequest(nil), a.requests...)
}

func TestArbitrageExecutorRiskLimitWithoutExposureIsSilent(t *testing.T) {
	instrumentA := Instrument{
		ID: 101, Exchange: "binance", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		PriceTick: "0.1", QuantityStep: "1",
	}
	instrumentB := Instrument{
		ID: 202, Exchange: "okx", ContractType: "perpetual",
		ExchangeSymbol: "BTC-USDT-SWAP", BaseAsset: "BTC", QuoteAsset: "USDT",
		PriceTick: "0.1", QuantityStep: "1",
	}
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61736", OwnerUsername: "admin",
		Status: "running", ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		OrderNotional: "1", CarryBaseQuantity: "0",
		LegA: ArbitrageLeg{TradingAccountID: 1, InstrumentID: 101, Exchange: "binance"},
		LegB: ArbitrageLeg{TradingAccountID: 2, InstrumentID: 202, Exchange: "okx"},
	}
	store := &executorFailStore{combo: combo}
	executor := NewArbitrageExecutor(
		store, nil, nil,
		silentRiskCatalog{101: instrumentA, 202: instrumentB},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"binance": unusedVenueAdapter{}, "okx": unusedVenueAdapter{},
		}),
		nil, "", time.Second, 0, 0, 0, time.Second, nil,
	)
	executor.Execute(
		context.Background(), combo,
		ArbitrageExecution{ID: "exec-silent", Direction: "ask"},
		marketdata.BBO{BidPrice: "100", AskPrice: "100.1"},
		marketdata.BBO{BidPrice: "100", AskPrice: "100.1"},
		nil,
	)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.failures != 0 || store.combo.ErrorMessage != "" {
		t.Fatalf("failures=%d combo=%+v", store.failures, store.combo)
	}
	if store.execution.Status != "canceled" || store.execution.ErrorMessage != "" {
		t.Fatalf("execution=%+v", store.execution)
	}
}

func TestArbitrageExecutorSilentSkipClearsUnchangedLegacyFailure(t *testing.T) {
	instrumentA := Instrument{
		ID: 101, Exchange: "binance", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", PriceTick: "0.1", QuantityStep: "1",
	}
	instrumentB := Instrument{
		ID: 202, Exchange: "okx", ContractType: "perpetual",
		ExchangeSymbol: "BTC-USDT-SWAP", PriceTick: "0.1", QuantityStep: "1",
	}
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61738", OwnerUsername: "admin",
		Status: "running", RuntimeState: "backoff",
		ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		OrderNotional: "1", ErrorMessage: "invalid argument",
		LegA: ArbitrageLeg{TradingAccountID: 1, InstrumentID: 101, Exchange: "binance"},
		LegB: ArbitrageLeg{TradingAccountID: 2, InstrumentID: 202, Exchange: "okx"},
	}
	store := &executorFailStore{combo: combo}
	executor := NewArbitrageExecutor(
		store, nil, nil,
		silentRiskCatalog{101: instrumentA, 202: instrumentB},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"binance": unusedVenueAdapter{}, "okx": unusedVenueAdapter{},
		}),
		nil, "", time.Second, 0, 0, 0, time.Second, nil,
	)
	executor.Execute(
		context.Background(), combo,
		ArbitrageExecution{ID: "exec-clear", Direction: "ask"},
		marketdata.BBO{BidPrice: "100", AskPrice: "100.1"},
		marketdata.BBO{BidPrice: "100", AskPrice: "100.1"},
		nil,
	)
	if store.cleared != 1 || store.combo.ErrorMessage != "" ||
		store.combo.RuntimeState != "monitoring" {
		t.Fatalf("cleared=%d combo=%+v", store.cleared, store.combo)
	}
}

func TestArbitrageExecutorBelowMinimumIsSilentOnlyWithoutExposure(t *testing.T) {
	instrumentA := Instrument{
		ID: 101, Exchange: "binance", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		PriceTick: "0.1", QuantityStep: "0.01",
		MinQuantity: "0.1", MinQuantityStatus: exchange.ConstraintKnown,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotionalStatus: exchange.ConstraintNotApplicable,
	}
	instrumentB := Instrument{
		ID: 202, Exchange: "okx", ContractType: "perpetual",
		ExchangeSymbol: "BTC-USDT-SWAP", BaseAsset: "BTC", QuoteAsset: "USDT",
		PriceTick: "0.1", QuantityStep: "0.01",
		MinQuantityStatus: exchange.ConstraintNotApplicable,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotionalStatus: exchange.ConstraintNotApplicable,
	}
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61737", OwnerUsername: "admin",
		Status: "running", ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		OrderNotional: "9", CarryBaseQuantity: "0",
		LegA: ArbitrageLeg{TradingAccountID: 1, InstrumentID: 101, Exchange: "binance"},
		LegB: ArbitrageLeg{TradingAccountID: 2, InstrumentID: 202, Exchange: "okx"},
	}
	newExecutor := func(store *executorFailStore) *ArbitrageExecutor {
		return NewArbitrageExecutor(
			store, nil, nil,
			silentRiskCatalog{101: instrumentA, 202: instrumentB},
			silentRiskCredentials{},
			exchange.NewTestRegistry(map[string]exchange.Adapter{
				"binance": unusedVenueAdapter{}, "okx": unusedVenueAdapter{},
			}),
			nil, "", time.Second, 0, 0, 0, time.Second, nil,
		)
	}
	bbo := marketdata.BBO{BidPrice: "100", AskPrice: "100.1"}

	withoutExposure := &executorFailStore{combo: combo}
	newExecutor(withoutExposure).Execute(
		context.Background(), combo,
		ArbitrageExecution{ID: "exec-below-minimum", Direction: "ask"}, bbo, bbo,
		nil,
	)
	if withoutExposure.failures != 0 ||
		withoutExposure.execution.Status != "canceled" ||
		withoutExposure.execution.ErrorMessage != "" {
		t.Fatalf(
			"without exposure execution=%+v failures=%d",
			withoutExposure.execution, withoutExposure.failures,
		)
	}

	withExposure := &executorFailStore{combo: combo}
	newExecutor(withExposure).Execute(
		context.Background(), combo,
		ArbitrageExecution{
			ID: "exec-below-minimum-exposed", Direction: "ask",
			LegAFilledQuantity: "0.01",
		},
		bbo, bbo,
		nil,
	)
	if withExposure.failures != 1 ||
		withExposure.execution.Status != "reconciling" ||
		withExposure.combo.ErrorMessage == "" {
		t.Fatalf(
			"with exposure execution=%+v failures=%d combo=%+v",
			withExposure.execution, withExposure.failures, withExposure.combo,
		)
	}
}

func TestArbitrageExecutorClosingBelowMinimumRecordsCloseFailure(t *testing.T) {
	instrumentA := Instrument{
		ID: 101, Exchange: "binance", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		PriceTick: "0.1", QuantityStep: "0.01",
		MinQuantity: "0.1", MinQuantityStatus: exchange.ConstraintKnown,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotionalStatus: exchange.ConstraintNotApplicable,
	}
	instrumentB := Instrument{
		ID: 202, Exchange: "okx", ContractType: "perpetual",
		ExchangeSymbol: "BTC-USDT-SWAP", BaseAsset: "BTC", QuoteAsset: "USDT",
		PriceTick: "0.1", QuantityStep: "0.01",
		MinQuantityStatus: exchange.ConstraintNotApplicable,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotionalStatus: exchange.ConstraintNotApplicable,
	}
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61751", OwnerUsername: "admin",
		Status: "closing", RuntimeState: "closing",
		ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		OrderNotional: "9", CarryBaseQuantity: "0",
		LegA: ArbitrageLeg{TradingAccountID: 1, InstrumentID: 101, Exchange: "binance"},
		LegB: ArbitrageLeg{TradingAccountID: 2, InstrumentID: 202, Exchange: "okx"},
	}
	store := &executorFailStore{combo: combo}
	executor := NewArbitrageExecutor(
		store, nil, nil,
		silentRiskCatalog{101: instrumentA, 202: instrumentB},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"binance": unusedVenueAdapter{}, "okx": unusedVenueAdapter{},
		}),
		nil, "", time.Second, 0, 0, 0, time.Second, nil,
	)
	bbo := marketdata.BBO{BidPrice: "100", AskPrice: "100.1"}
	for i := 0; i < 6; i++ {
		executor.Execute(
			context.Background(), store.combo,
			ArbitrageExecution{
				ID: fmt.Sprintf("exec-close-below-%d", i), Direction: "ask",
				PositionEffect: "close", ReduceOnly: true,
			},
			bbo, bbo, nil,
		)
	}
	if store.failures != 0 || store.closeFailures != 6 ||
		store.execution.Status != "failed" ||
		store.combo.LastFailureKey != "close_rejected" ||
		store.combo.RuntimeState != "closing" ||
		store.combo.Status != "closing" ||
		store.combo.CircuitOpen ||
		store.combo.NextRetryAt.IsZero() ||
		store.combo.NextRetryAt.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("execution=%+v combo=%+v failures=%d closeFailures=%d",
			store.execution, store.combo, store.failures, store.closeFailures)
	}
}

func TestArbitrageExecutorClosingVenueRejectKeepsClosing(t *testing.T) {
	instrumentA := lastClipCloseInstrument(101, "1", "5")
	instrumentB := lastClipCloseInstrument(202, "1", "5")
	combo := lastClipCloseCombination("bid")
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	store := &executorFailStore{combo: combo, intentStore: orderStore}
	adapter := &stubAdapter{
		err: exchange.ErrRejected,
		place: exchange.Result{
			Status: "rejected", ErrorCode: "10001", ErrorMessage: "min notional",
		},
	}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{101: instrumentA, 202: instrumentB},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": adapter, "okx": adapter,
		}),
		nil, "", time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	bbo := lastClipCloseBBO()
	executor.Execute(
		context.Background(), combo,
		ArbitrageExecution{
			ID: "exec-close-venue-reject", CombinationID: combo.ID,
			Direction: "bid", PositionEffect: "close", ReduceOnly: true,
			LastCloseClip:      true,
			TargetBaseQuantity: "20", RequestedNotional: "4.28",
		},
		bbo, bbo, nil,
	)
	if store.failures != 0 || store.closeFailures != 1 ||
		store.execution.Status != "failed" ||
		store.combo.LastFailureKey != "close_rejected" ||
		store.combo.RuntimeState != "closing" ||
		store.combo.Status != "closing" ||
		store.combo.CircuitOpen ||
		len(adapter.requests) == 0 {
		t.Fatalf("execution=%+v combo=%+v failures=%d closeFailures=%d requests=%d",
			store.execution, store.combo, store.failures, store.closeFailures,
			len(adapter.requests))
	}
	if adapter.requests[0].Quantity != "20" || !adapter.requests[0].ReduceOnly {
		t.Fatalf("request=%+v", adapter.requests[0])
	}
}

func TestArbitrageExecutorOneShotExitingRejectKeepsRunningAndBacksOff(t *testing.T) {
	instrumentA := lastClipCloseInstrument(101, "1", "5")
	instrumentB := lastClipCloseInstrument(202, "1", "5")
	combo := lastClipCloseCombination("bid")
	combo.Status = "running"
	combo.RuntimeState = "monitoring"
	combo.RunMode = "one_shot"
	combo.OneShotPhase = "exiting"
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	store := &executorFailStore{combo: combo, intentStore: orderStore}
	adapter := &stubAdapter{
		err: exchange.ErrRejected,
		place: exchange.Result{
			Status: "rejected", ErrorCode: "10001", ErrorMessage: "min notional",
		},
	}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{101: instrumentA, 202: instrumentB},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": adapter, "okx": adapter,
		}),
		nil, "", time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	bbo := lastClipCloseBBO()
	executor.Execute(
		context.Background(), combo,
		ArbitrageExecution{
			ID: "exec-one-shot-exit-reject", CombinationID: combo.ID,
			Direction: "bid", PositionEffect: "close", ReduceOnly: true,
			LastCloseClip:      true,
			TargetBaseQuantity: "20", RequestedNotional: "4.28",
		},
		bbo, bbo, nil,
	)
	if store.failures != 0 || store.closeFailures != 1 ||
		store.execution.Status != "failed" ||
		store.combo.LastFailureKey != "close_rejected" ||
		store.combo.RuntimeState != "backoff" ||
		store.combo.Status != "running" ||
		store.combo.OneShotPhase != "exiting" ||
		store.combo.CircuitOpen {
		t.Fatalf("execution=%+v combo=%+v failures=%d closeFailures=%d",
			store.execution, store.combo, store.failures, store.closeFailures)
	}
}

func TestArbitrageExecutorFailureKeepsCombinationRunning(t *testing.T) {
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61734", OwnerUsername: "admin",
		Status: "running", ExecutionMode: "unknown",
	}
	store := &executorFailStore{combo: combo}
	executor := NewArbitrageExecutor(
		store, nil, nil, nil, nil, nil, nil, "", time.Second, 0, 0, 0, time.Second, nil,
	)
	executor.Execute(
		context.Background(), combo, ArbitrageExecution{ID: "exec-1", Direction: "ask"},
		marketdata.BBO{}, marketdata.BBO{},
		nil,
	)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.combo.Status != "running" {
		t.Fatalf("status=%s", store.combo.Status)
	}
	if store.execution.Status != "failed" || store.failures != 1 || store.combo.ErrorMessage == "" {
		t.Fatalf("execution=%+v failures=%d combo=%+v", store.execution, store.failures, store.combo)
	}
}

func TestArbitrageExecutorCloseDoesNotRequireFailedStatus(t *testing.T) {
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61735", OwnerUsername: "admin",
		Status: "closing",
	}
	store := &executorFailStore{combo: combo}
	executor := NewArbitrageExecutor(
		store, nil, nil, nil, nil, nil, nil, "", time.Second, 0, 0, 0, time.Second, nil,
	)
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closed" {
		t.Fatalf("status=%s", store.combo.Status)
	}
}

func TestArbitrageExecutorCloseRejectsNonZeroPositions(t *testing.T) {
	combo := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61739", OwnerUsername: "admin",
		Status: "closing", RuntimeState: "closing",
		LegABasePosition: "-1197", LegBBasePosition: "1190", CarryBaseQuantity: "-7",
	}
	store := &executorFailStore{combo: combo}
	executor := NewArbitrageExecutor(
		store, nil, nil, nil, nil, nil, nil, "", time.Second, 0, 0, 0, time.Second, nil,
	)
	if err := executor.CloseCombination(context.Background(), combo); !errors.Is(
		err, ErrArbitrageNotClosable,
	) {
		t.Fatalf("close err=%v", err)
	}
	if store.combo.Status != "closing" || store.combo.RuntimeState != "closing" {
		t.Fatalf("combo=%+v", store.combo)
	}
	if store.combo.LegABasePosition != "-1197" ||
		store.combo.LegBBasePosition != "1190" ||
		store.combo.CarryBaseQuantity != "-7" {
		t.Fatalf("positions changed during close: %+v", store.combo)
	}
}

func TestArbitrageExecutorCloseFinalizesExecutionWithoutHedge(t *testing.T) {
	combo := closeTestCombination()
	combo.CarryBaseQuantity = "0"
	store := &executorFailStore{
		combo: combo,
		executions: []ArbitrageExecution{{
			ID: "execution-active", CombinationID: combo.ID,
			Direction: "ask", Status: "reconciling",
		}},
	}
	adapterA := &closeVenueAdapter{}
	adapterB := &closeVenueAdapter{}
	executor := NewArbitrageExecutor(
		store, nil, nil,
		closeTestCatalog(), silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"binance": adapterA, "okx": adapterB,
		}),
		nil, "", time.Millisecond, 0, 0, 0, 5*time.Millisecond, nil,
	)
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if len(store.executions) != 1 || store.executions[0].Status != "completed" {
		t.Fatalf("executions=%+v", store.executions)
	}
	if store.combo.Status != "closed" || store.combo.CarryBaseQuantity != "0" {
		t.Fatalf("combo=%+v", store.combo)
	}
	if adapterA.placeCalls != 0 || adapterB.placeCalls != 0 {
		t.Fatalf(
			"closing placed orders: leg_a=%d leg_b=%d",
			adapterA.placeCalls, adapterB.placeCalls,
		)
	}
}

func TestArbitrageExecutorCloseSkipsTerminalHistoricalOrders(t *testing.T) {
	combo := closeTestCombination()
	store := &executorFailStore{
		combo: combo,
		orders: []Order{{
			ID: "order-filled", Status: "filled", ArbitrageLeg: "a",
		}},
	}
	executor := NewArbitrageExecutor(
		store, nil, nil, nil, nil, nil, nil, "", time.Second, 0, 0, 0, time.Second, nil,
	)
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closed" {
		t.Fatalf("combo=%+v", store.combo)
	}
}

func TestArbitrageExecutorCloseCancelsActiveOrderBeforeClosing(t *testing.T) {
	combo := closeTestCombination()
	baseStore := &executorFailStore{
		combo: combo,
		orders: []Order{{
			ID: "order-open", OwnerUsername: combo.OwnerUsername,
			TradingAccountID: combo.LegA.TradingAccountID,
			InstrumentID:     combo.LegA.InstrumentID,
			Exchange:         "binance", ExchangeSymbol: "BTCUSDT",
			ClientOrderID: "client-open", VenueOrderID: "venue-open",
			Status: "open", ArbitrageLeg: "a",
		}},
	}
	store := &closeOrderStore{executorFailStore: baseStore}
	adapterA := &closeVenueAdapter{
		cancelResult: exchange.Result{Status: "canceled", VenueOrderID: "venue-open"},
		queryResult:  exchange.Result{Status: "canceled", VenueOrderID: "venue-open"},
	}
	adapterB := &closeVenueAdapter{}
	executor := newCloseTestExecutor(store, adapterA, adapterB, 20*time.Millisecond)
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closed" || store.orders[0].Status != "canceled" {
		t.Fatalf("combo=%+v order=%+v", store.combo, store.orders[0])
	}
	if adapterA.cancelCalls != 1 || adapterA.queryCalls != 0 ||
		adapterA.placeCalls != 0 || adapterB.cancelCalls != 0 ||
		adapterB.queryCalls != 0 || adapterB.placeCalls != 0 {
		t.Fatalf("leg_a=%+v leg_b=%+v", adapterA, adapterB)
	}
}

func TestArbitrageExecutorCloseKeepsClosingWhenOrderRemainsUnknown(t *testing.T) {
	combo := closeTestCombination()
	combo.LastFailureKey = "close_order_open"
	combo.RepeatedFailureCount = 99
	baseStore := &executorFailStore{
		combo: combo,
		orders: []Order{{
			ID: "order-unknown", OwnerUsername: combo.OwnerUsername,
			TradingAccountID: combo.LegA.TradingAccountID,
			InstrumentID:     combo.LegA.InstrumentID,
			Exchange:         "binance", ExchangeSymbol: "BTCUSDT",
			ClientOrderID: "client-unknown", VenueOrderID: "venue-unknown",
			Status: "unknown", ArbitrageLeg: "a",
		}},
	}
	store := &closeOrderStore{executorFailStore: baseStore}
	adapterA := &closeVenueAdapter{
		cancelResult: exchange.Result{Status: "unknown", VenueOrderID: "venue-unknown"},
		queryResult:  exchange.Result{Status: "unknown", VenueOrderID: "venue-unknown"},
	}
	adapterB := &closeVenueAdapter{}
	executor := newCloseTestExecutor(store, adapterA, adapterB, 5*time.Millisecond)
	err := executor.CloseCombination(context.Background(), combo)
	if !errors.Is(err, ErrCloseOrderUncertain) {
		t.Fatalf("err=%v", err)
	}
	if store.combo.Status != "closing" || store.events != 0 {
		t.Fatalf("combo=%+v events=%d", store.combo, store.events)
	}
	if adapterA.cancelCalls != 0 || adapterA.queryCalls == 0 || adapterA.placeCalls != 0 {
		t.Fatalf("leg_a=%+v", adapterA)
	}
}

func TestArbitrageExecutorCloseForceClosesUnknownOnTenthAttempt(t *testing.T) {
	combo := closeTestCombination()
	combo.ConsecutiveFailures = 9
	combo.LastFailureKey = "close_order_uncertain"
	combo.RepeatedFailureCount = 9
	baseStore := &executorFailStore{
		combo: combo,
		orders: []Order{{
			ID: "order-unknown", OwnerUsername: combo.OwnerUsername,
			TradingAccountID: combo.LegA.TradingAccountID,
			InstrumentID:     combo.LegA.InstrumentID,
			Exchange:         "binance", ExchangeSymbol: "BTCUSDT",
			ClientOrderID: "client-unknown", VenueOrderID: "venue-unknown",
			Status: "unknown", FilledQuantity: "0.4", AveragePrice: "100",
			ArbitrageLeg: "a",
		}},
	}
	store := &closeOrderStore{executorFailStore: baseStore}
	adapterA := &closeVenueAdapter{
		queryResult: exchange.Result{Status: "unknown", VenueOrderID: "venue-unknown"},
	}
	executor := newCloseTestExecutor(store, adapterA, &closeVenueAdapter{}, 5*time.Millisecond)
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closed" || !store.combo.PositionUncertain ||
		store.combo.RuntimeState != "position_uncertain" {
		t.Fatalf("combo=%+v", store.combo)
	}
	order := store.orders[0]
	if order.Status != "canceled" || order.FilledQuantity != "0.4" ||
		order.AveragePrice != "100" || order.ErrorCode != "close_state_unresolved" {
		t.Fatalf("order=%+v", order)
	}
	if adapterA.cancelCalls != 0 || adapterA.placeCalls != 0 {
		t.Fatalf("adapter=%+v", adapterA)
	}
}

func TestArbitrageExecutorCloseKeepsOpenOrderClosingAfterCancelFailure(t *testing.T) {
	combo := closeTestCombination()
	baseStore := &executorFailStore{
		combo: combo,
		orders: []Order{{
			ID: "order-open", OwnerUsername: combo.OwnerUsername,
			TradingAccountID: combo.LegA.TradingAccountID,
			InstrumentID:     combo.LegA.InstrumentID,
			Exchange:         "binance", ExchangeSymbol: "BTCUSDT",
			ClientOrderID: "client-open", VenueOrderID: "venue-open",
			Status: "open", ArbitrageLeg: "a",
		}},
	}
	store := &closeOrderStore{executorFailStore: baseStore}
	adapterA := &closeVenueAdapter{
		cancelErr:   exchange.ErrRateLimited,
		queryResult: exchange.Result{Status: "open", VenueOrderID: "venue-open"},
	}
	executor := newCloseTestExecutor(store, adapterA, &closeVenueAdapter{}, 5*time.Millisecond)
	err := executor.CloseCombination(context.Background(), combo)
	if !errors.Is(err, ErrCloseOrderOpen) || store.combo.Status != "closing" {
		t.Fatalf("combo=%+v err=%v", store.combo, err)
	}
	if adapterA.cancelCalls != 1 || adapterA.queryCalls != 1 {
		t.Fatalf("adapter=%+v", adapterA)
	}
}

func TestArbitrageExecutorCloseTrustedCancelSkipsGet(t *testing.T) {
	combo := closeTestCombination()
	baseStore := &executorFailStore{
		combo: combo,
		orders: []Order{{
			ID: "order-open", OwnerUsername: combo.OwnerUsername,
			TradingAccountID: combo.LegA.TradingAccountID,
			InstrumentID:     combo.LegA.InstrumentID,
			Exchange:         "binance", ExchangeSymbol: "BTCUSDT",
			ClientOrderID: "client-open", VenueOrderID: "venue-open",
			Status: "open", ArbitrageLeg: "a",
		}},
	}
	store := &closeOrderStore{executorFailStore: baseStore}
	adapterA := &closeVenueAdapter{
		cancelResult: exchange.Result{
			Status: "canceled", VenueOrderID: "venue-open", FilledQuantity: "0",
		},
	}
	executor := newCloseTestExecutor(store, adapterA, &closeVenueAdapter{}, 20*time.Millisecond)
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if adapterA.cancelCalls != 1 || adapterA.queryCalls != 0 {
		t.Fatalf("adapter=%+v", adapterA)
	}
}

func TestArbitrageExecutorClosePendingCancelQueriesOnce(t *testing.T) {
	combo := closeTestCombination()
	baseStore := &executorFailStore{
		combo: combo,
		orders: []Order{{
			ID: "order-open", OwnerUsername: combo.OwnerUsername,
			TradingAccountID: combo.LegA.TradingAccountID,
			InstrumentID:     combo.LegA.InstrumentID,
			Exchange:         "binance", ExchangeSymbol: "BTCUSDT",
			ClientOrderID: "client-open", VenueOrderID: "venue-open",
			Status: "open", ArbitrageLeg: "a",
		}},
	}
	store := &closeOrderStore{executorFailStore: baseStore}
	adapterA := &closeVenueAdapter{
		cancelResult: exchange.Result{Status: "pending", LocalCommandAck: true, VenueOrderID: "venue-open"},
		queryResult:  exchange.Result{Status: "canceled", VenueOrderID: "venue-open", FilledQuantity: "0"},
	}
	executor := newCloseTestExecutor(store, adapterA, &closeVenueAdapter{}, 20*time.Millisecond)
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closed" || store.orders[0].Status != "canceled" {
		t.Fatalf("combo=%+v order=%+v", store.combo, store.orders[0])
	}
	if adapterA.cancelCalls != 1 || adapterA.queryCalls != 1 {
		t.Fatalf("adapter=%+v", adapterA)
	}
}

func TestArbitrageExecutorClosePendingCancelOpenReturnsErrCloseOrderOpen(t *testing.T) {
	combo := closeTestCombination()
	baseStore := &executorFailStore{
		combo: combo,
		orders: []Order{{
			ID: "order-open", OwnerUsername: combo.OwnerUsername,
			TradingAccountID: combo.LegA.TradingAccountID,
			InstrumentID:     combo.LegA.InstrumentID,
			Exchange:         "binance", ExchangeSymbol: "BTCUSDT",
			ClientOrderID: "client-open", VenueOrderID: "venue-open",
			Status: "open", ArbitrageLeg: "a",
		}},
	}
	store := &closeOrderStore{executorFailStore: baseStore}
	adapterA := &closeVenueAdapter{
		cancelResult: exchange.Result{Status: "pending", LocalCommandAck: true, VenueOrderID: "venue-open"},
		queryResult:  exchange.Result{Status: "open", VenueOrderID: "venue-open"},
	}
	executor := newCloseTestExecutor(store, adapterA, &closeVenueAdapter{}, 5*time.Millisecond)
	err := executor.CloseCombination(context.Background(), combo)
	if !errors.Is(err, ErrCloseOrderOpen) || store.combo.Status != "closing" {
		t.Fatalf("combo=%+v err=%v", store.combo, err)
	}
	if adapterA.cancelCalls != 1 || adapterA.queryCalls != 1 {
		t.Fatalf("adapter=%+v", adapterA)
	}
}

func TestArbitrageExecutorCloseAmbiguousCancelQueriesOnce(t *testing.T) {
	combo := closeTestCombination()
	baseStore := &executorFailStore{
		combo: combo,
		orders: []Order{{
			ID: "order-open", OwnerUsername: combo.OwnerUsername,
			TradingAccountID: combo.LegA.TradingAccountID,
			InstrumentID:     combo.LegA.InstrumentID,
			Exchange:         "binance", ExchangeSymbol: "BTCUSDT",
			ClientOrderID: "client-open", VenueOrderID: "venue-open",
			Status: "open", ArbitrageLeg: "a",
		}},
	}
	store := &closeOrderStore{executorFailStore: baseStore}
	adapterA := &closeVenueAdapter{
		cancelResult: exchange.Result{Status: "unknown", VenueOrderID: "venue-open"},
		cancelErr:    exchange.ErrAmbiguousCancel,
		queryResult:  exchange.Result{Status: "canceled", VenueOrderID: "venue-open", FilledQuantity: "0"},
	}
	executor := newCloseTestExecutor(store, adapterA, &closeVenueAdapter{}, 20*time.Millisecond)
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if adapterA.cancelCalls != 1 || adapterA.queryCalls != 1 {
		t.Fatalf("adapter=%+v", adapterA)
	}
}

func TestMakerPostOnlyRepriceWhitelist(t *testing.T) {
	tests := []struct {
		name       string
		instrument Instrument
		order      Order
		want       bool
	}{
		{"gate spot", Instrument{Exchange: "gate", ContractType: "spot"}, Order{Status: "rejected", ErrorCode: "POC_FILL_IMMEDIATELY"}, true},
		{"gate futures", Instrument{Exchange: "gate", ContractType: "perpetual"}, Order{Status: "rejected", ErrorCode: "ORDER_POC_IMMEDIATE"}, true},
		{"binance spot", Instrument{Exchange: "binance", ContractType: "spot"}, Order{Status: "rejected", ErrorCode: "-2010", ErrorMessage: "Order would immediately match and take."}, true},
		{"binance futures", Instrument{Exchange: "binance", ContractType: "perpetual"}, Order{Status: "rejected", ErrorCode: "-5022"}, true},
		{"bybit", Instrument{Exchange: "bybit"}, Order{Status: "rejected", ErrorCode: "EC_PostOnlyWillTakeLiquidity"}, true},
		{"okx", Instrument{Exchange: "okx"}, Order{Status: "canceled", ErrorCode: "31"}, true},
		{"bitget", Instrument{Exchange: "bitget"}, Order{Status: "canceled", ErrorCode: "post_only_fill_cancel"}, true},
		{"binance generic rejected", Instrument{Exchange: "binance", ContractType: "spot"}, Order{Status: "rejected", ErrorCode: "-2010", ErrorMessage: "Account has insufficient balance."}, false},
		{"binance 2011 futures", Instrument{Exchange: "binance", ContractType: "perpetual"}, Order{Status: "rejected", ErrorCode: "-2011"}, false},
		{"binance 2011 spot", Instrument{Exchange: "binance", ContractType: "spot"}, Order{Status: "rejected", ErrorCode: "-2011", ErrorMessage: "Unknown order sent."}, false},
		{"bybit processing", Instrument{Exchange: "bybit"}, Order{Status: "rejected", ErrorCode: "110079"}, false},
		{"okx unverified", Instrument{Exchange: "okx"}, Order{Status: "rejected", ErrorCode: "51019"}, false},
		{"positive fill", Instrument{Exchange: "gate"}, Order{Status: "rejected", ErrorCode: "POC_FILL_IMMEDIATELY", FilledQuantity: "0.1"}, false},
		{"gate futures positive fill", Instrument{Exchange: "gate"}, Order{Status: "rejected", ErrorCode: "ORDER_POC_IMMEDIATE", FilledQuantity: "0.1"}, false},
		{"gate genuine reject", Instrument{Exchange: "gate"}, Order{Status: "rejected", ErrorCode: "BALANCE_NOT_ENOUGH"}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := makerPostOnlyReprice(test.instrument, test.order); got != test.want {
				t.Fatalf("got=%v want=%v", got, test.want)
			}
		})
	}
}

func TestGatePostOnlyImmediateRepricesWithoutBackoff(t *testing.T) {
	makerInstrument := Instrument{
		ID: 101, Exchange: "gate", ContractType: "perpetual",
		ExchangeSymbol: "BTC_USDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		ContractSize: "1", PriceTick: "0.1", QuantityStep: "0.001",
		MinQuantityStatus: exchange.ConstraintNotApplicable,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotionalStatus: exchange.ConstraintNotApplicable,
	}
	hedgeInstrument := Instrument{
		ID: 202, Exchange: "binance", ContractType: "perpetual",
		ExchangeSymbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
		ContractSize: "1", PriceTick: "0.1", QuantityStep: "0.001",
		MinQuantityStatus: exchange.ConstraintNotApplicable,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotionalStatus: exchange.ConstraintNotApplicable,
	}
	combination := ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61745", OwnerUsername: "admin",
		Status: "running", RuntimeState: "monitoring",
		ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		OrderNotional: "100", CarryBaseQuantity: "0",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: makerInstrument.ID,
			ProductName: "ARBITRAGE", Exchange: "gate",
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
	store := &executorFailStore{combo: combination}
	orders := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	gate := &stubAdapter{
		place: exchange.Result{
			Status:       "rejected",
			ErrorCode:    "ORDER_POC_IMMEDIATE",
			ErrorMessage: "order price 100.1 while counter price 100",
		},
		err: exchange.ErrRejected,
	}
	service := NewService(orders, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orders, service,
		silentRiskCatalog{
			makerInstrument.ID: makerInstrument,
			hedgeInstrument.ID: hedgeInstrument,
		},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"gate": gate, "binance": unusedVenueAdapter{},
		}),
		nil, "", time.Millisecond, 0, 0, 0, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "gate-post-only-reprice", CombinationID: combination.ID,
		Direction: "ask", RequestedNotional: "100",
		TriggerLegABid: "100", TriggerLegAAsk: "100.1",
		TriggerLegBBid: "100", TriggerLegBAsk: "100.1",
	}
	err := executor.executeMakerThenHedge(
		context.Background(), combination, &execution,
		marketdata.BBO{BidPrice: "100", AskPrice: "100.1"},
		marketdata.BBO{BidPrice: "100", AskPrice: "100.1"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	failures := store.failures
	runtimeState := store.combo.RuntimeState
	executionStatus := store.execution.Status
	eventTypes := append([]string(nil), store.eventTypes...)
	store.mu.Unlock()
	if failures != 0 || runtimeState != "repricing" || executionStatus != "completed" {
		t.Fatalf(
			"failures=%d runtime=%s execution=%s",
			failures, runtimeState, executionStatus,
		)
	}
	hasRequote, hasVenueFailure := false, false
	for _, eventType := range eventTypes {
		hasRequote = hasRequote || eventType == "maker_requote"
		hasVenueFailure = hasVenueFailure || eventType == "venue_rejected"
	}
	if !hasRequote || hasVenueFailure {
		t.Fatalf("events=%v", eventTypes)
	}
	orders.mu.Lock()
	orderEvents := append([]string(nil), orders.events...)
	orders.mu.Unlock()
	if len(orderEvents) != 2 ||
		orderEvents[0] != "submitted" ||
		orderEvents[1] != "reject" {
		t.Fatalf("order events=%v, want [submitted reject]", orderEvents)
	}
}

func closeTestCombination() ArbitrageCombination {
	return ArbitrageCombination{
		ID: "46c5b9e3-3fdd-4930-b529-c2d902b61740", OwnerUsername: "admin",
		Status: "closing", RuntimeState: "closing",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: 101,
			Exchange: "binance", ContractType: "perpetual", ExchangeSymbol: "BTCUSDT",
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: 202,
			Exchange: "okx", ContractType: "perpetual", ExchangeSymbol: "BTC-USDT-SWAP",
		},
	}
}

func closeTestCatalog() silentRiskCatalog {
	return silentRiskCatalog{
		101: {
			ID: 101, Exchange: "binance", ContractType: "perpetual",
			ExchangeSymbol: "BTCUSDT", QuantityStep: "0.001",
		},
		202: {
			ID: 202, Exchange: "okx", ContractType: "perpetual",
			ExchangeSymbol: "BTC-USDT-SWAP", QuantityStep: "0.001",
		},
	}
}

func newCloseTestExecutor(
	store *closeOrderStore,
	adapterA, adapterB exchange.Adapter,
	timeout time.Duration,
) *ArbitrageExecutor {
	registry := exchange.NewTestRegistry(map[string]exchange.Adapter{
		"binance": adapterA, "okx": adapterB,
	})
	service := NewService(store, nil, nil, nil, timeout, nil)
	return NewArbitrageExecutor(
		store, store, service,
		closeTestCatalog(), silentRiskCredentials{}, registry,
		nil, "", time.Millisecond, 0, 0, 0, timeout, nil,
	)
}

func closeFlattenReadyCombo() ArbitrageCombination {
	combo := closeTestCombination()
	combo.OrderNotional = "500"
	combo.LastPositionReconciledAt = time.Now().UTC()
	combo.LegAAverageEntryPrice = "100"
	combo.LegBAverageEntryPrice = "100"
	return combo
}

func ownedTinyCloseCombo(legA, legB string) ArbitrageCombination {
	combo := closeFlattenReadyCombo()
	combo.LegABasePosition = legA
	combo.LegBBasePosition = legB
	combo.CarryBaseQuantity = parseDecimal(legA).Add(parseDecimal(legB)).String()
	combo.VenueBaselineCapturedAt = time.Now().UTC()
	combo.LegAVenueBaselineBasePosition = "0"
	combo.LegBVenueBaselineBasePosition = "0"
	combo.LegAVenueBasePosition = legA
	combo.LegBVenueBasePosition = legB
	combo.LegAPositionDifference = "0"
	combo.LegBPositionDifference = "0"
	combo.LastPositionReconciledAt = time.Now().UTC()
	return combo
}

func flattenTestCatalog() silentRiskCatalog {
	catalog := closeTestCatalog()
	for id, item := range catalog {
		item.ContractSize = "1"
		item.MinQuantity = "10"
		item.MinQuantityStatus = exchange.ConstraintKnown
		item.MinNotional = "50"
		item.MinNotionalStatus = exchange.ConstraintKnown
		item.MarketQuantityStep = "0.001"
		item.MarketQuantityStepStatus = exchange.ConstraintKnown
		item.MarketMinQuantity = "10"
		item.MarketMinQuantityStatus = exchange.ConstraintKnown
		item.MarketMinNotional = "50"
		item.MarketMinNotionalStatus = exchange.ConstraintKnown
		catalog[id] = item
	}
	return catalog
}

func newCloseFlattenMarket(t *testing.T, bid, ask string) *marketdata.Manager {
	t.Helper()
	keyA, err := marketdata.NewKey("binance", "perpetual", "BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := marketdata.NewKey("okx", "perpetual", "BTC-USDT-SWAP")
	if err != nil {
		t.Fatal(err)
	}
	connA := &schedulerConnection{reads: make(chan []byte, 1), done: make(chan struct{})}
	connB := &schedulerConnection{reads: make(chan []byte, 1), done: make(chan struct{})}
	parser := func(
		item marketdata.Key, _ []byte, received time.Time,
	) (marketdata.BBO, bool, error) {
		return marketdata.BBO{
			Key: item, BidPrice: bid, AskPrice: ask,
			VenueTimestamp: received, ReceiveTimestamp: received,
		}, true, nil
	}
	market, err := marketdata.New(marketdata.Options{
		Connector: &schedulerConnector{
			connections: map[marketdata.Key]*schedulerConnection{
				keyA: connA, keyB: connB,
			},
		},
		Parsers: map[string]marketdata.Parser{
			"binance": parser, "okx": parser,
		},
		StaleAfter: time.Second, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = market.Close() })
	for _, key := range []marketdata.Key{keyA, keyB} {
		subscription, subErr := market.Subscribe(context.Background(), key)
		if subErr != nil {
			t.Fatal(subErr)
		}
		t.Cleanup(func() { subscription.Close() })
	}
	connA.reads <- []byte("bbo")
	connB.reads <- []byte("bbo")
	deadline := time.Now().Add(time.Second)
	for {
		_, errA := market.Latest(keyA)
		_, errB := market.Latest(keyB)
		if errA == nil && errB == nil {
			return market
		}
		if time.Now().After(deadline) {
			t.Fatal("market data was not published")
		}
		time.Sleep(time.Millisecond)
	}
}

func newOwnedFlattenExecutor(
	t *testing.T,
	arbStore arbitrageStore,
	ordStore arbitrageOrderStore,
	adapterA, adapterB exchange.Adapter,
	catalog silentRiskCatalog,
	bid, ask string,
) *ArbitrageExecutor {
	t.Helper()
	if catalog == nil {
		catalog = flattenTestCatalog()
	}
	registry := exchange.NewTestRegistry(map[string]exchange.Adapter{
		"binance": adapterA, "okx": adapterB,
	})
	service := NewService(ordStore, nil, nil, nil, time.Second, nil)
	return NewArbitrageExecutor(
		arbStore, ordStore, service, catalog, silentRiskCredentials{}, registry,
		newCloseFlattenMarket(t, bid, ask), "", time.Millisecond, 0, 0, 0, time.Second, nil,
	)
}

type failClosedWriteStore struct {
	*closeOrderStore
}

func (s *failClosedWriteStore) UpdateArbitrageCombinationRuntime(
	ctx context.Context, item ArbitrageCombination,
) (ArbitrageCombination, error) {
	if strings.EqualFold(item.Status, "closed") {
		return ArbitrageCombination{}, errors.New("write closed failed")
	}
	return s.closeOrderStore.UpdateArbitrageCombinationRuntime(ctx, item)
}

type mutateRecomputeStore struct {
	*closeOrderStore
	mutate func(*ArbitrageCombination)
}

func (s *mutateRecomputeStore) RecomputeArbitrageBasePositions(
	ctx context.Context, id string,
) (ArbitrageCombination, error) {
	combo, err := s.closeOrderStore.RecomputeArbitrageBasePositions(ctx, id)
	if err != nil {
		return combo, err
	}
	if s.mutate != nil {
		s.mutate(&combo)
		s.mu.Lock()
		s.combo = combo
		s.mu.Unlock()
	}
	return combo, nil
}

func TestArbitrageExecutorCloseFlattensOwnedZeroMinusTen(t *testing.T) {
	combo := ownedTinyCloseCombo("0", "-10")
	store := &closeOrderStore{executorFailStore: &executorFailStore{combo: combo}}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, nil, "1", "1")
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closed" {
		t.Fatalf("status=%s", store.combo.Status)
	}
	if len(adapterA.snapshot()) != 0 {
		t.Fatalf("leg a places=%d", len(adapterA.snapshot()))
	}
	requests := adapterB.snapshot()
	if len(requests) != 1 || requests[0].Side != "buy" ||
		requests[0].OrderType != "market" || !requests[0].ReduceOnly ||
		requests[0].Quantity != "10" {
		t.Fatalf("leg b request=%+v", requests)
	}
}

func TestArbitrageExecutorCloseFlattensOwnedSameSignBothLegs(t *testing.T) {
	combo := ownedTinyCloseCombo("5", "10")
	store := &closeOrderStore{executorFailStore: &executorFailStore{combo: combo}}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, nil, "1", "1")
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closed" {
		t.Fatalf("status=%s", store.combo.Status)
	}
	requestsA := adapterA.snapshot()
	requestsB := adapterB.snapshot()
	if len(requestsA) != 1 || requestsA[0].Side != "sell" ||
		!requestsA[0].ReduceOnly || requestsA[0].Quantity != "5" {
		t.Fatalf("leg a request=%+v", requestsA)
	}
	if len(requestsB) != 1 || requestsB[0].Side != "sell" ||
		!requestsB[0].ReduceOnly || requestsB[0].Quantity != "10" {
		t.Fatalf("leg b request=%+v", requestsB)
	}
}

func TestArbitrageExecutorCloseZeroOwnedPlacesNothing(t *testing.T) {
	combo := closeFlattenReadyCombo()
	combo.LegAVenueBasePosition = "0.001"
	combo.LegBVenueBasePosition = "-0.002"
	store := &closeOrderStore{executorFailStore: &executorFailStore{combo: combo}}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, nil, "1", "1")
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closed" {
		t.Fatalf("status=%s", store.combo.Status)
	}
	if len(adapterA.snapshot()) != 0 || len(adapterB.snapshot()) != 0 {
		t.Fatalf("places a=%d b=%d", len(adapterA.snapshot()), len(adapterB.snapshot()))
	}
}

func TestArbitrageExecutorCloseRejectsPairedOwnedAfterRecompute(t *testing.T) {
	combo := ownedTinyCloseCombo("0", "-10")
	base := &closeOrderStore{executorFailStore: &executorFailStore{combo: combo}}
	store := &mutateRecomputeStore{
		closeOrderStore: base,
		mutate: func(item *ArbitrageCombination) {
			item.LegABasePosition = "-50"
			item.LegBBasePosition = "40"
			item.CarryBaseQuantity = "-10"
			item.LegAVenueBasePosition = "-50"
			item.LegBVenueBasePosition = "40"
		},
	}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, nil, "1", "1")
	err := executor.CloseCombination(context.Background(), combo)
	if !errors.Is(err, ErrArbitrageNotClosable) || store.combo.Status == "closed" {
		t.Fatalf("status=%s err=%v", store.combo.Status, err)
	}
	if len(adapterA.snapshot()) != 0 || len(adapterB.snapshot()) != 0 {
		t.Fatalf("places a=%d b=%d", len(adapterA.snapshot()), len(adapterB.snapshot()))
	}
}

func TestArbitrageExecutorCloseFlattensPairedTinyUnequalLegs(t *testing.T) {
	combo := ownedTinyCloseCombo("0.5", "-0.57")
	combo.CarryBaseQuantity = "-0.07"
	combo.LegBPositionDifference = "3e-16"
	store := &closeOrderStore{executorFailStore: &executorFailStore{combo: combo}}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{}
	catalog := flattenTestCatalog()
	for id, item := range catalog {
		item.MarketQuantityStep = "0.01"
		item.QuantityStep = "0.01"
		catalog[id] = item
	}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, catalog, "25", "25")
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closed" {
		t.Fatalf("status=%s", store.combo.Status)
	}
	if combo.LegBPositionDifference != "3e-16" {
		t.Fatalf("difference mutated: %s", combo.LegBPositionDifference)
	}
	requestsA := adapterA.snapshot()
	requestsB := adapterB.snapshot()
	if len(requestsA) != 1 || requestsA[0].Side != "sell" ||
		!requestsA[0].ReduceOnly || requestsA[0].Quantity != "0.5" {
		t.Fatalf("leg a request=%+v", requestsA)
	}
	if len(requestsB) != 1 || requestsB[0].Side != "buy" ||
		!requestsB[0].ReduceOnly || requestsB[0].Quantity != "0.57" {
		t.Fatalf("leg b request=%+v", requestsB)
	}
	for _, request := range append(requestsA, requestsB...) {
		if request.Side == "buy" && request.Quantity == "0.07" {
			t.Fatalf("must not buy leftover carry: %+v", request)
		}
	}
}

func TestArbitrageExecutorClosePairedTinyWaitsWhenSnapshotLagsOrders(t *testing.T) {
	combo := ownedTinyCloseCombo("0.5", "-0.57")
	combo.CarryBaseQuantity = "-0.07"
	combo.LegBPositionDifference = "3e-16"
	combo.LastPositionReconciledAt = time.Now().UTC().Add(-10 * time.Second)
	store := &closeOrderStore{executorFailStore: &executorFailStore{
		combo: combo,
		orders: []Order{{
			ID: "order-1", Status: "filled", UpdatedAt: time.Now().UTC(),
		}},
	}}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{}
	catalog := flattenTestCatalog()
	for id, item := range catalog {
		item.MarketQuantityStep = "0.01"
		item.QuantityStep = "0.01"
		catalog[id] = item
	}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, catalog, "25", "25")
	err := executor.CloseCombination(context.Background(), combo)
	if !errors.Is(err, ErrArbitrageCloseWaitingSnapshot) || store.combo.Status == "closed" {
		t.Fatalf("status=%s err=%v", store.combo.Status, err)
	}
	if len(adapterA.snapshot()) != 0 || len(adapterB.snapshot()) != 0 {
		t.Fatalf("places a=%d b=%d", len(adapterA.snapshot()), len(adapterB.snapshot()))
	}
}

func TestArbitrageExecutorCloseRejectsMissingInstruments(t *testing.T) {
	combo := ownedTinyCloseCombo("0.5", "-0.57")
	store := &closeOrderStore{executorFailStore: &executorFailStore{combo: combo}}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, silentRiskCatalog{}, "25", "25")
	err := executor.CloseCombination(context.Background(), combo)
	if !errors.Is(err, ErrArbitrageNotClosable) || store.combo.Status == "closed" {
		t.Fatalf("status=%s err=%v", store.combo.Status, err)
	}
	if len(adapterA.snapshot()) != 0 || len(adapterB.snapshot()) != 0 {
		t.Fatalf("places a=%d b=%d", len(adapterA.snapshot()), len(adapterB.snapshot()))
	}
}

func TestArbitrageExecutorCloseHandoffKeyAllowsPairedTinyLeftover(t *testing.T) {
	combo := ownedTinyCloseCombo("1", "-2.2")
	combo.LastFailureKey = lastClipHedgeMinNotionalFailureKey
	store := &closeOrderStore{executorFailStore: &executorFailStore{combo: combo}}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, nil, "1", "1")
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closed" {
		t.Fatalf("status=%s", store.combo.Status)
	}
	requestsA := adapterA.snapshot()
	requestsB := adapterB.snapshot()
	if len(requestsA) != 1 || requestsA[0].Side != "sell" ||
		!requestsA[0].ReduceOnly || requestsA[0].Quantity != "1" {
		t.Fatalf("leg a request=%+v", requestsA)
	}
	if len(requestsB) != 1 || requestsB[0].Side != "buy" ||
		!requestsB[0].ReduceOnly || requestsB[0].Quantity != "2.2" {
		t.Fatalf("leg b request=%+v", requestsB)
	}
}

func lastCloseTinyCarryInstrument(id int64, venue, symbol string) Instrument {
	return Instrument{
		ID: id, Exchange: venue, ContractType: "perpetual",
		ExchangeSymbol: symbol, ContractSize: "1",
		QuantityStep: "0.01", PriceTick: "0.01",
		MinQuantity: "0.01", MinNotional: "5",
		MinQuantityStatus:  exchange.ConstraintKnown,
		MinNotionalStatus:  exchange.ConstraintKnown,
		MaxQuantityStatus:  exchange.ConstraintNotApplicable,
		MarketQuantityStep: "0.01", MarketMinQuantity: "0.01",
		MarketMinNotional:        "5",
		MarketQuantityStepStatus: exchange.ConstraintKnown,
		MarketMinQuantityStatus:  exchange.ConstraintKnown,
		MarketMaxQuantityStatus:  exchange.ConstraintNotApplicable,
		MarketMinNotionalStatus:  exchange.ConstraintKnown,
	}
}

func lastCloseTinyCarryCombo(makerLeg, carry string) ArbitrageCombination {
	combo := ownedTinyCloseCombo("0.5", "-0.57")
	combo.CarryBaseQuantity = carry
	combo.LegBPositionDifference = "3e-16"
	combo.MakerLeg = makerLeg
	combo.ExecutionMode = "maker_then_hedge"
	return combo
}

func TestLastCloseStandaloneCarryReady(t *testing.T) {
	instrument := lastCloseTinyCarryInstrument(101, "binance", "BTCUSDT")
	bbo := marketdata.BBO{BidPrice: "25", AskPrice: "25"}
	execution := ArbitrageExecution{
		PositionEffect: "close", ReduceOnly: true, LastCloseClip: true,
	}
	unsafe := lastCloseTinyCarryCombo("b", "-0.07")
	ready, payload := lastCloseStandaloneCarryReady(
		unsafe, execution, unsafe.LegA, instrument, bbo,
	)
	if ready || payload["reason"] != "not_reducible" {
		t.Fatalf("unsafe A buy must skip: ready=%v payload=%v", ready, payload)
	}

	safe := lastCloseTinyCarryCombo("a", "-0.07")
	ready, payload = lastCloseStandaloneCarryReady(
		safe, execution, safe.LegB, instrument, bbo,
	)
	if !ready {
		t.Fatalf("B buy should remain standalone: payload=%v", payload)
	}

	tiny := lastCloseTinyCarryCombo("a", "-0.005")
	ready, payload = lastCloseStandaloneCarryReady(
		tiny, execution, tiny.LegB, instrument, bbo,
	)
	if ready || payload["reason"] != "below_quantity_step" {
		t.Fatalf("below-step carry must skip: ready=%v payload=%v", ready, payload)
	}

	fallback := instrument
	fallback.MarketQuantityStep = ""
	fallback.MarketQuantityStepStatus = ""
	ready, payload = lastCloseStandaloneCarryReady(
		safe, execution, safe.LegB, fallback, bbo,
	)
	if !ready {
		t.Fatalf("QuantityStep fallback must still execute: payload=%v", payload)
	}

	missing := instrument
	missing.MarketQuantityStep = ""
	missing.MarketQuantityStepStatus = ""
	missing.QuantityStep = ""
	ready, payload = lastCloseStandaloneCarryReady(
		safe, execution, safe.LegB, missing, bbo,
	)
	if ready || payload["reason"] != "step_unavailable" {
		t.Fatalf("missing step must fail closed: ready=%v payload=%v", ready, payload)
	}
}

func TestArbitrageExecutorLastCloseSkipsUnsafeStandaloneCarry(t *testing.T) {
	combo := lastCloseTinyCarryCombo("b", "-0.07")
	instrumentA := lastCloseTinyCarryInstrument(101, "binance", "BTCUSDT")
	instrumentB := lastCloseTinyCarryInstrument(202, "okx", "BTC-USDT-SWAP")
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	store := &executorFailStore{combo: combo, intentStore: orderStore}
	adapterA := &stubAdapter{
		err: exchange.ErrRejected,
		place: exchange.Result{
			Status: "rejected", ErrorCode: "10001", ErrorMessage: "min notional",
		},
	}
	adapterB := &stubAdapter{
		err: exchange.ErrRejected,
		place: exchange.Result{
			Status: "rejected", ErrorCode: "10001", ErrorMessage: "min notional",
		},
	}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{101: instrumentA, 202: instrumentB},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"binance": adapterA, "okx": adapterB,
		}),
		newCloseFlattenMarket(t, "25", "25"), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	executor.Execute(
		context.Background(), combo,
		ArbitrageExecution{
			ID: "last-close-unsafe-carry", CombinationID: combo.ID,
			Direction: "bid", PositionEffect: "close", ReduceOnly: true,
			LastCloseClip: true, TargetBaseQuantity: "0.5", RequestedNotional: "12.5",
		},
		marketdata.BBO{BidPrice: "25", AskPrice: "25"},
		marketdata.BBO{BidPrice: "25", AskPrice: "25"},
		nil,
	)
	if store.combo.PositionUncertain || store.combo.RuntimeState == "position_uncertain" {
		t.Fatalf("combo=%+v", store.combo)
	}
	foundDeferred := false
	for _, eventType := range store.eventTypes {
		if eventType == lastCloseCarryDeferredEvent {
			foundDeferred = true
		}
		if eventType == "maker_accepted" {
			t.Fatalf("events=%v", store.eventTypes)
		}
	}
	if !foundDeferred {
		t.Fatalf("events=%v", store.eventTypes)
	}
	for _, request := range adapterA.requests {
		if request.Side == "buy" && request.Quantity == "0.07" {
			t.Fatalf("must not buy A carry: %+v", request)
		}
	}
	if len(adapterB.requests) == 0 || adapterB.requests[0].Side != "buy" ||
		adapterB.requests[0].Quantity != "0.5" {
		t.Fatalf("paired maker B=%+v a=%+v", adapterB.requests, adapterA.requests)
	}
}

func TestArbitrageExecutorLastCloseStandaloneCarryWhenReducible(t *testing.T) {
	combo := lastCloseTinyCarryCombo("a", "-0.07")
	instrumentA := lastCloseTinyCarryInstrument(101, "binance", "BTCUSDT")
	instrumentB := lastCloseTinyCarryInstrument(202, "okx", "BTC-USDT-SWAP")
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	store := &executorFailStore{combo: combo, intentStore: orderStore}
	adapterA := &stubAdapter{}
	adapterB := &stubAdapter{
		place: exchange.Result{
			VenueOrderID: "carry-1", Status: "filled",
			FilledQuantity: "0.07", AveragePrice: "25",
		},
	}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{101: instrumentA, 202: instrumentB},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"binance": adapterA, "okx": adapterB,
		}),
		newCloseFlattenMarket(t, "25", "25"), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	err := executor.executeMakerThenHedge(
		context.Background(), combo,
		&ArbitrageExecution{
			ID: "last-close-safe-carry", CombinationID: combo.ID,
			Direction: "bid", PositionEffect: "close", ReduceOnly: true,
			LastCloseClip: true, TargetBaseQuantity: "0.5", RequestedNotional: "12.5",
		},
		marketdata.BBO{BidPrice: "25", AskPrice: "25"},
		marketdata.BBO{BidPrice: "25", AskPrice: "25"},
		nil,
	)
	if err != nil && !errors.Is(err, ErrRiskLimit) {
		t.Fatal(err)
	}
	if len(adapterA.requests) != 0 {
		t.Fatalf("maker A placed=%+v", adapterA.requests)
	}
	if len(adapterB.requests) == 0 || adapterB.requests[0].Side != "buy" ||
		adapterB.requests[0].Quantity != "0.07" {
		t.Fatalf("expected standalone B buy 0.07: %+v", adapterB.requests)
	}
}

func TestArbitrageExecutorLastCloseBelowStepCarryContinuesPaired(t *testing.T) {
	combo := lastCloseTinyCarryCombo("a", "-0.005")
	instrumentA := lastCloseTinyCarryInstrument(101, "binance", "BTCUSDT")
	instrumentB := lastCloseTinyCarryInstrument(202, "okx", "BTC-USDT-SWAP")
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	store := &executorFailStore{combo: combo, intentStore: orderStore}
	adapterA := &stubAdapter{
		err: exchange.ErrRejected,
		place: exchange.Result{
			Status: "rejected", ErrorCode: "10001", ErrorMessage: "min notional",
		},
	}
	adapterB := &stubAdapter{}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{101: instrumentA, 202: instrumentB},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"binance": adapterA, "okx": adapterB,
		}),
		newCloseFlattenMarket(t, "25", "25"), "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	executor.Execute(
		context.Background(), combo,
		ArbitrageExecution{
			ID: "last-close-below-step", CombinationID: combo.ID,
			Direction: "bid", PositionEffect: "close", ReduceOnly: true,
			LastCloseClip: true, TargetBaseQuantity: "0.5", RequestedNotional: "12.5",
		},
		marketdata.BBO{BidPrice: "25", AskPrice: "25"},
		marketdata.BBO{BidPrice: "25", AskPrice: "25"},
		nil,
	)
	if store.combo.PositionUncertain || store.combo.RuntimeState == "position_uncertain" {
		t.Fatalf("combo=%+v", store.combo)
	}
	if len(adapterB.requests) != 0 {
		t.Fatalf("below-step must not place carry: %+v", adapterB.requests)
	}
	if len(adapterA.requests) == 0 {
		t.Fatal("paired maker should continue")
	}
}

func TestArbitrageExecutorCloseRejectsNotionalChangeAfterRecompute(t *testing.T) {
	combo := ownedTinyCloseCombo("0", "-10")
	base := &closeOrderStore{executorFailStore: &executorFailStore{combo: combo}}
	store := &mutateRecomputeStore{
		closeOrderStore: base,
		mutate: func(item *ArbitrageCombination) {
			item.LegBBasePosition = "-100"
			item.CarryBaseQuantity = "-100"
			item.LegBVenueBasePosition = "-100"
		},
	}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, nil, "1", "1")
	err := executor.CloseCombination(context.Background(), combo)
	if !errors.Is(err, ErrArbitrageNotClosable) || store.combo.Status == "closed" {
		t.Fatalf("status=%s err=%v", store.combo.Status, err)
	}
	if len(adapterA.snapshot()) != 0 || len(adapterB.snapshot()) != 0 {
		t.Fatalf("places a=%d b=%d", len(adapterA.snapshot()), len(adapterB.snapshot()))
	}
}

func TestArbitrageExecutorCloseWaitsWhenSnapshotLagsOrders(t *testing.T) {
	combo := ownedTinyCloseCombo("0", "-10")
	combo.LastPositionReconciledAt = time.Now().UTC().Add(-10 * time.Second)
	store := &closeOrderStore{executorFailStore: &executorFailStore{
		combo: combo,
		orders: []Order{{
			ID: "order-1", Status: "filled", UpdatedAt: time.Now().UTC(),
		}},
	}}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, nil, "1", "1")
	err := executor.CloseCombination(context.Background(), combo)
	if !errors.Is(err, ErrArbitrageCloseWaitingSnapshot) || store.combo.Status == "closed" {
		t.Fatalf("status=%s err=%v", store.combo.Status, err)
	}
	if len(adapterA.snapshot()) != 0 || len(adapterB.snapshot()) != 0 {
		t.Fatalf("places a=%d b=%d", len(adapterA.snapshot()), len(adapterB.snapshot()))
	}
}

func TestArbitrageExecutorCloseRejectsUnsafeLeftover(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*ArbitrageCombination)
	}{
		{
			name: "at_threshold",
			setup: func(combo *ArbitrageCombination) {
				combo.LegBBasePosition = "-20"
				combo.CarryBaseQuantity = "-20"
				combo.LegBVenueBasePosition = "-20"
			},
		},
		{
			name: "difference",
			setup: func(combo *ArbitrageCombination) {
				combo.LegBPositionDifference = "1"
			},
		},
		{
			name: "missing_baseline",
			setup: func(combo *ArbitrageCombination) {
				combo.VenueBaselineCapturedAt = time.Time{}
			},
		},
		{
			name: "stale_snapshot",
			setup: func(combo *ArbitrageCombination) {
				combo.LastPositionReconciledAt = time.Now().UTC().Add(-3 * time.Minute)
			},
		},
		{
			name: "short_venue_delta",
			setup: func(combo *ArbitrageCombination) {
				combo.LegBVenueBasePosition = "-5"
			},
		},
		{
			name: "opposite_venue_delta",
			setup: func(combo *ArbitrageCombination) {
				combo.LegBVenueBasePosition = "10"
			},
		},
		{
			name: "spot",
			setup: func(combo *ArbitrageCombination) {
				combo.LegA.ContractType = "spot"
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			combo := ownedTinyCloseCombo("0", "-10")
			tc.setup(&combo)
			store := &closeOrderStore{executorFailStore: &executorFailStore{combo: combo}}
			adapterA := &flattenVenueAdapter{}
			adapterB := &flattenVenueAdapter{}
			executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, nil, "1", "1")
			err := executor.CloseCombination(context.Background(), combo)
			if !errors.Is(err, ErrArbitrageNotClosable) || store.combo.Status == "closed" {
				t.Fatalf("status=%s err=%v", store.combo.Status, err)
			}
			if len(adapterA.snapshot()) != 0 || len(adapterB.snapshot()) != 0 {
				t.Fatalf("places a=%d b=%d", len(adapterA.snapshot()), len(adapterB.snapshot()))
			}
		})
	}
}

func TestArbitrageExecutorCloseSkipsZeroAfterStep(t *testing.T) {
	combo := ownedTinyCloseCombo("0.0004", "10")
	store := &closeOrderStore{executorFailStore: &executorFailStore{combo: combo}}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, nil, "1", "1")
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closed" {
		t.Fatalf("status=%s", store.combo.Status)
	}
	if len(adapterA.snapshot()) != 0 {
		t.Fatalf("leg a should skip zero after step, places=%d", len(adapterA.snapshot()))
	}
	if len(adapterB.snapshot()) != 1 {
		t.Fatalf("leg b places=%d", len(adapterB.snapshot()))
	}
}

func TestArbitrageExecutorClosePlaceFailureKeepsClosed(t *testing.T) {
	combo := ownedTinyCloseCombo("5", "10")
	store := &closeOrderStore{executorFailStore: &executorFailStore{combo: combo}}
	adapterA := &flattenVenueAdapter{err: errors.New("venue timeout")}
	adapterB := &flattenVenueAdapter{}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, nil, "1", "1")
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closed" {
		t.Fatalf("status=%s", store.combo.Status)
	}
	if len(adapterA.snapshot()) != 1 || len(adapterB.snapshot()) != 1 {
		t.Fatalf("places a=%d b=%d", len(adapterA.snapshot()), len(adapterB.snapshot()))
	}
}

func TestArbitrageExecutorCloseWriteFailurePlacesNothing(t *testing.T) {
	combo := ownedTinyCloseCombo("0", "-10")
	store := &failClosedWriteStore{
		closeOrderStore: &closeOrderStore{executorFailStore: &executorFailStore{combo: combo}},
	}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{}
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, nil, "1", "1")
	if err := executor.CloseCombination(context.Background(), combo); err == nil {
		t.Fatal("expected write closed failure")
	}
	if store.combo.Status == "closed" {
		t.Fatalf("status=%s", store.combo.Status)
	}
	if len(adapterA.snapshot()) != 0 || len(adapterB.snapshot()) != 0 {
		t.Fatalf("places a=%d b=%d", len(adapterA.snapshot()), len(adapterB.snapshot()))
	}
}

func TestArbitrageExecutorCloseFlattenSkipsInvalidVenueQuantity(t *testing.T) {
	combo := ownedTinyCloseCombo("0.001", "0.0005")
	store := &closeOrderStore{executorFailStore: &executorFailStore{combo: combo}}
	adapterA := &flattenVenueAdapter{}
	adapterB := &flattenVenueAdapter{}
	catalog := flattenTestCatalog()
	legB := catalog[202]
	legB.ContractSize = "0.001"
	legB.MarketQuantityStep = "0.0005"
	legB.MarketQuantityStepStatus = exchange.ConstraintKnown
	catalog[202] = legB
	executor := newOwnedFlattenExecutor(t, store, store, adapterA, adapterB, catalog, "1", "1")
	if err := executor.CloseCombination(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	if store.combo.Status != "closed" {
		t.Fatalf("status=%s", store.combo.Status)
	}
	if len(adapterA.snapshot()) != 1 {
		t.Fatalf("leg a places=%d", len(adapterA.snapshot()))
	}
	if len(adapterB.snapshot()) != 0 {
		t.Fatalf("leg b should skip invalid venue quantity, places=%d", len(adapterB.snapshot()))
	}
}

func TestExecutableHedgeQuantityConstraintStates(t *testing.T) {
	instrument := Instrument{
		QuantityStep:      "1",
		MinQuantity:       "10",
		MinQuantityStatus: exchange.ConstraintKnown,
		MinNotional:       "50",
		MinNotionalStatus: exchange.ConstraintKnown,
	}
	quantity, eligible, err := executableHedgeQuantity(
		decimal.RequireFromString("12.9"),
		decimal.RequireFromString("5"),
		instrument,
		false,
	)
	if err != nil || !eligible || quantity.String() != "12" {
		t.Fatalf("quantity=%s eligible=%v err=%v", quantity, eligible, err)
	}
	_, eligible, err = executableHedgeQuantity(
		decimal.RequireFromString("9"),
		decimal.RequireFromString("5"),
		instrument,
		false,
	)
	if err != nil || eligible {
		t.Fatalf("expected quantity dust, eligible=%v err=%v", eligible, err)
	}
	instrument.MinNotionalStatus = exchange.ConstraintNotApplicable
	_, eligible, err = executableHedgeQuantity(
		decimal.RequireFromString("10"),
		decimal.RequireFromString("1"),
		instrument,
		false,
	)
	if err != nil || !eligible {
		t.Fatalf("not-applicable notional eligible=%v err=%v", eligible, err)
	}
	instrument.MinNotionalStatus = exchange.ConstraintUnknown
	if _, _, err = executableHedgeQuantity(
		decimal.RequireFromString("10"),
		decimal.RequireFromString("5"),
		instrument,
		false,
	); err != ErrInstrumentUnavailable {
		t.Fatalf("unknown constraint err=%v", err)
	}
}

func TestExecutableHedgeQuantitySkipsMinNotionalForClose(t *testing.T) {
	instrument := Instrument{
		QuantityStep:      "1",
		MinQuantity:       "1",
		MinQuantityStatus: exchange.ConstraintKnown,
		MinNotional:       "5",
		MinNotionalStatus: exchange.ConstraintKnown,
	}
	quantity, eligible, err := executableHedgeQuantity(
		decimal.NewFromInt(20), decimal.RequireFromString("0.214"), instrument, true,
	)
	if err != nil || !eligible || quantity.String() != "20" {
		t.Fatalf("skip minNotional quantity=%s eligible=%v err=%v", quantity, eligible, err)
	}
	_, eligible, err = executableHedgeQuantity(
		decimal.NewFromInt(20), decimal.RequireFromString("0.214"), instrument, false,
	)
	if err != nil || eligible {
		t.Fatalf("open minNotional eligible=%v err=%v", eligible, err)
	}
	instrument.MinQuantity = "50"
	_, eligible, err = executableHedgeQuantity(
		decimal.NewFromInt(20), decimal.RequireFromString("0.214"), instrument, true,
	)
	if err != nil || eligible {
		t.Fatalf("minQty should stay ineligible, eligible=%v err=%v", eligible, err)
	}
}

func newFixedPriceMarket(t *testing.T, venue, symbol, bid, ask string) *marketdata.Manager {
	t.Helper()
	key, err := marketdata.NewKey(venue, "perpetual", symbol)
	if err != nil {
		t.Fatal(err)
	}
	connection := &schedulerConnection{
		reads: make(chan []byte, 1), done: make(chan struct{}),
	}
	market, err := marketdata.New(marketdata.Options{
		Connector: &schedulerConnector{
			connections: map[marketdata.Key]*schedulerConnection{key: connection},
		},
		Parsers: map[string]marketdata.Parser{
			venue: func(
				item marketdata.Key, _ []byte, received time.Time,
			) (marketdata.BBO, bool, error) {
				return marketdata.BBO{
					Key: item, BidPrice: bid, AskPrice: ask,
					VenueTimestamp: received, ReceiveTimestamp: received,
				}, true, nil
			},
		},
		StaleAfter: time.Second, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = market.Close() })
	subscription, err := market.Subscribe(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { subscription.Close() })
	connection.reads <- []byte("bbo")
	deadline := time.Now().Add(time.Second)
	for {
		if _, latestErr := market.Latest(key); latestErr == nil {
			return market
		}
		if time.Now().After(deadline) {
			t.Fatal("market data was not published")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHedgeCarryAskCloseBelowMinQtyIsUnsafeNotDust(t *testing.T) {
	instrument := lastClipCloseInstrument(202, "50", "5")
	instrument.Exchange = "okx"
	leg := ArbitrageLeg{
		TradingAccountID: 2, InstrumentID: 202,
		Exchange: "okx", ContractType: "perpetual", ExchangeSymbol: "USELESSUSDT",
	}
	combo := lastClipCloseCombination("ask")
	combo.CarryBaseQuantity = "20"
	combo.LegB = leg
	store := &executorFailStore{combo: combo}
	market := newFixedPriceMarket(t, "okx", "USELESSUSDT", "0.214", "0.214")
	executor := NewArbitrageExecutor(
		store, nil, nil, nil, nil, nil, market, "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "ask-close-dust", CombinationID: combo.ID,
		Direction: "ask", PositionEffect: "close", ReduceOnly: true,
		LastCloseClip: true,
	}
	hedged, err := executor.hedgeCarry(
		context.Background(), &combo, &execution, leg, instrument,
		Credentials{}, unusedVenueAdapter{}, nil, time.Time{},
	)
	if hedged || !errors.Is(err, ErrArbitrageUnsafeHedge) {
		t.Fatalf("hedged=%v err=%v state=%s events=%v",
			hedged, err, store.combo.RuntimeState, store.eventTypes)
	}
	for _, eventType := range store.eventTypes {
		if eventType == "hedge_deferred_dust" {
			t.Fatal("ask close must not defer dust")
		}
	}
	if store.combo.RuntimeState == "hedge_deferred_dust" {
		t.Fatalf("combo=%+v", store.combo)
	}

	exiting := lastClipCloseCombination("ask")
	exiting.Status = "running"
	exiting.RuntimeState = "monitoring"
	exiting.RunMode = "one_shot"
	exiting.OneShotPhase = "exiting"
	exiting.CarryBaseQuantity = "20"
	exiting.LegB = leg
	exitingStore := &executorFailStore{combo: exiting}
	exitingExecutor := NewArbitrageExecutor(
		exitingStore, nil, nil, nil, nil, nil, market, "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	exitingExecution := ArbitrageExecution{
		ID: "ask-close-exiting-minqty", CombinationID: exiting.ID,
		Direction: "ask", PositionEffect: "close", ReduceOnly: true,
		LastCloseClip: true,
	}
	hedged, err = exitingExecutor.hedgeCarry(
		context.Background(), &exiting, &exitingExecution, leg, instrument,
		Credentials{}, unusedVenueAdapter{}, nil, time.Time{},
	)
	if hedged || !errors.Is(err, ErrArbitrageUnsafeHedge) {
		t.Fatalf("exiting hedged=%v err=%v state=%s",
			hedged, err, exitingStore.combo.RuntimeState)
	}
	for _, eventType := range exitingStore.eventTypes {
		if eventType == "hedge_deferred_dust" {
			t.Fatal("one-shot exiting minQty must not defer dust")
		}
	}

	openStore := &executorFailStore{combo: combo}
	openExecutor := NewArbitrageExecutor(
		openStore, nil, nil, nil, nil, nil, market, "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	openExecution := ArbitrageExecution{
		ID: "ask-open-dust", CombinationID: combo.ID, Direction: "ask",
	}
	openInstrument := lastClipCloseInstrument(202, "1", "5")
	openInstrument.Exchange = "okx"
	hedged, err = openExecutor.hedgeCarry(
		context.Background(), &combo, &openExecution, leg, openInstrument,
		Credentials{}, unusedVenueAdapter{}, nil, time.Time{},
	)
	if hedged || err != nil || openStore.combo.RuntimeState != "hedge_deferred_dust" {
		t.Fatalf("open dust hedged=%v err=%v combo=%+v", hedged, err, openStore.combo)
	}
}

func TestHedgeIOCCloseBelowMinNotionalPlaces(t *testing.T) {
	instrument := lastClipCloseInstrument(202, "1", "5")
	instrument.Exchange = "okx"
	leg := ArbitrageLeg{
		TradingAccountID: 2, InstrumentID: 202,
		Exchange: "okx", ContractType: "perpetual", ExchangeSymbol: "USELESSUSDT",
	}
	combo := lastClipCloseCombination("ask")
	combo.LegB = leg
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	store := &executorFailStore{combo: combo, intentStore: orderStore}
	adapter := &stubAdapter{place: exchange.Result{
		VenueOrderID: "42", Status: "filled",
		FilledQuantity: "20", AveragePrice: "0.214",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	market := newFixedPriceMarket(t, "okx", "USELESSUSDT", "0.214", "0.214")
	executor := NewArbitrageExecutor(
		store, orderStore, service, nil, nil, nil, market, "",
		time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	execution := ArbitrageExecution{
		ID: "ioc-close-min-notional", CombinationID: combo.ID,
		Direction: "ask", PositionEffect: "close", ReduceOnly: true,
		LastCloseClip: true,
	}
	filled, err := executor.hedgeIOC(
		context.Background(), combo, &execution, leg, instrument,
		Credentials{TradingAccountID: 2}, adapter, "buy", decimal.NewFromInt(20),
		time.Now(),
	)
	if err != nil || !filled.Equal(decimal.NewFromInt(20)) {
		t.Fatalf("filled=%s err=%v", filled, err)
	}
	if len(adapter.requests) != 1 || adapter.requests[0].Quantity != "20" ||
		!adapter.requests[0].ReduceOnly {
		t.Fatalf("requests=%+v", adapter.requests)
	}
}

func TestCompleteExecutionLastCloseClipUnbalancedFills(t *testing.T) {
	combo := lastClipCloseCombination("bid")
	combo.Status = "running"
	combo.RunMode = "one_shot"
	combo.OneShotPhase = "exiting"
	execution := &ArbitrageExecution{
		ID: "exec-last-clip-unbalanced", CombinationID: combo.ID,
		Status: "hedging", Direction: "bid", PositionEffect: "close",
		ReduceOnly: true, LastCloseClip: true,
		LegAFilledQuantity: "10", LegBFilledQuantity: "0",
	}
	store := &executorFailStore{
		combo: combo, execution: *execution, activeExecution: execution,
	}
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	executor := NewArbitrageExecutor(
		store, orderStore, nil, nil, nil, nil, nil, "",
		time.Second, 0, 0, 0, time.Second, nil,
	)
	err := executor.completeExecution(context.Background(), combo, execution, parseDecimal("0.2435"))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if execution.Status != "failed" || store.execution.Status != "failed" {
		t.Fatalf("execution=%+v stored=%+v", execution, store.execution)
	}
	if execution.Status == "completed" || execution.Status == "reconciling" {
		t.Fatalf("execution=%+v", execution)
	}
	if execution.ClosedAt.IsZero() || execution.ClosedAt.Equal(time.Unix(0, 0).UTC()) {
		t.Fatalf("closed_at=%v", execution.ClosedAt)
	}
	if store.combo.RuntimeState != "manual_intervention" ||
		store.combo.Status != "running" || !store.combo.CircuitOpen {
		t.Fatalf("combo=%+v", store.combo)
	}
	if _, activeErr := store.GetActiveArbitrageExecution(context.Background(), combo.ID); !errors.Is(activeErr, ErrNotFound) {
		t.Fatalf("active err=%v", activeErr)
	}
	found := false
	for i, eventType := range store.eventTypes {
		if eventType != "last_close_clip_unbalanced" {
			continue
		}
		found = true
		if store.eventPayloads[i]["remainingHedgeQuantity"] != "10" ||
			store.eventPayloads[i]["legAFilledQuantity"] != "10" ||
			store.eventPayloads[i]["legBFilledQuantity"] != "0" {
			t.Fatalf("payload=%v", store.eventPayloads[i])
		}
	}
	if !found {
		t.Fatalf("events=%v", store.eventTypes)
	}
	for _, eventType := range store.eventTypes {
		if eventType == "hedge_deferred_dust" || eventType == "execution_completed" {
			t.Fatalf("events=%v", store.eventTypes)
		}
	}

	if err := executor.completeExecution(context.Background(), store.combo, execution, parseDecimal("0.2435")); err != nil {
		t.Fatalf("idempotent err=%v", err)
	}
	unbalancedEvents := 0
	for _, eventType := range store.eventTypes {
		if eventType == "last_close_clip_unbalanced" {
			unbalancedEvents++
		}
		if eventType == "execution_completed" || eventType == "maker_accepted" {
			t.Fatalf("events=%v", store.eventTypes)
		}
	}
	if unbalancedEvents != 1 || execution.Status != "failed" || store.execution.Status == "reconciling" {
		t.Fatalf("idempotent execution=%+v events=%v", execution, store.eventTypes)
	}

	closedCombo := combo
	closedCombo.Status = "closed"
	closedExecution := ArbitrageExecution{
		ID: "exec-last-clip-closed-combo", CombinationID: closedCombo.ID,
		Status: "hedging", Direction: "bid", PositionEffect: "close",
		ReduceOnly: true, LastCloseClip: true,
		LegAFilledQuantity: "10", LegBFilledQuantity: "0",
	}
	closedStore := &executorFailStore{combo: closedCombo, execution: closedExecution}
	if _, err := closedStore.FailLastCloseClipUnbalanced(
		context.Background(), closedCombo, closedExecution, "10", "0", "10",
	); !errors.Is(err, ErrArbitrageLastCloseClipUnbalancedConflict) {
		t.Fatalf("closed combo err=%v", err)
	}
	if closedStore.execution.Status != "hedging" ||
		closedStore.combo.RuntimeState == "manual_intervention" {
		t.Fatalf("closed combo mutated execution=%+v combo=%+v",
			closedStore.execution, closedStore.combo)
	}
	for _, eventType := range closedStore.eventTypes {
		if eventType == "last_close_clip_unbalanced" {
			t.Fatalf("closed combo wrote event: %v", closedStore.eventTypes)
		}
	}

	balanced := &ArbitrageExecution{
		ID: "exec-last-clip-balanced", CombinationID: combo.ID,
		Status: "hedging", Direction: "bid", PositionEffect: "close",
		ReduceOnly: true, LastCloseClip: true,
		LegAFilledQuantity: "10", LegBFilledQuantity: "10",
	}
	balancedStore := &executorFailStore{combo: combo, execution: *balanced}
	balancedExecutor := NewArbitrageExecutor(
		balancedStore, orderStore, nil, nil, nil, nil, nil, "",
		time.Second, 0, 0, 0, time.Second, nil,
	)
	if err := balancedExecutor.completeExecution(
		context.Background(), combo, balanced, parseDecimal("0.2435"),
	); err != nil {
		t.Fatal(err)
	}
	if balanced.Status != "completed" {
		t.Fatalf("status=%s", balanced.Status)
	}
}

func TestPairedArbitrageFillTotalsExcludeResidualIncludeNullAndEmpty(t *testing.T) {
	orders := []Order{
		{ArbitrageExecutionID: "exec-1", ArbitrageLeg: "a", ArbitrageRole: "maker", FilledQuantity: "10401"},
		{ArbitrageExecutionID: "exec-1", ArbitrageLeg: "b", ArbitrageRole: "hedge", FilledQuantity: "10401"},
		{ArbitrageExecutionID: "exec-1", ArbitrageLeg: "b", ArbitrageRole: "residual", FilledQuantity: "4501"},
		{ArbitrageExecutionID: "exec-1", ArbitrageLeg: "a", FilledQuantity: "100"},
		{ArbitrageExecutionID: "exec-1", ArbitrageLeg: "b", ArbitrageRole: "", FilledQuantity: "50"},
		{ArbitrageExecutionID: "exec-2", ArbitrageLeg: "a", ArbitrageRole: "maker", FilledQuantity: "1"},
	}
	legA, legB, found := pairedArbitrageFillTotals(orders, "exec-1")
	if !found || !legA.Equal(decimal.NewFromInt(10501)) || !legB.Equal(decimal.NewFromInt(10451)) {
		t.Fatalf("paired a=%s b=%s found=%v", legA, legB, found)
	}
}

func oneShotExitingLastClipCombo() ArbitrageCombination {
	combo := lastClipCloseCombination("bid")
	combo.Status = "running"
	combo.RuntimeState = "monitoring"
	combo.RunMode = "one_shot"
	combo.OneShotPhase = "exiting"
	combo.LegABasePosition = "10"
	combo.LegBBasePosition = "-10"
	combo.LegA.Exchange = "gate"
	combo.LegA.ExchangeSymbol = "USELESS_USDT"
	combo.LegB.Exchange = "bybit"
	combo.LegB.ExchangeSymbol = "USELESSUSDT"
	return combo
}

func oneShotExitingLastClipInstrument(id int64, venue, symbol string) Instrument {
	item := lastClipCloseInstrument(id, "1", "5")
	item.Exchange = venue
	item.ExchangeSymbol = symbol
	item.PriceTick = "0.0001"
	item.MarketQuantityStep = "1"
	return item
}

func TestOneShotExitingLastClipHedgeSkipsMinNotional(t *testing.T) {
	makerInstrument := oneShotExitingLastClipInstrument(101, "gate", "USELESS_USDT")
	hedgeInstrument := oneShotExitingLastClipInstrument(202, "bybit", "USELESSUSDT")
	combo := oneShotExitingLastClipCombo()
	market := newFixedPriceMarket(t, "bybit", "USELESSUSDT", "0.2435", "0.2435")
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	store := &executorFailStore{combo: combo, intentStore: orderStore}
	gate := &stubAdapter{place: exchange.Result{
		VenueOrderID: "maker-1", Status: "filled",
		FilledQuantity: "10", AveragePrice: "0.2435",
	}}
	bybit := &stubAdapter{place: exchange.Result{
		VenueOrderID: "hedge-1", Status: "filled",
		FilledQuantity: "10", AveragePrice: "0.2435",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{101: makerInstrument, 202: hedgeInstrument},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"gate": gate, "bybit": bybit,
		}),
		market, "", time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	bbo := marketdata.BBO{
		BidPrice: "0.2435", AskPrice: "0.2435",
		BidQuantity: "100000", AskQuantity: "100000",
	}
	executor.Execute(
		context.Background(), combo,
		ArbitrageExecution{
			ID: "one-shot-exit-last-clip", CombinationID: combo.ID,
			Direction: "bid", PositionEffect: "close", ReduceOnly: true,
			LastCloseClip: true, TargetBaseQuantity: "10", RequestedNotional: "2.435",
		},
		bbo, bbo, nil,
	)
	store.mu.Lock()
	execution := store.execution
	runtimeState := store.combo.RuntimeState
	carry := store.combo.CarryBaseQuantity
	eventTypes := append([]string(nil), store.eventTypes...)
	store.mu.Unlock()
	if execution.Status != "completed" {
		t.Fatalf("execution=%+v state=%s events=%v", execution, runtimeState, eventTypes)
	}
	if execution.LegAFilledQuantity != "10" || execution.LegBFilledQuantity != "10" {
		t.Fatalf("fills a=%s b=%s", execution.LegAFilledQuantity, execution.LegBFilledQuantity)
	}
	if parseDecimal(carry).IsZero() == false && carry != "0" {
		t.Fatalf("carry=%s", carry)
	}
	for _, eventType := range eventTypes {
		if eventType == "hedge_deferred_dust" {
			t.Fatalf("events=%v", eventTypes)
		}
	}
	if len(bybit.requests) == 0 || !bybit.requests[0].ReduceOnly {
		t.Fatalf("bybit requests=%+v", bybit.requests)
	}
	if bybit.requests[0].Quantity != "10" {
		t.Fatalf("hedge quantity=%s", bybit.requests[0].Quantity)
	}
}

func TestOneShotExitingLastClipHedgeRejectDoesNotComplete(t *testing.T) {
	makerInstrument := oneShotExitingLastClipInstrument(101, "gate", "USELESS_USDT")
	hedgeInstrument := oneShotExitingLastClipInstrument(202, "bybit", "USELESSUSDT")
	combo := oneShotExitingLastClipCombo()
	market := newFixedPriceMarket(t, "bybit", "USELESSUSDT", "0.2435", "0.2435")
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	store := &executorFailStore{combo: combo, intentStore: orderStore}
	gate := &stubAdapter{place: exchange.Result{
		VenueOrderID: "maker-1", Status: "filled",
		FilledQuantity: "10", AveragePrice: "0.2435",
	}}
	bybit := &stubAdapter{
		err: exchange.ErrRejected,
		place: exchange.Result{
			Status: "rejected", ErrorCode: "10001", ErrorMessage: "min notional",
		},
	}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{101: makerInstrument, 202: hedgeInstrument},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"gate": gate, "bybit": bybit,
		}),
		market, "", time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	bbo := marketdata.BBO{
		BidPrice: "0.2435", AskPrice: "0.2435",
		BidQuantity: "100000", AskQuantity: "100000",
	}
	executor.Execute(
		context.Background(), combo,
		ArbitrageExecution{
			ID: "one-shot-exit-last-clip-reject", CombinationID: combo.ID,
			Direction: "bid", PositionEffect: "close", ReduceOnly: true,
			LastCloseClip: true, TargetBaseQuantity: "10", RequestedNotional: "2.435",
		},
		bbo, bbo, nil,
	)
	store.mu.Lock()
	execution := store.execution
	runtimeState := store.combo.RuntimeState
	eventTypes := append([]string(nil), store.eventTypes...)
	uncertain := store.combo.PositionUncertain
	store.mu.Unlock()
	if execution.Status == "completed" {
		t.Fatalf("execution=%+v state=%s events=%v", execution, runtimeState, eventTypes)
	}
	if execution.Status != "reconciling" && execution.Status != "hedging" &&
		runtimeState != "reconciling" && runtimeState != "position_uncertain" && !uncertain {
		t.Fatalf("execution=%+v state=%s uncertain=%v events=%v",
			execution, runtimeState, uncertain, eventTypes)
	}
	for _, eventType := range eventTypes {
		if eventType == "hedge_deferred_dust" {
			t.Fatalf("events=%v", eventTypes)
		}
	}
}

func TestNormalizeHyperliquidHedgeIOCPrice(t *testing.T) {
	tests := []struct {
		name       string
		instrument Instrument
		side       string
		price      string
		want       string
		wantErr    error
	}{
		{
			name: "cashcat sell rounds down",
			instrument: Instrument{
				Exchange: "hyperliquid", QuantityStep: "0.1", PriceTick: "0.00001",
			},
			side: "sell", price: "0.216187", want: "0.21618",
		},
		{
			name: "cashcat buy rounds up",
			instrument: Instrument{
				Exchange: "HYPERLIQUID", QuantityStep: "0.1", PriceTick: "0.00001",
			},
			side: "buy", price: "0.216187", want: "0.21619",
		},
		{
			name: "size decimals cap price decimals",
			instrument: Instrument{
				Exchange: "hyperliquid", QuantityStep: "0.001", PriceTick: "0.001",
			},
			side: "sell", price: "0.216187", want: "0.216",
		},
		{
			name: "legal price unchanged",
			instrument: Instrument{
				Exchange: "hyperliquid", QuantityStep: "0.1", PriceTick: "0.00001",
			},
			side: "sell", price: "0.21618", want: "0.21618",
		},
		{
			name: "integer price always allowed",
			instrument: Instrument{
				Exchange: "hyperliquid", QuantityStep: "1", PriceTick: "0.000001",
			},
			side: "buy", price: "123456", want: "123456",
		},
		{
			name:       "other venue unchanged",
			instrument: Instrument{Exchange: "binance", QuantityStep: "0.005", PriceTick: "0.000001"},
			side:       "sell", price: "0.216187", want: "0.216187",
		},
		{
			name:       "invalid Hyperliquid size step fails closed",
			instrument: Instrument{Exchange: "hyperliquid", QuantityStep: "0.005", PriceTick: "0.000001"},
			side:       "sell", price: "0.216187", wantErr: ErrInstrumentUnavailable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeHedgeIOCPrice(test.instrument, test.side, test.price)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("err=%v want=%v", err, test.wantErr)
			}
			if test.wantErr != nil {
				return
			}
			if got != test.want {
				t.Fatalf("price=%s want=%s", got, test.want)
			}
			tick := parsePositiveDecimal(test.instrument.PriceTick)
			if strings.EqualFold(test.instrument.Exchange, "hyperliquid") &&
				!parsePositiveDecimal(got).Mod(tick).IsZero() {
				t.Fatalf("price=%s does not satisfy tick=%s", got, tick)
			}
		})
	}
}

func TestHyperliquidHedgeIOCRetriesAfterZeroFillCancel(t *testing.T) {
	key, err := marketdata.NewKey("hyperliquid", "perpetual", "CASHCAT")
	if err != nil {
		t.Fatal(err)
	}
	connection := &schedulerConnection{
		reads: make(chan []byte, 1), done: make(chan struct{}),
	}
	market, err := marketdata.New(marketdata.Options{
		Connector: &schedulerConnector{
			connections: map[marketdata.Key]*schedulerConnection{key: connection},
		},
		Parsers: map[string]marketdata.Parser{
			"hyperliquid": func(
				key marketdata.Key,
				_ []byte,
				received time.Time,
			) (marketdata.BBO, bool, error) {
				return marketdata.BBO{
					Key: key, BidPrice: "0.216187", AskPrice: "0.216188",
					VenueTimestamp: received, ReceiveTimestamp: received,
				}, true, nil
			},
		},
		StaleAfter: time.Second, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer market.Close()
	subscription, err := market.Subscribe(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	connection.reads <- []byte("bbo")
	deadline := time.Now().Add(time.Second)
	for {
		if _, latestErr := market.Latest(key); latestErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("market data was not published")
		}
		time.Sleep(time.Millisecond)
	}

	instrument := Instrument{
		ID: 202, Exchange: "hyperliquid", ContractType: "perpetual",
		ExchangeSymbol: "CASHCAT", BaseAsset: "CASHCAT", QuoteAsset: "USDC",
		QuantityStep: "1", PriceTick: "0.000001",
		MinQuantityStatus: exchange.ConstraintNotApplicable,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotionalStatus: exchange.ConstraintNotApplicable,
	}
	leg := ArbitrageLeg{
		TradingAccountID: 2, InstrumentID: instrument.ID,
		Exchange: "hyperliquid", ContractType: "perpetual",
		ExchangeSymbol: "CASHCAT",
	}
	combination := ArbitrageCombination{
		ID:            "46c5b9e3-3fdd-4930-b529-c2d902b61746",
		OwnerUsername: "admin", LegB: leg,
	}
	execution := ArbitrageExecution{
		ID: "hyperliquid-normalized-hedge", CombinationID: combination.ID,
	}
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	arbitrageStore := &executorFailStore{combo: combination, intentStore: orderStore}
	adapter := &stubAdapter{placeResults: []exchange.Result{
		{Status: "canceled", FilledQuantity: "0"},
		{
			VenueOrderID: "42", Status: "filled",
			FilledQuantity: "90", AveragePrice: "0.21618",
		},
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		arbitrageStore, orderStore, service, nil, nil, nil, market, "",
		time.Millisecond, 0, 0, 3, time.Second, nil,
	)
	filled, err := executor.hedgeIOC(
		context.Background(), combination, &execution, leg, instrument,
		Credentials{TradingAccountID: 2}, adapter, "sell", decimal.NewFromInt(90),
		time.Now(),
	)
	if err != nil || !filled.Equal(decimal.NewFromInt(90)) {
		t.Fatalf("filled=%s err=%v", filled, err)
	}
	if len(adapter.requests) != 2 {
		t.Fatalf("requests=%+v", adapter.requests)
	}
	for _, request := range adapter.requests {
		if request.Price != "0.21618" || request.Quantity != "90" ||
			request.TimeInForce != "IOC" {
			t.Fatalf("request=%+v", request)
		}
	}
	orderStore.mu.Lock()
	if len(orderStore.orders) != 2 {
		orderStore.mu.Unlock()
		t.Fatalf("orders=%+v", orderStore.orders)
	}
	statuses := map[string]int{}
	for _, order := range orderStore.orders {
		statuses[order.Status]++
		if order.Price != "0.21618" {
			orderStore.mu.Unlock()
			t.Fatalf("order=%+v", order)
		}
	}
	orderStore.mu.Unlock()
	if statuses["canceled"] != 1 || statuses["filled"] != 1 {
		t.Fatalf("statuses=%v", statuses)
	}

	fatalStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	fatalAdapter := &stubAdapter{err: exchange.ErrRejected}
	fatalService := NewService(fatalStore, nil, nil, nil, time.Second, nil)
	fatalArbitrage := &executorFailStore{combo: combination, intentStore: fatalStore}
	fatalExecutor := NewArbitrageExecutor(
		fatalArbitrage, fatalStore, fatalService, nil, nil, nil, market, "",
		time.Millisecond, 0, 0, 3, time.Second, nil,
	)
	fatalExecution := ArbitrageExecution{
		ID: "hyperliquid-rejected-hedge", CombinationID: combination.ID,
	}
	fatalFilled, fatalErr := fatalExecutor.hedgeIOC(
		context.Background(), combination, &fatalExecution, leg, instrument,
		Credentials{TradingAccountID: 2}, fatalAdapter, "sell", decimal.NewFromInt(90),
		time.Now(),
	)
	if !errors.Is(fatalErr, ErrVenueRejected) || !fatalFilled.IsZero() ||
		fatalAdapter.calls != 1 {
		t.Fatalf("filled=%s err=%v calls=%d", fatalFilled, fatalErr, fatalAdapter.calls)
	}
}

func TestBybitMakerHyperliquidHedgeCompletesWithNormalizedPrice(t *testing.T) {
	hedgeKey, err := marketdata.NewKey("hyperliquid", "perpetual", "CASHCAT")
	if err != nil {
		t.Fatal(err)
	}
	connection := &schedulerConnection{
		reads: make(chan []byte, 1), done: make(chan struct{}),
	}
	market, err := marketdata.New(marketdata.Options{
		Connector: &schedulerConnector{
			connections: map[marketdata.Key]*schedulerConnection{hedgeKey: connection},
		},
		Parsers: map[string]marketdata.Parser{
			"hyperliquid": func(
				key marketdata.Key,
				_ []byte,
				received time.Time,
			) (marketdata.BBO, bool, error) {
				return marketdata.BBO{
					Key: key, BidPrice: "0.216187", AskPrice: "0.216188",
					VenueTimestamp: received, ReceiveTimestamp: received,
				}, true, nil
			},
		},
		StaleAfter: time.Second, ReconnectInitial: time.Hour, ReconnectMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer market.Close()
	subscription, err := market.Subscribe(context.Background(), hedgeKey)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	connection.reads <- []byte("bbo")
	deadline := time.Now().Add(time.Second)
	for {
		if _, latestErr := market.Latest(hedgeKey); latestErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("market data was not published")
		}
		time.Sleep(time.Millisecond)
	}

	makerInstrument := Instrument{
		ID: 101, Exchange: "bybit", ContractType: "perpetual",
		ExchangeSymbol: "CASHCATUSDT", BaseAsset: "CASHCAT", QuoteAsset: "USDT",
		QuantityStep: "1", PriceTick: "0.000001",
		MinQuantityStatus: exchange.ConstraintNotApplicable,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotionalStatus: exchange.ConstraintNotApplicable,
	}
	hedgeInstrument := Instrument{
		ID: 202, Exchange: "hyperliquid", ContractType: "perpetual",
		ExchangeSymbol: "CASHCAT", BaseAsset: "CASHCAT", QuoteAsset: "USDC",
		QuantityStep: "1", PriceTick: "0.000001",
		MinQuantityStatus: exchange.ConstraintNotApplicable,
		MaxQuantityStatus: exchange.ConstraintNotApplicable,
		MinNotionalStatus: exchange.ConstraintNotApplicable,
	}
	combination := ArbitrageCombination{
		ID:            "46c5b9e3-3fdd-4930-b529-c2d902b61747",
		OwnerUsername: "admin", Status: "running", RuntimeState: "monitoring",
		ExecutionMode: "maker_then_hedge", MakerLeg: "a",
		OrderNotional: "0.216188", CarryBaseQuantity: "0",
		LegA: ArbitrageLeg{
			TradingAccountID: 1, InstrumentID: makerInstrument.ID,
			Exchange: "bybit", ContractType: "perpetual",
			ExchangeSymbol: makerInstrument.ExchangeSymbol,
		},
		LegB: ArbitrageLeg{
			TradingAccountID: 2, InstrumentID: hedgeInstrument.ID,
			Exchange: "hyperliquid", ContractType: "perpetual",
			ExchangeSymbol: hedgeInstrument.ExchangeSymbol,
		},
	}
	orderStore := &postOnlyOrderStore{memoryStore: newMemoryStore()}
	baseStore := &executorFailStore{combo: combination, intentStore: orderStore}
	store := &makerHedgeStore{executorFailStore: baseStore, orderStore: orderStore.memoryStore}
	bybit := &stubAdapter{place: exchange.Result{
		VenueOrderID: "maker-1", Status: "filled",
		FilledQuantity: "1", AveragePrice: "0.216188",
	}}
	hyperliquid := &stubAdapter{place: exchange.Result{
		VenueOrderID: "hedge-1", Status: "filled",
		FilledQuantity: "1", AveragePrice: "0.21619",
	}}
	service := NewService(orderStore, nil, nil, nil, time.Second, nil)
	executor := NewArbitrageExecutor(
		store, orderStore, service,
		silentRiskCatalog{
			makerInstrument.ID: makerInstrument, hedgeInstrument.ID: hedgeInstrument,
		},
		silentRiskCredentials{},
		exchange.NewTestRegistry(map[string]exchange.Adapter{
			"bybit": bybit, "hyperliquid": hyperliquid,
		}),
		market, "", time.Millisecond, 0, 0, 1, time.Second, nil,
	)
	executor.Execute(
		context.Background(), combination,
		ArbitrageExecution{
			ID: "bybit-maker-hyperliquid-hedge", CombinationID: combination.ID,
			Direction: "ask", RequestedNotional: "0.216188",
			TriggerLegABid: "0.216187", TriggerLegAAsk: "0.216188",
			TriggerLegBBid: "0.216187", TriggerLegBAsk: "0.216188",
		},
		marketdata.BBO{BidPrice: "0.216187", AskPrice: "0.216188"},
		marketdata.BBO{BidPrice: "0.216187", AskPrice: "0.216188"},
		nil,
	)

	baseStore.mu.Lock()
	execution := baseStore.execution
	failures := baseStore.failures
	runtimeState := baseStore.combo.RuntimeState
	carry := baseStore.combo.CarryBaseQuantity
	baseStore.mu.Unlock()
	if execution.Status != "completed" ||
		execution.LegAFilledQuantity != "1" ||
		execution.LegBFilledQuantity != "1" ||
		failures != 0 || runtimeState == "backoff" || carry != "0" {
		t.Fatalf(
			"execution=%+v failures=%d runtime=%s carry=%s",
			execution, failures, runtimeState, carry,
		)
	}
	if len(bybit.requests) != 1 || len(hyperliquid.requests) != 1 ||
		hyperliquid.requests[0].Side != "sell" ||
		hyperliquid.requests[0].Price != "0.21618" ||
		hyperliquid.requests[0].TimeInForce != "IOC" {
		t.Fatalf(
			"bybitRequests=%+v hyperliquidRequests=%+v",
			bybit.requests, hyperliquid.requests,
		)
	}
}

func TestOrderIntentDistinguishesSameAccountSpotAndPerpetualLegs(t *testing.T) {
	executor := &ArbitrageExecutor{}
	combination := ArbitrageCombination{
		OwnerUsername: "admin",
		LegA:          ArbitrageLeg{TradingAccountID: 7, InstrumentID: 101},
		LegB:          ArbitrageLeg{TradingAccountID: 7, InstrumentID: 202},
	}
	execution := ArbitrageExecution{ID: "execution-1", ReduceOnly: true}
	intent := executor.orderIntent(
		combination, execution, combination.LegB,
		Instrument{ID: 202, ContractType: "perpetual"},
		"buy", "limit", "1", "100", "hedge", 0,
	)
	if intent.ArbitrageLeg != "b" || !intent.ReduceOnly ||
		intent.IdempotencyKey != "arb:execution-1:b:hedge:0" {
		t.Fatalf("intent=%+v", intent)
	}
	spotIntent := executor.orderIntent(
		combination, execution, combination.LegA,
		Instrument{ID: 101, ContractType: "spot"},
		"sell", "limit", "1", "100", "maker", 0,
	)
	if spotIntent.ArbitrageLeg != "a" || spotIntent.ReduceOnly {
		t.Fatalf("spot intent=%+v", spotIntent)
	}
}

func TestBidSpotSnapshotIsCachedPerExecution(t *testing.T) {
	provider := &countingSpotSnapshots{snapshot: portfolio.Snapshot{
		SpotBalances: map[string]string{"BTC": "1.25"},
	}}
	service := &Service{portfolios: provider}
	executor := &ArbitrageExecutor{
		service: service, spotSnapshots: make(map[string]map[int64]portfolio.Snapshot),
	}
	credentials := Credentials{
		TradingAccountID: 7, APIKey: "key", APISecret: "secret",
	}
	leg := ArbitrageLeg{Exchange: "binance", BaseAsset: "BTC"}
	for range 2 {
		snapshot, err := executor.bidSpotSnapshot(
			context.Background(), "execution-1", leg, credentials,
		)
		if err != nil {
			t.Fatal(err)
		}
		if available := spotAvailableForSide(snapshot, "BTC", "sell"); !available.Equal(
			decimal.RequireFromString("1.25"),
		) {
			t.Fatalf("available=%s", available)
		}
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.calls != 1 {
		t.Fatalf("snapshot calls=%d", provider.calls)
	}
}

func TestReduceSpotSnapshotFailureFailsClosed(t *testing.T) {
	provider := &countingSpotSnapshots{err: errors.New("snapshot unavailable")}
	executor := &ArbitrageExecutor{
		service:       &Service{portfolios: provider},
		spotSnapshots: make(map[string]map[int64]portfolio.Snapshot),
	}
	err := executor.prefetchReduceSpotSnapshots(
		context.Background(),
		ArbitrageCombination{},
		ArbitrageExecution{
			ID: "execution-2", Direction: "ask",
			PositionEffect: "close", ReduceOnly: true,
		},
		arbitrageExecutionLeg{
			leg:         ArbitrageLeg{Exchange: "binance", ExchangeSymbol: "BTCUSDT"},
			instrument:  Instrument{ContractType: "spot"},
			credentials: Credentials{TradingAccountID: 7},
		},
	)
	if !errors.Is(err, ErrVenueUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestReduceCapsDoNotCrossVenueZero(t *testing.T) {
	combination := ArbitrageCombination{
		VenueBaselineCapturedAt:       time.Now().UTC(),
		LegAVenueBaselineBasePosition: "2",
		LegBVenueBaselineBasePosition: "-3",
		LegABasePosition:              "-0.5",
		LegBBasePosition:              "1",
		LegA:                          ArbitrageLeg{TradingAccountID: 1, InstrumentID: 11},
		LegB:                          ArbitrageLeg{TradingAccountID: 2, InstrumentID: 22},
	}
	if available := arbitrageReduceLegAvailable(
		combination, "a", "sell",
	); !available.Equal(decimal.RequireFromString("1.5")) {
		t.Fatalf("leg A available=%s", available)
	}
	if available := arbitrageReduceLegAvailable(
		combination, "b", "buy",
	); !available.Equal(decimal.NewFromInt(2)) {
		t.Fatalf("leg B available=%s", available)
	}
	if available := arbitrageReduceLegAvailable(
		combination, "a", "buy",
	); !available.IsZero() {
		t.Fatalf("reverse hedge available=%s", available)
	}

	combination.LegAVenueBaselineBasePosition = "-2"
	combination.LegBVenueBaselineBasePosition = "3"
	combination.LegABasePosition = "0.5"
	combination.LegBBasePosition = "-1"
	if available := arbitrageReduceLegAvailable(
		combination, "a", "buy",
	); !available.Equal(decimal.RequireFromString("1.5")) {
		t.Fatalf("ask leg A available=%s", available)
	}
	if available := arbitrageReduceLegAvailable(
		combination, "b", "sell",
	); !available.Equal(decimal.NewFromInt(2)) {
		t.Fatalf("ask leg B available=%s", available)
	}
}
