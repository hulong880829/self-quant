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
			VALUES ($1,$2,$3,$4,$5,TRUE,$6,$7,$8,NULLIF($9,0),NULLIF($10,0),
			        NULLIF($11,0),$12,$13,$14)
			ON CONFLICT (exchange, exchange_symbol) DO UPDATE SET
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
			int(item.IntervalHours*3600), metadata, item.SourceUpdatedAt)
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

func (r *Repository) MarkMissingInstrumentsInactive(
	ctx context.Context,
	exchangeName string,
	instruments []exchange.Instrument,
) error {
	symbols := make([]string, 0, len(instruments))
	for _, instrument := range instruments {
		symbols = append(symbols, instrument.ExchangeSymbol)
	}
	if _, err := r.pool.Exec(ctx, `
		UPDATE instruments
		SET active=FALSE,status='inactive',updated_at=now()
		WHERE exchange=$1 AND NOT (exchange_symbol=ANY($2))`,
		exchangeName, symbols); err != nil {
		return fmt.Errorf("mark missing instruments inactive: %w", err)
	}
	return nil
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
				FROM instruments WHERE exchange=$1 AND exchange_symbol=$2
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
				SELECT id,$3,$4,'current',$5,$6,$3*(24.0/$5)*365.0,
				       $4,NULLIF($7,0),NULLIF($8,0),NULLIF($9,0),
				       NULLIF($10,0),NULLIF($11,0),NULLIF($12,0),
				       NULLIF($13,0),NULLIF($14,0),$15,$16,now()
				FROM instruments WHERE exchange=$1 AND exchange_symbol=$2
				ON CONFLICT (instrument_id) WHERE record_kind='current' DO UPDATE SET
					funding_rate=EXCLUDED.funding_rate, funding_time=EXCLUDED.funding_time,
					interval_hours=EXCLUDED.interval_hours,
					next_funding_rate=EXCLUDED.next_funding_rate,
					annualized_rate=EXCLUDED.annualized_rate,
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
		       f.annualized_rate::float8,f.next_funding_rate::float8,
		       COALESCE((SELECT SUM(h.funding_rate)::float8 FROM funding_rates h
		                 WHERE h.instrument_id=i.id AND h.record_kind='settled'
		                   AND h.funding_time >= now()-interval '24 hours'),0),
		       COALESCE((SELECT SUM(h.funding_rate)::float8 FROM funding_rates h
		                 WHERE h.instrument_id=i.id AND h.record_kind='settled'
		                   AND h.funding_time >= now()-interval '7 days'),0),
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
		WHERE f.record_kind='current' AND i.active=TRUE
		  AND ($1='' OR i.exchange=$1)
		  AND ($2='' OR i.global_symbol=$2 OR i.exchange_symbol=$2)
		ORDER BY f.annualized_rate DESC NULLS LAST,i.exchange,i.exchange_symbol
		LIMIT $3 OFFSET $4`, query.Exchange, query.Symbol, query.Limit, query.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := make([]Rate, 0, query.Limit)
	instrumentIDs := make([]int64, 0, query.Limit)
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
		instrumentIDs = append(instrumentIDs, item.InstrumentID)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(instrumentIDs) == 0 {
		return result, total, nil
	}
	historyRows, err := r.pool.Query(ctx, `
		SELECT instrument_id,funding_rate::float8,funding_time
		FROM (
			SELECT instrument_id,funding_rate,funding_time,
			       row_number() OVER (PARTITION BY instrument_id ORDER BY funding_time DESC) AS rank
			FROM funding_rates
			WHERE record_kind='settled' AND instrument_id=ANY($1)
		) ranked
		WHERE rank <= 10
		ORDER BY instrument_id,funding_time DESC`, instrumentIDs)
	if err != nil {
		return nil, 0, err
	}
	defer historyRows.Close()
	byInstrument := make(map[int64]*Rate, len(result))
	for index := range result {
		byInstrument[result[index].InstrumentID] = &result[index]
	}
	for historyRows.Next() {
		var instrumentID int64
		var point HistoryPoint
		if err := historyRows.Scan(&instrumentID, &point.Rate, &point.SettledAt); err != nil {
			return nil, 0, err
		}
		if item := byInstrument[instrumentID]; item != nil {
			item.History = append(item.History, point)
		}
	}
	if err := historyRows.Err(); err != nil {
		return nil, 0, err
	}
	return result, total, nil
}
