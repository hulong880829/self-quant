package trader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"selfquant/backend/internal/trader/exchange"
)

type orderStore interface {
	CreateIntent(context.Context, Order) (Order, bool, error)
	GetByOwner(context.Context, string, string) (Order, error)
	ListByOwnerAccount(context.Context, string, int64, string, int, string) ([]Order, string, error)
	UpdateResult(context.Context, string, VenueResult) (Order, error)
	AppendEvent(context.Context, string, string, map[string]any) error
}

type Repository struct {
	pool         *pgxpool.Pool
	sqlMetrics   repositorySQLMetrics
	fillAverages *orderFillAverageTracker
}

const (
	maxOrderReconcileFailures     = 10
	maxOrderReconcileUncertainAge = 5 * time.Minute
	arbitrageOrderReconcileError  = "order state remained uncertain after bounded reconciliation"
)

const leaseDueOrdersSQL = `/* trader:lease_due_orders_equivalent */
	WITH candidate_ids AS (
		SELECT id FROM trader_orders
		WHERE status IN ('pending','open','partially_filled','unknown')
		UNION ALL
		SELECT o.id
		FROM trader_orders o
		WHERE o.arbitrage_execution_id IS NOT NULL
		  AND o.reconcile_failures>0
		  AND EXISTS (
			SELECT 1
			FROM trader_arbitrage_executions e
			JOIN trader_arbitrage_combinations c ON c.id=e.combination_id
			WHERE e.id=o.arbitrage_execution_id
			  AND c.status='running'
			  AND c.position_uncertain
			  AND c.error_message=$2
		  )
	)
	SELECT ` + orderColumns + `
	FROM trader_orders
	WHERE id IN (SELECT DISTINCT id FROM candidate_ids)
	  AND next_reconcile_at <= now()
	  AND (reconcile_lease_until IS NULL OR reconcile_lease_until < now())
	ORDER BY next_reconcile_at,updated_at
	LIMIT $1
	FOR UPDATE OF trader_orders SKIP LOCKED`

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{
		pool:         pool,
		fillAverages: newOrderFillAverageTracker(hyperliquidArbitrageFillAverageEnabled),
	}
}

func (r *Repository) CreateIntent(ctx context.Context, order Order) (Order, bool, error) {
	if order.ID == "" {
		order.ID = uuid.NewString()
	}
	if order.ClientOrderID == "" {
		order.ClientOrderID = clientOrderID()
	}
	var result Order
	err := r.pool.QueryRow(ctx, `
		INSERT INTO trader_orders (
			id, idempotency_key, owner_username, trading_account_id, product_name,
			exchange, instrument_id, contract_type, exchange_symbol, client_order_id,
			side, order_type, quantity, price, status, request_fingerprint, base_asset, quote_asset,
			twap_job_id, twap_slice_index, twap_attempt_index,
			arbitrage_execution_id, arbitrage_leg, arbitrage_role, reduce_only
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NULLIF($14,'')::numeric,'pending',$15,$16,$17,
			NULL, NULL, 0,NULLIF($18,'')::uuid,NULLIF($19,''),NULLIF($20,''),$21
		)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING `+orderColumns, orderWriteArgs(order)...).Scan(orderScanTargets(&result)...)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, getErr := r.getByIdempotency(ctx, order.IdempotencyKey)
		return existing, false, getErr
	}
	if err != nil {
		return Order{}, false, fmt.Errorf("create trader order: %w", err)
	}
	if err := r.AppendEvent(ctx, result.ID, "intent", map[string]any{
		"side": result.Side, "orderType": result.OrderType, "quantity": result.Quantity,
		"price": result.Price, "instrumentId": result.InstrumentID,
	}); err != nil {
		return Order{}, false, err
	}
	return result, true, nil
}

func (r *Repository) GetByOwner(ctx context.Context, owner, orderID string) (Order, error) {
	return r.getOrder(ctx, `WHERE owner_username=$1 AND id=$2::uuid`, owner, orderID)
}

