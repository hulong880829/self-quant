WITH
    toStartOfMinute(now() - INTERVAL 2 HOUR) AS from_time,
    toStartOfMinute(now() - INTERVAL 5 MINUTE) AS to_time
SELECT
    bucket,
    product,
    canonical_symbol,
    venue,
    raw.bbo AS raw_bbo,
    minute.bbo AS minute_bbo,
    raw.bbo = minute.bbo AS matches
FROM
(
    SELECT
        toDateTime(toStartOfMinute(ts), 'UTC') AS bucket,
        product,
        canonical_symbol,
        venue,
        argMax(tuple(bid_price, price_scale, ask_price, price_scale), ts) AS bbo,
        1 AS raw_present
    FROM market_data.crypto_bbo
    WHERE ts >= from_time AND ts < to_time
    GROUP BY bucket, product, canonical_symbol, venue
) AS raw
FULL OUTER JOIN
(
    SELECT
        bucket,
        product,
        canonical_symbol,
        venue,
        argMaxMerge(bbo_state) AS bbo,
        1 AS minute_present
    FROM market_data.crypto_bbo_minute
    WHERE bucket >= from_time AND bucket < to_time
    GROUP BY bucket, product, canonical_symbol, venue
) AS minute USING (bucket, product, canonical_symbol, venue)
WHERE raw.bbo != minute.bbo
   OR isNull(raw_present)
   OR isNull(minute_present)
ORDER BY bucket, product, canonical_symbol, venue
LIMIT 100
SETTINGS join_use_nulls = 1;
