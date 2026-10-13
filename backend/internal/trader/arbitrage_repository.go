package trader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

func (r *Repository) CreateArbitrageIntents(
	ctx context.Context,
	orders []Order,
) ([]Order, []bool, error) {
	if len(orders) == 0 {
		return nil, nil, ErrInvalidArgument
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	results := make([]Order, 0, len(orders))
	createdFlags := make([]bool, 0, len(orders))
	for _, order := range orders {
		if order.ID == "" {
			order.ID = uuid.NewString()
		}
		if order.ClientOrderID == "" {
			order.ClientOrderID = clientOrderID()
		}
		var result Order
		err := tx.QueryRow(ctx, `
			INSERT INTO trader_orders (
				id,idempotency_key,owner_username,trading_account_id,product_name,
				exchange,instrument_id,contract_type,exchange_symbol,client_order_id,
				side,order_type,quantity,price,status,request_fingerprint,base_asset,quote_asset,
				twap_job_id,twap_slice_index,twap_attempt_index,
				arbitrage_execution_id,arbitrage_leg,arbitrage_role,reduce_only
			) VALUES (
				$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NULLIF($14,'')::numeric,
				'pending',$15,$16,$17,NULL,NULL,0,NULLIF($18,'')::uuid,NULLIF($19,''),NULLIF($20,''),$21
			)
			ON CONFLICT (idempotency_key) DO NOTHING
			RETURNING `+orderColumns, orderWriteArgs(order)...,
		).Scan(orderScanTargets(&result)...)
		created := true
		if errors.Is(err, pgx.ErrNoRows) {
			created = false
			err = tx.QueryRow(ctx, `
				SELECT `+orderColumns+` FROM trader_orders WHERE idempotency_key=$1`,
				order.IdempotencyKey,
			).Scan(orderScanTargets(&result)...)
		}
		if err != nil {
			return nil, nil, err
		}
		if result.RequestFingerprint != order.RequestFingerprint {
			return nil, nil, ErrIdempotencyConflict
		}
		if created {
			payload, _ := json.Marshal(map[string]any{
				"side": result.Side, "orderType": result.OrderType,
				"quantity": result.Quantity, "arbitrageRole": result.ArbitrageRole,
			})
			if _, err := tx.Exec(ctx, `
				INSERT INTO trader_order_events(order_id,event_type,payload)
				VALUES($1::uuid,'intent',$2::jsonb)`, result.ID, payload); err != nil {
				return nil, nil, err
			}
		}
		results = append(results, result)
		createdFlags = append(createdFlags, created)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return results, createdFlags, nil
}

const activeArbitrageInstrumentConflictSQL = `
		WITH proposed(account_id,instrument_id) AS (
			VALUES ($2::bigint,$3::bigint),($4::bigint,$5::bigint)
		),
		active AS (
			SELECT id,leg_a_trading_account_id AS account_id,
				leg_a_instrument_id AS instrument_id,
				leg_a_account_name AS account_name,leg_a_exchange AS exchange,
				leg_a_exchange_symbol AS exchange_symbol
			FROM trader_arbitrage_combinations
			WHERE owner_username=$1 AND status IN ('running','closing')
			  AND idempotency_key<>$6
			UNION ALL
			SELECT id,leg_b_trading_account_id,leg_b_instrument_id,
				leg_b_account_name,leg_b_exchange,leg_b_exchange_symbol
			FROM trader_arbitrage_combinations
			WHERE owner_username=$1 AND status IN ('running','closing')
			  AND idempotency_key<>$6
		)
		SELECT active.id::text,active.account_name,active.exchange,active.exchange_symbol
		FROM active
		JOIN proposed USING(account_id,instrument_id)
		LIMIT 1`

func scanActiveArbitrageInstrumentConflict(row pgx.Row) error {
	var conflictID, conflictAccount, conflictExchange, conflictSymbol string
	err := row.Scan(&conflictID, &conflictAccount, &conflictExchange, &conflictSymbol)
	if err == nil {
		return fmt.Errorf(
			"%w: 账户 %s 的 %s %s 已被运行中套利组合 %s 占用，请先关闭该组合",
			ErrActiveArbitrageInstrumentConflict,
			conflictAccount, conflictExchange, conflictSymbol, conflictID,
		)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func (r *Repository) FindActiveArbitrageInstrumentConflict(
	ctx context.Context,
	owner, idempotencyKey string,
	accountA, instrumentA, accountB, instrumentB int64,
) error {
	return scanActiveArbitrageInstrumentConflict(r.pool.QueryRow(
		ctx, activeArbitrageInstrumentConflictSQL,
		owner, accountA, instrumentA, accountB, instrumentB, idempotencyKey,
	))
}

func (r *Repository) CreateArbitrageCombination(
	ctx context.Context,
	item ArbitrageCombination,
) (ArbitrageCombination, bool, error) {
	if item.ID == "" {
		item.ID = uuid.NewString()
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, item.OwnerUsername); err != nil {
		return ArbitrageCombination{}, false, err
	}
	if err := scanActiveArbitrageInstrumentConflict(tx.QueryRow(
		ctx, activeArbitrageInstrumentConflictSQL,
		item.OwnerUsername,
		item.LegA.TradingAccountID, item.LegA.InstrumentID,
		item.LegB.TradingAccountID, item.LegB.InstrumentID,
		item.IdempotencyKey,
	)); err != nil {
		return ArbitrageCombination{}, false, err
	}
	var created ArbitrageCombination
	err = tx.QueryRow(ctx, `
		INSERT INTO trader_arbitrage_combinations (
			id,idempotency_key,request_fingerprint,owner_username,
			leg_a_trading_account_id,leg_a_instrument_id,leg_a_product_name,leg_a_account_name,
			leg_a_exchange,leg_a_contract_type,leg_a_exchange_symbol,leg_a_base_asset,leg_a_quote_asset,
			leg_b_trading_account_id,leg_b_instrument_id,leg_b_product_name,leg_b_account_name,
			leg_b_exchange,leg_b_contract_type,leg_b_exchange_symbol,leg_b_base_asset,leg_b_quote_asset,
			ask_threshold_bps,bid_threshold_bps,target_notional,order_notional,
			execution_mode,maker_leg,status,position_notional,cumulative_turnover_notional,
			leg_a_venue_baseline_base_position,leg_b_venue_baseline_base_position,
			venue_baseline_captured_at,market_data_stale,
			run_mode,entry_direction,leg_a_leverage,leg_b_leverage,
			exit_policy,exit_annualized_rate,exit_after_seconds,one_shot_phase,
			early_exit_funding_8h_annualized_floor,early_exit_liquidation_distance_pct,scheduled_exit_at
		) VALUES (
			$1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,
			$14,$15,$16,$17,$18,$19,$20,$21,$22,
			$23::numeric,$24::numeric,$25::numeric,$26::numeric,
			$27,$28,$29,$30::numeric,$31::numeric,
			NULLIF($32,'')::numeric,NULLIF($33,'')::numeric,$34,$35,
			$36,NULLIF($37,''),$38::numeric,$39::numeric,
			NULLIF($40,''),$41::numeric,$42::integer,$43,$44::numeric,$45::numeric,
			CASE WHEN $42::integer IS NOT NULL THEN now() + make_interval(secs => $42::integer) ELSE NULL END
		)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING `+arbitrageCombinationColumns,
		arbitrageCombinationWriteArgs(item)...,
	).Scan(arbitrageCombinationScanTargets(&created)...)
	inserted := true
	if errors.Is(err, pgx.ErrNoRows) {
		inserted = false
		err = tx.QueryRow(ctx, `
			SELECT `+arbitrageCombinationColumns+`
			FROM trader_arbitrage_combinations WHERE idempotency_key=$1`,
			item.IdempotencyKey,
		).Scan(arbitrageCombinationScanTargets(&created)...)
	}
	if err != nil {
		return ArbitrageCombination{}, false, fmt.Errorf("create arbitrage combination: %w", err)
	}
	if inserted {
		event := map[string]any{"executionMode": created.ExecutionMode}
		if created.LegAVenueBaselineBasePosition != "" {
			event["legAVenueBaselineBasePosition"] = created.LegAVenueBaselineBasePosition
			event["legBVenueBaselineBasePosition"] = created.LegBVenueBaselineBasePosition
			event["venueBaselineCapturedAt"] = created.VenueBaselineCapturedAt
		}
		payload, _ := json.Marshal(event)
		if _, err := tx.Exec(ctx, `
			INSERT INTO trader_arbitrage_events(combination_id,event_type,payload)
			VALUES($1::uuid,'created',$2::jsonb)`, created.ID, payload); err != nil {
			return ArbitrageCombination{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ArbitrageCombination{}, false, err
	}
	return created, inserted, nil
}

func (r *Repository) GetArbitrageCombinationByOwner(
	ctx context.Context,
	owner, id string,
) (ArbitrageCombination, error) {
	var item ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		SELECT `+arbitrageCombinationColumns+`
		FROM trader_arbitrage_combinations
		WHERE owner_username=$1 AND id=$2::uuid`, owner, id,
	).Scan(arbitrageCombinationScanTargets(&item)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrNotFound
	}
	return item, err
}

func (r *Repository) GetArbitrageCombinationByIdempotencyKey(
	ctx context.Context,
	owner, key string,
) (ArbitrageCombination, error) {
	var item ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		SELECT `+arbitrageCombinationColumns+`
		FROM trader_arbitrage_combinations
		WHERE owner_username=$1 AND idempotency_key=$2`, owner, key,
	).Scan(arbitrageCombinationScanTargets(&item)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrNotFound
	}
	return item, err
}

func (r *Repository) GetArbitrageControlSummary(
	ctx context.Context,
	id string,
) (arbitrageControlSummary, error) {
	var summary arbitrageControlSummary
	err := r.pool.QueryRow(ctx, `
		SELECT version,status,runtime_state
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid`,
		id,
	).Scan(&summary.Version, &summary.Status, &summary.RuntimeState)
	if errors.Is(err, pgx.ErrNoRows) {
		return arbitrageControlSummary{}, ErrNotFound
	}
	return summary, err
}

func (r *Repository) ListArbitrageCombinations(
	ctx context.Context,
	owner, view string,
	limit int,
	cursor string,
) ([]ArbitrageCombination, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	statusClause := `status IN ('running','closing')`
	timeColumn := "updated_at"
	if view == "closed" {
		statusClause = `status IN ('closed','failed')`
		timeColumn = "closed_at"
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+arbitrageCombinationColumns+`
		FROM trader_arbitrage_combinations
		WHERE owner_username=$1 AND `+statusClause+`
		  AND ($2='' OR (`+timeColumn+`,id) < (
			SELECT `+timeColumn+`,id FROM trader_arbitrage_combinations WHERE id=$2::uuid
		  ))
		ORDER BY `+timeColumn+` DESC NULLS LAST,id DESC
		LIMIT $3`, owner, cursor, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := make([]ArbitrageCombination, 0, limit)
	for rows.Next() {
		var item ArbitrageCombination
		if err := rows.Scan(arbitrageCombinationScanTargets(&item)...); err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		next = items[limit-1].ID
		items = items[:limit]
	}
	return items, next, nil
}

func (r *Repository) CountArbitrageCombinations(
	ctx context.Context,
	owner, view string,
) (int64, error) {
	statusClause := `status IN ('running','closing')`
	if view == "closed" {
		statusClause = `status IN ('closed','failed')`
	}
	var count int64
	err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM trader_arbitrage_combinations
		WHERE owner_username=$1 AND `+statusClause, owner).Scan(&count)
	return count, err
}

func (r *Repository) UpdateArbitrageCombinationConfig(
	ctx context.Context,
	owner, id string,
	input UpdateArbitrageInput,
) (ArbitrageCombination, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current ArbitrageCombination
	err = tx.QueryRow(ctx, `
		SELECT `+arbitrageCombinationColumns+`
		FROM trader_arbitrage_combinations
		WHERE owner_username=$1 AND id=$2::uuid
		FOR UPDATE`, owner, id,
	).Scan(arbitrageCombinationScanTargets(&current)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrNotFound
	}
	if err != nil {
		return ArbitrageCombination{}, err
	}
	if current.Status != "running" {
		return ArbitrageCombination{}, ErrArbitrageConflict
	}
	previous := map[string]any{
		"askThresholdBps":                   current.AskThresholdBps,
		"bidThresholdBps":                   current.BidThresholdBps,
		"targetNotional":                    current.TargetNotional,
		"exitAfterSeconds":                  current.ExitAfterSeconds,
		"exitAnnualizedRate":                current.ExitAnnualizedRate,
		"earlyExitFunding8hAnnualizedFloor": current.EarlyExitFunding8hAnnualizedFloor,
		"earlyExitLiquidationDistancePct":   current.EarlyExitLiquidationDistancePct,
		"oneShotPhase":                      current.OneShotPhase,
	}
	ask, bid, target := current.AskThresholdBps, current.BidThresholdBps, current.TargetNotional
	if input.AskThresholdBps != nil {
		ask = *input.AskThresholdBps
	}
	if input.BidThresholdBps != nil {
		bid = *input.BidThresholdBps
	}
	if input.TargetNotional != nil {
		target = *input.TargetNotional
	}
	targetValue, targetErr := decimal.NewFromString(target)
	positionValue, positionErr := decimal.NewFromString(current.PositionNotional)
	if targetErr != nil || positionErr != nil || !targetValue.IsPositive() {
		return ArbitrageCombination{}, fmt.Errorf(
			"%w: target notional must be positive",
			ErrArbitrageConfigInvalid,
		)
	}
	if strings.EqualFold(strings.TrimSpace(current.RunMode), "one_shot") {
		updated, err := r.updateOneShotArbitrageCombinationConfig(
			ctx, tx, current, input, target, targetValue, positionValue,
		)
		if err != nil {
			return ArbitrageCombination{}, err
		}
		current = updated
	} else {
		if input.ExitAfterSeconds != nil || input.ExitAnnualizedRate != nil ||
			input.EarlyExitFunding8hAnnualizedFloor != nil ||
			input.EarlyExitLiquidationDistancePct != nil {
			return ArbitrageCombination{}, fmt.Errorf(
				"%w: spread combinations cannot patch one-shot exit fields",
				ErrArbitrageConfigInvalid,
			)
		}
		if input.TargetNotional != nil && targetValue.LessThan(positionValue.Abs()) {
			return ArbitrageCombination{}, fmt.Errorf(
				"%w: target notional cannot be below current absolute position %s",
				ErrArbitrageConfigInvalid, positionValue.Abs().String(),
			)
		}
		err = tx.QueryRow(ctx, `
			UPDATE trader_arbitrage_combinations SET
				ask_threshold_bps=$3::numeric,
				bid_threshold_bps=$4::numeric,
				target_notional=$5::numeric,
				version=version+1,updated_at=now()
			WHERE owner_username=$1 AND id=$2::uuid AND status='running' AND run_mode='spread'
			RETURNING `+arbitrageCombinationColumns,
			owner, id, ask, bid, target,
		).Scan(arbitrageCombinationScanTargets(&current)...)
		if errors.Is(err, pgx.ErrNoRows) {
			return ArbitrageCombination{}, ErrArbitrageConflict
		}
		if err != nil {
			return ArbitrageCombination{}, err
		}
	}
	payload, err := json.Marshal(map[string]any{
		"previous": previous,
		"updated": map[string]any{
			"askThresholdBps":                   ask,
			"bidThresholdBps":                   bid,
			"targetNotional":                    target,
			"exitAfterSeconds":                  input.ExitAfterSeconds,
			"exitAnnualizedRate":                input.ExitAnnualizedRate,
			"earlyExitFunding8hAnnualizedFloor": input.EarlyExitFunding8hAnnualizedFloor,
			"earlyExitLiquidationDistancePct":   input.EarlyExitLiquidationDistancePct,
		},
	})
	if err != nil {
		return ArbitrageCombination{}, err
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO trader_arbitrage_events(combination_id,event_type,payload)
		VALUES($1::uuid,'config_updated',$2::jsonb)`, id, payload); err != nil {
		return ArbitrageCombination{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ArbitrageCombination{}, err
	}
	return current, nil
}

func (r *Repository) updateOneShotArbitrageCombinationConfig(
	ctx context.Context,
	tx pgx.Tx,
	current ArbitrageCombination,
	input UpdateArbitrageInput,
	target string,
	targetValue, positionValue decimal.Decimal,
) (ArbitrageCombination, error) {
	phase := strings.TrimSpace(current.OneShotPhase)
	if phase != "building_target" && phase != "waiting_exit" {
		return ArbitrageCombination{}, ErrArbitrageConflict
	}
	if current.PositionUncertain || current.RuntimeState == "manual_intervention" {
		return ArbitrageCombination{}, ErrArbitrageConflict
	}
	if input.AskThresholdBps != nil || input.BidThresholdBps != nil {
		return ArbitrageCombination{}, fmt.Errorf(
			"%w: one_shot combinations cannot patch spread thresholds",
			ErrArbitrageConfigInvalid,
		)
	}
	var liveWork bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM trader_arbitrage_executions e
			WHERE e.combination_id=$1::uuid
			  AND e.status NOT IN ('completed','failed','canceled','dry_run')
		) OR EXISTS (
			SELECT 1 FROM trader_orders o
			JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
			WHERE e.combination_id=$1::uuid
			  AND o.status NOT IN ('filled','canceled','rejected','expired')
		)`, current.ID,
	).Scan(&liveWork); err != nil {
		return ArbitrageCombination{}, err
	}
	if liveWork {
		return ArbitrageCombination{}, ErrArbitrageConflict
	}
	oldTarget, _ := decimal.NewFromString(current.TargetNotional)
	occupancy := positionValue.Abs()
	if input.TargetNotional != nil {
		if occupancy.IsPositive() {
			if !targetValue.GreaterThan(oldTarget) || targetValue.LessThan(occupancy) {
				return ArbitrageCombination{}, fmt.Errorf(
					"%w: one_shot target with inventory must exceed the previous target and cover occupancy %s",
					ErrArbitrageConfigInvalid, occupancy.String(),
				)
			}
		}
	}
	exitAfter := current.ExitAfterSeconds
	if input.ExitAfterSeconds != nil {
		if current.ExitPolicy != "time" {
			return ArbitrageCombination{}, fmt.Errorf(
				"%w: historical annualized combinations cannot patch hold time",
				ErrArbitrageConfigInvalid,
			)
		}
		exitAfter = *input.ExitAfterSeconds
	}
	exitRate := current.ExitAnnualizedRate
	if input.ExitAnnualizedRate != nil {
		if strings.TrimSpace(current.ExitAnnualizedRate) == "" {
			return ArbitrageCombination{}, fmt.Errorf(
				"%w: annualized early-exit cannot be enabled after create",
				ErrArbitrageConfigInvalid,
			)
		}
		exitRate = *input.ExitAnnualizedRate
	}
	floor := current.EarlyExitFunding8hAnnualizedFloor
	if input.EarlyExitFunding8hAnnualizedFloor != nil {
		if strings.TrimSpace(current.EarlyExitFunding8hAnnualizedFloor) == "" {
			return ArbitrageCombination{}, fmt.Errorf(
				"%w: 8h funding early-exit cannot be enabled after create",
				ErrArbitrageConfigInvalid,
			)
		}
		floor = *input.EarlyExitFunding8hAnnualizedFloor
	}
	liquidationDistance := current.EarlyExitLiquidationDistancePct
	if input.EarlyExitLiquidationDistancePct != nil {
		if strings.TrimSpace(current.EarlyExitLiquidationDistancePct) == "" {
			return ArbitrageCombination{}, fmt.Errorf(
				"%w: liquidation distance early-exit cannot be enabled after create",
				ErrArbitrageConfigInvalid,
			)
		}
		liquidationDistance = *input.EarlyExitLiquidationDistancePct
	}
	scheduled := current.ScheduledExitAt
	if input.ExitAfterSeconds != nil && !current.CreatedAt.IsZero() {
		scheduled = scheduledExitFromCreated(current.CreatedAt, exitAfter)
	}
	nextPhase := phase
	if phase == "waiting_exit" && input.TargetNotional != nil &&
		targetValue.GreaterThan(oldTarget) &&
		(scheduled.IsZero() || scheduled.Equal(time.Unix(0, 0).UTC()) ||
			scheduled.After(time.Now().UTC())) {
		nextPhase = "building_target"
	}
	var updated ArbitrageCombination
	err := tx.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			target_notional=$3::numeric,
			exit_after_seconds=$4,
			exit_annualized_rate=$5::numeric,
			early_exit_funding_8h_annualized_floor=$6::numeric,
			early_exit_liquidation_distance_pct=$9::numeric,
			scheduled_exit_at=$7,
			one_shot_phase=$8,
			version=version+1,updated_at=now()
		WHERE id=$1::uuid AND owner_username=$2
		  AND status='running' AND run_mode='one_shot'
		  AND one_shot_phase IN ('building_target','waiting_exit')
		  AND NOT position_uncertain
		  AND runtime_state<>'manual_intervention'
		RETURNING `+arbitrageCombinationColumns,
		current.ID, current.OwnerUsername, target,
		nullableInt(exitAfter), nullableNumeric(exitRate), nullableNumeric(floor),
		nullableArbitrageTimestamp(scheduled), nextPhase,
		nullableNumeric(liquidationDistance),
	).Scan(arbitrageCombinationScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrArbitrageConflict
	}
	return updated, err
}

func (r *Repository) ListArbitrageCombinationsForPositionAudit(
	ctx context.Context,
	limit int,
) ([]ArbitrageCombination, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+arbitrageCombinationColumns+`
		FROM trader_arbitrage_combinations c
		WHERE (
				c.status IN ('running','closing')
				OR (
					c.status='closed'
					AND c.position_uncertain
					AND c.error_message LIKE $2
				)
			)
		  AND NOT EXISTS (
				SELECT 1 FROM trader_arbitrage_executions e
				WHERE e.combination_id=c.id
				  AND e.status NOT IN ('completed','failed','canceled','dry_run')
			)
		  AND NOT EXISTS (
				SELECT 1
				FROM trader_orders o
				JOIN trader_arbitrage_executions e
				  ON e.id=o.arbitrage_execution_id
				WHERE e.combination_id=c.id
				  AND (
					o.status NOT IN ('filled','canceled','rejected','expired')
					OR o.reconcile_failures>0
				  )
			)
		ORDER BY last_position_reconciled_at ASC NULLS FIRST,updated_at ASC
		LIMIT $1`, limit, arbitragePositionAuditErrorPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ArbitrageCombination, 0, limit)
	for rows.Next() {
		var item ArbitrageCombination
		if err := rows.Scan(arbitrageCombinationScanTargets(&item)...); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *Repository) UpdateArbitragePositionAudit(
	ctx context.Context,
	id string,
	expectedVersion int64,
	legAVenue, legBVenue, legADifference, legBDifference string,
	uncertain bool,
	message string,
	legALiquidationDistance, legBLiquidationDistance string,
) (ArbitrageCombination, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		currentVersion                   int64
		status                           string
		currentUncertain                 bool
		runtimeState                     string
		currentError                     string
		circuitOpen                      bool
		activeExecution                  string
		activeOrder                      bool
		reconcileFailure                 bool
		hasExecution                     bool
		hasOrder                         bool
		carryBaseQuantity                string
		consecutiveFailures              int
		repeatedFailureCount             int
		lastFailureKey                   string
		hasCompletedExecution            bool
		lastCloseResidualRecoveredLatest bool
	)
	err = tx.QueryRow(ctx, `
		SELECT c.version,c.status,c.position_uncertain,c.runtime_state,
		       c.error_message,c.circuit_open,
		       COALESCE((
					SELECT e.id::text
					FROM trader_arbitrage_executions e
					WHERE e.combination_id=c.id
					  AND e.status NOT IN ('completed','failed','canceled','dry_run')
					ORDER BY e.created_at DESC
					LIMIT 1
			   ),''),
		       EXISTS (
					SELECT 1
					FROM trader_orders o
					JOIN trader_arbitrage_executions e
					  ON e.id=o.arbitrage_execution_id
					WHERE e.combination_id=c.id
					  AND o.status NOT IN ('filled','canceled','rejected','expired')
			   ),
		       EXISTS (
					SELECT 1
					FROM trader_orders o
					JOIN trader_arbitrage_executions e
					  ON e.id=o.arbitrage_execution_id
					WHERE e.combination_id=c.id
					  AND o.reconcile_failures>0
			   ),
		       EXISTS (
					SELECT 1 FROM trader_arbitrage_executions e
					WHERE e.combination_id=c.id
			   ),
		       EXISTS (
					SELECT 1
					FROM trader_orders o
					JOIN trader_arbitrage_executions e
					  ON e.id=o.arbitrage_execution_id
					WHERE e.combination_id=c.id
			   ),
		       trim_scale(c.carry_base_quantity)::text,
		       c.consecutive_failures,
		       c.repeated_failure_count,
		       COALESCE(c.last_failure_key,''),
		       EXISTS (
					SELECT 1 FROM trader_arbitrage_executions e
					WHERE e.combination_id=c.id
					  AND e.status='completed'
			   ),
		       EXISTS (
					SELECT 1
					FROM trader_arbitrage_executions e
					WHERE e.combination_id=c.id
					  AND e.status='completed'
					  AND e.last_close_clip
					  AND lower(trim(e.position_effect))='close'
					  AND e.reduce_only
					  AND e.sequence=(
							SELECT max(sequence)
							FROM trader_arbitrage_executions
							WHERE combination_id=c.id
					  )
					  AND EXISTS (
							SELECT 1
							FROM trader_arbitrage_events ev
							WHERE ev.execution_id=e.id
							  AND ev.combination_id=c.id
							  AND ev.event_type='last_close_residual_recovered'
					  )
			   )
		FROM trader_arbitrage_combinations c
		WHERE c.id=$1::uuid
		FOR UPDATE OF c`,
		id,
	).Scan(
		&currentVersion, &status, &currentUncertain, &runtimeState,
		&currentError, &circuitOpen, &activeExecution, &activeOrder,
		&reconcileFailure, &hasExecution, &hasOrder, &carryBaseQuantity,
		&consecutiveFailures, &repeatedFailureCount, &lastFailureKey,
		&hasCompletedExecution, &lastCloseResidualRecoveredLatest,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrNotFound
	}
	if err != nil {
		return ArbitrageCombination{}, err
	}
	if currentVersion != expectedVersion {
		return ArbitrageCombination{}, &ArbitragePositionAuditDeferredError{
			Reason: ArbitragePositionAuditDeferredVersionConflict,
		}
	}
	if activeExecution != "" {
		return ArbitrageCombination{}, &ArbitragePositionAuditDeferredError{
			Reason: ArbitragePositionAuditDeferredActiveExecution, ExecutionID: activeExecution,
		}
	}
	if activeOrder {
		return ArbitrageCombination{}, &ArbitragePositionAuditDeferredError{
			Reason: ArbitragePositionAuditDeferredActiveOrder,
		}
	}
	if reconcileFailure {
		return ArbitrageCombination{}, &ArbitragePositionAuditDeferredError{
			Reason: ArbitragePositionAuditDeferredReconcileFailure,
		}
	}
	if status != "running" && status != "closing" &&
		!(status == "closed" && currentUncertain &&
			strings.HasPrefix(currentError, arbitragePositionAuditErrorPrefix)) {
		return ArbitrageCombination{}, ErrNotFound
	}

	nextUncertain := currentUncertain
	nextRuntime := runtimeState
	nextError := currentError
	auditOwned := strings.HasPrefix(currentError, arbitragePositionAuditErrorPrefix)
	zeroDifferences := exactZeroDecimal(legADifference) && exactZeroDecimal(legBDifference)
	if uncertain {
		if !circuitOpen && (status == "running" || status == "closing") &&
			(!currentUncertain || currentError == "" || auditOwned) {
			nextUncertain = true
			nextRuntime = "position_uncertain"
			nextError = truncateMessage(message)
		} else if status == "closed" && currentUncertain && auditOwned {
			nextError = truncateMessage(message)
		}
	} else {
		clearAuditOwned := currentUncertain && auditOwned && !circuitOpen
		clearStickyEmpty := currentUncertain && currentError == "" &&
			runtimeState == "monitoring" && status == "running" &&
			!circuitOpen && zeroDifferences
		clearOrderUncertain := currentUncertain &&
			currentError == arbitrageOrderReconcileError &&
			runtimeState == "position_uncertain" && status == "running" &&
			!circuitOpen && zeroDifferences && hasExecution && hasOrder
		clearLastCloseResidualAudited := lastCloseResidualAuditUnlockEligible(
			status, runtimeState, currentError,
			currentUncertain, circuitOpen, lastCloseResidualRecoveredLatest, zeroDifferences,
		)
		if clearAuditOwned || clearStickyEmpty || clearOrderUncertain ||
			clearLastCloseResidualAudited {
			nextUncertain = false
			nextError = ""
			switch status {
			case "closing":
				nextRuntime = "closing"
			default:
				nextRuntime = "monitoring"
			}
		}
	}
	if status == "running" &&
		runtimeState == "manual_intervention" &&
		!circuitOpen &&
		!currentUncertain &&
		!uncertain &&
		!nextUncertain &&
		exactZeroDecimal(carryBaseQuantity) &&
		zeroDifferences &&
		currentError == "" &&
		consecutiveFailures == 0 &&
		repeatedFailureCount == 0 &&
		lastFailureKey == "" &&
		hasCompletedExecution {
		nextRuntime = "monitoring"
		nextError = ""
	}

	var updated ArbitrageCombination
	err = tx.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			leg_a_venue_base_position=$2::numeric,
			leg_b_venue_base_position=$3::numeric,
			leg_a_position_difference=$4::numeric,
			leg_b_position_difference=$5::numeric,
			last_position_reconciled_at=now(),
			position_uncertain=$6,
			runtime_state=$7,
			error_message=$8,
			leg_a_liquidation_distance_pct=NULLIF($10,'')::numeric,
			leg_b_liquidation_distance_pct=NULLIF($11,'')::numeric,
			version=version+1,updated_at=now()
		WHERE id=$1::uuid AND version=$9
		RETURNING `+arbitrageCombinationColumns,
		id, legAVenue, legBVenue, legADifference, legBDifference,
		nextUncertain, nextRuntime, nextError, expectedVersion,
		legALiquidationDistance, legBLiquidationDistance,
	).Scan(arbitrageCombinationScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, &ArbitragePositionAuditDeferredError{
			Reason: ArbitragePositionAuditDeferredVersionConflict,
		}
	}
	if err != nil {
		return ArbitrageCombination{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ArbitrageCombination{}, err
	}
	return updated, nil
}

func exactZeroDecimal(value string) bool {
	return strictZeroFilledQuantity(value)
}

func (r *Repository) MarkArbitrageClosing(
	ctx context.Context,
	owner, id string,
) (ArbitrageCombination, error) {
	var item ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations
		SET status=CASE WHEN status='running' OR status='failed' THEN 'closing' ELSE status END,
		    runtime_state=CASE WHEN status='running' OR status='failed' THEN 'closing' ELSE runtime_state END,
		    error_message=CASE WHEN status='running' OR status='failed' THEN '' ELSE error_message END,
		    consecutive_failures=CASE WHEN status='running' OR status='failed' THEN 0 ELSE consecutive_failures END,
		    repeated_failure_count=CASE WHEN status='running' OR status='failed' THEN 0 ELSE repeated_failure_count END,
		    last_failure_key=CASE WHEN status='running' OR status='failed' THEN '' ELSE last_failure_key END,
		    next_retry_at=CASE WHEN status='running' OR status='failed' THEN '-infinity'::timestamptz ELSE next_retry_at END,
		    circuit_open=CASE WHEN status='running' OR status='failed' THEN FALSE ELSE circuit_open END,
		    scheduler_lease_until='-infinity',
		    closed_at=CASE WHEN status='failed' THEN NULL ELSE closed_at END,
		    version=version+1,updated_at=now()
		WHERE owner_username=$1 AND id=$2::uuid
		RETURNING `+arbitrageCombinationColumns, owner, id,
	).Scan(arbitrageCombinationScanTargets(&item)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrNotFound
	}
	if err == nil {
		_ = r.AppendArbitrageEvent(ctx, id, "", "closing", map[string]any{"by": "user"})
	}
	return item, err
}

func (r *Repository) MarkOneShotWaitingExit(
	ctx context.Context,
	id string,
	version int64,
	scheduledExitAt time.Time,
) (ArbitrageCombination, bool, error) {
	var item ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations
		SET one_shot_phase='waiting_exit',
		    target_reached_at=now(),
		    scheduled_exit_at=COALESCE(scheduled_exit_at, $3),
		    version=version+1,
		    updated_at=now()
		WHERE id=$1::uuid AND version=$2
		  AND status='running'
		  AND run_mode='one_shot'
		  AND one_shot_phase='building_target'
		RETURNING `+arbitrageCombinationColumns,
		id, version, nullableArbitrageTimestamp(scheduledExitAt),
	).Scan(arbitrageCombinationScanTargets(&item)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, false, nil
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	_ = r.AppendArbitrageEvent(ctx, id, "", "one_shot_waiting_exit", map[string]any{
		"scheduledExitAt": scheduledExitAt,
	})
	return item, true, nil
}

func (r *Repository) MarkOneShotExiting(
	ctx context.Context,
	id string,
	version int64,
	reason string,
	details map[string]any,
) (ArbitrageCombination, bool, error) {
	reason = strings.ToLower(strings.TrimSpace(reason))
	if reason != "annualized" && reason != "time" && reason != "funding_8h_floor" &&
		reason != "liquidation_distance_threshold" {
		return ArbitrageCombination{}, false, ErrInvalidArgument
	}
	var item ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations
		SET one_shot_phase='exiting',
		    version=version+1,
		    updated_at=now()
		WHERE id=$1::uuid AND version=$2
		  AND status='running'
		  AND run_mode='one_shot'
		  AND one_shot_phase IN ('building_target','waiting_exit')
		  AND (
			(
				$3='time'
				AND scheduled_exit_at IS NOT NULL
				AND scheduled_exit_at <= now()
			)
			OR (
				$3='annualized'
				AND exit_annualized_rate IS NOT NULL
			)
			OR (
				$3='funding_8h_floor'
				AND early_exit_funding_8h_annualized_floor IS NOT NULL
			)
			OR (
				$3='liquidation_distance_threshold'
				AND early_exit_liquidation_distance_pct IS NOT NULL
				AND (
					(
						leg_a_liquidation_distance_pct IS NOT NULL
						AND leg_a_liquidation_distance_pct
							<= early_exit_liquidation_distance_pct
					)
					OR (
						leg_b_liquidation_distance_pct IS NOT NULL
						AND leg_b_liquidation_distance_pct
							<= early_exit_liquidation_distance_pct
					)
				)
			)
		  )
		  AND NOT position_uncertain
		  AND runtime_state<>'manual_intervention'
		  AND NOT EXISTS (
			SELECT 1 FROM trader_arbitrage_executions e
			WHERE e.combination_id=trader_arbitrage_combinations.id
			  AND e.status NOT IN ('completed','failed','canceled','dry_run')
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM trader_orders o
			JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
			WHERE e.combination_id=trader_arbitrage_combinations.id
			  AND o.filled_quantity>0
			  AND o.status NOT IN ('filled','canceled','rejected','expired')
		  )
		RETURNING `+arbitrageCombinationColumns,
		id, version, reason,
	).Scan(arbitrageCombinationScanTargets(&item)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, false, nil
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	payload := map[string]any{"reason": reason}
	for key, value := range details {
		payload[key] = value
	}
	_ = r.AppendArbitrageEvent(ctx, id, "", "one_shot_exit_started", payload)
	return item, true, nil
}

func (r *Repository) MarkOneShotAnnualizedExiting(
	ctx context.Context,
	guard oneShotAnnualizedExitGuard,
) (ArbitrageCombination, bool, error) {
	if strings.TrimSpace(guard.CombinationID) == "" ||
		!guard.ExitAnnualizedRate.IsPositive() {
		return ArbitrageCombination{}, false, ErrInvalidArgument
	}
	var item ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations
		SET one_shot_phase='exiting',
		    version=version+1,
		    updated_at=now()
		WHERE id=$1::uuid
		  AND status='running'
		  AND run_mode='one_shot'
		  AND one_shot_phase IN ('building_target','waiting_exit')
		  AND exit_annualized_rate = $2::numeric
		  AND COALESCE(metrics_calculated_at, 'epoch'::timestamptz) = $3::timestamptz
		  AND leg_a_base_position = $4::numeric
		  AND leg_b_base_position = $5::numeric
		  AND leg_a_average_entry_price = $6::numeric
		  AND leg_b_average_entry_price = $7::numeric
		  AND COALESCE(leg_a_venue_baseline_base_position, 0) = $8::numeric
		  AND COALESCE(leg_b_venue_baseline_base_position, 0) = $9::numeric
		  AND COALESCE(venue_baseline_captured_at, 'epoch'::timestamptz) = $10::timestamptz
		  AND COALESCE(leg_a_venue_base_position, 0) = $11::numeric
		  AND COALESCE(leg_b_venue_base_position, 0) = $12::numeric
		  AND COALESCE(leg_a_position_difference, 0) = $13::numeric
		  AND COALESCE(leg_b_position_difference, 0) = $14::numeric
		  AND NOT position_uncertain
		  AND runtime_state <> 'manual_intervention'
		  AND NOT EXISTS (
			SELECT 1 FROM trader_arbitrage_executions e
			WHERE e.combination_id=trader_arbitrage_combinations.id
			  AND e.status NOT IN ('completed','failed','canceled','dry_run')
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM trader_orders o
			JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
			WHERE e.combination_id=trader_arbitrage_combinations.id
			  AND o.filled_quantity>0
			  AND o.status NOT IN ('filled','canceled','rejected','expired')
		  )
		RETURNING `+arbitrageCombinationColumns,
		guard.CombinationID,
		guard.ExitAnnualizedRate.String(),
		annualizedExitClock(guard.CalculatedAt),
		guard.ComboA.String(),
		guard.ComboB.String(),
		guard.AvgA.String(),
		guard.AvgB.String(),
		guard.BaselineA.String(),
		guard.BaselineB.String(),
		annualizedExitClock(guard.BaselineCapturedAt),
		guard.VenueA.String(),
		guard.VenueB.String(),
		guard.DifferenceA.String(),
		guard.DifferenceB.String(),
	).Scan(arbitrageCombinationScanTargets(&item)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, false, nil
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	_ = r.AppendArbitrageEvent(ctx, guard.CombinationID, "", "one_shot_exit_started", map[string]any{
		"reason": "annualized",
	})
	return item, true, nil
}

func (r *Repository) MarkOneShotExited(
	ctx context.Context,
	id string,
	version int64,
) (ArbitrageCombination, bool, error) {
	var item ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations
		SET one_shot_phase='exited',
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
		WHERE id=$1::uuid AND version=$2
		  AND status='running'
		  AND run_mode='one_shot'
		  AND one_shot_phase='exiting'
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
		id, version,
	).Scan(arbitrageCombinationScanTargets(&item)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, false, nil
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	_ = r.AppendArbitrageEvent(ctx, id, "", "one_shot_exit_completed", nil)
	return item, true, nil
}

func (r *Repository) MarkOneShotExitedWithDust(
	ctx context.Context,
	id string,
	version int64,
	expectedA, expectedB, expectedCarry string,
) (ArbitrageCombination, bool, error) {
	if _, err := decimal.NewFromString(strings.TrimSpace(expectedA)); err != nil {
		return ArbitrageCombination{}, false, ErrInvalidArgument
	}
	if _, err := decimal.NewFromString(strings.TrimSpace(expectedB)); err != nil {
		return ArbitrageCombination{}, false, ErrInvalidArgument
	}
	if _, err := decimal.NewFromString(strings.TrimSpace(expectedCarry)); err != nil {
		return ArbitrageCombination{}, false, ErrInvalidArgument
	}
	var item ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations
		SET one_shot_phase='exited',
		    final_metrics_status=CASE
		        WHEN final_metrics_status='completed' THEN 'completed'
		        ELSE 'partial'
		    END,
		    final_metrics_next_retry_at=CASE
		        WHEN final_metrics_status='completed' THEN final_metrics_next_retry_at
		        ELSE 'infinity'::timestamptz
		    END,
		    final_metrics_completed_at=CASE
		        WHEN final_metrics_status='completed' THEN final_metrics_completed_at
		        ELSE NULL
		    END,
		    final_metrics_last_error=CASE
		        WHEN final_metrics_status='completed' THEN final_metrics_last_error
		        ELSE 'one_shot_dust_metrics_skipped'
		    END,
		    version=version+1,
		    updated_at=now()
		WHERE id=$1::uuid AND version=$2
		  AND status='running'
		  AND run_mode='one_shot'
		  AND one_shot_phase='exiting'
		  AND NOT position_uncertain
		  AND runtime_state<>'manual_intervention'
		  AND circuit_open = false
		  AND carry_base_quantity <> 0
		  AND carry_base_quantity = leg_a_base_position + leg_b_base_position
		  AND leg_a_base_position = $3::numeric
		  AND leg_b_base_position = $4::numeric
		  AND carry_base_quantity = $5::numeric
		  AND (
			(leg_a_base_position <> 0 AND leg_b_base_position = 0)
			OR (leg_a_base_position = 0 AND leg_b_base_position <> 0)
		  )
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
		id, version, expectedA, expectedB, expectedCarry,
	).Scan(arbitrageCombinationScanTargets(&item)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, false, nil
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	_ = r.AppendArbitrageEvent(ctx, id, "", "one_shot_exited_with_dust", map[string]any{
		"legABasePosition":  expectedA,
		"legBBasePosition":  expectedB,
		"carryBaseQuantity": expectedCarry,
	})
	return item, true, nil
}

func (r *Repository) ArbitrageDustOrderGate(
	ctx context.Context,
	combinationID string,
	reconciledAt time.Time,
) (liveOrder, unreconciled, liveExecution bool, err error) {
	err = r.pool.QueryRow(ctx, `
		SELECT
		  EXISTS (
			SELECT 1
			FROM trader_orders o
			JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
			WHERE e.combination_id=$1::uuid
			  AND o.status NOT IN ('filled','canceled','rejected','expired')
		  ),
		  EXISTS (
			SELECT 1
			FROM trader_orders o
			JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
			WHERE e.combination_id=$1::uuid
			  AND o.updated_at > $2
		  ),
		  EXISTS (
			SELECT 1 FROM trader_arbitrage_executions e
			WHERE e.combination_id=$1::uuid
			  AND e.status NOT IN ('completed','failed','canceled','dry_run')
		  )`,
		combinationID, reconciledAt,
	).Scan(&liveOrder, &unreconciled, &liveExecution)
	return liveOrder, unreconciled, liveExecution, err
}

func (r *Repository) ListOneShotFunding8hExitRows(
	ctx context.Context,
	windowStart, windowEnd time.Time,
) ([]funding8hExitRow, error) {
	rows, err := r.pool.Query(ctx, `
		WITH candidates AS (
			SELECT id, version, entry_direction,
			       trim_scale(early_exit_funding_8h_annualized_floor)::text AS floor,
			       leg_a_instrument_id, leg_b_instrument_id,
			       leg_a_contract_type, leg_b_contract_type,
			       COALESCE(target_reached_at, created_at) AS target_reached_at
			FROM trader_arbitrage_combinations
			WHERE status='running'
			  AND run_mode='one_shot'
			  AND one_shot_phase IN ('building_target','waiting_exit')
			  AND early_exit_funding_8h_annualized_floor IS NOT NULL
		), legs AS (
			SELECT id, version, entry_direction, floor, 'a'::text AS leg,
			       leg_a_instrument_id AS instrument_id,
			       leg_a_contract_type AS contract_type,
			       target_reached_at
			FROM candidates
			UNION ALL
			SELECT id, version, entry_direction, floor, 'b',
			       leg_b_instrument_id, leg_b_contract_type, target_reached_at
			FROM candidates
		)
		SELECT l.id::text, l.version, COALESCE(l.entry_direction,''), l.floor,
		       l.leg, l.contract_type, l.target_reached_at,
		       f.funding_time, f.funding_rate::text, f.interval_hours::text
		FROM legs l
		LEFT JOIN funding_rates f
		  ON l.contract_type='perpetual'
		 AND f.instrument_id=l.instrument_id
		 AND f.record_kind='settled'
		 AND (
			(f.funding_time > $1 AND f.funding_time <= $2)
			OR f.funding_time = (
				SELECT MAX(p.funding_time) FROM funding_rates p
				WHERE p.instrument_id=l.instrument_id
				  AND p.record_kind='settled'
				  AND p.funding_time <= $1
			)
		 )`, windowStart, windowEnd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []funding8hExitRow
	for rows.Next() {
		var row funding8hExitRow
		if scanErr := rows.Scan(
			&row.CombinationID, &row.Version, &row.EntryDirection, &row.Floor,
			&row.Leg, &row.ContractType, &row.TargetReachedAt,
			&row.FundingTime, &row.FundingRate, &row.IntervalHours,
		); scanErr != nil {
			return nil, scanErr
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (r *Repository) ListArbitrageOrders(
	ctx context.Context,
	owner, combinationID string,
) ([]Order, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+orderColumns+`
		FROM trader_orders
		WHERE owner_username=$1
		  AND EXISTS (
			SELECT 1
			FROM trader_arbitrage_executions e
			WHERE e.id=trader_orders.arbitrage_execution_id
			  AND e.combination_id=$2::uuid
		  )
		ORDER BY created_at DESC`, owner, combinationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Order
	for rows.Next() {
		var item Order
		if err := rows.Scan(orderScanTargets(&item)...); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *Repository) ListRecentSubmittedArbitrageOrders(
	ctx context.Context,
	owner, combinationID string,
	limit int,
) ([]Order, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+orderColumns+`
		FROM trader_orders
		WHERE owner_username=$1
		  AND EXISTS (
			SELECT 1
			FROM trader_arbitrage_executions e
			WHERE e.id=trader_orders.arbitrage_execution_id
			  AND e.combination_id=$2::uuid
		  )
		  AND EXISTS (
			SELECT 1
			FROM trader_order_events event
			WHERE event.order_id=trader_orders.id
			  AND event.event_type='submitted'
		  )
		ORDER BY updated_at DESC,id DESC
		LIMIT $3`, owner, combinationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Order, 0, limit)
	for rows.Next() {
		var item Order
		if err := rows.Scan(orderScanTargets(&item)...); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *Repository) ListArbitrageExecutions(
	ctx context.Context,
	combinationID string,
	limit int,
) ([]ArbitrageExecution, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+arbitrageExecutionColumns+`
		FROM trader_arbitrage_executions
		WHERE combination_id=$1::uuid
		ORDER BY created_at DESC,id DESC LIMIT $2`, combinationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []ArbitrageExecution
	for rows.Next() {
		var item ArbitrageExecution
		if err := rows.Scan(arbitrageExecutionScanTargets(&item)...); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *Repository) ListArbitrageEvents(
	ctx context.Context,
	combinationID string,
	limit int,
) ([]ArbitrageEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id::text,combination_id::text,COALESCE(execution_id::text,''),
		       event_type,payload::text,created_at
		FROM trader_arbitrage_events
		WHERE combination_id=$1::uuid
		ORDER BY created_at DESC,id DESC LIMIT $2`, combinationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []ArbitrageEvent
	for rows.Next() {
		item, err := scanArbitrageEvent(rows.Scan)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *Repository) LeaseArbitrageCombinations(
	ctx context.Context,
	limit int,
	lease time.Duration,
) ([]ArbitrageCombination, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
		WITH due AS (
			SELECT id FROM trader_arbitrage_combinations
			WHERE status IN ('running','closing') AND scheduler_lease_until <= now()
			ORDER BY updated_at,id FOR UPDATE SKIP LOCKED LIMIT $1
		), leased AS (
			UPDATE trader_arbitrage_combinations c
			SET scheduler_lease_until=now()+$2::interval,updated_at=now()
			FROM due WHERE c.id=due.id
			RETURNING c.*
		)
		SELECT `+arbitrageCombinationColumns+` FROM leased`, limit, lease.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []ArbitrageCombination
	for rows.Next() {
		var item ArbitrageCombination
		if err := rows.Scan(arbitrageCombinationScanTargets(&item)...); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *Repository) RenewArbitrageLease(
	ctx context.Context,
	id string,
	lease time.Duration,
) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET scheduler_lease_until=now()+$2::interval
		WHERE id=$1::uuid AND status IN ('running','closing')
		  AND scheduler_lease_until > now()`, id, lease.String())
	return tag.RowsAffected() == 1, err
}

func (r *Repository) ClaimArbitrageExecution(
	ctx context.Context,
	execution ArbitrageExecution,
) (ArbitrageExecution, bool, error) {
	if execution.ID == "" {
		execution.ID = uuid.NewString()
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status, runtimeState, runMode, oneShotPhase string
	var positionUncertain bool
	var targetNotional, legABase, legBBase, baselineA, baselineB string
	var venueA, venueB string
	var baselineAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT status,runtime_state,run_mode,COALESCE(one_shot_phase,''),
		       position_uncertain,target_notional::text,
		       trim_scale(leg_a_base_position)::text,trim_scale(leg_b_base_position)::text,
		       COALESCE(leg_a_venue_baseline_base_position::text,''),
		       COALESCE(leg_b_venue_baseline_base_position::text,''),
		       COALESCE(venue_baseline_captured_at,'epoch'::timestamptz),
		       COALESCE(leg_a_venue_base_position::text,''),
		       COALESCE(leg_b_venue_base_position::text,'')
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid
		FOR UPDATE`,
		execution.CombinationID,
	).Scan(
		&status, &runtimeState, &runMode, &oneShotPhase,
		&positionUncertain, &targetNotional, &legABase, &legBBase,
		&baselineA, &baselineB, &baselineAt, &venueA, &venueB,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageExecution{}, false, nil
	}
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	combo := ArbitrageCombination{
		Status:                        status,
		RuntimeState:                  runtimeState,
		RunMode:                       runMode,
		OneShotPhase:                  oneShotPhase,
		PositionUncertain:             positionUncertain,
		LegABasePosition:              legABase,
		LegBBasePosition:              legBBase,
		LegAVenueBaselineBasePosition: baselineA,
		LegBVenueBaselineBasePosition: baselineB,
		VenueBaselineCapturedAt:       baselineAt,
		LegAVenueBasePosition:         venueA,
		LegBVenueBasePosition:         venueB,
	}
	flattening := arbitrageFlattening(combo)
	opening := strings.EqualFold(strings.TrimSpace(execution.PositionEffect), "open") ||
		strings.TrimSpace(execution.PositionEffect) == ""
	if positionUncertain {
		return ArbitrageExecution{}, false, nil
	}
	if runtimeState == "manual_intervention" {
		return ArbitrageExecution{}, false, nil
	}
	if flattening {
		if !strings.EqualFold(strings.TrimSpace(execution.PositionEffect), "close") ||
			!execution.ReduceOnly {
			return ArbitrageExecution{}, false, nil
		}
	} else if status != "running" {
		return ArbitrageExecution{}, false, nil
	}
	if !flattening && strings.EqualFold(runMode, "one_shot") &&
		(oneShotPhase == "waiting_exit" || oneShotPhase == "exited") {
		return ArbitrageExecution{}, false, nil
	}
	requested := parsePositiveDecimal(execution.RequestedNotional)
	targetBase := parsePositiveDecimal(execution.TargetBaseQuantity)
	if !requested.IsPositive() || !targetBase.IsPositive() {
		return ArbitrageExecution{}, false, nil
	}
	priceA := parsePositiveDecimal(execution.TriggerLegAAsk)
	priceB := parsePositiveDecimal(execution.TriggerLegBAsk)
	if strings.EqualFold(strings.TrimSpace(execution.Direction), "bid") {
		priceA = parsePositiveDecimal(execution.TriggerLegABid)
		priceB = parsePositiveDecimal(execution.TriggerLegBBid)
	}
	if !priceA.IsPositive() || !priceB.IsPositive() {
		return ArbitrageExecution{}, false, nil
	}
	implied := decimal.Max(targetBase.Mul(priceA), targetBase.Mul(priceB))
	midA := parsePositiveDecimal(execution.TriggerLegABid).Add(parsePositiveDecimal(execution.TriggerLegAAsk)).
		Div(decimal.NewFromInt(2))
	midB := parsePositiveDecimal(execution.TriggerLegBBid).Add(parsePositiveDecimal(execution.TriggerLegBAsk)).
		Div(decimal.NewFromInt(2))
	if !midA.IsPositive() {
		midA = priceA
	}
	if !midB.IsPositive() {
		midB = priceB
	}
	if opening {
		remaining := remainingDirectionNotional(
			parseDecimal(legABase), parseDecimal(legBBase),
			parsePositiveDecimal(targetNotional), midA, midB,
		)
		minOpen := decimal.NewFromInt(arbitrageMinOpenNotional)
		if remaining.LessThan(minOpen) ||
			requested.GreaterThan(remaining) ||
			implied.GreaterThan(remaining) {
			return ArbitrageExecution{}, false, nil
		}
	} else if flattening {
		ownedDirection, ownedOK := arbitrageOwnedCloseDirection(combo)
		venueDirection, venueOK := arbitrageVenueReduceDirection(combo)
		if !ownedOK || !venueOK ||
			!strings.EqualFold(execution.Direction, ownedDirection) ||
			ownedDirection != venueDirection {
			return ArbitrageExecution{}, false, nil
		}
		closeableBase := decimal.Min(
			arbitrageOwnedCloseableBase(combo),
			arbitrageCloseableBase(combo),
		)
		if !closeableBase.IsPositive() || targetBase.GreaterThan(closeableBase) {
			return ArbitrageExecution{}, false, nil
		}
		closeable := decimal.Max(closeableBase.Mul(priceA), closeableBase.Mul(priceB))
		if requested.GreaterThan(closeable) || implied.GreaterThan(closeable) {
			return ArbitrageExecution{}, false, nil
		}
	} else {
		closeableBase := arbitrageCloseableBase(combo)
		closeable := decimal.Zero
		if closeableBase.IsPositive() {
			closeable = decimal.Max(closeableBase.Mul(priceA), closeableBase.Mul(priceB))
		}
		if requested.GreaterThan(closeable) || implied.GreaterThan(closeable) {
			return ArbitrageExecution{}, false, nil
		}
	}
	var hasLiveOrder bool
	err = tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM trader_orders o
			JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
			WHERE e.combination_id=$1::uuid
			  AND o.status NOT IN ('filled','canceled','rejected','expired')
		)`,
		execution.CombinationID,
	).Scan(&hasLiveOrder)
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	if hasLiveOrder {
		return ArbitrageExecution{}, false, nil
	}
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
		ON CONFLICT DO NOTHING
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
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageExecution{}, false, nil
	}
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET version=version+1,updated_at=now()
		WHERE id=$1::uuid`,
		execution.CombinationID,
	); err != nil {
		return ArbitrageExecution{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ArbitrageExecution{}, false, err
	}
	return result, true, nil
}

func (r *Repository) ClaimOneShotFinalFlattenExecution(
	ctx context.Context,
	claim OneShotFinalFlattenClaim,
) (ArbitrageExecution, bool, error) {
	execution := claim.Execution
	if execution.ID == "" {
		execution.ID = uuid.NewString()
	}
	if strings.TrimSpace(execution.CombinationID) == "" {
		return ArbitrageExecution{}, false, ErrInvalidArgument
	}
	claim.Execution = execution
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
		FOR UPDATE`,
		execution.CombinationID,
	).Scan(arbitrageCombinationScanTargets(&combo)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageExecution{}, false, nil
	}
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	if !validateOneShotFinalFlattenClaim(combo, claim, time.Now().UTC()) {
		return ArbitrageExecution{}, false, nil
	}
	var hasLiveOrder bool
	err = tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM trader_orders o
			JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
			WHERE e.combination_id=$1::uuid
			  AND o.status NOT IN ('filled','canceled','rejected','expired')
		)`,
		execution.CombinationID,
	).Scan(&hasLiveOrder)
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	if hasLiveOrder {
		return ArbitrageExecution{}, false, nil
	}
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
		ON CONFLICT DO NOTHING
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
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageExecution{}, false, nil
	}
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET version=version+1,updated_at=now()
		WHERE id=$1::uuid`,
		execution.CombinationID,
	); err != nil {
		return ArbitrageExecution{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ArbitrageExecution{}, false, err
	}
	return result, true, nil
}

