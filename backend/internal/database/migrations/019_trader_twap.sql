CREATE TABLE IF NOT EXISTS trader_twap_jobs (
    id UUID PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    request_fingerprint TEXT NOT NULL,
    owner_username TEXT NOT NULL REFERENCES accounts(username) ON DELETE CASCADE,
    trading_account_id BIGINT NOT NULL REFERENCES trading_accounts(id) ON DELETE RESTRICT,
    product_name TEXT NOT NULL,
    exchange TEXT NOT NULL,
    instrument_id BIGINT NOT NULL REFERENCES instruments(id) ON DELETE RESTRICT,
    contract_type TEXT NOT NULL,
    exchange_symbol TEXT NOT NULL,
    base_asset TEXT NOT NULL,
    quote_asset TEXT NOT NULL,
    side TEXT NOT NULL,
    total_quantity NUMERIC(38, 18) NOT NULL,
    filled_quantity NUMERIC(38, 18) NOT NULL DEFAULT 0,
    average_price NUMERIC(38, 18) NOT NULL DEFAULT 0,
    start_at TIMESTAMPTZ NOT NULL,
    end_at TIMESTAMPTZ NOT NULL,
    interval_seconds INTEGER NOT NULL,
    limit_price NUMERIC(38, 18),
    max_quantity NUMERIC(38, 18),
    execution_type TEXT NOT NULL,
    order_timeout_seconds INTEGER,
    status TEXT NOT NULL DEFAULT 'pending',
    current_slice INTEGER NOT NULL DEFAULT 0,
    next_action_at TIMESTAMPTZ NOT NULL,
    active_order_id UUID REFERENCES trader_orders(id) ON DELETE SET NULL,
    scheduler_lease_until TIMESTAMPTZ,
    scheduler_failures INTEGER NOT NULL DEFAULT 0,
    error_message TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    closed_at TIMESTAMPTZ,
    CONSTRAINT trader_twap_side_chk CHECK (side IN ('buy', 'sell')),
    CONSTRAINT trader_twap_contract_chk CHECK (contract_type IN ('spot', 'perpetual')),
    CONSTRAINT trader_twap_execution_chk CHECK (execution_type IN ('maker', 'market')),
    CONSTRAINT trader_twap_status_chk CHECK (
        status IN (
            'pending', 'running', 'completed', 'partially_completed',
            'canceled', 'failed'
        )
    ),
    CONSTRAINT trader_twap_quantity_chk CHECK (total_quantity > 0),
    CONSTRAINT trader_twap_time_chk CHECK (end_at > start_at),
    CONSTRAINT trader_twap_interval_chk CHECK (interval_seconds > 0),
    CONSTRAINT trader_twap_timeout_chk CHECK (
        (execution_type = 'maker' AND order_timeout_seconds > 0
            AND order_timeout_seconds < interval_seconds)
        OR
        (execution_type = 'market' AND order_timeout_seconds IS NULL)
    )
);

ALTER TABLE trader_orders
    ADD COLUMN IF NOT EXISTS twap_job_id UUID
        REFERENCES trader_twap_jobs(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS twap_slice_index INTEGER;

CREATE UNIQUE INDEX IF NOT EXISTS trader_orders_twap_slice_uq
    ON trader_orders (twap_job_id, twap_slice_index)
    WHERE twap_job_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS trader_twap_jobs_due_idx
    ON trader_twap_jobs (next_action_at, updated_at)
    WHERE status IN ('pending', 'running');

CREATE INDEX IF NOT EXISTS trader_twap_jobs_account_active_idx
    ON trader_twap_jobs (owner_username, trading_account_id, created_at DESC)
    WHERE status IN ('pending', 'running');

CREATE INDEX IF NOT EXISTS trader_twap_jobs_closed_idx
    ON trader_twap_jobs (closed_at)
    WHERE status IN ('completed', 'partially_completed', 'canceled', 'failed');

CREATE TABLE IF NOT EXISTS trader_twap_events (
    id BIGSERIAL PRIMARY KEY,
    twap_job_id UUID NOT NULL REFERENCES trader_twap_jobs(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS trader_twap_events_job_idx
    ON trader_twap_events (twap_job_id, created_at ASC);