func (r *Repository) ListByOwnerAccount(
	ctx context.Context,
	owner string,
	accountID int64,
	view string,
	limit int,
	cursor string,
) ([]Order, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	statusClause := `status IN ('pending','open','partially_filled','unknown')`
	orderClause := `updated_at DESC, id DESC`
	timeColumn := "updated_at"
	if view == "history" {
		statusClause = `status IN ('filled','canceled','rejected','expired')`
		orderClause = `created_at DESC, id DESC`
		timeColumn = "created_at"
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+orderColumns+`
		FROM trader_orders
		WHERE owner_username=$1 AND trading_account_id=$2
		  AND `+statusClause+`
		  AND ($3='' OR (`+timeColumn+`, id) < (
		      SELECT `+timeColumn+`, id FROM trader_orders WHERE id=$3::uuid
		  ))
		ORDER BY `+orderClause+`
		LIMIT $4`, owner, accountID, cursor, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list trader orders: %w", err)
	}
	defer rows.Close()
	result := make([]Order, 0, limit+1)
	for rows.Next() {
		var item Order
		if err := rows.Scan(orderScanTargets(&item)...); err != nil {
			return nil, "", fmt.Errorf("scan trader order: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(result) > limit {
		result = result[:limit]
		next = result[len(result)-1].ID
	}
	return result, next, nil
}

func (r *Repository) UpdateResult(ctx context.Context, orderID string, result VenueResult) (Order, error) {
	updated, _, err := r.UpdateResultWithFillDelta(ctx, orderID, result)
	return updated, err
}

func (r *Repository) UpdateResultWithFillDelta(
	ctx context.Context,
	orderID string,
	result VenueResult,
) (Order, bool, error) {
	eventAt := result.Reference.EventAt
	if eventAt.IsZero() && !result.LocalCommandAck {
		eventAt = time.Now()
	}
	return r.applyOrderUpdate(ctx, orderID, result, eventAt, time.Time{}, false, nil, false)
}

func (r *Repository) DeferReconcile(ctx context.Context, orderID string, next time.Time) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE trader_orders
		SET next_reconcile_at=$2,reconcile_lease_until=NULL
		WHERE id=$1::uuid`, orderID, next)
	return err
}

// ApplyStreamUpdate atomically deduplicates fills and monotonically merges a
// websocket order snapshot. A zero EventAt is treated as the current time.
func (r *Repository) ApplyStreamUpdate(
	ctx context.Context,
	orderID string,
	update StreamUpdate,
) (Order, error) {
	eventAt := update.EventAt
	if eventAt.IsZero() {
		eventAt = time.Now()
	}
	receivedAt := update.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	updated, _, err := r.applyOrderUpdate(
		ctx, orderID, update.Result, eventAt, receivedAt, true, update.Fills,
		update.SkipVenueWatermark,
	)
	return updated, err
}

func (r *Repository) applyOrderUpdate(
	ctx context.Context,
	orderID string,
	result VenueResult,
	eventAt time.Time,
	receivedAt time.Time,
	stream bool,
	fills []OrderFill,
	skipVenueWatermark bool,
) (Order, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Order{}, false, fmt.Errorf("begin trader order update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current Order
	err = tx.QueryRow(ctx, `
		SELECT `+orderColumns+` FROM trader_orders
		WHERE id=$1::uuid FOR UPDATE`, orderID,
	).Scan(orderScanTargets(&current)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, false, ErrNotFound
	}
	if err != nil {
		return Order{}, false, fmt.Errorf("lock trader order: %w", err)
	}

	var (
		handle           orderFillAverageHandle
		pending          orderFillAveragePending
		trackerLocked    bool
		trackerPrepared  bool
		trackerCommitted bool
		markTerminal     bool
		terminalExpiry   time.Time
	)
	track := r.fillAverages.enabled(current)
	if track {
		handle = r.fillAverages.Acquire(current.ID)
		handle.state.mu.Lock()
		trackerLocked = true
		defer func() {
			if !trackerLocked {
				return
			}
			if trackerPrepared && !trackerCommitted {
				r.fillAverages.Abort(pending)
			}
			handle.state.mu.Unlock()
			trackerLocked = false
			r.fillAverages.Release(handle, markTerminal, terminalExpiry)
		}()
	}

	venueOrderID := current.VenueOrderID
	if venueOrderID == "" {
		venueOrderID = strings.TrimSpace(result.VenueOrderID)
	}
	insertedFills := make([]OrderFill, 0, len(fills))
	for _, fill := range fills {
		fill.TradeID = strings.TrimSpace(fill.TradeID)
		fill.Quantity = strings.TrimSpace(fill.Quantity)
		fill.Price = strings.TrimSpace(fill.Price)
		if fill.TradeID == "" || venueOrderID == "" {
			return Order{}, false, fmt.Errorf("%w: fill trade and venue order IDs are required", ErrInvalidArgument)
		}
		quantity, parseErr := decimal.NewFromString(fill.Quantity)
		if parseErr != nil || !quantity.IsPositive() {
			return Order{}, false, fmt.Errorf("%w: invalid fill quantity", ErrInvalidArgument)
		}
		if fill.Price == "" {
			fill.Price = "0"
		}
		price, parseErr := decimal.NewFromString(fill.Price)
		if parseErr != nil || price.IsNegative() {
			return Order{}, false, fmt.Errorf("%w: invalid fill price", ErrInvalidArgument)
		}
		if strings.EqualFold(current.Exchange, "hyperliquid") && !price.IsPositive() {
			continue
		}
		var executedAt any
		if !fill.ExecutedAt.IsZero() {
			executedAt = fill.ExecutedAt
		}
		var inserted int
		err = tx.QueryRow(ctx, `
			INSERT INTO trader_order_fills(
				order_id,trading_account_id,exchange,venue_order_id,trade_id,
				quantity,price,executed_at
			) VALUES($1::uuid,$2,$3,$4,$5,$6::numeric,$7::numeric,$8)
			ON CONFLICT (trading_account_id,exchange,venue_order_id,trade_id) DO NOTHING
			RETURNING 1`,
			current.ID, current.TradingAccountID, current.Exchange, venueOrderID,
			fill.TradeID, quantity.String(), price.String(), executedAt,
		).Scan(&inserted)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return Order{}, false, fmt.Errorf("insert trader order fill: %w", err)
		}
		insertedFills = append(insertedFills, fill)
	}

	merged, err := mergeOrderUpdate(current, result, eventAt, insertedFills, skipVenueWatermark)
	if err != nil {
		return Order{}, false, err
	}
	authoritativeTerminal := isAuthoritativeTerminalResult(result, stream, merged.Status)
	if track {
		authoritative := current.FilledQuantity
		if stream && !skipVenueWatermark {
			authoritative = merged.FilledQuantity
		}
		var average string
		var write bool
		pending, average, write = r.fillAverages.Prepare(
			handle, current, merged.Status, authoritative, fills, insertedFills,
		)
		trackerPrepared = true
		if write {
			merged.AveragePrice = average
		}
	}
	filledQuantityChanged := !parseDecimal(current.FilledQuantity).Equal(parseDecimal(merged.FilledQuantity))
	var streamEventAt, venueEventAt any
	if stream {
		streamEventAt = receivedAt
	}
	if !result.LocalCommandAck && !skipVenueWatermark {
		venueEventAt = eventAt
	}
	var updated Order
	err = tx.QueryRow(ctx, `
		UPDATE trader_orders SET
			venue_order_id=$2,status=$3,filled_quantity=$4::numeric,
			average_price=$5::numeric,error_code=$6,error_message=$7,
			last_stream_event_at=CASE WHEN $8::timestamptz IS NULL THEN last_stream_event_at
				ELSE GREATEST(COALESCE(last_stream_event_at,'-infinity'),$8::timestamptz) END,
			last_venue_event_at=CASE WHEN $9::timestamptz IS NULL THEN last_venue_event_at
				ELSE GREATEST(COALESCE(last_venue_event_at,'-infinity'),$9::timestamptz) END,
			updated_at=now(),
			last_reconciled_at=CASE WHEN $10 THEN last_reconciled_at ELSE now() END,
			reconcile_failures=CASE WHEN $11 THEN 0 WHEN $10 THEN reconcile_failures ELSE 0 END,
			reconcile_lease_until=CASE WHEN $11 THEN NULL WHEN $10 THEN reconcile_lease_until ELSE NULL END,
			next_reconcile_at=CASE
				WHEN $11 THEN 'infinity'::timestamptz
				WHEN $3 IN ('filled','canceled','rejected','expired') THEN next_reconcile_at
				WHEN $3 IN ('pending','unknown','partially_filled') THEN now()+interval '2 seconds'
				ELSE now()+interval '5 seconds'
			END,
			absence_confirmations=0
		WHERE id=$1::uuid
		RETURNING `+orderColumns,
		orderID, merged.VenueOrderID, merged.Status, merged.FilledQuantity,
		merged.AveragePrice, merged.ErrorCode, merged.ErrorMessage,
		streamEventAt, venueEventAt, stream, authoritativeTerminal,
	).Scan(orderScanTargets(&updated)...)
	if err != nil {
		return Order{}, false, fmt.Errorf("merge trader order: %w", err)
	}
	if venueReferencePresent(result.Reference) {
		ref := result.Reference
		if ref.VenueOrderID == "" {
			ref.VenueOrderID = merged.VenueOrderID
		}
		if result.LocalCommandAck && !current.LastVenueEventAt.IsZero() {
			ref.ReconcileStatus = ""
		}
		if ref.EventAt.IsZero() && !result.LocalCommandAck && !skipVenueWatermark {
			ref.EventAt = eventAt
		}
		var refEventAt any
		if !result.LocalCommandAck && !skipVenueWatermark && !ref.EventAt.IsZero() {
			refEventAt = ref.EventAt
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO trader_order_venue_refs(
				order_id,venue_client_order_id,venue_order_id,cloid,tx_hash,nonce,
				account_index,api_key_index,client_order_index,last_venue_event_at,reconcile_status
			) VALUES(
				$1::uuid,NULLIF($2,''),NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),
				CASE WHEN $9::bigint<>0 THEN $6::bigint ELSE NULL END,
				CASE WHEN $9::bigint<>0 THEN $7::bigint ELSE NULL END,
				CASE WHEN $9::bigint<>0 THEN $8::smallint ELSE NULL END,
				NULLIF($9,0),$10,NULLIF($11,'')
			)
			ON CONFLICT(order_id) DO UPDATE SET
				venue_client_order_id=COALESCE(EXCLUDED.venue_client_order_id,trader_order_venue_refs.venue_client_order_id),
				venue_order_id=COALESCE(EXCLUDED.venue_order_id,trader_order_venue_refs.venue_order_id),
				cloid=COALESCE(EXCLUDED.cloid,trader_order_venue_refs.cloid),
				tx_hash=COALESCE(EXCLUDED.tx_hash,trader_order_venue_refs.tx_hash),
				nonce=COALESCE(EXCLUDED.nonce,trader_order_venue_refs.nonce),
				account_index=COALESCE(EXCLUDED.account_index,trader_order_venue_refs.account_index),
				api_key_index=COALESCE(EXCLUDED.api_key_index,trader_order_venue_refs.api_key_index),
				client_order_index=COALESCE(EXCLUDED.client_order_index,trader_order_venue_refs.client_order_index),
				last_venue_event_at=COALESCE(EXCLUDED.last_venue_event_at,trader_order_venue_refs.last_venue_event_at),
				reconcile_status=COALESCE(EXCLUDED.reconcile_status,trader_order_venue_refs.reconcile_status),
				updated_at=now()`,
			orderID, ref.ClientOrderID, ref.VenueOrderID, ref.Cloid, ref.TxHash, ref.Nonce,
			ref.AccountIndex, ref.APIKeyIndex, ref.ClientOrderIndex, refEventAt, ref.ReconcileStatus,
		)
		if err != nil {
			return Order{}, false, fmt.Errorf("upsert trader order venue reference: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Order{}, false, fmt.Errorf("commit trader order update: %w", err)
	}
	if track {
		r.fillAverages.Commit(handle, pending)
		trackerCommitted = true
		if pending.writeAverage {
			slog.Debug(
				"hyperliquid_fill_average_updated",
				slog.String("order_id", current.ID),
				slog.String("average_price", merged.AveragePrice),
				slog.String("filled_quantity", merged.FilledQuantity),
			)
		}
		if r.fillAverages.shouldMarkTerminal(handle, merged.Status) {
			markTerminal = true
			terminalExpiry = time.Now().Add(orderFillAverageTerminalTTL)
		}
		handle.state.mu.Unlock()
		trackerLocked = false
		r.fillAverages.Release(handle, markTerminal, terminalExpiry)
	}
	if updated.TwapJobID != "" {
		if _, refreshErr := r.RefreshTwapProgress(ctx, updated.TwapJobID); refreshErr != nil {
			return updated, filledQuantityChanged, refreshErr
		}
	}
	return updated, filledQuantityChanged, nil
}

func venueReferencePresent(ref exchange.VenueReference) bool {
	return strings.TrimSpace(ref.ClientOrderID) != "" ||
		strings.TrimSpace(ref.VenueOrderID) != "" ||
		strings.TrimSpace(ref.Cloid) != "" ||
		strings.TrimSpace(ref.TxHash) != "" ||
		ref.Nonce != 0 || ref.AccountIndex != 0 || ref.APIKeyIndex != 0 ||
		ref.ClientOrderIndex != 0 || !ref.EventAt.IsZero() ||
		strings.TrimSpace(ref.ReconcileStatus) != ""
}

func mergeOrderUpdate(
	current Order,
	result VenueResult,
	eventAt time.Time,
	insertedFills []OrderFill,
	skipVenueWatermark bool,
) (Order, error) {
	merged := current
	lastEventAt := current.LastVenueEventAt
	if result.LocalCommandAck && hasVenueEventWatermark(lastEventAt) {
		if merged.VenueOrderID == "" && strings.TrimSpace(result.VenueOrderID) != "" {
			merged.VenueOrderID = strings.TrimSpace(result.VenueOrderID)
		}
		oldFilled, err := decimal.NewFromString(current.FilledQuantity)
		if err != nil {
			return Order{}, fmt.Errorf("parse stored filled quantity: %w", err)
		}
		incomingFilled := strings.TrimSpace(result.FilledQuantity)
		if incomingFilled == "" {
			return merged, nil
		}
		candidate, parseErr := decimal.NewFromString(incomingFilled)
		if parseErr != nil || candidate.IsNegative() {
			return Order{}, fmt.Errorf("%w: invalid cumulative filled quantity", ErrInvalidArgument)
		}
		if !candidate.GreaterThan(oldFilled) {
			return merged, nil
		}
		if strings.TrimSpace(result.AveragePrice) == "" {
			return merged, nil
		}
		average, err := decimal.NewFromString(strings.TrimSpace(result.AveragePrice))
		if err != nil || average.IsNegative() {
			return Order{}, fmt.Errorf("%w: invalid average price", ErrInvalidArgument)
		}
		merged.FilledQuantity = candidate.String()
		merged.AveragePrice = average.String()
		return merged, nil
	}

	if skipVenueWatermark {
		// Hyperliquid userFills are audit-only: insert fill rows, but do not
		// change cumulative quantity, status, or last_venue_event_at.
		if merged.VenueOrderID == "" && strings.TrimSpace(result.VenueOrderID) != "" {
			merged.VenueOrderID = strings.TrimSpace(result.VenueOrderID)
		}
		return merged, nil
	}

	fresh := lastEventAt.IsZero() || eventAt.IsZero() || !eventAt.Before(lastEventAt)

	if merged.VenueOrderID == "" && strings.TrimSpace(result.VenueOrderID) != "" {
		merged.VenueOrderID = strings.TrimSpace(result.VenueOrderID)
	}
	oldFilled, err := decimal.NewFromString(current.FilledQuantity)
	if err != nil {
		return Order{}, fmt.Errorf("parse stored filled quantity: %w", err)
	}
	quantity, err := decimal.NewFromString(current.Quantity)
	if err != nil {
		return Order{}, fmt.Errorf("parse stored order quantity: %w", err)
	}
	oldAverage, err := decimal.NewFromString(current.AveragePrice)
	if err != nil {
		return Order{}, fmt.Errorf("parse stored average price: %w", err)
	}
	filled := oldFilled
	average := oldAverage
	incomingFilled := strings.TrimSpace(result.FilledQuantity)
	if incomingFilled != "" {
		candidate, parseErr := decimal.NewFromString(incomingFilled)
		if parseErr != nil || candidate.IsNegative() {
			return Order{}, fmt.Errorf("%w: invalid cumulative filled quantity", ErrInvalidArgument)
		}
		if candidate.GreaterThan(filled) {
			filled = candidate
			if strings.TrimSpace(result.AveragePrice) != "" {
				average, err = decimal.NewFromString(strings.TrimSpace(result.AveragePrice))
				if err != nil || average.IsNegative() {
					return Order{}, fmt.Errorf("%w: invalid average price", ErrInvalidArgument)
				}
			}
		} else if candidate.Equal(filled) && fresh && strings.TrimSpace(result.AveragePrice) != "" {
			average, err = decimal.NewFromString(strings.TrimSpace(result.AveragePrice))
			if err != nil || average.IsNegative() {
				return Order{}, fmt.Errorf("%w: invalid average price", ErrInvalidArgument)
			}
		}
	} else if len(insertedFills) > 0 {
		notional := oldFilled.Mul(oldAverage)
		for _, fill := range insertedFills {
			fillQuantity, _ := decimal.NewFromString(fill.Quantity)
			fillPrice, _ := decimal.NewFromString(fill.Price)
			filled = filled.Add(fillQuantity)
			notional = notional.Add(fillQuantity.Mul(fillPrice))
		}
		if filled.IsPositive() {
			average = notional.Div(filled)
		}
	}
	if quantity.IsPositive() && filled.GreaterThanOrEqual(quantity) {
		filled = quantity
	}
	merged.FilledQuantity = filled.String()
	merged.AveragePrice = average.String()

	incomingStatus := ""
	if fresh {
		incomingStatus = strings.TrimSpace(result.Status)
	}
	merged.Status, err = mergeOrderStatus(current.Status, incomingStatus, filled, quantity)
	if err != nil {
		return Order{}, err
	}
	if fresh {
		merged.ErrorCode = result.ErrorCode
		merged.ErrorMessage = truncateMessage(result.ErrorMessage)
	}
	return merged, nil
}

func mergeOrderStatus(
	current, incoming string,
	filled, quantity decimal.Decimal,
) (string, error) {
	if quantity.IsPositive() && filled.GreaterThanOrEqual(quantity) {
		return "filled", nil
	}
	valid := map[string]bool{
		"pending": true, "unknown": true, "open": true, "partially_filled": true,
		"filled": true, "canceled": true, "rejected": true, "expired": true,
	}
	if !valid[current] {
		return "", fmt.Errorf("invalid stored order status %q", current)
	}
	if current == "filled" || terminalStatus(current) {
		return current, nil
	}
	if incoming == "" {
		if filled.IsPositive() {
			return "partially_filled", nil
		}
		return current, nil
	}
	if !valid[incoming] {
		return "", fmt.Errorf("%w: invalid order status %q", ErrInvalidArgument, incoming)
	}
	if terminalStatus(incoming) {
		return incoming, nil
	}
	rank := map[string]int{"unknown": 0, "pending": 1, "open": 2, "partially_filled": 3}
	if rank[incoming] < rank[current] {
		return current, nil
	}
	if filled.IsPositive() && incoming != "filled" {
		return "partially_filled", nil
	}
	return incoming, nil
}

func (r *Repository) AppendEvent(
	ctx context.Context,
	orderID string,
	eventType string,
	payload map[string]any,
) error {
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(redactPayload(payload))
	if err != nil {
		return fmt.Errorf("encode trader order event: %w", err)
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO trader_order_events (order_id, event_type, payload)
		VALUES ($1::uuid, $2, $3::jsonb)`, orderID, eventType, raw)
	if err != nil {
		return fmt.Errorf("append trader order event: %w", err)
	}
	return nil
}

func (r *Repository) LeaseDueOrders(
	ctx context.Context,
	limit int,
	lease time.Duration,
) (items []Order, resultErr error) {
	started := time.Now()
	r.sqlMetrics.leaseCalls.Add(1)
	defer func() {
		r.sqlMetrics.leaseDurationMS.Add(
			uint64(time.Since(started).Milliseconds()),
		)
		if resultErr != nil {
			r.sqlMetrics.leaseErrors.Add(1)
		}
	}()
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if lease <= 0 {
		lease = 30 * time.Second
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin reconcile lease: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(
		ctx, leaseDueOrdersSQL, limit, arbitrageOrderReconcileError,
	)
	if err != nil {
		return nil, fmt.Errorf("lease due trader orders: %w", err)
	}
	items = make([]Order, 0, limit)
	for rows.Next() {
		var item Order
		if err := rows.Scan(orderScanTargets(&item)...); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan reconcile order: %w", err)
		}
		items = append(items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	r.sqlMetrics.leaseReturned.Add(uint64(len(items)))
	if len(items) > 0 {
		ids := make([]string, len(items))
		for index := range items {
			ids[index] = items[index].ID
		}
		tag, err := tx.Exec(ctx, `
			/* trader:lease_due_orders_bulk_update */
			UPDATE trader_orders
			SET reconcile_lease_until=now()+$2::interval
			WHERE id=ANY($1::uuid[])`, ids, lease.String())
		if err != nil {
			return nil, fmt.Errorf("set reconcile lease: %w", err)
		}
		if tag.RowsAffected() != int64(len(items)) {
			return nil, fmt.Errorf(
				"set reconcile lease: updated %d trader orders, expected %d",
				tag.RowsAffected(), len(items),
			)
		}
		r.sqlMetrics.leaseWrites.Add(uint64(tag.RowsAffected()))
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit reconcile lease: %w", err)
	}
	return items, nil
}

func (r *Repository) MarkReconcileFailure(
	ctx context.Context,
	orderID string,
	next time.Time,
) error {
	_, err := r.pool.Exec(ctx, `
		WITH failed AS (
			UPDATE trader_orders SET
				reconcile_failures=reconcile_failures+1,
				absence_confirmations=0,
				reconcile_lease_until=NULL,
				next_reconcile_at=$2,
				last_reconciled_at=now()
			WHERE id=$1::uuid
			  AND status IN ('pending','open','partially_filled','unknown')
			RETURNING arbitrage_execution_id,reconcile_failures,created_at
		),
		uncertain_combinations AS (
			SELECT DISTINCT e.combination_id
			FROM failed f
			JOIN trader_arbitrage_executions e
			  ON e.id=f.arbitrage_execution_id
			WHERE f.reconcile_failures >= $3
			   OR now()-f.created_at >= $4::interval
		)
		UPDATE trader_arbitrage_combinations c SET
			position_uncertain=TRUE,
			runtime_state='position_uncertain',
			error_message=$5,
			updated_at=now(),
			version=version+1
		FROM uncertain_combinations u
		WHERE c.id=u.combination_id
		  AND c.status IN ('running','closing')`,
		orderID, next, maxOrderReconcileFailures, maxOrderReconcileUncertainAge.String(),
		arbitrageOrderReconcileError,
	)
	if err != nil {
		return fmt.Errorf("mark reconcile failure: %w", err)
	}
	return nil
}

func (r *Repository) ConfirmOrderAbsence(
	ctx context.Context,
	orderID string,
	expectedUpdatedAt time.Time,
	next time.Time,
) (Order, bool, error) {
	if next.IsZero() {
		next = time.Now().UTC().Add(2 * time.Second)
	}
	var updated Order
	err := r.pool.QueryRow(ctx, `
		UPDATE trader_orders SET
			absence_confirmations=absence_confirmations+1,
			reconcile_lease_until=NULL,
			last_reconciled_at=now(),
			updated_at=now(),
			status=CASE WHEN absence_confirmations+1 >= $4 THEN 'rejected' ELSE status END,
			error_code=CASE WHEN absence_confirmations+1 >= $4 THEN $5 ELSE error_code END,
			error_message=CASE WHEN absence_confirmations+1 >= $4 THEN $6 ELSE error_message END,
			filled_quantity=CASE WHEN absence_confirmations+1 >= $4 THEN 0 ELSE filled_quantity END,
			reconcile_failures=CASE WHEN absence_confirmations+1 >= $4 THEN 0 ELSE reconcile_failures END,
			next_reconcile_at=CASE
				WHEN absence_confirmations+1 >= $4 THEN 'infinity'::timestamptz
				ELSE $3
			END
		WHERE id=$1::uuid
		  AND updated_at=$2
		  AND venue_order_id=''
		  AND filled_quantity=0
		  AND status IN ('pending','open','partially_filled','unknown')
		  AND NOT EXISTS (
			SELECT 1 FROM trader_order_fills f WHERE f.order_id=trader_orders.id
		  )
		RETURNING `+orderColumns,
		orderID, expectedUpdatedAt, next, confirmedAbsentRejectAfter,
		errorConfirmedAbsentAfterUncertainSubmit,
		"venue confirmed the order was never accepted",
	).Scan(orderScanTargets(&updated)...)
	if errors.Is(err, pgx.ErrNoRows) {
		current, getErr := r.getOrder(ctx, `WHERE id=$1::uuid`, orderID)
		if getErr != nil {
			return Order{}, false, getErr
		}
		return current, false, nil
	}
	if err != nil {
		return Order{}, false, fmt.Errorf("confirm order absence: %w", err)
	}
	return updated, confirmedAbsentReliableZeroFill(updated), nil
}

func (r *Repository) getByIdempotency(ctx context.Context, key string) (Order, error) {
	return r.getOrder(ctx, `WHERE idempotency_key=$1`, key)
}

func (r *Repository) getOrder(ctx context.Context, clause string, args ...any) (Order, error) {
	var result Order
	err := r.pool.QueryRow(ctx, `SELECT `+orderColumns+` FROM trader_orders `+clause, args...).
		Scan(orderScanTargets(&result)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, fmt.Errorf("get trader order: %w", err)
	}
	return result, nil
}

const orderColumns = `
	id::text, idempotency_key, owner_username, trading_account_id, product_name,
	exchange, instrument_id, contract_type, exchange_symbol, client_order_id,
	base_asset, quote_asset,
	COALESCE(venue_order_id,''), side, order_type, trim_scale(quantity)::text,
	COALESCE(trim_scale(price)::text,''), trim_scale(filled_quantity)::text,
	trim_scale(average_price)::text,
	status, error_code, error_message, request_fingerprint, created_at, updated_at,
	COALESCE(last_reconciled_at, 'epoch'::timestamptz),
	reconcile_failures,
	CASE
		WHEN last_reconciled_at IS NULL THEN 'pending'
		WHEN reconcile_failures > 0 THEN 'delayed'
		ELSE 'synced'
	END,
	COALESCE(last_stream_event_at, 'epoch'::timestamptz),
	COALESCE(last_venue_event_at, 'epoch'::timestamptz),
	COALESCE(twap_job_id::text,''), COALESCE(twap_slice_index,0),
	COALESCE(twap_attempt_index,0),COALESCE(arbitrage_execution_id::text,''),
	COALESCE(arbitrage_leg,''),COALESCE(arbitrage_role,''),COALESCE(reduce_only,FALSE),
	COALESCE(absence_confirmations,0)`

func orderScanTargets(order *Order) []any {
	return []any{
		&order.ID, &order.IdempotencyKey, &order.OwnerUsername, &order.TradingAccountID,
		&order.ProductName, &order.Exchange, &order.InstrumentID, &order.ContractType,
		&order.ExchangeSymbol, &order.ClientOrderID, &order.BaseAsset, &order.QuoteAsset,
		&order.VenueOrderID, &order.Side,
		&order.OrderType, &order.Quantity, &order.Price, &order.FilledQuantity,
		&order.AveragePrice, &order.Status, &order.ErrorCode, &order.ErrorMessage,
		&order.RequestFingerprint, &order.CreatedAt, &order.UpdatedAt,
		&order.LastReconciledAt, &order.ReconcileFailures, &order.SyncState,
		&order.LastStreamEventAt, &order.LastVenueEventAt,
		&order.TwapJobID, &order.TwapSliceIndex, &order.TwapAttemptIndex,
		&order.ArbitrageExecutionID, &order.ArbitrageLeg, &order.ArbitrageRole, &order.ReduceOnly,
		&order.AbsenceConfirmations,
	}
}

func orderWriteArgs(order Order) []any {
	return []any{
		order.ID, order.IdempotencyKey, order.OwnerUsername, order.TradingAccountID,
		order.ProductName, order.Exchange, order.InstrumentID, order.ContractType,
		order.ExchangeSymbol, order.ClientOrderID, order.Side, order.OrderType,
		order.Quantity, order.Price, order.RequestFingerprint, order.BaseAsset, order.QuoteAsset,
		order.ArbitrageExecutionID, order.ArbitrageLeg, order.ArbitrageRole, order.ReduceOnly,
	}
}

func clientOrderID() string {
	return "sq" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
}

func truncateMessage(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 500 {
		return value[:500]
	}
	return value
}

func redactPayload(payload map[string]any) map[string]any {
	redacted := make(map[string]any, len(payload))
	for key, value := range payload {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "secret") || strings.Contains(lower, "key") ||
			strings.Contains(lower, "pass") || strings.Contains(lower, "token") {
			continue
		}
		redacted[key] = value
	}
	return redacted
}
