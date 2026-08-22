UPDATE instruments
SET contract_type = 'perpetual'
WHERE contract_type IS NULL OR btrim(contract_type) = '';

DROP INDEX IF EXISTS instruments_exchange_symbol_uq;

-- Defensive cleanup for databases that may have been populated without the old
-- unique index. Prefer the newest instrument and preserve non-conflicting rates.
WITH ranked AS (
    SELECT id,
           first_value(id) OVER (
               PARTITION BY exchange, contract_type, exchange_symbol
               ORDER BY updated_at DESC, id DESC
           ) AS keeper_id
    FROM instruments
), duplicates AS (
    SELECT id, keeper_id FROM ranked WHERE id <> keeper_id
)
DELETE FROM funding_rates duplicate_rate
USING duplicates duplicate
WHERE duplicate_rate.instrument_id = duplicate.id
  AND EXISTS (
      SELECT 1
      FROM funding_rates keeper_rate
      WHERE keeper_rate.instrument_id = duplicate.keeper_id
        AND keeper_rate.record_kind = duplicate_rate.record_kind
        AND (
            duplicate_rate.record_kind = 'current'
            OR keeper_rate.funding_time = duplicate_rate.funding_time
        )
  );

WITH ranked AS (
    SELECT id,
           first_value(id) OVER (
               PARTITION BY exchange, contract_type, exchange_symbol
               ORDER BY updated_at DESC, id DESC
           ) AS keeper_id
    FROM instruments
), duplicates AS (
    SELECT id, keeper_id FROM ranked WHERE id <> keeper_id
)
UPDATE funding_rates rate
SET instrument_id = duplicate.keeper_id
FROM duplicates duplicate
WHERE rate.instrument_id = duplicate.id;

WITH ranked AS (
    SELECT id,
           row_number() OVER (
               PARTITION BY exchange, contract_type, exchange_symbol
               ORDER BY updated_at DESC, id DESC
           ) AS rank
    FROM instruments
)
DELETE FROM instruments instrument
USING ranked
WHERE instrument.id = ranked.id AND ranked.rank > 1;

CREATE UNIQUE INDEX instruments_exchange_type_symbol_uq
    ON instruments (exchange, contract_type, exchange_symbol);

ALTER TABLE funding_rates
    DROP CONSTRAINT IF EXISTS funding_rates_instrument_id_fkey;
ALTER TABLE funding_rates
    ADD CONSTRAINT funding_rates_instrument_id_fkey
    FOREIGN KEY (instrument_id) REFERENCES instruments(id) ON DELETE CASCADE;

ALTER TABLE funding_rates
    ADD COLUMN IF NOT EXISTS cumulative_24h NUMERIC(30, 18),
    ADD COLUMN IF NOT EXISTS cumulative_7d NUMERIC(30, 18);
