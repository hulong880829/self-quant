ALTER TABLE trader_arbitrage_combinations
    ADD COLUMN IF NOT EXISTS leg_a_liquidation_distance_pct NUMERIC,
    ADD COLUMN IF NOT EXISTS leg_b_liquidation_distance_pct NUMERIC;
