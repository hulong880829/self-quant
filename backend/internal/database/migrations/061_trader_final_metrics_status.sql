ALTER TABLE trader_arbitrage_combinations
    ADD COLUMN IF NOT EXISTS final_metrics_status TEXT NOT NULL DEFAULT 'pending',
    ADD COLUMN IF NOT EXISTS final_metrics_retry_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS final_metrics_last_error TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS final_metrics_next_retry_at TIMESTAMPTZ NOT NULL DEFAULT '-infinity',
    ADD COLUMN IF NOT EXISTS final_metrics_completed_at TIMESTAMPTZ;

ALTER TABLE trader_arbitrage_combinations
    DROP CONSTRAINT IF EXISTS trader_arbitrage_final_metrics_status_chk;
ALTER TABLE trader_arbitrage_combinations
    ADD CONSTRAINT trader_arbitrage_final_metrics_status_chk
    CHECK (final_metrics_status IN ('pending', 'partial', 'completed'));

CREATE INDEX IF NOT EXISTS trader_arbitrage_final_metrics_due_idx
    ON trader_arbitrage_combinations (final_metrics_next_retry_at, id)
    WHERE final_metrics_status IN ('pending', 'partial')
      AND final_metrics_next_retry_at < 'infinity'::timestamptz
      AND (
        status = 'closing'
        OR status = 'closed'
        OR (
          status = 'running'
          AND run_mode = 'one_shot'
          AND one_shot_phase = 'exited'
        )
      );

-- All SET expressions read the pre-update row, so failure keys stay visible
-- while this statement also clears them.
UPDATE trader_arbitrage_combinations c
SET
    error_message = CASE
        WHEN (
            c.status = 'closing'
            OR (c.run_mode = 'one_shot' AND c.one_shot_phase = 'exiting')
        ) AND c.last_failure_key IN ('final_metrics_not_ready', 'final_metrics_persist_failed')
        THEN ''
        ELSE c.error_message
    END,
    consecutive_failures = CASE
        WHEN (
            c.status = 'closing'
            OR (c.run_mode = 'one_shot' AND c.one_shot_phase = 'exiting')
        ) AND c.last_failure_key IN ('final_metrics_not_ready', 'final_metrics_persist_failed')
        THEN 0
        ELSE c.consecutive_failures
    END,
    next_retry_at = CASE
        WHEN (
            c.status = 'closing'
            OR (c.run_mode = 'one_shot' AND c.one_shot_phase = 'exiting')
        ) AND c.last_failure_key IN ('final_metrics_not_ready', 'final_metrics_persist_failed')
        THEN '-infinity'::timestamptz
        ELSE c.next_retry_at
    END,
    last_failure_key = CASE
        WHEN (
            c.status = 'closing'
            OR (c.run_mode = 'one_shot' AND c.one_shot_phase = 'exiting')
        ) AND c.last_failure_key IN ('final_metrics_not_ready', 'final_metrics_persist_failed')
        THEN ''
        ELSE c.last_failure_key
    END,
    final_metrics_retry_count = 0,
    final_metrics_last_error = '',
    final_metrics_status = CASE
        WHEN c.status = 'closing'
            AND c.last_failure_key IN ('final_metrics_not_ready', 'final_metrics_persist_failed')
            THEN 'pending'
        WHEN c.run_mode = 'one_shot'
            AND c.one_shot_phase = 'exiting'
            AND c.last_failure_key IN ('final_metrics_not_ready', 'final_metrics_persist_failed')
            THEN 'pending'
        WHEN EXISTS (
            SELECT 1 FROM trader_arbitrage_events e
            WHERE e.combination_id = c.id
              AND e.event_type = 'one_shot_exit_completed'
        ) AND NOT (
            c.status = 'closing'
            AND c.last_failure_key IN ('final_metrics_not_ready', 'final_metrics_persist_failed')
        ) AND NOT (
            c.run_mode = 'one_shot'
            AND c.one_shot_phase = 'exiting'
            AND c.last_failure_key IN ('final_metrics_not_ready', 'final_metrics_persist_failed')
        ) THEN 'completed'
        WHEN (
            EXISTS (
                SELECT 1 FROM trader_arbitrage_events e
                WHERE e.combination_id = c.id
                  AND e.event_type = 'closed_with_dust'
            ) OR EXISTS (
                SELECT 1 FROM trader_arbitrage_events e
                WHERE e.combination_id = c.id
                  AND e.event_type = 'one_shot_exited_with_dust'
            )
        ) AND NOT EXISTS (
            SELECT 1 FROM trader_arbitrage_events e
            WHERE e.combination_id = c.id
              AND e.event_type = 'one_shot_exit_completed'
        ) THEN 'partial'
        WHEN c.status = 'closed'
            OR (c.run_mode = 'one_shot' AND c.one_shot_phase = 'exited')
            THEN 'pending'
        ELSE 'pending'
    END,
    final_metrics_next_retry_at = CASE
        WHEN (
            c.status = 'closing'
            OR (c.run_mode = 'one_shot' AND c.one_shot_phase = 'exiting')
        ) AND c.last_failure_key IN ('final_metrics_not_ready', 'final_metrics_persist_failed')
            THEN now()
        WHEN EXISTS (
            SELECT 1 FROM trader_arbitrage_events e
            WHERE e.combination_id = c.id
              AND e.event_type = 'one_shot_exit_completed'
        ) AND NOT (
            (
                c.status = 'closing'
                OR (c.run_mode = 'one_shot' AND c.one_shot_phase = 'exiting')
            ) AND c.last_failure_key IN ('final_metrics_not_ready', 'final_metrics_persist_failed')
        ) THEN 'infinity'::timestamptz
        WHEN c.status = 'closed'
            OR (c.run_mode = 'one_shot' AND c.one_shot_phase = 'exited')
            OR EXISTS (
                SELECT 1 FROM trader_arbitrage_events e
                WHERE e.combination_id = c.id
                  AND e.event_type IN ('closed_with_dust', 'one_shot_exited_with_dust')
            )
            THEN 'infinity'::timestamptz
        ELSE '-infinity'::timestamptz
    END,
    final_metrics_completed_at = CASE
        WHEN EXISTS (
            SELECT 1 FROM trader_arbitrage_events e
            WHERE e.combination_id = c.id
              AND e.event_type = 'one_shot_exit_completed'
        ) AND NOT (
            (
                c.status = 'closing'
                OR (c.run_mode = 'one_shot' AND c.one_shot_phase = 'exiting')
            ) AND c.last_failure_key IN ('final_metrics_not_ready', 'final_metrics_persist_failed')
        ) THEN COALESCE(c.metrics_calculated_at, now())
        ELSE NULL
    END;
