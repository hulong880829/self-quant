ALTER TABLE trader_arbitrage_executions
    ADD COLUMN IF NOT EXISTS last_close_clip BOOLEAN NOT NULL DEFAULT FALSE;
