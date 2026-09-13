package trader

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

type confirmedAbsentFinalizeResult string

const (
	confirmedAbsentFinalizeSkipped      confirmedAbsentFinalizeResult = "skipped"
	confirmedAbsentFinalizeCanceledExec confirmedAbsentFinalizeResult = "canceled_exec"
	confirmedAbsentFinalizeRecovered    confirmedAbsentFinalizeResult = "recovered"
)

const (
	confirmedAbsentCanceledEvent  = "confirmed_absent_canceled"
	confirmedAbsentRecoveredEvent = "confirmed_absent_recovered"
)

func confirmedAbsentFinalizeEligible(combo ArbitrageCombination) bool {
	return combo.Status == "running" && !combo.CircuitOpen
}

func evaluateConfirmedAbsentZeroFillFinalize(
	combo ArbitrageCombination,
	execution ArbitrageExecution,
	orders []Order,
	executions []ArbitrageExecution,
) confirmedAbsentFinalizeResult {
	if !confirmedAbsentFinalizeEligible(combo) {
		return confirmedAbsentFinalizeSkipped
	}
	_ = executions
	if !strictZeroFilledQuantity(execution.LegAFilledQuantity) ||
		!strictZeroFilledQuantity(execution.LegBFilledQuantity) {
		return confirmedAbsentFinalizeSkipped
	}
	hasSignature := false
	for _, order := range orders {
		if order.ArbitrageExecutionID != execution.ID {
			continue
		}
		if !terminalStatus(order.Status) || order.ReconcileFailures > 0 {
			return confirmedAbsentFinalizeSkipped
		}
		if filledWithZeroQuantity(order) {
			return confirmedAbsentFinalizeSkipped
		}
		if !strictZeroFilledQuantity(order.FilledQuantity) {
			return confirmedAbsentFinalizeSkipped
		}
		if confirmedAbsentReliableZeroFill(order) {
			hasSignature = true
		}
	}
	if !hasSignature {
		return confirmedAbsentFinalizeSkipped
	}
	return confirmedAbsentFinalizeCanceledExec
}

func (r *Repository) FinalizeConfirmedAbsentZeroFillExecution(
	ctx context.Context,
	executionID string,
) (confirmedAbsentFinalizeResult, error) {
	if r == nil || r.pool == nil || executionID == "" {
		return confirmedAbsentFinalizeSkipped, nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var combinationID string
	err = tx.QueryRow(ctx, `
		SELECT combination_id::text
		FROM trader_arbitrage_executions
		WHERE id=$1::uuid`,
		executionID,
	).Scan(&combinationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return confirmedAbsentFinalizeSkipped, nil
	}
	if err != nil {
		return "", err
	}

	var combo ArbitrageCombination
	err = tx.QueryRow(ctx, `
		SELECT `+arbitrageCombinationColumns+`
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid
		FOR UPDATE`,
		combinationID,
	).Scan(arbitrageCombinationScanTargets(&combo)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return confirmedAbsentFinalizeSkipped, nil
	}
	if err != nil {
		return "", err
	}

	execRows, err := tx.Query(ctx, `
		SELECT `+arbitrageExecutionColumns+`
		FROM trader_arbitrage_executions
		WHERE combination_id=$1::uuid
		ORDER BY id
		FOR UPDATE`,
		combinationID,
	)
	if err != nil {
		return "", err
	}
	var executions []ArbitrageExecution
	for execRows.Next() {
		var item ArbitrageExecution
		if scanErr := execRows.Scan(arbitrageExecutionScanTargets(&item)...); scanErr != nil {
			execRows.Close()
			return "", scanErr
		}
		executions = append(executions, item)
	}
	if err = execRows.Err(); err != nil {
		execRows.Close()
		return "", err
	}
	execRows.Close()

	var execution ArbitrageExecution
	foundExec := false
	for _, item := range executions {
		if item.ID == executionID {
			execution = item
			foundExec = true
			break
		}
	}
	if !foundExec {
		return confirmedAbsentFinalizeSkipped, nil
	}

	orderRows, err := tx.Query(ctx, `
		SELECT `+orderColumns+`
		FROM trader_orders o
		WHERE o.arbitrage_execution_id IN (
			SELECT e.id FROM trader_arbitrage_executions e WHERE e.combination_id=$1::uuid
		)
		ORDER BY o.id
		FOR UPDATE OF o`,
		combinationID,
	)
	if err != nil {
		return "", err
	}
	var orders []Order
	for orderRows.Next() {
		var order Order
		if scanErr := orderRows.Scan(orderScanTargets(&order)...); scanErr != nil {
			orderRows.Close()
			return "", scanErr
		}
		orders = append(orders, order)
	}
	if err = orderRows.Err(); err != nil {
		orderRows.Close()
		return "", err
	}
	orderRows.Close()

	outcome := evaluateConfirmedAbsentZeroFillFinalize(combo, execution, orders, executions)
	if outcome == confirmedAbsentFinalizeSkipped {
		return confirmedAbsentFinalizeSkipped, nil
	}

	canceledNow := false
	if !terminalArbitrageExecutionStatus(execution.Status) {
		err = tx.QueryRow(ctx, `
			UPDATE trader_arbitrage_executions SET
				status='canceled',
				error_message='',
				updated_at=now(),
				closed_at=COALESCE(closed_at,now())
			WHERE id=$1::uuid
			  AND status NOT IN ('completed','failed','canceled','dry_run')
			RETURNING `+arbitrageExecutionColumns,
			execution.ID,
		).Scan(arbitrageExecutionScanTargets(&execution)...)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `
				SELECT `+arbitrageExecutionColumns+`
				FROM trader_arbitrage_executions
				WHERE id=$1::uuid`,
				executionID,
			).Scan(arbitrageExecutionScanTargets(&execution)...)
			if errors.Is(err, pgx.ErrNoRows) {
				return confirmedAbsentFinalizeSkipped, nil
			}
			if err != nil {
				return "", err
			}
			if !terminalArbitrageExecutionStatus(execution.Status) {
				return confirmedAbsentFinalizeSkipped, nil
			}
		} else if err != nil {
			return "", err
		} else {
			canceledNow = true
		}
	}

	eventType := confirmedAbsentCanceledEvent
	var protectedCombo ArbitrageCombination
	err = tx.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			position_uncertain=TRUE,
			runtime_state='position_uncertain',
			error_message=$2,
			version=version+1,
			updated_at=now()
		WHERE id=$1::uuid
		  AND status='running'
		  AND NOT circuit_open
		RETURNING `+arbitrageCombinationColumns,
		combo.ID, arbitrageOrderReconcileError,
	).Scan(arbitrageCombinationScanTargets(&protectedCombo)...)
	if errors.Is(err, pgx.ErrNoRows) {
		if !canceledNow {
			return confirmedAbsentFinalizeSkipped, nil
		}
	} else if err != nil {
		return "", err
	}

	encoded, err := json.Marshal(redactPayload(map[string]any{
		"reason": "confirmed_absent_zero_fill",
		"result": string(outcome),
	}))
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO trader_arbitrage_events(combination_id,execution_id,event_type,payload)
		VALUES($1::uuid,$2::uuid,$3,$4::jsonb)`,
		combo.ID, execution.ID, eventType, encoded,
	); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return outcome, nil
}
