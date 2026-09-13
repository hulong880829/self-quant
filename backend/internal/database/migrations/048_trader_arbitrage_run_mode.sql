-- Deploy gate: non-terminal executions must already have a frozen requested_notional.
-- Backfill once from the combination order_notional if needed; never re-randomize.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM trader_arbitrage_executions
        WHERE status NOT IN ('completed', 'failed', 'canceled', 'dry_run')
          AND (requested_notional IS NULL OR requested_notional <= 0)
    ) THEN
        RAISE EXCEPTION
            'non-terminal trader_arbitrage_executions have empty requested_notional; wait for them to finish or backfill from combination.order_notional before applying 048';
    END IF;
END
$$;

DO $$
DECLARE
    constraint_name TEXT;
BEGIN
    FOR constraint_name IN
        SELECT con.conname
        FROM pg_constraint con
        JOIN pg_class rel ON rel.oid = con.conrelid
        JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
        WHERE rel.relname = 'trader_arbitrage_combinations'
          AND nsp.nspname = current_schema()
          AND con.contype = 'c'
          AND pg_get_constraintdef(con.oid) LIKE '%order_notional%'
    LOOP
        EXECUTE format(
            'ALTER TABLE trader_arbitrage_combinations DROP CONSTRAINT %I',
            constraint_name
        );
    END LOOP;
END
$$;

ALTER TABLE trader_arbitrage_combinations
    ALTER COLUMN order_notional DROP NOT NULL;

ALTER TABLE trader_arbitrage_combinations
    ADD COLUMN IF NOT EXISTS run_mode TEXT NOT NULL DEFAULT 'spread',
    ADD COLUMN IF NOT EXISTS entry_direction TEXT,
    ADD COLUMN IF NOT EXISTS leg_a_leverage NUMERIC,
    ADD COLUMN IF NOT EXISTS leg_b_leverage NUMERIC,
    ADD COLUMN IF NOT EXISTS exit_policy TEXT,
    ADD COLUMN IF NOT EXISTS exit_annualized_rate NUMERIC,
    ADD COLUMN IF NOT EXISTS exit_after_seconds INTEGER,
    ADD COLUMN IF NOT EXISTS target_reached_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS scheduled_exit_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS one_shot_phase TEXT;

UPDATE trader_arbitrage_combinations
SET run_mode = 'spread'
WHERE run_mode IS NULL OR run_mode = '';

ALTER TABLE trader_arbitrage_combinations
    DROP CONSTRAINT IF EXISTS trader_arbitrage_run_mode_chk,
    ADD CONSTRAINT trader_arbitrage_run_mode_chk
        CHECK (run_mode IN ('spread', 'one_shot'));

ALTER TABLE trader_arbitrage_combinations
    DROP CONSTRAINT IF EXISTS trader_arbitrage_run_mode_fields_chk,
    ADD CONSTRAINT trader_arbitrage_run_mode_fields_chk
        CHECK (
            (
                run_mode = 'spread'
                AND entry_direction IS NULL
                AND exit_policy IS NULL
                AND exit_annualized_rate IS NULL
                AND exit_after_seconds IS NULL
                AND one_shot_phase IS NULL
            )
            OR (
                run_mode = 'one_shot'
                AND entry_direction IN ('ask', 'bid')
                AND exit_policy IN ('annualized', 'time')
                AND one_shot_phase IN ('building_target', 'waiting_exit')
                AND (
                    (
                        exit_policy = 'annualized'
                        AND exit_annualized_rate IS NOT NULL
                        AND exit_after_seconds IS NULL
                    )
                    OR (
                        exit_policy = 'time'
                        AND exit_after_seconds IN (3600, 14400, 28800, 86400)
                        AND exit_annualized_rate IS NULL
                    )
                )
            )
        );

ALTER TABLE trader_arbitrage_combinations
    DROP CONSTRAINT IF EXISTS trader_arbitrage_leg_a_leverage_chk,
    ADD CONSTRAINT trader_arbitrage_leg_a_leverage_chk
        CHECK (
            leg_a_leverage IS NULL
            OR (
                (leg_a_contract_type = 'spot' AND leg_a_leverage = 1)
                OR (leg_a_contract_type = 'perpetual' AND leg_a_leverage >= 1)
            )
        );

ALTER TABLE trader_arbitrage_combinations
    DROP CONSTRAINT IF EXISTS trader_arbitrage_leg_b_leverage_chk,
    ADD CONSTRAINT trader_arbitrage_leg_b_leverage_chk
        CHECK (
            leg_b_leverage IS NULL
            OR (
                (leg_b_contract_type = 'spot' AND leg_b_leverage = 1)
                OR (leg_b_contract_type = 'perpetual' AND leg_b_leverage >= 1)
            )
        );
