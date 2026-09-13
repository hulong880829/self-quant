CREATE OR REPLACE FUNCTION report_period_date(occurred_at timestamptz)
RETURNS date
LANGUAGE sql
STABLE
AS $$
    SELECT CASE
        WHEN (occurred_at AT TIME ZONE 'Asia/Shanghai')::time < TIME '09:00:00'
            THEN (occurred_at AT TIME ZONE 'Asia/Shanghai')::date
        ELSE ((occurred_at AT TIME ZONE 'Asia/Shanghai')::date + 1)
    END
$$;

ALTER TABLE product_cash_flows
    ADD COLUMN IF NOT EXISTS period_rule_version INTEGER NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS idempotency_key TEXT;

UPDATE product_cash_flows
SET flow_date = report_period_date(occurred_at);

UPDATE product_cash_flows
SET idempotency_key = 'legacy:' || id::text
WHERE idempotency_key IS NULL OR btrim(idempotency_key) = '';

ALTER TABLE product_cash_flows
    ALTER COLUMN idempotency_key SET NOT NULL;

ALTER TABLE product_cash_flows
    DROP CONSTRAINT IF EXISTS product_cash_flows_idempotency_uq;
ALTER TABLE product_cash_flows
    ADD CONSTRAINT product_cash_flows_idempotency_uq UNIQUE (product_id, idempotency_key);

ALTER TABLE product_cash_flows
    DROP CONSTRAINT IF EXISTS product_cash_flows_amount_sign_chk;
ALTER TABLE product_cash_flows
    ADD CONSTRAINT product_cash_flows_amount_sign_chk CHECK (
        (flow_type IN ('subscription', 'deposit') AND amount_usd > 0)
        OR (flow_type IN ('redemption', 'withdrawal') AND amount_usd < 0)
        OR (flow_type IN ('transfer', 'adjustment') AND amount_usd <> 0)
    );

CREATE OR REPLACE FUNCTION product_cash_flows_assign_report_date()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.flow_date := report_period_date(NEW.occurred_at);
    IF NEW.period_rule_version IS NULL OR NEW.period_rule_version < 1 THEN
        NEW.period_rule_version := 1;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS product_cash_flows_assign_report_date ON product_cash_flows;
CREATE TRIGGER product_cash_flows_assign_report_date
BEFORE INSERT OR UPDATE OF occurred_at, flow_date, period_rule_version
ON product_cash_flows
FOR EACH ROW EXECUTE FUNCTION product_cash_flows_assign_report_date();

CREATE INDEX IF NOT EXISTS product_cash_flows_product_occurred_idx
    ON product_cash_flows (product_id, occurred_at);

ALTER TABLE product_cash_flows
    DROP CONSTRAINT IF EXISTS product_cash_flows_flow_date_chk;
ALTER TABLE product_cash_flows
    ADD CONSTRAINT product_cash_flows_flow_date_chk
    CHECK (flow_date = report_period_date(occurred_at));

ALTER TABLE product_daily_snapshots
    ADD COLUMN IF NOT EXISTS subscription_usd NUMERIC(38, 18) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS redemption_usd NUMERIC(38, 18) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS cash_flow_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS period_rule_version INTEGER NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS period_start TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS period_end TIMESTAMPTZ;

ALTER TABLE product_daily_snapshots
    DROP CONSTRAINT IF EXISTS product_daily_snapshots_status_chk;
ALTER TABLE product_daily_snapshots
    ADD CONSTRAINT product_daily_snapshots_status_chk
    CHECK (status IN ('final', 'partial', 'recomputing', 'invalid'));
ALTER TABLE product_daily_snapshots
    DROP CONSTRAINT IF EXISTS product_daily_snapshots_cash_flow_count_chk;
ALTER TABLE product_daily_snapshots
    ADD CONSTRAINT product_daily_snapshots_cash_flow_count_chk
    CHECK (cash_flow_count >= 0);

CREATE TABLE IF NOT EXISTS product_report_recompute_jobs (
    id BIGSERIAL PRIMARY KEY,
    product_id BIGINT NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    report_date DATE NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    before_pnl_usd NUMERIC(38, 18),
    before_return_rate NUMERIC(38, 18),
    after_pnl_usd NUMERIC(38, 18),
    after_return_rate NUMERIC(38, 18),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    needs_rerun BOOLEAN NOT NULL DEFAULT FALSE,
    CONSTRAINT product_report_recompute_jobs_product_date_uq UNIQUE (product_id, report_date),
    CONSTRAINT product_report_recompute_jobs_status_chk
        CHECK (status IN ('pending', 'running', 'done', 'failed')),
    CONSTRAINT product_report_recompute_jobs_attempts_chk CHECK (attempts >= 0)
);
CREATE INDEX IF NOT EXISTS product_report_recompute_jobs_status_idx
    ON product_report_recompute_jobs (status, created_at);

INSERT INTO product_report_recompute_jobs (product_id, report_date, status)
SELECT product_id, report_date, 'pending'
FROM product_daily_snapshots
ON CONFLICT (product_id, report_date) DO NOTHING;

UPDATE product_daily_snapshots
SET status = 'recomputing'
WHERE status IN ('final', 'partial');
