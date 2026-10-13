package jev

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/shopspring/decimal"
	"selfquant/backend/internal/config"
)

type ClickHouseQuotes struct {
	conn     driver.Conn
	database string
	table    string
	timeout  time.Duration
}

func OpenQuotes(ctx context.Context, cfg config.Jev) (*ClickHouseQuotes, error) {
	options := &clickhouse.Options{
		Addr: []string{cfg.ClickHouseAddr},
		Auth: clickhouse.Auth{
			Database: cfg.ClickHouseDatabase, Username: cfg.ClickHouseUser, Password: cfg.ClickHousePassword,
		},
		Protocol:     clickhouse.Native,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  cfg.QueryTimeout,
		MaxOpenConns: cfg.PoolSize,
		MaxIdleConns: cfg.PoolSize,
		Settings: clickhouse.Settings{
			"readonly":    1,
			"max_threads": 2,
		},
	}
	if cfg.ClickHouseTLS {
		options.TLS = &tls.Config{InsecureSkipVerify: cfg.ClickHouseTLSSkip} //nolint:gosec
	}
	conn, err := clickhouse.Open(options)
	if err != nil {
		return nil, fmt.Errorf("open clickhouse: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := conn.Ping(pingCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping clickhouse: %w", err)
	}
	return &ClickHouseQuotes{
		conn: conn, database: cfg.ClickHouseDatabase, table: cfg.ClickHouseTable, timeout: cfg.QueryTimeout,
	}, nil
}

func (q *ClickHouseQuotes) Close() error {
	if q == nil || q.conn == nil {
		return nil
	}
	return q.conn.Close()
}

