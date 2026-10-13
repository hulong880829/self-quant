ALTER TABLE jev_arb_runs
    ADD COLUMN IF NOT EXISTS strategy_version TEXT NOT NULL DEFAULT 'JEVStrategyV6';

DROP INDEX IF EXISTS jev_arb_runs_active_uq;
CREATE UNIQUE INDEX IF NOT EXISTS jev_arb_runs_active_strategy_uq
    ON jev_arb_runs (strategy_version)
    WHERE status = 'active';

ALTER TABLE jev_arb_positions
    ADD COLUMN IF NOT EXISTS strategy_version TEXT NOT NULL DEFAULT 'JEVStrategyV6',
    ADD COLUMN IF NOT EXISTS exit_confirm_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS net_if_exit NUMERIC NULL,
    ADD COLUMN IF NOT EXISTS net_if_exit_at TIMESTAMPTZ NULL,
    ADD COLUMN IF NOT EXISTS bbo_missing_since TIMESTAMPTZ NULL;

ALTER TABLE jev_arb_decisions
    ADD COLUMN IF NOT EXISTS strategy_version TEXT NOT NULL DEFAULT 'JEVStrategyV6',
    ADD COLUMN IF NOT EXISTS parse_error_type TEXT NULL;
