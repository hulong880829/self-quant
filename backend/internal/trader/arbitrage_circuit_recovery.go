package trader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

const (
	bybitInsufficientBalanceCode = "110007"
	circuitCloseExplained        = "explained"
	circuitCloseFilled           = "filled"
	circuitHedgeRetryEligible    = "circuit_hedge_retry_eligible"
	circuitHedgeRetryStarted     = "circuit_hedge_retry_started"
	circuitHedgeRetryRejected    = "circuit_hedge_retry_rejected"
	circuitHedgeRetryCompleted   = "circuit_hedge_retry_completed"
	circuitQuoteRetryEligible    = "circuit_quote_retry_eligible"
	circuitQuoteRetryStarted     = "circuit_quote_retry_started"
	circuitQuoteRetryRejected    = "circuit_quote_retry_rejected"
	circuitQuoteRetryCompleted   = "circuit_quote_retry_completed"
)

type circuitRecoveryContextKey struct{}

func withCircuitRecoveryCompletion(ctx context.Context) context.Context {
	return context.WithValue(ctx, circuitRecoveryContextKey{}, true)
}

func circuitRecoveryCompletion(ctx context.Context) bool {
	enabled, _ := ctx.Value(circuitRecoveryContextKey{}).(bool)
	return enabled
}

func circuitOpenManualHotPathSuppressed(combination ArbitrageCombination) bool {
	if !combination.CircuitOpen ||
		!strings.EqualFold(strings.TrimSpace(combination.RuntimeState), "manual_intervention") {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(combination.Status), "running") {
		return false
	}
	if arbitrageFlattening(combination) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(combination.OneShotPhase)) {
	case "", "building", "building_target", "waiting_exit":
		return true
	default:
		return false
	}
}

func circuitRecoverySnapshotFresh(started, observed, now time.Time) bool {
	if started.IsZero() || observed.IsZero() || observed.Before(started) {
		return false
	}
	age := now.Sub(observed)
	return age >= 0 && age <= circuitOpenReconcileInterval
}

func recoverableMarginReject(order Order) bool {
	return explicitVenueRejectedZeroFill(order) &&
		strings.EqualFold(strings.TrimSpace(order.Exchange), "bybit") &&
		strings.TrimSpace(order.ErrorCode) == bybitInsufficientBalanceCode
}

func circuitOrderNeedsConfirm(order Order) bool {
	if explicitVenueRejectedZeroFill(order) || confirmedAbsentReliableZeroFill(order) {
		return false
	}
	return !terminalStatus(order.Status)
}

func hedgeRecoveryOrder(execution ArbitrageExecution, orders []Order) (Order, bool) {
	if terminalArbitrageExecutionStatus(execution.Status) {
		return Order{}, false
	}
	hedgeID := strings.TrimSpace(execution.HedgeOrderID)
	if hedgeID == "" {
		return Order{}, false
	}
	for _, order := range orders {
		if order.ID != hedgeID {
			continue
		}
		if order.ArbitrageExecutionID != execution.ID ||
			!isArbitrageCarryHedgeRole(order.ArbitrageRole) ||
			!terminalStatus(order.Status) || order.ReconcileFailures > 0 {
			return Order{}, false
		}
		filled := parseDecimal(order.FilledQuantity)
		if filled.IsPositive() || recoverableMarginReject(order) {
			return order, true
		}
		return Order{}, false
	}
	return Order{}, false
}

func quoteRecoveryOrder(execution ArbitrageExecution, orders []Order) (Order, bool) {
	if !strings.EqualFold(strings.TrimSpace(execution.Status), "failed") ||
		!strings.EqualFold(strings.TrimSpace(execution.PositionEffect), "open") ||
		execution.LastCloseClip {
		return Order{}, false
	}
	var selected Order
	found := false
	makerID := strings.TrimSpace(execution.MakerOrderID)
	for _, order := range orders {
		if order.ArbitrageExecutionID != execution.ID {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(order.ArbitrageRole))
		if role != "maker" && role != "quote" {
			continue
		}
		if makerID != "" && order.ID != makerID {
			continue
		}
		selected = order
		found = true
		if makerID != "" {
			break
		}
	}
	if !found || !recoverableMarginReject(selected) {
		return Order{}, false
	}
	return selected, true
}

func latestFailedArbitrageExecution(items []ArbitrageExecution) (ArbitrageExecution, bool) {
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item.Status), "failed") {
			return item, true
		}
	}
	return ArbitrageExecution{}, false
}

