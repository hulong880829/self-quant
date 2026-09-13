ALTER TABLE trading_accounts DROP CONSTRAINT IF EXISTS trading_accounts_exchange_chk;
ALTER TABLE trading_accounts ADD CONSTRAINT trading_accounts_exchange_chk CHECK (
    exchange IN (
        'binance',
        'okx',
        'bybit',
        'bitget',
        'gate',
        'hyperliquid',
        'aster',
        'lighter',
        'polymarket'
    )
);
