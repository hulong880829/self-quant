package trader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

func (r *Repository) PersistFinalArbitragePositionMetrics(
	ctx context.Context,
	combinationID string,
	expectedVersion int64,
	excludeResidual bool,
	asOf time.Time,
) (ArbitrageCombination, error) {
	return r.writeFinalArbitragePositionMetrics(
		ctx, combinationID, expectedVersion, excludeResidual, asOf, false,
	)
}

func (r *Repository) RepairFinalArbitragePositionMetrics(
	ctx context.Context,
	combinationID string,
) (ArbitrageCombination, error) {
	combo, err := r.getArbitrageCombinationByID(ctx, r.pool, combinationID)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	if err := r.assertRepairFinalMetricsReady(ctx, combo); err != nil {
		return ArbitrageCombination{}, err
	}
	return r.writeFinalArbitragePositionMetrics(
		ctx, combo.ID, combo.Version, false, time.Time{}, true,
	)
}

func (r *Repository) writeFinalArbitragePositionMetrics(
	ctx context.Context,
	combinationID string,
	expectedVersion int64,
	excludeResidual bool,
	asOf time.Time,
	allowRepair bool,
) (ArbitrageCombination, error) {
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ArbitrageCombination{}, fmt.Errorf("%w: %v", ErrFinalMetricsPersist, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	combo, err := r.lockCombinationForFinalMetrics(ctx, tx, combinationID, expectedVersion, allowRepair)
	if err != nil {
		return ArbitrageCombination{}, err
	}

	replay, err := loadArbitrageReplay(ctx, tx, combinationID, true)
	if err != nil {
		return ArbitrageCombination{}, fmt.Errorf("%w: %v", ErrFinalMetricsPersist, err)
	}
	if !replay.coverageComplete {
		return ArbitrageCombination{}, ErrFinalMetricsNotReady
	}
	zeroFill, err := combinationHasZeroFillsAndFlat(combo, replay)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	if lastFill := lastReplayFillAt(replay); !lastFill.IsZero() {
		asOf = lastFill
	}

	var (
		legA, legB      arbitrageLegCost
		exposure        decimal.Decimal
		holding         decimal.Decimal
		funding         decimal.Decimal
		estimatedFee    decimal.Decimal
		annualized      decimal.Decimal
		writeAnnualized bool
		quality         = metricsQualityComplete
	)
	if zeroFill {
		quality = metricsQualityComplete
	} else {
		if !replay.coverageComplete {
			return ArbitrageCombination{}, ErrFinalMetricsNotReady
		}
		baselineA := parseDecimal(combo.LegAVenueBaselineBasePosition)
		baselineB := parseDecimal(combo.LegBVenueBaselineBasePosition)
		legA, legB = replayArbitrageCosts(replay.fills, time.Time{}, baselineA, baselineB)
		exposure, holding = replayArbitrageExposure(
			replay.fills, combo.CreatedAt, asOf, baselineA, baselineB,
		)
		var fundingComplete bool
		funding, fundingComplete, err = loadArbitrageFundingPnL(
			ctx, tx, combinationID, replay, asOf, baselineA, baselineB,
		)
		if err != nil {
			return ArbitrageCombination{}, fmt.Errorf("%w: %v", ErrFinalMetricsPersist, err)
		}
		feeOrders, feeErr := loadTerminalFilledOrders(ctx, tx, combinationID)
		if feeErr != nil {
			return ArbitrageCombination{}, fmt.Errorf("%w: %v", ErrFinalMetricsPersist, feeErr)
		}
		accounts, accErr := loadAccountFeeSnapshots(ctx, tx, combinationID)
		if accErr != nil {
			return ArbitrageCombination{}, fmt.Errorf("%w: %v", ErrFinalMetricsPersist, accErr)
		}
		var feesComplete bool
		estimatedFee, feesComplete = estimateTradingFees(feeOrders, accounts)
		if !fundingComplete || !feesComplete {
			return ArbitrageCombination{}, ErrFinalMetricsNotReady
		}
		flat := parseDecimal(combo.LegABasePosition).IsZero() &&
			parseDecimal(combo.LegBBasePosition).IsZero()
		if !flat && !excludeResidual {
			return ArbitrageCombination{}, ErrFinalMetricsNotReady
		}
		if excludeResidual {
			quality = metricsQualityPartial
		} else {
			quality = metricsQualityComplete
		}
		profit := legA.realized.Add(legB.realized).Add(funding).Sub(estimatedFee)
		thresholdHours, thresholdErr := r.arbitrageFundingThresholdHours(ctx, tx, combinationID)
		if thresholdErr != nil {
			return ArbitrageCombination{}, fmt.Errorf("%w: %v", ErrFinalMetricsPersist, thresholdErr)
		}
		annualized, writeAnnualized = calculateCombinedAnnualized(
			profit, exposure, holding, thresholdHours,
		)
		if !writeAnnualized {
			annualized = decimal.Zero
		}
	}

	pairedInventory := legA.position.IsPositive() != legB.position.IsPositive() &&
		!legA.position.IsZero() && !legB.position.IsZero() &&
		legA.average.IsPositive() && legB.average.IsPositive()
	entrySpread := decimal.Zero
	if pairedInventory {
		entrySpread = legB.average.Div(legA.average).
			Sub(decimal.NewFromInt(1)).Mul(decimal.NewFromInt(10000))
	}
	realized := legA.realized.Add(legB.realized)
	zeroNotional := parseDecimal(combo.LegABasePosition).IsZero() &&
		parseDecimal(combo.LegBBasePosition).IsZero() &&
		parseDecimal(combo.CarryBaseQuantity).IsZero()

	var updated ArbitrageCombination
	err = tx.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			gross_turnover_notional=$3::numeric,
			leg_a_average_entry_price=$4::numeric,
			leg_b_average_entry_price=$5::numeric,
			average_entry_spread_bps=$6::numeric,
			leg_a_unrealized_pnl=0,
			leg_b_unrealized_pnl=0,
			realized_spread_pnl=$7::numeric,
			estimated_funding_pnl=$8::numeric,
			notional_exposure_seconds=$9::numeric,
			holding_seconds=$10::numeric,
			exposure_updated_at=$2,
			combined_position_annualized=$11::numeric,
			estimated_trading_fee=$12::numeric,
			metrics_calculated_at=$2,
			funding_history_complete=TRUE,
			metrics_quality=$13,
			position_notional=CASE WHEN $15::bool THEN 0 ELSE position_notional END,
			final_metrics_status='completed',
			final_metrics_completed_at=$2,
			final_metrics_last_error='',
			final_metrics_next_retry_at='infinity'::timestamptz,
			updated_at=now()
		WHERE id=$1::uuid
		  AND version=$14
		  AND final_metrics_status IN ('pending','partial')
		RETURNING `+arbitrageCombinationColumns,
		combinationID,
		asOf,
		replay.grossTurnover.String(),
		nullableDecimal(legA.average, !legA.position.IsZero() && legA.average.IsPositive()),
		nullableDecimal(legB.average, !legB.position.IsZero() && legB.average.IsPositive()),
		nullableDecimal(entrySpread, pairedInventory),
		realized.String(),
		funding.String(),
		exposure.String(),
		holding.String(),
		nullableDecimal(annualized, writeAnnualized),
		nullableDecimal(estimatedFee, true),
		quality,
		expectedVersion,
		zeroNotional,
	).Scan(arbitrageCombinationScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrArbitrageConflict
	}
	if err != nil {
		return ArbitrageCombination{}, fmt.Errorf("%w: %v", ErrFinalMetricsPersist, err)
	}
	if err = tx.Commit(ctx); err != nil {
		return ArbitrageCombination{}, fmt.Errorf("%w: %v", ErrFinalMetricsPersist, err)
	}
	return updated, nil
}

func (r *Repository) lockCombinationForFinalMetrics(
	ctx context.Context,
	tx pgx.Tx,
	combinationID string,
	expectedVersion int64,
	allowRepair bool,
) (ArbitrageCombination, error) {
	var combo ArbitrageCombination
	err := tx.QueryRow(ctx, `
		SELECT `+arbitrageCombinationColumns+`
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid
		FOR UPDATE`, combinationID,
	).Scan(arbitrageCombinationScanTargets(&combo)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrNotFound
	}
	if err != nil {
		return ArbitrageCombination{}, fmt.Errorf("%w: %v", ErrFinalMetricsPersist, err)
	}
	if combo.Version != expectedVersion {
		return ArbitrageCombination{}, ErrArbitrageConflict
	}
	if combo.PositionUncertain || combo.RuntimeState == "manual_intervention" {
		return ArbitrageCombination{}, ErrArbitrageConflict
	}
	flat := parseDecimal(combo.LegABasePosition).IsZero() &&
		parseDecimal(combo.LegBBasePosition).IsZero() &&
		parseDecimal(combo.CarryBaseQuantity).IsZero()
	taskOpen := combo.FinalMetricsStatus == finalMetricsPending ||
		combo.FinalMetricsStatus == finalMetricsPartial
	closed := strings.EqualFold(combo.Status, "closed") && flat && taskOpen
	exited := strings.EqualFold(combo.Status, "running") &&
		strings.EqualFold(combo.RunMode, "one_shot") &&
		combo.OneShotPhase == "exited" && flat && taskOpen
	if !closed && !exited {
		_ = allowRepair
		return ArbitrageCombination{}, ErrArbitrageConflict
	}
	var liveWork bool
	if err = tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM trader_arbitrage_executions e
			WHERE e.combination_id=$1::uuid
			  AND e.status NOT IN ('completed','failed','canceled','dry_run')
		) OR EXISTS (
			SELECT 1 FROM trader_orders o
			JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
			WHERE e.combination_id=$1::uuid
			  AND o.status NOT IN ('filled','canceled','rejected','expired')
		)`, combinationID,
	).Scan(&liveWork); err != nil {
		return ArbitrageCombination{}, fmt.Errorf("%w: %v", ErrFinalMetricsPersist, err)
	}
	if liveWork {
		return ArbitrageCombination{}, ErrFinalMetricsNotReady
	}
	return combo, nil
}

