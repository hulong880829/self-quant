CREATE TABLE IF NOT EXISTS trader_arbitrage_combinations (
    id UUID PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    request_fingerprint TEXT NOT NULL,
    owner_username TEXT NOT NULL,
    leg_a_trading_account_id BIGINT NOT NULL REFERENCES trading_accounts(id),
    leg_a_instrument_id BIGINT NOT NULL REFERENCES instruments(id),
    leg_a_product_name TEXT NOT NULL,
    leg_a_account_name TEXT NOT NULL,
    leg_a_exchange TEXT NOT NULL,
    leg_a_contract_type TEXT NOT NULL,
    leg_a_exchange_symbol TEXT NOT NULL,
    leg_a_base_asset TEXT NOT NULL,
    leg_a_quote_asset TEXT NOT NULL,
    leg_b_trading_account_id BIGINT NOT NULL REFERENCES trading_accounts(id),
    leg_b_instrument_id BIGINT NOT NULL REFERENCES instruments(id),
    leg_b_product_name TEXT NOT NULL,
    leg_b_account_name TEXT NOT NULL,
    leg_b_exchange TEXT NOT NULL,
    leg_b_contract_type TEXT NOT NULL,
    leg_b_exchange_symbol TEXT NOT NULL,
    leg_b_base_asset TEXT NOT NULL,
    leg_b_quote_asset TEXT NOT NULL,
    ask_threshold_bps NUMERIC NOT NULL,
    bid_threshold_bps NUMERIC NOT NULL,
    target_notional NUMERIC NOT NULL,
    order_notional NUMERIC NOT NULL,
    max_delta_notional NUMERIC NOT NULL,
    execution_mode TEXT NOT NULL CHECK (execution_mode IN ('maker_then_hedge','simultaneous_market')),
    maker_leg TEXT NOT NULL DEFAULT '' CHECK (maker_leg IN ('','a','b')),
    status TEXT NOT NULL CHECK (status IN ('running','closing','closed','failed')),
    completed_notional NUMERIC NOT NULL DEFAULT 0,
    current_ask_spread_bps NUMERIC,
    current_bid_spread_bps NUMERIC,
    market_data_stale BOOLEAN NOT NULL DEFAULT TRUE,
    error_message TEXT NOT NULL DEFAULT '',
    scheduler_lease_until TIMESTAMPTZ NOT NULL DEFAULT '-infinity',
    version BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at TIMESTAMPTZ,
    CHECK (leg_a_trading_account_id <> leg_b_trading_account_id),
    CHECK (leg_a_exchange <> leg_b_exchange),
    CHECK (ask_threshold_bps > 0),
    CHECK (bid_threshold_bps < 0),
    CHECK (target_notional > 0 AND order_notional > 0 AND order_notional <= target_notional),
    CHECK (max_delta_notional > 0 AND max_delta_notional <= order_notional)
);

CREATE INDEX IF NOT EXISTS trader_arbitrage_owner_running_idx
    ON trader_arbitrage_combinations(owner_username, updated_at DESC, id DESC)
    WHERE status IN ('running','closing');
CREATE INDEX IF NOT EXISTS trader_arbitrage_lease_idx
    ON trader_arbitrage_combinations(scheduler_lease_until, updated_at)
    WHERE status IN ('running','closing');
CREATE INDEX IF NOT EXISTS trader_arbitrage_cleanup_idx
    ON trader_arbitrage_combinations(closed_at, id)
    WHERE status IN ('closed','failed');

CREATE TABLE IF NOT EXISTS trader_arbitrage_executions (
    id UUID PRIMARY KEY,
    combination_id UUID NOT NULL REFERENCES trader_arbitrage_combinations(id) ON DELETE CASCADE,
    direction TEXT NOT NULL CHECK (direction IN ('ask','bid')),
    sequence BIGINT NOT NULL,
    status TEXT NOT NULL CHECK (status IN (
        'claimed','maker_open','maker_canceling','hedging','reconciling',
        'completed','failed','canceled','dry_run'
    )),
    trigger_ask_spread_bps NUMERIC NOT NULL,
    trigger_bid_spread_bps NUMERIC NOT NULL,
    trigger_leg_a_bid NUMERIC NOT NULL,
    trigger_leg_a_ask NUMERIC NOT NULL,
    trigger_leg_b_bid NUMERIC NOT NULL,
    trigger_leg_b_ask NUMERIC NOT NULL,
    target_base_quantity NUMERIC NOT NULL,
    leg_a_filled_quantity NUMERIC NOT NULL DEFAULT 0,
    leg_b_filled_quantity NUMERIC NOT NULL DEFAULT 0,
    delta_notional NUMERIC NOT NULL DEFAULT 0,
    maker_order_id UUID REFERENCES trader_orders(id) ON DELETE SET NULL,
    hedge_order_id UUID REFERENCES trader_orders(id) ON DELETE SET NULL,
    attempt INTEGER NOT NULL DEFAULT 0,
    error_message TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at TIMESTAMPTZ,
    UNIQUE (combination_id, direction, sequence)
);

CREATE UNIQUE INDEX IF NOT EXISTS trader_arbitrage_one_active_direction_idx
    ON trader_arbitrage_executions(combination_id, direction)
    WHERE status NOT IN ('completed','failed','canceled','dry_run');
CREATE INDEX IF NOT EXISTS trader_arbitrage_execution_combo_idx
    ON trader_arbitrage_executions(combination_id, created_at DESC);

CREATE TABLE IF NOT EXISTS trader_arbitrage_events (
    id BIGSERIAL PRIMARY KEY,
    combination_id UUID NOT NULL REFERENCES trader_arbitrage_combinations(id) ON DELETE CASCADE,
    execution_id UUID REFERENCES trader_arbitrage_executions(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS trader_arbitrage_events_combo_idx
    ON trader_arbitrage_events(combination_id, created_at DESC);

ALTER TABLE trader_orders
    ADD COLUMN IF NOT EXISTS arbitrage_execution_id UUID
        REFERENCES trader_arbitrage_executions(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS arbitrage_leg TEXT
        CHECK (arbitrage_leg IS NULL OR arbitrage_leg IN ('a','b')),
    ADD COLUMN IF NOT EXISTS arbitrage_role TEXT
        CHECK (arbitrage_role IS NULL OR arbitrage_role IN ('maker','hedge','market','residual'));

CREATE INDEX IF NOT EXISTS trader_orders_arbitrage_execution_idx
    ON trader_orders(arbitrage_execution_id, created_at)
    WHERE arbitrage_execution_id IS NOT NULL;
