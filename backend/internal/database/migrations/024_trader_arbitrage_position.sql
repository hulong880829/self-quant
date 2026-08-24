ALTER TABLE trader_arbitrage_combinations
    RENAME COLUMN completed_notional TO cumulative_turnover_notional;

ALTER TABLE trader_arbitrage_combinations
    ADD COLUMN IF NOT EXISTS position_notional NUMERIC NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS consecutive_failures INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS next_retry_at TIMESTAMPTZ NOT NULL DEFAULT '-infinity',
    ADD COLUMN IF NOT EXISTS position_uncertain BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE trader_arbitrage_combinations
    DROP CONSTRAINT IF EXISTS trader_arbitrage_position_bounds_check;

ALTER TABLE trader_arbitrage_combinations
    ADD CONSTRAINT trader_arbitrage_position_bounds_check
    CHECK (position_notional >= -target_notional AND position_notional <= target_notional);

ALTER TABLE trader_arbitrage_executions
    ADD COLUMN IF NOT EXISTS requested_notional NUMERIC NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS position_effect TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS reduce_only BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE trader_arbitrage_executions
    DROP CONSTRAINT IF EXISTS trader_arbitrage_executions_position_effect_check;

ALTER TABLE trader_arbitrage_executions
    ADD CONSTRAINT trader_arbitrage_executions_position_effect_check
    CHECK (position_effect IN ('', 'open', 'close'));

DROP INDEX IF EXISTS trader_arbitrage_one_active_direction_idx;

CREATE UNIQUE INDEX IF NOT EXISTS trader_arbitrage_one_active_execution_idx
    ON trader_arbitrage_executions(combination_id)
    WHERE status NOT IN ('completed', 'failed', 'canceled', 'dry_run');

ALTER TABLE trader_orders
    ADD COLUMN IF NOT EXISTS reduce_only BOOLEAN NOT NULL DEFAULT FALSE;

WITH marks AS (
    SELECT
        e.combination_id,
        e.direction,
        e.status,
        e.leg_a_filled_quantity,
        e.leg_b_filled_quantity,
        CASE e.direction
            WHEN 'ask' THEN (e.trigger_leg_a_ask + e.trigger_leg_b_ask) / 2
            ELSE (e.trigger_leg_a_bid + e.trigger_leg_b_bid) / 2
        END AS mark
    FROM trader_arbitrage_executions e
),
completed AS (
    SELECT
        combination_id,
        SUM(
            CASE WHEN direction = 'ask' THEN 1 ELSE -1 END
            * LEAST(leg_a_filled_quantity, leg_b_filled_quantity)
            * mark
        ) AS reconstructed_position,
        SUM(LEAST(leg_a_filled_quantity, leg_b_filled_quantity) * mark) AS reconstructed_turnover
    FROM marks
    WHERE status = 'completed'
    GROUP BY combination_id
)
UPDATE trader_arbitrage_combinations c
SET
    position_notional = GREATEST(
        -c.target_notional,
        LEAST(c.target_notional, COALESCE(completed.reconstructed_position, 0))
    ),
    cumulative_turnover_notional = COALESCE(completed.reconstructed_turnover, 0)
FROM completed
WHERE c.id = completed.combination_id;

UPDATE trader_arbitrage_combinations c
SET
    position_uncertain = TRUE,
    error_message = CASE
        WHEN c.error_message = '' THEN 'position reconstruction is uncertain; new executions blocked'
        ELSE c.error_message
    END
WHERE EXISTS (
    SELECT 1
    FROM trader_arbitrage_executions e
    WHERE e.combination_id = c.id
      AND (
          e.status NOT IN ('completed', 'failed', 'canceled', 'dry_run')
          OR (
              e.status IN ('failed', 'canceled')
              AND (e.leg_a_filled_quantity > 0 OR e.leg_b_filled_quantity > 0)
          )
      )
)
OR EXISTS (
    SELECT 1
    FROM trader_arbitrage_executions e
    WHERE e.combination_id = c.id
      AND e.status = 'completed'
    GROUP BY e.combination_id
    HAVING ABS(SUM(
        CASE WHEN e.direction = 'ask' THEN 1 ELSE -1 END
        * LEAST(e.leg_a_filled_quantity, e.leg_b_filled_quantity)
        * CASE e.direction
            WHEN 'ask' THEN (e.trigger_leg_a_ask + e.trigger_leg_b_ask) / 2
            ELSE (e.trigger_leg_a_bid + e.trigger_leg_b_bid) / 2
        END
    )) > c.target_notional
);