func (r *Repository) FinalizeOneShotFinalFlattenExecution(
	ctx context.Context,
	executionID string,
) (oneShotFinalFlattenFinalizeResult, error) {
	if strings.TrimSpace(executionID) == "" {
		return oneShotFinalFlattenFinalizeResult{}, ErrInvalidArgument
	}
	return retryOnDeadlock(ctx, func() (oneShotFinalFlattenFinalizeResult, error) {
		return r.finalizeOneShotFinalFlattenExecutionOnce(ctx, executionID)
	})
}

func (r *Repository) finalizeOneShotFinalFlattenExecutionOnce(
	ctx context.Context,
	executionID string,
) (oneShotFinalFlattenFinalizeResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return oneShotFinalFlattenFinalizeResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var execution ArbitrageExecution
	err = tx.QueryRow(ctx, `
		SELECT `+arbitrageExecutionColumns+`
		FROM trader_arbitrage_executions
		WHERE id=$1::uuid
		FOR UPDATE`,
		executionID,
	).Scan(arbitrageExecutionScanTargets(&execution)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return oneShotFinalFlattenFinalizeResult{}, ErrNotFound
	}
	if err != nil {
		return oneShotFinalFlattenFinalizeResult{}, err
	}

	var combo ArbitrageCombination
	err = tx.QueryRow(ctx, `
		SELECT `+arbitrageCombinationColumns+`
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid
		FOR UPDATE`,
		execution.CombinationID,
	).Scan(arbitrageCombinationScanTargets(&combo)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return oneShotFinalFlattenFinalizeResult{}, ErrNotFound
	}
	if err != nil {
		return oneShotFinalFlattenFinalizeResult{}, err
	}

	rows, err := tx.Query(ctx, `
		SELECT `+orderColumns+`
		FROM trader_orders
		WHERE arbitrage_execution_id=$1::uuid
		ORDER BY created_at ASC`,
		executionID,
	)
	if err != nil {
		return oneShotFinalFlattenFinalizeResult{}, err
	}
	var orders []Order
	for rows.Next() {
		var item Order
		if err := rows.Scan(orderScanTargets(&item)...); err != nil {
			rows.Close()
			return oneShotFinalFlattenFinalizeResult{}, err
		}
		orders = append(orders, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return oneShotFinalFlattenFinalizeResult{}, err
	}
	rows.Close()

	if !oneShotFinalFlattenOrdersTerminal(orders, executionID) {
		return oneShotFinalFlattenFinalizeResult{
			Combination: combo, Execution: execution,
		}, ErrArbitrageReconciling
	}

	if terminalArbitrageExecutionStatus(execution.Status) {
		if err := tx.Commit(ctx); err != nil {
			return oneShotFinalFlattenFinalizeResult{}, err
		}
		return oneShotFinalFlattenFinalizeResult{
			Combination: combo, Execution: execution,
		}, nil
	}

	updated, err := updateArbitrageCombinationPositions(ctx, tx, execution.CombinationID)
	if err != nil {
		return oneShotFinalFlattenFinalizeResult{}, err
	}
	combo = updated
	fillA, fillB, notional := oneShotFinalFlattenFillTotals(orders, executionID)
	status := "completed"
	if !fillA.IsPositive() && !fillB.IsPositive() {
		status = "failed"
	}
	var finalized ArbitrageExecution
	err = tx.QueryRow(ctx, `
		UPDATE trader_arbitrage_executions SET
			status=$2,
			leg_a_filled_quantity=$3::numeric,
			leg_b_filled_quantity=$4::numeric,
			delta_notional=$5::numeric,
			error_message='',
			closed_at=now(),
			updated_at=now()
		WHERE id=$1::uuid
		  AND status NOT IN ('completed','failed','canceled','dry_run')
		RETURNING `+arbitrageExecutionColumns,
		executionID, status, fillA.String(), fillB.String(), notional.String(),
	).Scan(arbitrageExecutionScanTargets(&finalized)...)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return oneShotFinalFlattenFinalizeResult{}, err
		}
		return oneShotFinalFlattenFinalizeResult{
			Combination: combo, Execution: execution,
		}, nil
	}
	if err != nil {
		return oneShotFinalFlattenFinalizeResult{}, err
	}
	if notional.IsPositive() {
		err = tx.QueryRow(ctx, `
			UPDATE trader_arbitrage_combinations SET
				cumulative_turnover_notional=cumulative_turnover_notional+ABS($2::numeric),
				runtime_state=CASE
					WHEN runtime_state IN ('hedging','maker_open','repricing') THEN 'monitoring'
					ELSE runtime_state
				END,
				version=version+1,
				updated_at=now()
			WHERE id=$1::uuid
			RETURNING `+arbitrageCombinationColumns,
			execution.CombinationID, notional.String(),
		).Scan(arbitrageCombinationScanTargets(&combo)...)
		if err != nil {
			return oneShotFinalFlattenFinalizeResult{}, err
		}
	} else {
		err = tx.QueryRow(ctx, `
			UPDATE trader_arbitrage_combinations SET
				runtime_state=CASE
					WHEN runtime_state IN ('hedging','maker_open','repricing') THEN 'monitoring'
					ELSE runtime_state
				END,
				version=version+1,
				updated_at=now()
			WHERE id=$1::uuid
			RETURNING `+arbitrageCombinationColumns,
			execution.CombinationID,
		).Scan(arbitrageCombinationScanTargets(&combo)...)
		if err != nil {
			return oneShotFinalFlattenFinalizeResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return oneShotFinalFlattenFinalizeResult{}, err
	}
	return oneShotFinalFlattenFinalizeResult{
		Combination: combo, Execution: finalized, Applied: true,
	}, nil
}