func combinationHasZeroFillsAndFlat(
	combo ArbitrageCombination,
	replay arbitrageReplay,
) (bool, error) {
	flat := parseDecimal(combo.LegABasePosition).IsZero() &&
		parseDecimal(combo.LegBBasePosition).IsZero()
	if len(replay.fills) == 0 && replay.orderRowCount == 0 && flat {
		return true, nil
	}
	return false, nil
}

func (r *Repository) assertRepairFinalMetricsReady(
	ctx context.Context,
	combo ArbitrageCombination,
) error {
	if !parseDecimal(combo.LegABasePosition).IsZero() ||
		!parseDecimal(combo.LegBBasePosition).IsZero() ||
		!parseDecimal(combo.CarryBaseQuantity).IsZero() ||
		!parseDecimal(combo.LegAVenueBasePosition).IsZero() ||
		!parseDecimal(combo.LegBVenueBasePosition).IsZero() ||
		!parseDecimal(combo.LegAPositionDifference).IsZero() ||
		!parseDecimal(combo.LegBPositionDifference).IsZero() {
		return fmt.Errorf("%w: combination is not flat", ErrInvalidArgument)
	}
	var liveWork bool
	if err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM trader_arbitrage_executions e
			WHERE e.combination_id=$1::uuid
			  AND e.status NOT IN ('completed','failed','canceled','dry_run')
		) OR EXISTS (
			SELECT 1 FROM trader_orders o
			JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
			WHERE e.combination_id=$1::uuid
			  AND o.status NOT IN ('filled','canceled','rejected','expired')
		)`, combo.ID,
	).Scan(&liveWork); err != nil {
		return err
	}
	if liveWork {
		return fmt.Errorf("%w: live execution or order", ErrFinalMetricsNotReady)
	}
	var missing int
	if err := r.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM trader_orders o
		JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
		WHERE e.combination_id=$1::uuid
		  AND o.filled_quantity>0
		  AND COALESCE(o.average_price,0)<=0`, combo.ID,
	).Scan(&missing); err != nil {
		return err
	}
	if missing > 0 {
		return fmt.Errorf("%w: missing fill averages", ErrFinalMetricsNotReady)
	}
	return nil
}

