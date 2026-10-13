package trader

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

func (r *Repository) ClaimCircuitRecoveryExecution(
	ctx context.Context,
	in ClaimCircuitRecoveryExecutionInput,
) (ArbitrageExecution, bool, error) {
	execution := in.Execution
	if strings.TrimSpace(execution.CombinationID) == "" ||
		strings.TrimSpace(in.FailedExecutionID) == "" {
		return ArbitrageExecution{}, false, ErrInvalidArgument
	}
	if execution.ID == "" {
		execution.ID = newCircuitRecoveryID()
	}
	if !strings.EqualFold(strings.TrimSpace(execution.PositionEffect), "open") ||
		execution.LastCloseClip || execution.ReduceOnly {
		return ArbitrageExecution{}, false, nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var combo ArbitrageCombination
	err = tx.QueryRow(ctx, `
		SELECT `+arbitrageCombinationColumns+`
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid
		FOR UPDATE`, execution.CombinationID,
	).Scan(arbitrageCombinationScanTargets(&combo)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageExecution{}, false, ErrNotFound
	}
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	if combo.Version != in.ExpectedVersion || combo.Status != "running" ||
		!combo.CircuitOpen || arbitrageFlattening(combo) {
		return ArbitrageExecution{}, false, nil
	}
	phase := strings.ToLower(strings.TrimSpace(combo.OneShotPhase))
	if phase == "waiting_exit" || phase == "exiting" || phase == "exited" {
		return ArbitrageExecution{}, false, nil
	}
	var active int
	if err = tx.QueryRow(ctx, `
		SELECT count(*)
		FROM trader_arbitrage_executions
		WHERE combination_id=$1::uuid
		  AND status NOT IN ('completed','failed','canceled','dry_run')`,
		combo.ID,
	).Scan(&active); err != nil {
		return ArbitrageExecution{}, false, err
	}
	if active > 0 {
		return ArbitrageExecution{}, false, nil
	}
	var live int
	if err = tx.QueryRow(ctx, `
		SELECT count(*)
		FROM trader_orders o
		JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
		WHERE e.combination_id=$1::uuid
		  AND o.status NOT IN ('filled','canceled','rejected','expired')`,
		combo.ID,
	).Scan(&live); err != nil {
		return ArbitrageExecution{}, false, err
	}
	if live > 0 {
		return ArbitrageExecution{}, false, nil
	}
	var failed ArbitrageExecution
	err = tx.QueryRow(ctx, `
		SELECT `+arbitrageExecutionColumns+`
		FROM trader_arbitrage_executions
		WHERE id=$1::uuid AND combination_id=$2::uuid
		FOR UPDATE`, in.FailedExecutionID, combo.ID,
	).Scan(arbitrageExecutionScanTargets(&failed)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageExecution{}, false, nil
	}
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	if !strings.EqualFold(failed.Status, "failed") ||
		!strings.EqualFold(strings.TrimSpace(failed.PositionEffect), "open") ||
		failed.LastCloseClip {
		return ArbitrageExecution{}, false, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT `+orderColumns+`
		FROM trader_orders
		WHERE arbitrage_execution_id=$1::uuid
		FOR UPDATE`, failed.ID,
	)
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	defer rows.Close()
	var failedOrders []Order
	for rows.Next() {
		var order Order
		if scanErr := rows.Scan(orderScanTargets(&order)...); scanErr != nil {
			return ArbitrageExecution{}, false, scanErr
		}
		failedOrders = append(failedOrders, order)
	}
	if err = rows.Err(); err != nil {
		return ArbitrageExecution{}, false, err
	}
	rows.Close()
	if _, ok := quoteRecoveryOrder(failed, failedOrders); !ok {
		return ArbitrageExecution{}, false, nil
	}
	requested := parsePositiveDecimal(execution.RequestedNotional)
	targetBase := parsePositiveDecimal(execution.TargetBaseQuantity)
	if !requested.IsPositive() || !targetBase.IsPositive() {
		return ArbitrageExecution{}, false, nil
	}
	priceA := parsePositiveDecimal(execution.TriggerLegAAsk)
	priceB := parsePositiveDecimal(execution.TriggerLegBAsk)
	if !priceA.IsPositive() || !priceB.IsPositive() {
		return ArbitrageExecution{}, false, nil
	}
	midA := parsePositiveDecimal(execution.TriggerLegABid).Add(priceA).Div(decimal.NewFromInt(2))
	midB := parsePositiveDecimal(execution.TriggerLegBBid).Add(priceB).Div(decimal.NewFromInt(2))
	if !midA.IsPositive() {
		midA = priceA
	}
	if !midB.IsPositive() {
		midB = priceB
	}
	remaining := remainingDirectionNotional(
		parseDecimal(combo.LegABasePosition), parseDecimal(combo.LegBBasePosition),
		parsePositiveDecimal(combo.TargetNotional), midA, midB,
	)
	if remaining.LessThan(decimal.NewFromInt(arbitrageMinOpenNotional)) ||
		requested.GreaterThan(remaining) {
		return ArbitrageExecution{}, false, nil
	}
	execution.Status = "claimed"
	execution.PositionEffect = "open"
	execution.ReduceOnly = false
	execution.LastCloseClip = false
	var result ArbitrageExecution
	err = tx.QueryRow(ctx, `
		INSERT INTO trader_arbitrage_executions (
			id,combination_id,direction,sequence,status,
			trigger_ask_spread_bps,trigger_bid_spread_bps,
			trigger_leg_a_bid,trigger_leg_a_ask,trigger_leg_b_bid,trigger_leg_b_ask,
			target_base_quantity,requested_notional,position_effect,reduce_only,last_close_clip,
			leg_a_filled_quantity,leg_b_filled_quantity,delta_notional,
			hedge_sequence,attempt,error_message
		)
		VALUES ($1::uuid,$2::uuid,$3,
			COALESCE((SELECT max(sequence)+1 FROM trader_arbitrage_executions WHERE combination_id=$2::uuid),1),
			$4,$5::numeric,$6::numeric,$7::numeric,$8::numeric,$9::numeric,$10::numeric,
			$11::numeric,$12::numeric,$13,$14,$15,$16::numeric,$17::numeric,$18::numeric,$19,$20,$21)
		RETURNING `+arbitrageExecutionColumns,
		execution.ID, execution.CombinationID, execution.Direction, execution.Status,
		execution.TriggerAskSpread, execution.TriggerBidSpread,
		execution.TriggerLegABid, execution.TriggerLegAAsk,
		execution.TriggerLegBBid, execution.TriggerLegBAsk,
		execution.TargetBaseQuantity, zeroString(execution.RequestedNotional),
		execution.PositionEffect, execution.ReduceOnly, execution.LastCloseClip,
		zeroString(execution.LegAFilledQuantity),
		zeroString(execution.LegBFilledQuantity), zeroString(execution.DeltaNotional),
		execution.HedgeSequence, execution.Attempt, execution.ErrorMessage,
	).Scan(arbitrageExecutionScanTargets(&result)...)
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET version=version+1, updated_at=now()
		WHERE id=$1::uuid AND version=$2 AND circuit_open AND runtime_state='manual_intervention'`,
		combo.ID, combo.Version,
	)
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	if tag.RowsAffected() != 1 {
		return ArbitrageExecution{}, false, nil
	}
	if err = tx.Commit(ctx); err != nil {
		return ArbitrageExecution{}, false, err
	}
	return result, true, nil
}

func (r *Repository) CompleteCircuitOpenRecovery(
	ctx context.Context,
	request circuitOpenRecoveryCloseRequest,
) (ArbitrageCombination, bool, error) {
	if strings.TrimSpace(request.CombinationID) == "" ||
		strings.TrimSpace(request.ExecutionID) == "" {
		return ArbitrageCombination{}, false, ErrInvalidArgument
	}
	if request.Kind != circuitCloseExplained && request.Kind != circuitCloseFilled {
		return ArbitrageCombination{}, false, nil
	}
	if !request.MarkA.IsPositive() || !request.MarkB.IsPositive() {
		return ArbitrageCombination{}, false, nil
	}
	if !circuitRecoverySnapshotFresh(request.RecoveryStartedAt, request.ObservedAt, time.Now().UTC()) {
		return ArbitrageCombination{}, false, nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var owner string
	err = tx.QueryRow(ctx, `
		SELECT owner_username FROM trader_arbitrage_combinations WHERE id=$1::uuid`,
		request.CombinationID,
	).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, false, ErrNotFound
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, owner); err != nil {
		return ArbitrageCombination{}, false, err
	}
	var combo ArbitrageCombination
	err = tx.QueryRow(ctx, `
		SELECT `+arbitrageCombinationColumns+`
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid
		FOR UPDATE`, request.CombinationID,
	).Scan(arbitrageCombinationScanTargets(&combo)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, false, ErrNotFound
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	if combo.Version != request.ExpectedVersion || combo.Status != "running" ||
		!combo.CircuitOpen || !circuitOpenBaselineComplete(combo) {
		return combo, false, nil
	}
	if !circuitRecoverySnapshotFresh(request.RecoveryStartedAt, request.ObservedAt, time.Now().UTC()) {
		return combo, false, nil
	}
	var conflictID string
	err = tx.QueryRow(ctx, `
		WITH proposed(account_id,instrument_id) AS (
			VALUES ($2::bigint,$3::bigint),($4::bigint,$5::bigint)
		),
		active AS (
			SELECT id,leg_a_trading_account_id AS account_id,leg_a_instrument_id AS instrument_id
			FROM trader_arbitrage_combinations
			WHERE owner_username=$1 AND status IN ('running','closing') AND id<>$6::uuid
			UNION ALL
			SELECT id,leg_b_trading_account_id,leg_b_instrument_id
			FROM trader_arbitrage_combinations
			WHERE owner_username=$1 AND status IN ('running','closing') AND id<>$6::uuid
		)
		SELECT active.id::text
		FROM active
		JOIN proposed USING(account_id,instrument_id)
		LIMIT 1`,
		combo.OwnerUsername,
		combo.LegA.TradingAccountID, combo.LegA.InstrumentID,
		combo.LegB.TradingAccountID, combo.LegB.InstrumentID,
		combo.ID,
	).Scan(&conflictID)
	if err == nil {
		return combo, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, false, err
	}
	var execution ArbitrageExecution
	err = tx.QueryRow(ctx, `
		SELECT `+arbitrageExecutionColumns+`
		FROM trader_arbitrage_executions
		WHERE id=$1::uuid AND combination_id=$2::uuid
		FOR UPDATE`, request.ExecutionID, combo.ID,
	).Scan(arbitrageExecutionScanTargets(&execution)...)
	if errors.Is(err, pgx.ErrNoRows) || terminalArbitrageExecutionStatus(execution.Status) {
		return combo, false, nil
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	rows, err := tx.Query(ctx, `
		SELECT `+orderColumns+`
		FROM trader_orders o
		WHERE o.arbitrage_execution_id IN (
			SELECT e.id FROM trader_arbitrage_executions e WHERE e.combination_id=$1::uuid
		)
		ORDER BY o.id
		FOR UPDATE OF o`, combo.ID,
	)
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	defer rows.Close()
	orderSumA, orderSumB := decimal.Zero, decimal.Zero
	for rows.Next() {
		var order Order
		if scanErr := rows.Scan(orderScanTargets(&order)...); scanErr != nil {
			return ArbitrageCombination{}, false, scanErr
		}
		if !terminalStatus(order.Status) || order.ReconcileFailures > 0 {
			return combo, false, nil
		}
		if parseDecimal(order.FilledQuantity).IsPositive() {
			delta := signedFilledBase(order)
			if strings.EqualFold(order.ArbitrageLeg, "b") {
				orderSumB = orderSumB.Add(delta)
			} else {
				orderSumA = orderSumA.Add(delta)
			}
		}
	}
	if err = rows.Err(); err != nil {
		return ArbitrageCombination{}, false, err
	}
	rows.Close()
	if !orderSumA.Equal(request.OrderSumA) || !orderSumB.Equal(request.OrderSumB) {
		return combo, false, nil
	}
	baselineA := parseDecimal(combo.LegAVenueBaselineBasePosition)
	baselineB := parseDecimal(combo.LegBVenueBaselineBasePosition)
	adjustmentA := parseDecimal(combo.LegAReconciliationAdjustment)
	adjustmentB := parseDecimal(combo.LegBReconciliationAdjustment)
	if !request.VenueA.Equal(baselineA.Add(orderSumA).Add(adjustmentA)) ||
		!request.VenueB.Equal(baselineB.Add(orderSumB).Add(adjustmentB)) {
		return combo, false, nil
	}
	fillA := parseDecimal(request.LegAFilled)
	fillB := parseDecimal(request.LegBFilled)
	if !fillA.IsPositive() || !fillB.IsPositive() {
		return combo, false, nil
	}
	localA := orderSumA.Add(adjustmentA)
	localB := orderSumB.Add(adjustmentB)
	updatedCombo := combo
	updatedCombo.LegABasePosition = localA.String()
	updatedCombo.LegBBasePosition = localB.String()
	_, comboNotional, _ := arbitragePositionNotionals(updatedCombo, request.MarkA, request.MarkB)
	turnover := parseDecimal(request.TurnoverDelta).Abs()
	fillAText := zeroString(request.LegAFilled)
	fillBText := zeroString(request.LegBFilled)
	deltaText := zeroString(request.DeltaNotional)
	writeFills := request.Kind == circuitCloseFilled
	var applied ArbitrageCombination
	err = tx.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			leg_a_base_position=$2::numeric,
			leg_b_base_position=$3::numeric,
			position_notional=$4::numeric,
			cumulative_turnover_notional=cumulative_turnover_notional+ABS($5::numeric),
			leg_a_position_difference=0,
			leg_b_position_difference=0,
			leg_a_venue_base_position=$6::numeric,
			leg_b_venue_base_position=$7::numeric,
			last_position_reconciled_at=now(),
			circuit_open=FALSE,
			position_uncertain=FALSE,
			runtime_state='monitoring',
			error_message='',
			consecutive_failures=0,
			repeated_failure_count=0,
			last_failure_key='',
			next_retry_at='-infinity'::timestamptz,
			version=version+1,
			updated_at=now()
		WHERE id=$1::uuid AND version=$8 AND circuit_open
		  AND runtime_state='manual_intervention'
		RETURNING `+arbitrageCombinationColumns,
		combo.ID, localA.String(), localB.String(), comboNotional.String(), turnover.String(),
		request.VenueA.String(), request.VenueB.String(), combo.Version,
	).Scan(arbitrageCombinationScanTargets(&applied)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return combo, false, nil
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE trader_arbitrage_executions SET
			status='completed',
			error_message='',
			leg_a_filled_quantity=CASE WHEN $2 THEN $3::numeric ELSE leg_a_filled_quantity END,
			leg_b_filled_quantity=CASE WHEN $2 THEN $4::numeric ELSE leg_b_filled_quantity END,
			delta_notional=CASE WHEN $2 THEN $5::numeric ELSE delta_notional END,
			closed_at=now(),
			updated_at=now()
		WHERE id=$1::uuid
		  AND status NOT IN ('completed','failed','canceled','dry_run')`,
		execution.ID, writeFills, fillAText, fillBText, deltaText,
	)
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	if tag.RowsAffected() != 1 {
		return combo, false, nil
	}
	eventType := strings.TrimSpace(request.EventType)
	if eventType == "" {
		eventType = "execution_completed"
	}
	payload := request.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	payload["executionId"] = execution.ID
	payload["kind"] = request.Kind
	body, err := json.Marshal(redactPayload(payload))
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO trader_arbitrage_events(combination_id,execution_id,event_type,payload)
		VALUES($1::uuid,$2::uuid,$3,$4::jsonb)`,
		combo.ID, execution.ID, eventType, body,
	); err != nil {
		return ArbitrageCombination{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ArbitrageCombination{}, false, err
	}
	return applied, true, nil
}