func (r *Repository) GetActiveArbitrageExecution(
	ctx context.Context,
	combinationID string,
) (ArbitrageExecution, error) {
	var item ArbitrageExecution
	err := r.pool.QueryRow(ctx, `
		SELECT `+arbitrageExecutionColumns+`
		FROM trader_arbitrage_executions
		WHERE combination_id=$1::uuid
		  AND status NOT IN ('completed','failed','canceled','dry_run')`,
		combinationID,
	).Scan(arbitrageExecutionScanTargets(&item)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageExecution{}, ErrNotFound
	}
	return item, err
}

func (r *Repository) PrepareArbitrageHedgeIntent(
	ctx context.Context,
	in PrepareArbitrageHedgeIntentInput,
) (PrepareArbitrageHedgeIntentResult, error) {
	if in.Execution.ID == "" || in.Order.IdempotencyKey == "" {
		return PrepareArbitrageHedgeIntentResult{}, ErrInvalidArgument
	}
	combinationID := strings.TrimSpace(in.Combination.ID)
	if combinationID == "" {
		combinationID = strings.TrimSpace(in.Execution.CombinationID)
	}
	if combinationID == "" {
		return PrepareArbitrageHedgeIntentResult{}, ErrInvalidArgument
	}
	order := in.Order
	if order.ID == "" {
		order.ID = uuid.NewString()
	}
	if order.ClientOrderID == "" {
		order.ClientOrderID = clientOrderID()
	}
	in.Combination.ID = combinationID
	in.Order = order
	return retryOnDeadlock(ctx, func() (PrepareArbitrageHedgeIntentResult, error) {
		return r.prepareArbitrageHedgeIntentOnce(ctx, in)
	})
}

