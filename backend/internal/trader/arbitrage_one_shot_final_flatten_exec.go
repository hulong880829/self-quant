package trader

import (
	"context"
	"errors"
	"strings"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
	"selfquant/backend/internal/trader/marketdata"
)

func (e *ArbitrageExecutor) executeOneShotFinalFlatten(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	bboA, bboB marketdata.BBO,
) error {
	store, ok := e.store.(arbitrageOneShotStore)
	if !ok {
		return ErrInvalidArgument
	}
	instrumentA, credentialsA, adapterA, err := e.executionDependencies(
		ctx, combination.OwnerUsername, combination.LegA,
	)
	if err != nil {
		return err
	}
	instrumentB, credentialsB, adapterB, err := e.executionDependencies(
		ctx, combination.OwnerUsername, combination.LegB,
	)
	if err != nil {
		return err
	}
	refreshed, refreshErr := e.refreshArbitragePositions(ctx, combination, execution)
	if refreshErr != nil {
		return refreshErr
	}
	combination = refreshed
	_ = e.store.AppendArbitrageEvent(
		ctx, combination.ID, execution.ID, oneShotFinalFlattenStartedEvent,
		map[string]any{
			"legABasePosition": combination.LegABasePosition,
			"legBBasePosition": combination.LegBBasePosition,
		},
	)
	planned := planOwnedFlattenLegs(combination, instrumentA, instrumentB)
	existing, listErr := e.store.ListArbitrageOrders(
		ctx, combination.OwnerUsername, combination.ID,
	)
	if listErr != nil {
		return listErr
	}
	byLeg := map[string]Order{}
	for _, order := range executionOrdersFor(existing, execution.ID) {
		if !strings.Contains(order.IdempotencyKey, ":"+oneShotFinalFlattenIdempotencyRole+":") {
			continue
		}
		byLeg[strings.ToLower(strings.TrimSpace(order.ArbitrageLeg))] = order
	}
	toCreate := make([]Order, 0, 2)
	for _, leg := range planned {
		if _, found := byLeg[leg.LegName]; found {
			continue
		}
		instrument := instrumentA
		if leg.LegName == "b" {
			instrument = instrumentB
		}
		if _, ok := closeFlattenPlaceRequest(instrument, signedFlattenBase(leg)); !ok {
			continue
		}
		toCreate = append(toCreate, oneShotFinalFlattenOrderIntent(
			combination, *execution, leg, instrument,
		))
	}
	if len(toCreate) > 0 {
		if e.orders == nil {
			return ErrInvalidArgument
		}
		created, _, createErr := e.orders.CreateArbitrageIntents(ctx, toCreate)
		if createErr != nil {
			return createErr
		}
		for _, order := range created {
			byLeg[strings.ToLower(strings.TrimSpace(order.ArbitrageLeg))] = order
		}
	}
	if len(byLeg) == 0 {
		return e.finishOneShotFinalFlatten(
			ctx, store, combination, execution, instrumentA, instrumentB, nil, false,
		)
	}
	if order, ok := byLeg["a"]; ok {
		execution.MakerOrderID = order.ID
	}
	if order, ok := byLeg["b"]; ok {
		execution.HedgeOrderID = order.ID
	}
	execution.Status = "hedging"
	execution.ErrorMessage = ""
	if _, err := e.store.UpdateArbitrageExecution(ctx, *execution); err != nil {
		return err
	}
	combination.RuntimeState = "hedging"
	combination.ErrorMessage = ""
	if updated, runtimeErr := e.store.UpdateArbitrageCombinationRuntime(ctx, combination); runtimeErr == nil {
		combination = updated
	}

	ordersA := e.subscribeOrders(ctx, credentialsA, instrumentA)
	if ordersA != nil {
		defer ordersA.Close()
	}
	ordersB := e.subscribeOrders(ctx, credentialsB, instrumentB)
	if ordersB != nil {
		defer ordersB.Close()
	}

	type flattenSubmit struct {
		leg         string
		instrument  Instrument
		credentials Credentials
		adapter     exchange.Adapter
		order       Order
	}
	jobs := make([]flattenSubmit, 0, 2)
	if order, ok := byLeg["a"]; ok {
		jobs = append(jobs, flattenSubmit{
			leg: "a", instrument: instrumentA, credentials: credentialsA,
			adapter: adapterA, order: order,
		})
	}
	if order, ok := byLeg["b"]; ok {
		jobs = append(jobs, flattenSubmit{
			leg: "b", instrument: instrumentB, credentials: credentialsB,
			adapter: adapterB, order: order,
		})
	}

	uncertain := false
	unexecutable := false
	var firstErr error
	for i := range jobs {
		job := &jobs[i]
		if terminalStatus(job.order.Status) {
			continue
		}
		created := job.order.Status == "pending" && job.order.VenueOrderID == ""
		var submitErr error
		if e.service == nil {
			submitErr = ErrInvalidArgument
		} else {
			lock := e.service.accountLock(job.credentials.TradingAccountID)
			lock.Lock()
			if created {
				job.order, submitErr = e.service.submit(
					ctx, job.adapter, job.credentials, job.instrument, job.order,
				)
			} else {
				job.order, submitErr = e.service.recover(
					ctx, job.adapter, job.credentials, job.instrument, job.order,
				)
			}
			lock.Unlock()
		}
		_ = e.store.AppendArbitrageEvent(
			ctx, combination.ID, execution.ID, oneShotFinalFlattenLegSubmittedEvent,
			map[string]any{
				"leg": job.leg, "venue": job.instrument.Exchange,
				"side": job.order.Side, "quantity": job.order.Quantity,
				"reduceOnly":      job.order.ReduceOnly,
				"exchangeOrderId": job.order.VenueOrderID,
			},
		)
		if submitErr != nil {
			if firstErr == nil {
				firstErr = submitErr
			}
			if errors.Is(submitErr, ErrVenueUncertain) {
				uncertain = true
			}
			if oneShotFinalFlattenUnexecutable(submitErr) {
				unexecutable = true
			}
		}
	}
	for i := range jobs {
		job := &jobs[i]
		watch := e.watchOrder(ctx, nil, job.adapter, job.order)
		if job.leg == "a" {
			watch = e.watchOrder(ctx, ordersA, job.adapter, job.order)
		} else {
			watch = e.watchOrder(ctx, ordersB, job.adapter, job.order)
		}
		sub := ordersA
		if job.leg == "b" {
			sub = ordersB
		}
		observed, observeErr := e.observeToTerminal(
			ctx, job.credentials, job.instrument, job.adapter, job.order,
			orderObservation{subscription: sub, watch: watch},
		)
		job.order = observed
		_ = e.store.AppendArbitrageEvent(
			ctx, combination.ID, execution.ID, oneShotFinalFlattenLegTerminalEvent,
			map[string]any{
				"leg": job.leg, "venue": job.instrument.Exchange,
				"side": job.order.Side, "quantity": job.order.Quantity,
				"status": job.order.Status, "filledQuantity": job.order.FilledQuantity,
				"exchangeOrderId": job.order.VenueOrderID,
				"error":           job.order.ErrorMessage,
			},
		)
		if observeErr != nil || !terminalStatus(job.order.Status) {
			uncertain = true
			if firstErr == nil {
				firstErr = observeErr
				if firstErr == nil {
					firstErr = ErrVenueUncertain
				}
			}
		}
		if oneShotFinalFlattenUnexecutable(observeErr) ||
			oneShotFinalFlattenUnexecutable(errors.New(job.order.ErrorMessage)) {
			unexecutable = true
		}
	}
	if uncertain {
		execution.Status = "reconciling"
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
		combination.PositionUncertain = true
		combination.RuntimeState = "position_uncertain"
		combination.ErrorMessage = sanitizeError(firstErr)
		_, _ = e.store.UpdateArbitrageCombinationRuntime(ctx, combination)
		return ErrArbitrageReconciling
	}
	return e.finishOneShotFinalFlatten(
		ctx, store, combination, execution, instrumentA, instrumentB, firstErr, unexecutable,
	)
}