func combinationTerminalOrderSums(orders []Order) (sumA, sumB decimal.Decimal, allTerminal bool) {
	allTerminal = true
	for _, order := range orders {
		if !terminalStatus(order.Status) || order.ReconcileFailures > 0 {
			allTerminal = false
		}
		if !parseDecimal(order.FilledQuantity).IsPositive() || !terminalStatus(order.Status) {
			continue
		}
		delta := signedFilledBase(order)
		if strings.EqualFold(strings.TrimSpace(order.ArbitrageLeg), "b") {
			sumB = sumB.Add(delta)
		} else {
			sumA = sumA.Add(delta)
		}
	}
	return sumA, sumB, allTerminal
}

func executionConfirmedLegFills(executionID string, orders []Order) (fillA, fillB decimal.Decimal) {
	for _, order := range orders {
		if strings.TrimSpace(order.ArbitrageExecutionID) != strings.TrimSpace(executionID) {
			continue
		}
		if !terminalStatus(order.Status) {
			continue
		}
		filled := parseDecimal(order.FilledQuantity)
		if !filled.IsPositive() {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(order.ArbitrageLeg), "b") {
			fillB = fillB.Add(filled)
		} else {
			fillA = fillA.Add(filled)
		}
	}
	return fillA, fillB
}

func (e *ArbitrageExecutor) logCircuitRecoverySkip(combinationID, reason string, attrs ...any) {
	if e == nil {
		return
	}
	key := combinationID + "|" + reason
	e.mu.Lock()
	if e.circuitSkipLogged == nil {
		e.circuitSkipLogged = map[string]time.Time{}
	}
	if last, ok := e.circuitSkipLogged[key]; ok && time.Since(last) < circuitOpenReconcileInterval {
		e.mu.Unlock()
		return
	}
	e.circuitSkipLogged[key] = time.Now()
	e.mu.Unlock()
	if e.logger == nil {
		return
	}
	e.logger.Info("circuit open recovery skipped", append([]any{
		"combination_id", combinationID, "reason", reason,
	}, attrs...)...)
}

func (e *ArbitrageExecutor) recoverCircuitOpen(
	ctx context.Context,
	combination ArbitrageCombination,
) error {
	if e == nil || e.store == nil {
		return nil
	}
	if !combination.CircuitOpen || !strings.EqualFold(combination.Status, "running") {
		return nil
	}
	if e.catalog == nil || e.credentials == nil || e.venues == nil || e.portfolios == nil {
		e.logCircuitRecoverySkip(combination.ID, "dependencies_unavailable")
		return nil
	}
	started := time.Now().UTC()
	timeout := e.timeout
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	execution, active, err := e.circuitRecoveryExecution(queryCtx, combination.ID)
	if err != nil {
		return err
	}
	orders, err := e.store.ListArbitrageOrders(queryCtx, combination.OwnerUsername, combination.ID)
	if err != nil {
		return err
	}
	if active {
		return e.recoverActiveCircuitExecution(
			ctx, queryCtx, combination, execution, orders, started,
		)
	}
	failed, ok := latestFailedArbitrageExecution(ordersExecutionFallback(execution, orders))
	if !ok {
		executions, listErr := e.store.ListArbitrageExecutions(queryCtx, combination.ID, 20)
		if listErr != nil {
			return listErr
		}
		failed, ok = latestFailedArbitrageExecution(executions)
	}
	if !ok {
		e.logCircuitRecoverySkip(combination.ID, "no_failed_execution")
		return nil
	}
	return e.recoverFailedCircuitQuote(ctx, queryCtx, combination, failed, orders)
}

func ordersExecutionFallback(execution ArbitrageExecution, _ []Order) []ArbitrageExecution {
	if execution.ID == "" {
		return nil
	}
	return []ArbitrageExecution{execution}
}

func (e *ArbitrageExecutor) circuitRecoveryExecution(
	ctx context.Context,
	combinationID string,
) (ArbitrageExecution, bool, error) {
	execution, err := e.store.GetActiveArbitrageExecution(ctx, combinationID)
	if err == nil {
		return execution, true, nil
	}
	if err != nil && !isNotFound(err) {
		return ArbitrageExecution{}, false, err
	}
	executions, listErr := e.store.ListArbitrageExecutions(ctx, combinationID, 20)
	if listErr != nil {
		return ArbitrageExecution{}, false, listErr
	}
	failed, ok := latestFailedArbitrageExecution(executions)
	if !ok {
		return ArbitrageExecution{}, false, nil
	}
	return failed, false, nil
}

func isNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}

