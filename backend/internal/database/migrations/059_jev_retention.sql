ALTER TABLE jev_arb_runs
    ADD COLUMN IF NOT EXISTS archived_realized_spread_pnl NUMERIC NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS archived_funding_pnl NUMERIC NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS archived_cost_paid NUMERIC NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS archived_position_count BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS jev_arb_snapshots_created_id_idx
    ON jev_arb_snapshots (created_at, id);

CREATE INDEX IF NOT EXISTS jev_arb_equity_points_ts_id_idx
    ON jev_arb_equity_points (ts, id);

CREATE INDEX IF NOT EXISTS jev_arb_decisions_terminal_decision_at_idx
    ON jev_arb_decisions (decision_at, id)
    WHERE execution_status <> 'pending';

CREATE INDEX IF NOT EXISTS jev_arb_positions_closed_at_idx
    ON jev_arb_positions (closed_at, id)
    WHERE status = 'closed';

CREATE INDEX IF NOT EXISTS jev_arb_runs_stopped_updated_idx
    ON jev_arb_runs (updated_at, id)
    WHERE status = 'stopped';
