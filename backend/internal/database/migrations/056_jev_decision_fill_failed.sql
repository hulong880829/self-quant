ALTER TABLE jev_arb_decisions
    DROP CONSTRAINT IF EXISTS jev_arb_decisions_execution_status_check;

ALTER TABLE jev_arb_decisions
    ADD CONSTRAINT jev_arb_decisions_execution_status_check
    CHECK (
        execution_status IN (
            'not_applicable',
            'pending',
            'filled',
            'expired',
            'portfolio_rejected',
            'fill_failed'
        )
    );
