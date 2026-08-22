CREATE TABLE IF NOT EXISTS fair_price_snapshots (
    profile TEXT NOT NULL,
    symbol TEXT NOT NULL,
    model_id TEXT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    ring_epoch NUMERIC(20,0) NOT NULL,
    ring_sequence NUMERIC(20,0) NOT NULL,
    generation NUMERIC(20,0) NOT NULL,
    source_wall_ns BIGINT NOT NULL,
    exchange_ts_ns BIGINT NOT NULL,
    price_scale SMALLINT NOT NULL CHECK (price_scale BETWEEN 0 AND 18),
    quantity_scale SMALLINT NOT NULL CHECK (quantity_scale BETWEEN 0 AND 18),
    price NUMERIC(38,18) NOT NULL,
    price_raw NUMERIC(38,18) NOT NULL,
    mid NUMERIC(38,18) NOT NULL,
    microprice NUMERIC(38,18) NOT NULL,
    best_bid NUMERIC(38,18) NOT NULL,
    best_ask NUMERIC(38,18) NOT NULL,
    effective_bid NUMERIC(38,18) NOT NULL,
    effective_ask NUMERIC(38,18) NOT NULL,
    impact_bid NUMERIC(38,18),
    impact_ask NUMERIC(38,18),
    crossed_quantity NUMERIC(38,18),
    imbalance DOUBLE PRECISION NOT NULL,
    spread_bps DOUBLE PRECISION NOT NULL,
    cross_bps DOUBLE PRECISION,
    weighted_bid_depth DOUBLE PRECISION NOT NULL,
    weighted_ask_depth DOUBLE PRECISION NOT NULL,
    depth_bid_notional DOUBLE PRECISION NOT NULL,
    depth_ask_notional DOUBLE PRECISION NOT NULL,
    active_venue_mask INTEGER NOT NULL,
    crossed BOOLEAN NOT NULL,
    degraded BOOLEAN NOT NULL,
    degraded_reasons TEXT[] NOT NULL DEFAULT '{}',
    PRIMARY KEY (profile, symbol, model_id, observed_at)
);

CREATE INDEX IF NOT EXISTS fair_price_snapshots_symbol_time_idx
    ON fair_price_snapshots (profile, symbol, observed_at DESC);

CREATE INDEX IF NOT EXISTS fair_price_snapshots_retention_idx
    ON fair_price_snapshots (observed_at);
