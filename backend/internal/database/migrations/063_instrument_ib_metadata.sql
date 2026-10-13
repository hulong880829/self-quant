ALTER TABLE instruments
    ADD COLUMN IF NOT EXISTS ib_con_id BIGINT,
    ADD COLUMN IF NOT EXISTS primary_exchange TEXT,
    ADD COLUMN IF NOT EXISTS local_symbol TEXT,
    ADD COLUMN IF NOT EXISTS trading_class TEXT,
    ADD COLUMN IF NOT EXISTS last_verified_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS next_verify_at TIMESTAMPTZ;

CREATE UNIQUE INDEX IF NOT EXISTS instruments_ib_con_id_uq
    ON instruments (ib_con_id)
    WHERE ib_con_id IS NOT NULL;
