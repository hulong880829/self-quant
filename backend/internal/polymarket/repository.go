package polymarket

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

type AccountSummary struct {
	TradingAccountID int64
	AccountName      string
	WalletAddress    string
	AvailableBalance string
	PositionValue    string
	TotalAssets      string
	SourceUpdatedAt  time.Time
	Stale            bool
}

type Position struct {
	ID               string
	TradingAccountID int64
	ConditionID      string
	TokenID          string
	Market           string
	Outcome          string
	Size             string
	AveragePrice     string
	CurrentPrice     string
	InitialValue     string
	CurrentValue     string
	CashPnL          string
	PercentPnL       string
	Redeemable       bool
	EndTime          time.Time
	SourceUpdatedAt  time.Time
}

type Order struct {
	ID               string
	IdempotencyKey   string
	CLOBOrderID      string
	TradingAccountID int64
	MarketID         string
	TokenID          string
	Outcome          string
	Side             string
	RequestedAmount  string
	AmountUnit       string
	ExecutionType    string
	LimitPrice       string
	CLOBOrderType    string
	FilledSize       string
	AveragePrice     string
	Status           string
	ErrorCode        string
	ErrorMessage     string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) UpsertMarkets(ctx context.Context, markets []Market) error {
	if len(markets) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, market := range markets {
		batch.Queue(`
			INSERT INTO polymarket_markets (
				id, condition_id, slug, asset, period, title, window_start, window_end,
				up_token_id, down_token_id, tick_size, negative_risk, active, source_updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			ON CONFLICT (id) DO UPDATE SET
				condition_id=EXCLUDED.condition_id, slug=EXCLUDED.slug,
				asset=EXCLUDED.asset, period=EXCLUDED.period, title=EXCLUDED.title,
				window_start=EXCLUDED.window_start, window_end=EXCLUDED.window_end,
				up_token_id=EXCLUDED.up_token_id, down_token_id=EXCLUDED.down_token_id,
				tick_size=EXCLUDED.tick_size, negative_risk=EXCLUDED.negative_risk,
				active=EXCLUDED.active, source_updated_at=EXCLUDED.source_updated_at,
				updated_at=now()`,
			market.ID, market.ConditionID, market.Slug, market.Asset, market.Period,
			market.Title, market.WindowStart, market.WindowEnd, market.UpTokenID,
			market.DownTokenID, market.TickSize, market.NegativeRisk, market.Active,
			market.SourceUpdatedAt,
		)
	}
	results := r.pool.SendBatch(ctx, batch)
	defer results.Close()
	for range markets {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("upsert polymarket market: %w", err)
		}
	}
	return nil
}

func (r *Repository) ListMarkets(ctx context.Context) ([]Market, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, condition_id, slug, asset, period, title, window_start, window_end,
		       up_token_id, down_token_id, tick_size::text, negative_risk, active,
		       source_updated_at
		FROM polymarket_markets
		ORDER BY window_start DESC, asset, period`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Market, 0)
	for rows.Next() {
		var market Market
		if err := rows.Scan(
			&market.ID, &market.ConditionID, &market.Slug, &market.Asset, &market.Period,
			&market.Title, &market.WindowStart, &market.WindowEnd, &market.UpTokenID,
			&market.DownTokenID, &market.TickSize, &market.NegativeRisk, &market.Active,
			&market.SourceUpdatedAt,
		); err != nil {
			return nil, err
		}
		result = append(result, market)
	}
	return result, rows.Err()
}

func (r *Repository) LoadRecentPoints(
	ctx context.Context,
	marketID string,
	since time.Time,
) ([]PricePoint, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT observed_at, COALESCE(open_price::text,''), COALESCE(chainlink_price::text,'')
		FROM polymarket_price_points
		WHERE market_id=$1 AND observed_at >= $2
		ORDER BY observed_at`, marketID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]PricePoint, 0)
	for rows.Next() {
		var point PricePoint
		if err := rows.Scan(&point.Timestamp, &point.OpenPrice, &point.ChainlinkPrice); err != nil {
			return nil, err
		}
		result = append(result, point)
	}
	return result, rows.Err()
}

