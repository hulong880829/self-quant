ALTER TABLE trader_arbitrage_combinations
    ADD COLUMN IF NOT EXISTS leg_a_reconciliation_adjustment NUMERIC(38, 18) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS leg_b_reconciliation_adjustment NUMERIC(38, 18) NOT NULL DEFAULT 0;
