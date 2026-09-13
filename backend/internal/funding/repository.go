package funding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"selfquant/backend/internal/exchange"
)

var (
	ErrSettledRateRequired = errors.New("settled funding rate required")
	ErrSettledRateMismatch = errors.New("settled funding rate does not match instrument")
)

type Repository struct{ pool *pgxpool.Pool }

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

type instrumentWriteRow struct {
	Exchange                 string          `json:"exchange"`
	ExchangeSymbol           string          `json:"exchange_symbol"`
	BaseAsset                string          `json:"base_asset"`
	QuoteAsset               string          `json:"quote_asset"`
	GlobalSymbol             string          `json:"global_symbol"`
	SettleAsset              string          `json:"settle_asset"`
	ContractType             string          `json:"contract_type"`
	Status                   string          `json:"status"`
	ContractSize             float64         `json:"contract_size"`
	PriceTick                float64         `json:"price_tick"`
	QuantityStep             float64         `json:"quantity_step"`
	FundingIntervalSeconds   int32           `json:"funding_interval_seconds"`
	Metadata                 json.RawMessage `json:"metadata"`
	SourceUpdatedAt          time.Time       `json:"source_updated_at"`
	MinQuantity              float64         `json:"min_quantity"`
	MinNotional              float64         `json:"min_notional"`
	MinQuantityStatus        string          `json:"min_quantity_status"`
	MinNotionalStatus        string          `json:"min_notional_status"`
	MaxQuantity              string          `json:"max_quantity"`
	MaxQuantityStatus        string          `json:"max_quantity_status"`
	MarketQuantityStep       string          `json:"market_quantity_step"`
	MarketQuantityStepStatus string          `json:"market_quantity_step_status"`
	MarketMinQuantity        string          `json:"market_min_quantity"`
	MarketMinQuantityStatus  string          `json:"market_min_quantity_status"`
	MarketMaxQuantity        string          `json:"market_max_quantity"`
	MarketMaxQuantityStatus  string          `json:"market_max_quantity_status"`
	MarketMinNotional        string          `json:"market_min_notional"`
	MarketMinNotionalStatus  string          `json:"market_min_notional_status"`
}

type currentRateWriteRow struct {
	Exchange                string    `json:"exchange"`
	ExchangeSymbol          string    `json:"exchange_symbol"`
	Rate                    float64   `json:"funding_rate"`
	FundingTime             time.Time `json:"funding_time"`
	IntervalHours           float64   `json:"interval_hours"`
	NextRate                *float64  `json:"next_funding_rate"`
	MarkPrice               float64   `json:"mark_price"`
	IndexPrice              float64   `json:"index_price"`
	LastPrice               float64   `json:"last_price"`
	OpenInterestContracts   float64   `json:"open_interest_contracts"`
	OpenInterestBase        float64   `json:"open_interest_base"`
	OpenInterestNotionalUSD float64   `json:"open_interest_notional_usd"`
	Volume24hBase           float64   `json:"volume_24h_base"`
	Turnover24hUSD          float64   `json:"turnover_24h_usd"`
	PriceChange24h          float64   `json:"price_change_24h"`
	SourceUpdatedAt         time.Time `json:"source_updated_at"`
}

type intervalWriteRow struct {
	Exchange       string `json:"exchange"`
	ExchangeSymbol string `json:"exchange_symbol"`
	Seconds        int32  `json:"funding_interval_seconds"`
}

type settledRateWriteRow struct {
	FundingRate     float64   `json:"funding_rate"`
	FundingTime     time.Time `json:"funding_time"`
	IntervalHours   float64   `json:"interval_hours"`
	SourceUpdatedAt time.Time `json:"source_updated_at"`
}

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
	ContractMultiplier  float64
	History24hComplete  *bool
	History7dComplete   *bool
	VenueContractType   string
}

type HistoryPoint struct {
	Rate      float64
	SettledAt time.Time
}

type HistoryKey struct {
	Exchange       string
	ExchangeSymbol string
}

type Query struct {
	Exchange string
	Symbol   string
	Limit    int
	Offset   int
}

