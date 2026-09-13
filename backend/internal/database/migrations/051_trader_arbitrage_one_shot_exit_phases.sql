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
                AND one_shot_phase IN (
                    'building_target',
                    'waiting_exit',
                    'exiting',
                    'exited'
                )
                AND (
                    (
                        exit_policy = 'annualized'
                        AND exit_annualized_rate IS NOT NULL
                        AND exit_after_seconds IS NULL
                    )
                    OR (
                        exit_policy = 'time'
                        AND exit_after_seconds IN (
                            3600,
                            14400,
                            28800,
                            86400,
                            604800
                        )
                        AND exit_annualized_rate IS NULL
                    )
                )
            )
        );
