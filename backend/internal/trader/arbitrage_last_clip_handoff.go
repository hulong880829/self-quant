package trader

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
)

const (
	lastClipHedgeMinNotionalFailureKey = "last_clip_hedge_min_notional"
	lastClipDustHandoffEvent           = "last_close_clip_dust_handoff"
)

var errLastClipDustHandoff = errors.New("last clip dust handoff")

func lastClipMinNotionalMessage(message string) bool {
	lower := strings.ToLower(message)
	for _, needle := range []string{
		"minimum value",
		"minimum notional",
		"below venue minimum",
		"minimum quantity",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func lastClipHedgeMinNotionalError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrVenueUncertain) || errors.Is(err, ErrMarketDataStale) {
		return false
	}
	if errors.Is(err, ErrOrderBelowMinimum) {
		return true
	}
	if errors.Is(err, exchange.ErrInvalidQuantity) && lastClipMinNotionalMessage(err.Error()) {
		return true
	}
	return lastClipMinNotionalMessage(sanitizeError(err))
}

func lastClipExecutionOrders(orders []Order, executionID string) []Order {
	matched := make([]Order, 0, len(orders))
	for _, order := range orders {
		if strings.TrimSpace(order.ArbitrageExecutionID) != strings.TrimSpace(executionID) {
			continue
		}
		matched = append(matched, order)
	}
	return matched
}

func lastClipRoleOrders(orders []Order, role string) []Order {
	matched := make([]Order, 0, len(orders))
	for _, order := range orders {
		if !strings.EqualFold(strings.TrimSpace(order.ArbitrageRole), role) {
			continue
		}
		matched = append(matched, order)
	}
	return matched
}

func lastClipAggregateFilled(orders []Order) decimal.Decimal {
	total := decimal.Zero
	for _, order := range orders {
		total = total.Add(parseDecimal(order.FilledQuantity))
	}
	return total
}

func lastClipLatestOrder(orders []Order) Order {
	var latest Order
	for _, order := range orders {
		if latest.ID == "" {
			latest = order
			continue
		}
		if order.UpdatedAt.After(latest.UpdatedAt) {
			latest = order
			continue
		}
		if order.UpdatedAt.Equal(latest.UpdatedAt) && order.ID > latest.ID {
			latest = order
		}
	}
	return latest
}

func lastClipHedgeOrdersReady(hedges []Order) bool {
	if len(hedges) == 0 {
		return false
	}
	for _, order := range hedges {
		status := strings.ToLower(strings.TrimSpace(order.Status))
		if status == "unknown" || !terminalStatus(order.Status) {
			return false
		}
		if strings.EqualFold(status, "partially_filled") {
			return false
		}
	}
	return strings.EqualFold(strings.TrimSpace(lastClipLatestOrder(hedges).Status), "rejected")
}

func lastClipMakerFillConfirmed(makers []Order) bool {
	if len(makers) == 0 {
		return false
	}
	for _, order := range makers {
		if !terminalStatus(order.Status) {
			return false
		}
	}
	return lastClipAggregateFilled(makers).IsPositive()
}

func lastClipHedgeMinNotionalHandoffReady(
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	orders []Order,
) bool {
	if !strings.EqualFold(strings.TrimSpace(combination.Status), "closing") {
		return false
	}
	if combination.PositionUncertain {
		return false
	}
	if !execution.LastCloseClip || !execution.ReduceOnly ||
		!strings.EqualFold(strings.TrimSpace(execution.PositionEffect), "close") {
		return false
	}
	if terminalArbitrageExecutionStatus(execution.Status) {
		return false
	}
	matched := lastClipExecutionOrders(orders, execution.ID)
	makers := lastClipRoleOrders(matched, "maker")
	hedges := lastClipRoleOrders(matched, "hedge")
	if !lastClipMakerFillConfirmed(makers) || !lastClipHedgeOrdersReady(hedges) {
		return false
	}
	return lastClipAggregateFilled(makers).Sub(lastClipAggregateFilled(hedges)).IsPositive()
}

