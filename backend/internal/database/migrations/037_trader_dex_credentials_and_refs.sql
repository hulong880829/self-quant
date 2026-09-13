ALTER TABLE trading_accounts
    ADD COLUMN IF NOT EXISTS credential_kind TEXT,
    ADD COLUMN IF NOT EXISTS trading_api_key_enc BYTEA,
    ADD COLUMN IF NOT EXISTS trading_api_secret_enc BYTEA,
    ADD COLUMN IF NOT EXISTS signing_address TEXT,
    ADD COLUMN IF NOT EXISTS vault_address TEXT,
    ADD COLUMN IF NOT EXISTS account_index BIGINT,
    ADD COLUMN IF NOT EXISTS api_key_index SMALLINT;

ALTER TABLE trading_accounts DROP CONSTRAINT IF EXISTS trading_accounts_credential_kind_chk;
ALTER TABLE trading_accounts ADD CONSTRAINT trading_accounts_credential_kind_chk CHECK (
    credential_kind IS NULL OR credential_kind IN ('aster_hmac', 'hyperliquid_agent', 'lighter_api')
);

ALTER TABLE trading_accounts DROP CONSTRAINT IF EXISTS trading_accounts_api_key_index_chk;
ALTER TABLE trading_accounts ADD CONSTRAINT trading_accounts_api_key_index_chk CHECK (
    api_key_index IS NULL OR api_key_index BETWEEN 0 AND 255
);

CREATE TABLE IF NOT EXISTS trader_order_venue_refs (
    order_id UUID PRIMARY KEY REFERENCES trader_orders(id) ON DELETE CASCADE,
    venue_client_order_id TEXT,
    venue_order_id TEXT,
    cloid TEXT,
    tx_hash TEXT,
    nonce BIGINT,
    account_index BIGINT,
    api_key_index SMALLINT,
    client_order_index BIGINT,
    last_venue_event_at TIMESTAMPTZ,
    reconcile_status TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS trader_order_venue_refs_cloid_uq
    ON trader_order_venue_refs (cloid) WHERE cloid IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS trader_order_venue_refs_account_client_uq
    ON trader_order_venue_refs (account_index, api_key_index, client_order_index)
    WHERE account_index IS NOT NULL AND api_key_index IS NOT NULL AND client_order_index IS NOT NULL;
CREATE INDEX IF NOT EXISTS trader_order_venue_refs_tx_hash_idx
    ON trader_order_venue_refs (tx_hash) WHERE tx_hash IS NOT NULL;
