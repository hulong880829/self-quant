ALTER TABLE account_trade_fills
    ADD COLUMN IF NOT EXISTS quote_notional_usd NUMERIC(38, 18);

UPDATE account_trade_fills
SET quote_notional_usd = abs(quantity * price)
WHERE quote_notional_usd IS NULL;

ALTER TABLE product_daily_snapshots
    ADD COLUMN IF NOT EXISTS volume_24h_usd NUMERIC(38, 18);