func (e *ArbitrageExecutor) recoverActiveCircuitExecution(
	ctx, queryCtx context.Context,
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	orders []Order,
	started time.Time,
) error {
	for _, order := range orders {
		if order.ArbitrageExecutionID == execution.ID {
			continue
		}
		if !terminalStatus(order.Status) || order.ReconcileFailures > 0 {
			e.logCircuitRecoverySkip(combination.ID, "other_orders_unconfirmed")
			return nil
		}
	}
	instrumentA, credentialsA, adapterA, err := e.executionDependencies(
		queryCtx, combination.OwnerUsername, combination.LegA,
	)
	if err != nil {
		return err
	}
	if credentialsA.Exchange == "" {
		credentialsA.Exchange = combination.LegA.Exchange
	}
	instrumentB, credentialsB, adapterB, err := e.executionDependencies(
		queryCtx, combination.OwnerUsername, combination.LegB,
	)
	if err != nil {
		return err
	}
	if credentialsB.Exchange == "" {
		credentialsB.Exchange = combination.LegB.Exchange
	}
	confirmed := false
	for _, order := range orders {
		if order.ArbitrageExecutionID != execution.ID || !circuitOrderNeedsConfirm(order) {
			continue
		}
		instrument, credentials, adapter := instrumentA, credentialsA, adapterA
		if strings.EqualFold(order.ArbitrageLeg, "b") {
			instrument, credentials, adapter = instrumentB, credentialsB, adapterB
		}
		if _, confirmErr := e.confirmCircuitOpenOrder(
			queryCtx, credentials, instrument, adapter, order,
		); confirmErr != nil {
			e.logCircuitRecoverySkip(combination.ID, "order_unconfirmed",
				"order_id", order.ID, "error", confirmErr)
			return nil
		}
		confirmed = true
	}
	if confirmed {
		orders, err = e.store.ListArbitrageOrders(queryCtx, combination.OwnerUsername, combination.ID)
		if err != nil {
			return err
		}
	}
	for _, order := range orders {
		if order.ArbitrageExecutionID == execution.ID && circuitOrderNeedsConfirm(order) {
			e.logCircuitRecoverySkip(combination.ID, "order_inflight", "order_id", order.ID)
			return nil
		}
	}

	snapshotA, err := e.circuitOpenAccountSnapshot(queryCtx, credentialsA)
	if err != nil {
		return err
	}
	snapshotB, err := e.circuitOpenAccountSnapshot(queryCtx, credentialsB)
	if err != nil {
		return err
	}
	observedAt := time.Now().UTC()
	venueA, err := venueBasePosition(snapshotA, combination.LegA, instrumentA)
	if err != nil {
		return err
	}
	venueB, err := venueBasePosition(snapshotB, combination.LegB, instrumentB)
	if err != nil {
		return err
	}
	markA := e.circuitOpenMark(combination.LegA, snapshotA)
	markB := e.circuitOpenMark(combination.LegB, snapshotB)
	if !markA.IsPositive() || !markB.IsPositive() {
		e.logCircuitRecoverySkip(combination.ID, "valuation_incomplete")
		return nil
	}
	orderSumA, orderSumB, allTerminal := combinationTerminalOrderSums(orders)
	if !allTerminal {
		e.logCircuitRecoverySkip(combination.ID, "orders_unconfirmed")
		return nil
	}
	baselineA := parseDecimal(combination.LegAVenueBaselineBasePosition)
	baselineB := parseDecimal(combination.LegBVenueBaselineBasePosition)
	adjustmentA := parseDecimal(combination.LegAReconciliationAdjustment)
	adjustmentB := parseDecimal(combination.LegBReconciliationAdjustment)
	localA := orderSumA.Add(adjustmentA)
	localB := orderSumB.Add(adjustmentB)
	explained := venueA.Equal(baselineA.Add(orderSumA).Add(adjustmentA)) &&
		venueB.Equal(baselineB.Add(orderSumB).Add(adjustmentB))
	carry := localA.Add(localB)
	dust := circuitOpenCarryDust(carry, instrumentA, instrumentB, markA, markB)
	fillA, fillB := executionConfirmedLegFills(execution.ID, orders)
	if explained && dust {
		if !fillA.IsPositive() || !fillB.IsPositive() {
			e.logCircuitRecoverySkip(combination.ID, "zero_fill_not_completed",
				"execution_id", execution.ID)
			return nil
		}
		_, ok, closeErr := e.store.CompleteCircuitOpenRecovery(ctx, circuitOpenRecoveryCloseRequest{
			CombinationID: combination.ID, ExecutionID: execution.ID,
			ExpectedVersion: combination.Version, Kind: circuitCloseExplained,
			EventType: "execution_completed",
			VenueA:    venueA, VenueB: venueB, MarkA: markA, MarkB: markB,
			InstrumentA: instrumentA, InstrumentB: instrumentB,
			ObservedAt: observedAt, RecoveryStartedAt: started,
			OrderSumA: orderSumA, OrderSumB: orderSumB,
			LegAFilled: fillA.String(), LegBFilled: fillB.String(),
			Payload: map[string]any{
				"branch": "explained", "executionId": execution.ID,
				"carry": carry.String(),
			},
		})
		if closeErr != nil {
			return closeErr
		}
		if !ok {
			e.logCircuitRecoverySkip(combination.ID, "explained_close_rejected")
		}
		return nil
	}

	applied, ok, applyErr := e.store.ApplyCircuitOpenExternalReconcile(
		ctx, circuitOpenExternalReconcileRequest{
			CombinationID: combination.ID, ExecutionID: execution.ID,
			ExpectedVersion: combination.Version,
			VenueA:          venueA, VenueB: venueB, MarkA: markA, MarkB: markB,
			InstrumentA: instrumentA, InstrumentB: instrumentB,
		},
	)
	if applyErr != nil {
		return applyErr
	}
	if ok {
		if e.logger != nil {
			e.logger.Info("circuit open externally reconciled",
				"combination_id", applied.ID, "execution_id", execution.ID,
				"carry", applied.CarryBaseQuantity)
		}
		return nil
	}
	if !explained || !circuitOpenLegsNotSameDirection(localA, localB) {
		e.logCircuitRecoverySkip(combination.ID, "position_unexplained",
			"execution_id", execution.ID, "carry", carry.String())
		return nil
	}
	return e.replaceCircuitHedge(
		ctx, queryCtx, combination, execution, orders, carry, instrumentA, instrumentB,
		credentialsA, credentialsB, adapterA, adapterB,
	)
}

