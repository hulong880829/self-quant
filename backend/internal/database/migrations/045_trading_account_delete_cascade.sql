ALTER TABLE trader_orders
    DROP CONSTRAINT IF EXISTS trader_orders_trading_account_id_fkey;
ALTER TABLE trader_orders
    ADD CONSTRAINT trader_orders_trading_account_id_fkey
    FOREIGN KEY (trading_account_id) REFERENCES trading_accounts(id) ON DELETE CASCADE;

ALTER TABLE trader_order_fills
    DROP CONSTRAINT IF EXISTS trader_order_fills_trading_account_id_fkey;
ALTER TABLE trader_order_fills
    ADD CONSTRAINT trader_order_fills_trading_account_id_fkey
    FOREIGN KEY (trading_account_id) REFERENCES trading_accounts(id) ON DELETE CASCADE;

ALTER TABLE trader_twap_jobs
    DROP CONSTRAINT IF EXISTS trader_twap_jobs_trading_account_id_fkey;
ALTER TABLE trader_twap_jobs
    ADD CONSTRAINT trader_twap_jobs_trading_account_id_fkey
    FOREIGN KEY (trading_account_id) REFERENCES trading_accounts(id) ON DELETE CASCADE;

ALTER TABLE trader_arbitrage_combinations
    DROP CONSTRAINT IF EXISTS trader_arbitrage_combinations_leg_a_trading_account_id_fkey;
ALTER TABLE trader_arbitrage_combinations
    ADD CONSTRAINT trader_arbitrage_combinations_leg_a_trading_account_id_fkey
    FOREIGN KEY (leg_a_trading_account_id) REFERENCES trading_accounts(id) ON DELETE CASCADE;

ALTER TABLE trader_arbitrage_combinations
    DROP CONSTRAINT IF EXISTS trader_arbitrage_combinations_leg_b_trading_account_id_fkey;
ALTER TABLE trader_arbitrage_combinations
    ADD CONSTRAINT trader_arbitrage_combinations_leg_b_trading_account_id_fkey
    FOREIGN KEY (leg_b_trading_account_id) REFERENCES trading_accounts(id) ON DELETE CASCADE;
