UPDATE polymarket_account_credentials
SET signature_type = 1, updated_at = now()
WHERE wallet_type = 'deposit' AND signature_type = 3;