func lastReplayFillAt(replay arbitrageReplay) time.Time {
	var last time.Time
	for _, fill := range replay.fills {
		if last.IsZero() || fill.at.After(last) {
			last = fill.at
		}
	}
	return last
}

func persistFinalMetricsBeforeTerminal(
	ctx context.Context,
	store arbitrageFinalMetricsStore,
	combo ArbitrageCombination,
	excludeResidual bool,
	asOf time.Time,
) (ArbitrageCombination, error) {
	return store.PersistFinalArbitragePositionMetrics(
		ctx, combo.ID, combo.Version, excludeResidual, asOf,
	)
}

func recordFinalMetricsFailure(
	ctx context.Context,
	failures arbitrageCloseFailureStore,
	combo ArbitrageCombination,
	err error,
) ArbitrageCombination {
	if failures == nil || err == nil {
		return combo
	}
	key := finalMetricsPersistKey
	if errors.Is(err, ErrFinalMetricsNotReady) {
		key = finalMetricsNotReadyKey
	}
	recorded, recErr := failures.RecordArbitrageCloseFailure(
		ctx, combo.ID, key, sanitizeError(err),
	)
	if recErr == nil {
		return recorded
	}
	return combo
}

func (r *Repository) MarkArbitrageClosedAndScheduleFinalMetrics(
	ctx context.Context,
	combinationID string,
	expectedVersion int64,
) (ArbitrageCombination, bool, error) {
	var updated ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			status='closed',
			closed_at=now(),
			market_data_stale=TRUE,
			runtime_state=CASE
				WHEN position_uncertain THEN 'position_uncertain'
				ELSE 'monitoring'
			END,
			error_message=CASE WHEN position_uncertain THEN error_message ELSE '' END,
			consecutive_failures=CASE WHEN position_uncertain THEN consecutive_failures ELSE 0 END,
			last_failure_key=CASE WHEN position_uncertain THEN last_failure_key ELSE '' END,
			repeated_failure_count=CASE WHEN position_uncertain THEN repeated_failure_count ELSE 0 END,
			next_retry_at=CASE WHEN position_uncertain THEN next_retry_at ELSE '-infinity'::timestamptz END,
			one_shot_phase=CASE
				WHEN run_mode='one_shot' THEN 'exited'
				ELSE one_shot_phase
			END,
			final_metrics_status=CASE
				WHEN final_metrics_status='completed' THEN 'completed'
				ELSE 'pending'
			END,
			final_metrics_next_retry_at=CASE
				WHEN final_metrics_status='completed' THEN final_metrics_next_retry_at
				ELSE now()
			END,
			version=version+1,
			updated_at=now()
		WHERE id=$1::uuid
		  AND version=$2
		  AND status='closing'
		  AND NOT position_uncertain
		  AND runtime_state<>'manual_intervention'
		  AND leg_a_base_position=0
		  AND leg_b_base_position=0
		  AND carry_base_quantity=0
		  AND NOT EXISTS (
			SELECT 1 FROM trader_arbitrage_executions e
			WHERE e.combination_id=trader_arbitrage_combinations.id
			  AND e.status NOT IN ('completed','failed','canceled','dry_run')
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM trader_orders o
			JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
			WHERE e.combination_id=trader_arbitrage_combinations.id
			  AND o.status NOT IN ('filled','canceled','rejected','expired')
		  )
		RETURNING `+arbitrageCombinationColumns,
		combinationID, expectedVersion,
	).Scan(arbitrageCombinationScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, false, nil
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	return updated, true, nil
}

func (r *Repository) ClaimDueFinalMetrics(
	ctx context.Context,
	limit int,
) ([]ArbitrageCombination, error) {
	if limit <= 0 {
		limit = finalMetricsBatchSize
	}
	rows, err := r.pool.Query(ctx, `
		WITH due AS (
			SELECT c.id
			FROM trader_arbitrage_combinations c
			WHERE c.final_metrics_status IN ('pending','partial')
			  AND c.final_metrics_next_retry_at <= now()
			  AND c.final_metrics_next_retry_at < 'infinity'::timestamptz
			  AND NOT c.position_uncertain
			  AND c.runtime_state <> 'manual_intervention'
			  AND (
				(
					c.status = 'closed'
					AND c.leg_a_base_position = 0
					AND c.leg_b_base_position = 0
					AND c.carry_base_quantity = 0
				) OR (
					c.status = 'running'
					AND c.run_mode = 'one_shot'
					AND c.one_shot_phase = 'exited'
					AND c.leg_a_base_position = 0
					AND c.leg_b_base_position = 0
					AND c.carry_base_quantity = 0
				) OR (
					c.status = 'closing'
					AND EXISTS (
						SELECT 1
						FROM trader_orders o
						JOIN trader_arbitrage_executions e ON e.id = o.arbitrage_execution_id
						WHERE e.combination_id = c.id
						  AND o.status IN ('filled','canceled','rejected','expired')
						  AND o.filled_quantity > 0
						  AND o.average_price <= 0
					)
				)
			  )
			ORDER BY c.final_metrics_next_retry_at, c.id
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		),
		updated AS (
			UPDATE trader_arbitrage_combinations c
			SET final_metrics_next_retry_at = now() + interval '10 minutes',
			    updated_at = now()
			FROM due
			WHERE c.id = due.id
			RETURNING c.*
		)
		SELECT `+arbitrageCombinationColumns+`
		FROM updated c`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim due final metrics: %w", err)
	}
	defer rows.Close()
	claimed := make([]ArbitrageCombination, 0)
	for rows.Next() {
		var item ArbitrageCombination
		if scanErr := rows.Scan(arbitrageCombinationScanTargets(&item)...); scanErr != nil {
			return nil, fmt.Errorf("scan claimed final metrics: %w", scanErr)
		}
		claimed = append(claimed, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed final metrics: %w", err)
	}
	return claimed, nil
}

func (r *Repository) RecordFinalMetricsRetry(
	ctx context.Context,
	combinationID string,
	expectedVersion int64,
	expectedLeaseUntil time.Time,
	message string,
) (ArbitrageCombination, bool, error) {
	var updated ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			final_metrics_status='partial',
			final_metrics_last_error=$2,
			final_metrics_retry_count=final_metrics_retry_count+1,
			final_metrics_next_retry_at=now() + make_interval(secs => CASE final_metrics_retry_count
				WHEN 0 THEN 2
				WHEN 1 THEN 4
				WHEN 2 THEN 8
				WHEN 3 THEN 16
				WHEN 4 THEN 30
				ELSE 60
			END),
			updated_at=now()
		WHERE id=$1::uuid
		  AND version=$3
		  AND final_metrics_next_retry_at=$4
		  AND final_metrics_status IN ('pending','partial')
		  AND (
			status IN ('closed','closing')
			OR (
				status='running'
				AND run_mode='one_shot'
				AND one_shot_phase='exited'
			)
		  )
		RETURNING `+arbitrageCombinationColumns,
		combinationID, truncateMessage(message), expectedVersion, expectedLeaseUntil,
	).Scan(arbitrageCombinationScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, false, nil
	}
	if err != nil {
		return ArbitrageCombination{}, false, fmt.Errorf("record final metrics retry: %w", err)
	}
	return updated, true, nil
}

func (r *Repository) ReleaseFinalMetricsClaim(
	ctx context.Context,
	combinationID string,
	expectedVersion int64,
	expectedLeaseUntil time.Time,
) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET final_metrics_next_retry_at=now(), updated_at=now()
		WHERE id=$1::uuid
		  AND version=$2
		  AND final_metrics_next_retry_at=$3
		  AND final_metrics_status IN ('pending','partial')`,
		combinationID, expectedVersion, expectedLeaseUntil,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