func lastClipHedgeRejectCause(err error, execution ArbitrageExecution, hedges []Order) error {
	if lastClipHedgeMinNotionalError(err) {
		return err
	}
	if msg := strings.TrimSpace(execution.ErrorMessage); msg != "" && lastClipMinNotionalMessage(msg) {
		return errors.New(msg)
	}
	latest := lastClipLatestOrder(hedges)
	message := strings.TrimSpace(strings.Join(
		[]string{latest.ErrorCode, latest.ErrorMessage},
		" ",
	))
	if lastClipMinNotionalMessage(message) {
		return errors.New(message)
	}
	return nil
}

func lastClipHedgeMinNotionalRejected(
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	err error,
	orders []Order,
) bool {
	if !lastClipHedgeMinNotionalHandoffReady(combination, execution, orders) {
		return false
	}
	if combination.LastFailureKey == lastClipHedgeMinNotionalFailureKey {
		return true
	}
	hedges := lastClipRoleOrders(lastClipExecutionOrders(orders, execution.ID), "hedge")
	return lastClipHedgeRejectCause(err, execution, hedges) != nil
}

func lastClipHedgeMinNotionalTinyClose(item ArbitrageCombination) bool {
	return strings.EqualFold(strings.TrimSpace(item.Status), "closing") &&
		item.LastFailureKey == lastClipHedgeMinNotionalFailureKey &&
		!item.PositionUncertain
}

func (e *ArbitrageExecutor) maybeHandoffLastClipHedgeMinNotional(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	cause error,
) error {
	if execution == nil || !strings.EqualFold(strings.TrimSpace(combination.Status), "closing") ||
		!execution.LastCloseClip {
		return nil
	}
	if terminalArbitrageExecutionStatus(execution.Status) {
		if combination.LastFailureKey == lastClipHedgeMinNotionalFailureKey {
			return errLastClipDustHandoff
		}
		return nil
	}
	orders, err := e.store.ListArbitrageOrders(ctx, combination.OwnerUsername, combination.ID)
	if err != nil {
		return nil
	}
	if !lastClipHedgeMinNotionalRejected(combination, *execution, cause, orders) {
		return nil
	}
	return e.completeLastClipDustHandoff(ctx, combination, execution, cause, orders)
}

func (e *ArbitrageExecutor) completeLastClipDustHandoff(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	cause error,
	orders []Order,
) error {
	if _, err := e.store.RecomputeArbitrageBasePositionsForExecution(ctx, execution.ID); err != nil {
		return err
	}
	hedges := lastClipRoleOrders(lastClipExecutionOrders(orders, execution.ID), "hedge")
	message := sanitizeError(cause)
	if message == "" {
		if reject := lastClipHedgeRejectCause(cause, *execution, hedges); reject != nil {
			message = sanitizeError(reject)
		}
	}
	if combination.LastFailureKey != lastClipHedgeMinNotionalFailureKey {
		failures, ok := e.store.(arbitrageCloseFailureStore)
		if !ok {
			return cause
		}
		updated, err := failures.RecordArbitrageCloseFailure(
			ctx, combination.ID, lastClipHedgeMinNotionalFailureKey, message,
		)
		if err != nil {
			return err
		}
		combination = updated
	}
	failed := *execution
	failed.Status = "failed"
	failed.ErrorMessage = message
	if _, err := e.store.UpdateArbitrageExecution(ctx, failed); err != nil {
		return errLastClipDustHandoff
	}
	*execution = failed
	_ = e.store.AppendArbitrageEvent(
		ctx, combination.ID, execution.ID, lastClipDustHandoffEvent,
		map[string]any{
			"failureKey": lastClipHedgeMinNotionalFailureKey,
			"message":    message,
			"handoffAt":  time.Now().UTC().Format(time.RFC3339Nano),
		},
	)
	return errLastClipDustHandoff
}
