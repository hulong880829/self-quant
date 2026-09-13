DO $$
DECLARE
    constraint_name TEXT;
BEGIN
    FOR constraint_name IN
        SELECT conname
        FROM pg_constraint
        WHERE conrelid = 'trader_arbitrage_combinations'::regclass
          AND contype = 'c'
          AND (
              pg_get_constraintdef(oid) LIKE '%leg_a_trading_account_id <> leg_b_trading_account_id%'
              OR pg_get_constraintdef(oid) LIKE '%leg_a_exchange <> leg_b_exchange%'
          )
    LOOP
        EXECUTE format(
            'ALTER TABLE trader_arbitrage_combinations DROP CONSTRAINT %I',
            constraint_name
        );
    END LOOP;
END
$$;
