package report

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
		       COALESCE(latest.return_rate::text,''),
		       GREATEST(p.updated_at,COALESCE(latest.updated_at,p.updated_at))
		FROM products p
		JOIN trading_accounts a ON a.product_id=p.id
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
		SELECT p.id,p.owner_username,p.name,p.display_name,p.category,p.strategy,
		       p.base_currency,p.timezone,p.active,p.inception_date::text,p.updated_at
		FROM products p
		WHERE p.active=TRUE
		  AND EXISTS (SELECT 1 FROM trading_accounts a WHERE a.product_id=p.id)
		ORDER BY p.id`)
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
		       COALESCE(s.return_rate::text,''),
		       COALESCE(s.volume_24h_usd::text,'0'),
		       s.sample_count,s.status,s.finalized_at,
		       COALESCE(s.subscription_usd::text,'0'),
		       COALESCE(s.redemption_usd::text,'0'),
		       s.cash_flow_count,s.period_rule_version,
		       s.period_start,s.period_end
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
		var periodStart, periodEnd *time.Time
		if err := rows.Scan(
			&item.ID, &item.ProductID, &item.ReportDate, &item.OpeningEquityUSD,
			&item.ClosingEquityUSD, &item.NetCashFlowUSD, &item.PnLUSD,
			&item.ReturnRate, &item.Volume24hUSD, &item.SampleCount, &item.Status,
			&item.FinalizedAt, &item.SubscriptionUSD, &item.RedemptionUSD,
			&item.CashFlowCount, &item.PeriodRuleVersion, &periodStart, &periodEnd,
		); err != nil {
			return nil, fmt.Errorf("scan daily snapshot: %w", err)
		}
		if periodStart != nil {
			item.PeriodStart = *periodStart
		}
		if periodEnd != nil {
			item.PeriodEnd = *periodEnd
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
		       f.confirmed_at,f.created_by,f.created_at,
		       COALESCE(f.idempotency_key,''),COALESCE(f.period_rule_version,1)
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
			&item.IdempotencyKey, &item.PeriodRuleVersion,
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
	idempotencyKey string,
) (CashFlow, error) {
	var item CashFlow
	var confirmedAt *time.Time
	err := r.pool.QueryRow(ctx, `
		INSERT INTO product_cash_flows (
			product_id,flow_date,occurred_at,amount_usd,flow_type,note,source,confirmed,
			confirmed_by,confirmed_at,created_by,period_rule_version,idempotency_key)
		SELECT p.id,$3::date,$4,$5::numeric,$6,$7,'manual',$8,
		       CASE WHEN $8 THEN $1 ELSE NULL END,
		       CASE WHEN $8 THEN now() ELSE NULL END,$1,$9,$10
		FROM products p WHERE p.id=$2 AND p.owner_username=$1
		RETURNING id,product_id,flow_date::text,occurred_at,amount_usd::text,flow_type,note,
		          confirmed,COALESCE(confirmed_by,''),confirmed_at,created_by,created_at,
		          COALESCE(idempotency_key,''),period_rule_version`,
		owner, productID, flowDate, occurredAt, amount, flowType, note, confirmed,
		PeriodRuleVersion, idempotencyKey,
	).Scan(
		&item.ID, &item.ProductID, &item.FlowDate, &item.OccurredAt, &item.AmountUSD,
		&item.FlowType, &item.Note, &item.Confirmed, &item.ConfirmedBy,
		&confirmedAt, &item.CreatedBy, &item.CreatedAt,
		&item.IdempotencyKey, &item.PeriodRuleVersion,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CashFlow{}, ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return CashFlow{}, fmt.Errorf("%w: %s", ErrDuplicate, idempotencyKey)
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

func finalizeLockKeys(productID int64, reportDate string) (int32, int32) {
	sum := sha256.Sum256([]byte(fmt.Sprintf("report-finalize:%d:%s", productID, reportDate)))
	return int32(binary.BigEndian.Uint32(sum[0:4])), int32(binary.BigEndian.Uint32(sum[4:8]))
}

func (r *Repository) FinalizeDate(
	ctx context.Context,
	productID int64,
	reportDate time.Time,
	location *time.Location,
	finalizedAt time.Time,
) (bool, error) {
	if location == nil {
		location = PeriodLocation()
	}
	date := reportDate.In(location)
	reportDateText := date.Format(time.DateOnly)
	periodStart, periodEnd, err := PeriodBounds(reportDateText)
	if err != nil {
		return false, err
	}
	windowStart := time.Date(date.Year(), date.Month(), date.Day(), 8, 55, 0, 0, location)
	windowEnd := time.Date(date.Year(), date.Month(), date.Day(), 9, 0, 5, 0, location)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin daily finalize: %w", err)
	}
	defer tx.Rollback(ctx)

	lockA, lockB := finalizeLockKeys(productID, reportDateText)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1,$2)`, lockA, lockB); err != nil {
		return false, fmt.Errorf("lock daily finalize: %w", err)
	}

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
	var openingText string
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE((
			SELECT closing_equity_usd FROM product_daily_snapshots
			WHERE product_id=$1 AND report_date < $2::date
			ORDER BY report_date DESC LIMIT 1
		),$3::numeric)::text`,
		productID, reportDateText, closingText).Scan(&openingText); err != nil {
		return false, fmt.Errorf("load opening equity: %w", err)
	}
	flowRows, err := tx.Query(ctx, `
		SELECT occurred_at,amount_usd::text,flow_type
		FROM product_cash_flows
		WHERE product_id=$1 AND confirmed=TRUE
		  AND occurred_at >= $2 AND occurred_at < $3
		ORDER BY occurred_at,id`,
		productID, periodStart.UTC(), periodEnd.UTC())
	if err != nil {
		return false, fmt.Errorf("load period cash flows: %w", err)
	}
	var flows []PeriodCashFlow
	for flowRows.Next() {
		var occurredAt time.Time
		var amountText, flowType string
		if err := flowRows.Scan(&occurredAt, &amountText, &flowType); err != nil {
			flowRows.Close()
			return false, fmt.Errorf("scan period cash flow: %w", err)
		}
		amount, parseErr := decimal.NewFromString(amountText)
		if parseErr != nil {
			flowRows.Close()
			return false, fmt.Errorf("parse cash flow amount: %w", parseErr)
		}
		flows = append(flows, PeriodCashFlow{
			OccurredAt: occurredAt, Amount: amount, FlowType: flowType,
		})
	}
	if err := flowRows.Err(); err != nil {
		flowRows.Close()
		return false, fmt.Errorf("iterate period cash flows: %w", err)
	}
	flowRows.Close()
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
	computed := ComputeDaily(opening, closing, flows, periodStart, periodEnd)
	var returnRate any
	if computed.HasReturn {
		returnRate = computed.ReturnRate.String()
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
	if computed.Invalid {
		status = "invalid"
	}
	if computed.HasSimple {
		slog.Info("report return comparison",
			"product_id", productID,
			"report_date", reportDateText,
			"simple_return", computed.SimpleReturn.String(),
			"dietz_return", returnRate,
			"pnl_usd", computed.PnL.String(),
			"cash_flow_count", computed.CashFlowCount,
			"period_rule_version", PeriodRuleVersion,
		)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO product_daily_snapshots (
			product_id,report_date,opening_equity_usd,closing_equity_usd,
			net_cash_flow_usd,pnl_usd,return_rate,volume_24h_usd,
			sample_count,status,finalized_at,subscription_usd,redemption_usd,
			cash_flow_count,period_rule_version,period_start,period_end)
		VALUES ($1,$2::date,$3::numeric,$4::numeric,$5::numeric,$6::numeric,
		        $7::numeric,$8::numeric,$9,$10,$11,$12::numeric,$13::numeric,
		        $14,$15,$16,$17)
		ON CONFLICT (product_id,report_date) DO UPDATE SET
			opening_equity_usd=EXCLUDED.opening_equity_usd,
			closing_equity_usd=EXCLUDED.closing_equity_usd,
			net_cash_flow_usd=EXCLUDED.net_cash_flow_usd,
			pnl_usd=EXCLUDED.pnl_usd,return_rate=EXCLUDED.return_rate,
			volume_24h_usd=EXCLUDED.volume_24h_usd,
			sample_count=EXCLUDED.sample_count,status=EXCLUDED.status,
			finalized_at=EXCLUDED.finalized_at,updated_at=now(),
			subscription_usd=EXCLUDED.subscription_usd,
			redemption_usd=EXCLUDED.redemption_usd,
			cash_flow_count=EXCLUDED.cash_flow_count,
			period_rule_version=EXCLUDED.period_rule_version,
			period_start=EXCLUDED.period_start,period_end=EXCLUDED.period_end`,
		productID, reportDateText, opening.String(), closing.String(),
		computed.NetCashFlow.String(), computed.PnL.String(), returnRate, volume24hText,
		sampleCount, status, finalizedAt.UTC(), computed.SubscriptionUSD.String(),
		computed.RedemptionUSD.String(), computed.CashFlowCount, PeriodRuleVersion,
		periodStart.UTC(), periodEnd.UTC()); err != nil {
		return false, fmt.Errorf("upsert daily snapshot: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit daily finalize: %w", err)
	}
	return true, nil
}

