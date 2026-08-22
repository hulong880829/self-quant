package funding

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"selfquant/backend/internal/exchange"
)

type Repository struct{ pool *pgxpool.Pool }

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

type Rate struct {
	InstrumentID        int64
	Exchange            string
	ExchangeSymbol      string
	GlobalSymbol        string
	BaseAsset           string
	QuoteAsset          string
	Rate                float64
	FundingTime         time.Time
	Settled             bool
	IntervalHours       float64
	AnnualizedRate      float64
	Cumulative24h       float64
	Cumulative7d        float64
	History             []HistoryPoint
	NextRate            *float64
	MarkPrice           float64
	IndexPrice          float64
	LastPrice           float64
	PositionQuantity    float64
	PositionNotionalUSD float64
	Volume24hBase       float64
	Turnover24hUSD      float64
	PriceChange24h      float64
	SourceUpdatedAt     time.Time
}

type HistoryPoint struct {
	Rate      float64
	SettledAt time.Time
}

type Query struct {
	Exchange string
	Symbol   string
	Limit    int
	Offset   int
}

func (r *Repository) UpsertInstruments(ctx context.Context, instruments []exchange.Instrument) error {
	batch := &pgx.Batch{}
	for _, item := range instruments {
		metadata := item.Metadata
		if len(metadata) == 0 {
			metadata = []byte(`{}`)
		}
		batch.Queue(`
			INSERT INTO instruments (
				exchange, exchange_symbol, base_asset, quote_asset, global_symbol,
				active, settle_asset, contract_type, status, contract_size,
				price_tick, quantity_step, funding_interval_seconds, metadata,
				source_updated_at)
			VALUES ($1,$2,$3,$4,$5,TRUE,$6,$7,$8,NULLIF($9::numeric,0),NULLIF($10::numeric,0),
			        NULLIF($11::numeric,0),$12,$13,$14)
			ON CONFLICT (exchange, contract_type, exchange_symbol) DO UPDATE SET
				base_asset=EXCLUDED.base_asset, quote_asset=EXCLUDED.quote_asset,
				global_symbol=EXCLUDED.global_symbol, active=TRUE,
				settle_asset=EXCLUDED.settle_asset, contract_type=EXCLUDED.contract_type,
				status=EXCLUDED.status, contract_size=EXCLUDED.contract_size,
				price_tick=EXCLUDED.price_tick, quantity_step=EXCLUDED.quantity_step,
				funding_interval_seconds=EXCLUDED.funding_interval_seconds,
				metadata=EXCLUDED.metadata, source_updated_at=EXCLUDED.source_updated_at,
				updated_at=now()`,
			item.Exchange, item.ExchangeSymbol, item.BaseAsset, item.QuoteAsset,
			item.GlobalSymbol, item.SettleAsset, item.ContractType, item.Status,
			item.ContractSize, item.PriceTick, item.QuantityStep,
			int32(item.IntervalHours*3600), metadata, item.SourceUpdatedAt)
	}
	results := r.pool.SendBatch(ctx, batch)
	defer results.Close()
	for range instruments {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("upsert instruments: %w", err)
		}
	}
	return nil
}

func (r *Repository) DeactivateMissingInstruments(
	ctx context.Context,
	exchangeName string,
	contractType string,
	instruments []exchange.Instrument,
) error {
	if len(instruments) == 0 {
		return nil
	}
	symbols := make([]string, 0, len(instruments))
	for _, instrument := range instruments {
		symbols = append(symbols, instrument.ExchangeSymbol)
	}
	if _, err := r.pool.Exec(ctx, `
		UPDATE instruments
		SET active=FALSE, status='inactive', updated_at=now()
		WHERE exchange=$1 AND contract_type=$2
		  AND active=TRUE
		  AND NOT (exchange_symbol=ANY($3))`,
		exchangeName, contractType, symbols); err != nil {
		return fmt.Errorf("deactivate missing instruments: %w", err)
	}
	return nil
}

