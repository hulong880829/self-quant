ALTER TABLE funding_ranking_snapshots
    DROP CONSTRAINT IF EXISTS funding_ranking_snapshots_pkey;

ALTER TABLE funding_ranking_snapshots
    ADD CONSTRAINT funding_ranking_snapshots_pkey
    PRIMARY KEY (period, schema_version, model_version);

DROP INDEX IF EXISTS funding_ranking_snapshots_model_version_idx;

CREATE INDEX IF NOT EXISTS funding_ranking_snapshots_period_updated_idx
    ON funding_ranking_snapshots (period, updated_at DESC);