func (r *Repository) RecomputeDate(
	ctx context.Context,
	productID int64,
	reportDate time.Time,
	location *time.Location,
	finalizedAt time.Time,
) (bool, error) {
	if location == nil {
		location = PeriodLocation()
	}
	date := reportDate.In(location)
	reportDateText := date.Format(time.DateOnly)
	periodStart, periodEnd, err := PeriodBounds(reportDateText)
	if err != nil {
		return false, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin daily recompute: %w", err)
	}
	defer tx.Rollback(ctx)

	lockA, lockB := finalizeLockKeys(productID, reportDateText)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1,$2)`, lockA, lockB); err != nil {
		return false, fmt.Errorf("lock daily recompute: %w", err)
	}

	var openingText, closingText string
	var sampleCount int
	err = tx.QueryRow(ctx, `
		SELECT opening_equity_usd::text, closing_equity_usd::text, sample_count
		FROM product_daily_snapshots
		WHERE product_id=$1 AND report_date=$2::date`,
		productID, reportDateText,
	).Scan(&openingText, &closingText, &sampleCount)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Rollback(ctx); err != nil {
			return false, fmt.Errorf("rollback missing snapshot recompute: %w", err)
		}
		return r.FinalizeDate(ctx, productID, reportDate, location, finalizedAt)
	}
	if err != nil {
		return false, fmt.Errorf("load snapshot for recompute: %w", err)
	}
	flowRows, err := tx.Query(ctx, `
		SELECT occurred_at,amount_usd::text,flow_type
		FROM product_cash_flows
		WHERE product_id=$1 AND confirmed=TRUE
		  AND occurred_at >= $2 AND occurred_at < $3
		ORDER BY occurred_at,id`,
		productID, periodStart.UTC(), periodEnd.UTC())
	if err != nil {
		return false, fmt.Errorf("load period cash flows: %w", err)
	}
	var flows []PeriodCashFlow
	for flowRows.Next() {
		var occurredAt time.Time
		var amountText, flowType string
		if err := flowRows.Scan(&occurredAt, &amountText, &flowType); err != nil {
			flowRows.Close()
			return false, fmt.Errorf("scan period cash flow: %w", err)
		}
		amount, parseErr := decimal.NewFromString(amountText)
		if parseErr != nil {
			flowRows.Close()
			return false, fmt.Errorf("parse cash flow amount: %w", parseErr)
		}
		flows = append(flows, PeriodCashFlow{
			OccurredAt: occurredAt, Amount: amount, FlowType: flowType,
		})
	}
	if err := flowRows.Err(); err != nil {
		flowRows.Close()
		return false, fmt.Errorf("iterate period cash flows: %w", err)
	}
	flowRows.Close()
	opening, err := decimal.NewFromString(openingText)
	if err != nil {
		return false, fmt.Errorf("parse opening equity: %w", err)
	}
	closing, err := decimal.NewFromString(closingText)
	if err != nil {
		return false, fmt.Errorf("parse closing equity: %w", err)
	}
	computed := ComputeDaily(opening, closing, flows, periodStart, periodEnd)
	var returnRate any
	if computed.HasReturn {
		returnRate = computed.ReturnRate.String()
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
	if computed.Invalid {
		status = "invalid"
	}
	if computed.HasSimple {
		slog.Info("report return comparison",
			"product_id", productID,
			"report_date", reportDateText,
			"simple_return", computed.SimpleReturn.String(),
			"dietz_return", returnRate,
			"pnl_usd", computed.PnL.String(),
			"cash_flow_count", computed.CashFlowCount,
			"period_rule_version", PeriodRuleVersion,
			"recompute", true,
		)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE product_daily_snapshots SET
			net_cash_flow_usd=$3::numeric,
			pnl_usd=$4::numeric,
			return_rate=$5::numeric,
			status=$6,
			updated_at=now(),
			subscription_usd=$7::numeric,
			redemption_usd=$8::numeric,
			cash_flow_count=$9,
			period_rule_version=$10,
			period_start=$11,
			period_end=$12
		WHERE product_id=$1 AND report_date=$2::date`,
		productID, reportDateText, computed.NetCashFlow.String(), computed.PnL.String(),
		returnRate, status, computed.SubscriptionUSD.String(), computed.RedemptionUSD.String(),
		computed.CashFlowCount, PeriodRuleVersion, periodStart.UTC(), periodEnd.UTC(),
	); err != nil {
		return false, fmt.Errorf("update recomputed snapshot: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit daily recompute: %w", err)
	}
	return true, nil
}