func signedFlattenBase(leg OwnedFlattenLeg) decimal.Decimal {
	if strings.EqualFold(leg.Side, "buy") {
		return leg.BaseQuantity.Neg()
	}
	return leg.BaseQuantity
}

func (e *ArbitrageExecutor) finishOneShotFinalFlatten(
	ctx context.Context,
	store arbitrageOneShotStore,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	instrumentA, instrumentB Instrument,
	submitErr error,
	unexecutable bool,
) error {
	result, err := store.FinalizeOneShotFinalFlattenExecution(ctx, execution.ID)
	if errors.Is(err, ErrArbitrageReconciling) {
		execution.Status = "reconciling"
		_, _ = e.store.UpdateArbitrageExecution(ctx, *execution)
		combination.PositionUncertain = true
		combination.RuntimeState = "position_uncertain"
		combination.ErrorMessage = sanitizeError(err)
		_, _ = e.store.UpdateArbitrageCombinationRuntime(ctx, combination)
		return ErrArbitrageReconciling
	}
	if err != nil {
		return err
	}
	combination = result.Combination
	*execution = result.Execution
	orders, listErr := e.store.ListArbitrageOrders(
		ctx, combination.OwnerUsername, combination.ID,
	)
	if listErr != nil {
		return listErr
	}
	if combinationLooksFlat(combination) &&
		oneShotFinalFlattenReconcileReady(combination, orders, execution.ID, instrumentA, instrumentB) {
		updated, applied, markErr := store.MarkOneShotExited(
			ctx, combination.ID, combination.Version,
		)
		if markErr != nil {
			return markErr
		}
		if applied {
			combination = updated
			_ = e.store.AppendArbitrageEvent(
				ctx, combination.ID, execution.ID, oneShotFinalFlattenCompletedEvent,
				map[string]any{
					"legABasePosition": combination.LegABasePosition,
					"legBBasePosition": combination.LegBBasePosition,
				},
			)
		}
		return nil
	}
	if oneShotFinalFlattenShouldDust(
		combination, instrumentA, instrumentB, orders, execution.ID, unexecutable,
	) {
		_ = e.store.AppendArbitrageEvent(
			ctx, combination.ID, execution.ID, oneShotFinalFlattenUnexecutableEvent,
			map[string]any{
				"legABasePosition": combination.LegABasePosition,
				"legBBasePosition": combination.LegBBasePosition,
				"error":            sanitizeError(submitErr),
			},
		)
		updated, applied, dustErr := store.MarkOneShotExitedWithDust(
			ctx, combination.ID, combination.Version,
			combination.LegABasePosition, combination.LegBBasePosition,
			combination.CarryBaseQuantity,
		)
		if dustErr != nil {
			return dustErr
		}
		if applied {
			combination = updated
		}
		return nil
	}
	if parseDecimal(combination.LegABasePosition).IsZero() &&
		parseDecimal(combination.LegBBasePosition).IsZero() {
		_ = e.store.AppendArbitrageEvent(
			ctx, combination.ID, execution.ID, oneShotFinalFlattenRetryEvent,
			map[string]any{"reason": "waiting_position_reconcile"},
		)
		return nil
	}
	_ = e.store.AppendArbitrageEvent(
		ctx, combination.ID, execution.ID, oneShotFinalFlattenRetryEvent,
		map[string]any{
			"legABasePosition": combination.LegABasePosition,
			"legBBasePosition": combination.LegBBasePosition,
		},
	)
	return nil
}

