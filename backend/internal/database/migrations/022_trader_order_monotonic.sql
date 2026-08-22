ALTER TABLE trader_orders
    ADD COLUMN IF NOT EXISTS last_stream_event_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_venue_event_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS trader_order_fills (
    id BIGSERIAL PRIMARY KEY,
    order_id UUID NOT NULL REFERENCES trader_orders(id) ON DELETE CASCADE,
    trading_account_id BIGINT NOT NULL REFERENCES trading_accounts(id) ON DELETE RESTRICT,
    exchange TEXT NOT NULL,
    venue_order_id TEXT NOT NULL,
    trade_id TEXT NOT NULL,
    quantity NUMERIC(38, 18) NOT NULL CHECK (quantity > 0),
    price NUMERIC(38, 18) NOT NULL CHECK (price >= 0),
    executed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT trader_order_fills_venue_trade_uq
        UNIQUE (trading_account_id, exchange, venue_order_id, trade_id)
);

CREATE INDEX IF NOT EXISTS trader_order_fills_order_idx
    ON trader_order_fills(order_id, created_at, id);

ALTER TABLE trader_arbitrage_executions
    ADD COLUMN IF NOT EXISTS hedge_sequence BIGINT NOT NULL DEFAULT 0;
