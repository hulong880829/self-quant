ALTER TABLE trader_arbitrage_combinations
    DROP CONSTRAINT IF EXISTS trader_arbitrage_combinations_ask_threshold_bps_check,
    DROP CONSTRAINT IF EXISTS trader_arbitrage_combinations_bid_threshold_bps_check;
