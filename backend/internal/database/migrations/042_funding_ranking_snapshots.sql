CREATE TABLE IF NOT EXISTS funding_ranking_snapshots (
    period TEXT NOT NULL CHECK (period IN ('8h', '24h')),
    schema_version INTEGER NOT NULL,
    model_version TEXT NOT NULL,
    generation BIGINT NOT NULL CHECK (generation >= 0),
    payload JSONB NOT NULL,
    calculated_at TIMESTAMPTZ NOT NULL,
    last_successful_at TIMESTAMPTZ NOT NULL,
    data_through TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (period, schema_version, model_version)
);

CREATE INDEX IF NOT EXISTS funding_ranking_snapshots_period_updated_idx
    ON funding_ranking_snapshots (period, updated_at DESC);