func (r *Repository) InsertPricePoint(ctx context.Context, marketID string, point PricePoint) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO polymarket_price_points (
			market_id, observed_at, open_price, chainlink_price, price_source
		) VALUES ($1,$2,NULLIF($3,'')::numeric,NULLIF($4,'')::numeric,'chainlink')
		ON CONFLICT (market_id, observed_at) DO UPDATE SET
			open_price=COALESCE(EXCLUDED.open_price, polymarket_price_points.open_price),
			chainlink_price=COALESCE(EXCLUDED.chainlink_price, polymarket_price_points.chainlink_price),
			price_source=EXCLUDED.price_source`,
		marketID, point.Timestamp.Truncate(time.Second), point.OpenPrice, point.ChainlinkPrice)
	return err
}

func (r *Repository) CleanupMarketData(ctx context.Context, before time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(
		ctx,
		`DELETE FROM polymarket_price_points WHERE observed_at < $1`,
		before,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM polymarket_markets m
		WHERE m.window_end < $1
		  AND NOT EXISTS (
		      SELECT 1 FROM polymarket_orders o WHERE o.market_id = m.id
		  )`, before); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repository) UpsertAccountSummary(ctx context.Context, summary AccountSummary) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO polymarket_account_snapshots (
			trading_account_id, available_balance, position_value, total_assets, source_updated_at
		) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (trading_account_id) DO UPDATE SET
			available_balance=EXCLUDED.available_balance,
			position_value=EXCLUDED.position_value,
			total_assets=EXCLUDED.total_assets,
			source_updated_at=EXCLUDED.source_updated_at,
			updated_at=now()`,
		summary.TradingAccountID, summary.AvailableBalance, summary.PositionValue,
		summary.TotalAssets, summary.SourceUpdatedAt)
	return err
}

func (r *Repository) LoadAccountSummary(
	ctx context.Context,
	accountID int64,
) (AccountSummary, error) {
	var summary AccountSummary
	err := r.pool.QueryRow(ctx, `
		SELECT trading_account_id, available_balance::text, position_value::text,
		       total_assets::text, source_updated_at
		FROM polymarket_account_snapshots
		WHERE trading_account_id=$1`, accountID,
	).Scan(
		&summary.TradingAccountID, &summary.AvailableBalance,
		&summary.PositionValue, &summary.TotalAssets, &summary.SourceUpdatedAt,
	)
	return summary, err
}

func (r *Repository) UpsertPositions(
	ctx context.Context,
	accountID int64,
	positions []Position,
) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM polymarket_positions WHERE trading_account_id=$1`, accountID); err != nil {
		return err
	}
	for _, position := range positions {
		_, err := tx.Exec(ctx, `
			INSERT INTO polymarket_positions (
				trading_account_id, token_id, condition_id, market_title, outcome,
				size, average_price, current_price, initial_value, current_value,
				cash_pnl, percent_pnl, redeemable, source_updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			accountID, position.TokenID, position.ConditionID, position.Market,
			position.Outcome, position.Size, position.AveragePrice, position.CurrentPrice,
			position.InitialValue, position.CurrentValue, position.CashPnL,
			position.PercentPnL, position.Redeemable, position.SourceUpdatedAt)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *Repository) LoadPositions(
	ctx context.Context,
	accountID int64,
) ([]Position, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT token_id, condition_id, market_title, outcome, size::text,
		       average_price::text, current_price::text, initial_value::text,
		       current_value::text, cash_pnl::text, percent_pnl::text,
		       redeemable, source_updated_at
		FROM polymarket_positions
		WHERE trading_account_id=$1
		ORDER BY market_title, outcome`, accountID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	positions := make([]Position, 0)
	for rows.Next() {
		var position Position
		position.TradingAccountID = accountID
		if err := rows.Scan(
			&position.TokenID, &position.ConditionID, &position.Market, &position.Outcome,
			&position.Size, &position.AveragePrice, &position.CurrentPrice,
			&position.InitialValue, &position.CurrentValue, &position.CashPnL,
			&position.PercentPnL, &position.Redeemable, &position.SourceUpdatedAt,
		); err != nil {
			return nil, err
		}
		position.ID = position.TokenID
		positions = append(positions, position)
	}
	return positions, rows.Err()
}

func (r *Repository) CreateOrderIntent(
	ctx context.Context,
	order Order,
) (Order, bool, error) {
	if order.ID == "" {
		order.ID = uuid.NewString()
	}
	if order.ExecutionType == "" {
		order.ExecutionType = "book"
	}
	if order.CLOBOrderType == "" {
		order.CLOBOrderType = "FAK"
	}
	var result Order
	err := r.pool.QueryRow(ctx, `
		INSERT INTO polymarket_orders (
			id, idempotency_key, trading_account_id, market_id, token_id,
			outcome, side, requested_amount, amount_unit, execution_type,
			limit_price, clob_order_type, status
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,'')::numeric,$12,'pending')
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id::text, idempotency_key, trading_account_id, market_id, token_id,
		          outcome, side, requested_amount::text, amount_unit, COALESCE(clob_order_id,''),
		          filled_size::text, average_price::text, status, error_code,
		          created_at, updated_at, execution_type, COALESCE(limit_price::text,''),
		          clob_order_type, error_message`,
		order.ID, order.IdempotencyKey, order.TradingAccountID, order.MarketID,
		order.TokenID, order.Outcome, order.Side, order.RequestedAmount, order.AmountUnit,
		order.ExecutionType, order.LimitPrice, order.CLOBOrderType,
	).Scan(orderScanTargets(&result)...)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, getErr := r.GetOrderByIdempotency(ctx, order.IdempotencyKey)
		return existing, false, getErr
	}
	return result, true, err
}

