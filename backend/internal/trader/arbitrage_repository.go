package trader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
				arbitrage_execution_id,arbitrage_leg,arbitrage_role
			) VALUES (
				$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NULLIF($14,'')::numeric,
				'pending',$15,$16,$17,NULL,NULL,0,NULLIF($18,'')::uuid,NULLIF($19,''),NULLIF($20,'')
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
	var created ArbitrageCombination
	err = tx.QueryRow(ctx, `
		INSERT INTO trader_arbitrage_combinations (
			id,idempotency_key,request_fingerprint,owner_username,
			leg_a_trading_account_id,leg_a_instrument_id,leg_a_product_name,leg_a_account_name,
			leg_a_exchange,leg_a_contract_type,leg_a_exchange_symbol,leg_a_base_asset,leg_a_quote_asset,
			leg_b_trading_account_id,leg_b_instrument_id,leg_b_product_name,leg_b_account_name,
			leg_b_exchange,leg_b_contract_type,leg_b_exchange_symbol,leg_b_base_asset,leg_b_quote_asset,
			ask_threshold_bps,bid_threshold_bps,target_notional,order_notional,max_delta_notional,
			execution_mode,maker_leg,status,completed_notional,market_data_stale
		) VALUES (
			$1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,
			$14,$15,$16,$17,$18,$19,$20,$21,$22,
			$23::numeric,$24::numeric,$25::numeric,$26::numeric,$27::numeric,
			$28,$29,$30,$31::numeric,$32
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
		payload, _ := json.Marshal(map[string]any{"executionMode": created.ExecutionMode})
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

func (r *Repository) MarkArbitrageClosing(
	ctx context.Context,
	owner, id string,
) (ArbitrageCombination, error) {
	var item ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations
		SET status=CASE WHEN status='running' OR status='failed' THEN 'closing' ELSE status END,
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

func (r *Repository) ListArbitrageOrders(
	ctx context.Context,
	owner, combinationID string,
) ([]Order, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+orderColumns+`
		FROM trader_orders o
		JOIN trader_arbitrage_executions e ON e.id=o.arbitrage_execution_id
		WHERE o.owner_username=$1 AND e.combination_id=$2::uuid
		ORDER BY o.created_at DESC`, owner, combinationID)
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
		var item ArbitrageEvent
		if err := rows.Scan(
			&item.ID, &item.CombinationID, &item.ExecutionID,
			&item.Type, &item.Message, &item.CreatedAt,
		); err != nil {
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
	var result ArbitrageExecution
	err := r.pool.QueryRow(ctx, `
		INSERT INTO trader_arbitrage_executions (
			id,combination_id,direction,sequence,status,
			trigger_ask_spread_bps,trigger_bid_spread_bps,
			trigger_leg_a_bid,trigger_leg_a_ask,trigger_leg_b_bid,trigger_leg_b_ask,
			target_base_quantity,leg_a_filled_quantity,leg_b_filled_quantity,delta_notional,
			hedge_sequence,attempt,error_message
		)
		SELECT $1::uuid,$2::uuid,$3,
			COALESCE((SELECT max(sequence)+1 FROM trader_arbitrage_executions WHERE combination_id=$2::uuid),1),
			$4,$5::numeric,$6::numeric,$7::numeric,$8::numeric,$9::numeric,$10::numeric,
			$11::numeric,$12::numeric,$13::numeric,$14::numeric,$15,$16,$17
		WHERE EXISTS (
			SELECT 1 FROM trader_arbitrage_combinations
			WHERE id=$2::uuid AND status='running'
		)
		ON CONFLICT DO NOTHING
		RETURNING `+arbitrageExecutionColumns,
		execution.ID, execution.CombinationID, execution.Direction, execution.Status,
		execution.TriggerAskSpread, execution.TriggerBidSpread,
		execution.TriggerLegABid, execution.TriggerLegAAsk,
		execution.TriggerLegBBid, execution.TriggerLegBAsk,
		execution.TargetBaseQuantity, zeroString(execution.LegAFilledQuantity),
		zeroString(execution.LegBFilledQuantity), zeroString(execution.DeltaNotional),
		execution.HedgeSequence, execution.Attempt, execution.ErrorMessage,
	).Scan(arbitrageExecutionScanTargets(&result)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageExecution{}, false, nil
	}
	return result, err == nil, err
}

func (r *Repository) GetActiveArbitrageExecution(
	ctx context.Context,
	combinationID, direction string,
) (ArbitrageExecution, error) {
	var item ArbitrageExecution
	err := r.pool.QueryRow(ctx, `
		SELECT `+arbitrageExecutionColumns+`
		FROM trader_arbitrage_executions
		WHERE combination_id=$1::uuid AND direction=$2
		  AND status NOT IN ('completed','failed','canceled','dry_run')`,
		combinationID, direction,
	).Scan(arbitrageExecutionScanTargets(&item)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageExecution{}, ErrNotFound
	}
	return item, err
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
			closed_at=CASE WHEN $2 IN ('completed','failed','canceled','dry_run') THEN now() ELSE NULL END
		WHERE id=$1::uuid
		RETURNING `+arbitrageExecutionColumns,
		item.ID, item.Status, zeroString(item.LegAFilledQuantity),
		zeroString(item.LegBFilledQuantity), zeroString(item.DeltaNotional),
		item.MakerOrderID, item.HedgeOrderID, item.HedgeSequence, item.Attempt, item.ErrorMessage,
	).Scan(arbitrageExecutionScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageExecution{}, ErrNotFound
	}
	return updated, err
}

