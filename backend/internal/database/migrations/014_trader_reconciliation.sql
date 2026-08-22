ALTER TABLE trader_orders
    ADD COLUMN IF NOT EXISTS base_asset TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS quote_asset TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_reconciled_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS next_reconcile_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN IF NOT EXISTS reconcile_failures INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS reconcile_lease_until TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS trader_orders_reconcile_due_idx
    ON trader_orders (next_reconcile_at, updated_at)
    WHERE status IN ('pending', 'open', 'partially_filled', 'unknown');
