package ranking

import (
	"context"
	"crypto/tls"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type RepositoryConfig struct {
	Address        string
	Database       string
	Table          string
	User           string
	Password       string
	TLS            bool
	TLSSkipVerify  bool
	QueryTimeout   time.Duration
	PoolSize       int
	UseMinuteTable bool
}

type Repository struct {
	conn       driver.Conn
	database   string
	table      string
	timeout    time.Duration
	settings   clickhouse.Settings
	queryBatch func(
		context.Context,
		time.Time,
		time.Time,
		[]HistoryPair,
		time.Duration,
		func(HistoryQuote) error,
	) error
}

type historyConsumerError struct {
	err error
}

func (e historyConsumerError) Error() string { return e.err.Error() }
func (e historyConsumerError) Unwrap() error { return e.err }

type historyQueryBinding struct {
	pairVenues        []string
	pairSymbols       []string
	uniqueVenues      []string
	uniqueSymbols     []string
	canonicalBySource map[string]string
}

func rankingClickHouseSettings() clickhouse.Settings {
	return clickhouse.Settings{
		"readonly":    1,
		"max_threads": 2,
	}
}

func rankingClickHouseOptions(cfg RepositoryConfig) *clickhouse.Options {
	options := &clickhouse.Options{
		Addr: []string{cfg.Address},
		Auth: clickhouse.Auth{
			Database: cfg.Database, Username: cfg.User, Password: cfg.Password,
		},
		Protocol:     clickhouse.Native,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  max(cfg.QueryTimeout, 2*time.Minute),
		MaxOpenConns: cfg.PoolSize, MaxIdleConns: cfg.PoolSize,
		Settings: rankingClickHouseSettings(),
	}
	if cfg.TLS {
		options.TLS = &tls.Config{InsecureSkipVerify: cfg.TLSSkipVerify} //nolint:gosec // explicit operator setting
	}
	return options
}

func Open(ctx context.Context, cfg RepositoryConfig) (*Repository, error) {
	options := rankingClickHouseOptions(cfg)
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
	table := cfg.Table
	if cfg.UseMinuteTable {
		table = minuteTable(table)
	}
	repository := &Repository{
		conn: conn, database: cfg.Database, table: table, timeout: cfg.QueryTimeout,
		settings: options.Settings,
	}
	repository.queryBatch = repository.queryPairBatch
	return repository, nil
}

func (r *Repository) Close() error {
	if r == nil || r.conn == nil {
		return nil
	}
	return r.conn.Close()
}

func (r *Repository) QueryPairs(
	ctx context.Context,
	from time.Time,
	to time.Time,
	pairs []HistoryPair,
	consume func(HistoryQuote) error,
) error {
	return r.queryPairs(ctx, from, to, pairs, r.timeout, consume)
}

func (r *Repository) QueryWarmPairs(
	ctx context.Context,
	from time.Time,
	to time.Time,
	pairs []HistoryPair,
	consume func(HistoryQuote) error,
) error {
	return r.queryPairs(ctx, from, to, pairs, max(r.timeout, 2*time.Minute), consume)
}

func (r *Repository) queryPairs(
	ctx context.Context,
	from time.Time,
	to time.Time,
	pairs []HistoryPair,
	timeout time.Duration,
	consume func(HistoryQuote) error,
) error {
	if len(pairs) == 0 || !from.Before(to) {
		return nil
	}
	pairs = uniqueHistoryPairs(pairs)
	batch := r.queryBatch
	if batch == nil {
		batch = r.queryPairBatch
	}
	return batch(ctx, from, to, pairs, timeout, consume)
}

func historyQueryBindings(pairs []HistoryPair) historyQueryBinding {
	binding := historyQueryBinding{
		pairVenues:        make([]string, len(pairs)),
		pairSymbols:       make([]string, len(pairs)),
		canonicalBySource: make(map[string]string, len(pairs)),
	}
	venues := make([]string, 0, len(pairs))
	symbols := make([]string, 0, len(pairs))
	for index, pair := range pairs {
		binding.pairVenues[index] = pair.Venue
		binding.pairSymbols[index] = pair.SourceSymbol
		binding.canonicalBySource[pair.Venue+"\x00"+pair.SourceSymbol] = pair.CanonicalSymbol
		venues = append(venues, pair.Venue)
		symbols = append(symbols, pair.SourceSymbol)
	}
	binding.uniqueVenues = uniqueSorted(venues)
	binding.uniqueSymbols = uniqueSorted(symbols)
	return binding
}

func (r *Repository) queryPairBatch(
	ctx context.Context,
	from time.Time,
	to time.Time,
	pairs []HistoryPair,
	timeout time.Duration,
	consume func(HistoryQuote) error,
) error {
	binding := historyQueryBindings(pairs)
	queryCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		queryCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	rows, err := r.conn.Query(
		queryCtx, exactHistoryQueryForTable(r.database, r.table),
		clickhouse.Named("from", from.UTC()),
		clickhouse.Named("to", to.UTC()),
		clickhouse.Named("pair_venues", binding.pairVenues),
		clickhouse.Named("pair_symbols", binding.pairSymbols),
		clickhouse.Named("unique_venues", binding.uniqueVenues),
		clickhouse.Named("unique_symbols", binding.uniqueSymbols),
	)
	if err != nil {
		return fmt.Errorf("query exact opportunity bbo: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			bucket             time.Time
			symbol, venue      string
			bidRaw, askRaw     int64
			bidScale, askScale uint8
		)
		if err := rows.Scan(
			&bucket, &symbol, &venue, &bidRaw, &bidScale, &askRaw, &askScale,
		); err != nil {
			return fmt.Errorf("scan exact opportunity bbo: %w", err)
		}
		bid, bidOK := decodePrice(bidRaw, bidScale)
		ask, askOK := decodePrice(askRaw, askScale)
		if !bidOK || !askOK || bid > ask {
			continue
		}
		canonical, ok := binding.canonicalBySource[venue+"\x00"+symbol]
		if !ok {
			continue
		}
		if err := consume(HistoryQuote{
			Pair: HistoryPair{
				Venue: venue, SourceSymbol: symbol, CanonicalSymbol: canonical,
			},
			Quote: MinuteQuote{Minute: bucket.Unix() / 60, Bid: bid, Ask: ask},
		}); err != nil {
			return historyConsumerError{err: err}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read exact opportunity bbo: %w", err)
	}
	return nil
}

func exactHistoryQueryForTable(database, table string) string {
	if strings.HasSuffix(table, "_minute") {
		return exactHistoryQuery(database, table)
	}
	return exactRawHistoryQuery(database, table)
}

func exactHistoryQuery(database, table string) string {
	return fmt.Sprintf(`
SELECT
	bucket,
	canonical_symbol,
	venue,
	tupleElement(argMaxMerge(bbo_state), 1) AS bid_raw,
	tupleElement(argMaxMerge(bbo_state), 2) AS bid_scale,
	tupleElement(argMaxMerge(bbo_state), 3) AS ask_raw,
	tupleElement(argMaxMerge(bbo_state), 4) AS ask_scale
FROM %s.%s
PREWHERE product = 'perpetual'
	AND canonical_symbol IN @unique_symbols
	AND venue IN @unique_venues
	AND bucket >= toStartOfMinute(@from) AND bucket < toStartOfMinute(@to)
WHERE has(
		arrayMap((venue_key, symbol_key) -> (venue_key, symbol_key), @pair_venues, @pair_symbols),
		(venue, canonical_symbol)
	)
GROUP BY bucket, canonical_symbol, venue
ORDER BY venue, canonical_symbol, bucket`, database, table)
}

func exactRawHistoryQuery(database, table string) string {
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
	AND canonical_symbol IN @unique_symbols
	AND venue IN @unique_venues
	AND ts >= @from AND ts < @to
WHERE has(
		arrayMap((venue_key, symbol_key) -> (venue_key, symbol_key), @pair_venues, @pair_symbols),
		(venue, canonical_symbol)
	)
GROUP BY bucket, canonical_symbol, venue
ORDER BY venue, canonical_symbol, bucket`, database, table)
}

func uniqueHistoryPairs(values []HistoryPair) []HistoryPair {
	result := append([]HistoryPair(nil), values...)
	sortHistoryPairs(result)
	write := 0
	for _, value := range result {
		if write > 0 && result[write-1] == value {
			continue
		}
		result[write] = value
		write++
	}
	return result[:write]
}

func minuteTable(table string) string {
	if strings.HasSuffix(table, "_minute") {
		return table
	}
	return table + "_minute"
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