func (r *Repository) UpsertInstruments(ctx context.Context, instruments []exchange.Instrument) ([]FundingInstrument, error) {
	rowsByKey := make(map[string]instrumentWriteRow, len(instruments))
	itemsByKey := make(map[string]exchange.Instrument, len(instruments))
	keys := make([]string, 0, len(instruments))
	for _, item := range instruments {
		metadata := item.Metadata
		if len(metadata) == 0 {
			metadata = []byte(`{}`)
		}
		key := item.Exchange + "\x00" + item.ContractType + "\x00" + item.ExchangeSymbol
		if _, exists := rowsByKey[key]; !exists {
			keys = append(keys, key)
		}
		rowsByKey[key] = instrumentWriteRow{
			Exchange: item.Exchange, ExchangeSymbol: item.ExchangeSymbol,
			BaseAsset: item.BaseAsset, QuoteAsset: item.QuoteAsset,
			GlobalSymbol: item.GlobalSymbol, SettleAsset: item.SettleAsset,
			ContractType: item.ContractType, Status: item.Status,
			ContractSize: item.ContractSize, PriceTick: item.PriceTick,
			QuantityStep:           item.QuantityStep,
			FundingIntervalSeconds: int32(item.IntervalHours * 3600),
			Metadata:               metadata, SourceUpdatedAt: item.SourceUpdatedAt,
			MinQuantity: item.MinQuantity, MinNotional: item.MinNotional,
			MinQuantityStatus:        constraintStatus(item.MinQuantityStatus),
			MinNotionalStatus:        constraintStatus(item.MinNotionalStatus),
			MaxQuantity:              item.MaxQuantity,
			MaxQuantityStatus:        constraintStatus(item.MaxQuantityStatus),
			MarketQuantityStep:       item.MarketQuantityStep,
			MarketQuantityStepStatus: constraintStatus(item.MarketQuantityStepStatus),
			MarketMinQuantity:        item.MarketMinQuantity,
			MarketMinQuantityStatus:  constraintStatus(item.MarketMinQuantityStatus),
			MarketMaxQuantity:        item.MarketMaxQuantity,
			MarketMaxQuantityStatus:  constraintStatus(item.MarketMaxQuantityStatus),
			MarketMinNotional:        item.MarketMinNotional,
			MarketMinNotionalStatus:  constraintStatus(item.MarketMinNotionalStatus),
		}
		itemsByKey[key] = item
	}
	if len(keys) == 0 {
		return nil, nil
	}
	input := make([]instrumentWriteRow, 0, len(keys))
	for _, key := range keys {
		input = append(input, rowsByKey[key])
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode instruments: %w", err)
	}
	rows, err := r.pool.Query(ctx, `
		WITH input AS (
			SELECT *
			FROM jsonb_to_recordset($1::jsonb) AS item(
				exchange text,exchange_symbol text,base_asset text,quote_asset text,
				global_symbol text,settle_asset text,contract_type text,status text,
				contract_size double precision,price_tick double precision,
				quantity_step double precision,funding_interval_seconds integer,
				metadata jsonb,source_updated_at timestamptz,
				min_quantity double precision,min_notional double precision,
				min_quantity_status text,min_notional_status text,
				max_quantity text,max_quantity_status text,
				market_quantity_step text,market_quantity_step_status text,
				market_min_quantity text,market_min_quantity_status text,
				market_max_quantity text,market_max_quantity_status text,
				market_min_notional text,market_min_notional_status text
			)
		), upserted AS (
			INSERT INTO instruments (
				exchange,exchange_symbol,base_asset,quote_asset,global_symbol,
				active,settle_asset,contract_type,status,contract_size,
				price_tick,quantity_step,funding_interval_seconds,metadata,
				source_updated_at,min_quantity,min_notional,
				min_quantity_status,min_notional_status,
				max_quantity,max_quantity_status,
				market_quantity_step,market_quantity_step_status,
				market_min_quantity,market_min_quantity_status,
				market_max_quantity,market_max_quantity_status,
				market_min_notional,market_min_notional_status
			)
			SELECT exchange,exchange_symbol,base_asset,quote_asset,global_symbol,
			       TRUE,settle_asset,contract_type,status,NULLIF(contract_size,0),
			       NULLIF(price_tick,0),NULLIF(quantity_step,0),
			       funding_interval_seconds,metadata,source_updated_at,
			       NULLIF(min_quantity,0),NULLIF(min_notional,0),
			       min_quantity_status,min_notional_status,
			       NULLIF(max_quantity,'')::numeric,max_quantity_status,
			       NULLIF(market_quantity_step,'')::numeric,market_quantity_step_status,
			       NULLIF(market_min_quantity,'')::numeric,market_min_quantity_status,
			       NULLIF(market_max_quantity,'')::numeric,market_max_quantity_status,
			       NULLIF(market_min_notional,'')::numeric,market_min_notional_status
			FROM input
			ON CONFLICT (exchange,contract_type,exchange_symbol) DO UPDATE SET
				base_asset=EXCLUDED.base_asset,quote_asset=EXCLUDED.quote_asset,
				global_symbol=EXCLUDED.global_symbol,active=TRUE,
				settle_asset=EXCLUDED.settle_asset,status=EXCLUDED.status,
				contract_size=EXCLUDED.contract_size,price_tick=EXCLUDED.price_tick,
				quantity_step=EXCLUDED.quantity_step,
				funding_interval_seconds=EXCLUDED.funding_interval_seconds,
				min_quantity=EXCLUDED.min_quantity,min_notional=EXCLUDED.min_notional,
				min_quantity_status=EXCLUDED.min_quantity_status,
				min_notional_status=EXCLUDED.min_notional_status,
				max_quantity=EXCLUDED.max_quantity,
				max_quantity_status=EXCLUDED.max_quantity_status,
				market_quantity_step=EXCLUDED.market_quantity_step,
				market_quantity_step_status=EXCLUDED.market_quantity_step_status,
				market_min_quantity=EXCLUDED.market_min_quantity,
				market_min_quantity_status=EXCLUDED.market_min_quantity_status,
				market_max_quantity=EXCLUDED.market_max_quantity,
				market_max_quantity_status=EXCLUDED.market_max_quantity_status,
				market_min_notional=EXCLUDED.market_min_notional,
				market_min_notional_status=EXCLUDED.market_min_notional_status,
				metadata=EXCLUDED.metadata,source_updated_at=EXCLUDED.source_updated_at,
				updated_at=now()
			WHERE ROW(
				instruments.base_asset,instruments.quote_asset,instruments.global_symbol,
				instruments.active,instruments.settle_asset,instruments.status,
				instruments.contract_size,instruments.price_tick,instruments.quantity_step,
				instruments.funding_interval_seconds,instruments.min_quantity,
				instruments.min_notional,instruments.min_quantity_status,
				instruments.min_notional_status,instruments.max_quantity,
				instruments.max_quantity_status,instruments.market_quantity_step,
				instruments.market_quantity_step_status,instruments.market_min_quantity,
				instruments.market_min_quantity_status,instruments.market_max_quantity,
				instruments.market_max_quantity_status,instruments.market_min_notional,
				instruments.market_min_notional_status,instruments.metadata,
				instruments.source_updated_at
			) IS DISTINCT FROM ROW(
				EXCLUDED.base_asset,EXCLUDED.quote_asset,EXCLUDED.global_symbol,
				EXCLUDED.active,EXCLUDED.settle_asset,EXCLUDED.status,
				EXCLUDED.contract_size,EXCLUDED.price_tick,EXCLUDED.quantity_step,
				EXCLUDED.funding_interval_seconds,EXCLUDED.min_quantity,
				EXCLUDED.min_notional,EXCLUDED.min_quantity_status,
				EXCLUDED.min_notional_status,EXCLUDED.max_quantity,
				EXCLUDED.max_quantity_status,EXCLUDED.market_quantity_step,
				EXCLUDED.market_quantity_step_status,EXCLUDED.market_min_quantity,
				EXCLUDED.market_min_quantity_status,EXCLUDED.market_max_quantity,
				EXCLUDED.market_max_quantity_status,EXCLUDED.market_min_notional,
				EXCLUDED.market_min_notional_status,EXCLUDED.metadata,
				EXCLUDED.source_updated_at
			)
			RETURNING id,exchange,contract_type,exchange_symbol,(xmax=0) AS created
		)
		SELECT id,exchange,contract_type,exchange_symbol,created FROM upserted`,
		raw,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert instruments: %w", err)
	}
	defer rows.Close()
	var inserted []FundingInstrument
	for rows.Next() {
		var id int64
		var exchangeName, contractType, symbol string
		var created bool
		if err := rows.Scan(&id, &exchangeName, &contractType, &symbol, &created); err != nil {
			return nil, fmt.Errorf("upsert instruments: %w", err)
		}
		item := itemsByKey[exchangeName+"\x00"+contractType+"\x00"+symbol]
		if created && item.ContractType == exchange.ContractTypePerpetual && item.Status == "active" {
			inserted = append(inserted, FundingInstrument{ID: id, Instrument: item})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("upsert instruments: %w", err)
	}
	return inserted, nil
}

func (r *Repository) HasExchangeInstruments(ctx context.Context, exchangeName string) (bool, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM instruments WHERE exchange=$1)`, exchangeName,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("check exchange instruments: %w", err)
	}
	return exists, nil
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
		       COALESCE(quantity_step::float8,0),
		       COALESCE(min_quantity::float8,0),COALESCE(min_notional::float8,0),
		       min_quantity_status,min_notional_status,
		       COALESCE(trim_scale(max_quantity)::text,''),max_quantity_status,
		       COALESCE(trim_scale(market_quantity_step)::text,''),market_quantity_step_status,
		       COALESCE(trim_scale(market_min_quantity)::text,''),market_min_quantity_status,
		       COALESCE(trim_scale(market_max_quantity)::text,''),market_max_quantity_status,
		       COALESCE(trim_scale(market_min_notional)::text,''),market_min_notional_status,
		       metadata,
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
			&item.QuantityStep, &item.MinQuantity, &item.MinNotional,
			&item.MinQuantityStatus, &item.MinNotionalStatus,
			&item.MaxQuantity, &item.MaxQuantityStatus,
			&item.MarketQuantityStep, &item.MarketQuantityStepStatus,
			&item.MarketMinQuantity, &item.MarketMinQuantityStatus,
			&item.MarketMaxQuantity, &item.MarketMaxQuantityStatus,
			&item.MarketMinNotional, &item.MarketMinNotionalStatus,
			&item.Metadata, &item.SourceUpdatedAt,
		); err != nil {
			return nil, err
		}
		item.IntervalHours = float64(intervalSeconds) / 3600
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *Repository) ListActivePerpetualInstruments(
	ctx context.Context,
	exchanges []string,
) ([]FundingInstrument, error) {
	if len(exchanges) == 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id,exchange,exchange_symbol,base_asset,quote_asset,global_symbol,
		       funding_interval_seconds,settle_asset,contract_type,status,
		       COALESCE(contract_size::float8,0),COALESCE(price_tick::float8,0),
		       COALESCE(quantity_step::float8,0),
		       COALESCE(min_quantity::float8,0),COALESCE(min_notional::float8,0),
		       min_quantity_status,min_notional_status,
		       COALESCE(trim_scale(max_quantity)::text,''),max_quantity_status,
		       COALESCE(trim_scale(market_quantity_step)::text,''),market_quantity_step_status,
		       COALESCE(trim_scale(market_min_quantity)::text,''),market_min_quantity_status,
		       COALESCE(trim_scale(market_max_quantity)::text,''),market_max_quantity_status,
		       COALESCE(trim_scale(market_min_notional)::text,''),market_min_notional_status,
		       metadata,
		       COALESCE(source_updated_at,updated_at)
		FROM instruments
		WHERE active=TRUE AND status='active' AND contract_type='perpetual'
		  AND exchange=ANY($1)
		ORDER BY exchange,exchange_symbol`, exchanges)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []FundingInstrument
	for rows.Next() {
		var item FundingInstrument
		var intervalSeconds int32
		if err := rows.Scan(
			&item.ID, &item.Exchange, &item.ExchangeSymbol, &item.BaseAsset, &item.QuoteAsset,
			&item.GlobalSymbol, &intervalSeconds, &item.SettleAsset,
			&item.ContractType, &item.Status, &item.ContractSize, &item.PriceTick,
			&item.QuantityStep, &item.MinQuantity, &item.MinNotional,
			&item.MinQuantityStatus, &item.MinNotionalStatus,
			&item.MaxQuantity, &item.MaxQuantityStatus,
			&item.MarketQuantityStep, &item.MarketQuantityStepStatus,
			&item.MarketMinQuantity, &item.MarketMinQuantityStatus,
			&item.MarketMaxQuantity, &item.MarketMaxQuantityStatus,
			&item.MarketMinNotional, &item.MarketMinNotionalStatus,
			&item.Metadata, &item.SourceUpdatedAt,
		); err != nil {
			return nil, err
		}
		item.IntervalHours = float64(intervalSeconds) / 3600
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *Repository) SettledBounds(ctx context.Context, ids []int64) (map[int64]settledBound, error) {
	result := make(map[int64]settledBound, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT instrument_id,min(funding_time),max(funding_time)
		FROM funding_rates
		WHERE record_kind='settled' AND instrument_id=ANY($1)
		GROUP BY instrument_id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var minTime, maxTime time.Time
		if err := rows.Scan(&id, &minTime, &maxTime); err != nil {
			return nil, err
		}
		result[id] = settledBound{Min: minTime.UTC(), Max: maxTime.UTC()}
	}
	return result, rows.Err()
}