func (e *ArbitrageExecutor) replaceCircuitHedge(
	ctx, queryCtx context.Context,
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	orders []Order,
	carry decimal.Decimal,
	instrumentA, instrumentB Instrument,
	credentialsA, credentialsB Credentials,
	adapterA, adapterB exchange.Adapter,
) error {
	order, ok := hedgeRecoveryOrder(execution, orders)
	if !ok {
		e.logCircuitRecoverySkip(combination.ID, "hedge_not_recoverable",
			"execution_id", execution.ID, "hedge_order_id", execution.HedgeOrderID)
		return nil
	}
	if !strings.EqualFold(combination.Status, "running") || arbitrageFlattening(combination) {
		e.logCircuitRecoverySkip(combination.ID, "combination_not_running")
		return nil
	}
	leg := combination.LegA
	instrument := instrumentA
	credentials := credentialsA
	adapter := adapterA
	if strings.EqualFold(order.ArbitrageLeg, "b") {
		leg = combination.LegB
		instrument = instrumentB
		credentials = credentialsB
		adapter = adapterB
	}
	side := carryHedgeSide(carry)
	if e.market == nil {
		e.logCircuitRecoverySkip(combination.ID, "hedge_price_unavailable",
			"execution_id", execution.ID)
		return nil
	}
	bbo, bboErr := e.latestForLeg(leg)
	if bboErr != nil {
		e.logCircuitRecoverySkip(combination.ID, "hedge_price_stale",
			"execution_id", execution.ID, "error", bboErr.Error())
		return nil
	}
	price := parsePositiveDecimal(bbo.AskPrice)
	if side == "sell" {
		price = parsePositiveDecimal(bbo.BidPrice)
	}
	if !parseDecimal(combination.CarryBaseQuantity).Equal(carry) {
		e.logCircuitRecoverySkip(combination.ID, "carry_mismatch",
			"execution_id", execution.ID, "carry", carry.String(),
			"stored_carry", combination.CarryBaseQuantity)
		return nil
	}
	quantity, eligible, qtyErr := executableHedgeQuantity(carry.Abs(), price, instrument, false)
	if qtyErr != nil || !eligible {
		e.logCircuitRecoverySkip(combination.ID, "hedge_quantity_not_executable",
			"execution_id", execution.ID, "carry", carry.String())
		return nil
	}
	limitPrice := protectedIOCPrice(bbo, side, parsePositiveDecimal(instrument.PriceTick), e.iocBps)
	limitPrice, priceErr := normalizeHedgeIOCPrice(instrument, side, limitPrice)
	if priceErr != nil {
		return priceErr
	}
	preparedQty, preparedPrice, prepareErr := prepareArbitrageOrder(
		instrument, "limit", quantity.String(), limitPrice, limitPrice, execution,
	)
	if prepareErr != nil {
		e.logCircuitRecoverySkip(combination.ID, "hedge_prepare_rejected",
			"execution_id", execution.ID, "error", prepareErr.Error())
		return nil
	}
	_ = e.store.AppendArbitrageEvent(ctx, combination.ID, execution.ID, circuitHedgeRetryEligible, map[string]any{
		"branch": "hedge", "exchange": order.Exchange, "errorCode": order.ErrorCode,
		"carry": carry.String(), "leg": order.ArbitrageLeg, "side": side,
		"quantity": preparedQty,
	})
	intent := e.orderIntent(
		combination, execution, leg, instrument, side, "limit",
		preparedQty, preparedPrice, "hedge", int(execution.HedgeSequence),
	)
	if strings.TrimSpace(intent.ClientOrderID) == "" {
		intent.ClientOrderID = clientOrderID()
	}
	_ = e.store.AppendArbitrageEvent(ctx, combination.ID, execution.ID, circuitHedgeRetryStarted, map[string]any{
		"branch": "hedge", "exchange": order.Exchange, "errorCode": order.ErrorCode,
		"carry": carry.String(), "leg": intent.ArbitrageLeg, "side": side,
		"quantity": preparedQty, "clientOrderId": intent.ClientOrderID,
	})
	prepared, err := e.store.PrepareArbitrageHedgeIntent(ctx, PrepareArbitrageHedgeIntentInput{
		Combination:           combination,
		Execution:             execution,
		Order:                 intent,
		ExpectedSequence:      execution.HedgeSequence,
		HedgeLeg:              leg,
		HedgeSide:             side,
		TargetQuantity:        preparedQty,
		CarryQuantity:         carry.String(),
		FastPathAdmission:     false,
		AggregateCarry:        true,
		ExpectedCarryQuantity: combination.CarryBaseQuantity,
	})
	if err != nil {
		_ = e.store.AppendArbitrageEvent(ctx, combination.ID, execution.ID, circuitHedgeRetryRejected, map[string]any{
			"branch": "hedge", "error": err.Error(), "carry": carry.String(),
		})
		return nil
	}
	combination = prepared.Combination
	execution = prepared.Execution
	if !prepared.Created || e.service == nil {
		return nil
	}
	submitted, submitErr := e.service.submitPrepared(
		queryCtx, adapter, credentials, instrument, prepared.Order, "IOC", false,
	)
	if submitErr != nil || !parseDecimal(submitted.FilledQuantity).IsPositive() {
		_ = e.store.AppendArbitrageEvent(ctx, combination.ID, execution.ID, circuitHedgeRetryRejected, map[string]any{
			"branch": "hedge", "exchange": submitted.Exchange, "errorCode": submitted.ErrorCode,
			"carry": carry.String(), "status": submitted.Status,
		})
		return nil
	}
	return e.completeExecution(
		withCircuitRecoveryCompletion(ctx), combination, &execution,
		parsePositiveDecimal(preparedPrice),
	)
}

