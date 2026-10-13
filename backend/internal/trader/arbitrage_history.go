package trader

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
)

type arbitrageHistoryStore interface {
	ListArbitrageOrderHistory(context.Context, string, string, ArbitrageHistoryQuery) ([]Order, string, error)
	ListArbitrageExecutionHistory(context.Context, string, ArbitrageHistoryQuery) ([]ArbitrageExecution, string, error)
	ListArbitrageEventHistory(context.Context, string, ArbitrageHistoryQuery) ([]ArbitrageEvent, string, error)
}

type historyCursor struct {
	CreatedAt time.Time `json:"createdAt"`
	ID        string    `json:"id"`
}

func normalizeHistoryQuery(query ArbitrageHistoryQuery) (ArbitrageHistoryQuery, historyCursor, error) {
	if query.Limit <= 0 {
		query.Limit = 50
	}
	if query.Limit > 100 || (!query.From.IsZero() && !query.To.IsZero() && query.From.After(query.To)) {
		return ArbitrageHistoryQuery{}, historyCursor{}, ErrInvalidArgument
	}
	query.ExecutionID = strings.TrimSpace(query.ExecutionID)
	if query.ExecutionID != "" && !validUUID(query.ExecutionID) {
		return ArbitrageHistoryQuery{}, historyCursor{}, ErrInvalidArgument
	}
	var cursor historyCursor
	if strings.TrimSpace(query.Cursor) != "" {
		raw, err := base64.RawURLEncoding.DecodeString(query.Cursor)
		if err != nil || json.Unmarshal(raw, &cursor) != nil ||
			cursor.CreatedAt.IsZero() || strings.TrimSpace(cursor.ID) == "" {
			return ArbitrageHistoryQuery{}, historyCursor{}, ErrInvalidArgument
		}
	}
	return query, cursor, nil
}

func encodeHistoryCursor(createdAt time.Time, id string) string {
	raw, _ := json.Marshal(historyCursor{CreatedAt: createdAt.UTC(), ID: id})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (s *Service) ListArbitrageCombinationOrders(
	ctx context.Context, token, combinationID string, query ArbitrageHistoryQuery,
) ([]Order, string, error) {
	owner, store, normalized, err := s.prepareHistory(ctx, token, combinationID, query)
	if err != nil {
		return nil, "", err
	}
	return store.ListArbitrageOrderHistory(ctx, owner, combinationID, normalized)
}

func (s *Service) ListArbitrageCombinationExecutions(
	ctx context.Context, token, combinationID string, query ArbitrageHistoryQuery,
) ([]ArbitrageExecution, string, error) {
	_, store, normalized, err := s.prepareHistory(ctx, token, combinationID, query)
	if err != nil {
		return nil, "", err
	}
	return store.ListArbitrageExecutionHistory(ctx, combinationID, normalized)
}

func (s *Service) ListArbitrageCombinationEvents(
	ctx context.Context, token, combinationID string, query ArbitrageHistoryQuery,
) ([]ArbitrageEvent, string, error) {
	_, store, normalized, err := s.prepareHistory(ctx, token, combinationID, query)
	if err != nil {
		return nil, "", err
	}
	return store.ListArbitrageEventHistory(ctx, combinationID, normalized)
}

func (s *Service) prepareHistory(
	ctx context.Context, token, combinationID string, query ArbitrageHistoryQuery,
) (string, arbitrageHistoryStore, ArbitrageHistoryQuery, error) {
	if s.arbitrage == nil || strings.TrimSpace(token) == "" || !validUUID(combinationID) {
		return "", nil, ArbitrageHistoryQuery{}, ErrInvalidArgument
	}
	normalized, _, err := normalizeHistoryQuery(query)
	if err != nil {
		return "", nil, ArbitrageHistoryQuery{}, err
	}
	owner, err := s.credentials.Owner(ctx, token)
	if err != nil {
		return "", nil, ArbitrageHistoryQuery{}, err
	}
	if _, err = s.arbitrage.GetArbitrageCombinationByOwner(ctx, owner, combinationID); err != nil {
		return "", nil, ArbitrageHistoryQuery{}, err
	}
	store, ok := s.arbitrage.(arbitrageHistoryStore)
	if !ok {
		return "", nil, ArbitrageHistoryQuery{}, ErrNotFound
	}
	return owner, store, normalized, nil
}

func nullableHistoryTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}

