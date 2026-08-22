# self-quant core

C++20 low-latency market-data foundation split into:

- `utils`: stable market-data schemas, symbol normalization, lock-free queues,
  order-book ladder, timestamp service, and hardware topology support.
- `mds`: Binance, OKX, Bybit, Bitget, Gate, and Hyperliquid public
  Spot/perpetual adapters, epoll/TLS/WebSocket ingestion, sequence
  reconciliation, and shared-memory publication.
- `oms`: single-owner Inline/Dedicated order state with bounded Binance,
  Polymarket and deterministic fake trade-adapter boundaries.
- `strategyframe`: C++20 strategy callbacks, YAML configuration, MDS
  shared-memory consumption, bounded order/position/timer views, and an owning
  OMS facade.
- `docs`: requirements, wire protocol, Binance adapter, vertical slice, and
  validation specifications.

StrategyFrame currently tracks open orders and fill-derived net positions.
Balances, margin, PnL and framework-enforced risk controls remain deferred.

## Build

Required toolchain:

- CMake 3.20+
- A C++20 compiler
- OpenSSL development package
- simdjson development package when JSON adapters are enabled
- yaml-cpp development package for `mds_producer`
- yaml-cpp development package when StrategyFrame is enabled

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
ctest --test-dir build --output-on-failure
```

Build only the reusable `utils` library when network dependencies are absent:

```bash
cmake -S . -B build-utils \
  -DCMAKE_BUILD_TYPE=Release \
  -DSELF_QUANT_ENABLE_MDS=OFF
cmake --build build-utils -j
ctest --test-dir build-utils --output-on-failure
```

Build the OMS without MDS:

```bash
cmake -S . -B build-oms -G Ninja -DCMAKE_BUILD_TYPE=Release \
  -DSELF_QUANT_ENABLE_MDS=OFF -DSELF_QUANT_ENABLE_OMS=ON \
  -DSELF_QUANT_ENABLE_WERROR=ON
cmake --build build-oms -j
ctest --test-dir build-oms --output-on-failure
```

See `docs/oms_trade_adapter.md` and `docs/oms_runbook.md`. External venue
acceptance is PENDING and is never implied by offline fixture tests.

Build and test StrategyFrame with both MDS and OMS enabled:

```bash
cmake -S . -B build-strategyframe -G Ninja \
  -DCMAKE_BUILD_TYPE=Release \
  -DSELF_QUANT_ENABLE_MDS=ON \
  -DSELF_QUANT_ENABLE_OMS=ON \
  -DSELF_QUANT_ENABLE_STRATEGYFRAME=ON \
  -DSELF_QUANT_ENABLE_WERROR=ON
cmake --build build-strategyframe -j2
ctest --test-dir build-strategyframe --output-on-failure
```

The StrategyFrame v4 callback, YAML, threading, shared-memory, failure and
packaging contract is in [`docs/strategyframe.md`](docs/strategyframe.md).
The installed facade boundaries are summarized in
[`docs/public_api_spec.md`](docs/public_api_spec.md); minimal, cross-venue
arbitrage, and market-maker skeletons are under `strategyframe/examples`.
Binance order entry is strict trading-WebSocket; Polymarket is the documented
REST order-entry exception. External acceptance remains **PENDING**.

The current host must be provisioned with a compiler and CMake before running
these commands.

## YAML market-data producer

Validate or run the Chinese-annotated multi-symbol configuration:

```bash
./build/mds/mds_producer \
  --config mds/config/mds_producer.example.yaml \
  --validate-only

./build/mds/mds_producer \
  --config mds/config/mds_producer.example.yaml \
  --duration 0