func (e *ArbitrageExecutor) recoverFailedCircuitQuote(
	ctx, queryCtx context.Context,
	combination ArbitrageCombination,
	failed ArbitrageExecution,
	orders []Order,
) error {
	order, ok := quoteRecoveryOrder(failed, orders)
	if !ok {
		e.logCircuitRecoverySkip(combination.ID, "quote_not_recoverable",
			"execution_id", failed.ID)
		return nil
	}
	if !e.circuitQuoteSignalOpen(combination, failed) {
		e.logCircuitRecoverySkip(combination.ID, "quote_signal_gone",
			"execution_id", failed.ID, "exchange", order.Exchange, "errorCode", order.ErrorCode)
		return nil
	}
	instrumentA, _, _, err := e.executionDependencies(queryCtx, combination.OwnerUsername, combination.LegA)
	if err != nil {
		return err
	}
	instrumentB, _, _, err := e.executionDependencies(queryCtx, combination.OwnerUsername, combination.LegB)
	if err != nil {
		return err
	}
	credentialsA, credentialsB, snapErr := e.circuitLegCredentials(combination)
	if snapErr != nil {
		return snapErr
	}
	snapshotA, err := e.circuitOpenAccountSnapshot(queryCtx, credentialsA)
	if err != nil {
		return err
	}
	snapshotB, err := e.circuitOpenAccountSnapshot(queryCtx, credentialsB)
	if err != nil {
		return err
	}
	venueA, err := venueBasePosition(snapshotA, combination.LegA, instrumentA)
	if err != nil {
		return err
	}
	venueB, err := venueBasePosition(snapshotB, combination.LegB, instrumentB)
	if err != nil {
		return err
	}
	orderSumA, orderSumB, allTerminal := combinationTerminalOrderSums(orders)
	if !allTerminal {
		e.logCircuitRecoverySkip(combination.ID, "orders_unconfirmed")
		return nil
	}
	baselineA := parseDecimal(combination.LegAVenueBaselineBasePosition)
	baselineB := parseDecimal(combination.LegBVenueBaselineBasePosition)
	adjustmentA := parseDecimal(combination.LegAReconciliationAdjustment)
	adjustmentB := parseDecimal(combination.LegBReconciliationAdjustment)
	if !venueA.Equal(baselineA.Add(orderSumA).Add(adjustmentA)) ||
		!venueB.Equal(baselineB.Add(orderSumB).Add(adjustmentB)) {
		e.logCircuitRecoverySkip(combination.ID, "quote_position_unexplained",
			"execution_id", failed.ID)
		return nil
	}
	localA := orderSumA.Add(adjustmentA)
	localB := orderSumB.Add(adjustmentB)
	if !circuitOpenLegsNotSameDirection(localA, localB) || !localA.Add(localB).IsZero() {
		e.logCircuitRecoverySkip(combination.ID, "quote_exposure",
			"execution_id", failed.ID, "carry", localA.Add(localB).String())
		return nil
	}
	bboA, bboB := e.circuitRecoveryBBO(combination)
	execution := e.circuitRecoveryQuoteExecution(combination, failed, bboA, bboB)
	if execution.ID == "" {
		e.logCircuitRecoverySkip(combination.ID, "quote_size_rejected",
			"execution_id", failed.ID)
		return nil
	}
	_ = e.store.AppendArbitrageEvent(ctx, combination.ID, failed.ID, circuitQuoteRetryEligible, map[string]any{
		"branch": "quote", "exchange": order.Exchange, "errorCode": order.ErrorCode,
		"carry": "0", "direction": execution.Direction,
		"quantity": execution.TargetBaseQuantity,
	})
	claimed, ok, claimErr := e.store.ClaimCircuitRecoveryExecution(ctx, ClaimCircuitRecoveryExecutionInput{
		ExpectedVersion: combination.Version, FailedExecutionID: failed.ID, Execution: execution,
	})
	if claimErr != nil {
		return claimErr
	}
	if !ok {
		_ = e.store.AppendArbitrageEvent(ctx, combination.ID, failed.ID, circuitQuoteRetryRejected, map[string]any{
			"branch": "quote", "exchange": order.Exchange, "errorCode": order.ErrorCode,
			"reason": "claim_rejected",
		})
		return nil
	}
	if refreshed, refreshErr := e.store.GetArbitrageCombinationByOwner(
		ctx, combination.OwnerUsername, combination.ID,
	); refreshErr == nil && refreshed.ID != "" {
		combination = refreshed
	}
	_ = e.store.AppendArbitrageEvent(ctx, combination.ID, claimed.ID, circuitQuoteRetryStarted, map[string]any{
		"branch": "quote", "exchange": order.Exchange, "errorCode": order.ErrorCode,
		"direction": claimed.Direction, "quantity": claimed.TargetBaseQuantity,
		"failedExecutionId": failed.ID,
	})
	err = e.executeMakerThenHedge(
		withCircuitRecoveryCompletion(ctx), combination, &claimed, bboA, bboB, nil,
	)
	if err != nil {
		_ = e.store.AppendArbitrageEvent(ctx, combination.ID, claimed.ID, circuitQuoteRetryRejected, map[string]any{
			"branch": "quote", "error": err.Error(),
		})
	}
	return nil
}

