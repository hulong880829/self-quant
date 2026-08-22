-- Remove Binance-sourced price points written before Chainlink RTDS fix.
DELETE FROM polymarket_price_points;

ALTER TABLE polymarket_price_points
    ADD COLUMN IF NOT EXISTS price_source TEXT NOT NULL DEFAULT 'chainlink';

ALTER TABLE polymarket_price_points
    DROP CONSTRAINT IF EXISTS polymarket_price_points_source_chk;

ALTER TABLE polymarket_price_points
    ADD CONSTRAINT polymarket_price_points_source_chk CHECK (
        price_source IN ('chainlink')
    );
