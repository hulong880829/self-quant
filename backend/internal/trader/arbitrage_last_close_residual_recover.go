package trader

import (
	"context"
	"errors"
	"strings"

	"github.com/shopspring/decimal"
)

type lastCloseResidualRecoveredRecorder interface {
	RecordLastCloseResidualRecovered(
		context.Context, string, string, string, string, string,
	) error
}

func (e *ArbitrageExecutor) recoverLastCloseClipClosing(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
) (bool, error) {
	if execution == nil || !lastCloseClipClosingEligible(combination, *execution) {
		return false, nil
	}
	locked := lastCloseClipLockedResumeEligible(combination, *execution)
	if terminalArbitrageExecutionStatus(execution.Status) {
		return true, nil
	}
	orders, err := e.store.ListArbitrageOrders(
		ctx, combination.OwnerUsername, combination.ID,
	)
	if err != nil {
		return true, err
	}
	execOrders := lastCloseClipExecutionOrders(orders, execution.ID)
	if locked {
		return true, e.finishLockedLastCloseClip(ctx, combination, execution, execOrders)
	}
	if len(execOrders) == 0 {
		return false, nil
	}
	if lastCloseClipOrdersBlocked(execOrders) {
		return true, e.parkLastCloseClipReconciling(ctx, combination, execution)
	}
	alloc := allocateLastCloseClipResidualFills(execOrders, combination, *execution)
	switch {
	case alloc.matched():
		mark := lastCloseRecoveryMark(execOrders, *execution)
		if !mark.IsPositive() {
			return true, e.parkLastCloseClipReconciling(ctx, combination, execution)
		}
		execution.LegAFilledQuantity = alloc.FillA.String()
		execution.LegBFilledQuantity = alloc.FillB.String()
		if completeErr := e.completeExecution(ctx, combination, execution, mark); completeErr != nil {
			if errors.Is(completeErr, ErrArbitrageExecutionTerminal) {
				e.recordLastCloseResidualRecovered(ctx, combination.ID, execution.ID, alloc)
				return true, nil
			}
			return true, completeErr
		}
		e.recordLastCloseResidualRecovered(ctx, combination.ID, execution.ID, alloc)
		return true, nil
	case alloc.residualShort():
		remaining := alloc.MakerFill.Sub(alloc.EffectiveHedgeFill).Abs()
		result, failErr := e.store.FailLastCloseClipUnbalanced(
			ctx, combination, *execution,
			alloc.FillA.String(), alloc.FillB.String(), remaining.String(),
		)
		if failErr != nil {
			return true, failErr
		}
		*execution = result.Execution
		return true, nil
	default:
		return false, nil
	}
}

func (e *ArbitrageExecutor) finishLockedLastCloseClip(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	execOrders []Order,
) error {
	if len(execOrders) == 0 || lastCloseClipOrdersBlocked(execOrders) {
		e.recordLastCloseRecoveryBlocked(
			ctx, combination.ID, execution.ID, lastCloseRecoveryOrderNotTerminalEvent,
		)
		return nil
	}
	if !lastCloseClipOrdersAllReduceOnly(execOrders) {
		e.recordLastCloseRecoveryBlocked(
			ctx, combination.ID, execution.ID, lastCloseRecoveryFillMismatchEvent,
		)
		return nil
	}
	alloc := allocateLastCloseClipResidualFills(execOrders, combination, *execution)
	if !alloc.matched() {
		e.recordLastCloseRecoveryBlocked(
			ctx, combination.ID, execution.ID, lastCloseRecoveryFillMismatchEvent,
		)
		return nil
	}
	mark := lastCloseRecoveryMark(execOrders, *execution)
	if !mark.IsPositive() {
		e.recordLastCloseRecoveryBlocked(
			ctx, combination.ID, execution.ID, lastCloseRecoveryOrderNotTerminalEvent,
		)
		return nil
	}
	execution.LegAFilledQuantity = alloc.FillA.String()
	execution.LegBFilledQuantity = alloc.FillB.String()
	if completeErr := e.completeExecution(ctx, combination, execution, mark); completeErr != nil {
		if errors.Is(completeErr, ErrArbitrageExecutionTerminal) {
			e.recordLastCloseResidualRecovered(ctx, combination.ID, execution.ID, alloc)
			return nil
		}
		return completeErr
	}
	e.recordLastCloseResidualRecovered(ctx, combination.ID, execution.ID, alloc)
	return nil
}

type lastCloseRecoveryBlockRecorder interface {
	RecordLastCloseRecoveryBlocked(context.Context, string, string, string) error
}

func (e *ArbitrageExecutor) recordLastCloseRecoveryBlocked(
	ctx context.Context,
	combinationID, executionID, eventType string,
) {
	switch eventType {
	case lastCloseRecoveryOrderNotTerminalEvent, lastCloseRecoveryFillMismatchEvent:
	default:
		return
	}
	if recorder, ok := e.store.(lastCloseRecoveryBlockRecorder); ok {
		_ = recorder.RecordLastCloseRecoveryBlocked(ctx, combinationID, executionID, eventType)
		return
	}
	_ = e.store.AppendArbitrageEvent(ctx, combinationID, executionID, eventType, map[string]any{
		"executionId": executionID,
	})
}

func (e *ArbitrageExecutor) parkLastCloseClipReconciling(
	ctx context.Context,
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
) error {
	execution.Status = "reconciling"
	execution.ErrorMessage = ""
	if _, err := e.store.UpdateArbitrageExecution(ctx, *execution); err != nil {
		if errors.Is(err, ErrArbitrageExecutionTerminal) {
			return nil
		}
		return err
	}
	combination.RuntimeState = "reconciling"
	combination.ErrorMessage = ""
	_, err := e.store.UpdateArbitrageCombinationRuntime(ctx, combination)
	return err
}

func (e *ArbitrageExecutor) recordLastCloseResidualRecovered(
	ctx context.Context,
	combinationID, executionID string,
	alloc lastCloseClipAllocatedFills,
) {
	recorder, ok := e.store.(lastCloseResidualRecoveredRecorder)
	if !ok {
		return
	}
	_ = recorder.RecordLastCloseResidualRecovered(
		ctx, combinationID, executionID,
		alloc.MakerFill.String(),
		alloc.ResidualFill.String(),
		alloc.EffectiveHedgeFill.String(),
	)
}

func applyLastCloseClipAllocatedFills(
	combination ArbitrageCombination,
	execution *ArbitrageExecution,
	orders []Order,
) (decimal.Decimal, decimal.Decimal) {
	fillA := parseDecimal(execution.LegAFilledQuantity)
	fillB := parseDecimal(execution.LegBFilledQuantity)
	if !lastCloseClipClosingEligible(combination, *execution) {
		return fillA, fillB
	}
	if strings.EqualFold(strings.TrimSpace(combination.ExecutionMode), "simultaneous_market") {
		marketA, marketB, found := aggregateSimultaneousMarketLastCloseFills(execution.ID, orders)
		if !found {
			return fillA, fillB
		}
		execution.LegAFilledQuantity = marketA.String()
		execution.LegBFilledQuantity = marketB.String()
		return marketA, marketB
	}
	alloc := allocateLastCloseClipResidualFills(orders, combination, *execution)
	if !alloc.Found {
		return fillA, fillB
	}
	execution.LegAFilledQuantity = alloc.FillA.String()
	execution.LegBFilledQuantity = alloc.FillB.String()
	return alloc.FillA, alloc.FillB
}