func (r *Repository) ListInstruments(ctx context.Context) ([]exchange.Instrument, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT exchange,exchange_symbol,base_asset,quote_asset,global_symbol,
		       funding_interval_seconds,settle_asset,contract_type,status,
		       COALESCE(contract_size::float8,0),COALESCE(price_tick::float8,0),
		       COALESCE(quantity_step::float8,0),metadata,
		       COALESCE(source_updated_at,updated_at)
		FROM instruments
		WHERE active=TRUE AND status='active'
		ORDER BY exchange,contract_type,exchange_symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []exchange.Instrument
	for rows.Next() {
		var item exchange.Instrument
		var intervalSeconds int32
		if err := rows.Scan(
			&item.Exchange, &item.ExchangeSymbol, &item.BaseAsset, &item.QuoteAsset,
			&item.GlobalSymbol, &intervalSeconds, &item.SettleAsset,
			&item.ContractType, &item.Status, &item.ContractSize, &item.PriceTick,
			&item.QuantityStep, &item.Metadata, &item.SourceUpdatedAt,
		); err != nil {
			return nil, err
		}
		item.IntervalHours = float64(intervalSeconds) / 3600
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *Repository) UpsertRates(ctx context.Context, rates []exchange.FundingRate) error {
	batch := &pgx.Batch{}
	for _, item := range rates {
		if item.Settled {
			batch.Queue(`
				INSERT INTO funding_rates (
					instrument_id, funding_rate, funding_time, record_kind,
					interval_hours, annualized_rate, source_updated_at, received_at)
				SELECT id,$3,$4,'settled',$5,$3*(24.0/$5)*365.0,$6,now()
				FROM instruments
				WHERE exchange=$1 AND contract_type='perpetual' AND exchange_symbol=$2
				  AND active=TRUE AND status='active'
				ON CONFLICT (instrument_id, funding_time) WHERE record_kind='settled'
				DO UPDATE SET funding_rate=EXCLUDED.funding_rate,
					interval_hours=EXCLUDED.interval_hours,
					annualized_rate=EXCLUDED.annualized_rate,
					source_updated_at=EXCLUDED.source_updated_at, updated_at=now()`,
				item.Exchange, item.ExchangeSymbol, item.Rate, item.FundingTime,
				item.IntervalHours, item.SourceUpdatedAt)
		} else {
			batch.Queue(`
				INSERT INTO funding_rates (
					instrument_id, funding_rate, funding_time, record_kind,
					interval_hours, next_funding_rate, annualized_rate,
					next_funding_at, mark_price, index_price, last_price,
					open_interest_contracts, open_interest_base,
					open_interest_notional_usd, volume_24h_base, turnover_24h_usd,
					price_change_24h, source_updated_at, received_at)
				SELECT id,$3,$4,'current',$5,$6,NULL,
				       $4,NULLIF($7::numeric,0),NULLIF($8::numeric,0),NULLIF($9::numeric,0),
				       NULLIF($10::numeric,0),NULLIF($11::numeric,0),NULLIF($12::numeric,0),
				       NULLIF($13::numeric,0),NULLIF($14::numeric,0),$15,$16,now()
				FROM instruments
				WHERE exchange=$1 AND contract_type='perpetual' AND exchange_symbol=$2
				  AND active=TRUE AND status='active'
				ON CONFLICT (instrument_id) WHERE record_kind='current' DO UPDATE SET
					funding_rate=EXCLUDED.funding_rate, funding_time=EXCLUDED.funding_time,
					interval_hours=EXCLUDED.interval_hours,
					next_funding_rate=EXCLUDED.next_funding_rate,
					next_funding_at=EXCLUDED.next_funding_at,
					mark_price=EXCLUDED.mark_price, index_price=EXCLUDED.index_price,
					last_price=EXCLUDED.last_price,
					open_interest_contracts=EXCLUDED.open_interest_contracts,
					open_interest_base=EXCLUDED.open_interest_base,
					open_interest_notional_usd=EXCLUDED.open_interest_notional_usd,
					volume_24h_base=EXCLUDED.volume_24h_base,
					turnover_24h_usd=EXCLUDED.turnover_24h_usd,
					price_change_24h=EXCLUDED.price_change_24h,
					source_updated_at=EXCLUDED.source_updated_at,
					received_at=now(), updated_at=now()`,
				item.Exchange, item.ExchangeSymbol, item.Rate, item.FundingTime,
				item.IntervalHours, item.NextRate, item.MarkPrice, item.IndexPrice,
				item.LastPrice, item.OpenInterestContracts, item.OpenInterestBase,
				item.OpenInterestNotionalUSD, item.Volume24hBase,
				item.Turnover24hUSD, item.PriceChange24h, item.SourceUpdatedAt)
		}
	}
	results := r.pool.SendBatch(ctx, batch)
	defer results.Close()
	for range rates {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("upsert funding rates: %w", err)
		}
	}
	return nil
}