type settledBound struct {
	Min time.Time
	Max time.Time
}

func constraintStatus(value string) string {
	switch value {
	case exchange.ConstraintKnown, exchange.ConstraintNotApplicable:
		return value
	default:
		return exchange.ConstraintUnknown
	}
}

func (r *Repository) UpsertCurrentRates(ctx context.Context, rates []exchange.FundingRate) error {
	rowsByKey := make(map[string]currentRateWriteRow, len(rates))
	keys := make([]string, 0, len(rates))
	for _, item := range rates {
		if item.Settled {
			continue
		}
		key := instrumentKey(item.Exchange, item.ExchangeSymbol)
		if _, exists := rowsByKey[key]; !exists {
			keys = append(keys, key)
		}
		rowsByKey[key] = currentRateWriteRow{
			Exchange: item.Exchange, ExchangeSymbol: item.ExchangeSymbol,
			Rate: item.Rate, FundingTime: item.FundingTime,
			IntervalHours: item.IntervalHours, NextRate: item.NextRate,
			MarkPrice: item.MarkPrice, IndexPrice: item.IndexPrice,
			LastPrice: item.LastPrice, OpenInterestContracts: item.OpenInterestContracts,
			OpenInterestBase:        item.OpenInterestBase,
			OpenInterestNotionalUSD: item.OpenInterestNotionalUSD,
			Volume24hBase:           item.Volume24hBase, Turnover24hUSD: item.Turnover24hUSD,
			PriceChange24h: item.PriceChange24h, SourceUpdatedAt: item.SourceUpdatedAt,
		}
	}
	if len(keys) == 0 {
		return nil
	}
	input := make([]currentRateWriteRow, 0, len(keys))
	for _, key := range keys {
		input = append(input, rowsByKey[key])
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode current funding rates: %w", err)
	}
	if _, err := r.pool.Exec(ctx, `
		WITH input AS (
			SELECT *
			FROM jsonb_to_recordset($1::jsonb) AS item(
				exchange text,exchange_symbol text,funding_rate double precision,
				funding_time timestamptz,interval_hours double precision,
				next_funding_rate double precision,mark_price double precision,
				index_price double precision,last_price double precision,
				open_interest_contracts double precision,open_interest_base double precision,
				open_interest_notional_usd double precision,volume_24h_base double precision,
				turnover_24h_usd double precision,price_change_24h double precision,
				source_updated_at timestamptz
			)
		)
		INSERT INTO funding_rates (
			instrument_id,funding_rate,funding_time,record_kind,
			interval_hours,next_funding_rate,annualized_rate,next_funding_at,
			mark_price,index_price,last_price,open_interest_contracts,
			open_interest_base,open_interest_notional_usd,volume_24h_base,
			turnover_24h_usd,price_change_24h,source_updated_at,received_at
		)
		SELECT instrument.id,input.funding_rate,input.funding_time,'current',
		       input.interval_hours,input.next_funding_rate,NULL,input.funding_time,
		       NULLIF(input.mark_price,0),NULLIF(input.index_price,0),
		       NULLIF(input.last_price,0),NULLIF(input.open_interest_contracts,0),
		       NULLIF(input.open_interest_base,0),
		       NULLIF(input.open_interest_notional_usd,0),
		       NULLIF(input.volume_24h_base,0),NULLIF(input.turnover_24h_usd,0),
		       input.price_change_24h,input.source_updated_at,now()
		FROM input
		JOIN instruments instrument
		  ON instrument.exchange=input.exchange
		 AND instrument.contract_type='perpetual'
		 AND instrument.exchange_symbol=input.exchange_symbol
		 AND instrument.active=TRUE
		 AND instrument.status='active'
		ON CONFLICT (instrument_id) WHERE record_kind='current' DO UPDATE SET
			funding_rate=EXCLUDED.funding_rate,funding_time=EXCLUDED.funding_time,
			interval_hours=EXCLUDED.interval_hours,
			next_funding_rate=EXCLUDED.next_funding_rate,
			next_funding_at=EXCLUDED.next_funding_at,
			mark_price=EXCLUDED.mark_price,index_price=EXCLUDED.index_price,
			last_price=EXCLUDED.last_price,
			open_interest_contracts=EXCLUDED.open_interest_contracts,
			open_interest_base=EXCLUDED.open_interest_base,
			open_interest_notional_usd=EXCLUDED.open_interest_notional_usd,
			volume_24h_base=EXCLUDED.volume_24h_base,
			turnover_24h_usd=EXCLUDED.turnover_24h_usd,
			price_change_24h=EXCLUDED.price_change_24h,
			source_updated_at=EXCLUDED.source_updated_at,
			received_at=now(),updated_at=now()`,
		raw,
	); err != nil {
		return fmt.Errorf("upsert current funding rates: %w", err)
	}
	return nil
}