func (e *ArbitrageExecutor) circuitLegCredentials(
	combination ArbitrageCombination,
) (Credentials, Credentials, error) {
	credentialsA, err := e.credentials.GetInternal(
		context.Background(), e.token, combination.OwnerUsername, combination.LegA.TradingAccountID,
	)
	if err != nil {
		return Credentials{}, Credentials{}, err
	}
	if credentialsA.Exchange == "" {
		credentialsA.Exchange = combination.LegA.Exchange
	}
	credentialsB, err := e.credentials.GetInternal(
		context.Background(), e.token, combination.OwnerUsername, combination.LegB.TradingAccountID,
	)
	if err != nil {
		return Credentials{}, Credentials{}, err
	}
	if credentialsB.Exchange == "" {
		credentialsB.Exchange = combination.LegB.Exchange
	}
	return credentialsA, credentialsB, nil
}

func (e *ArbitrageExecutor) circuitRecoveryBBO(
	combination ArbitrageCombination,
) (marketdata.BBO, marketdata.BBO) {
	now := time.Now().UTC()
	bboA := marketdata.BBO{
		BidPrice: combination.CurrentBidSpreadBps, AskPrice: "100",
		ReceiveTimestamp: now, VenueTimestamp: now,
	}
	bboB := bboA
	if e.market == nil {
		bboA.BidPrice, bboA.AskPrice = "100", "100.1"
		bboB = bboA
		return bboA, bboB
	}
	if latest, err := e.latestForLeg(combination.LegA); err == nil {
		bboA = latest
	} else {
		bboA.BidPrice, bboA.AskPrice = "100", "100.1"
	}
	if latest, err := e.latestForLeg(combination.LegB); err == nil {
		bboB = latest
	} else {
		bboB.BidPrice, bboB.AskPrice = "100", "100.1"
	}
	return bboA, bboB
}