func (r *Repository) prepareArbitrageHedgeIntentOnce(
	ctx context.Context,
	in PrepareArbitrageHedgeIntentInput,
) (PrepareArbitrageHedgeIntentResult, error) {
	order := in.Order
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return PrepareArbitrageHedgeIntentResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockArbitrageCombination(ctx, tx, in.Combination.ID); err != nil {
		return PrepareArbitrageHedgeIntentResult{}, err
	}

	var execution ArbitrageExecution
	err = tx.QueryRow(ctx, `
		SELECT `+arbitrageExecutionColumns+`
		FROM trader_arbitrage_executions
		WHERE id=$1::uuid
		FOR UPDATE`, in.Execution.ID,
	).Scan(arbitrageExecutionScanTargets(&execution)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return PrepareArbitrageHedgeIntentResult{}, ErrNotFound
	}
	if err != nil {
		return PrepareArbitrageHedgeIntentResult{}, err
	}
	if execution.CombinationID != in.Combination.ID {
		return PrepareArbitrageHedgeIntentResult{}, ErrInvalidArgument
	}
	if terminalArbitrageExecutionStatus(execution.Status) {
		return PrepareArbitrageHedgeIntentResult{}, ErrArbitrageExecutionTerminal
	}
	if in.FastPathAdmission {
		if err := admitFastPathHedgeIntent(ctx, tx, in, &execution); err != nil {
			return PrepareArbitrageHedgeIntentResult{}, err
		}
	}
	if in.AggregateCarry {
		if err := admitAggregateCarry(ctx, tx, in, &execution); err != nil {
			return PrepareArbitrageHedgeIntentResult{}, err
		}
	}

	var existing Order
	existingErr := tx.QueryRow(ctx, `
		SELECT `+orderColumns+`
		FROM trader_orders
		WHERE idempotency_key=$1
		FOR UPDATE`, order.IdempotencyKey,
	).Scan(orderScanTargets(&existing)...)
	created := false
	stored := existing
	if errors.Is(existingErr, pgx.ErrNoRows) {
		hedgeOrderID := strings.TrimSpace(execution.HedgeOrderID)
		if in.FastPathAdmission && hedgeOrderID != "" {
			return PrepareArbitrageHedgeIntentResult{}, fmt.Errorf(
				"%w: hedge order already set", ErrArbitrageHedgeAdmissionConflict,
			)
		}
		if in.AggregateCarry && hedgeOrderID != "" {
			if err := admitAggregateCarryHedgeReplacement(
				ctx, tx, execution, order.ArbitrageLeg,
			); err != nil {
				return PrepareArbitrageHedgeIntentResult{}, err
			}
		}
		if execution.HedgeSequence != in.ExpectedSequence {
			return PrepareArbitrageHedgeIntentResult{}, fmt.Errorf(
				"%w: expected %d have %d",
				ErrArbitrageHedgeSequence, in.ExpectedSequence, execution.HedgeSequence,
			)
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO trader_orders (
				id,idempotency_key,owner_username,trading_account_id,product_name,
				exchange,instrument_id,contract_type,exchange_symbol,client_order_id,
				side,order_type,quantity,price,status,request_fingerprint,base_asset,quote_asset,
				twap_job_id,twap_slice_index,twap_attempt_index,
				arbitrage_execution_id,arbitrage_leg,arbitrage_role,reduce_only
			) VALUES (
				$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NULLIF($14,'')::numeric,
				'pending',$15,$16,$17,NULL,NULL,0,NULLIF($18,'')::uuid,NULLIF($19,''),NULLIF($20,''),$21
			)
			RETURNING `+orderColumns, orderWriteArgs(order)...,
		).Scan(orderScanTargets(&stored)...)
		if err != nil {
			return PrepareArbitrageHedgeIntentResult{}, err
		}
		created = true
		intentPayload, _ := json.Marshal(redactPayload(map[string]any{
			"side": stored.Side, "orderType": stored.OrderType, "quantity": stored.Quantity,
			"price": stored.Price, "instrumentId": stored.InstrumentID,
		}))
		if _, err := tx.Exec(ctx, `
			INSERT INTO trader_order_events(order_id,event_type,payload)
			VALUES($1::uuid,'intent',$2::jsonb)`, stored.ID, intentPayload); err != nil {
			return PrepareArbitrageHedgeIntentResult{}, err
		}
		submittedPayload, _ := json.Marshal(redactPayload(map[string]any{
			"exchange": stored.Exchange, "symbol": stored.ExchangeSymbol,
			"clientOrderId": stored.ClientOrderID,
		}))
		if _, err := tx.Exec(ctx, `
			INSERT INTO trader_order_events(order_id,event_type,payload)
			VALUES($1::uuid,'submitted',$2::jsonb)`, stored.ID, submittedPayload); err != nil {
			return PrepareArbitrageHedgeIntentResult{}, err
		}
		execution.HedgeSequence++
	} else if existingErr != nil {
		return PrepareArbitrageHedgeIntentResult{}, existingErr
	} else if stored.ArbitrageExecutionID != execution.ID ||
		stored.ArbitrageRole != order.ArbitrageRole ||
		stored.ArbitrageLeg != order.ArbitrageLeg ||
		stored.RequestFingerprint != order.RequestFingerprint {
		return PrepareArbitrageHedgeIntentResult{}, ErrIdempotencyConflict
	}

	previousStatus := execution.Status
	execution.HedgeOrderID = stored.ID
	execution.Status = "hedging"
	execution.ErrorMessage = ""
	var updatedExecution ArbitrageExecution
	err = tx.QueryRow(ctx, `
		UPDATE trader_arbitrage_executions SET
			status=$2,leg_a_filled_quantity=$3::numeric,leg_b_filled_quantity=$4::numeric,
			delta_notional=$5::numeric,maker_order_id=NULLIF($6,'')::uuid,
			hedge_order_id=NULLIF($7,'')::uuid,hedge_sequence=$8,attempt=$9,error_message=$10,
			updated_at=now()
		WHERE id=$1::uuid
		  AND status NOT IN ('completed','failed','canceled','dry_run')
		RETURNING `+arbitrageExecutionColumns,
		execution.ID, execution.Status, zeroString(execution.LegAFilledQuantity),
		zeroString(execution.LegBFilledQuantity), zeroString(execution.DeltaNotional),
		execution.MakerOrderID, execution.HedgeOrderID, execution.HedgeSequence,
		execution.Attempt, execution.ErrorMessage,
	).Scan(arbitrageExecutionScanTargets(&updatedExecution)...)
	if err != nil {
		return PrepareArbitrageHedgeIntentResult{}, err
	}

	var combination ArbitrageCombination
	err = tx.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			error_message=CASE
				WHEN position_uncertain OR runtime_state IN (
					'position_uncertain','closing','manual_intervention'
				) OR status IN ('closing','closed','failed') THEN error_message
				ELSE ''
			END,
			runtime_state=CASE
				WHEN position_uncertain OR runtime_state IN (
					'position_uncertain','closing','manual_intervention'
				) OR status IN ('closing','closed','failed') THEN runtime_state
				ELSE 'hedging'
			END,
			version=version+1,
			updated_at=now()
		WHERE id=$1::uuid
		RETURNING `+arbitrageCombinationColumns, in.Combination.ID,
	).Scan(arbitrageCombinationScanTargets(&combination)...)
	if err != nil {
		return PrepareArbitrageHedgeIntentResult{}, err
	}

	if created && previousStatus != "hedging" {
		payload, _ := json.Marshal(redactPayload(map[string]any{
			"leg": in.HedgeLeg.InstrumentID, "side": in.HedgeSide,
			"targetBaseQuantity": in.TargetQuantity,
			"carryBaseQuantity":  in.CarryQuantity,
		}))
		if _, err := tx.Exec(ctx, `
			INSERT INTO trader_arbitrage_events(combination_id,execution_id,event_type,payload)
			VALUES($1::uuid,$2::uuid,'hedge_started',$3::jsonb)`,
			in.Combination.ID, execution.ID, payload); err != nil {
			return PrepareArbitrageHedgeIntentResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return PrepareArbitrageHedgeIntentResult{}, err
	}
	return PrepareArbitrageHedgeIntentResult{
		Order: stored, Created: created, Execution: updatedExecution, Combination: combination,
	}, nil
}

func requireUniqueActiveExecution(
	ctx context.Context,
	tx pgx.Tx,
	combinationID, executionID string,
) error {
	rows, err := tx.Query(ctx, `
		SELECT id::text
		FROM trader_arbitrage_executions
		WHERE combination_id=$1::uuid
		  AND status NOT IN ('completed','failed','canceled','dry_run')
		FOR UPDATE`, combinationID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var activeIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		activeIDs = append(activeIDs, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(activeIDs) != 1 || activeIDs[0] != executionID {
		return fmt.Errorf("%w: active execution", ErrArbitrageHedgeAdmissionConflict)
	}
	return nil
}

func admitFastPathHedgeIntent(
	ctx context.Context,
	tx pgx.Tx,
	in PrepareArbitrageHedgeIntentInput,
	execution *ArbitrageExecution,
) error {
	if err := requireUniqueActiveExecution(ctx, tx, in.Combination.ID, execution.ID); err != nil {
		return err
	}
	switch execution.Status {
	case "maker_open", "maker_canceling", "hedging":
	default:
		return fmt.Errorf(
			"%w: status %s", ErrArbitrageHedgeAdmissionConflict, execution.Status,
		)
	}
	makerID := strings.TrimSpace(execution.MakerOrderID)
	inputMakerID := strings.TrimSpace(in.Execution.MakerOrderID)
	if makerID == "" || (inputMakerID != "" && inputMakerID != makerID) {
		return fmt.Errorf("%w: maker order id", ErrArbitrageHedgeAdmissionConflict)
	}
	var maker Order
	err := tx.QueryRow(ctx, `
		SELECT `+orderColumns+`
		FROM trader_orders
		WHERE id=$1::uuid
		FOR UPDATE`, makerID,
	).Scan(orderScanTargets(&maker)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: maker order not found", ErrArbitrageHedgeAdmissionConflict)
	}
	if err != nil {
		return err
	}
	if maker.ArbitrageExecutionID != execution.ID ||
		maker.ArbitrageRole != "maker" ||
		!terminalStatus(maker.Status) {
		return fmt.Errorf("%w: maker order", ErrArbitrageHedgeAdmissionConflict)
	}
	authoritativeMaker := parseDecimal(maker.FilledQuantity)
	if !authoritativeMaker.Equal(parseDecimal(in.ConfirmedMakerFilled)) {
		return fmt.Errorf("%w: maker fill mismatch", ErrArbitrageHedgeAdmissionConflict)
	}
	hedgeFilled := executionHedgeFilled(*execution, in.Combination.MakerLeg)
	if !hedgeFilled.Equal(parseDecimal(in.ConfirmedHedgeFilled)) {
		return fmt.Errorf("%w: hedge fill mismatch", ErrArbitrageHedgeAdmissionConflict)
	}
	setExecutionMakerFilled(execution, in.Combination.MakerLeg, authoritativeMaker)
	return nil
}

func admitAggregateCarry(
	ctx context.Context,
	tx pgx.Tx,
	in PrepareArbitrageHedgeIntentInput,
	execution *ArbitrageExecution,
) error {
	if err := requireUniqueActiveExecution(ctx, tx, in.Combination.ID, execution.ID); err != nil {
		return err
	}
	makerID := strings.TrimSpace(execution.MakerOrderID)
	if makerID != "" {
		var maker Order
		err := tx.QueryRow(ctx, `
			SELECT `+orderColumns+`
			FROM trader_orders
			WHERE id=$1::uuid
			FOR UPDATE`, makerID,
		).Scan(orderScanTargets(&maker)...)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: maker order not found", ErrArbitrageHedgeAdmissionConflict)
		}
		if err != nil {
			return err
		}
		if maker.ArbitrageExecutionID != execution.ID ||
			maker.ArbitrageRole != "maker" ||
			!terminalStatus(maker.Status) {
			return fmt.Errorf("%w: maker order", ErrArbitrageHedgeAdmissionConflict)
		}
	}
	var carry string
	err := tx.QueryRow(ctx, `
		SELECT carry_base_quantity::text
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid`, in.Combination.ID,
	).Scan(&carry)
	if err != nil {
		return err
	}
	expected := parseDecimal(in.ExpectedCarryQuantity)
	if strings.TrimSpace(in.ExpectedCarryQuantity) == "" || !parseDecimal(carry).Equal(expected) {
		return fmt.Errorf("%w: carry mismatch", ErrArbitrageHedgeAdmissionConflict)
	}
	if !parseDecimal(in.Order.Quantity).IsPositive() ||
		parseDecimal(in.Order.Quantity).GreaterThan(expected.Abs()) {
		return fmt.Errorf("%w: hedge quantity", ErrArbitrageHedgeAdmissionConflict)
	}
	return nil
}

func admitAggregateCarryHedgeReplacement(
	ctx context.Context,
	tx pgx.Tx,
	execution ArbitrageExecution,
	hedgeLeg string,
) error {
	var previous Order
	err := tx.QueryRow(ctx, `
		SELECT `+orderColumns+`
		FROM trader_orders
		WHERE id=$1::uuid
		FOR UPDATE`, strings.TrimSpace(execution.HedgeOrderID),
	).Scan(orderScanTargets(&previous)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: previous hedge not found", ErrArbitrageHedgeAdmissionConflict)
	}
	if err != nil {
		return err
	}
	if previous.ArbitrageExecutionID != execution.ID ||
		!isArbitrageCarryHedgeRole(previous.ArbitrageRole) ||
		previous.ArbitrageLeg != hedgeLeg ||
		!terminalStatus(previous.Status) ||
		previous.ReconcileFailures != 0 {
		return fmt.Errorf("%w: previous hedge not replaceable", ErrArbitrageHedgeAdmissionConflict)
	}
	rows, err := tx.Query(ctx, `
		SELECT id
		FROM trader_orders
		WHERE arbitrage_execution_id = $1::uuid
		  AND arbitrage_role IN ('hedge','residual')
		  AND status NOT IN ('filled','canceled','rejected','expired')
		FOR UPDATE`, execution.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("%w: active hedge exists", ErrArbitrageHedgeAdmissionConflict)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

func executionHedgeFilled(execution ArbitrageExecution, makerLeg string) decimal.Decimal {
	if strings.EqualFold(strings.TrimSpace(makerLeg), "b") {
		return parseDecimal(execution.LegAFilledQuantity)
	}
	return parseDecimal(execution.LegBFilledQuantity)
}

func setExecutionMakerFilled(execution *ArbitrageExecution, makerLeg string, filled decimal.Decimal) {
	if strings.EqualFold(strings.TrimSpace(makerLeg), "b") {
		execution.LegBFilledQuantity = filled.String()
		return
	}
	execution.LegAFilledQuantity = filled.String()
}

func (r *Repository) UpdateArbitrageExecution(
	ctx context.Context,
	item ArbitrageExecution,
) (ArbitrageExecution, error) {
	var updated ArbitrageExecution
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_executions SET
			status=$2,leg_a_filled_quantity=$3::numeric,leg_b_filled_quantity=$4::numeric,
			delta_notional=$5::numeric,maker_order_id=NULLIF($6,'')::uuid,
			hedge_order_id=NULLIF($7,'')::uuid,hedge_sequence=$8,attempt=$9,error_message=$10,
			updated_at=now(),
			closed_at=CASE
				WHEN $2 IN ('completed','failed','canceled','dry_run') THEN COALESCE(closed_at,now())
				ELSE closed_at
			END
		WHERE id=$1::uuid
		  AND (
			status NOT IN ('completed','failed','canceled','dry_run')
			OR $2 IN ('completed','failed','canceled','dry_run')
		  )
		RETURNING `+arbitrageExecutionColumns,
		item.ID, item.Status, zeroString(item.LegAFilledQuantity),
		zeroString(item.LegBFilledQuantity), zeroString(item.DeltaNotional),
		item.MakerOrderID, item.HedgeOrderID, item.HedgeSequence, item.Attempt, item.ErrorMessage,
	).Scan(arbitrageExecutionScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		var current string
		lookupErr := r.pool.QueryRow(ctx, `
			SELECT status FROM trader_arbitrage_executions WHERE id=$1::uuid`,
			item.ID,
		).Scan(&current)
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			return ArbitrageExecution{}, ErrNotFound
		}
		if lookupErr != nil {
			return ArbitrageExecution{}, lookupErr
		}
		if terminalArbitrageExecutionStatus(current) &&
			!terminalArbitrageExecutionStatus(item.Status) {
			return ArbitrageExecution{}, ErrArbitrageExecutionTerminal
		}
		return ArbitrageExecution{}, ErrNotFound
	}
	return updated, err
}

func (r *Repository) FailLastCloseClipUnbalanced(
	ctx context.Context,
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	fillA, fillB, remaining string,
) (FailLastCloseClipUnbalancedResult, error) {
	if strings.TrimSpace(combination.ID) == "" || strings.TrimSpace(execution.ID) == "" {
		return FailLastCloseClipUnbalancedResult{}, ErrInvalidArgument
	}
	return retryOnDeadlock(ctx, func() (FailLastCloseClipUnbalancedResult, error) {
		return r.failLastCloseClipUnbalancedOnce(ctx, combination, execution, fillA, fillB, remaining)
	})
}

func (r *Repository) failLastCloseClipUnbalancedOnce(
	ctx context.Context,
	combination ArbitrageCombination,
	execution ArbitrageExecution,
	fillA, fillB, remaining string,
) (FailLastCloseClipUnbalancedResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return FailLastCloseClipUnbalancedResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockArbitrageCombination(ctx, tx, combination.ID); err != nil {
		return FailLastCloseClipUnbalancedResult{}, err
	}

	var locked ArbitrageExecution
	err = tx.QueryRow(ctx, `
		SELECT `+arbitrageExecutionColumns+`
		FROM trader_arbitrage_executions
		WHERE id=$1::uuid
		FOR UPDATE`, execution.ID,
	).Scan(arbitrageExecutionScanTargets(&locked)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return FailLastCloseClipUnbalancedResult{}, ErrNotFound
	}
	if err != nil {
		return FailLastCloseClipUnbalancedResult{}, err
	}
	if locked.CombinationID != combination.ID {
		return FailLastCloseClipUnbalancedResult{}, ErrInvalidArgument
	}

	var hasEvent bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM trader_arbitrage_events
			WHERE execution_id=$1::uuid
			  AND event_type='last_close_clip_unbalanced'
		)`, locked.ID,
	).Scan(&hasEvent); err != nil {
		return FailLastCloseClipUnbalancedResult{}, err
	}

	var currentCombo ArbitrageCombination
	err = tx.QueryRow(ctx, `
		SELECT `+arbitrageCombinationColumns+`
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid`, combination.ID,
	).Scan(arbitrageCombinationScanTargets(&currentCombo)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return FailLastCloseClipUnbalancedResult{}, ErrNotFound
	}
	if err != nil {
		return FailLastCloseClipUnbalancedResult{}, err
	}

	if locked.Status == "failed" && hasEvent {
		if err := tx.Commit(ctx); err != nil {
			return FailLastCloseClipUnbalancedResult{}, err
		}
		return FailLastCloseClipUnbalancedResult{
			Combination: currentCombo, Execution: locked,
		}, nil
	}

	message := lastCloseClipUnbalancedErrorMessage(locked.ID, fillA, fillB, remaining)
	var updatedCombo ArbitrageCombination
	err = tx.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			runtime_state='manual_intervention',
			error_message=$3,
			circuit_open=TRUE,
			version=version+1,
			updated_at=now()
		WHERE id=$1::uuid
		  AND version=$2
		  AND status IN ('running','closing')
		RETURNING `+arbitrageCombinationColumns,
		combination.ID, combination.Version, message,
	).Scan(arbitrageCombinationScanTargets(&updatedCombo)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return FailLastCloseClipUnbalancedResult{}, ErrArbitrageLastCloseClipUnbalancedConflict
	}
	if err != nil {
		return FailLastCloseClipUnbalancedResult{}, err
	}

	var updatedExecution ArbitrageExecution
	err = tx.QueryRow(ctx, `
		UPDATE trader_arbitrage_executions SET
			status='failed',
			error_message=$2,
			updated_at=now(),
			closed_at=COALESCE(closed_at,now())
		WHERE id=$1::uuid
		  AND status NOT IN ('completed','failed','canceled','dry_run')
		RETURNING `+arbitrageExecutionColumns,
		locked.ID, message,
	).Scan(arbitrageExecutionScanTargets(&updatedExecution)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return FailLastCloseClipUnbalancedResult{}, ErrArbitrageLastCloseClipUnbalancedConflict
	}
	if err != nil {
		return FailLastCloseClipUnbalancedResult{}, err
	}

	payload, err := json.Marshal(redactPayload(lastCloseClipUnbalancedPayload(fillA, fillB, remaining)))
	if err != nil {
		return FailLastCloseClipUnbalancedResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO trader_arbitrage_events(combination_id,execution_id,event_type,payload)
		SELECT $1::uuid,$2::uuid,'last_close_clip_unbalanced',$3::jsonb
		WHERE NOT EXISTS (
			SELECT 1
			FROM trader_arbitrage_events
			WHERE execution_id=$2::uuid
			  AND event_type='last_close_clip_unbalanced'
		)`, combination.ID, locked.ID, payload); err != nil {
		return FailLastCloseClipUnbalancedResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return FailLastCloseClipUnbalancedResult{}, err
	}
	return FailLastCloseClipUnbalancedResult{
		Combination: updatedCombo, Execution: updatedExecution,
	}, nil
}

func (r *Repository) UpdateArbitrageCombinationRuntime(
	ctx context.Context,
	item ArbitrageCombination,
) (ArbitrageCombination, error) {
	var updated ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			status=CASE
				WHEN status IN ('closing','closed','failed') AND $2='running' THEN status
				ELSE $2
			END,
			current_ask_spread_bps=NULLIF($3,'')::numeric,
			current_bid_spread_bps=NULLIF($4,'')::numeric,
			market_data_stale=$5,
			error_message=CASE
				WHEN $2 NOT IN ('closed','failed')
				  AND NOT $8
				  AND runtime_state IN ('closing','manual_intervention')
				  AND $7<>runtime_state THEN error_message
				WHEN $8 OR NOT position_uncertain THEN $6
				ELSE error_message
			END,
			runtime_state=CASE
				WHEN $2 NOT IN ('closed','failed')
				  AND NOT $8
				  AND runtime_state IN ('closing','manual_intervention')
				  AND $7<>runtime_state THEN runtime_state
				WHEN $8 OR NOT position_uncertain THEN $7
				ELSE runtime_state
			END,
			position_uncertain=position_uncertain OR $8,
			consecutive_failures=CASE WHEN $2='closed' THEN 0 ELSE consecutive_failures END,
			repeated_failure_count=CASE WHEN $2='closed' THEN 0 ELSE repeated_failure_count END,
			last_failure_key=CASE WHEN $2='closed' THEN '' ELSE last_failure_key END,
			next_retry_at=CASE WHEN $2='closed' THEN '-infinity'::timestamptz ELSE next_retry_at END,
			circuit_open=CASE
				WHEN $2='closed' THEN FALSE
				ELSE circuit_open OR $7='manual_intervention'
			END,
			version=version+1,updated_at=now(),
			closed_at=CASE WHEN $2 IN ('closed','failed') THEN COALESCE(closed_at,now()) ELSE closed_at END
		WHERE id=$1::uuid
		RETURNING `+arbitrageCombinationColumns,
		item.ID, item.Status, item.CurrentAskSpreadBps, item.CurrentBidSpreadBps,
		item.MarketDataStale, item.ErrorMessage, arbitrageRuntimeState(item.RuntimeState),
		item.PositionUncertain,
	).Scan(arbitrageCombinationScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrNotFound
	}
	return updated, err
}

func (r *Repository) RecordArbitrageCloseFailure(
	ctx context.Context,
	id, failureKey, errorMessage string,
) (ArbitrageCombination, error) {
	var updated ArbitrageCombination
	failureKey = truncateMessage(failureKey)
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			error_message=$3,
			consecutive_failures=consecutive_failures+1,
			repeated_failure_count=CASE
				WHEN last_failure_key=$2 THEN repeated_failure_count+1 ELSE 1 END,
			last_failure_key=$2,
			next_retry_at=now() + make_interval(secs => CASE consecutive_failures
				WHEN 0 THEN 2
				WHEN 1 THEN 4
				WHEN 2 THEN 8
				WHEN 3 THEN 16
				WHEN 4 THEN 30
				ELSE 60
			END),
			runtime_state=CASE
				WHEN status='closing' THEN 'closing'
				ELSE 'backoff'
			END,
			combined_position_annualized=CASE
				WHEN $2='final_metrics_not_ready' THEN NULL
				ELSE combined_position_annualized
			END,
			metrics_quality=CASE
				WHEN $2='final_metrics_not_ready' THEN 'partial'
				ELSE metrics_quality
			END,
			version=version+1,
			updated_at=now()
		WHERE id=$1::uuid
		  AND NOT position_uncertain
		  AND runtime_state<>'manual_intervention'
		  AND (
			status='closing'
			OR (
				status='running'
				AND run_mode='one_shot'
				AND one_shot_phase='exiting'
			)
		  )
		RETURNING `+arbitrageCombinationColumns,
		id, failureKey, truncateMessage(errorMessage),
	).Scan(arbitrageCombinationScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrNotFound
	}
	return updated, err
}

func (r *Repository) UpdateArbitrageMarketSnapshot(
	ctx context.Context,
	id, askSpread, bidSpread string,
	stale bool,
) (arbitrageMarketSnapshot, error) {
	var updated arbitrageMarketSnapshot
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			current_ask_spread_bps=NULLIF($2,'')::numeric,
			current_bid_spread_bps=NULLIF($3,'')::numeric,
			market_data_stale=$4,
			runtime_state=CASE
				WHEN runtime_state NOT IN (
					'backoff','hedge_deferred_dust','position_uncertain',
					'manual_intervention','closing'
				) AND NOT EXISTS (
					SELECT 1 FROM trader_arbitrage_executions e
					WHERE e.combination_id=trader_arbitrage_combinations.id
					  AND e.status NOT IN ('completed','failed','canceled','dry_run')
				) THEN 'monitoring'
				ELSE runtime_state
			END,
			updated_at=now()
		WHERE id=$1::uuid AND status IN ('running','closing')
		RETURNING COALESCE(current_ask_spread_bps::text,''),
		          COALESCE(current_bid_spread_bps::text,''),
		          market_data_stale,runtime_state,status,version,updated_at`,
		id, askSpread, bidSpread, stale,
	).Scan(
		&updated.CurrentAskSpreadBps, &updated.CurrentBidSpreadBps,
		&updated.MarketDataStale, &updated.RuntimeState, &updated.Status,
		&updated.Version, &updated.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return arbitrageMarketSnapshot{}, ErrNotFound
	}
	return updated, err
}

func (r *Repository) ClearArbitrageFailureIfUnchanged(
	ctx context.Context,
	id, expectedError string,
) (ArbitrageCombination, error) {
	var updated ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			error_message='',
			consecutive_failures=0,
			last_failure_key='',
			repeated_failure_count=0,
			next_retry_at='-infinity',
			runtime_state=CASE WHEN runtime_state='backoff' THEN 'monitoring' ELSE runtime_state END,
			version=version+1,updated_at=now()
		WHERE id=$1::uuid
		  AND error_message=$2
		  AND NOT position_uncertain
		  AND NOT circuit_open
		  AND runtime_state NOT IN ('manual_intervention','position_uncertain')
		RETURNING `+arbitrageCombinationColumns,
		id, expectedError,
	).Scan(arbitrageCombinationScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrNotFound
	}
	return updated, err
}

func (r *Repository) AddArbitragePositionDelta(
	ctx context.Context,
	id, delta string,
) (ArbitrageCombination, error) {
	var updated ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			position_notional=position_notional+$2::numeric,
			cumulative_turnover_notional=cumulative_turnover_notional+ABS($2::numeric),
			consecutive_failures=0,
			next_retry_at='-infinity',
			error_message=CASE WHEN position_uncertain THEN error_message ELSE '' END,
			version=version+1,updated_at=now()
		WHERE id=$1::uuid AND status IN ('running','closing')
		RETURNING `+arbitrageCombinationColumns,
		id, delta,
	).Scan(arbitrageCombinationScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrNotFound
	}
	return updated, err
}

func (r *Repository) UpdateArbitragePositionFromBase(
	ctx context.Context,
	id, mark, turnoverDelta string,
) (ArbitrageCombination, error) {
	var updated ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			position_notional=CASE
				WHEN leg_a_base_position>0 AND leg_b_base_position<0
					THEN LEAST(leg_a_base_position,-leg_b_base_position)*$2::numeric
				WHEN leg_a_base_position<0 AND leg_b_base_position>0
					THEN -LEAST(-leg_a_base_position,leg_b_base_position)*$2::numeric
				ELSE 0
			END,
			cumulative_turnover_notional=
				cumulative_turnover_notional+ABS($3::numeric),
			consecutive_failures=0,
			next_retry_at='-infinity',
			last_failure_key='',
			repeated_failure_count=0,
			circuit_open=FALSE,
			error_message=CASE WHEN position_uncertain THEN error_message ELSE '' END,
			version=version+1,updated_at=now()
		WHERE id=$1::uuid AND status IN ('running','closing')
		RETURNING `+arbitrageCombinationColumns,
		id, mark, turnoverDelta,
	).Scan(arbitrageCombinationScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrNotFound
	}
	return updated, err
}