type recomputeJob struct {
	ID         int64
	ProductID  int64
	ReportDate string
	Attempts   int
}

func (r *Repository) SnapshotExists(
	ctx context.Context,
	productID int64,
	reportDate string,
) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM product_daily_snapshots
			WHERE product_id=$1 AND report_date=$2::date
		)`, productID, reportDate).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("lookup daily snapshot: %w", err)
	}
	return exists, nil
}

func (r *Repository) EnqueueRecompute(
	ctx context.Context,
	productID int64,
	reportDate string,
) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin recompute enqueue: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO product_report_recompute_jobs (
			product_id,report_date,status,before_pnl_usd,before_return_rate)
		SELECT $1,$2::date,'pending',s.pnl_usd,s.return_rate
		FROM product_daily_snapshots s
		WHERE s.product_id=$1 AND s.report_date=$2::date
		ON CONFLICT (product_id,report_date) DO UPDATE SET
			status=CASE
				WHEN product_report_recompute_jobs.status='running' THEN 'running'
				ELSE 'pending'
			END,
			needs_rerun=CASE
				WHEN product_report_recompute_jobs.status='running' THEN TRUE
				ELSE FALSE
			END,
			last_error=CASE
				WHEN product_report_recompute_jobs.status='running'
				THEN product_report_recompute_jobs.last_error
				ELSE ''
			END,
			finished_at=CASE
				WHEN product_report_recompute_jobs.status='running'
				THEN product_report_recompute_jobs.finished_at
				ELSE NULL
			END,
			before_pnl_usd=COALESCE(EXCLUDED.before_pnl_usd, product_report_recompute_jobs.before_pnl_usd),
			before_return_rate=COALESCE(EXCLUDED.before_return_rate, product_report_recompute_jobs.before_return_rate),
			updated_at=now()`,
		productID, reportDate); err != nil {
		return fmt.Errorf("enqueue recompute job: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE product_daily_snapshots
		SET status='recomputing', updated_at=now()
		WHERE product_id=$1 AND report_date=$2::date
		  AND status <> 'recomputing'`,
		productID, reportDate); err != nil {
		return fmt.Errorf("mark snapshot recomputing: %w", err)
	}
	return tx.Commit(ctx)
}

func (r *Repository) ClaimRecomputeJobs(ctx context.Context, limit int) ([]recomputeJob, error) {
	if limit <= 0 {
		limit = 10
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin claim recompute jobs: %w", err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		WITH claimed AS (
			SELECT id
			FROM product_report_recompute_jobs
			WHERE status IN ('pending','failed') AND attempts < 8
			ORDER BY created_at,id
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE product_report_recompute_jobs job
		SET status='running', attempts=job.attempts+1, started_at=now(), updated_at=now()
		FROM claimed
		WHERE job.id=claimed.id
		RETURNING job.id,job.product_id,job.report_date::text,job.attempts`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim recompute jobs: %w", err)
	}
	var items []recomputeJob
	for rows.Next() {
		var item recomputeJob
		if err := rows.Scan(&item.ID, &item.ProductID, &item.ReportDate, &item.Attempts); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan recompute job: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit claim recompute jobs: %w", err)
	}
	return items, nil
}

