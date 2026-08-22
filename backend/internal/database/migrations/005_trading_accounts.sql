CREATE TABLE IF NOT EXISTS trading_accounts (
    id BIGSERIAL PRIMARY KEY,
    owner_username TEXT NOT NULL REFERENCES accounts(username) ON DELETE CASCADE,
    product_name TEXT NOT NULL,
    exchange TEXT NOT NULL,
    account_name TEXT NOT NULL,
    api_key_enc BYTEA NOT NULL,
    api_secret_enc BYTEA NOT NULL,
    passphrase_enc BYTEA,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT trading_accounts_exchange_chk CHECK (
        exchange IN ('binance', 'okx', 'bybit', 'bitget', 'gate', 'hyperliquid')
    ),
    CONSTRAINT trading_accounts_owner_product_exchange_name_uq
        UNIQUE (owner_username, product_name, exchange, account_name)
);

CREATE INDEX IF NOT EXISTS trading_accounts_owner_idx
    ON trading_accounts (owner_username);