func (r *Repository) RecomputeArbitrageBasePositions(
	ctx context.Context,
	id string,
) (ArbitrageCombination, error) {
	if strings.TrimSpace(id) == "" {
		return ArbitrageCombination{}, ErrInvalidArgument
	}
	return retryOnDeadlock(ctx, func() (ArbitrageCombination, error) {
		return r.recomputeArbitrageBasePositionsOnce(ctx, id)
	})
}

func (r *Repository) recomputeArbitrageBasePositionsOnce(
	ctx context.Context,
	id string,
) (ArbitrageCombination, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockArbitrageCombination(ctx, tx, id); err != nil {
		return ArbitrageCombination{}, err
	}
	updated, err := updateArbitrageCombinationPositions(ctx, tx, id)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ArbitrageCombination{}, err
	}
	return updated, nil
}

func (r *Repository) RecomputeArbitrageBasePositionsForExecution(
	ctx context.Context,
	executionID string,
) (ArbitrageCombination, error) {
	if strings.TrimSpace(executionID) == "" {
		return ArbitrageCombination{}, ErrInvalidArgument
	}
	return retryOnDeadlock(ctx, func() (ArbitrageCombination, error) {
		return r.recomputeArbitrageBasePositionsForExecutionOnce(ctx, executionID)
	})
}

