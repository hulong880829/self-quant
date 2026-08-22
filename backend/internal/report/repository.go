package report

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) ListProducts(ctx context.Context, owner string) ([]Product, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT p.id,p.owner_username,p.name,p.display_name,p.category,p.strategy,
		       p.base_currency,p.timezone,p.active,p.inception_date::text,
		       count(DISTINCT a.id)::int,
		       COALESCE(latest.closing_equity_usd::text,'0'),
		       COALESCE(latest.pnl_usd::text,'0'),
		       COALESCE(latest.return_rate::text,'0'),
		       GREATEST(p.updated_at,COALESCE(latest.updated_at,p.updated_at))
		FROM products p
		LEFT JOIN trading_accounts a ON a.product_id=p.id
		LEFT JOIN LATERAL (
			SELECT closing_equity_usd,pnl_usd,return_rate,updated_at
			FROM product_daily_snapshots
			WHERE product_id=p.id
			ORDER BY report_date DESC LIMIT 1
		) latest ON TRUE
		WHERE p.owner_username=$1
		GROUP BY p.id,latest.closing_equity_usd,latest.pnl_usd,
		         latest.return_rate,latest.updated_at
		ORDER BY p.name,p.id`, owner)
	if err != nil {
		return nil, fmt.Errorf("list products: %w", err)
	}
	defer rows.Close()
	var items []Product
	for rows.Next() {
		var item Product
		if err := rows.Scan(
			&item.ID, &item.OwnerUsername, &item.Name, &item.DisplayName,
			&item.Category, &item.Strategy, &item.BaseCurrency,
			&item.Timezone, &item.Active, &item.InceptionDate, &item.AccountCount,
			&item.LatestEquityUSD, &item.LatestPnLUSD, &item.LatestReturnRate,
			&item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan product: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *Repository) ListActiveProducts(ctx context.Context) ([]Product, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id,owner_username,name,display_name,category,strategy,
		       base_currency,timezone,active,inception_date::text,updated_at
		FROM products WHERE active=TRUE ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list active products: %w", err)
	}
	defer rows.Close()
	var items []Product
	for rows.Next() {
		var item Product
		if err := rows.Scan(
			&item.ID, &item.OwnerUsername, &item.Name, &item.DisplayName,
			&item.Category, &item.Strategy, &item.BaseCurrency,
			&item.Timezone, &item.Active, &item.InceptionDate, &item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan active product: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *Repository) GetProduct(ctx context.Context, owner string, id int64) (Product, error) {
	items, err := r.ListProducts(ctx, owner)
	if err != nil {
		return Product{}, err
	}
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	return Product{}, ErrNotFound
}

func (r *Repository) ProductDetail(ctx context.Context, owner string, id int64) (ProductDetail, error) {
	product, err := r.GetProduct(ctx, owner, id)
	if err != nil {
		return ProductDetail{}, err
	}
	daily, err := r.ListDailySnapshots(ctx, owner, id, DateRange{Limit: 365})
	if err != nil {
		return ProductDetail{}, err
	}
	flows, err := r.ListCashFlows(ctx, owner, id, DateRange{Limit: 20})
	if err != nil {
		return ProductDetail{}, err
	}
	detail := ProductDetail{Product: product, Daily: daily, RecentCashFlows: flows}
	if len(daily) > 0 {
		detail.LatestSnapshot = &daily[0]
	}
	return detail, nil
}

func normalizeRange(value DateRange) DateRange {
	if value.Limit <= 0 {
		value.Limit = 100
	}
	if value.Limit > 1000 {
		value.Limit = 1000
	}
	return value
}

func (r *Repository) ListDailySnapshots(
	ctx context.Context,
	owner string,
	productID int64,
	dateRange DateRange,
) ([]DailySnapshot, error) {
	dateRange = normalizeRange(dateRange)
	rows, err := r.pool.Query(ctx, `
		SELECT s.id,s.product_id,s.report_date::text,s.opening_equity_usd::text,
		       s.closing_equity_usd::text,s.net_cash_flow_usd::text,s.pnl_usd::text,
		       COALESCE(s.return_rate::text,'0'),
		       COALESCE(s.volume_24h_usd::text,'0'),
		       s.sample_count,s.status,s.finalized_at
		FROM product_daily_snapshots s
		JOIN products p ON p.id=s.product_id
		WHERE p.owner_username=$1 AND s.product_id=$2
		  AND ($3='' OR s.report_date >= $3::date)
		  AND ($4='' OR s.report_date <= $4::date)
		ORDER BY s.report_date DESC LIMIT $5`,
		owner, productID, dateRange.From, dateRange.To, dateRange.Limit)
	if err != nil {
		return nil, fmt.Errorf("list daily snapshots: %w", err)
	}
	defer rows.Close()
	var items []DailySnapshot
	for rows.Next() {
		var item DailySnapshot
		if err := rows.Scan(
			&item.ID, &item.ProductID, &item.ReportDate, &item.OpeningEquityUSD,
			&item.ClosingEquityUSD, &item.NetCashFlowUSD, &item.PnLUSD,
			&item.ReturnRate, &item.Volume24hUSD, &item.SampleCount, &item.Status,
			&item.FinalizedAt,
		); err != nil {
			return nil, fmt.Errorf("scan daily snapshot: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *Repository) ListCashFlows(
	ctx context.Context,
	owner string,
	productID int64,
	dateRange DateRange,
) ([]CashFlow, error) {
	dateRange = normalizeRange(dateRange)
	rows, err := r.pool.Query(ctx, `
		SELECT f.id,f.product_id,f.flow_date::text,f.occurred_at,
		       f.amount_usd::text,f.flow_type,
		       f.note,f.confirmed,COALESCE(f.confirmed_by,''),
		       f.confirmed_at,f.created_by,f.created_at
		FROM product_cash_flows f
		JOIN products p ON p.id=f.product_id
		WHERE p.owner_username=$1 AND f.product_id=$2
		  AND ($3='' OR f.flow_date >= $3::date)
		  AND ($4='' OR f.flow_date <= $4::date)
		ORDER BY f.flow_date DESC,f.id DESC LIMIT $5`,
		owner, productID, dateRange.From, dateRange.To, dateRange.Limit)
	if err != nil {
		return nil, fmt.Errorf("list cash flows: %w", err)
	}
	defer rows.Close()
	var items []CashFlow
	for rows.Next() {
		var item CashFlow
		var confirmedAt *time.Time
		if err := rows.Scan(
			&item.ID, &item.ProductID, &item.FlowDate, &item.OccurredAt, &item.AmountUSD,
			&item.FlowType, &item.Note, &item.Confirmed, &item.ConfirmedBy,
			&confirmedAt, &item.CreatedBy, &item.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan cash flow: %w", err)
		}
		if confirmedAt != nil {
			item.ConfirmedAt = *confirmedAt
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *Repository) CreateCashFlow(
	ctx context.Context,
	owner string,
	productID int64,
	flowDate string,
	occurredAt time.Time,
	amount string,
	flowType string,
	note string,
	confirmed bool,
) (CashFlow, error) {
	var item CashFlow
	var confirmedAt *time.Time
	err := r.pool.QueryRow(ctx, `
		INSERT INTO product_cash_flows (
			product_id,flow_date,occurred_at,amount_usd,flow_type,note,source,confirmed,
			confirmed_by,confirmed_at,created_by)
		SELECT p.id,$3::date,$4,$5::numeric,$6,$7,'manual',$8,
		       CASE WHEN $8 THEN $1 ELSE NULL END,
		       CASE WHEN $8 THEN now() ELSE NULL END,$1
		FROM products p WHERE p.id=$2 AND p.owner_username=$1
		RETURNING id,product_id,flow_date::text,occurred_at,amount_usd::text,flow_type,note,
		          confirmed,COALESCE(confirmed_by,''),confirmed_at,created_by,created_at`,
		owner, productID, flowDate, occurredAt, amount, flowType, note, confirmed,
	).Scan(
		&item.ID, &item.ProductID, &item.FlowDate, &item.OccurredAt, &item.AmountUSD,
		&item.FlowType, &item.Note, &item.Confirmed, &item.ConfirmedBy,
		&confirmedAt, &item.CreatedBy, &item.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CashFlow{}, ErrNotFound
	}
	if err != nil {
		return CashFlow{}, fmt.Errorf("create cash flow: %w", err)
	}
	if confirmedAt != nil {
		item.ConfirmedAt = *confirmedAt
	}
	return item, nil
}

func (r *Repository) InsertEquitySamples(
	ctx context.Context,
	productID int64,
	sampledAt time.Time,
	items []AccountEquity,
) error {
	batch := &pgx.Batch{}
	for _, item := range items {
		batch.Queue(`
			INSERT INTO account_equity_samples (
				product_id,trading_account_id,equity_usd,available_funds_usd,
				source_updated_at,sampled_at)
			SELECT $1,a.id,$3::numeric,NULLIF($4,'')::numeric,$5,$6
			FROM trading_accounts a WHERE a.id=$2 AND a.product_id=$1
			ON CONFLICT (trading_account_id,sampled_at) DO UPDATE SET
				equity_usd=EXCLUDED.equity_usd,
				available_funds_usd=EXCLUDED.available_funds_usd,
				source_updated_at=EXCLUDED.source_updated_at`,
			productID, item.TradingAccountID, item.EquityUSD,
			item.AvailableFundsUSD, item.SourceUpdatedAt, sampledAt.UTC())
	}
	results := r.pool.SendBatch(ctx, batch)
	defer results.Close()
	for range items {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("insert equity sample: %w", err)
		}
	}
	return nil
}

func (r *Repository) FinalizeDate(
	ctx context.Context,
	productID int64,
	reportDate time.Time,
	location *time.Location,
	finalizedAt time.Time,
) (bool, error) {
	date := reportDate.In(location)
	windowStart := time.Date(date.Year(), date.Month(), date.Day(), 8, 55, 0, 0, location)
	windowEnd := time.Date(date.Year(), date.Month(), date.Day(), 9, 0, 5, 0, location)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin daily finalize: %w", err)
	}
	defer tx.Rollback(ctx)

	var closingText string
	var sampleCount int
	err = tx.QueryRow(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (trading_account_id)
			       trading_account_id,equity_usd
			FROM account_equity_samples
			WHERE product_id=$1 AND sampled_at >= $2 AND sampled_at <= $3
			ORDER BY trading_account_id,sampled_at DESC
		)
		SELECT COALESCE(sum(equity_usd),0)::text,count(*)::int FROM latest`,
		productID, windowStart.UTC(), windowEnd.UTC()).Scan(&closingText, &sampleCount)
	if err != nil {
		return false, fmt.Errorf("load closing equity: %w", err)
	}
	if sampleCount == 0 {
		return false, nil
	}
	var openingText, cashFlowText string
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE((
			SELECT closing_equity_usd FROM product_daily_snapshots
			WHERE product_id=$1 AND report_date < $2::date
			ORDER BY report_date DESC LIMIT 1
		),$3::numeric)::text`,
		productID, date.Format(time.DateOnly), closingText).Scan(&openingText); err != nil {
		return false, fmt.Errorf("load opening equity: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(sum(amount_usd),0)::text
		FROM product_cash_flows
		WHERE product_id=$1 AND flow_date=$2::date AND confirmed=TRUE`,
		productID, date.Format(time.DateOnly)).Scan(&cashFlowText); err != nil {
		return false, fmt.Errorf("load daily cash flow: %w", err)
	}
	var volume24hText string
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(sum(COALESCE(f.quote_notional_usd,abs(f.quantity*f.price))),0)::text
		FROM account_trade_fills f
		JOIN trading_accounts a ON a.id=f.trading_account_id
		WHERE a.product_id=$1 AND f.filled_at >= $2 AND f.filled_at < $3`,
		productID, windowEnd.Add(-24*time.Hour).UTC(), windowEnd.UTC(),
	).Scan(&volume24hText); err != nil {
		return false, fmt.Errorf("load 24h trade volume: %w", err)
	}
	opening, err := decimal.NewFromString(openingText)
	if err != nil {
		return false, fmt.Errorf("parse opening equity: %w", err)
	}
	closing, err := decimal.NewFromString(closingText)
	if err != nil {
		return false, fmt.Errorf("parse closing equity: %w", err)
	}
	cashFlow, err := decimal.NewFromString(cashFlowText)
	if err != nil {
		return false, fmt.Errorf("parse cash flow: %w", err)
	}
	pnl := closing.Sub(opening).Sub(cashFlow)
	var returnRate any
	if !opening.IsZero() {
		returnRate = pnl.Div(opening).String()
	}
	status := "final"
	var accountCount int
	if err := tx.QueryRow(ctx,
		`SELECT count(*)::int FROM trading_accounts WHERE product_id=$1`,
		productID).Scan(&accountCount); err != nil {
		return false, fmt.Errorf("count product accounts: %w", err)
	}
	if sampleCount < accountCount {
		status = "partial"
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO product_daily_snapshots (
			product_id,report_date,opening_equity_usd,closing_equity_usd,
			net_cash_flow_usd,pnl_usd,return_rate,volume_24h_usd,
			sample_count,status,finalized_at)
		VALUES ($1,$2::date,$3::numeric,$4::numeric,$5::numeric,$6::numeric,
		        $7::numeric,$8::numeric,$9,$10,$11)
		ON CONFLICT (product_id,report_date) DO UPDATE SET
			opening_equity_usd=EXCLUDED.opening_equity_usd,
			closing_equity_usd=EXCLUDED.closing_equity_usd,
			net_cash_flow_usd=EXCLUDED.net_cash_flow_usd,
			pnl_usd=EXCLUDED.pnl_usd,return_rate=EXCLUDED.return_rate,
			volume_24h_usd=EXCLUDED.volume_24h_usd,
			sample_count=EXCLUDED.sample_count,status=EXCLUDED.status,
			finalized_at=EXCLUDED.finalized_at,updated_at=now()`,
		productID, date.Format(time.DateOnly), opening.String(), closing.String(),
		cashFlow.String(), pnl.String(), returnRate, volume24hText, sampleCount,
		status, finalizedAt.UTC()); err != nil {
		return false, fmt.Errorf("upsert daily snapshot: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit daily finalize: %w", err)
	}
	return true, nil
}
