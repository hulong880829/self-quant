ALTER TABLE trading_accounts DROP CONSTRAINT IF EXISTS trading_accounts_credential_kind_chk;
ALTER TABLE trading_accounts ADD CONSTRAINT trading_accounts_credential_kind_chk CHECK (
    credential_kind IS NULL OR credential_kind IN (
        'aster_hmac',
        'aster_api_wallet',
        'hyperliquid_agent',
        'hyperliquid_api_wallet',
        'lighter_api',
        'lighter_api_wallet'
    )
);

UPDATE trading_accounts
SET credential_kind = CASE exchange
    WHEN 'hyperliquid' THEN 'hyperliquid_api_wallet'
    WHEN 'aster' THEN 'aster_api_wallet'
    WHEN 'lighter' THEN 'lighter_api_wallet'
END
WHERE exchange IN ('hyperliquid', 'aster', 'lighter')
  AND NULLIF(TRIM(credential_kind), '') IS NULL;
