ALTER TABLE trading_accounts DROP CONSTRAINT IF EXISTS trading_accounts_exchange_chk;
ALTER TABLE trading_accounts ADD CONSTRAINT trading_accounts_exchange_chk CHECK (
    exchange IN ('binance', 'okx', 'bybit', 'bitget', 'gate', 'hyperliquid', 'polymarket')
);

CREATE TABLE IF NOT EXISTS polymarket_account_credentials (
    trading_account_id BIGINT PRIMARY KEY REFERENCES trading_accounts(id) ON DELETE CASCADE,
    signer_address TEXT NOT NULL,
    funder_address TEXT NOT NULL,
    wallet_type TEXT NOT NULL,
    signature_type SMALLINT NOT NULL,
    credential_nonce BIGINT NOT NULL DEFAULT 0,
    private_key_enc BYTEA NOT NULL,
    api_key_enc BYTEA NOT NULL,
    api_secret_enc BYTEA NOT NULL,
    passphrase_enc BYTEA NOT NULL,
    binding_status TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT polymarket_wallet_type_chk CHECK (wallet_type IN ('eoa', 'deposit')),
    CONSTRAINT polymarket_binding_status_chk CHECK (binding_status IN ('active', 'invalid', 'revoked'))
);

CREATE INDEX IF NOT EXISTS polymarket_credentials_signer_idx
    ON polymarket_account_credentials (signer_address);

CREATE TABLE IF NOT EXISTS polymarket_markets (
    id TEXT PRIMARY KEY,
    condition_id TEXT NOT NULL UNIQUE,
    slug TEXT NOT NULL UNIQUE,
    asset TEXT NOT NULL,
    period TEXT NOT NULL,
    title TEXT NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    window_end TIMESTAMPTZ NOT NULL,
    up_token_id TEXT NOT NULL,
    down_token_id TEXT NOT NULL,
    tick_size NUMERIC(20,10) NOT NULL DEFAULT 0.01,
    negative_risk BOOLEAN NOT NULL DEFAULT false,
    active BOOLEAN NOT NULL DEFAULT true,
    source_updated_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS polymarket_markets_selector_idx
    ON polymarket_markets (asset, period, active, window_start DESC);

CREATE TABLE IF NOT EXISTS polymarket_price_points (
    market_id TEXT NOT NULL REFERENCES polymarket_markets(id) ON DELETE CASCADE,
    observed_at TIMESTAMPTZ NOT NULL,
    open_price NUMERIC(30,12),
    chainlink_price NUMERIC(30,12),
    PRIMARY KEY (market_id, observed_at)
);

CREATE INDEX IF NOT EXISTS polymarket_price_points_recent_idx
    ON polymarket_price_points (market_id, observed_at DESC);

CREATE TABLE IF NOT EXISTS polymarket_positions (
    trading_account_id BIGINT NOT NULL REFERENCES trading_accounts(id) ON DELETE CASCADE,
    token_id TEXT NOT NULL,
    condition_id TEXT NOT NULL,
    market_title TEXT NOT NULL,
    outcome TEXT NOT NULL,
    size NUMERIC(30,12) NOT NULL DEFAULT 0,
    average_price NUMERIC(20,10) NOT NULL DEFAULT 0,
    current_price NUMERIC(20,10) NOT NULL DEFAULT 0,
    initial_value NUMERIC(30,12) NOT NULL DEFAULT 0,
    current_value NUMERIC(30,12) NOT NULL DEFAULT 0,
    cash_pnl NUMERIC(30,12) NOT NULL DEFAULT 0,
    percent_pnl NUMERIC(20,10) NOT NULL DEFAULT 0,
    redeemable BOOLEAN NOT NULL DEFAULT false,
    source_updated_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (trading_account_id, token_id)
);

CREATE INDEX IF NOT EXISTS polymarket_positions_account_value_idx
    ON polymarket_positions (trading_account_id, current_value DESC);

CREATE TABLE IF NOT EXISTS polymarket_account_snapshots (
    trading_account_id BIGINT PRIMARY KEY REFERENCES trading_accounts(id) ON DELETE CASCADE,
    available_balance NUMERIC(30,12) NOT NULL DEFAULT 0,
    position_value NUMERIC(30,12) NOT NULL DEFAULT 0,
    total_assets NUMERIC(30,12) NOT NULL DEFAULT 0,
    source_updated_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS polymarket_orders (
    id UUID PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    trading_account_id BIGINT NOT NULL REFERENCES trading_accounts(id) ON DELETE CASCADE,
    market_id TEXT NOT NULL REFERENCES polymarket_markets(id),
    token_id TEXT NOT NULL,
    outcome TEXT NOT NULL,
    side TEXT NOT NULL,
    requested_amount NUMERIC(30,12) NOT NULL,
    amount_unit TEXT NOT NULL,
    clob_order_id TEXT,
    filled_size NUMERIC(30,12) NOT NULL DEFAULT 0,
    average_price NUMERIC(20,10) NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    error_code TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT polymarket_order_side_chk CHECK (side IN ('buy', 'sell')),
    CONSTRAINT polymarket_order_outcome_chk CHECK (outcome IN ('up', 'down')),
    CONSTRAINT polymarket_order_unit_chk CHECK (amount_unit IN ('usd', 'shares'))
);

CREATE INDEX IF NOT EXISTS polymarket_orders_account_created_idx
    ON polymarket_orders (trading_account_id, created_at DESC);

CREATE INDEX IF NOT EXISTS polymarket_orders_pending_idx
    ON polymarket_orders (status, updated_at)
    WHERE status IN ('pending', 'open', 'partially_filled');