func instrumentKey(exchangeName, symbol string) string {
	return exchangeName + "\x00" + symbol
}

func (r *Repository) HistoryWatermarks(ctx context.Context) (map[string]time.Time, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT i.exchange,i.exchange_symbol,max(f.funding_time)
		FROM instruments i
		LEFT JOIN funding_rates f
		  ON f.instrument_id=i.id AND f.record_kind='settled'
		WHERE i.active=TRUE AND i.status='active' AND i.contract_type='perpetual'
		GROUP BY i.id,i.exchange,i.exchange_symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]time.Time)
	for rows.Next() {
		var exchangeName, symbol string
		var watermark *time.Time
		if err := rows.Scan(&exchangeName, &symbol, &watermark); err != nil {
			return nil, err
		}
		if watermark != nil {
			result[instrumentKey(exchangeName, symbol)] = watermark.UTC()
		}
	}
	return result, rows.Err()
}

func (r *Repository) HistoryStarts(ctx context.Context) (map[string]time.Time, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT i.exchange,i.exchange_symbol,min(f.funding_time)
		FROM instruments i
		LEFT JOIN funding_rates f
		  ON f.instrument_id=i.id AND f.record_kind='settled'
		WHERE i.active=TRUE AND i.status='active' AND i.contract_type='perpetual'
		GROUP BY i.id,i.exchange,i.exchange_symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]time.Time)
	for rows.Next() {
		var exchangeName, symbol string
		var start *time.Time
		if err := rows.Scan(&exchangeName, &symbol, &start); err != nil {
			return nil, err
		}
		if start != nil {
			result[instrumentKey(exchangeName, symbol)] = start.UTC()
		}
	}
	return result, rows.Err()
}

func (r *Repository) UpdateInstrumentIntervals(ctx context.Context, rates []exchange.FundingRate) error {
	batch := &pgx.Batch{}
	for _, rate := range rates {
		if rate.Settled || rate.IntervalHours <= 0 {
			continue
		}
		batch.Queue(`
			UPDATE instruments
			SET funding_interval_seconds=$3,updated_at=now()
			WHERE exchange=$1 AND contract_type='perpetual' AND exchange_symbol=$2
			  AND active=TRUE AND status='active'`,
			rate.Exchange, rate.ExchangeSymbol, int32(rate.IntervalHours*3600))
	}
	results := r.pool.SendBatch(ctx, batch)
	defer results.Close()
	for _, rate := range rates {
		if rate.Settled || rate.IntervalHours <= 0 {
			continue
		}
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("update funding interval: %w", err)
		}
	}
	return nil
}

func (r *Repository) RefreshAggregates(ctx context.Context) error {
	if _, err := r.pool.Exec(ctx, `
		UPDATE funding_rates
		SET cumulative_24h=0,cumulative_7d=0,annualized_rate=NULL,updated_at=now()
		WHERE record_kind='current';

		WITH aggregate AS (
			SELECT instrument_id,
			       COALESCE(SUM(funding_rate) FILTER (
			           WHERE funding_time >= now()-interval '24 hours'),0) AS cumulative_24h,
			       COALESCE(SUM(funding_rate) FILTER (
			           WHERE funding_time >= now()-interval '7 days'),0) AS cumulative_7d,
			       SUM(funding_rate) AS total_rate,
			       MIN(funding_time) AS coverage_start,
			       MAX(funding_time + interval_hours * interval '1 hour') AS coverage_end
			FROM funding_rates
			WHERE record_kind='settled'
			  AND funding_time >= now()-interval '1 year'
			GROUP BY instrument_id
		)
		UPDATE funding_rates current
		SET cumulative_24h=aggregate.cumulative_24h,
		    cumulative_7d=aggregate.cumulative_7d,
		    annualized_rate=CASE
		        WHEN aggregate.coverage_end > aggregate.coverage_start
		        THEN aggregate.total_rate * 365.0 /
		             (EXTRACT(EPOCH FROM (
		                 aggregate.coverage_end-aggregate.coverage_start
		             )) / 86400.0)
		        ELSE NULL
		    END,
		    updated_at=now()
		FROM aggregate
		WHERE current.instrument_id=aggregate.instrument_id
		  AND current.record_kind='current'`); err != nil {
		return fmt.Errorf("refresh funding aggregates: %w", err)
	}
	return nil
}