func (r *Repository) recomputeArbitrageBasePositionsForExecutionOnce(
	ctx context.Context,
	executionID string,
) (ArbitrageCombination, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var combinationID string
	err = tx.QueryRow(ctx, `
		SELECT c.id::text
		FROM trader_arbitrage_combinations c
		JOIN trader_arbitrage_executions e ON e.combination_id=c.id
		WHERE e.id=$1::uuid
		FOR UPDATE OF c`, executionID,
	).Scan(&combinationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrNotFound
	}
	if err != nil {
		return ArbitrageCombination{}, err
	}

	var lockedExecutionID string
	err = tx.QueryRow(ctx, `
		SELECT id::text
		FROM trader_arbitrage_executions
		WHERE id=$1::uuid
		FOR UPDATE`, executionID,
	).Scan(&lockedExecutionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrNotFound
	}
	if err != nil {
		return ArbitrageCombination{}, err
	}

	if _, err := tx.Exec(ctx, `
		WITH combo AS (
			SELECT e.id AS execution_id,
			       c.status,
			       COALESCE(NULLIF(lower(trim(c.maker_leg)), ''), 'a') AS maker_leg,
			       lower(trim(e.direction)) AS direction,
			       e.last_close_clip,
			       e.position_effect,
			       e.reduce_only,
			       lower(trim(c.execution_mode)) AS execution_mode
			FROM trader_arbitrage_executions e
			JOIN trader_arbitrage_combinations c ON c.id=e.combination_id
			WHERE e.id=$1::uuid
		),
		legs AS (
			SELECT
				execution_id,
				maker_leg,
				CASE WHEN maker_leg='b' THEN 'a' ELSE 'b' END AS hedge_leg,
				CASE
					WHEN direction='ask' AND maker_leg='b' THEN 'buy'
					WHEN direction='ask' THEN 'sell'
					WHEN direction='bid' AND maker_leg='b' THEN 'sell'
					WHEN direction='bid' THEN 'buy'
					ELSE ''
				END AS hedge_side,
				(
					lower(trim(status))='closing'
					AND last_close_clip
					AND lower(trim(position_effect))='close'
					AND reduce_only
				) AS last_close_closing,
				execution_mode
			FROM combo
		),
		agg AS (
			SELECT
				l.execution_id,
				l.maker_leg,
				l.last_close_closing,
				l.execution_mode,
				COALESCE(SUM(o.filled_quantity) FILTER (
					WHERE lower(trim(o.arbitrage_role))='maker'
					  AND lower(trim(o.arbitrage_leg))=l.maker_leg
					  AND o.filled_quantity>0
				),0) AS maker_fill,
				COALESCE(SUM(o.filled_quantity) FILTER (
					WHERE lower(trim(o.arbitrage_role))='hedge'
					  AND lower(trim(o.arbitrage_leg))=l.hedge_leg
					  AND lower(trim(o.side))=l.hedge_side
					  AND o.filled_quantity>0
				),0) AS hedge_fill,
				COALESCE(SUM(o.filled_quantity) FILTER (
					WHERE lower(trim(o.arbitrage_role))='residual'
					  AND lower(trim(o.arbitrage_leg))=l.hedge_leg
					  AND lower(trim(o.side))=l.hedge_side
					  AND o.filled_quantity>0
				),0) AS residual_fill,
				COALESCE(SUM(o.filled_quantity) FILTER (
					WHERE o.arbitrage_leg='a'
					  AND o.filled_quantity>0
					  AND o.arbitrage_role IS DISTINCT FROM 'residual'
				),0) AS paired_a,
				COALESCE(SUM(o.filled_quantity) FILTER (
					WHERE o.arbitrage_leg='b'
					  AND o.filled_quantity>0
					  AND o.arbitrage_role IS DISTINCT FROM 'residual'
				),0) AS paired_b,
				COALESCE(SUM(o.filled_quantity) FILTER (
					WHERE lower(trim(o.arbitrage_role))='market'
					  AND lower(trim(o.arbitrage_leg))='a'
					  AND o.filled_quantity>0
				),0) AS market_a,
				COALESCE(SUM(o.filled_quantity) FILTER (
					WHERE lower(trim(o.arbitrage_role))='market'
					  AND lower(trim(o.arbitrage_leg))='b'
					  AND o.filled_quantity>0
				),0) AS market_b
			FROM legs l
			LEFT JOIN trader_orders o ON o.arbitrage_execution_id=l.execution_id
			GROUP BY l.execution_id, l.maker_leg, l.last_close_closing, l.execution_mode
		)
		UPDATE trader_arbitrage_executions e SET
			leg_a_filled_quantity=CASE
				WHEN a.last_close_closing AND a.execution_mode='simultaneous_market' THEN a.market_a
				WHEN a.last_close_closing AND a.maker_leg='a' THEN a.maker_fill
				WHEN a.last_close_closing AND a.maker_leg='b' THEN
					a.hedge_fill + LEAST(a.residual_fill, GREATEST(a.maker_fill - a.hedge_fill, 0))
				ELSE a.paired_a
			END,
			leg_b_filled_quantity=CASE
				WHEN a.last_close_closing AND a.execution_mode='simultaneous_market' THEN a.market_b
				WHEN a.last_close_closing AND a.maker_leg='b' THEN a.maker_fill
				WHEN a.last_close_closing AND a.maker_leg='a' THEN
					a.hedge_fill + LEAST(a.residual_fill, GREATEST(a.maker_fill - a.hedge_fill, 0))
				ELSE a.paired_b
			END,
			updated_at=now()
		FROM agg a
		WHERE e.id=a.execution_id`, executionID); err != nil {
		return ArbitrageCombination{}, err
	}

	updated, err := updateArbitrageCombinationPositions(ctx, tx, combinationID)
	if err != nil {
		return ArbitrageCombination{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ArbitrageCombination{}, err
	}
	return updated, nil
}

func lockArbitrageCombination(
	ctx context.Context,
	tx pgx.Tx,
	id string,
) error {
	var locked string
	err := tx.QueryRow(ctx, `
		SELECT id::text
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid
		FOR UPDATE`, id,
	).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func updateArbitrageCombinationPositions(
	ctx context.Context,
	tx pgx.Tx,
	combinationID string,
) (ArbitrageCombination, error) {
	var updated ArbitrageCombination
	err := tx.QueryRow(ctx, `
		WITH positions AS (
			SELECT
				COALESCE(SUM(CASE
					WHEN o.arbitrage_leg='a' AND o.side='buy' THEN o.filled_quantity
					WHEN o.arbitrage_leg='a' AND o.side='sell' THEN -o.filled_quantity
					ELSE 0
				END),0) AS leg_a_position,
				COALESCE(SUM(CASE
					WHEN o.arbitrage_leg='b' AND o.side='buy' THEN o.filled_quantity
					WHEN o.arbitrage_leg='b' AND o.side='sell' THEN -o.filled_quantity
					ELSE 0
				END),0) AS leg_b_position
			FROM trader_orders o
			JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
			WHERE e.combination_id=$1::uuid AND o.filled_quantity>0
		)
		UPDATE trader_arbitrage_combinations c SET
			leg_a_base_position=positions.leg_a_position+c.leg_a_reconciliation_adjustment,
			leg_b_base_position=positions.leg_b_position+c.leg_b_reconciliation_adjustment,
			version=version+1,updated_at=now()
		FROM positions
		WHERE c.id=$1::uuid
		RETURNING `+arbitrageCombinationColumns,
		combinationID,
	).Scan(arbitrageCombinationScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrNotFound
	}
	return updated, err
}

func (r *Repository) ApplyCircuitOpenExternalReconcile(
	ctx context.Context,
	request circuitOpenExternalReconcileRequest,
) (ArbitrageCombination, bool, error) {
	if strings.TrimSpace(request.CombinationID) == "" ||
		strings.TrimSpace(request.ExecutionID) == "" {
		return ArbitrageCombination{}, false, ErrInvalidArgument
	}
	if !request.MarkA.IsPositive() || !request.MarkB.IsPositive() {
		return ArbitrageCombination{}, false, nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var owner string
	err = tx.QueryRow(ctx, `
		SELECT owner_username
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid`,
		request.CombinationID,
	).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, false, ErrNotFound
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	if _, err = tx.Exec(
		ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, owner,
	); err != nil {
		return ArbitrageCombination{}, false, err
	}

	var combo ArbitrageCombination
	err = tx.QueryRow(ctx, `
		SELECT `+arbitrageCombinationColumns+`
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid
		FOR UPDATE`,
		request.CombinationID,
	).Scan(arbitrageCombinationScanTargets(&combo)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, false, ErrNotFound
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	if combo.Version != request.ExpectedVersion ||
		combo.Status != "running" || !combo.CircuitOpen ||
		!circuitOpenBaselineComplete(combo) {
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
		FOR UPDATE`,
		request.ExecutionID, combo.ID,
	).Scan(arbitrageExecutionScanTargets(&execution)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return combo, false, nil
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	if terminalArbitrageExecutionStatus(execution.Status) {
		return combo, false, nil
	}

	rows, err := tx.Query(ctx, `
		SELECT `+orderColumns+`
		FROM trader_orders o
		WHERE o.arbitrage_execution_id IN (
			SELECT e.id FROM trader_arbitrage_executions e WHERE e.combination_id=$1::uuid
		)
		ORDER BY o.id
		FOR UPDATE OF o`,
		combo.ID,
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
			if order.ArbitrageLeg == "b" {
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

	baselineA := parseDecimal(combo.LegAVenueBaselineBasePosition)
	baselineB := parseDecimal(combo.LegBVenueBaselineBasePosition)
	adjustmentA := request.VenueA.Sub(baselineA).Sub(orderSumA)
	adjustmentB := request.VenueB.Sub(baselineB).Sub(orderSumB)
	localA := orderSumA.Add(adjustmentA)
	localB := orderSumB.Add(adjustmentB)
	if !circuitOpenLegsNotSameDirection(localA, localB) {
		return commitCircuitOpenVenueSnapshot(ctx, tx, combo, request.VenueA, request.VenueB)
	}
	carry := localA.Add(localB)
	if !circuitOpenCarryDust(
		carry, request.InstrumentA, request.InstrumentB, request.MarkA, request.MarkB,
	) {
		return commitCircuitOpenVenueSnapshot(ctx, tx, combo, request.VenueA, request.VenueB)
	}
	updatedCombo := combo
	updatedCombo.LegABasePosition = localA.String()
	updatedCombo.LegBBasePosition = localB.String()
	_, comboNotional, _ := arbitragePositionNotionals(
		updatedCombo, request.MarkA, request.MarkB,
	)

	var applied ArbitrageCombination
	err = tx.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			leg_a_reconciliation_adjustment=$2::numeric,
			leg_b_reconciliation_adjustment=$3::numeric,
			leg_a_base_position=$4::numeric,
			leg_b_base_position=$5::numeric,
			position_notional=$6::numeric,
			leg_a_position_difference=0,
			leg_b_position_difference=0,
			leg_a_venue_base_position=$7::numeric,
			leg_b_venue_base_position=$8::numeric,
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
		WHERE id=$1::uuid AND version=$9
		RETURNING `+arbitrageCombinationColumns,
		combo.ID,
		adjustmentA.String(), adjustmentB.String(),
		localA.String(), localB.String(), comboNotional.String(),
		request.VenueA.String(), request.VenueB.String(),
		combo.Version,
	).Scan(arbitrageCombinationScanTargets(&applied)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return combo, false, nil
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}

	if _, err = tx.Exec(ctx, `
		UPDATE trader_arbitrage_executions SET
			status='canceled',
			error_message='externally_reconciled',
			updated_at=now(),
			closed_at=now()
		WHERE id=$1::uuid`,
		execution.ID,
	); err != nil {
		return ArbitrageCombination{}, false, err
	}

	payload, err := json.Marshal(redactPayload(map[string]any{
		"executionId":            execution.ID,
		"errorMessage":           combo.ErrorMessage,
		"legAVenue":              request.VenueA.String(),
		"legBVenue":              request.VenueB.String(),
		"legABaseline":           combo.LegAVenueBaselineBasePosition,
		"legBBaseline":           combo.LegBVenueBaselineBasePosition,
		"legAOrderSum":           orderSumA.String(),
		"legBOrderSum":           orderSumB.String(),
		"legAPreviousAdjustment": combo.LegAReconciliationAdjustment,
		"legBPreviousAdjustment": combo.LegBReconciliationAdjustment,
		"legAAdjustment":         adjustmentA.String(),
		"legBAdjustment":         adjustmentB.String(),
		"carryBaseQuantity":      applied.CarryBaseQuantity,
		"targetNotional":         combo.TargetNotional,
		"orderNotional":          combo.OrderNotional,
		"positionNotional":       applied.PositionNotional,
		"overTarget": arbitragePositionOverTarget(
			comboNotional, parsePositiveDecimal(combo.TargetNotional),
		),
		"externally_reconciled": true,
	}))
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO trader_arbitrage_events(combination_id,execution_id,event_type,payload)
		VALUES($1::uuid,$2::uuid,'externally_reconciled',$3::jsonb)`,
		combo.ID, execution.ID, payload,
	); err != nil {
		return ArbitrageCombination{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ArbitrageCombination{}, false, err
	}
	return applied, true, nil
}

func (r *Repository) ApplyClosingExternalFlatReconcile(
	ctx context.Context,
	request closingExternalFlatReconcileRequest,
) (ArbitrageCombination, bool, error) {
	if strings.TrimSpace(request.CombinationID) == "" {
		return ArbitrageCombination{}, false, ErrInvalidArgument
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var owner string
	err = tx.QueryRow(ctx, `
		SELECT owner_username
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid`,
		request.CombinationID,
	).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, false, ErrNotFound
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	if _, err = tx.Exec(
		ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, owner,
	); err != nil {
		return ArbitrageCombination{}, false, err
	}

	var combo ArbitrageCombination
	err = tx.QueryRow(ctx, `
		SELECT `+arbitrageCombinationColumns+`
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid
		FOR UPDATE`,
		request.CombinationID,
	).Scan(arbitrageCombinationScanTargets(&combo)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, false, ErrNotFound
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	if combo.Version != request.ExpectedVersion ||
		combo.Status != "closing" || !combo.PositionUncertain ||
		!circuitOpenBaselineComplete(combo) ||
		!strings.EqualFold(combo.LegA.ContractType, "perpetual") ||
		!strings.EqualFold(combo.LegB.ContractType, "perpetual") {
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

	var activeExecution string
	err = tx.QueryRow(ctx, `
		SELECT e.id::text
		FROM trader_arbitrage_executions e
		WHERE e.combination_id=$1::uuid
		  AND e.status NOT IN ('completed','failed','canceled','dry_run')
		LIMIT 1`,
		combo.ID,
	).Scan(&activeExecution)
	if err == nil {
		return combo, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, false, err
	}

	var blockedOrders bool
	err = tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM trader_orders o
			JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
			WHERE e.combination_id=$1::uuid
			  AND (
				o.status NOT IN ('filled','canceled','rejected','expired')
				OR o.reconcile_failures>0
			  )
		)`, combo.ID,
	).Scan(&blockedOrders)
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	if blockedOrders {
		return combo, false, nil
	}
	if combo.ErrorMessage == ErrArbitrageUnsafeHedge.Error() {
		if combo.CircuitOpen ||
			!strings.EqualFold(strings.TrimSpace(combo.RuntimeState), "position_uncertain") {
			return combo, false, nil
		}
		recoveredAfterSnapshot, recoveredErr := recoveredLastCloseFlatSnapshotReady(ctx, tx, combo)
		if recoveredErr != nil {
			return ArbitrageCombination{}, false, recoveredErr
		}
		if !recoveredAfterSnapshot {
			return combo, false, errLastCloseExternalFlatSnapshotStale
		}
	}

	baselineA := parseDecimal(combo.LegAVenueBaselineBasePosition)
	baselineB := parseDecimal(combo.LegBVenueBaselineBasePosition)
	if !venueMatchesCreationBaseline(request.VenueA, baselineA) ||
		!venueMatchesCreationBaseline(request.VenueB, baselineB) ||
		!venueMatchesCreationBaseline(parseDecimal(combo.LegAVenueBasePosition), baselineA) ||
		!venueMatchesCreationBaseline(parseDecimal(combo.LegBVenueBasePosition), baselineB) {
		return combo, false, nil
	}

	ledgerA := parseDecimal(combo.LegABasePosition)
	ledgerB := parseDecimal(combo.LegBBasePosition)
	diffA := request.VenueA.Sub(baselineA.Add(ledgerA))
	diffB := request.VenueB.Sub(baselineB.Add(ledgerB))
	adjustmentA := parseDecimal(combo.LegAReconciliationAdjustment).Add(diffA)
	adjustmentB := parseDecimal(combo.LegBReconciliationAdjustment).Add(diffB)

	var applied ArbitrageCombination
	err = tx.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			leg_a_reconciliation_adjustment=$2::numeric,
			leg_b_reconciliation_adjustment=$3::numeric,
			leg_a_base_position=0,
			leg_b_base_position=0,
			position_notional=0,
			leg_a_position_difference=0,
			leg_b_position_difference=0,
			leg_a_venue_base_position=$4::numeric,
			leg_b_venue_base_position=$5::numeric,
			last_position_reconciled_at=now(),
			position_uncertain=FALSE,
			runtime_state='closing',
			error_message='',
			next_retry_at='-infinity'::timestamptz,
			version=version+1,
			updated_at=now()
		WHERE id=$1::uuid AND version=$6
		RETURNING `+arbitrageCombinationColumns,
		combo.ID,
		adjustmentA.String(), adjustmentB.String(),
		request.VenueA.String(), request.VenueB.String(),
		combo.Version,
	).Scan(arbitrageCombinationScanTargets(&applied)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return combo, false, nil
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}

	reason := closingExternalFlatReason(combo.ErrorMessage)
	payload, err := json.Marshal(redactPayload(map[string]any{
		"reason":                    reason,
		"legALocalBasePosition":     combo.LegABasePosition,
		"legBLocalBasePosition":     combo.LegBBasePosition,
		"legAVenueBaselinePosition": combo.LegAVenueBaselineBasePosition,
		"legBVenueBaselinePosition": combo.LegBVenueBaselineBasePosition,
		"legAVenueBasePosition":     request.VenueA.String(),
		"legBVenueBasePosition":     request.VenueB.String(),
		"legAPositionDifference":    diffA.String(),
		"legBPositionDifference":    diffB.String(),
		"legAPreviousAdjustment":    combo.LegAReconciliationAdjustment,
		"legBPreviousAdjustment":    combo.LegBReconciliationAdjustment,
		"legAAdjustment":            adjustmentA.String(),
		"legBAdjustment":            adjustmentB.String(),
		"previousError":             combo.ErrorMessage,
	}))
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO trader_arbitrage_events(combination_id,event_type,payload)
		VALUES($1::uuid,'closing_external_flat_reconciled',$2::jsonb)`,
		combo.ID, payload,
	); err != nil {
		return ArbitrageCombination{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ArbitrageCombination{}, false, err
	}
	return applied, true, nil
}

var errLastCloseExternalFlatSnapshotStale = errors.New(
	"closing external flat snapshot is not after last close recovery",
)

func recoveredLastCloseFlatSnapshotReady(
	ctx context.Context,
	tx pgx.Tx,
	combo ArbitrageCombination,
) (bool, error) {
	var ready bool
	err := tx.QueryRow(ctx, `
		SELECT COALESCE((
			SELECT ex.status='completed'
			   AND $2::timestamptz >= ev.created_at
			FROM trader_arbitrage_events ev
			JOIN trader_arbitrage_executions ex ON ex.id=ev.execution_id
			WHERE ev.combination_id=$1::uuid
			  AND ev.event_type=$3
			ORDER BY ev.created_at DESC
			LIMIT 1
		), false)`,
		combo.ID, combo.LastPositionReconciledAt, lastCloseResidualRecoveredEvent,
	).Scan(&ready)
	return ready, err
}

func commitCircuitOpenVenueSnapshot(
	ctx context.Context,
	tx pgx.Tx,
	combo ArbitrageCombination,
	venueA, venueB decimal.Decimal,
) (ArbitrageCombination, bool, error) {
	expectedA, expectedB := arbitrageExpectedVenuePositions(
		combo,
		parseDecimal(combo.LegABasePosition),
		parseDecimal(combo.LegBBasePosition),
	)
	diffA := venueA.Sub(expectedA)
	diffB := venueB.Sub(expectedB)
	var updated ArbitrageCombination
	err := tx.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			leg_a_venue_base_position=$2::numeric,
			leg_b_venue_base_position=$3::numeric,
			leg_a_position_difference=$4::numeric,
			leg_b_position_difference=$5::numeric,
			last_position_reconciled_at=now()
		WHERE id=$1::uuid AND circuit_open AND status='running'
		RETURNING `+arbitrageCombinationColumns,
		combo.ID, venueA.String(), venueB.String(), diffA.String(), diffB.String(),
	).Scan(arbitrageCombinationScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return combo, false, nil
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ArbitrageCombination{}, false, err
	}
	return updated, false, nil
}

func (r *Repository) RecordArbitrageFailure(
	ctx context.Context,
	id, errorMessage string,
) (ArbitrageCombination, error) {
	var updated ArbitrageCombination
	failureKey := truncateMessage(errorMessage)
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			error_message=$2,
			consecutive_failures=consecutive_failures+1,
			repeated_failure_count=CASE
				WHEN last_failure_key=$3 THEN repeated_failure_count+1 ELSE 1 END,
			last_failure_key=$3,
			circuit_open=CASE
				WHEN last_failure_key=$3 AND repeated_failure_count+1>=5 THEN TRUE
				ELSE circuit_open END,
			next_retry_at=CASE
				WHEN circuit_open OR (last_failure_key=$3 AND repeated_failure_count+1>=5)
					THEN 'infinity'::timestamptz
				ELSE now() + make_interval(
					secs => LEAST(60, (2 * POWER(2, consecutive_failures))::int)
				)
			END,
			runtime_state=CASE
				WHEN circuit_open OR (last_failure_key=$3 AND repeated_failure_count+1>=5)
					THEN 'manual_intervention'
				ELSE 'backoff'
			END,
			version=version+1,updated_at=now()
		WHERE id=$1::uuid AND status IN ('running','closing')
		RETURNING `+arbitrageCombinationColumns,
		id, truncateMessage(errorMessage), failureKey,
	).Scan(arbitrageCombinationScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrNotFound
	}
	return updated, err
}

func (r *Repository) AppendArbitrageEvent(
	ctx context.Context,
	combinationID, executionID, eventType string,
	payload map[string]any,
) error {
	encoded, err := json.Marshal(redactPayload(payload))
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO trader_arbitrage_events(combination_id,execution_id,event_type,payload)
		VALUES($1::uuid,NULLIF($2,'')::uuid,$3,$4::jsonb)`,
		combinationID, executionID, eventType, encoded)
	return err
}

func (r *Repository) RecordLastCloseRecoveryBlocked(
	ctx context.Context,
	combinationID, executionID, eventType string,
) error {
	switch eventType {
	case lastCloseRecoveryOrderNotTerminalEvent, lastCloseRecoveryFillMismatchEvent:
	default:
		return ErrInvalidArgument
	}
	if strings.TrimSpace(combinationID) == "" || strings.TrimSpace(executionID) == "" {
		return ErrInvalidArgument
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO trader_arbitrage_events(combination_id,execution_id,event_type,payload)
		SELECT $1::uuid,$2::uuid,$3,'{}'::jsonb
		WHERE NOT EXISTS (
			SELECT 1
			FROM trader_arbitrage_events
			WHERE execution_id=$2::uuid
			  AND event_type=$3
		)`,
		combinationID, executionID, eventType)
	return err
}

func (r *Repository) RecordLastCloseResidualRecovered(
	ctx context.Context,
	combinationID, executionID, makerFill, residualFill, effectiveHedgeFill string,
) error {
	if strings.TrimSpace(combinationID) == "" || strings.TrimSpace(executionID) == "" {
		return ErrInvalidArgument
	}
	payload, err := json.Marshal(redactPayload(map[string]any{
		"makerFill":          makerFill,
		"residualFill":       residualFill,
		"effectiveHedgeFill": effectiveHedgeFill,
	}))
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO trader_arbitrage_events(combination_id,execution_id,event_type,payload)
		SELECT $1::uuid,$2::uuid,$3,$4::jsonb
		WHERE NOT EXISTS (
			SELECT 1
			FROM trader_arbitrage_events
			WHERE execution_id=$2::uuid
			  AND event_type=$3
		)`,
		combinationID, executionID, lastCloseResidualRecoveredEvent, payload)
	return err
}

func (r *Repository) LastCloseResidualRecovered(
	ctx context.Context,
	combinationID string,
) (bool, error) {
	if strings.TrimSpace(combinationID) == "" {
		return false, ErrInvalidArgument
	}
	var recovered bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM trader_arbitrage_events
			WHERE combination_id=$1::uuid
			  AND event_type=$2
		)`,
		combinationID, lastCloseResidualRecoveredEvent,
	).Scan(&recovered)
	return recovered, err
}

