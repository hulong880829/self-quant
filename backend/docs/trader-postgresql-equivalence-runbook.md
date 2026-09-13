# Trader PostgreSQL performance and correctness runbook

Use this runbook to compare the fixed Trader SQL implementation before and
after database-path changes. The service no longer has legacy, shadow, or
equivalent runtime modes; the previously validated equivalent statements are
the only implementation.

## Stable operation names

Keep these SQL comment tags unchanged. They join source code, JSON plans, and
`pg_stat_statements` samples:

- `/* trader:funding_estimate_candidates */`
- `/* trader:funding_estimate_upsert */`
- `/* trader:lease_due_orders_equivalent */`
- `/* trader:lease_due_orders_bulk_update */`
- `/* trader:arbitrage_replay_orders */`
- `/* trader:arbitrage_replay_fills */`

## Preconditions

Use an operator-managed `psql` connection. Never write database URLs,
credentials, order payloads, or account identifiers into collected artifacts.
Record the UTC window, deployment revision, PostgreSQL version, and sample
name.

Check whether statement statistics are available:

```sql
SELECT extversion
FROM pg_extension
WHERE extname = 'pg_stat_statements';
```

An empty result means SQL-level call and latency baselines cannot be collected
from `pg_stat_statements`; use application metrics or enable the extension in a
separate operational change.

Confirm trader connection identity:

```sql
SELECT application_name, state, count(*) AS connections
FROM pg_stat_activity
WHERE datname = current_database()
  AND application_name = 'selfquant-trader-service'
GROUP BY application_name, state
ORDER BY state;
```

## Statement baseline

Capture this read-only query at the beginning and end of equal-duration
windows. Compare counter deltas by `queryid`, not cumulative totals:

```sql
SELECT
  CASE
    WHEN query LIKE '%/* trader:funding_estimate_candidates */%'
      THEN 'trader:funding_estimate_candidates'
    WHEN query LIKE '%/* trader:funding_estimate_upsert */%'
      THEN 'trader:funding_estimate_upsert'
    WHEN query LIKE '%/* trader:lease_due_orders_equivalent */%'
      THEN 'trader:lease_due_orders_equivalent'
    WHEN query LIKE '%/* trader:lease_due_orders_bulk_update */%'
      THEN 'trader:lease_due_orders_bulk_update'
    WHEN query LIKE '%/* trader:arbitrage_replay_orders */%'
      THEN 'trader:arbitrage_replay_orders'
    WHEN query LIKE '%/* trader:arbitrage_replay_fills */%'
      THEN 'trader:arbitrage_replay_fills'
  END AS operation_name,
  queryid,
  calls,
  round(total_exec_time::numeric, 3) AS total_exec_ms,
  round(mean_exec_time::numeric, 3) AS mean_exec_ms,
  rows,
  round(rows::numeric / NULLIF(calls, 0), 3) AS rows_per_call,
  shared_blks_hit,
  shared_blks_read,
  shared_blks_dirtied,
  shared_blks_written,
  temp_blks_read,
  temp_blks_written,
  blk_read_time,
  blk_write_time
FROM pg_stat_statements
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND (
    query LIKE '%/* trader:funding_estimate_candidates */%'
    OR query LIKE '%/* trader:funding_estimate_upsert */%'
    OR query LIKE '%/* trader:lease_due_orders_equivalent */%'
    OR query LIKE '%/* trader:lease_due_orders_bulk_update */%'
    OR query LIKE '%/* trader:arbitrage_replay_orders */%'
    OR query LIKE '%/* trader:arbitrage_replay_fills */%'
  )
ORDER BY operation_name, queryid;
```

`pg_stat_statements.rows` is returned-or-affected rows, not scanned rows.
Obtain actual and filtered rows from JSON plans.

## Replay workload size

Count fills through the real relationship to combinations:

```sql
WITH fill_counts AS (
  SELECT e.combination_id, count(*) AS fills
  FROM trader_order_fills f
  JOIN trader_orders o ON o.id = f.order_id
  JOIN trader_arbitrage_executions e ON e.id = o.arbitrage_execution_id
  GROUP BY e.combination_id
)
SELECT
  count(*) AS combinations,
  percentile_cont(0.5) WITHIN GROUP (ORDER BY fills) AS median_fills,
  percentile_cont(0.99) WITHIN GROUP (ORDER BY fills) AS p99_fills,
  max(fills) AS max_fills
FROM fill_counts;
```

SQL statement time excludes row scanning, decimal parsing, map construction,
sorting, and replay calculations. Evaluate it together with the application's
`arbitrage_replay_*` counters or a pprof profile. Use P99 fill count, total
duration, share of Trader CPU, and growth trend together when deciding whether
checkpointing is justified.

## Read-path write audit

`RefreshTwapProgress` has six call sites: one `GetTwap` read path, one
order-result path, two scheduler paths, and two close/cancel paths. Runtime
frequency must be grouped by those caller classes before changing its
`updated_at` behavior. This optimization deliberately leaves all six calls and
the unconditional progress update unchanged.

`GetOrder` also performs live venue reconciliation and may persist the result,
append events, and indirectly refresh a TWAP. This is intentional read-through
reconciliation rather than a hidden polling-list write. The audit found no
write in `ListTwaps`, `ListOrders`, `GetArbitrageCombination`, or
`ListArbitrageCombinations`.

## JSON plan collection

Use a fixed anonymized sample in a non-production schema. For each tagged
candidate or replay `SELECT`:

```sql
BEGIN;
SET LOCAL statement_timeout = '30s';
EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
<exact tagged SELECT with fixed sample values>;
ROLLBACK;
```

Collect three warm-cache runs. Record planning/execution time, actual rows and
loops, rows removed by filter, buffer and temporary-block activity, node and
index names, sort method, and peak sort space. Before/after runs must use the
same data snapshot, bind values, PostgreSQL settings, and cardinality.

## Correctness and deployment gates

Before deploying SQL-path changes:

- funding estimates must preserve every persisted business field and suppress
  physical writes for unchanged inputs;
- incomplete or late funding/fill data must remain repairable;
- `LeaseDueOrders` must preserve all eligibility branches and ordering;
- selected and leased row counts must match;
- concurrent workers must not duplicate or miss eligible orders;
- no new timeout, deadlock, lock-timeout, or database error class may appear.

Deploy changes separately and compare equal-duration windows. Stop rollout when
correctness differs or when statement latency, scanned rows, temporary blocks,
lock waits, PostgreSQL CPU, or Trader CPU regress in two consecutive windows.

## Timestamp semantics

`trader_arbitrage_funding_estimates.updated_at` records a physical business
result change and remains unchanged for a guarded no-op.

`trader_arbitrage_combinations.updated_at` participates in listing and is also
written by the one-second market snapshot path. The optimized snapshot
`RETURNING` remains narrow but does not change this timestamp behavior.

`trader_twap_jobs.updated_at` remains unchanged by this optimization project:
`RefreshTwapProgress` still performs its existing update so active-list sorting
and cursor behavior are preserved.