func (r *Repository) FinishRecomputeJob(
	ctx context.Context,
	jobID int64,
	productID int64,
	reportDate string,
	jobErr error,
) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin finish recompute job: %w", err)
	}
	defer tx.Rollback(ctx)
	if jobErr != nil {
		if _, err := tx.Exec(ctx, `
			UPDATE product_report_recompute_jobs
			SET status='failed', last_error=$2, finished_at=now(), updated_at=now()
			WHERE id=$1`, jobID, jobErr.Error()); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	var pendingRerun bool
	if err := tx.QueryRow(ctx, `
		UPDATE product_report_recompute_jobs job
		SET status=CASE WHEN job.needs_rerun THEN 'pending' ELSE 'done' END,
		    needs_rerun=FALSE, last_error='', updated_at=now(),
		    finished_at=CASE WHEN job.needs_rerun THEN NULL ELSE now() END,
		    after_pnl_usd=s.pnl_usd, after_return_rate=s.return_rate
		FROM product_daily_snapshots s
		WHERE job.id=$1 AND s.product_id=$2 AND s.report_date=$3::date
		RETURNING job.status='pending'`,
		jobID, productID, reportDate,
	).Scan(&pendingRerun); err != nil {
		return fmt.Errorf("finish recompute job: %w", err)
	}
	if pendingRerun {
		if _, err := tx.Exec(ctx, `
			UPDATE product_daily_snapshots
			SET status='recomputing', updated_at=now()
			WHERE product_id=$1 AND report_date=$2::date`,
			productID, reportDate); err != nil {
			return fmt.Errorf("mark snapshot rerun: %w", err)
		}
	}
	return tx.Commit(ctx)
}

