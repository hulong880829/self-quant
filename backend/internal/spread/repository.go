package spread

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"selfquant/backend/internal/config"
)

type HistoryStore interface {
	QueryHistory(context.Context, HistoryRequest) (History, error)
	Ping(context.Context) error
	Close() error
}

type Repository struct {
	conn     driver.Conn
	database string
	table    string
	timeout  time.Duration
}

func Open(ctx context.Context, cfg config.Spread) (*Repository, error) {
	options := &clickhouse.Options{
		Addr: []string{cfg.ClickHouseAddr},
		Auth: clickhouse.Auth{
			Database: cfg.ClickHouseDatabase,
			Username: cfg.ClickHouseUser,
			Password: cfg.ClickHousePassword,
		},
		Protocol:     clickhouse.Native,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  cfg.QueryTimeout,
		MaxOpenConns: cfg.PoolSize,
		MaxIdleConns: cfg.PoolSize,
		Settings: clickhouse.Settings{
			"readonly": 1,
		},
	}
	if cfg.ClickHouseTLS {
		options.TLS = &tls.Config{InsecureSkipVerify: cfg.ClickHouseTLSSkip}
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
	return &Repository{
		conn: conn, database: cfg.ClickHouseDatabase, table: cfg.ClickHouseTable,
		timeout: cfg.QueryTimeout,
	}, nil
}

func (r *Repository) Ping(ctx context.Context) error {
	return r.conn.Ping(ctx)
}

func (r *Repository) Close() error {
	if r == nil || r.conn == nil {
		return nil
	}
	return r.conn.Close()
}

func (r *Repository) QueryHistory(ctx context.Context, request HistoryRequest) (History, error) {
	normalized, err := NormalizeRequest(request)
	if err != nil {
		return History{}, err
	}
	queryCtx := ctx
	if r.timeout > 0 {
		var cancel context.CancelFunc
		queryCtx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}
	symbol := CanonicalSymbol(normalized.BaseAsset, normalized.QuoteAsset)
	from := normalized.Now.Add(-normalized.Range.Window())
	query := historyQuery(
		r.database, r.table, int(normalized.Range.Resolution().Seconds()),
		normalized.CompareVenue,
	)
	args := []any{
		clickhouse.Named("venue", normalized.Venue),
		clickhouse.Named("symbol", symbol),
		clickhouse.Named("from", from),
		clickhouse.Named("to", normalized.Now),
	}
	if normalized.CompareVenue != "" {
		args = append(args, clickhouse.Named("compare_venue", normalized.CompareVenue))
	}
	rows, err := r.conn.Query(queryCtx, query, args...)
	if err != nil {
		if queryCtx.Err() != nil {
			return History{}, fmt.Errorf("%w: %w", ErrTimeout, err)
		}
		return History{}, fmt.Errorf("%w: %w", ErrQueryFailed, err)
	}
	defer rows.Close()
	parsed := make([]bucketRow, 0, normalized.Range.ExpectedBuckets())
	for rows.Next() {
		var row bucketRow
		if err := rows.Scan(
			&row.Bucket, &row.SpotAskRaw, &row.SpotScale,
			&row.PerpAskRaw, &row.PerpScale, &row.Samples,
		); err != nil {
			return History{}, fmt.Errorf("%w: %w", ErrQueryFailed, err)
		}
		parsed = append(parsed, row)
	}
	if err := rows.Err(); err != nil {
		if queryCtx.Err() != nil {
			return History{}, fmt.Errorf("%w: %w", ErrTimeout, err)
		}
		return History{}, fmt.Errorf("%w: %w", ErrQueryFailed, err)
	}
	return buildHistory(normalized, parsed), nil
}

func historyQuery(database, table string, resolutionSeconds int, compareVenue string) string {
	if compareVenue != "" {
		return fmt.Sprintf(`
SELECT
	toStartOfInterval(ts, INTERVAL %d SECOND) AS bucket,
	argMaxIf(ask_price, ts, venue = @compare_venue) AS spot_ask_raw,
	argMaxIf(price_scale, ts, venue = @compare_venue) AS spot_scale,
	argMaxIf(ask_price, ts, venue = @venue) AS perp_ask_raw,
	argMaxIf(price_scale, ts, venue = @venue) AS perp_scale,
	toUInt32(countIf(venue IN (@venue, @compare_venue))) AS samples
FROM %s.%s
PREWHERE venue IN (@venue, @compare_venue)
	AND canonical_symbol = @symbol
	AND product = 'perpetual'
	AND ts >= @from AND ts < @to
GROUP BY bucket
HAVING bucket >= @from AND bucket < @to
ORDER BY bucket
`, resolutionSeconds, database, table)
	}
	return fmt.Sprintf(`
SELECT
	toStartOfInterval(ts, INTERVAL %d SECOND) AS bucket,
	argMaxIf(ask_price, ts, product = 'spot') AS spot_ask_raw,
	argMaxIf(price_scale, ts, product = 'spot') AS spot_scale,
	argMaxIf(ask_price, ts, product = 'perpetual') AS perp_ask_raw,
	argMaxIf(price_scale, ts, product = 'perpetual') AS perp_scale,
	toUInt32(countIf(product IN ('spot', 'perpetual'))) AS samples
FROM %s.%s
PREWHERE venue = @venue
	AND canonical_symbol = @symbol
	AND product IN ('spot', 'perpetual')
	AND ts >= @from AND ts < @to
GROUP BY bucket
HAVING bucket >= @from AND bucket < @to
ORDER BY bucket
`, resolutionSeconds, database, table)
}
