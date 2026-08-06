CREATE TABLE IF NOT EXISTS instruments (
    id BIGSERIAL PRIMARY KEY,
    exchange TEXT NOT NULL,
    exchange_symbol TEXT NOT NULL,
    base_asset TEXT NOT NULL,
    quote_asset TEXT NOT NULL,
    global_symbol TEXT NOT NULL,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS instruments_exchange_symbol_uq
    ON instruments (exchange, exchange_symbol);
CREATE INDEX IF NOT EXISTS instruments_global_symbol_idx
    ON instruments (global_symbol);

CREATE TABLE IF NOT EXISTS funding_rates (
    id BIGSERIAL PRIMARY KEY,
    instrument_id BIGINT NOT NULL REFERENCES instruments(id) ON DELETE CASCADE,
    rate DOUBLE PRECISION NOT NULL,
    funding_time TIMESTAMPTZ NOT NULL,
    settled BOOLEAN NOT NULL,
    interval_hours DOUBLE PRECISION NOT NULL DEFAULT 8,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS funding_rates_current_uq
    ON funding_rates (instrument_id) WHERE settled = FALSE;
CREATE UNIQUE INDEX IF NOT EXISTS funding_rates_settled_uq
    ON funding_rates (instrument_id, funding_time) WHERE settled = TRUE;
CREATE INDEX IF NOT EXISTS funding_rates_lookup_idx
    ON funding_rates (funding_time DESC, instrument_id);