type EquityAnomaly struct {
	ProductName      string
	ReportDate       string
	OpeningEquityUSD string
	ClosingEquityUSD string
	NetCashFlowUSD   string
	PnLUSD           string
}

func (r *Repository) ListEquityAnomalies(
	ctx context.Context,
	productName string,
	threshold string,
) ([]EquityAnomaly, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT p.name,s.report_date::text,s.opening_equity_usd::text,
		       s.closing_equity_usd::text,s.net_cash_flow_usd::text,s.pnl_usd::text
		FROM product_daily_snapshots s
		JOIN products p ON p.id=s.product_id
		WHERE ($1='' OR p.name=$1)
		  AND abs(s.closing_equity_usd - s.opening_equity_usd) >= $2::numeric
		  AND abs(s.net_cash_flow_usd) < 1
		ORDER BY s.report_date`, productName, threshold)
	if err != nil {
		return nil, fmt.Errorf("list equity anomalies: %w", err)
	}
	defer rows.Close()
	var items []EquityAnomaly
	for rows.Next() {
		var item EquityAnomaly
		if err := rows.Scan(
			&item.ProductName, &item.ReportDate, &item.OpeningEquityUSD,
			&item.ClosingEquityUSD, &item.NetCashFlowUSD, &item.PnLUSD,
		); err != nil {
			return nil, fmt.Errorf("scan equity anomaly: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *Repository) LoadSnapshot(
	ctx context.Context,
	productName string,
	reportDate string,
) (DailySnapshot, error) {
	var item DailySnapshot
	var periodStart, periodEnd *time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT s.id,s.product_id,s.report_date::text,s.opening_equity_usd::text,
		       s.closing_equity_usd::text,s.net_cash_flow_usd::text,s.pnl_usd::text,
		       COALESCE(s.return_rate::text,''),s.sample_count,s.status,s.finalized_at,
		       COALESCE(s.subscription_usd::text,'0'),COALESCE(s.redemption_usd::text,'0'),
		       s.cash_flow_count,s.period_rule_version,s.period_start,s.period_end
		FROM product_daily_snapshots s
		JOIN products p ON p.id=s.product_id
		WHERE p.name=$1 AND s.report_date=$2::date
		ORDER BY s.id DESC LIMIT 1`, productName, reportDate).Scan(
		&item.ID, &item.ProductID, &item.ReportDate, &item.OpeningEquityUSD,
		&item.ClosingEquityUSD, &item.NetCashFlowUSD, &item.PnLUSD,
		&item.ReturnRate, &item.SampleCount, &item.Status, &item.FinalizedAt,
		&item.SubscriptionUSD, &item.RedemptionUSD, &item.CashFlowCount,
		&item.PeriodRuleVersion, &periodStart, &periodEnd,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return DailySnapshot{}, ErrNotFound
	}
	if err != nil {
		return DailySnapshot{}, fmt.Errorf("load snapshot: %w", err)
	}
	if periodStart != nil {
		item.PeriodStart = *periodStart
	}
	if periodEnd != nil {
		item.PeriodEnd = *periodEnd
	}
	return item, nil
}
