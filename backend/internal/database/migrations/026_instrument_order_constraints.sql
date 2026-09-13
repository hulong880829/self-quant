ALTER TABLE instruments
    ADD COLUMN IF NOT EXISTS min_quantity NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS min_notional NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS min_quantity_status TEXT NOT NULL DEFAULT 'unknown',
    ADD COLUMN IF NOT EXISTS min_notional_status TEXT NOT NULL DEFAULT 'unknown';

ALTER TABLE instruments
    DROP CONSTRAINT IF EXISTS instruments_min_quantity_status_check,
    DROP CONSTRAINT IF EXISTS instruments_min_notional_status_check,
    ADD CONSTRAINT instruments_min_quantity_status_check
        CHECK (min_quantity_status IN ('known', 'not_applicable', 'unknown')),
    ADD CONSTRAINT instruments_min_notional_status_check
        CHECK (min_notional_status IN ('known', 'not_applicable', 'unknown')),
    DROP CONSTRAINT IF EXISTS instruments_min_quantity_value_check,
    DROP CONSTRAINT IF EXISTS instruments_min_notional_value_check,
    ADD CONSTRAINT instruments_min_quantity_value_check
        CHECK (
            (min_quantity_status = 'known' AND min_quantity > 0)
            OR (min_quantity_status <> 'known' AND min_quantity IS NULL)
        ),
    ADD CONSTRAINT instruments_min_notional_value_check
        CHECK (
            (min_notional_status = 'known' AND min_notional > 0)
            OR (min_notional_status <> 'known' AND min_notional IS NULL)
        );
