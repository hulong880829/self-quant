package account

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
)

type instrumentSpec struct {
	Exchange, ExchangeSymbol string
	BaseAsset, QuoteAsset    string
	SettleAsset              string
	ContractSize             decimal.Decimal
	Metadata                 map[string]any
}

type instrumentCatalog interface {
	Lookup(context.Context, string, string) (instrumentSpec, bool, error)
}

type postgresInstrumentCatalog struct {
	pool  *pgxpool.Pool
	ttl   time.Duration
	mu    sync.RWMutex
	items map[string]instrumentSpec
	until time.Time
	load  singleflight.Group
}

func NewInstrumentCatalog(pool *pgxpool.Pool, ttl time.Duration) instrumentCatalog {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &postgresInstrumentCatalog{pool: pool, ttl: ttl, items: make(map[string]instrumentSpec)}
}

func instrumentKey(exchange, symbol string) string {
	return strings.ToLower(strings.TrimSpace(exchange)) + "\x00" +
		strings.ToUpper(strings.TrimSpace(symbol))
}

func (c *postgresInstrumentCatalog) Lookup(
	ctx context.Context,
	exchange string,
	symbol string,
) (instrumentSpec, bool, error) {
	c.mu.RLock()
	fresh := time.Now().Before(c.until)
	item, ok := c.items[instrumentKey(exchange, symbol)]
	c.mu.RUnlock()
	if fresh {
		return item, ok, nil
	}
	_, err, _ := c.load.Do("refresh", func() (any, error) {
		return nil, c.refresh(ctx)
	})
	if err != nil {
		return instrumentSpec{}, false, err
	}
	c.mu.RLock()
	item, ok = c.items[instrumentKey(exchange, symbol)]
	c.mu.RUnlock()
	return item, ok, nil
}

func (c *postgresInstrumentCatalog) refresh(ctx context.Context) error {
	if c.pool == nil {
		return fmt.Errorf("instrument catalog database is unavailable")
	}
	rows, err := c.pool.Query(ctx, `
		SELECT exchange, exchange_symbol, base_asset, quote_asset, settle_asset,
		       COALESCE(contract_size::text, ''), metadata
		FROM instruments
		WHERE active=TRUE AND status='active' AND contract_type='perpetual'`)
	if err != nil {
		return fmt.Errorf("load instrument catalog: %w", err)
	}
	defer rows.Close()
	items := make(map[string]instrumentSpec)
	for rows.Next() {
		var item instrumentSpec
		var contractSize string
		var metadata []byte
		if err := rows.Scan(
			&item.Exchange, &item.ExchangeSymbol, &item.BaseAsset, &item.QuoteAsset,
			&item.SettleAsset, &contractSize, &metadata,
		); err != nil {
			return fmt.Errorf("scan instrument catalog: %w", err)
		}
		item.ContractSize, err = decimal.NewFromString(contractSize)
		if err != nil || !item.ContractSize.IsPositive() {
			continue
		}
		item.Metadata = make(map[string]any)
		if len(metadata) > 0 {
			_ = json.Unmarshal(metadata, &item.Metadata)
		}
		items[instrumentKey(item.Exchange, item.ExchangeSymbol)] = item
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate instrument catalog: %w", err)
	}
	c.mu.Lock()
	c.items, c.until = items, time.Now().Add(c.ttl)
	c.mu.Unlock()
	return nil
}

func instrumentMetadataString(spec instrumentSpec, name string) string {
	for key, value := range spec.Metadata {
		if !strings.EqualFold(key, name) {
			continue
		}
		switch typed := value.(type) {
		case string:
			return typed
		case json.Number:
			return typed.String()
		case float64:
			return decimal.NewFromFloat(typed).String()
		}
	}
	return ""
}
