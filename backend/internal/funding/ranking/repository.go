package ranking

import (
	"context"
	"crypto/tls"
	"fmt"
	"sort"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type HistoryStore interface {
	Query(context.Context, time.Time, time.Time, []string, []string) ([]Quote, error)
	Close() error
}

type RepositoryConfig struct {
	Address       string
	Database      string
	Table         string
	User          string
	Password      string
	TLS           bool
	TLSSkipVerify bool
	QueryTimeout  time.Duration
	PoolSize      int
}

type Repository struct {
	conn     driver.Conn
	database string
	table    string
	timeout  time.Duration
}

func Open(ctx context.Context, cfg RepositoryConfig) (*Repository, error) {
	options := &clickhouse.Options{
		Addr: []string{cfg.Address},
		Auth: clickhouse.Auth{
			Database: cfg.Database, Username: cfg.User, Password: cfg.Password,
		},
		Protocol: clickhouse.Native, DialTimeout: 5 * time.Second,
		ReadTimeout: cfg.QueryTimeout, MaxOpenConns: cfg.PoolSize, MaxIdleConns: cfg.PoolSize,
		Settings: clickhouse.Settings{"readonly": 1},
	}
	if cfg.TLS {
		options.TLS = &tls.Config{InsecureSkipVerify: cfg.TLSSkipVerify} //nolint:gosec // explicit operator setting
	}
	conn, err := clickhouse.Open(options)
	if err != nil {
		return nil, fmt.Errorf("open opportunity clickhouse: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := conn.Ping(pingCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping opportunity clickhouse: %w", err)
	}
	return &Repository{conn: conn, database: cfg.Database, table: cfg.Table, timeout: cfg.QueryTimeout}, nil
}

func (r *Repository) Close() error {
	if r == nil || r.conn == nil {
		return nil
	}
	return r.conn.Close()
}

func (r *Repository) Query(
	ctx context.Context,
	from time.Time,
	to time.Time,
	symbols []string,
	venues []string,
) ([]Quote, error) {
	if len(symbols) == 0 || len(venues) == 0 || !from.Before(to) {
		return nil, nil
	}
	symbols = uniqueSorted(symbols)
	venues = uniqueSorted(venues)
	queryCtx := ctx
	if r.timeout > 0 {
		var cancel context.CancelFunc
		queryCtx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}
	rows, err := r.conn.Query(
		queryCtx, historyQuery(r.database, r.table),
		clickhouse.Named("from", from.UTC()), clickhouse.Named("to", to.UTC()),
		clickhouse.Named("symbols", symbols), clickhouse.Named("venues", venues),
	)
	if err != nil {
		return nil, fmt.Errorf("query opportunity bbo: %w", err)
	}
	defer rows.Close()
	result := make([]Quote, 0)
	for rows.Next() {
		var (
			bucket             time.Time
			symbol, venue      string
			bidRaw, askRaw     int64
			bidScale, askScale uint8
		)
		if err := rows.Scan(&bucket, &symbol, &venue, &bidRaw, &bidScale, &askRaw, &askScale); err != nil {
			return nil, fmt.Errorf("scan opportunity bbo: %w", err)
		}
		bid, bidOK := decodePrice(bidRaw, bidScale)
		ask, askOK := decodePrice(askRaw, askScale)
		if !bidOK || !askOK || bid > ask {
			continue
		}
		result = append(result, Quote{
			TS: bucket.UTC(), Symbol: symbol, Venue: venue, Bid: bid, Ask: ask,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read opportunity bbo: %w", err)
	}
	return result, nil
}

func historyQuery(database, table string) string {
	return fmt.Sprintf(`
SELECT
	toStartOfMinute(ts) AS bucket,
	canonical_symbol,
	venue,
	argMax(bid_price, ts) AS bid_raw,
	argMax(price_scale, ts) AS bid_scale,
	argMax(ask_price, ts) AS ask_raw,
	argMax(price_scale, ts) AS ask_scale
FROM %s.%s
PREWHERE product = 'perpetual'
	AND ts >= @from AND ts < @to
	AND canonical_symbol IN @symbols
	AND venue IN @venues
GROUP BY bucket, canonical_symbol, venue
HAVING bucket >= @from AND bucket < @to
ORDER BY canonical_symbol, venue, bucket
`, database, table)
}

func decodePrice(raw int64, scale uint8) (float64, bool) {
	if raw <= 0 {
		return 0, false
	}
	value := float64(raw)
	for range scale {
		value /= 10
	}
	return value, value > 0
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