func (r *Repository) GetLastCloseResidualFlattenExecution(
	ctx context.Context,
	combinationID string,
) (ArbitrageExecution, bool, error) {
	if strings.TrimSpace(combinationID) == "" {
		return ArbitrageExecution{}, false, ErrInvalidArgument
	}
	var item ArbitrageExecution
	err := r.pool.QueryRow(ctx, `
		SELECT `+arbitrageExecutionColumns+`
		FROM trader_arbitrage_executions
		WHERE id=$1::uuid AND combination_id=$2::uuid`,
		lastCloseResidualFlattenExecutionID(combinationID), combinationID,
	).Scan(arbitrageExecutionScanTargets(&item)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageExecution{}, false, nil
	}
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	return item, true, nil
}

func (r *Repository) ClaimLastCloseResidualFlattenExecution(
	ctx context.Context,
	claim LastCloseResidualFlattenClaim,
) (ArbitrageExecution, bool, error) {
	combinationID := strings.TrimSpace(claim.Combination.ID)
	if combinationID == "" || claim.ExpectedVersion <= 0 || len(claim.Orders) == 0 {
		return ArbitrageExecution{}, false, ErrInvalidArgument
	}
	executionID := lastCloseResidualFlattenExecutionID(combinationID)
	if err := validateLastCloseResidualFlattenOrders(executionID, claim.Orders); err != nil {
		return ArbitrageExecution{}, false, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockArbitrageCombination(ctx, tx, combinationID); err != nil {
		return ArbitrageExecution{}, false, err
	}
	var existing ArbitrageExecution
	err = tx.QueryRow(ctx, `
		SELECT `+arbitrageExecutionColumns+`
		FROM trader_arbitrage_executions
		WHERE id=$1::uuid`,
		executionID,
	).Scan(arbitrageExecutionScanTargets(&existing)...)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return ArbitrageExecution{}, false, err
		}
		return existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageExecution{}, false, err
	}
	var version int64
	var status, runtimeState string
	var uncertain, circuitOpen bool
	err = tx.QueryRow(ctx, `
		SELECT version, status, position_uncertain, circuit_open, runtime_state
		FROM trader_arbitrage_combinations
		WHERE id=$1::uuid`,
		combinationID,
	).Scan(&version, &status, &uncertain, &circuitOpen, &runtimeState)
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	if version != claim.ExpectedVersion ||
		!strings.EqualFold(status, "closing") ||
		uncertain || circuitOpen ||
		runtimeState == "manual_intervention" {
		return ArbitrageExecution{}, false, nil
	}
	var otherActive bool
	err = tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM trader_arbitrage_executions
			WHERE combination_id=$1::uuid
			  AND id<>$2::uuid
			  AND status NOT IN ('completed','failed','canceled','dry_run')
		) OR EXISTS (
			SELECT 1 FROM trader_orders o
			JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
			WHERE e.combination_id=$1::uuid
			  AND o.arbitrage_execution_id<>$2::uuid
			  AND o.status NOT IN ('filled','canceled','rejected','expired')
		)`,
		combinationID, executionID,
	).Scan(&otherActive)
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	if otherActive {
		return ArbitrageExecution{}, false, nil
	}
	direction := "bid"
	target := decimal.Zero
	for _, order := range claim.Orders {
		target = target.Add(parseDecimal(order.Quantity))
		if strings.EqualFold(order.ArbitrageLeg, "a") && strings.EqualFold(order.Side, "buy") {
			direction = "ask"
		}
	}
	var created ArbitrageExecution
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
			'hedging',0,0,0,0,0,0,
			$4::numeric,0,'close',TRUE,FALSE,0,0,0,0,0,'')
		RETURNING `+arbitrageExecutionColumns,
		executionID, combinationID, direction, target.String(),
	).Scan(arbitrageExecutionScanTargets(&created)...)
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	for _, order := range claim.Orders {
		if order.ID == "" {
			order.ID = uuid.NewString()
		}
		if order.ClientOrderID == "" {
			order.ClientOrderID = clientOrderID()
		}
		order.ArbitrageExecutionID = executionID
		order.Status = "pending"
		if _, err := tx.Exec(ctx, `
			INSERT INTO trader_orders (
				id,idempotency_key,owner_username,trading_account_id,product_name,
				exchange,instrument_id,contract_type,exchange_symbol,client_order_id,
				side,order_type,quantity,price,status,request_fingerprint,base_asset,quote_asset,
				twap_job_id,twap_slice_index,twap_attempt_index,
				arbitrage_execution_id,arbitrage_leg,arbitrage_role,reduce_only
			) VALUES (
				$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NULLIF($14,'')::numeric,
				'pending',$15,$16,$17,NULL,NULL,0,NULLIF($18,'')::uuid,NULLIF($19,''),NULLIF($20,''),$21
			)`, orderWriteArgs(order)...); err != nil {
			return ArbitrageExecution{}, false, err
		}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE trader_arbitrage_combinations
		SET version=version+1, updated_at=now()
		WHERE id=$1::uuid AND version=$2 AND status='closing'`,
		combinationID, claim.ExpectedVersion,
	)
	if err != nil {
		return ArbitrageExecution{}, false, err
	}
	if tag.RowsAffected() != 1 {
		return ArbitrageExecution{}, false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return ArbitrageExecution{}, false, err
	}
	return created, true, nil
}

func validateLastCloseResidualFlattenOrders(executionID string, orders []Order) error {
	if len(orders) == 0 || len(orders) > 2 {
		return ErrInvalidArgument
	}
	seen := map[string]bool{}
	for _, order := range orders {
		leg := strings.ToLower(strings.TrimSpace(order.ArbitrageLeg))
		if leg != "a" && leg != "b" {
			return ErrInvalidArgument
		}
		if seen[leg] || !order.ReduceOnly || order.OrderType != "market" {
			return ErrInvalidArgument
		}
		if order.ArbitrageExecutionID != executionID ||
			order.IdempotencyKey != lastCloseResidualFlattenIdempotencyKey(executionID, leg) {
			return ErrInvalidArgument
		}
		if !parseDecimal(order.Quantity).IsPositive() {
			return ErrInvalidArgument
		}
		seen[leg] = true
	}
	return nil
}

func (r *Repository) CloseLastCloseResidualWithDust(
	ctx context.Context,
	close LastCloseResidualDustClose,
) (ArbitrageCombination, bool, error) {
	if strings.TrimSpace(close.CombinationID) == "" || close.ExpectedVersion <= 0 {
		return ArbitrageCombination{}, false, ErrInvalidArgument
	}
	if !close.LegANotional.LessThan(decimal.NewFromInt(arbitrageMinOpenNotional)) ||
		!close.LegBNotional.LessThan(decimal.NewFromInt(arbitrageMinOpenNotional)) ||
		close.LegAReducible || close.LegBReducible ||
		close.LegANotional.IsNegative() || close.LegBNotional.IsNegative() {
		return ArbitrageCombination{}, false, nil
	}
	payload, err := json.Marshal(redactPayload(close.Payload))
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockArbitrageCombination(ctx, tx, close.CombinationID); err != nil {
		return ArbitrageCombination{}, false, err
	}
	lastOrder := close.LastOrderUpdatedAt.UTC()
	if lastOrder.IsZero() {
		lastOrder = time.Unix(0, 0).UTC()
	}
	var updated ArbitrageCombination
	err = tx.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			status='closed',
			closed_at=now(),
			market_data_stale=TRUE,
			runtime_state='monitoring',
			error_message='',
			consecutive_failures=0,
			last_failure_key='',
			repeated_failure_count=0,
			next_retry_at='-infinity'::timestamptz,
			final_metrics_status=CASE
				WHEN final_metrics_status='completed' THEN 'completed'
				ELSE 'partial'
			END,
			final_metrics_next_retry_at=CASE
				WHEN final_metrics_status='completed' THEN final_metrics_next_retry_at
				ELSE 'infinity'::timestamptz
			END,
			final_metrics_completed_at=CASE
				WHEN final_metrics_status='completed' THEN final_metrics_completed_at
				ELSE NULL
			END,
			final_metrics_last_error=CASE
				WHEN final_metrics_status='completed' THEN final_metrics_last_error
				ELSE 'residual_dust_metrics_skipped'
			END,
			version=version+1,
			updated_at=now()
		WHERE id=$1::uuid
		  AND version=$2
		  AND status='closing'
		  AND NOT position_uncertain
		  AND NOT circuit_open
		  AND runtime_state<>'manual_intervention'
		  AND last_position_reconciled_at > '1970-01-02'::timestamptz
		  AND (
			$3::timestamptz <= '1970-01-02'::timestamptz
			OR last_position_reconciled_at >= $3
		  )
		  AND $4::numeric < 20
		  AND $5::numeric < 20
		  AND NOT $6
		  AND NOT $7
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
		close.CombinationID, close.ExpectedVersion, lastOrder,
		close.LegANotional.String(), close.LegBNotional.String(),
		close.LegAReducible, close.LegBReducible,
	).Scan(arbitrageCombinationScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, false, nil
	}
	if err != nil {
		return ArbitrageCombination{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO trader_arbitrage_events(combination_id,execution_id,event_type,payload)
		SELECT $1::uuid, NULLIF($2,'')::uuid, $3, $4::jsonb
		WHERE NOT EXISTS (
			SELECT 1 FROM trader_arbitrage_events
			WHERE combination_id=$1::uuid AND event_type=$3
		)`,
		close.CombinationID, close.ExecutionID, lastCloseResidualDustEvent, payload,
	); err != nil {
		return ArbitrageCombination{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ArbitrageCombination{}, false, err
	}
	return updated, true, nil
}

