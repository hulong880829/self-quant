ALTER TABLE trader_arbitrage_combinations
    ADD COLUMN IF NOT EXISTS gross_turnover_notional NUMERIC(38, 18) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS leg_a_average_entry_price NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS leg_b_average_entry_price NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS average_entry_spread_bps NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS leg_a_unrealized_pnl NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS leg_b_unrealized_pnl NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS realized_spread_pnl NUMERIC(38, 18) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS estimated_funding_pnl NUMERIC(38, 18) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS notional_exposure_seconds NUMERIC(38, 18) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS holding_seconds NUMERIC(38, 18) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS exposure_updated_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS combined_position_annualized NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS funding_history_complete BOOLEAN NOT NULL DEFAULT TRUE;
