ALTER TABLE trader_arbitrage_combinations
    ADD COLUMN IF NOT EXISTS leg_a_venue_base_position NUMERIC(38,18),
    ADD COLUMN IF NOT EXISTS leg_b_venue_base_position NUMERIC(38,18),
    ADD COLUMN IF NOT EXISTS leg_a_position_difference NUMERIC(38,18),
    ADD COLUMN IF NOT EXISTS leg_b_position_difference NUMERIC(38,18),
    ADD COLUMN IF NOT EXISTS last_position_reconciled_at TIMESTAMPTZ;

