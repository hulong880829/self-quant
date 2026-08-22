# Strategy SDK consumers

`strategys/` contains out-of-tree C++ strategy applications. They consume the
installed public SDK and do not include files from `core/*/internal`.

Build and install the SDK locally:

```bash
./strategys/scripts/sync_depend.sh
```

Build poly-mm independently:

```bash
cmake -S strategys/poly-mm -B strategys/poly-mm/build \
  -DCMAKE_PREFIX_PATH="$PWD/strategys/depend"
cmake --build strategys/poly-mm/build -j4
ctest --test-dir strategys/poly-mm/build --output-on-failure
```

The example configuration is shadow-safe (`oms.venues.polymarket.enabled:
false`). Even with a venue enabled, `poly-mm` refuses to start unless
`--accept-live-trading` is supplied. Set credentials through the named
environment variables and cross that explicit gate only after latency, replay,
and live-shadow checks pass.

Polymarket rolling windows use a stable canonical selector but a different
physical `instrument_id` for every market window. `poly-mm` owns the
active/next transition, drains orders and positions on the old IDs, and treats
its `PnlLedger` as the authority for flat state. StrategyFrame position lookup
is only a live-view cross-check.

For an embedded producer, set `mds.source: self_hosted` and
`mds.producer.config_path` to the same producer YAML used by standalone
`mds_producer`. External mode continues to list the producer-owned SHM
segments; neither mode uses a producer control socket.

Measure the exact Go fair-price WebSocket path before enabling taker opens:

```bash
FAIRPRICE_ORIGIN=http://allowed-origin.example \
FAIRPRICE_SECURE=false \
./strategys/poly-mm/build/poly-mm-latency-probe \
  127.0.0.1 9093 PROFILE BTCUSDT 30
```

The probe reports `wall_ns -> local receive` p50/p95/p99/max, parser errors,
reconnects, and queue drops. Plain `ws://` and TLS `wss://` are both supported;
configure `strategy.fairprice.secure` accordingly.
