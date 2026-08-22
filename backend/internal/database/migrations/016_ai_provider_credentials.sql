CREATE TABLE IF NOT EXISTS ai_provider_credentials (
    id BIGSERIAL PRIMARY KEY,
    owner_username TEXT NOT NULL REFERENCES accounts(username) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    api_key_enc BYTEA NOT NULL,
    api_key_masked TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'unknown'
        CHECK (status IN ('unknown', 'valid', 'invalid')),
    last_error TEXT,
    last_tested_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner_username, provider)
);
