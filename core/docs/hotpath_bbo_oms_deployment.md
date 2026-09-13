# Hot-path BBO/OMS deployment gate

The BBO origin change keeps the SQMD schema number and record sizes unchanged,
but old decoders reject the newly non-zero BBO/Ticker flag bits. Mixed-version
SHM readers are therefore unsupported.

## Build gate

Build every item below from the same commit:

- `mds_producer`
- `mds_aggregator`
- `mds_shm_consumer`
- `mds_record_tool`
- StrategyFrame and every linked strategy, including `poly-mm`
- installed-package smoke consumers (`mds_package_consumer` and
  `mds_record_package_consumer`)

The `mds_shm_consumer` binary is deployed in three roles. Inventory and replace
all `--gateway`, `--record`, and `--clickhouse-bbo` instances even though only
the ClickHouse role attaches directly to venue ticker rings.

## Restart gate

This is a stop-the-world upgrade, not a rolling upgrade:

1. Stop every StrategyFrame/strategy process and every SHM consumer.
2. Stop `mds_aggregator`.
3. Stop `mds_producer`.
4. Replace all binaries and the installed SDK.
5. Start producers.
6. Start the aggregator and wait for aggregate segments to become live.
7. Start all consumer roles and strategies.

Do not start a producer that emits origin-tagged BBO/Ticker records while an
old reader remains attached.

## Post-start checks

- BBO/Ticker codec rejection counters are zero.
- ClickHouse recorder `decode_failed` is zero and row rate, symbols, prices,
  quantities, and sampling cadence match the pre-upgrade baseline.
- `bbo_origin_missing` and strict-policy `bbo_origin_unknown` are zero.
- AggBbo/AggOrderBook origin-mask bits are zero; aggregate TTL, flags, publish
  counts, and values match baseline.
- `.sqrec` aggregate records match the deterministic baseline fields; only
  sample wall/monotonic clock values may differ.
- StrategyFrame reports no policy-starved configured instrument.

