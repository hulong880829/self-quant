ALTER TABLE trader_arbitrage_combinations
    ADD COLUMN IF NOT EXISTS leg_a_base_position NUMERIC(38, 18) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS leg_b_base_position NUMERIC(38, 18) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS carry_base_quantity NUMERIC(38, 18)
        GENERATED ALWAYS AS (leg_a_base_position + leg_b_base_position) STORED,
    ADD COLUMN IF NOT EXISTS runtime_state TEXT NOT NULL DEFAULT 'monitoring';

ALTER TABLE trader_arbitrage_combinations
    DROP CONSTRAINT IF EXISTS trader_arbitrage_runtime_state_check,
    ADD CONSTRAINT trader_arbitrage_runtime_state_check
        CHECK (runtime_state IN (
            'monitoring','maker_open','maker_canceling','repricing',
            'opportunity_gone','hedging','reconciling','backoff'
        ));

WITH positions AS (
    SELECT
        e.combination_id,
        COALESCE(SUM(
            CASE
                WHEN o.arbitrage_leg = 'a' AND o.side = 'buy' THEN o.filled_quantity
                WHEN o.arbitrage_leg = 'a' AND o.side = 'sell' THEN -o.filled_quantity
                ELSE 0
            END
        ), 0) AS leg_a_position,
        COALESCE(SUM(
            CASE
                WHEN o.arbitrage_leg = 'b' AND o.side = 'buy' THEN o.filled_quantity
                WHEN o.arbitrage_leg = 'b' AND o.side = 'sell' THEN -o.filled_quantity
                ELSE 0
            END
        ), 0) AS leg_b_position
    FROM trader_orders o
    JOIN trader_arbitrage_executions e ON e.id = o.arbitrage_execution_id
    WHERE o.filled_quantity > 0
    GROUP BY e.combination_id
)
UPDATE trader_arbitrage_combinations c
SET
    leg_a_base_position = positions.leg_a_position,
    leg_b_base_position = positions.leg_b_position
FROM positions
WHERE c.id = positions.combination_id;

WITH latest_marks AS (
    SELECT DISTINCT ON (combination_id)
        combination_id,
        (
            trigger_leg_a_bid + trigger_leg_a_ask
            + trigger_leg_b_bid + trigger_leg_b_ask
        ) / 4 AS mark
    FROM trader_arbitrage_executions
    WHERE trigger_leg_a_bid > 0
      AND trigger_leg_a_ask > 0
      AND trigger_leg_b_bid > 0
      AND trigger_leg_b_ask > 0
    ORDER BY combination_id, created_at DESC
)
UPDATE trader_arbitrage_combinations c
SET position_notional = GREATEST(
    -c.target_notional,
    LEAST(
        c.target_notional,
        CASE
            WHEN c.leg_a_base_position > 0 AND c.leg_b_base_position < 0
                THEN LEAST(c.leg_a_base_position, -c.leg_b_base_position) * latest_marks.mark
            WHEN c.leg_a_base_position < 0 AND c.leg_b_base_position > 0
                THEN -LEAST(-c.leg_a_base_position, c.leg_b_base_position) * latest_marks.mark
            ELSE 0
        END
    )
)
FROM latest_marks
WHERE c.id = latest_marks.combination_id;