func (r *Repository) DeleteExpiredHistory(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	command, err := r.pool.Exec(ctx, `
		DELETE FROM funding_rates
		WHERE id IN (
			SELECT id FROM funding_rates
			WHERE record_kind='settled' AND funding_time < $1
			ORDER BY funding_time
			LIMIT $2
		)`, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired funding history: %w", err)
	}
	return command.RowsAffected(), nil
}

func (r *Repository) List(ctx context.Context, query Query) ([]Rate, int, error) {
	if query.Limit <= 0 || query.Limit > 10000 {
		query.Limit = 10000
	}
	if query.Offset < 0 {
		query.Offset = 0
	}
	rows, err := r.pool.Query(ctx, `
		SELECT i.id,i.exchange,i.exchange_symbol,i.global_symbol,i.base_asset,i.quote_asset,
		       f.funding_rate::float8,f.next_funding_at,f.interval_hours::float8,
		       COALESCE(f.annualized_rate::float8,0),f.next_funding_rate::float8,
		       COALESCE(f.cumulative_24h::float8,0),
		       COALESCE(f.cumulative_7d::float8,0),
		       COALESCE(f.mark_price::float8,0),COALESCE(f.index_price::float8,0),
		       COALESCE(f.last_price::float8,0),
		       COALESCE(f.open_interest_base::float8,f.open_interest_contracts::float8,0),
		       COALESCE(f.open_interest_notional_usd::float8,0),
		       COALESCE(f.volume_24h_base::float8,0),
		       COALESCE(f.turnover_24h_usd::float8,0),
		       COALESCE(f.price_change_24h::float8,0),
		       COALESCE(f.source_updated_at,f.received_at),
		       COUNT(*) OVER()
		FROM funding_rates f JOIN instruments i ON i.id=f.instrument_id
		WHERE f.record_kind='current' AND i.active=TRUE AND i.status='active'
		  AND i.contract_type='perpetual'
		  AND ($1='' OR i.exchange=$1)
		  AND ($2='' OR i.global_symbol=$2 OR i.exchange_symbol=$2)
		ORDER BY f.annualized_rate DESC NULLS LAST,i.exchange,i.exchange_symbol
		LIMIT $3 OFFSET $4`, query.Exchange, query.Symbol, query.Limit, query.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := make([]Rate, 0, query.Limit)
	total := 0
	for rows.Next() {
		var item Rate
		if err := rows.Scan(
			&item.InstrumentID, &item.Exchange, &item.ExchangeSymbol,
			&item.GlobalSymbol, &item.BaseAsset, &item.QuoteAsset,
			&item.Rate, &item.FundingTime, &item.IntervalHours,
			&item.AnnualizedRate, &item.NextRate,
			&item.Cumulative24h, &item.Cumulative7d,
			&item.MarkPrice, &item.IndexPrice, &item.LastPrice,
			&item.PositionQuantity, &item.PositionNotionalUSD,
			&item.Volume24hBase, &item.Turnover24hUSD,
			&item.PriceChange24h, &item.SourceUpdatedAt, &total,
		); err != nil {
			return nil, 0, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return result, total, nil
}

func (r *Repository) ListHistory(
	ctx context.Context,
	exchangeName string,
	exchangeSymbol string,
	limit int,
) ([]HistoryPoint, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `
		SELECT f.funding_rate::float8,f.funding_time
		FROM funding_rates f
		JOIN instruments i ON i.id=f.instrument_id
		WHERE f.record_kind='settled'
		  AND i.exchange=$1
		  AND i.exchange_symbol=$2
		  AND i.contract_type='perpetual'
		  AND i.active=TRUE
		  AND i.status='active'
		ORDER BY f.funding_time DESC
		LIMIT $3`, exchangeName, exchangeSymbol, limit)
	if err != nil {
		return nil, fmt.Errorf("list funding history: %w", err)
	}
	defer rows.Close()
	result := make([]HistoryPoint, 0, limit)
	for rows.Next() {
		var point HistoryPoint
		if err := rows.Scan(&point.Rate, &point.SettledAt); err != nil {
			return nil, err
		}
		result = append(result, point)
	}
	return result, rows.Err()
}
