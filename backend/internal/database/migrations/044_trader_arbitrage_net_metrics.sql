ALTER TABLE trader_arbitrage_combinations
    ADD COLUMN IF NOT EXISTS estimated_trading_fee NUMERIC(38, 18),
    ADD COLUMN IF NOT EXISTS metrics_calculated_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS metrics_quality TEXT;

ALTER TABLE trader_arbitrage_combinations
    DROP CONSTRAINT IF EXISTS trader_arbitrage_metrics_quality_chk;
ALTER TABLE trader_arbitrage_combinations
    ADD CONSTRAINT trader_arbitrage_metrics_quality_chk CHECK (
        metrics_quality IS NULL
        OR metrics_quality IN ('complete', 'estimated', 'partial')
    );

-- Derived per-settlement estimates are no longer written. Clear stale rows after
-- the old SyncArbitrageFundingEstimates worker has been stopped.
DELETE FROM trader_arbitrage_funding_estimates;