func (q *ClickHouseQuotes) Latest(ctx context.Context, keys []MarketKey) (map[string]Quote, error) {
	if len(keys) == 0 {
		return map[string]Quote{}, nil
	}
	venues := make([]string, 0, len(keys))
	symbols := make([]string, 0, len(keys))
	wanted := map[string]MarketKey{}
	for _, key := range keys {
		normalized := NormalizeMarketKey(key.Venue, key.Symbol, key.Product)
		wanted[normalized.QuoteKey()] = normalized
		venues = append(venues, normalized.Venue)
		symbols = append(symbols, normalized.Symbol)
	}
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	query := latestBBOQuery(q.database, q.table)
	rows, err := q.conn.Query(queryCtx, query,
		clickhouse.Named("venues", venues),
		clickhouse.Named("symbols", symbols),
	)
	if err != nil {
		return nil, fmt.Errorf("query latest bbo: %w", err)
	}
	defer rows.Close()
	out := map[string]Quote{}
	for rows.Next() {
		var venue, symbol, product string
		var bidRaw, askRaw, bidQtyRaw, askQtyRaw int64
		var priceScale, qtyScale uint8
		var ts time.Time
		if err := rows.Scan(&venue, &symbol, &product, &bidRaw, &askRaw, &bidQtyRaw, &askQtyRaw, &priceScale, &qtyScale, &ts); err != nil {
			return nil, fmt.Errorf("scan latest bbo: %w", err)
		}
		key := NormalizeMarketKey(venue, symbol, product)
		if _, ok := wanted[key.QuoteKey()]; !ok {
			continue
		}
		bid, bidOK := decodeScaled(bidRaw, priceScale)
		ask, askOK := decodeScaled(askRaw, priceScale)
		bidQty, _ := decodeScaled(bidQtyRaw, qtyScale)
		askQty, _ := decodeScaled(askQtyRaw, qtyScale)
		out[key.QuoteKey()] = Quote{
			Key: key, Bid: bid, Ask: ask, BidQty: bidQty, AskQty: askQty, TS: ts.UTC(), Available: bidOK && askOK,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (q *ClickHouseQuotes) MidBefore(
	ctx context.Context,
	key MarketKey,
	settledAt time.Time,
	maxAge time.Duration,
) (decimal.Decimal, time.Time, bool, error) {
	normalized := NormalizeMarketKey(key.Venue, key.Symbol, key.Product)
	from := settledAt.Add(-maxAge)
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	query := fmt.Sprintf(`
SELECT
	bid_price,
	ask_price,
	price_scale,
	ts
FROM %s.%s
PREWHERE venue = @venue
	AND canonical_symbol = @symbol
	AND product = @product
	AND ts <= @settled_at
	AND ts >= @from
ORDER BY ts DESC
LIMIT 1`, q.database, q.table)
	row := q.conn.QueryRow(queryCtx, query,
		clickhouse.Named("venue", normalized.Venue),
		clickhouse.Named("symbol", normalized.Symbol),
		clickhouse.Named("product", normalized.Product),
		clickhouse.Named("settled_at", settledAt.UTC()),
		clickhouse.Named("from", from.UTC()),
	)
	var bidRaw, askRaw int64
	var scale uint8
	var ts time.Time
	if err := row.Scan(&bidRaw, &askRaw, &scale, &ts); err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return decimal.Zero, time.Time{}, false, nil
		}
		return decimal.Zero, time.Time{}, false, err
	}
	bid, bidOK := decodeScaled(bidRaw, scale)
	ask, askOK := decodeScaled(askRaw, scale)
	if !bidOK || !askOK {
		return decimal.Zero, time.Time{}, false, nil
	}
	return bid.Add(ask).Div(decimal.NewFromInt(2)), ts.UTC(), true, nil
}

func latestBBOQuery(database, table string) string {
	return fmt.Sprintf(`
SELECT
	venue,
	canonical_symbol,
	product,
	argMax(bid_price, ts) AS bid_raw,
	argMax(ask_price, ts) AS ask_raw,
	argMax(bid_quantity, ts) AS bid_qty_raw,
	argMax(ask_quantity, ts) AS ask_qty_raw,
	argMax(price_scale, ts) AS price_scale,
	argMax(quantity_scale, ts) AS quantity_scale,
	max(ts) AS latest_ts
FROM %s.%s
PREWHERE venue IN @venues
	AND canonical_symbol IN @symbols
	AND product IN ('spot', 'perpetual')
	AND ts >= now() - INTERVAL 1 HOUR
GROUP BY venue, canonical_symbol, product`, database, table)
}

func (q *ClickHouseQuotes) TenMinuteBBOs(
	ctx context.Context,
	keys []MarketKey,
	from, untilExclusive time.Time,
) ([]TenMinuteBBO, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	venues := make([]string, 0, len(keys))
	symbols := make([]string, 0, len(keys))
	products := make([]string, 0, len(keys))
	wanted := map[string]MarketKey{}
	for _, key := range keys {
		normalized := NewCanonicalMarketKey(key.Venue, key.Symbol, key.Product)
		if !normalized.CanonicalOK() {
			continue
		}
		wanted[normalized.QuoteKey()] = normalized
		venues = append(venues, normalized.Venue)
		symbols = append(symbols, normalized.Symbol)
		products = append(products, normalized.Product)
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	queryCtx, cancel := q.queryContext(ctx)
	defer cancel()
	rows, err := q.conn.Query(queryCtx, tenMinuteBBOQuery(q.database),
		clickhouse.Named("venues", venues),
		clickhouse.Named("symbols", symbols),
		clickhouse.Named("products", products),
		clickhouse.Named("from", from.UTC()),
		clickhouse.Named("until", untilExclusive.UTC()),
	)
	if err != nil {
		return nil, fmt.Errorf("query ten-minute bbo: %w", err)
	}
	defer rows.Close()
	out := make([]TenMinuteBBO, 0)
	for rows.Next() {
		var venue, symbol, product string
		var bucket time.Time
		var bidRaw, askRaw int64
		var bidScale, askScale uint8
		if err := rows.Scan(&venue, &symbol, &product, &bucket, &bidRaw, &bidScale, &askRaw, &askScale); err != nil {
			return nil, fmt.Errorf("scan ten-minute bbo: %w", err)
		}
		key := NewCanonicalMarketKey(venue, symbol, product)
		if _, ok := wanted[key.QuoteKey()]; !ok {
			continue
		}
		bid, bidOK := decodeScaled(bidRaw, bidScale)
		ask, askOK := decodeScaled(askRaw, askScale)
		if !bidOK || !askOK {
			continue
		}
		out = append(out, TenMinuteBBO{Key: key, Bucket: bucket.UTC(), Bid: bid, Ask: ask})
	}
	return out, rows.Err()
}

func (q *ClickHouseQuotes) queryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if q.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, q.timeout)
}

func decodeScaled(raw int64, scale uint8) (decimal.Decimal, bool) {
	if raw <= 0 {
		return decimal.Zero, false
	}
	value := decimal.NewFromInt(raw)
	if scale > 0 {
		value = value.Shift(-int32(scale))
	}
	return value, value.IsPositive()
}
