CREATE INDEX IF NOT EXISTS trader_arbitrage_execution_history_idx
    ON trader_arbitrage_executions (combination_id, created_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS trader_arbitrage_event_history_idx
    ON trader_arbitrage_events (combination_id, created_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS trader_order_arbitrage_history_idx
    ON trader_orders (arbitrage_execution_id, created_at DESC, id DESC)
    WHERE arbitrage_execution_id IS NOT NULL;
