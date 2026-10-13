# IBKR MDS deployment

The IBKR producer is built on the host against TWS API 10.45. It reads
candidate contracts from PostgreSQL or YAML, probes configured metrics once at
startup, and continuously publishes only the selected BBO subscriptions. When
PostgreSQL supplies candidates, the producer writes complete STK 90-day
turnover estimates back to the matching instruments before applying the
subscription limit. A write failure is logged but does not block BBO startup.

## New Ubuntu 24.04 host

```bash
./scripts/bootstrap.sh
./scripts/bootstrap-ibkr-deps.sh --accept-ib-license
./scripts/build-ibkr-mds.sh
```

The dependency bootstrap caches files below `core/.deps/ibkr`, which is
gitignored. It is idempotent and validates:

- TWS source version `10.45.01` and the pinned original-source fingerprint;
- a libbid build containing `__bid64_to_string` and
  `__bid64_from_string`;
- a minimal link against the selected decimal library.

The libbid source order is Ubuntu `libintelrdfpmath-dev`, pinned netlib U5,
then the BSD-3 source in `third_party/intel-rdfp`. Force one path when
diagnosing a machine:

```bash
./scripts/bootstrap-ibkr-deps.sh --accept-ib-license \
  --libbid-source vendored --force
```

IBKR no longer keeps the fixed 10.45 archive at its former public URL. The
script tries that official URL but never downloads TWS from a third-party
mirror. If IB returns 404, use an official archive or an existing authorized
installation:

```bash
./scripts/bootstrap-ibkr-deps.sh --accept-ib-license \
  --tws-archive /path/to/twsapi_macunix.1045.01.zip

# Existing extracted official installation:
./scripts/bootstrap-ibkr-deps.sh --accept-ib-license \
  --tws-source /path/to/IBJts
```

`--tws-source` imports and verifies the source into the same cache. CMake
generates all TWS protobuf C++ files in the build directory; it does not modify
the imported API tree.

## Configuration validation

Load the backend environment before validating a config that uses PostgreSQL:

```bash
set -a
source backend/.env
set +a
core/build-release/mds/mds_ibkr_producer \
  --config core/mds/config/mds_ibkr.yaml --validate-only
```

`postgres.url_env` and a non-empty YAML `subscriptions` list are mutually
exclusive. Candidate loading remains inside `BEGIN READ ONLY`; the producer
does not run migrations. After metric probing, a separate short transaction
updates `instruments.turnover_usd_90d` and `ib_metrics_observed_at` by
`ib_con_id`. Apply migration `066_instrument_ib_turnover.sql` before deploying
the new producer.
`ibkr.max_subscriptions` is the only subscription cap. Candidate lists larger
than the cap still load; filtering and ranking happen before truncation.

`selection` accepts only optional `min_average_volume_90d` (shares/day) and
`min_market_cap_usd` (USD). An omitted threshold is not applied. Only `STK`
contracts are filtered. A stock missing a configured metric is excluded.
`CASH` and `CMDTY` bypass both thresholds, are ordered by `con_id`, and take
subscription slots before stocks. Remaining slots go to stocks ordered by
90-day average volume, with `con_id` breaking ties. Results above
`max_subscriptions` are truncated and startup continues.

Metric probes are cancelled before persistent subscriptions begin. Five stock
canaries test generic tick 258 when a market-cap threshold is configured. Any
`MKTCAP` keeps the threshold, so later stocks missing market cap are excluded.
If none of the canaries returns `MKTCAP`, startup ignores
`min_market_cap_usd`, logs a high-visibility warning, and continues with the
volume threshold and ranking.

The turnover estimate is
`average_volume_90d_shares * reference_price_usd`. The producer reuses standard
price ticks from the existing metric probe: close or delayed close is preferred,
with last or delayed last as fallback. A stock probe waits for average volume
and a reference price until the existing five-second deadline. Missing price
does not affect selection or startup and does not overwrite an older turnover
snapshot. The database stores only the estimated average daily USD turnover and
its observation time; volume, price, and price source remain runtime diagnostics.

## Optional live metric smoke test

Only perform this against an already authorized Gateway. Use one liquid STK
and confirm:

- generic tick 165 produces tick 21 (90-day average volume);
- generic tick 258 may produce tick 47 with `MKTCAP`.

Tick 258 is deprecated and may return nothing even with TWS API 10.45. Confirm
whether a liquid stock returns `MKTCAP` before relying on
`min_market_cap_usd`. A smoke test must not start the ClickHouse recorder or
change the live subscription set.

## Process contract

The producer publishes to:

```text
/selfquant.mds.bbo.ibkr.equity.ticker.shard0.*
```

`core/mds/config/mds_clickhouse_bbo.example.yaml` includes the corresponding
`ibkr/equity/shard0` selector. Run exactly one ClickHouse BBO consumer for
crypto and IBKR together. `restart_recoder.sh` is the only restart path: it
stops the unified consumer and producers, removes the exact IBKR ring under
`/dev/shm`, then starts producers and one consumer. Ring removal also runs
after a crash or `kill -9`, because `unlink_on_shutdown` only covers a normal
producer exit. `restart_ibkr_producer.sh` delegates to that full restart and
does not replace the IBKR ring while the consumer remains attached.

Do not run either restart script as part of dependency bootstrap, build, or
validation. A live cutover is a separate operational action. The systemd unit
is not an independent restart path.

The unified restart waits up to 300 seconds for IBKR health by default because
five concurrent probes can take roughly 230 seconds for about 230 candidates in
the worst case. Override this independently with
`MDS_IBKR_HEALTH_TIMEOUT_S`; `MDS_BBO_RING_TIMEOUT_S` remains 120 seconds.
