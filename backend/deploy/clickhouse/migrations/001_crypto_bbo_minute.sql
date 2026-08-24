CREATE TABLE IF NOT EXISTS market_data.crypto_bbo_minute
(
    bucket DateTime('UTC'),
    venue LowCardinality(String),
    product LowCardinality(String),
    canonical_symbol String,
    bbo_state AggregateFunction(
        argMax,
        Tuple(Int64, UInt8, Int64, UInt8),
        DateTime64(9, 'UTC')
    )
)
ENGINE = AggregatingMergeTree
PARTITION BY toDate(bucket)
ORDER BY (product, canonical_symbol, venue, bucket)
TTL bucket + INTERVAL 10 DAY DELETE;

CREATE MATERIALIZED VIEW IF NOT EXISTS market_data.crypto_bbo_minute_mv
TO market_data.crypto_bbo_minute
AS
SELECT
    toStartOfMinute(ts) AS bucket,
    venue,
    product,
    canonical_symbol,
    argMaxState(
        tuple(bid_price, price_scale, ask_price, price_scale),
        ts
    ) AS bbo_state
FROM market_data.crypto_bbo
GROUP BY bucket, venue, product, canonical_symbol;
