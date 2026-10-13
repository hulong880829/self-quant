CREATE DATABASE IF NOT EXISTS market_data;

CREATE TABLE IF NOT EXISTS market_data.crypto_bbo
(
    ts DateTime64(9, 'UTC'),
    venue LowCardinality(String),
    product LowCardinality(String),
    canonical_symbol String,
    bid_price Int64,
    bid_quantity Int64,
    ask_price Int64,
    ask_quantity Int64,
    price_scale UInt8,
    quantity_scale UInt8,
    instrument_id UInt64,
    venue_symbol String
)
ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY (venue, product, canonical_symbol, ts);