func (r *Repository) GetOrderByIdempotency(ctx context.Context, key string) (Order, error) {
	return r.getOrder(ctx, `WHERE idempotency_key=$1`, key)
}

func (r *Repository) GetOrderByOwner(
	ctx context.Context,
	owner, orderID string,
) (Order, error) {
	return r.getOrder(ctx, `
		JOIN trading_accounts t ON t.id=o.trading_account_id
		WHERE t.owner_username=$1 AND o.id=$2::uuid`, owner, orderID)
}

func (r *Repository) ListSubmissionUnknown(
	ctx context.Context,
	accountID int64,
) ([]Order, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id::text, idempotency_key, trading_account_id, market_id,
		       token_id, outcome, side, requested_amount::text, amount_unit,
		       COALESCE(clob_order_id,''), filled_size::text, average_price::text,
		       status, error_code, created_at, updated_at, execution_type,
		       COALESCE(limit_price::text,''), clob_order_type, error_message
		FROM polymarket_orders
		WHERE trading_account_id=$1 AND status='submission_unknown'
		ORDER BY created_at`, accountID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Order, 0)
	for rows.Next() {
		var order Order
		if err := rows.Scan(orderScanTargets(&order)...); err != nil {
			return nil, err
		}
		result = append(result, order)
	}
	return result, rows.Err()
}

func (r *Repository) LoadOpenOrders(
	ctx context.Context,
	accountID int64,
) ([]OpenOrder, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT clob_order_id, market_id, token_id, outcome, side,
		       COALESCE(limit_price::text, average_price::text, ''),
		       requested_amount::text, filled_size::text, status,
		       clob_order_type, created_at
		FROM polymarket_orders
		WHERE trading_account_id=$1
		  AND clob_order_id IS NOT NULL
		  AND status IN ('open','partially_filled')
		ORDER BY created_at DESC`, accountID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := make([]OpenOrder, 0)
	for rows.Next() {
		var order OpenOrder
		if err := rows.Scan(
			&order.ID, &order.ConditionID, &order.TokenID, &order.Outcome, &order.Side,
			&order.Price, &order.OriginalSize, &order.MatchedSize, &order.Status,
			&order.OrderType, &order.CreatedAt,
		); err != nil {
			return nil, err
		}
		original, _ := decimal.NewFromString(order.OriginalSize)
		matched, _ := decimal.NewFromString(order.MatchedSize)
		order.RemainingSize = original.Sub(matched).String()
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

func (r *Repository) getOrder(ctx context.Context, clause string, args ...any) (Order, error) {
	var result Order
	query := `
		SELECT o.id::text, o.idempotency_key, o.trading_account_id, o.market_id,
		       o.token_id, o.outcome, o.side, o.requested_amount::text, o.amount_unit,
		       COALESCE(o.clob_order_id,''), o.filled_size::text, o.average_price::text,
		       o.status, o.error_code, o.created_at, o.updated_at, o.execution_type,
		       COALESCE(o.limit_price::text,''), o.clob_order_type, o.error_message
		FROM polymarket_orders o ` + clause
	err := r.pool.QueryRow(ctx, query, args...).Scan(orderScanTargets(&result)...)
	return result, err
}

func (r *Repository) UpdateOrderResult(
	ctx context.Context,
	orderID, clobOrderID, status, filledSize, averagePrice, clobOrderType,
	errorCode, errorMessage string,
) (Order, error) {
	var result Order
	err := r.pool.QueryRow(ctx, `
		UPDATE polymarket_orders SET
			clob_order_id=NULLIF($2,''), status=$3, filled_size=$4,
			average_price=$5,
			clob_order_type=COALESCE(NULLIF($6,''),clob_order_type),
			error_code=$7,
			error_message=$8, updated_at=now()
		WHERE id=$1::uuid
		RETURNING id::text, idempotency_key, trading_account_id, market_id,
		          token_id, outcome, side, requested_amount::text, amount_unit,
		          COALESCE(clob_order_id,''), filled_size::text, average_price::text,
		          status, error_code, created_at, updated_at, execution_type,
		          COALESCE(limit_price::text,''), clob_order_type, error_message`,
		orderID, clobOrderID, status, filledSize, averagePrice, clobOrderType,
		errorCode, errorMessage,
	).Scan(orderScanTargets(&result)...)
	return result, err
}

func (r *Repository) MarkOrderCanceled(
	ctx context.Context,
	accountID int64,
	clobOrderID string,
) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE polymarket_orders
		SET status='canceled', error_code='', error_message='', updated_at=now()
		WHERE trading_account_id=$1 AND clob_order_id=$2`,
		accountID, clobOrderID,
	)
	return err
}

func (r *Repository) UpdateOrderFromStream(
	ctx context.Context,
	accountID int64,
	clobOrderID string,
	status string,
	filledSize string,
) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE polymarket_orders
		SET status=$3, filled_size=$4, updated_at=now()
		WHERE trading_account_id=$1 AND clob_order_id=$2`,
		accountID, clobOrderID, status, filledSize,
	)
	return err
}

func orderScanTargets(order *Order) []any {
	return []any{
		&order.ID, &order.IdempotencyKey, &order.TradingAccountID, &order.MarketID,
		&order.TokenID, &order.Outcome, &order.Side, &order.RequestedAmount,
		&order.AmountUnit, &order.CLOBOrderID, &order.FilledSize, &order.AveragePrice,
		&order.Status, &order.ErrorCode, &order.CreatedAt, &order.UpdatedAt,
		&order.ExecutionType, &order.LimitPrice, &order.CLOBOrderType,
		&order.ErrorMessage,
	}
}
