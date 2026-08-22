package trader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type twapStore interface {
	CreateTwap(context.Context, TwapJob) (TwapJob, bool, error)
	GetTwapByOwner(context.Context, string, string) (TwapJob, error)
	GetTwap(context.Context, string) (TwapJob, error)
	ListTwaps(context.Context, string, int64, string, string, int, string) ([]TwapJob, string, error)
	ListTwapOrders(context.Context, string, string) ([]Order, error)
	LeaseDueTwaps(context.Context, int, time.Duration) ([]TwapJob, error)
	RefreshTwapProgress(context.Context, string) (TwapJob, error)
	UpdateTwapSchedule(context.Context, string, string, int, int, time.Time, string, string) (TwapJob, error)
	CloseTwap(context.Context, string, string, string) (TwapJob, error)
	AppendTwapEvent(context.Context, string, string, map[string]any) error
	DeleteClosedTwaps(context.Context, time.Time, int) (int64, error)
}

type twapChildStore interface {
	CreateTwapIntent(context.Context, string, Order) (Order, bool, error)
}

func (r *Repository) CreateTwapIntent(
	ctx context.Context,
	jobID string,
	order Order,
) (Order, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Order{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	if err := tx.QueryRow(ctx, `
		SELECT status FROM trader_twap_jobs WHERE id=$1::uuid FOR UPDATE`, jobID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, false, ErrNotFound
		}
		return Order{}, false, err
	}
	if status != "pending" && status != "running" {
		return Order{}, false, ErrTwapNotCancelable
	}
	if order.ID == "" {
		order.ID = uuid.NewString()
	}
	var result Order
	var created bool
	err = tx.QueryRow(ctx, `
		INSERT INTO trader_orders (
			id,idempotency_key,owner_username,trading_account_id,product_name,
			exchange,instrument_id,contract_type,exchange_symbol,client_order_id,
			side,order_type,quantity,price,status,request_fingerprint,base_asset,quote_asset,
			twap_job_id,twap_slice_index,twap_attempt_index
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NULLIF($14,'')::numeric,
			'pending',$15,$16,$17,$18::uuid,$19,$20
		)
		ON CONFLICT (idempotency_key) DO UPDATE
			SET idempotency_key=EXCLUDED.idempotency_key
		RETURNING `+orderColumns+`,(xmax=0)`,
		order.ID, order.IdempotencyKey, order.OwnerUsername, order.TradingAccountID,
		order.ProductName, order.Exchange, order.InstrumentID, order.ContractType,
		order.ExchangeSymbol, order.ClientOrderID, order.Side, order.OrderType,
		order.Quantity, order.Price, order.RequestFingerprint, order.BaseAsset,
		order.QuoteAsset, jobID, order.TwapSliceIndex, order.TwapAttemptIndex,
	).Scan(append(orderScanTargets(&result), &created)...)
	if err != nil {
		return Order{}, false, fmt.Errorf("create twap child intent: %w", err)
	}
	if result.RequestFingerprint != order.RequestFingerprint || result.TwapJobID != jobID {
		return Order{}, false, ErrIdempotencyConflict
	}
	if _, err := tx.Exec(ctx, `
		UPDATE trader_twap_jobs SET active_order_id=$2::uuid,updated_at=now()
		WHERE id=$1::uuid`, jobID, result.ID); err != nil {
		return Order{}, false, err
	}
	if created {
		payload, _ := json.Marshal(map[string]any{
			"slice": order.TwapSliceIndex, "attempt": order.TwapAttemptIndex,
			"quantity": order.Quantity, "price": order.Price,
		})
		if _, err := tx.Exec(ctx, `
			INSERT INTO trader_order_events(order_id,event_type,payload)
			VALUES($1::uuid,'intent',$2::jsonb)`, result.ID, payload); err != nil {
			return Order{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Order{}, false, err
	}
	return result, created, nil
}

func (r *Repository) CreateTwap(ctx context.Context, job TwapJob) (TwapJob, bool, error) {
	if job.ID == "" {
		job.ID = uuid.NewString()
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return TwapJob{}, false, fmt.Errorf("begin create twap: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	existing, err := getTwapRow(ctx, tx, `WHERE idempotency_key=$1`, job.IdempotencyKey)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return TwapJob{}, false, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, job.TradingAccountID); err != nil {
		return TwapJob{}, false, fmt.Errorf("lock twap account: %w", err)
	}
	existing, err = getTwapRow(ctx, tx, `WHERE idempotency_key=$1`, job.IdempotencyKey)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return TwapJob{}, false, err
	}
	var active int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM trader_twap_jobs
		WHERE trading_account_id=$1 AND status IN ('pending','running')`,
		job.TradingAccountID,
	).Scan(&active); err != nil {
		return TwapJob{}, false, fmt.Errorf("count active twaps: %w", err)
	}
	if active >= 5 {
		return TwapJob{}, false, ErrActiveTwapLimit
	}
	var result TwapJob
	err = tx.QueryRow(ctx, `
		INSERT INTO trader_twap_jobs (
			id, idempotency_key, request_fingerprint, owner_username, trading_account_id,
			product_name, exchange, instrument_id, contract_type, exchange_symbol,
			base_asset, quote_asset, side, total_quantity, start_at, end_at,
			interval_seconds, limit_price, max_quantity, execution_type,
			order_timeout_seconds, status, next_action_at
		) VALUES (
			$1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14::numeric,$15,$16,
			$17,NULLIF($18,'')::numeric,NULLIF($19,'')::numeric,$20,
			NULLIF($21,0),'pending',$22
		)
		RETURNING `+twapColumns,
		job.ID, job.IdempotencyKey, job.RequestFingerprint, job.OwnerUsername,
		job.TradingAccountID, job.ProductName, job.Exchange, job.InstrumentID,
		job.ContractType, job.ExchangeSymbol, job.BaseAsset, job.QuoteAsset, job.Side,
		job.TotalQuantity, job.StartAt, job.EndAt, job.IntervalSeconds, job.LimitPrice,
		job.MaxQuantity, job.ExecutionType, job.OrderTimeoutSeconds, job.NextActionAt,
	).Scan(twapScanTargets(&result)...)
	if err != nil {
		if existing, getErr := getTwapRow(ctx, tx, `WHERE idempotency_key=$1`, job.IdempotencyKey); getErr == nil {
			return existing, false, nil
		}
		return TwapJob{}, false, fmt.Errorf("create twap: %w", err)
	}
	if err := appendTwapEventTx(ctx, tx, result.ID, "created", map[string]any{
		"totalQuantity": result.TotalQuantity,
		"startAt":       result.StartAt,
		"endAt":         result.EndAt,
		"executionType": result.ExecutionType,
	}); err != nil {
		return TwapJob{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TwapJob{}, false, fmt.Errorf("commit create twap: %w", err)
	}
	return result, true, nil
}

func (r *Repository) GetTwapByOwner(ctx context.Context, owner, id string) (TwapJob, error) {
	return getTwapRow(ctx, r.pool, `WHERE owner_username=$1 AND id=$2::uuid`, owner, id)
}

func (r *Repository) GetTwap(ctx context.Context, id string) (TwapJob, error) {
	return getTwapRow(ctx, r.pool, `WHERE id=$1::uuid`, id)
}

func (r *Repository) ListTwaps(
	ctx context.Context,
	owner string,
	accountID int64,
	view string,
	status string,
	limit int,
	cursor string,
) ([]TwapJob, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	viewClause := `status IN ('pending','running')`
	timeColumn := "updated_at"
	if view == "closed" {
		viewClause = `status IN ('completed','partially_completed','canceled','failed')`
		timeColumn = "closed_at"
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+twapColumns+` FROM trader_twap_jobs
		WHERE owner_username=$1
		  AND ($2=0 OR trading_account_id=$2)
		  AND `+viewClause+`
		  AND ($3='' OR status=$3)
		  AND ($4='' OR (`+timeColumn+`,id) < (
		    SELECT `+timeColumn+`,id FROM trader_twap_jobs WHERE id=$4::uuid
		  ))
		ORDER BY `+timeColumn+` DESC NULLS LAST,id DESC
		LIMIT $5`, owner, accountID, status, cursor, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list twaps: %w", err)
	}
	defer rows.Close()
	items := make([]TwapJob, 0, limit+1)
	for rows.Next() {
		var item TwapJob
		if err := rows.Scan(twapScanTargets(&item)...); err != nil {
			return nil, "", fmt.Errorf("scan twap: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1].ID
	}
	return items, next, nil
}

func (r *Repository) ListTwapOrders(ctx context.Context, owner, jobID string) ([]Order, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+orderColumns+` FROM trader_orders
		WHERE owner_username=$1 AND twap_job_id=$2::uuid
		ORDER BY twap_slice_index ASC, twap_attempt_index ASC`, owner, jobID)
	if err != nil {
		return nil, fmt.Errorf("list twap orders: %w", err)
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

func (r *Repository) LeaseDueTwaps(ctx context.Context, limit int, lease time.Duration) ([]TwapJob, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if lease <= 0 {
		lease = 30 * time.Second
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT `+twapColumns+` FROM trader_twap_jobs
		WHERE status IN ('pending','running') AND next_action_at <= now()
		  AND (scheduler_lease_until IS NULL OR scheduler_lease_until < now())
		ORDER BY next_action_at,created_at
		LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("lease twaps: %w", err)
	}
	var items []TwapJob
	for rows.Next() {
		var item TwapJob
		if err := rows.Scan(twapScanTargets(&item)...); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, item)
	}
	rows.Close()
	for _, item := range items {
		if _, err := tx.Exec(ctx, `
			UPDATE trader_twap_jobs
			SET scheduler_lease_until=now()+$2::interval
			WHERE id=$1::uuid`, item.ID, lease.String()); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

func (r *Repository) RefreshTwapProgress(ctx context.Context, id string) (TwapJob, error) {
	var result TwapJob
	err := r.pool.QueryRow(ctx, `
		WITH totals AS (
			SELECT COALESCE(sum(filled_quantity),0) AS filled,
			       CASE WHEN COALESCE(sum(filled_quantity),0)>0
			            THEN sum(filled_quantity*average_price)/sum(filled_quantity)
			            ELSE 0 END AS average
			FROM trader_orders WHERE twap_job_id=$1::uuid
		)
		UPDATE trader_twap_jobs j SET
			filled_quantity=LEAST(j.total_quantity,t.filled),
			average_price=t.average,
			updated_at=now()
		FROM totals t WHERE j.id=$1::uuid
		RETURNING `+twapColumns, id).
		Scan(twapScanTargets(&result)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return TwapJob{}, ErrNotFound
	}
	if err != nil {
		return TwapJob{}, fmt.Errorf("refresh twap progress: %w", err)
	}
	return result, nil
}

func (r *Repository) UpdateTwapSchedule(
	ctx context.Context,
	id string,
	status string,
	slice int,
	attempt int,
	next time.Time,
	activeOrderID string,
	errorMessage string,
) (TwapJob, error) {
	var result TwapJob
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_twap_jobs SET
			status=$2,current_slice=$3,current_attempt=$4,next_action_at=$5,
			active_order_id=NULLIF($6,'')::uuid,error_message=$7,
			started_at=CASE WHEN $2='running' THEN COALESCE(started_at,now()) ELSE started_at END,
			scheduler_lease_until=NULL,scheduler_failures=0,updated_at=now()
		WHERE id=$1::uuid AND status IN ('pending','running')
		RETURNING `+twapColumns,
		id, status, slice, attempt, next, activeOrderID, truncateMessage(errorMessage),
	).Scan(twapScanTargets(&result)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return TwapJob{}, ErrNotFound
	}
	return result, err
}

func (r *Repository) CloseTwap(ctx context.Context, id, status, message string) (TwapJob, error) {
	var result TwapJob
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_twap_jobs SET
			status=$2,error_message=$3,active_order_id=NULL,
			closed_at=COALESCE(closed_at,now()),updated_at=now(),
			next_action_at=COALESCE(closed_at,now()),scheduler_lease_until=NULL
		WHERE id=$1::uuid AND status IN ('pending','running')
		RETURNING `+twapColumns, id, status, truncateMessage(message)).
		Scan(twapScanTargets(&result)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return r.GetTwap(ctx, id)
	}
	return result, err
}

func (r *Repository) AppendTwapEvent(ctx context.Context, id, eventType string, payload map[string]any) error {
	return appendTwapEventTx(ctx, r.pool, id, eventType, payload)
}

func (r *Repository) DeleteClosedTwaps(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM trader_twap_jobs WHERE id IN (
			SELECT id FROM trader_twap_jobs
			WHERE status IN ('completed','partially_completed','canceled','failed')
			  AND closed_at<$1
			ORDER BY closed_at LIMIT $2
			FOR UPDATE SKIP LOCKED
		)`, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("delete closed twaps: %w", err)
	}
	return tag.RowsAffected(), nil
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type execQuerier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func getTwapRow(ctx context.Context, q rowQuerier, clause string, args ...any) (TwapJob, error) {
	var result TwapJob
	err := q.QueryRow(ctx, `SELECT `+twapColumns+` FROM trader_twap_jobs `+clause, args...).
		Scan(twapScanTargets(&result)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return TwapJob{}, ErrNotFound
	}
	if err != nil {
		return TwapJob{}, fmt.Errorf("get twap: %w", err)
	}
	return result, nil
}

func appendTwapEventTx(ctx context.Context, q execQuerier, id, eventType string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(redactPayload(payload))
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `
		INSERT INTO trader_twap_events (twap_job_id,event_type,payload)
		VALUES ($1::uuid,$2,$3::jsonb)`, id, eventType, raw)
	return err
}