func (e *ArbitrageExecutor) circuitQuoteSignalOpen(
	combination ArbitrageCombination,
	failed ArbitrageExecution,
) bool {
	if strings.EqualFold(combination.Status, "closing") || arbitrageFlattening(combination) {
		return false
	}
	phase := strings.ToLower(strings.TrimSpace(combination.OneShotPhase))
	if phase == "waiting_exit" || phase == "exiting" || phase == "exited" {
		return false
	}
	target := parsePositiveDecimal(combination.TargetNotional)
	if !target.IsPositive() {
		return false
	}
	bboA, bboB := e.circuitRecoveryBBO(combination)
	ask := parseDecimal(combination.CurrentAskSpreadBps)
	bid := parseDecimal(combination.CurrentBidSpreadBps)
	if e.market != nil {
		if computedAsk, computedBid, ok := executableArbitrageSpreads(
			combination,
			parsePositiveDecimal(bboA.BidPrice), parsePositiveDecimal(bboA.AskPrice),
			parsePositiveDecimal(bboB.BidPrice), parsePositiveDecimal(bboB.AskPrice),
		); ok {
			ask, bid = computedAsk, computedBid
		}
	}
	askThreshold := parseDecimal(combination.AskThresholdBps)
	bidThreshold := parseDecimal(combination.BidThresholdBps)
	midA := parsePositiveDecimal(bboA.BidPrice).Add(parsePositiveDecimal(bboA.AskPrice)).Div(decimal.NewFromInt(2))
	midB := parsePositiveDecimal(bboB.BidPrice).Add(parsePositiveDecimal(bboB.AskPrice)).Div(decimal.NewFromInt(2))
	if !midA.IsPositive() {
		midA = decimal.NewFromInt(100)
	}
	if !midB.IsPositive() {
		midB = midA
	}
	venueNotional, _, _ := arbitragePositionNotionals(combination, midA, midB)
	var direction string
	if strings.EqualFold(combination.RunMode, "one_shot") {
		direction = combination.EntryDirection
	} else {
		direction = selectArbitragePositionDirection(
			ask, bid, askThreshold, bidThreshold, venueNotional, target,
		)
	}
	if !strings.EqualFold(direction, failed.Direction) || !strings.EqualFold(direction, "ask") {
		return false
	}
	remaining := remainingDirectionNotional(
		parseDecimal(combination.LegABasePosition), parseDecimal(combination.LegBBasePosition),
		target, midA, midB,
	)
	return !remaining.LessThan(decimal.NewFromInt(arbitrageMinOpenNotional))
}

func (e *ArbitrageExecutor) circuitRecoveryQuoteExecution(
	combination ArbitrageCombination,
	failed ArbitrageExecution,
	bboA, bboB marketdata.BBO,
) ArbitrageExecution {
	target := parsePositiveDecimal(combination.TargetNotional)
	orderNotional := parsePositiveDecimal(combination.OrderNotional)
	if !orderNotional.IsPositive() {
		orderNotional = decimal.NewFromInt(arbitrageMinOpenNotional)
	}
	midA := parsePositiveDecimal(bboA.BidPrice).Add(parsePositiveDecimal(bboA.AskPrice)).Div(decimal.NewFromInt(2))
	midB := parsePositiveDecimal(bboB.BidPrice).Add(parsePositiveDecimal(bboB.AskPrice)).Div(decimal.NewFromInt(2))
	if !midA.IsPositive() {
		midA = decimal.NewFromInt(100)
	}
	if !midB.IsPositive() {
		midB = midA
	}
	venueNotional, _, _ := arbitragePositionNotionals(combination, midA, midB)
	plan, ok := planArbitragePosition(venueNotional, target, orderNotional, failed.Direction)
	if !ok || !strings.EqualFold(plan.Effect, "open") {
		return ArbitrageExecution{}
	}
	price := parsePositiveDecimal(bboA.AskPrice)
	if !price.IsPositive() {
		price = midA
	}
	qty := plan.RequestedNotional.Div(price)
	if !qty.IsPositive() {
		return ArbitrageExecution{}
	}
	return ArbitrageExecution{
		ID: newCircuitRecoveryID(), CombinationID: combination.ID,
		Direction: failed.Direction, Status: "claimed", PositionEffect: "open",
		TriggerAskSpread: combination.CurrentAskSpreadBps,
		TriggerBidSpread: combination.CurrentBidSpreadBps,
		TriggerLegABid:   bboA.BidPrice, TriggerLegAAsk: bboA.AskPrice,
		TriggerLegBBid: bboB.BidPrice, TriggerLegBAsk: bboB.AskPrice,
		TargetBaseQuantity: qty.String(), RequestedNotional: plan.RequestedNotional.String(),
		LegAFilledQuantity: "0", LegBFilledQuantity: "0", DeltaNotional: "0",
	}
}

