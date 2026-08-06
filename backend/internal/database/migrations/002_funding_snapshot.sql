ALTER TABLE instruments
    ADD COLUMN IF NOT EXISTS settle_asset TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS contract_type TEXT NOT NULL DEFAULT 'perpetual',
    ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'active',
    ADD COLUMN IF NOT EXISTS contract_size NUMERIC(30, 18),
    ADD COLUMN IF NOT EXISTS price_tick NUMERIC(30, 18),
    ADD COLUMN IF NOT EXISTS quantity_step NUMERIC(30, 18),
    ADD COLUMN IF NOT EXISTS funding_interval_seconds INTEGER NOT NULL DEFAULT 28800,
    ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS source_updated_at TIMESTAMPTZ;

ALTER TABLE funding_rates
    ADD COLUMN IF NOT EXISTS record_kind TEXT,
    ADD COLUMN IF NOT EXISTS next_funding_rate NUMERIC(30, 18),
    ADD COLUMN IF NOT EXISTS annualized_rate NUMERIC(30, 18),
    ADD COLUMN IF NOT EXISTS next_funding_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS mark_price NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS index_price NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS last_price NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS open_interest_contracts NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS open_interest_base NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS open_interest_notional_usd NUMERIC(38, 8),
    ADD COLUMN IF NOT EXISTS volume_24h_base NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS turnover_24h_usd NUMERIC(38, 8),
    ADD COLUMN IF NOT EXISTS price_change_24h NUMERIC(30, 18),
    ADD COLUMN IF NOT EXISTS source_updated_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS received_at TIMESTAMPTZ NOT NULL DEFAULT now();

UPDATE funding_rates
SET record_kind = CASE WHEN settled THEN 'settled' ELSE 'current' END
WHERE record_kind IS NULL;

ALTER TABLE funding_rates
    ALTER COLUMN record_kind SET NOT NULL,
    ALTER COLUMN rate TYPE NUMERIC(30, 18) USING rate::numeric,
    ALTER COLUMN interval_hours TYPE NUMERIC(10, 4) USING interval_hours::numeric;

ALTER TABLE funding_rates
    RENAME COLUMN rate TO funding_rate;

ALTER TABLE funding_rates
    DROP CONSTRAINT IF EXISTS funding_rates_record_kind_check;
ALTER TABLE funding_rates
    ADD CONSTRAINT funding_rates_record_kind_check
    CHECK (record_kind IN ('current', 'settled'));

ALTER TABLE funding_rates
    DROP CONSTRAINT IF EXISTS funding_rates_instrument_id_fkey;
ALTER TABLE funding_rates
    ADD CONSTRAINT funding_rates_instrument_id_fkey
    FOREIGN KEY (instrument_id) REFERENCES instruments(id) ON DELETE RESTRICT;

DROP INDEX IF EXISTS funding_rates_current_uq;
DROP INDEX IF EXISTS funding_rates_settled_uq;
CREATE UNIQUE INDEX IF NOT EXISTS funding_rates_current_uq
    ON funding_rates (instrument_id) WHERE record_kind = 'current';
CREATE UNIQUE INDEX IF NOT EXISTS funding_rates_settled_uq
    ON funding_rates (instrument_id, funding_time) WHERE record_kind = 'settled';
CREATE INDEX IF NOT EXISTS funding_rates_history_idx
    ON funding_rates (instrument_id, funding_time DESC)
    WHERE record_kind = 'settled';

ALTER TABLE funding_rates DROP COLUMN IF EXISTS settled;
