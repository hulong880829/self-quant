CREATE TABLE IF NOT EXISTS products (
    id BIGSERIAL PRIMARY KEY,
    owner_username TEXT NOT NULL REFERENCES accounts(username) ON DELETE CASCADE,
    name TEXT NOT NULL,
    display_name TEXT NOT NULL,
    category TEXT NOT NULL DEFAULT '',
    strategy TEXT NOT NULL DEFAULT '',
    base_currency TEXT NOT NULL DEFAULT 'USD',
    timezone TEXT NOT NULL DEFAULT 'Asia/Shanghai',
    active BOOLEAN NOT NULL DEFAULT TRUE,
    inception_date DATE NOT NULL DEFAULT CURRENT_DATE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT products_owner_name_uq UNIQUE (owner_username, name),
    CONSTRAINT products_name_chk CHECK (length(btrim(name)) > 0)
);

INSERT INTO products (owner_username, name, display_name)
SELECT DISTINCT owner_username, product_name, product_name
FROM trading_accounts
ON CONFLICT (owner_username, name) DO NOTHING;

ALTER TABLE trading_accounts ADD COLUMN IF NOT EXISTS product_id BIGINT;

UPDATE trading_accounts account
SET product_id = product.id
FROM products product
WHERE product.owner_username = account.owner_username
  AND product.name = account.product_name
  AND account.product_id IS NULL;

CREATE OR REPLACE FUNCTION assign_trading_account_product()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    INSERT INTO products (owner_username, name, display_name)
    VALUES (NEW.owner_username, NEW.product_name, NEW.product_name)
    ON CONFLICT (owner_username, name) DO UPDATE SET updated_at = now()
    RETURNING id INTO NEW.product_id;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trading_accounts_assign_product ON trading_accounts;
CREATE TRIGGER trading_accounts_assign_product
BEFORE INSERT OR UPDATE OF owner_username, product_name ON trading_accounts
FOR EACH ROW EXECUTE FUNCTION assign_trading_account_product();

ALTER TABLE trading_accounts ALTER COLUMN product_id SET NOT NULL;
ALTER TABLE trading_accounts
    DROP CONSTRAINT IF EXISTS trading_accounts_product_id_fkey;
ALTER TABLE trading_accounts
    ADD CONSTRAINT trading_accounts_product_id_fkey
    FOREIGN KEY (product_id) REFERENCES products(id) ON DELETE RESTRICT;
CREATE INDEX IF NOT EXISTS trading_accounts_product_idx
    ON trading_accounts (product_id);

CREATE TABLE IF NOT EXISTS account_equity_samples (
    id BIGSERIAL PRIMARY KEY,
    product_id BIGINT NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    trading_account_id BIGINT NOT NULL REFERENCES trading_accounts(id) ON DELETE CASCADE,
    equity_usd NUMERIC(38, 18) NOT NULL,
    available_funds_usd NUMERIC(38, 18),
    source_updated_at TIMESTAMPTZ NOT NULL,
    sampled_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT account_equity_samples_value_chk CHECK (equity_usd >= 0),
    CONSTRAINT account_equity_samples_account_time_uq
        UNIQUE (trading_account_id, sampled_at)
);
CREATE INDEX IF NOT EXISTS account_equity_samples_product_time_idx
    ON account_equity_samples (product_id, sampled_at DESC);

CREATE TABLE IF NOT EXISTS product_cash_flows (
    id BIGSERIAL PRIMARY KEY,
    product_id BIGINT NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    flow_date DATE NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    amount_usd NUMERIC(38, 18) NOT NULL,
    flow_type TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT 'manual',
    confirmed BOOLEAN NOT NULL DEFAULT FALSE,
    confirmed_by TEXT REFERENCES accounts(username) ON DELETE SET NULL,
    confirmed_at TIMESTAMPTZ,
    created_by TEXT NOT NULL REFERENCES accounts(username) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT product_cash_flows_type_chk
        CHECK (flow_type IN (
            'subscription', 'redemption', 'deposit', 'withdrawal',
            'transfer', 'adjustment'
        )),
    CONSTRAINT product_cash_flows_source_chk CHECK (source = 'manual'),
    CONSTRAINT product_cash_flows_confirmation_chk CHECK (
        (confirmed AND confirmed_by IS NOT NULL AND confirmed_at IS NOT NULL)
        OR (NOT confirmed AND confirmed_by IS NULL AND confirmed_at IS NULL)
    )
);
CREATE INDEX IF NOT EXISTS product_cash_flows_product_date_idx
    ON product_cash_flows (product_id, flow_date DESC, id DESC);