func (r *Repository) UpsertSettledRates(
	ctx context.Context,
	instrument FundingInstrument,
	rates []exchange.FundingRate,
) (UpsertStats, error) {
	if instrument.ID <= 0 {
		return UpsertStats{}, fmt.Errorf("settled upsert requires instrument id")
	}
	if err := validateSettledRates(instrument, rates); err != nil {
		return UpsertStats{}, err
	}
	if len(rates) == 0 {
		return UpsertStats{}, nil
	}
	rowsByTime := make(map[int64]settledRateWriteRow, len(rates))
	keys := make([]int64, 0, len(rates))
	for _, item := range rates {
		intervalHours := item.IntervalHours
		if intervalHours <= 0 {
			intervalHours = 8
		}
		key := item.FundingTime.UTC().UnixMicro()
		if _, exists := rowsByTime[key]; !exists {
			keys = append(keys, key)
		}
		rowsByTime[key] = settledRateWriteRow{
			FundingRate: item.Rate, FundingTime: item.FundingTime,
			IntervalHours: intervalHours, SourceUpdatedAt: item.SourceUpdatedAt,
		}
	}
	input := make([]settledRateWriteRow, 0, len(keys))
	for _, key := range keys {
		input = append(input, rowsByTime[key])
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return UpsertStats{}, fmt.Errorf("encode settled funding rates: %w", err)
	}
	tag, err := r.pool.Exec(ctx, `
		WITH input AS (
			SELECT *
			FROM jsonb_to_recordset($2::jsonb) AS item(
				funding_rate double precision,funding_time timestamptz,
				interval_hours double precision,source_updated_at timestamptz
			)
		)
		INSERT INTO funding_rates (
			instrument_id,funding_rate,funding_time,record_kind,
			interval_hours,annualized_rate,source_updated_at,received_at
		)
		SELECT $1,input.funding_rate,input.funding_time,'settled',
		       input.interval_hours,
		       input.funding_rate*(24.0/input.interval_hours)*365.0,
		       input.source_updated_at,now()
		FROM input
		ON CONFLICT (instrument_id,funding_time) WHERE record_kind='settled'
		DO UPDATE SET funding_rate=EXCLUDED.funding_rate,
			interval_hours=EXCLUDED.interval_hours,
			annualized_rate=EXCLUDED.annualized_rate,
			source_updated_at=EXCLUDED.source_updated_at,updated_at=now()
		WHERE funding_rates.funding_rate IS DISTINCT FROM EXCLUDED.funding_rate
		   OR funding_rates.interval_hours IS DISTINCT FROM EXCLUDED.interval_hours`,
		instrument.ID, raw,
	)
	if err != nil {
		return UpsertStats{}, fmt.Errorf("upsert settled funding rates: %w", err)
	}
	changed := int(tag.RowsAffected())
	return UpsertStats{Changed: changed, Unchanged: len(input) - changed}, nil
}