func oneShotFinalFlattenShouldDust(
	combination ArbitrageCombination,
	instrumentA, instrumentB Instrument,
	orders []Order,
	executionID string,
	unexecutable bool,
) bool {
	if !oneShotRunningExiting(combination) || combination.PositionUncertain {
		return false
	}
	ownedA := parseDecimal(combination.LegABasePosition)
	ownedB := parseDecimal(combination.LegBBasePosition)
	if ownedA.IsZero() && ownedB.IsZero() {
		return false
	}
	latest := oneShotFinalFlattenLatestOrderAt(orders, executionID)
	if latest.IsZero() || combination.LastPositionReconciledAt.Before(latest) {
		return false
	}
	steppedA := closeFlattenOwnedPlaceBase(combination, instrumentA, "a")
	steppedB := closeFlattenOwnedPlaceBase(combination, instrumentB, "b")
	if !steppedA.IsZero() || !steppedB.IsZero() {
		return unexecutable
	}
	return true
}

func (e *ArbitrageExecutor) recoverOneShotFinalFlatten(
	ctx context.Context,
	combination ArbitrageCombination,
	execution ArbitrageExecution,
) error {
	return e.executeOneShotFinalFlatten(
		ctx, combination, &execution, marketdata.BBO{}, marketdata.BBO{},
	)
}