CREATE TABLE IF NOT EXISTS product_daily_snapshots (
    id BIGSERIAL PRIMARY KEY,
    product_id BIGINT NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    report_date DATE NOT NULL,
    opening_equity_usd NUMERIC(38, 18) NOT NULL,
    closing_equity_usd NUMERIC(38, 18) NOT NULL,
    net_cash_flow_usd NUMERIC(38, 18) NOT NULL DEFAULT 0,
    pnl_usd NUMERIC(38, 18) NOT NULL,
    return_rate NUMERIC(38, 18),
    sample_count INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'final',
    finalized_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT product_daily_snapshots_product_date_uq UNIQUE (product_id, report_date),
    CONSTRAINT product_daily_snapshots_status_chk CHECK (status IN ('final', 'partial')),
    CONSTRAINT product_daily_snapshots_sample_count_chk CHECK (sample_count >= 0)
);
CREATE INDEX IF NOT EXISTS product_daily_snapshots_product_date_idx
    ON product_daily_snapshots (product_id, report_date DESC);

CREATE TABLE IF NOT EXISTS account_trade_fills (
    id BIGSERIAL PRIMARY KEY,
    trading_account_id BIGINT NOT NULL REFERENCES trading_accounts(id) ON DELETE CASCADE,
    exchange TEXT NOT NULL,
    exchange_fill_id TEXT NOT NULL,
    exchange_order_id TEXT,
    symbol TEXT NOT NULL,
    side TEXT NOT NULL,
    quantity NUMERIC(38, 18) NOT NULL,
    price NUMERIC(38, 18) NOT NULL,
    fee NUMERIC(38, 18),
    fee_currency TEXT,
    realized_pnl_usd NUMERIC(38, 18),
    filled_at TIMESTAMPTZ NOT NULL,
    raw JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT account_trade_fills_side_chk CHECK (side IN ('buy', 'sell')),
    CONSTRAINT account_trade_fills_quantity_chk CHECK (quantity > 0),
    CONSTRAINT account_trade_fills_price_chk CHECK (price >= 0),
    CONSTRAINT account_trade_fills_exchange_fill_uq
        UNIQUE (trading_account_id, exchange_fill_id)
);
CREATE INDEX IF NOT EXISTS account_trade_fills_account_time_idx
    ON account_trade_fills (trading_account_id, filled_at DESC);

CREATE TABLE IF NOT EXISTS trade_sync_cursors (
    trading_account_id BIGINT PRIMARY KEY REFERENCES trading_accounts(id) ON DELETE CASCADE,
    cursor_value TEXT NOT NULL DEFAULT '',
    last_fill_at TIMESTAMPTZ,
    last_success_at TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS trade_sync_job_runs (
    id BIGSERIAL PRIMARY KEY,
    trading_account_id BIGINT REFERENCES trading_accounts(id) ON DELETE SET NULL,
    job_kind TEXT NOT NULL,
    status TEXT NOT NULL,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    records_written INTEGER NOT NULL DEFAULT 0,
    error_message TEXT NOT NULL DEFAULT '',
    CONSTRAINT trade_sync_job_runs_status_chk
        CHECK (status IN ('running', 'succeeded', 'failed')),
    CONSTRAINT trade_sync_job_runs_records_chk CHECK (records_written >= 0)
);
CREATE INDEX IF NOT EXISTS trade_sync_job_runs_started_idx
    ON trade_sync_job_runs (started_at DESC);
