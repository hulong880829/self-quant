CREATE TABLE IF NOT EXISTS trader_arbitrage_funding_estimates (
    id BIGSERIAL PRIMARY KEY,
    combination_id UUID NOT NULL
        REFERENCES trader_arbitrage_combinations(id) ON DELETE CASCADE,
    leg TEXT NOT NULL CHECK (leg IN ('a', 'b')),
    instrument_id BIGINT NOT NULL REFERENCES instruments(id) ON DELETE RESTRICT,
    funding_time TIMESTAMPTZ NOT NULL,
    funding_rate DOUBLE PRECISION NOT NULL,
    signed_position_quantity NUMERIC(38, 18) NOT NULL DEFAULT 0,
    reference_price NUMERIC(38, 18) NOT NULL DEFAULT 0,
    estimated_funding_pnl NUMERIC(38, 18) NOT NULL DEFAULT 0,
    coverage_complete BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (combination_id, leg, funding_time)
);

CREATE INDEX IF NOT EXISTS trader_arbitrage_funding_estimates_combo_idx
    ON trader_arbitrage_funding_estimates(combination_id, funding_time);
