# MDS Aggregate Recording Format

`mds_shm_consumer --record` samples complete aggregate images at a configurable
interval of at least 200 ms. It records `aggbbo`, `aggorderbook`, or both.
Raw order-book deltas are intentionally not sampled because dropping deltas
would make reconstruction invalid.

## Layout and retention

Files are UTC hourly shards:

```text
<output>/<YYYY-MM-DD>/<HH>/<profile>/<symbol>/<stream>.sqrec.zst
```

Each shard is written to a PID-suffixed temporary path, finalized with a
container trailer and CRCs, fsynced, then atomically renamed. The container and
every frame are versioned little-endian data. The zstd stream contains:

- a container header with `SQRC` magic and format version;
- framed records with `SQRF` magic, payload length and CRC;
- a trailer with `SQRE` magic, record count and CRC.

AggBbo entries preserve the aggregate image plus raw/gated minimum and maximum
cross BPS observed during the sampling window. AggOrderBook entries contain
only valid levels up to the configured depth (maximum 50 per side), encoded
field-by-field without C++ padding. Every entry includes wall/monotonic sample
time, ring epoch/sequence, topic generation, and reset/gap flags.
Sampling may persist the same last-good aggregate image more than once while
the upstream topic is not publishing. Replay and research consumers that need
the true update frequency must deduplicate unchanged entries by ring epoch,
ring sequence, and topic generation rather than treating each sample as a new
market-data update.

The recorder removes only finalized shards older than the configured retention,
which cannot exceed 24 hours. It never removes the current temporary shard.
Low disk space, compression failure, or write failure marks recording failed
without blocking shared-memory consumption or the WebSocket Gateway.

`manifest.v1.json` is atomically replaced and contains recorder state, current
active topics, sampling/depth/retention settings, and finalized shard paths.
After an unclean shutdown, stale temporary shards are discarded and the
manifest is reconciled before new recording starts.

## Reading and validation

Applications can use `mds/record/record_reader.h`. The reader validates zstd
completion, container/frame/trailer CRCs, versions, counts, aggregate metadata,
price ordering, quantities, and venue attribution. Reconstructed records are
semantically equivalent over their effective counts; unused fixed-array tails
and ABI padding are not part of the format.

The installed tool provides validation and JSON Lines export:

```bash
mds_record_tool --validate shard.sqrec.zst
mds_record_tool --export-jsonl shard.sqrec.zst
```
