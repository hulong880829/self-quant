ALTER TABLE trading_accounts
    ADD COLUMN IF NOT EXISTS spot_maker_fee_rate NUMERIC(20, 12),
    ADD COLUMN IF NOT EXISTS spot_taker_fee_rate NUMERIC(20, 12),
    ADD COLUMN IF NOT EXISTS contract_maker_fee_rate NUMERIC(20, 12),
    ADD COLUMN IF NOT EXISTS contract_taker_fee_rate NUMERIC(20, 12),
    ADD COLUMN IF NOT EXISTS spot_fee_status TEXT,
    ADD COLUMN IF NOT EXISTS contract_fee_status TEXT,
    ADD COLUMN IF NOT EXISTS fee_source TEXT,
    ADD COLUMN IF NOT EXISTS fee_updated_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS fee_sync_status TEXT,
    ADD COLUMN IF NOT EXISTS fee_sync_error TEXT,
    ADD COLUMN IF NOT EXISTS fee_markets JSONB;

ALTER TABLE trading_accounts
    DROP CONSTRAINT IF EXISTS trading_accounts_spot_fee_status_chk;
ALTER TABLE trading_accounts
    ADD CONSTRAINT trading_accounts_spot_fee_status_chk CHECK (
        spot_fee_status IS NULL OR spot_fee_status IN ('unknown', 'ok', 'unsupported')
    );

ALTER TABLE trading_accounts
    DROP CONSTRAINT IF EXISTS trading_accounts_contract_fee_status_chk;
ALTER TABLE trading_accounts
    ADD CONSTRAINT trading_accounts_contract_fee_status_chk CHECK (
        contract_fee_status IS NULL OR contract_fee_status IN ('unknown', 'ok', 'unsupported')
    );

ALTER TABLE trading_accounts
    DROP CONSTRAINT IF EXISTS trading_accounts_fee_sync_status_chk;
ALTER TABLE trading_accounts
    ADD CONSTRAINT trading_accounts_fee_sync_status_chk CHECK (
        fee_sync_status IS NULL OR fee_sync_status IN (
            'pending', 'ok', 'retrying', 'failed', 'credential_invalid'
        )
    );
