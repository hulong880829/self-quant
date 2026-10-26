CREATE TABLE IF NOT EXISTS trading_account_bind_idempotency (
    owner_username TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    trading_account_id BIGINT REFERENCES trading_accounts(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    PRIMARY KEY (owner_username, idempotency_key),
    CONSTRAINT trading_account_bind_idempotency_key_len
        CHECK (char_length(idempotency_key) BETWEEN 1 AND 128)
);
