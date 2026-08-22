package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"selfquant/backend/internal/account/portfolio"
)

type tradeFillStore interface {
	LoadCursor(context.Context, int64) (time.Time, error)
	SaveFills(context.Context, int64, string, []portfolio.TradeFill, time.Time) (int, error)
	SaveSyncError(context.Context, int64, string) error
}

type TradeFillRepository struct {
	pool *pgxpool.Pool
}

func NewTradeFillRepository(pool *pgxpool.Pool) *TradeFillRepository {
	return &TradeFillRepository{pool: pool}
}

func (r *TradeFillRepository) LoadCursor(ctx context.Context, accountID int64) (time.Time, error) {
	var value string
	var cursor *time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT cursor_value, last_fill_at
		FROM trade_sync_cursors
		WHERE trading_account_id=$1`, accountID).Scan(&value, &cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("load trade sync cursor: %w", err)
	}
	if parsed, parseErr := time.Parse(time.RFC3339Nano, value); parseErr == nil {
		return parsed.UTC(), nil
	}
	if cursor == nil {
		return time.Time{}, nil
	}
	return cursor.UTC(), nil
}

func (r *TradeFillRepository) SaveFills(
	ctx context.Context,
	accountID int64,
	exchange string,
	fills []portfolio.TradeFill,
	syncedThrough time.Time,
) (int, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin trade fill transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	inserted := 0
	lastFill := time.Time{}
	for _, fill := range fills {
		tag, insertErr := tx.Exec(ctx, `
			INSERT INTO account_trade_fills (
				trading_account_id, exchange, exchange_fill_id, exchange_order_id,
				symbol, side, quantity, price, quote_notional_usd, fee, fee_currency, filled_at
			) VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,NULLIF($9,'')::numeric,
			          NULLIF($10,'')::numeric,NULLIF($11,''),$12)
			ON CONFLICT (trading_account_id, exchange_fill_id) DO NOTHING`,
			accountID, exchange, fill.ExternalTradeID, fill.OrderID, fill.Symbol,
			fill.Side, fill.Quantity, fill.Price, fill.QuoteNotionalUSD, fill.Fee,
			fill.FeeCurrency, fill.TradedAt,
		)
		if insertErr != nil {
			return 0, fmt.Errorf("insert trade fill: %w", insertErr)
		}
		inserted += int(tag.RowsAffected())
		if fill.TradedAt.After(lastFill) {
			lastFill = fill.TradedAt
		}
	}
	var lastFillValue any
	if !lastFill.IsZero() {
		lastFillValue = lastFill
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO trade_sync_cursors (
			trading_account_id, cursor_value, last_fill_at, last_success_at, last_error
		) VALUES ($1,$2,$3,now(),'')
		ON CONFLICT (trading_account_id) DO UPDATE SET
			cursor_value=EXCLUDED.cursor_value,
			last_fill_at=CASE
				WHEN EXCLUDED.last_fill_at IS NULL THEN trade_sync_cursors.last_fill_at
				ELSE GREATEST(
					COALESCE(trade_sync_cursors.last_fill_at, EXCLUDED.last_fill_at),
					EXCLUDED.last_fill_at
				)
			END,
			last_success_at=EXCLUDED.last_success_at,
			last_error='',
			updated_at=now()`,
		accountID, syncedThrough.UTC().Format(time.RFC3339Nano), lastFillValue,
	)
	if err != nil {
		return 0, fmt.Errorf("save trade sync cursor: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit trade fills: %w", err)
	}
	return inserted, nil
}

func (r *TradeFillRepository) SaveSyncError(
	ctx context.Context,
	accountID int64,
	message string,
) error {
	if len(message) > 1000 {
		message = message[:1000]
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO trade_sync_cursors (trading_account_id, last_error)
		VALUES ($1,$2)
		ON CONFLICT (trading_account_id) DO UPDATE SET
			last_error=EXCLUDED.last_error, updated_at=now()`, accountID, message)
	if err != nil {
		return fmt.Errorf("save trade sync error: %w", err)
	}
	return nil
}
