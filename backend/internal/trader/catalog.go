package trader

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"golang.org/x/sync/singleflight"
	"selfquant/backend/internal/trader/exchange"
)

type instrumentCatalog interface {
	List(context.Context, string, string) ([]Instrument, error)
	Get(context.Context, int64) (Instrument, error)
}

type postgresInstrumentCatalog struct {
	pool  *pgxpool.Pool
	ttl   time.Duration
	mu    sync.RWMutex
	items map[int64]Instrument
	until time.Time
	load  singleflight.Group
}

func NewInstrumentCatalog(pool *pgxpool.Pool, ttl time.Duration) instrumentCatalog {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &postgresInstrumentCatalog{pool: pool, ttl: ttl, items: make(map[int64]Instrument)}
}

func (c *postgresInstrumentCatalog) List(
	ctx context.Context,
	exchange string,
	contractType string,
) ([]Instrument, error) {
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}
	exchange = strings.ToLower(strings.TrimSpace(exchange))
	contractType = strings.ToLower(strings.TrimSpace(contractType))
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]Instrument, 0)
	for _, item := range c.items {
		if item.Exchange == exchange && item.ContractType == contractType {
			result = append(result, item)
		}
	}
	return result, nil
}

func (c *postgresInstrumentCatalog) Get(ctx context.Context, id int64) (Instrument, error) {
	if err := c.ensure(ctx); err != nil {
		return Instrument{}, err
	}
	c.mu.RLock()
	item, ok := c.items[id]
	c.mu.RUnlock()
	if !ok {
		return Instrument{}, ErrInstrumentUnavailable
	}
	return item, nil
}

func (c *postgresInstrumentCatalog) ensure(ctx context.Context) error {
	c.mu.RLock()
	fresh := time.Now().Before(c.until)
	c.mu.RUnlock()
	if fresh {
		return nil
	}
	_, err, _ := c.load.Do("refresh", func() (any, error) {
		return nil, c.refresh(ctx)
	})
	return err
}

func (c *postgresInstrumentCatalog) refresh(ctx context.Context) error {
	if c.pool == nil {
		return fmt.Errorf("instrument catalog database is unavailable")
	}
	rows, err := c.pool.Query(ctx, `
		SELECT id, exchange, contract_type, exchange_symbol, base_asset, quote_asset,
		       settle_asset, COALESCE(trim_scale(contract_size)::text, ''),
		       COALESCE(trim_scale(price_tick)::text, ''),
		       COALESCE(trim_scale(quantity_step)::text, ''),
		       COALESCE(trim_scale(min_quantity)::text, ''),
		       COALESCE(trim_scale(min_notional)::text, ''),
		       min_quantity_status, min_notional_status,
		       COALESCE(trim_scale(max_quantity)::text, ''), max_quantity_status,
		       COALESCE(trim_scale(market_quantity_step)::text, ''),
		       market_quantity_step_status,
		       COALESCE(trim_scale(market_min_quantity)::text, ''),
		       market_min_quantity_status,
		       COALESCE(trim_scale(market_max_quantity)::text, ''),
		       market_max_quantity_status,
		       COALESCE(trim_scale(market_min_notional)::text, ''),
		       market_min_notional_status,
		       metadata
		FROM instruments
		WHERE active=TRUE AND status='active'
		  AND (
		    (contract_type IN ('spot', 'perpetual')
		      AND exchange IN ('binance', 'okx', 'bybit', 'bitget', 'gate'))
		    OR
		    (contract_type='perpetual'
		      AND exchange IN ('hyperliquid', 'lighter', 'aster'))
		  )`)
	if err != nil {
		return fmt.Errorf("load trader instruments: %w", err)
	}
	defer rows.Close()
	items := make(map[int64]Instrument)
	for rows.Next() {
		var item Instrument
		var metadata []byte
		if err := rows.Scan(
			&item.ID, &item.Exchange, &item.ContractType, &item.ExchangeSymbol,
			&item.BaseAsset, &item.QuoteAsset, &item.SettleAsset, &item.ContractSize,
			&item.PriceTick, &item.QuantityStep, &item.MinQuantity, &item.MinNotional,
			&item.MinQuantityStatus, &item.MinNotionalStatus,
			&item.MaxQuantity, &item.MaxQuantityStatus,
			&item.MarketQuantityStep, &item.MarketQuantityStepStatus,
			&item.MarketMinQuantity, &item.MarketMinQuantityStatus,
			&item.MarketMaxQuantity, &item.MarketMaxQuantityStatus,
			&item.MarketMinNotional, &item.MarketMinNotionalStatus,
			&metadata,
		); err != nil {
			return fmt.Errorf("scan trader instrument: %w", err)
		}
		item.Exchange = strings.ToLower(item.Exchange)
		item.ContractType = strings.ToLower(item.ContractType)
		item.Metadata = make(map[string]any)
		if len(metadata) > 0 {
			_ = json.Unmarshal(metadata, &item.Metadata)
		}
		if item.ContractType == "perpetual" {
			size, err := decimal.NewFromString(strings.TrimSpace(item.ContractSize))
			if err != nil || !size.IsPositive() {
				continue
			}
			if !linearSettlement(item) {
				continue
			}
			baseStep, err := exchange.BaseQuantityStep(toVenueInstrument(item), item.QuantityStep)
			if err != nil {
				continue
			}
			item.QuantityStep = baseStep
			if item.MinQuantityStatus == exchange.ConstraintKnown {
				baseMinimum, err := exchange.BaseQuantityStep(
					toVenueInstrument(item), item.MinQuantity,
				)
				if err != nil {
					item.MinQuantity = ""
					item.MinQuantityStatus = exchange.ConstraintUnknown
				} else {
					item.MinQuantity = baseMinimum
				}
			}
			convertBaseRule := func(value *string, status *string) {
				if *status != exchange.ConstraintKnown {
					return
				}
				baseValue, convertErr := exchange.BaseQuantityStep(
					toVenueInstrument(item), *value,
				)
				if convertErr != nil {
					*value = ""
					*status = exchange.ConstraintUnknown
					return
				}
				*value = baseValue
			}
			convertBaseRule(&item.MaxQuantity, &item.MaxQuantityStatus)
			convertBaseRule(&item.MarketQuantityStep, &item.MarketQuantityStepStatus)
			convertBaseRule(&item.MarketMinQuantity, &item.MarketMinQuantityStatus)
			convertBaseRule(&item.MarketMaxQuantity, &item.MarketMaxQuantityStatus)
		}
		items[item.ID] = item
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate trader instruments: %w", err)
	}
	c.mu.Lock()
	c.items, c.until = items, time.Now().Add(c.ttl)
	c.mu.Unlock()
	return nil
}

func linearSettlement(item Instrument) bool {
	settle := strings.ToUpper(strings.TrimSpace(item.SettleAsset))
	if settle == "" {
		settle = strings.ToUpper(strings.TrimSpace(item.QuoteAsset))
	}
	if settle != "USDT" && settle != "USDC" {
		return false
	}
	for key, value := range item.Metadata {
		if !strings.EqualFold(key, "contractModel") {
			continue
		}
		model, _ := value.(string)
		return !strings.EqualFold(strings.TrimSpace(model), "inverse")
	}
	return true
}