```

`--duration 0` runs until SIGINT/SIGTERM. Each `(venue, product)` uses one
multiplexed WebSocket and each `(venue, product, symbol, stream)` keeps an
independent shared-memory Ring. Stale segments fail fast instead of being
deleted automatically. Set `shared_memory.unlink_on_shutdown: true` only when
the producer owns the complete segment lifecycle.

Orderbook subscriptions default to the venue's fastest channel that carries at
least ten levels per side. A rejected login/topic or an invalid first snapshot
fails the producer; it never silently changes to a slower feed. For controlled
testing, `subscriptions[].orderbook_channel` can explicitly select another
supported top-10 channel. `update_interval_ms` is accepted only by Binance and
Gate because the other venues encode cadence in the channel itself.
`ladder_ticks_per_side` is a fixed-price-tick capacity, not a market-depth
count. `order_book.default_price_band_bps` (or a subscription-level
`price_band_bps`) defines the minimum price band that this capacity must cover;
the producer validates it after loading venue tick-size metadata.

OKX `books-l2-tbt` requires public-WebSocket login and the corresponding OKX
VIP entitlement. Secrets are never stored in YAML; reference environment
variables instead:

```yaml
auth:
  api_key_env: OKX_MDS_API_KEY
  secret_env: OKX_MDS_SECRET
  passphrase_env: OKX_MDS_PASSPHRASE
```

The producer reads these variables at startup and never prints their values.
Explicit `orderbook_channel: books` selects the unauthenticated 100ms OKX feed
and is reported as `explicit_override=true`.

### Polymarket rolling MDS

Polymarket binary-option market data uses fixed ticker/orderbook SHM segments
for the stable MDS-only aliases `BTC5MUP` and `BTC5MDOWN`. Gamma REST discovers
the exact current market and token IDs asynchronously; all real-time
book/BBO/tick/lifecycle data comes from WSS. On rollover, the alias identity
and segment stay fixed while the current slug may change only with a new
`book_generation` and `InstrumentUpdate(Building)`.

Token IDs remain adapter-private, the aliases are not directly OMS-tradeable,
and the existing BBO/Book wire schema is unchanged. These feeds are
single-venue and are not cross-venue aggregation members. Disconnect or stale
data removes the book from Live until reconnect and a fresh snapshot rebuild
the next generation. The isolated live smoke across two rollovers remains
**PENDING**; see `docs/validation.md`.

## Cross-venue aggregation

`mds_aggregator` consumes existing single-venue Rings; it does not open
exchange connections. Start the required producers first, then validate or run:

```bash
./build/mds/mds_aggregator \
  --config mds/config/mds_aggregator.example.yaml \
  --validate-only

./build/mds/mds_aggregator \
  --config mds/config/mds_aggregator.example.yaml \
  --duration 0
```

AggBbo and AggOrderBook outputs can be enabled independently. Each venue
contributes at most ten levels per side (or every available level when fewer
than ten), with exact-price merging and per-venue base-quantity attribution.
AggOrderBook may be legitimately crossed across several levels.

Member TTLs default to three times the venue capability cadence. Standalone
aggregator YAML can override either stream independently without changing
in-process SDK behavior:

```yaml
members:
  - venue: okx
    source_quote_asset: USDT
    bbo_ttl_ms: 300
    orderbook_ttl_ms: 300
```

AggBbo publishes a diagnostic raw BBO and a routing-oriented gated BBO. Raw is
finite-lived:
`max(ttl, min(saturating_mul(ttl, 10), 5 seconds))`; gated additionally applies
TTL, FX and optional cross-skew enforcement. Cross-skew defaults to
observe-only. Keep this mode until measured timestamp/skew distributions
justify enforcement.

Hyperliquid USDC members use Binance Spot `USDCUSDT` bid/ask conversion when
the output bucket is USDT. An unavailable, stale, or excessive-depeg FX quote
excludes Hyperliquid from raw, gated, and aggregate depth; unconverted USDC is
never compared directly with USDT.

Display aggregate Rings with:

```bash
./build/mds/mds_shm_consumer --agg-depth 10 \
  /selfquant.mds.agg_perp_usdt_binance-bitget-bybit-gate-hyperliquid-okx.btcusdt.aggorderbook.2
```

For an in-place terminal dashboard with independent BBO and depth sequence
labels, pass both Rings. The display coalesces updates at the requested refresh
rate; the two Rings remain independent snapshots:

```bash
./build/mds/mds_shm_consumer \
  --live-agg-bbo --agg-depth 10 --refresh-ms 100 \
  /selfquant.mds.agg_perp_usdt_binance-bitget-bybit-gate-hyperliquid-okx.btcusdt.aggbbo.2 \
  /selfquant.mds.agg_perp_usdt_binance-bitget-bybit-gate-hyperliquid-okx.btcusdt.aggorderbook.2