func (r *Repository) ListArbitrageOrderHistory(
	ctx context.Context, owner, combinationID string, query ArbitrageHistoryQuery,
) ([]Order, string, error) {
	query, cursor, err := normalizeHistoryQuery(query)
	if err != nil {
		return nil, "", err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+orderColumns+`
		FROM trader_orders
		WHERE owner_username=$1
		  AND EXISTS (
		    SELECT 1 FROM trader_arbitrage_executions e
		    WHERE e.id=trader_orders.arbitrage_execution_id
		      AND e.combination_id=$2::uuid
		  )
		  AND ($3='' OR arbitrage_execution_id=$3::uuid)
		  AND ($4::timestamptz IS NULL OR created_at >= $4)
		  AND ($5::timestamptz IS NULL OR created_at <= $5)
		  AND ($6::timestamptz IS NULL OR (created_at,id) < ($6,$7::uuid))
		ORDER BY created_at DESC,id DESC
		LIMIT $8`,
		owner, combinationID, query.ExecutionID,
		nullableHistoryTime(query.From), nullableHistoryTime(query.To),
		nullableHistoryTime(cursor.CreatedAt), nullableUUID(cursor.ID), query.Limit+1,
	)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := make([]Order, 0, query.Limit+1)
	for rows.Next() {
		var item Order
		if err := rows.Scan(orderScanTargets(&item)...); err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(items) <= query.Limit {
		return items, "", nil
	}
	items = items[:query.Limit]
	last := items[len(items)-1]
	return items, encodeHistoryCursor(last.CreatedAt, last.ID), nil
}

func (r *Repository) ListArbitrageExecutionHistory(
	ctx context.Context, combinationID string, query ArbitrageHistoryQuery,
) ([]ArbitrageExecution, string, error) {
	query, cursor, err := normalizeHistoryQuery(query)
	if err != nil {
		return nil, "", err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+arbitrageExecutionColumns+`
		FROM trader_arbitrage_executions
		WHERE combination_id=$1::uuid
		  AND ($2='' OR id=$2::uuid)
		  AND ($3::timestamptz IS NULL OR created_at >= $3)
		  AND ($4::timestamptz IS NULL OR created_at <= $4)
		  AND ($5::timestamptz IS NULL OR (created_at,id) < ($5,$6::uuid))
		ORDER BY created_at DESC,id DESC
		LIMIT $7`,
		combinationID, query.ExecutionID,
		nullableHistoryTime(query.From), nullableHistoryTime(query.To),
		nullableHistoryTime(cursor.CreatedAt), nullableUUID(cursor.ID), query.Limit+1,
	)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := make([]ArbitrageExecution, 0, query.Limit+1)
	for rows.Next() {
		var item ArbitrageExecution
		if err := rows.Scan(arbitrageExecutionScanTargets(&item)...); err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(items) <= query.Limit {
		return items, "", nil
	}
	items = items[:query.Limit]
	last := items[len(items)-1]
	return items, encodeHistoryCursor(last.CreatedAt, last.ID), nil
}

func (r *Repository) ListArbitrageEventHistory(
	ctx context.Context, combinationID string, query ArbitrageHistoryQuery,
) ([]ArbitrageEvent, string, error) {
	query, cursor, err := normalizeHistoryQuery(query)
	if err != nil {
		return nil, "", err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id::text,combination_id::text,COALESCE(execution_id::text,''),
		       event_type,payload::text,created_at
		FROM trader_arbitrage_events
		WHERE combination_id=$1::uuid
		  AND ($2='' OR execution_id=$2::uuid)
		  AND ($3::timestamptz IS NULL OR created_at >= $3)
		  AND ($4::timestamptz IS NULL OR created_at <= $4)
		  AND ($5::timestamptz IS NULL OR (created_at,id) < ($5,$6::bigint))
		ORDER BY created_at DESC,id DESC
		LIMIT $7`,
		combinationID, query.ExecutionID,
		nullableHistoryTime(query.From), nullableHistoryTime(query.To),
		nullableHistoryTime(cursor.CreatedAt), nullableBigint(cursor.ID), query.Limit+1,
	)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := make([]ArbitrageEvent, 0, query.Limit+1)
	for rows.Next() {
		item, err := scanArbitrageEvent(rows.Scan)
		if err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(items) <= query.Limit {
		return items, "", nil
	}
	items = items[:query.Limit]
	last := items[len(items)-1]
	return items, encodeHistoryCursor(last.CreatedAt, last.ID), nil
}

type eventScanner func(...any) error

func scanArbitrageEvent(scan eventScanner) (ArbitrageEvent, error) {
	var item ArbitrageEvent
	var raw string
	if err := scan(
		&item.ID, &item.CombinationID, &item.ExecutionID,
		&item.Type, &raw, &item.CreatedAt,
	); err != nil {
		return ArbitrageEvent{}, err
	}
	item.Message = raw
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err == nil && payload != nil {
		item.Payload, item.PayloadObject = payload, true
	} else {
		slog.Default().Warn(
			"arbitrage event payload is not an object",
			"eventId", item.ID, "eventType", item.Type,
		)
	}
	return item, nil
}

func nullableUUID(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if _, err := uuid.Parse(value); err != nil {
		return nil
	}
	return value
}

func nullableBigint(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
