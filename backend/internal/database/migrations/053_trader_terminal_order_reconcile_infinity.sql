UPDATE trader_orders
SET next_reconcile_at = 'infinity'::timestamptz,
    reconcile_lease_until = NULL
WHERE arbitrage_execution_id IS NOT NULL
  AND status IN ('filled','canceled','rejected','expired')
  AND next_reconcile_at < 'infinity'::timestamptz;