```

The consumer can also obtain segment names and future service settings from
YAML:

```bash
./build/mds/mds_shm_consumer \
  --config mds/config/mds_shm_consumer.example.yaml \
  --validate-only
```

The Gateway uses plaintext WebSocket and requires `auth_token_env` on every
listener; clients send the environment variable's value as a bearer token.
Because the token is sent in plaintext, keep this endpoint on a trusted private
network. The example starts a 50 ms latest-state Gateway and an independent
200 ms, 24-hour zstd recorder together with:

```bash
export MDS_GATEWAY_TOKEN='replace-with-a-long-random-secret'
./build/mds/mds_shm_consumer \
  --config mds/config/mds_shm_consumer.example.yaml \
  --gateway --record
```

Clients connect to `ws://host:port/v1/market-data` and subscribe by exact
configured segment name. See `docs/mds_gateway_protocol.md`. The gateway is for
frontends and medium/low-frequency strategies; latency-sensitive strategies
should continue to read SHM directly.

Recording detection is optional and can be disabled with
`-DMDS_ENABLE_RECORDING=OFF`. CMake checks `pkg-config` for `libzstd` before
falling back to `zstd.h` and the zstd library. Missing zstd does not disable the
print consumer, Gateway, producer, aggregator, or `libmds.a`; `--record` returns
`InvalidConfig` until `libzstd-dev` is installed and the project rebuilt.

For fixed-rate crypto BBO snapshots in ClickHouse, start a ClickHouse server
first (local Docker example from the repo root):

```bash
backend/deploy/clickhouse/generate-certs.sh
sudo cp backend/deploy/clickhouse/certs/server.crt \
  /usr/local/share/ca-certificates/clickhouse-localhost.crt
sudo update-ca-certificates

export CLICKHOUSE_BBO_PASSWORD='replace-me'
docker compose -f backend/deploy/docker-compose.yml up -d clickhouse
curl -sk https://127.0.0.1:8443/ping   # expect Ok.
```

Then run the dedicated raw multiplex consumer mode (after `mds_producer` is
live and `clickhouse_bbo` config uses `host: 127.0.0.1`, `service: "8443"`,
and matching `shm_prefix` / shard `selectors`):

```bash
export CLICKHOUSE_BBO_PASSWORD='replace-me'
./build/mds/mds_shm_consumer \
  --config mds/config/mds_clickhouse_bbo.example.yaml \
  --clickhouse-bbo
```

`clickhouse_bbo.selectors` must explicitly name a crypto venue, product, and
shard. They resolve only to producer ticker multiplex Rings such as
`/selfquant.mds.binance.perpetual.ticker.shard0.2`; `all`, Polymarket,
per-symbol, order-book, and aggregate inputs are rejected. The recorder waits
for bounded instrument-catalog metadata before accepting BBOs, samples no
faster than 200 ms, and sends asynchronous RowBinary batches. Failed HTTP
batches are requeued in the bounded local queue; capacity and final-shutdown
drops are reported on exit. The configured user needs `CREATE TABLE` and
`INSERT` permission for the target database/table.

Recordings are UTC hourly `.sqrec.zst` shards with atomic `manifest.v1.json`
discovery. Validate or export them using:

```bash
./build/mds/mds_record_tool --validate <shard.sqrec.zst>
./build/mds/mds_record_tool --export-jsonl <shard.sqrec.zst>
```

The recording format and public reader contract are documented in
`docs/mds_recording_format.md`.

Public SDK users can independently call `register_agg_bbo()` and
`register_agg_orderbook()` between `init()` and `start()`, then use the typed
`try_read_agg_*` functions. The two handles are not an atomic snapshot.

## Production gates

1. Freeze and review the byte-level wire protocol and public API.
2. Pass the Binance `BTCUSDT` Spot vertical slice and deterministic replay.
3. Complete per-venue parser, sequence/recovery, multiplexing, and reconnect
   validation.
4. Pass long-running, fault-injection, sanitizer, and 2x peak-load tests.

No latency or availability claim is valid until it is measured on the target
hardware and network.
