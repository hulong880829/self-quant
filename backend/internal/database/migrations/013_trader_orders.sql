CREATE TABLE IF NOT EXISTS trader_orders (
    id UUID PRIMARY KEY,
    idempotency_key TEXT NOT NULL,
    owner_username TEXT NOT NULL REFERENCES accounts(username) ON DELETE CASCADE,
    trading_account_id BIGINT NOT NULL REFERENCES trading_accounts(id) ON DELETE RESTRICT,
    product_name TEXT NOT NULL,
    exchange TEXT NOT NULL,
    instrument_id BIGINT NOT NULL REFERENCES instruments(id) ON DELETE RESTRICT,
    contract_type TEXT NOT NULL,
    exchange_symbol TEXT NOT NULL,
    client_order_id TEXT NOT NULL,
    venue_order_id TEXT NOT NULL DEFAULT '',
    side TEXT NOT NULL,
    order_type TEXT NOT NULL,
    quantity NUMERIC(38, 18) NOT NULL,
    price NUMERIC(38, 18),
    filled_quantity NUMERIC(38, 18) NOT NULL DEFAULT 0,
    average_price NUMERIC(38, 18) NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    error_code TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    request_fingerprint TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT trader_orders_idempotency_uq UNIQUE (idempotency_key),
    CONSTRAINT trader_orders_client_order_uq UNIQUE (client_order_id),
    CONSTRAINT trader_orders_side_chk CHECK (side IN ('buy', 'sell')),
    CONSTRAINT trader_orders_type_chk CHECK (order_type IN ('market', 'limit')),
    CONSTRAINT trader_orders_contract_chk CHECK (contract_type IN ('spot', 'perpetual')),
    CONSTRAINT trader_orders_status_chk CHECK (
        status IN (
            'pending', 'open', 'partially_filled', 'filled',
            'canceled', 'rejected', 'expired', 'unknown'
        )
    )
);

CREATE INDEX IF NOT EXISTS trader_orders_owner_created_idx
    ON trader_orders (owner_username, created_at DESC);
CREATE INDEX IF NOT EXISTS trader_orders_account_created_idx
    ON trader_orders (trading_account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS trader_orders_pending_idx
    ON trader_orders (trading_account_id, status)
    WHERE status IN ('pending', 'open', 'partially_filled', 'unknown');

CREATE TABLE IF NOT EXISTS trader_order_events (
    id BIGSERIAL PRIMARY KEY,
    order_id UUID NOT NULL REFERENCES trader_orders(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS trader_order_events_order_idx
    ON trader_order_events (order_id, created_at ASC);