func (r *Repository) UpdateArbitrageCombinationRuntime(
	ctx context.Context,
	item ArbitrageCombination,
) (ArbitrageCombination, error) {
	var updated ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			status=$2,completed_notional=$3::numeric,
			current_ask_spread_bps=NULLIF($4,'')::numeric,
			current_bid_spread_bps=NULLIF($5,'')::numeric,
			market_data_stale=$6,error_message=$7,version=version+1,updated_at=now(),
			closed_at=CASE WHEN $2 IN ('closed','failed') THEN COALESCE(closed_at,now()) ELSE closed_at END
		WHERE id=$1::uuid
		RETURNING `+arbitrageCombinationColumns,
		item.ID, item.Status, zeroString(item.CompletedNotional),
		item.CurrentAskSpreadBps, item.CurrentBidSpreadBps,
		item.MarketDataStale, item.ErrorMessage,
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
) (ArbitrageCombination, error) {
	var updated ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			current_ask_spread_bps=NULLIF($2,'')::numeric,
			current_bid_spread_bps=NULLIF($3,'')::numeric,
			market_data_stale=$4,updated_at=now()
		WHERE id=$1::uuid AND status IN ('running','closing')
		RETURNING `+arbitrageCombinationColumns,
		id, askSpread, bidSpread, stale,
	).Scan(arbitrageCombinationScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArbitrageCombination{}, ErrNotFound
	}
	return updated, err
}

func (r *Repository) AddArbitrageCompletedNotional(
	ctx context.Context,
	id, delta string,
) (ArbitrageCombination, error) {
	var updated ArbitrageCombination
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_arbitrage_combinations SET
			completed_notional=LEAST(target_notional,completed_notional+$2::numeric),
			status=CASE
				WHEN status='closing' THEN 'closing'
				WHEN completed_notional+$2::numeric >= target_notional THEN 'closed'
				ELSE status
			END,
			market_data_stale=CASE
				WHEN status='running' AND completed_notional+$2::numeric >= target_notional THEN TRUE
				ELSE market_data_stale
			END,
			closed_at=CASE
				WHEN status='running' AND completed_notional+$2::numeric >= target_notional
					THEN COALESCE(closed_at,now())
				ELSE closed_at
			END,
			version=version+1,updated_at=now()
		WHERE id=$1::uuid AND status IN ('running','closing')
		RETURNING `+arbitrageCombinationColumns,
		id, delta,
	).Scan(arbitrageCombinationScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		err = r.pool.QueryRow(ctx, `
			SELECT `+arbitrageCombinationColumns+`
			FROM trader_arbitrage_combinations WHERE id=$1::uuid`, id,
		).Scan(arbitrageCombinationScanTargets(&updated)...)
		if err == nil && updated.Status == "closed" {
			return updated, nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ArbitrageCombination{}, ErrNotFound
		}
		if err != nil {
			return ArbitrageCombination{}, err
		}
		return ArbitrageCombination{}, ErrArbitrageConflict
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
	ask_threshold_bps::text,bid_threshold_bps::text,target_notional::text,order_notional::text,
	max_delta_notional::text,execution_mode,maker_leg,status,completed_notional::text,
	COALESCE(current_ask_spread_bps::text,''),COALESCE(current_bid_spread_bps::text,''),
	market_data_stale,error_message,
	CASE WHEN scheduler_lease_until='-infinity'::timestamptz
		THEN 'epoch'::timestamptz ELSE scheduler_lease_until END,
	created_at,updated_at,
	COALESCE(closed_at,'epoch'::timestamptz)`

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
		&item.OrderNotional, &item.MaxDeltaNotional, &item.ExecutionMode, &item.MakerLeg,
		&item.Status, &item.CompletedNotional, &item.CurrentAskSpreadBps,
		&item.CurrentBidSpreadBps, &item.MarketDataStale, &item.ErrorMessage,
		&item.SchedulerLeaseUntil, &item.CreatedAt, &item.UpdatedAt, &item.ClosedAt,
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
		item.OrderNotional, item.MaxDeltaNotional, item.ExecutionMode, item.MakerLeg,
		item.Status, zeroString(item.CompletedNotional), item.MarketDataStale,
	}
}

const arbitrageExecutionColumns = `
	id::text,combination_id::text,direction,sequence,status,
	trigger_ask_spread_bps::text,trigger_bid_spread_bps::text,
	trigger_leg_a_bid::text,trigger_leg_a_ask::text,trigger_leg_b_bid::text,trigger_leg_b_ask::text,
	target_base_quantity::text,leg_a_filled_quantity::text,leg_b_filled_quantity::text,
	delta_notional::text,COALESCE(maker_order_id::text,''),COALESCE(hedge_order_id::text,''),
	hedge_sequence,attempt,error_message,created_at,updated_at,COALESCE(closed_at,'epoch'::timestamptz)`

func arbitrageExecutionScanTargets(item *ArbitrageExecution) []any {
	return []any{
		&item.ID, &item.CombinationID, &item.Direction, &item.Sequence, &item.Status,
		&item.TriggerAskSpread, &item.TriggerBidSpread,
		&item.TriggerLegABid, &item.TriggerLegAAsk, &item.TriggerLegBBid, &item.TriggerLegBAsk,
		&item.TargetBaseQuantity, &item.LegAFilledQuantity, &item.LegBFilledQuantity,
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
