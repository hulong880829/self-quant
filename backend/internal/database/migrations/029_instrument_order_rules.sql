ALTER TABLE instruments
    ADD COLUMN IF NOT EXISTS max_quantity NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS max_quantity_status TEXT NOT NULL DEFAULT 'unknown',
    ADD COLUMN IF NOT EXISTS market_quantity_step NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS market_quantity_step_status TEXT NOT NULL DEFAULT 'unknown',
    ADD COLUMN IF NOT EXISTS market_min_quantity NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS market_min_quantity_status TEXT NOT NULL DEFAULT 'unknown',
    ADD COLUMN IF NOT EXISTS market_max_quantity NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS market_max_quantity_status TEXT NOT NULL DEFAULT 'unknown',
    ADD COLUMN IF NOT EXISTS market_min_notional NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS market_min_notional_status TEXT NOT NULL DEFAULT 'unknown';

ALTER TABLE instruments
    ADD CONSTRAINT instruments_max_quantity_status_check
        CHECK (max_quantity_status IN ('known', 'not_applicable', 'unknown')),
    ADD CONSTRAINT instruments_market_quantity_step_status_check
        CHECK (market_quantity_step_status IN ('known', 'not_applicable', 'unknown')),
    ADD CONSTRAINT instruments_market_min_quantity_status_check
        CHECK (market_min_quantity_status IN ('known', 'not_applicable', 'unknown')),
    ADD CONSTRAINT instruments_market_max_quantity_status_check
        CHECK (market_max_quantity_status IN ('known', 'not_applicable', 'unknown')),
    ADD CONSTRAINT instruments_market_min_notional_status_check
        CHECK (market_min_notional_status IN ('known', 'not_applicable', 'unknown')),
    ADD CONSTRAINT instruments_max_quantity_value_check
        CHECK (
            (max_quantity_status = 'known' AND max_quantity > 0)
            OR (max_quantity_status <> 'known' AND max_quantity IS NULL)
        ),
    ADD CONSTRAINT instruments_market_quantity_step_value_check
        CHECK (
            (market_quantity_step_status = 'known' AND market_quantity_step > 0)
            OR (market_quantity_step_status <> 'known' AND market_quantity_step IS NULL)
        ),
    ADD CONSTRAINT instruments_market_min_quantity_value_check
        CHECK (
            (market_min_quantity_status = 'known' AND market_min_quantity > 0)
            OR (market_min_quantity_status <> 'known' AND market_min_quantity IS NULL)
        ),
    ADD CONSTRAINT instruments_market_max_quantity_value_check
        CHECK (
            (market_max_quantity_status = 'known' AND market_max_quantity > 0)
            OR (market_max_quantity_status <> 'known' AND market_max_quantity IS NULL)
        ),
    ADD CONSTRAINT instruments_market_min_notional_value_check
        CHECK (
            (market_min_notional_status = 'known' AND market_min_notional > 0)
            OR (market_min_notional_status <> 'known' AND market_min_notional IS NULL)
        );
