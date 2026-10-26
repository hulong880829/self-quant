ALTER TABLE trader_order_fills
    ADD COLUMN fee_amount numeric,
    ADD COLUMN fee_currency text,
    ADD COLUMN liquidity_role text;

ALTER TABLE trader_order_fills
    ADD CONSTRAINT trader_order_fills_liquidity_role_check
    CHECK (liquidity_role IS NULL OR liquidity_role IN ('maker', 'taker', 'unknown'));
