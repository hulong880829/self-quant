ALTER TABLE trader_arbitrage_combinations
    ADD COLUMN IF NOT EXISTS leg_a_venue_baseline_base_position NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS leg_b_venue_baseline_base_position NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS venue_baseline_captured_at TIMESTAMPTZ;

ALTER TABLE trader_arbitrage_combinations
    DROP CONSTRAINT IF EXISTS trader_arbitrage_position_baselines_all_or_none;

ALTER TABLE trader_arbitrage_combinations
    ADD CONSTRAINT trader_arbitrage_position_baselines_all_or_none CHECK (
        (
            leg_a_venue_baseline_base_position IS NULL
            AND leg_b_venue_baseline_base_position IS NULL
            AND venue_baseline_captured_at IS NULL
        )
        OR
        (
            leg_a_venue_baseline_base_position IS NOT NULL
            AND leg_b_venue_baseline_base_position IS NOT NULL
            AND venue_baseline_captured_at IS NOT NULL
        )
    );

CREATE INDEX IF NOT EXISTS trader_arbitrage_active_leg_a_owner_idx
    ON trader_arbitrage_combinations(
        owner_username, leg_a_trading_account_id, leg_a_instrument_id
    )
    WHERE status IN ('running', 'closing');

CREATE INDEX IF NOT EXISTS trader_arbitrage_active_leg_b_owner_idx
    ON trader_arbitrage_combinations(
        owner_username, leg_b_trading_account_id, leg_b_instrument_id
    )
    WHERE status IN ('running', 'closing');