const twapColumns = `
	id::text,idempotency_key,request_fingerprint,owner_username,trading_account_id,
	product_name,exchange,instrument_id,contract_type,exchange_symbol,base_asset,quote_asset,
	side,total_quantity::text,filled_quantity::text,average_price::text,
	start_at,end_at,interval_seconds,COALESCE(limit_price::text,''),
	COALESCE(max_quantity::text,''),execution_type,COALESCE(order_timeout_seconds,0),
	status,current_slice,current_attempt,
	CASE WHEN next_action_at='infinity'::timestamptz
		THEN COALESCE(closed_at,updated_at) ELSE next_action_at END,
	COALESCE(active_order_id::text,''),
	COALESCE(scheduler_lease_until,'epoch'::timestamptz),scheduler_failures,error_message,
	created_at,updated_at,COALESCE(started_at,'epoch'::timestamptz),
	COALESCE(closed_at,'epoch'::timestamptz)`

func twapScanTargets(job *TwapJob) []any {
	return []any{
		&job.ID, &job.IdempotencyKey, &job.RequestFingerprint, &job.OwnerUsername,
		&job.TradingAccountID, &job.ProductName, &job.Exchange, &job.InstrumentID,
		&job.ContractType, &job.ExchangeSymbol, &job.BaseAsset, &job.QuoteAsset,
		&job.Side, &job.TotalQuantity, &job.FilledQuantity, &job.AveragePrice,
		&job.StartAt, &job.EndAt, &job.IntervalSeconds, &job.LimitPrice,
		&job.MaxQuantity, &job.ExecutionType, &job.OrderTimeoutSeconds, &job.Status,
		&job.CurrentSlice, &job.CurrentAttempt, &job.NextActionAt, &job.ActiveOrderID,
		&job.SchedulerLeaseUntil, &job.SchedulerFailures, &job.ErrorMessage,
		&job.CreatedAt, &job.UpdatedAt, &job.StartedAt, &job.ClosedAt,
	}
}
