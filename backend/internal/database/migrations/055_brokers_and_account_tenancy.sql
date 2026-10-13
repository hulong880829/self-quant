CREATE TABLE IF NOT EXISTS brokers (
    broker_id TEXT PRIMARY KEY,
    name TEXT NOT NULL DEFAULT '',
    client_secret_hash TEXT NOT NULL,
    client_secret_masked TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    token_version BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT brokers_status_chk CHECK (status IN ('active', 'disabled'))
);

ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS broker_id TEXT REFERENCES brokers(broker_id) ON DELETE RESTRICT,
    ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'active',
    ADD COLUMN IF NOT EXISTS created_source TEXT NOT NULL DEFAULT 'selfquant',
    ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS auth_version BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'accounts_status_chk'
    ) THEN
        ALTER TABLE accounts
            ADD CONSTRAINT accounts_status_chk
            CHECK (status IN ('active', 'disabled', 'archived'));
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS accounts_broker_status_idx
    ON accounts (broker_id, status);
CREATE INDEX IF NOT EXISTS accounts_status_idx
    ON accounts (status);

CREATE TABLE IF NOT EXISTS admin_audit_events (
    id BIGSERIAL PRIMARY KEY,
    actor_username TEXT NOT NULL DEFAULT '',
    actor_id TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS admin_audit_events_created_idx
    ON admin_audit_events (created_at DESC);
CREATE INDEX IF NOT EXISTS admin_audit_events_resource_idx
    ON admin_audit_events (resource_type, resource_id, created_at DESC);

CREATE TABLE IF NOT EXISTS trading_account_equity_snapshots (
    trading_account_id BIGINT PRIMARY KEY
        REFERENCES trading_accounts(id) ON DELETE CASCADE,
    venue_identity TEXT NOT NULL,
    equity_usd TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'missing',
    observed_at TIMESTAMPTZ,
    last_success_at TIMESTAMPTZ,
    error_code TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS trading_account_equity_snapshots_venue_idx
    ON trading_account_equity_snapshots (venue_identity);
