UPDATE trader_twap_jobs
SET next_action_at = COALESCE(closed_at, updated_at, now())
WHERE status IN ('completed', 'partially_completed', 'canceled', 'failed')
  AND next_action_at = 'infinity'::timestamptz;

ALTER TABLE trader_twap_jobs
    ADD COLUMN IF NOT EXISTS current_attempt INTEGER NOT NULL DEFAULT 0;

ALTER TABLE trader_orders
    ADD COLUMN IF NOT EXISTS twap_attempt_index INTEGER NOT NULL DEFAULT 0;

DROP INDEX IF EXISTS trader_orders_twap_slice_uq;

CREATE UNIQUE INDEX IF NOT EXISTS trader_orders_twap_attempt_uq
    ON trader_orders (twap_job_id, twap_slice_index, twap_attempt_index)
    WHERE twap_job_id IS NOT NULL;

UPDATE trader_orders child
SET status = 'canceled',
    updated_at = now(),
    next_reconcile_at = 'infinity'::timestamptz
FROM trader_twap_jobs job
WHERE child.twap_job_id = job.id
  AND child.status IN ('pending', 'open', 'partially_filled', 'unknown')
  AND child.id IS DISTINCT FROM job.active_order_id;

CREATE UNIQUE INDEX IF NOT EXISTS trader_orders_twap_active_uq
    ON trader_orders (twap_job_id)
    WHERE twap_job_id IS NOT NULL
      AND status IN ('pending', 'open', 'partially_filled', 'unknown');