func newCircuitRecoveryID() string {
	return uuid.NewString()
}

func (e *ArbitrageExecutor) finishCircuitRecoveryExecution(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	mark decimal.Decimal,
) error {
	if e == nil || e.store == nil || execution == nil {
		return fmt.Errorf("%w: circuit recovery completion unavailable", ErrInvalidArgument)
	}
	started := time.Now().UTC()
	instrumentA, credentialsA, _, err := e.executionDependencies(
		ctx, combination.OwnerUsername, combination.LegA,
	)
	if err != nil {
		return err
	}
	if credentialsA.Exchange == "" {
		credentialsA.Exchange = combination.LegA.Exchange
	}
	instrumentB, credentialsB, _, err := e.executionDependencies(
		ctx, combination.OwnerUsername, combination.LegB,
	)
	if err != nil {
		return err
	}
	if credentialsB.Exchange == "" {
		credentialsB.Exchange = combination.LegB.Exchange
	}
	snapshotA, err := e.circuitOpenAccountSnapshot(ctx, credentialsA)
	if err != nil {
		return err
	}
	snapshotB, err := e.circuitOpenAccountSnapshot(ctx, credentialsB)
	if err != nil {
		return err
	}
	observedAt := time.Now().UTC()
	venueA, err := venueBasePosition(snapshotA, combination.LegA, instrumentA)
	if err != nil {
		return err
	}
	venueB, err := venueBasePosition(snapshotB, combination.LegB, instrumentB)
	if err != nil {
		return err
	}
	orders, err := e.store.ListArbitrageOrders(ctx, combination.OwnerUsername, combination.ID)
	if err != nil {
		return err
	}
	orderSumA, orderSumB, allTerminal := combinationTerminalOrderSums(orders)
	if !allTerminal {
		return fmt.Errorf("%w: circuit recovery orders are not terminal", ErrArbitrageReconciling)
	}
	markA := e.circuitOpenMark(combination.LegA, snapshotA)
	markB := e.circuitOpenMark(combination.LegB, snapshotB)
	if !markA.IsPositive() {
		markA = mark
	}
	if !markB.IsPositive() {
		markB = mark
	}
	eventType := circuitHedgeRetryCompleted
	if strings.TrimSpace(execution.MakerOrderID) != "" && strings.TrimSpace(execution.HedgeOrderID) != "" {
		eventType = circuitQuoteRetryCompleted
	}
	updated, ok, closeErr := e.store.CompleteCircuitOpenRecovery(ctx, circuitOpenRecoveryCloseRequest{
		CombinationID: combination.ID, ExecutionID: execution.ID,
		ExpectedVersion: combination.Version, Kind: circuitCloseFilled,
		EventType: eventType,
		VenueA:    venueA, VenueB: venueB, MarkA: markA, MarkB: markB,
		InstrumentA: instrumentA, InstrumentB: instrumentB,
		ObservedAt: observedAt, RecoveryStartedAt: started,
		OrderSumA: orderSumA, OrderSumB: orderSumB,
		LegAFilled: execution.LegAFilledQuantity, LegBFilled: execution.LegBFilledQuantity,
		DeltaNotional: execution.DeltaNotional,
		TurnoverDelta: parseDecimal(execution.DeltaNotional).Abs().String(),
		Payload: map[string]any{
			"branch": "filled", "executionId": execution.ID,
			"legAFilledQuantity": execution.LegAFilledQuantity,
			"legBFilledQuantity": execution.LegBFilledQuantity,
			"deltaNotional":      execution.DeltaNotional,
		},
	})
	if closeErr != nil {
		return closeErr
	}
	if !ok {
		return fmt.Errorf("%w: circuit recovery close rejected", ErrInvalidArgument)
	}
	execution.Status = "completed"
	execution.ErrorMessage = ""
	combination = updated
	return nil
}
