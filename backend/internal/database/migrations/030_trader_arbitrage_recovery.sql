ALTER TABLE trader_arbitrage_combinations
    ADD COLUMN IF NOT EXISTS last_failure_key TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS repeated_failure_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS circuit_open BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE trader_arbitrage_combinations
    DROP CONSTRAINT IF EXISTS trader_arbitrage_runtime_state_check,
    ADD CONSTRAINT trader_arbitrage_runtime_state_check
        CHECK (runtime_state IN (
            'monitoring','maker_open','maker_canceling','repricing',
            'opportunity_gone','hedging','hedge_deferred_dust',
            'reconciling','backoff','position_uncertain',
            'manual_intervention','closing'
        ));

ALTER TABLE trader_arbitrage_combinations
    DROP CONSTRAINT IF EXISTS trader_arbitrage_repeated_failure_count_check,
    ADD CONSTRAINT trader_arbitrage_repeated_failure_count_check
        CHECK (repeated_failure_count >= 0);