func validateSettledRates(instrument FundingInstrument, rates []exchange.FundingRate) error {
	for _, item := range rates {
		if !item.Settled {
			return fmt.Errorf("%w: %s %s", ErrSettledRateRequired, item.Exchange, item.ExchangeSymbol)
		}
		if item.Exchange != instrument.Exchange || item.ExchangeSymbol != instrument.ExchangeSymbol {
			return fmt.Errorf("%w: got %s %s want %s %s",
				ErrSettledRateMismatch, item.Exchange, item.ExchangeSymbol,
				instrument.Exchange, instrument.ExchangeSymbol)
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
	rowsByKey := make(map[string]intervalWriteRow, len(rates))
	keys := make([]string, 0, len(rates))
	for _, rate := range rates {
		if rate.Settled || rate.IntervalHours <= 0 {
			continue
		}
		key := instrumentKey(rate.Exchange, rate.ExchangeSymbol)
		if _, exists := rowsByKey[key]; !exists {
			keys = append(keys, key)
		}
		rowsByKey[key] = intervalWriteRow{
			Exchange: rate.Exchange, ExchangeSymbol: rate.ExchangeSymbol,
			Seconds: int32(rate.IntervalHours * 3600),
		}
	}
	if len(keys) == 0 {
		return nil
	}
	input := make([]intervalWriteRow, 0, len(keys))
	for _, key := range keys {
		input = append(input, rowsByKey[key])
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode funding intervals: %w", err)
	}
	if _, err := r.pool.Exec(ctx, `
		WITH input AS (
			SELECT *
			FROM jsonb_to_recordset($1::jsonb) AS item(
				exchange text,exchange_symbol text,funding_interval_seconds integer
			)
		)
		UPDATE instruments instrument
		SET funding_interval_seconds=input.funding_interval_seconds,updated_at=now()
		FROM input
		WHERE instrument.exchange=input.exchange
		  AND instrument.contract_type='perpetual'
		  AND instrument.exchange_symbol=input.exchange_symbol
		  AND instrument.active=TRUE
		  AND instrument.status='active'
		  AND instrument.funding_interval_seconds
		      IS DISTINCT FROM input.funding_interval_seconds`,
		raw,
	); err != nil {
		return fmt.Errorf("update funding interval: %w", err)
	}
	return nil
}

func (r *Repository) RefreshAggregates(ctx context.Context) error {
	if _, err := r.pool.Exec(ctx, `
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
		), desired AS (
			SELECT current.id,
			       COALESCE(aggregate.cumulative_24h,0) AS cumulative_24h,
			       COALESCE(aggregate.cumulative_7d,0) AS cumulative_7d,
			       CASE
			           WHEN aggregate.coverage_end > aggregate.coverage_start
			           THEN aggregate.total_rate * 365.0 /
			                (EXTRACT(EPOCH FROM (
			                    aggregate.coverage_end-aggregate.coverage_start
			                )) / 86400.0)
			           ELSE NULL
			       END AS annualized_rate
			FROM funding_rates current
			LEFT JOIN aggregate ON aggregate.instrument_id=current.instrument_id
			WHERE current.record_kind='current'
		)
		UPDATE funding_rates current
		SET cumulative_24h=desired.cumulative_24h,
		    cumulative_7d=desired.cumulative_7d,
		    annualized_rate=desired.annualized_rate,
		    updated_at=now()
		FROM desired
		WHERE current.id=desired.id
		  AND ROW(current.cumulative_24h,current.cumulative_7d,current.annualized_rate)
		      IS DISTINCT FROM
		      ROW(desired.cumulative_24h,desired.cumulative_7d,desired.annualized_rate)`); err != nil {
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
		       COALESCE(i.contract_size::float8,0),
		       COALESCE(i.metadata, '{}'::jsonb),
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
		var metadata json.RawMessage
		if err := rows.Scan(
			&item.InstrumentID, &item.Exchange, &item.ExchangeSymbol,
			&item.GlobalSymbol, &item.BaseAsset, &item.QuoteAsset,
			&item.Rate, &item.FundingTime, &item.IntervalHours,
			&item.AnnualizedRate, &item.NextRate,
			&item.Cumulative24h, &item.Cumulative7d,
			&item.MarkPrice, &item.IndexPrice, &item.LastPrice,
			&item.PositionQuantity, &item.PositionNotionalUSD,
			&item.Volume24hBase, &item.Turnover24hUSD,
			&item.PriceChange24h, &item.SourceUpdatedAt,
			&item.ContractMultiplier, &metadata, &total,
		); err != nil {
			return nil, 0, err
		}
		item.ContractMultiplier = NormalizeContractMultiplier(item.ContractMultiplier)
		item.VenueContractType = DeriveVenueContractType(item.Exchange, item.ExchangeSymbol, metadata)
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return result, total, nil
}

func (r *Repository) ListCurrentByKeys(
	ctx context.Context,
	keys []RateLookupKey,
) ([]*Rate, error) {
	result := make([]*Rate, len(keys))
	if len(keys) == 0 {
		return result, nil
	}
	exchanges := make([]string, len(keys))
	symbols := make([]string, len(keys))
	bases := make([]string, len(keys))
	quotes := make([]string, len(keys))
	for index, key := range keys {
		normalized := NormalizeRateLookupKey(key)
		exchanges[index] = strings.ToLower(normalized.Exchange)
		symbols[index] = normalized.ExchangeSymbol
		bases[index] = normalized.BaseAsset
		quotes[index] = normalized.QuoteAsset
	}
	rows, err := r.pool.Query(ctx, `
		WITH requested AS (
			SELECT ordinality::int AS ord,
			       exchange,
			       exchange_symbol,
			       base_asset,
			       quote_asset
			FROM unnest($1::text[], $2::text[], $3::text[], $4::text[])
			     WITH ORDINALITY AS item(exchange, exchange_symbol, base_asset, quote_asset, ordinality)
		)
		SELECT DISTINCT ON (requested.ord)
		       requested.ord,
		       i.id,i.exchange,i.exchange_symbol,i.global_symbol,i.base_asset,i.quote_asset,
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
		       COALESCE(i.contract_size::float8,0),
		       COALESCE(i.metadata, '{}'::jsonb)
		FROM requested
		JOIN instruments i
		  ON i.active=TRUE AND i.status='active' AND i.contract_type='perpetual'
		 AND lower(i.exchange)=requested.exchange
		 AND (
		       (requested.exchange_symbol <> '' AND lower(i.exchange_symbol)=lower(requested.exchange_symbol))
		    OR (
		         requested.base_asset <> '' AND requested.quote_asset <> ''
		         AND i.base_asset=requested.base_asset
		         AND i.quote_asset=requested.quote_asset
		       )
		 )
		JOIN funding_rates f ON i.id=f.instrument_id AND f.record_kind='current'
		ORDER BY requested.ord,
		         CASE WHEN requested.exchange_symbol <> ''
		                   AND lower(i.exchange_symbol)=lower(requested.exchange_symbol)
		              THEN 0 ELSE 1 END,
		         f.annualized_rate DESC NULLS LAST,
		         i.exchange,
		         i.exchange_symbol`,
		exchanges, symbols, bases, quotes)
	if err != nil {
		return nil, fmt.Errorf("list current funding rates by keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ord int
		var item Rate
		var metadata json.RawMessage
		if err := rows.Scan(
			&ord,
			&item.InstrumentID, &item.Exchange, &item.ExchangeSymbol,
			&item.GlobalSymbol, &item.BaseAsset, &item.QuoteAsset,
			&item.Rate, &item.FundingTime, &item.IntervalHours,
			&item.AnnualizedRate, &item.NextRate,
			&item.Cumulative24h, &item.Cumulative7d,
			&item.MarkPrice, &item.IndexPrice, &item.LastPrice,
			&item.PositionQuantity, &item.PositionNotionalUSD,
			&item.Volume24hBase, &item.Turnover24hUSD,
			&item.PriceChange24h, &item.SourceUpdatedAt,
			&item.ContractMultiplier, &metadata,
		); err != nil {
			return nil, err
		}
		if ord < 1 || ord > len(result) {
			continue
		}
		item.ContractMultiplier = NormalizeContractMultiplier(item.ContractMultiplier)
		item.VenueContractType = DeriveVenueContractType(item.Exchange, item.ExchangeSymbol, metadata)
		copied := item
		result[ord-1] = &copied
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
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

func (r *Repository) ListSettledHistoryRange(
	ctx context.Context,
	from time.Time,
	to time.Time,
	keys []HistoryKey,
) (map[HistoryKey][]HistoryPoint, error) {
	result := make(map[HistoryKey][]HistoryPoint, len(keys))
	if len(keys) == 0 || !from.Before(to) {
		return result, nil
	}
	wanted := make(map[HistoryKey]struct{}, len(keys))
	exchanges := make([]string, 0, len(keys))
	symbols := make([]string, 0, len(keys))
	seenExchanges := make(map[string]struct{})
	seenSymbols := make(map[string]struct{})
	for _, key := range keys {
		if key.Exchange == "" || key.ExchangeSymbol == "" {
			continue
		}
		wanted[key] = struct{}{}
		if _, ok := seenExchanges[key.Exchange]; !ok {
			seenExchanges[key.Exchange] = struct{}{}
			exchanges = append(exchanges, key.Exchange)
		}
		if _, ok := seenSymbols[key.ExchangeSymbol]; !ok {
			seenSymbols[key.ExchangeSymbol] = struct{}{}
			symbols = append(symbols, key.ExchangeSymbol)
		}
	}
	if len(wanted) == 0 {
		return result, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT i.exchange,i.exchange_symbol,
		       f.funding_rate::float8,f.funding_time
		FROM funding_rates f
		JOIN instruments i ON i.id=f.instrument_id
		WHERE f.record_kind='settled'
		  AND f.funding_time > $1 AND f.funding_time <= $2
		  AND i.exchange=ANY($3)
		  AND i.exchange_symbol=ANY($4)
		  AND i.contract_type='perpetual'
		  AND i.active=TRUE
		  AND i.status='active'
		ORDER BY i.exchange,i.exchange_symbol,f.funding_time`,
		from, to, exchanges, symbols)
	if err != nil {
		return nil, fmt.Errorf("list settled funding history range: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key HistoryKey
		var point HistoryPoint
		if err := rows.Scan(
			&key.Exchange, &key.ExchangeSymbol, &point.Rate, &point.SettledAt,
		); err != nil {
			return nil, err
		}
		if _, ok := wanted[key]; !ok {
			continue
		}
		point.SettledAt = point.SettledAt.UTC()
		result[key] = append(result[key], point)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