func (r *Repository) DeleteExpiredArbitrageCombinations(
	ctx context.Context,
	before time.Time,
	limit int,
) (int64, error) {
	if limit <= 0 {
		limit = 100
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT id::text FROM trader_arbitrage_combinations
		WHERE status IN ('closed','failed') AND closed_at < $1
		ORDER BY closed_at,id FOR UPDATE SKIP LOCKED LIMIT $2`, before, limit)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 {
		_ = tx.Commit(ctx)
		return 0, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE trader_orders SET arbitrage_execution_id=NULL,arbitrage_leg=NULL,arbitrage_role=NULL
		WHERE arbitrage_execution_id IN (
			SELECT id FROM trader_arbitrage_executions WHERE combination_id=ANY($1::uuid[])
		)`, ids); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM trader_arbitrage_combinations WHERE id=ANY($1::uuid[])`, ids)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

const arbitrageCombinationColumns = `
	id::text,idempotency_key,request_fingerprint,owner_username,
	leg_a_trading_account_id,leg_a_instrument_id,leg_a_product_name,leg_a_account_name,
	leg_a_exchange,leg_a_contract_type,leg_a_exchange_symbol,leg_a_base_asset,leg_a_quote_asset,
	leg_b_trading_account_id,leg_b_instrument_id,leg_b_product_name,leg_b_account_name,
	leg_b_exchange,leg_b_contract_type,leg_b_exchange_symbol,leg_b_base_asset,leg_b_quote_asset,
	ask_threshold_bps::text,bid_threshold_bps::text,target_notional::text,COALESCE(order_notional::text,''),
	execution_mode,maker_leg,status,trim_scale(position_notional)::text,
	runtime_state,trim_scale(cumulative_turnover_notional)::text,
	trim_scale(gross_turnover_notional)::text,
	trim_scale(leg_a_base_position)::text,trim_scale(leg_b_base_position)::text,
	trim_scale(carry_base_quantity)::text,
	COALESCE(trim_scale(leg_a_average_entry_price)::text,''),
	COALESCE(trim_scale(leg_b_average_entry_price)::text,''),
	COALESCE(trim_scale(average_entry_spread_bps)::text,''),
	COALESCE(trim_scale(leg_a_unrealized_pnl)::text,''),
	COALESCE(trim_scale(leg_b_unrealized_pnl)::text,''),
	trim_scale(realized_spread_pnl)::text,
	trim_scale(estimated_funding_pnl)::text,
	COALESCE(trim_scale(estimated_trading_fee)::text,''),
	COALESCE(metrics_quality,''),
	trim_scale(notional_exposure_seconds)::text,
	trim_scale(holding_seconds)::text,
	COALESCE(exposure_updated_at,'epoch'::timestamptz),
	COALESCE(trim_scale(combined_position_annualized)::text,''),
	funding_history_complete,
	COALESCE(trim_scale(leg_a_venue_baseline_base_position)::text,''),
	COALESCE(trim_scale(leg_b_venue_baseline_base_position)::text,''),
	COALESCE(venue_baseline_captured_at,'epoch'::timestamptz),
	trim_scale(COALESCE(leg_a_venue_base_position,0))::text,
	trim_scale(COALESCE(leg_b_venue_base_position,0))::text,
	trim_scale(COALESCE(leg_a_position_difference,0))::text,
	trim_scale(COALESCE(leg_b_position_difference,0))::text,
	trim_scale(COALESCE(leg_a_reconciliation_adjustment,0))::text,
	trim_scale(COALESCE(leg_b_reconciliation_adjustment,0))::text,
	COALESCE(last_position_reconciled_at,'epoch'::timestamptz),
	COALESCE(current_ask_spread_bps::text,''),COALESCE(current_bid_spread_bps::text,''),
	market_data_stale,error_message,consecutive_failures,
	CASE WHEN next_retry_at IN ('-infinity'::timestamptz, 'infinity'::timestamptz)
		THEN 'epoch'::timestamptz ELSE next_retry_at END,
	position_uncertain,
	last_failure_key,repeated_failure_count,circuit_open,
	version,
	CASE WHEN scheduler_lease_until='-infinity'::timestamptz
		THEN 'epoch'::timestamptz ELSE scheduler_lease_until END,
	created_at,updated_at,
	COALESCE(closed_at,'epoch'::timestamptz),
	run_mode,COALESCE(entry_direction,''),
	COALESCE(leg_a_leverage::text,''),COALESCE(leg_b_leverage::text,''),
	COALESCE(exit_policy,''),COALESCE(exit_annualized_rate::text,''),
	COALESCE(exit_after_seconds,0),
	COALESCE(target_reached_at,'epoch'::timestamptz),
	COALESCE(scheduled_exit_at,'epoch'::timestamptz),
	COALESCE(one_shot_phase,''),
	COALESCE(trim_scale(early_exit_funding_8h_annualized_floor)::text,''),
	COALESCE(trim_scale(early_exit_liquidation_distance_pct)::text,''),
	COALESCE(metrics_calculated_at,'epoch'::timestamptz),
	final_metrics_status,
	final_metrics_retry_count,
	final_metrics_last_error,
	CASE WHEN final_metrics_next_retry_at IN ('-infinity'::timestamptz, 'infinity'::timestamptz)
		THEN 'epoch'::timestamptz ELSE final_metrics_next_retry_at END,
	final_metrics_next_retry_at < 'infinity'::timestamptz,
	COALESCE(final_metrics_completed_at,'epoch'::timestamptz),
	COALESCE(trim_scale(leg_a_liquidation_distance_pct)::text,''),
	COALESCE(trim_scale(leg_b_liquidation_distance_pct)::text,'')`

func arbitrageCombinationScanTargets(item *ArbitrageCombination) []any {
	return []any{
		&item.ID, &item.IdempotencyKey, &item.RequestFingerprint, &item.OwnerUsername,
		&item.LegA.TradingAccountID, &item.LegA.InstrumentID, &item.LegA.ProductName,
		&item.LegA.AccountName, &item.LegA.Exchange, &item.LegA.ContractType,
		&item.LegA.ExchangeSymbol, &item.LegA.BaseAsset, &item.LegA.QuoteAsset,
		&item.LegB.TradingAccountID, &item.LegB.InstrumentID, &item.LegB.ProductName,
		&item.LegB.AccountName, &item.LegB.Exchange, &item.LegB.ContractType,
		&item.LegB.ExchangeSymbol, &item.LegB.BaseAsset, &item.LegB.QuoteAsset,
		&item.AskThresholdBps, &item.BidThresholdBps, &item.TargetNotional,
		&item.OrderNotional, &item.ExecutionMode, &item.MakerLeg,
		&item.Status, &item.PositionNotional, &item.RuntimeState,
		&item.CumulativeTurnoverNotional,
		&item.GrossTurnoverNotional,
		&item.LegABasePosition, &item.LegBBasePosition, &item.CarryBaseQuantity,
		&item.LegAAverageEntryPrice, &item.LegBAverageEntryPrice,
		&item.AverageEntrySpreadBps,
		&item.LegAUnrealizedPnl, &item.LegBUnrealizedPnl,
		&item.RealizedSpreadPnl, &item.EstimatedFundingPnl,
		&item.EstimatedTradingFee, &item.MetricsQuality,
		&item.NotionalExposureSeconds, &item.HoldingSeconds,
		&item.ExposureUpdatedAt, &item.CombinedPositionAnnualized,
		&item.FundingHistoryComplete,
		&item.LegAVenueBaselineBasePosition, &item.LegBVenueBaselineBasePosition,
		&item.VenueBaselineCapturedAt,
		&item.LegAVenueBasePosition, &item.LegBVenueBasePosition,
		&item.LegAPositionDifference, &item.LegBPositionDifference,
		&item.LegAReconciliationAdjustment, &item.LegBReconciliationAdjustment,
		&item.LastPositionReconciledAt,
		&item.CurrentAskSpreadBps, &item.CurrentBidSpreadBps, &item.MarketDataStale,
		&item.ErrorMessage, &item.ConsecutiveFailures, &item.NextRetryAt, &item.PositionUncertain,
		&item.LastFailureKey, &item.RepeatedFailureCount, &item.CircuitOpen,
		&item.Version,
		&item.SchedulerLeaseUntil, &item.CreatedAt, &item.UpdatedAt, &item.ClosedAt,
		&item.RunMode, &item.EntryDirection, &item.LegALeverage, &item.LegBLeverage,
		&item.ExitPolicy, &item.ExitAnnualizedRate, &item.ExitAfterSeconds,
		&item.TargetReachedAt, &item.ScheduledExitAt, &item.OneShotPhase,
		&item.EarlyExitFunding8hAnnualizedFloor,
		&item.EarlyExitLiquidationDistancePct,
		&item.MetricsCalculatedAt,
		&item.FinalMetricsStatus, &item.FinalMetricsRetryCount, &item.FinalMetricsLastError,
		&item.FinalMetricsNextRetryAt, &item.FinalMetricsRetryFinite,
		&item.FinalMetricsCompletedAt,
		&item.LegALiquidationDistancePct, &item.LegBLiquidationDistancePct,
	}
}

func arbitrageCombinationWriteArgs(item ArbitrageCombination) []any {
	return []any{
		item.ID, item.IdempotencyKey, item.RequestFingerprint, item.OwnerUsername,
		item.LegA.TradingAccountID, item.LegA.InstrumentID, item.LegA.ProductName,
		item.LegA.AccountName, item.LegA.Exchange, item.LegA.ContractType,
		item.LegA.ExchangeSymbol, item.LegA.BaseAsset, item.LegA.QuoteAsset,
		item.LegB.TradingAccountID, item.LegB.InstrumentID, item.LegB.ProductName,
		item.LegB.AccountName, item.LegB.Exchange, item.LegB.ContractType,
		item.LegB.ExchangeSymbol, item.LegB.BaseAsset, item.LegB.QuoteAsset,
		item.AskThresholdBps, item.BidThresholdBps, item.TargetNotional,
		nullableNumeric(item.OrderNotional), item.ExecutionMode, item.MakerLeg,
		item.Status, zeroString(item.PositionNotional), zeroString(item.CumulativeTurnoverNotional),
		item.LegAVenueBaselineBasePosition, item.LegBVenueBaselineBasePosition,
		nullableArbitrageTimestamp(item.VenueBaselineCapturedAt),
		item.MarketDataStale,
		defaultRunMode(item.RunMode), item.EntryDirection,
		nullableNumeric(item.LegALeverage), nullableNumeric(item.LegBLeverage),
		item.ExitPolicy, nullableNumeric(item.ExitAnnualizedRate),
		nullableInt(item.ExitAfterSeconds), nullableOneShotPhase(item),
		nullableNumeric(item.EarlyExitFunding8hAnnualizedFloor),
		nullableNumeric(item.EarlyExitLiquidationDistancePct),
	}
}

func scheduledExitFromCreated(created time.Time, seconds int) time.Time {
	if created.IsZero() || seconds <= 0 {
		return time.Time{}
	}
	return created.Add(time.Duration(seconds) * time.Second)
}

func nullableArbitrageTimestamp(value time.Time) any {
	if value.IsZero() || value.Equal(time.Unix(0, 0).UTC()) {
		return nil
	}
	return value
}

func nullableNumeric(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return strings.TrimSpace(value)
}

func nullableInt(value int) any {
	if value <= 0 {
		return nil
	}
	return value
}

func defaultRunMode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "spread"
	}
	return value
}

func nullableOneShotPhase(item ArbitrageCombination) any {
	if !strings.EqualFold(strings.TrimSpace(item.RunMode), "one_shot") {
		return nil
	}
	phase := strings.ToLower(strings.TrimSpace(item.OneShotPhase))
	if phase == "" {
		return "building_target"
	}
	return phase
}

const arbitrageExecutionColumns = `
	id::text,combination_id::text,direction,sequence,status,
	trigger_ask_spread_bps::text,trigger_bid_spread_bps::text,
	trigger_leg_a_bid::text,trigger_leg_a_ask::text,trigger_leg_b_bid::text,trigger_leg_b_ask::text,
	target_base_quantity::text,requested_notional::text,position_effect,reduce_only,last_close_clip,
	leg_a_filled_quantity::text,leg_b_filled_quantity::text,
	delta_notional::text,COALESCE(maker_order_id::text,''),COALESCE(hedge_order_id::text,''),
	hedge_sequence,attempt,error_message,created_at,updated_at,COALESCE(closed_at,'epoch'::timestamptz)`

func arbitrageExecutionScanTargets(item *ArbitrageExecution) []any {
	return []any{
		&item.ID, &item.CombinationID, &item.Direction, &item.Sequence, &item.Status,
		&item.TriggerAskSpread, &item.TriggerBidSpread,
		&item.TriggerLegABid, &item.TriggerLegAAsk, &item.TriggerLegBBid, &item.TriggerLegBAsk,
		&item.TargetBaseQuantity, &item.RequestedNotional, &item.PositionEffect, &item.ReduceOnly,
		&item.LastCloseClip,
		&item.LegAFilledQuantity, &item.LegBFilledQuantity,
		&item.DeltaNotional, &item.MakerOrderID, &item.HedgeOrderID, &item.HedgeSequence, &item.Attempt,
		&item.ErrorMessage, &item.CreatedAt, &item.UpdatedAt, &item.ClosedAt,
	}
}

func zeroString(value string) string {
	if value == "" {
		return "0"
	}
	return value
}

func arbitrageRuntimeState(value string) string {
	switch value {
	case "monitoring", "maker_open", "maker_canceling", "repricing",
		"opportunity_gone", "hedging", "hedge_deferred_dust",
		"reconciling", "backoff", "position_uncertain",
		"manual_intervention", "closing":
		return value
	default:
		return "monitoring"
	}
}

func isArbitrageCarryHedgeRole(role string) bool {
	switch strings.TrimSpace(role) {
	case "hedge", "residual":
		return true
	default:
		return false
	}
}

func lastCloseClipUnbalancedErrorMessage(executionID, fillA, fillB, remaining string) string {
	return fmt.Sprintf(
		"last close clip unbalanced execution=%s fillA=%s fillB=%s remaining=%s",
		executionID, fillA, fillB, remaining,
	)
}

func lastCloseClipUnbalancedPayload(fillA, fillB, remaining string) map[string]any {
	return map[string]any{
		"legAFilledQuantity":     fillA,
		"legBFilledQuantity":     fillB,
		"remainingHedgeQuantity": remaining,
	}
}

func pairedArbitrageFillTotals(orders []Order, executionID string) (legA, legB decimal.Decimal, found bool) {
	for _, order := range orders {
		if order.ArbitrageExecutionID != executionID {
			continue
		}
		found = true
		if order.ArbitrageRole == "residual" {
			continue
		}
		filled := parseDecimal(order.FilledQuantity)
		if !filled.IsPositive() {
			continue
		}
		switch order.ArbitrageLeg {
		case "a":
			legA = legA.Add(filled)
		case "b":
			legB = legB.Add(filled)
		}
	}
	return
}
