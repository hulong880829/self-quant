CREATE TABLE IF NOT EXISTS jev_arb_runs (
    id BIGSERIAL PRIMARY KEY,
    status TEXT NOT NULL CHECK (status IN ('active', 'stopped')),
    initial_equity NUMERIC NOT NULL,
    equity NUMERIC NOT NULL,
    peak_equity NUMERIC NOT NULL,
    net_pnl NUMERIC NOT NULL DEFAULT 0,
    funding_pnl NUMERIC NOT NULL DEFAULT 0,
    realized_spread_pnl NUMERIC NOT NULL DEFAULT 0,
    unrealized_spread_pnl NUMERIC NOT NULL DEFAULT 0,
    cumulative_cost NUMERIC NOT NULL DEFAULT 0,
    max_drawdown NUMERIC NOT NULL DEFAULT 0,
    snapshot_version BIGINT NOT NULL DEFAULT 0,
    last_successful_at TIMESTAMPTZ,
    last_decision_at TIMESTAMPTZ,
    last_market_data_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX jev_arb_runs_active_uq
    ON jev_arb_runs ((true))
    WHERE status = 'active';

CREATE TABLE IF NOT EXISTS jev_arb_positions (
    id BIGSERIAL PRIMARY KEY,
    run_id BIGINT NOT NULL REFERENCES jev_arb_runs(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('single', 'cross')),
    status TEXT NOT NULL CHECK (status IN ('open', 'closed')),
    candidate_key TEXT NOT NULL,
    base_asset TEXT NOT NULL,
    quote_asset TEXT NOT NULL,
    global_symbol TEXT NOT NULL,
    long_exchange TEXT NOT NULL,
    long_symbol TEXT NOT NULL,
    long_product TEXT NOT NULL CHECK (long_product IN ('spot', 'perpetual')),
    short_exchange TEXT NOT NULL,
    short_symbol TEXT NOT NULL,
    short_product TEXT NOT NULL CHECK (short_product IN ('spot', 'perpetual')),
    shared_base_qty NUMERIC NOT NULL,
    long_qty NUMERIC NOT NULL,
    short_qty NUMERIC NOT NULL,
    long_entry_price NUMERIC NOT NULL,
    short_entry_price NUMERIC NOT NULL,
    long_mark_price NUMERIC,
    short_mark_price NUMERIC,
    realized_spread_pnl NUMERIC NOT NULL DEFAULT 0,
    unrealized_spread_pnl NUMERIC NOT NULL DEFAULT 0,
    funding_pnl NUMERIC NOT NULL DEFAULT 0,
    cost_paid NUMERIC NOT NULL DEFAULT 0,
    funding_checked_through_long TIMESTAMPTZ,
    funding_checked_through_short TIMESTAMPTZ,
    exit_pending_reason TEXT,
    degraded_reason TEXT,
    opened_at TIMESTAMPTZ NOT NULL,
    closed_at TIMESTAMPTZ,
    last_mark_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX jev_arb_positions_run_status_idx
    ON jev_arb_positions (run_id, status, id DESC);
CREATE INDEX jev_arb_positions_run_base_open_idx
    ON jev_arb_positions (run_id, base_asset)
    WHERE status = 'open';

CREATE TABLE IF NOT EXISTS jev_arb_decisions (
    id BIGSERIAL PRIMARY KEY,
    run_id BIGINT NOT NULL REFERENCES jev_arb_runs(id) ON DELETE CASCADE,
    position_id BIGINT REFERENCES jev_arb_positions(id) ON DELETE SET NULL,
    candidate_key TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('single', 'cross')),
    decision TEXT NOT NULL CHECK (decision IN ('hold', 'enter', 'exit')),
    policy_status TEXT NOT NULL CHECK (policy_status IN ('suggested', 'accepted', 'rejected')),
    execution_status TEXT NOT NULL CHECK (
        execution_status IN (
            'not_applicable',
            'pending',
            'filled',
            'expired',
            'portfolio_rejected'
        )
    ),
    enter_probability NUMERIC,
    exit_probability NUMERIC,
    confidence NUMERIC,
    expected_net_return_24h NUMERIC,
    rationale TEXT,
    prompt_version TEXT NOT NULL,
    policy_version TEXT NOT NULL,
    model_requested TEXT,
    model_actual TEXT,
    raw_response JSONB NOT NULL DEFAULT '{}'::jsonb,
    error_message TEXT,
    decision_at TIMESTAMPTZ NOT NULL,
    execution_started_at TIMESTAMPTZ,
    fill_deadline TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX jev_arb_decisions_run_id_idx
    ON jev_arb_decisions (run_id, id DESC);
CREATE INDEX jev_arb_decisions_pending_idx
    ON jev_arb_decisions (run_id, fill_deadline)
    WHERE execution_status = 'pending';

CREATE TABLE IF NOT EXISTS jev_arb_trades (
    id BIGSERIAL PRIMARY KEY,
    run_id BIGINT NOT NULL REFERENCES jev_arb_runs(id) ON DELETE CASCADE,
    position_id BIGINT NOT NULL REFERENCES jev_arb_positions(id) ON DELETE CASCADE,
    decision_id BIGINT REFERENCES jev_arb_decisions(id) ON DELETE SET NULL,
    side TEXT NOT NULL CHECK (side IN ('enter', 'exit')),
    shared_base_qty NUMERIC NOT NULL,
    long_fill_price NUMERIC NOT NULL,
    short_fill_price NUMERIC NOT NULL,
    notional NUMERIC NOT NULL,
    cost NUMERIC NOT NULL,
    filled_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX jev_arb_trades_run_id_idx
    ON jev_arb_trades (run_id, id DESC);

CREATE TABLE IF NOT EXISTS jev_arb_equity_points (
    id BIGSERIAL PRIMARY KEY,
    run_id BIGINT NOT NULL REFERENCES jev_arb_runs(id) ON DELETE CASCADE,
    ts TIMESTAMPTZ NOT NULL,
    equity NUMERIC NOT NULL,
    net_pnl NUMERIC NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX jev_arb_equity_points_run_ts_idx
    ON jev_arb_equity_points (run_id, ts DESC);

CREATE TABLE IF NOT EXISTS jev_arb_snapshots (
    id BIGSERIAL PRIMARY KEY,
    run_id BIGINT NOT NULL REFERENCES jev_arb_runs(id) ON DELETE CASCADE,
    snapshot_version BIGINT NOT NULL,
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX jev_arb_snapshots_run_version_uq
    ON jev_arb_snapshots (run_id, snapshot_version);
CREATE INDEX jev_arb_snapshots_run_id_idx
    ON jev_arb_snapshots (run_id, id DESC);
