ALTER TABLE polymarket_orders
    ADD COLUMN IF NOT EXISTS execution_type TEXT NOT NULL DEFAULT 'book',
    ADD COLUMN IF NOT EXISTS limit_price NUMERIC(20,10),
    ADD COLUMN IF NOT EXISTS clob_order_type TEXT NOT NULL DEFAULT 'FAK',
    ADD COLUMN IF NOT EXISTS error_message TEXT NOT NULL DEFAULT '';

ALTER TABLE polymarket_orders
    DROP CONSTRAINT IF EXISTS polymarket_orders_execution_type_chk;

ALTER TABLE polymarket_orders
    ADD CONSTRAINT polymarket_orders_execution_type_chk CHECK (
        execution_type IN ('book', 'limit')
    );

ALTER TABLE polymarket_orders
    DROP CONSTRAINT IF EXISTS polymarket_orders_clob_order_type_chk;

ALTER TABLE polymarket_orders
    ADD CONSTRAINT polymarket_orders_clob_order_type_chk CHECK (
        clob_order_type IN ('FAK', 'GTC')
    );

UPDATE polymarket_account_credentials
SET signature_type = 3, updated_at = now()
WHERE wallet_type = 'deposit' AND signature_type <> 3;
