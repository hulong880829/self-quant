# StrategyFrame v4 runbook and API contract

This document describes the StrategyFrame implementation currently in this
repository. It is an operational contract, not a claim of ABI stability or
external venue acceptance. The installed CMake package is currently versioned
`0.1.0`; “v4” names this StrategyFrame delivery, not the package ABI.

## Scope and current status

StrategyFrame is a C++20, single-strategy facade over the MDS shared-memory
wire format and the owning OMS `ExecutionChannel`. It provides:

- compile-time validation of a fixed strategy callback surface;
- YAML loading for MDS, OMS and strategy parameters;
- shared-memory, self-hosted MDS and internal replay source modes;
- one strategy callback/manager owner thread;
- Inline or Dedicated-I/O OMS operation;
- bounded order, position, timer and OMS queue storage.

External Binance Spot/USD-M and Polymarket order acceptance is **PENDING**.
Fixture, replay, loopback, package and benchmark results do not prove that a
venue accepted, canceled or reconciled a live order.

## Public include and Strategy concept

Consumers normally include:

```cpp
#include "strategyframe/strategyframe.h"
```

A strategy type must satisfy `strategyframe::Strategy<T>` by implementing all
eight callbacks with the exact `void` return types:

```cpp
void init(strategyframe::StrategyContext&);
void on_bbo_update(const strategyframe::BboUpdate&);
void on_orderbook_update(const strategyframe::OrderBookUpdate&);
void on_agg_bbo_update(const strategyframe::AggBboUpdate&);
void on_agg_orderbook_update(const strategyframe::AggOrderBookUpdate&);
void on_order_update(const strategyframe::ExecutionUpdate&);
void on_oms_status(const strategyframe::OmsStatusUpdate&);
void on_timer(const strategyframe::TimerEvent&);
```

There are no optional callbacks in the current concept; use no-op bodies for
events the strategy does not consume. `init` is called once after MDS starts,
after an initial metadata drain, and after OMS creation when instrument
metadata was available. Market callbacks are suppressed until `init` returns.

Callbacks execute synchronously and serially on the thread running
`StrategyRunner::run()`. A callback must not block on work that depends on that
same owner loop. Exceptions are caught at the template boundary, counted as
`callback_failures`, and terminate the run with `Error::CallbackFailed`.

The update spans and string views are borrowed. In particular, order-book and
aggregate level spans are only safe during the callback. Copy anything that
must outlive the callback.

## StrategyRunner lifecycle

```cpp
auto loaded = strategyframe::load_config("strategy.yaml");
if (!loaded) return 1;

strategyframe::StrategyRunner<MyStrategy> runner(
    std::move(loaded.value), MyStrategy{});
const strategyframe::Error result = runner.run();
```

`StrategyRunner` is non-copyable and single-use:

1. `run()` may be entered only once and from one thread.
2. The runtime starts MDS and performs up to 64 bounded initial metadata-drain
   rounds.
3. YAML instruments are merged with MDS metadata, then the OMS channel and lane
   1 are initialized.
4. Enabled live venues are pre-warmed until every adapter is `Ready` or
   `startup_timeout_ns` expires. Startup status events are retained.
5. `init(context)` runs on the owner thread, followed by retained startup
   status callbacks.
6. Each loop services Inline OMS I/O when selected, drains up to
   `event_budget` OMS updates, dispatches due timers, then polls up to
   `event_budget` market records. Bounded budgets prevent one source from
   starving the others.
7. `request_stop()` may be called from another thread or through the context.
8. MDS readers are unregistered, the self-hosted MDS singleton is shut down
   when owned, and the OMS channel is shut down.

Calling `run()` concurrently or a second time returns `InvalidState`.
`request_stop()` before `run()` is remembered. `metrics()` on the runner is the
last completed run's snapshot; it is not a live, synchronized metrics API.

The runtime does not install signal handlers. The application should translate
SIGINT/SIGTERM into `runner.request_stop()` from an appropriate control path.

## StrategyContext contract

The context is created by the runtime and remains valid only while that runtime
exists. A strategy may retain its address for later callbacks, as the bundled
minimal example does, but must not use it after `run()` returns.

### Commands

- `place_order(request)` is the only order-entry call. Strategies set
  `OrderRequest::type`, `time_in_force`, `flags`, price, quantity and
  expiration explicitly; IOC, FOK, post-only and market orders do not use
  separate helper APIs.
- `cancel(token)` submits a cancel for a currently tracked open order.
- `schedule_timer(first_deadline_ns, interval_ns)` uses the same monotonic
  nanosecond domain as `now_ns()`. An interval of zero creates a one-shot.
- `cancel_timer(handle)` rejects stale or inactive generation handles.

Timers use one nonblocking monotonic `timerfd` plus a fixed-capacity,
generation-safe registry. Low-CPU mode polls that descriptor and the Dedicated
OMS update descriptor with a bounded timeout; timer callbacks remain serialized
on the strategy owner thread.

Order and cancel success means the bounded OMS command was accepted, not that
the venue accepted the operation. Final state comes from `on_order_update`.
Commands, cancels and timer mutation are owner-thread-only; a call from another
thread returns `InvalidThread`. The current runtime has one lane (`lane=1`).
`client_order_id` is limited to 64 bytes at the context boundary.

### Queries

- `open_orders()` returns a borrowed dense span of nonterminal orders.
- `find_order(token)` returns a value copy or `NotFound`.
- `positions()` returns a borrowed dense span.
- `find_position(account, instrument, side)` returns a value copy or
  `NotFound`.
- `find_instrument(id)` returns fixed-capacity instrument metadata without
  exposing MDS or OMS types.
- `find_instrument(InstrumentSelector)` selects the current unexpired physical
  instrument for a venue/product/canonical-symbol series. Exact-ID catalog
  lookup remains available for draining an old rolling window.
- `query_open_orders(account)` and `query_positions(account)` start
  account-routed asynchronous venue queries. Snapshot and completion callbacks
  are authoritative query results and never overwrite the local managers.
- The `(account, instrument_id)` overloads issue a fixed-size
  `SingleInstrument` query carrying the venue-native symbol/token reference.
  Unknown identities are counted and held in the fixed reconciliation
  quarantine with `instrument_id == 0`; they do not fail the rest of a query.
- `execution_ready(instrument_id)` means catalog notification, Execution
  Directory acknowledgement, and adapter readiness are all complete. It does
  not depend on order-book liveness.
- `oms_status(adapter_kind)` returns the latest cached venue-status update or
  `NotFound` before one has arrived.
- `params()` exposes immutable typed strategy parameters.
- `metrics()` returns a snapshot.
- `now_ns()` returns steady/monotonic time, not wall-clock or venue time.
- `request_stop()` asks the run loop to exit.

MDS subscriptions are frozen by the YAML configuration before `run()` starts;
the current context intentionally does not mutate producer subscriptions after
the MDS readers and OMS Execution Directory have been initialized.

Mutating the managers can invalidate the ordering and contents represented by
previously returned spans. Treat spans as callback-local views.

## Account, order and position managers

`OrderManager` and `PositionManager` allocate fixed vectors and open-addressing
indexes at construction. Configured capacities must be powers of two and at
least two.

`OrderManager`:

- inserts a local `PendingSubmit` row after OMS accepts a place command;
- tracks `Open` and `PartiallyFilled`;
- removes terminal `Filled`, `Canceled`, `Rejected` and `Expired` rows;
- copies client/venue IDs into fixed 96-byte internal storage (the public
  context rejects client IDs over 64 bytes);
- reports duplicate terminal updates as a missing local order.

`PositionManager`:

- updates net/long/short quantity from fills;
- rescales integer quantities to the position's existing scale;
- stores a generation value;
- swap-removes a row when its quantity reaches zero;
- supports low-frequency physical-instrument retirement without clearing the
  independent fill-dedup ring;
- has internal snapshot replacement support, which omits zero rows.

The runtime currently applies fills only to `PositionSide::Net`. Manager
capacity, scale and arithmetic failures increment `out_of_order_updates`;
duplicate fills increment `duplicate_updates`. These conditions are observable
through metrics but are not delivered as a separate strategy callback. Size
capacities conservatively.

`AccountManager` is currently only a non-owning grouping of the order and
position managers. It has no account-balance, available-funds, margin,
collateral, fee, realized/unrealized PnL or risk-limit state, and is not exposed
as a separate `StrategyContext` accessor.

## YAML contract

The root must contain only `mds`, `oms` and optional `strategy` maps. Unknown
keys in the typed MDS/OMS sections are rejected regardless of
`oms.strictness`. `load_config()` intentionally collapses parser diagnostics to
`Error::InvalidConfig`; use `strategyframe_config_tool` for pass/fail
validation, not detailed error reporting.

### MDS

External shared memory:

```yaml
mds:
  source: external_shm
  segments:
    - name: /selfquant.mds.binance.spot.btcusdt.ticker.2
      ring_bytes: 8388608
      max_record_bytes: 65536
      expected_reader_budget: 1
      heartbeat_interval_ns: 500000000
```

`external_shm` requires at least one segment. StrategyFrame attaches with
`create=false`, starts each reader at the current writer cursor, and therefore
does not replay old records. Before registering, it requires the segment's
current active reader count to be less than `expected_reader_budget`. This
field is a StrategyFrame admission ceiling, not a reservation of producer
slots. Registration can still fail if the ring's own registry is full.

`ring_bytes` and `max_record_bytes` must exactly match the producer-owned
segment header; a mismatch fails startup with `InvalidConfig`. Readers heartbeat
at `heartbeat_interval_ns` and unregister on normal teardown.

The five-venue production aggregate segments use `ring_bytes: 33554432` and
`max_record_bytes: 32768`. A future StrategyFrame strategy that attaches to an
`aggbbo` or `aggorderbook` segment must declare those values. This does not
change strategies that attach only to raw producer BBO/order-book rings.

Self-hosted producer:

```yaml
mds:
  source: self_hosted
  producer:
    config_path: core/mds/config/mds_producer.example.yaml
  required_instruments:
    - {venue: binance, product: spot, symbol: BTCUSDT}
```

`self_hosted` loads exactly the standalone `mds_producer` YAML schema and runs
the shared `ProducerRuntime`; StrategyFrame no longer maintains a reduced
venue/subscription schema. The runtime starts MDS and attaches readers to its
resolved segments. External mode remains static YAML plus SHM and has no
control plane.

`required_instruments` is a startup barrier. Strategy `init()` and OMS creation
wait until every selector resolves to a current catalog or
`startup_timeout_ns` expires.

`replay` is a supported programmatic/internal source mode. YAML accepts it, but
the public `StrategyRunner` has no API for injecting replay records; replay
injection lives on the internal `MarketDataSource` test surface.

### External SHM and Lossless boundaries

StrategyFrame does not create or select the mode of an external segment. If the
producer created a `Lossless` ring, every active StrategyFrame reader
participates in producer backpressure: an initializing/suspect reader or a
reader that would be overwritten causes producer `publish()` to return
`QuotaExceeded`. It does not wait. The producer must decide whether to retry,
pause feeds, reclaim a stale reader, or fail; StrategyFrame cannot make that
decision on its behalf.

For overwrite-oldest rings, a lagging reader can observe overwrite/rejection.
StrategyFrame resets its local book, advances that reader to the latest writer
cursor, increments `out_of_order_updates`, and continues. It does not deliver a
dedicated gap callback to the strategy, so strategies needing a hard data-loss
halt must monitor metrics and market generation/state, or enforce that policy
outside the current public facade.

One StrategyFrame stream consumes one shared-memory reader slot. Ensure the
producer's `max_readers`, StrategyFrame's admission budget, stale-reader lease
policy and operational process count agree before deployment.

### OMS

```yaml
oms:
  threading: single_thread       # single_thread | multi_io_thread
  idle_policy: busy_spin         # busy_spin | adaptive | low_cpu
  strictness: strict             # parsed; see caveats below
  strategy_cpu: -1
  io_cpu: -1
  numa_node: -1
  session_epoch: 1
  event_budget: 64
  metric_sample_rate: 100
  startup_timeout_ns: 10000000000
  memory:
    lock_pages: false
    prefault: true
    strict: false
  socket:
    tcp_nodelay: true
    receive_buffer_bytes: 0
    send_buffer_bytes: 0
    busy_poll_us: 0
  capacities:
    command_queue: 1024
    update_queue: 4096
    market_data_queue: 4096
    order_table: 4096
    position_table: 1024
    fill_dedup: 8192
    timer_table: 1024
    instrument_directory: 4096
```

`session_epoch`, `event_budget`, `metric_sample_rate`,
`retire_deferred_warning_count` and `startup_timeout_ns` must be nonzero.
Every capacity must be a power of two and at least two. The session epoch
becomes part of each 16-byte `OrderToken`; use a process/run-unique nonzero
value to reduce stale-token ambiguity.

`oms.instruments` is a compatibility validation list. Executable instruments
come from MDS catalog records and are registered in the fixed-capacity OMS
Execution Directory without changing frozen routes on existing orders.
Polymarket condition IDs accept 32-byte hex and token IDs accept either
32-byte hex or decimal uint256 text.

`oms.venues` keys are `binance_spot`, `binance_usdm` and `polymarket`.
Endpoints are `{host, port}` maps; `host` must contain neither scheme, path nor
port. Secrets are environment-variable names and are resolved only when the
live channel is created. An enabled venue requires at least API-key and secret
environment names; Polymarket runtime creation additionally requires signer,
funder, private key and passphrase values.

Example live venue shape:

```yaml
oms:
  # ...runtime fields...
  venues:
    binance_usdm:
      enabled: true
      rest: {host: fapi.binance.com, port: "443"}
      trading_websocket: {host: ws-fapi.binance.com, port: "443"}
      user_websocket: {host: fstream.binance.com, port: "443"}
      api_key_env: BINANCE_USDM_API_KEY
      secret_env: BINANCE_USDM_SECRET
    polymarket:
      enabled: false
      rest: {host: clob.polymarket.com, port: "443"}
      user_websocket:
        {host: ws-subscriptions-clob.polymarket.com, port: "443"}
      signer_address_env: POLY_SIGNER_ADDRESS
      funder_address_env: POLY_FUNDER_ADDRESS
      private_key_env: POLY_PRIVATE_KEY
      api_key_env: POLY_API_KEY
      secret_env: POLY_API_SECRET
      passphrase_env: POLY_PASSPHRASE
```

### Strategy parameters

`strategy` may contain scalars, nested maps and sequences. Nested paths are
flattened with dots and read through `contains`, typed accessors, `child()` and
`sequence_size()`. Integer values are integers; `require_double` also accepts
integers. Returned string views are owned by the immutable configuration and
remain valid for the runtime/config lifetime.

## Threading and I/O ownership

`single_thread` maps to OMS `ExecutionMode::Inline`. The StrategyFrame owner
thread performs market polling, OMS `service_io(0)`, update draining, timers
and every callback. The Inline `ExecutionChannel` owner constraint is strict:
only the thread that creates/owns the runtime may service I/O or issue
owner-thread StrategyContext commands.

`multi_io_thread` maps to `DedicatedIo`. OMS state and socket I/O run on its
internal worker; the StrategyFrame owner still polls MDS, drains OMS updates
and invokes every callback. StrategyFrame currently polls the lane rather than
exposing/waiting on `notification_fd`, so low-CPU wakeup behavior is not a
public StrategyFrame guarantee.

There is exactly one strategy owner thread and one OMS lane in both modes.
“Multi-I/O” does not mean concurrent callbacks, multiple strategies or
multiple lanes.

## Binance WSS and Polymarket REST execution boundary

For live Binance Spot and USD-M, place and cancel construction sets
`use_trading_websocket=true`. The adapter requires the dedicated Binance
trading WebSocket session to be ready and does not silently fall back to REST
order entry. The separate user WebSocket carries execution/account events;
REST remains in the adapter for control/query/reconciliation operations.

Polymarket is the intentional order-entry exception: signed place, cancel and
open-order reconciliation requests use HTTPS REST. Its authenticated
WebSocket supplies user/session events and liveness. Thus “strict WSS order
entry” applies to Binance, not Polymarket. All configured hosts are passed to
TLS clients as bare hosts; there is no plaintext live transport option in this
facade.

## Failure and reconciliation behavior

- A callback exception/failure stops the runtime with `CallbackFailed`.
- MDS attach, decode, heartbeat, commit or unrecoverable read errors stop with
  `MdsFailure`.
- Overwrite/gap advances to latest as described above; local book state resets.
- OMS queue full maps to `WouldBlock`; callers may retry only according to
  their own bounded policy.
- Place/cancel success is asynchronous. Do not infer venue state from a token.
- Venue status and reconcile completion arrive through `on_oms_status`.
- Binance/Polymarket adapters reconnect and reconcile after relevant stream or
  uncertain-request failures. Initial strategy startup is held behind the
  adapter-ready pre-warm barrier; later places are locally rejected while the
  selected adapter is unavailable.
- StrategyFrame exposes status notifications but does not expose
  `ExecutionChannel::reconcile()` or `venue_status()` through
  `StrategyContext`; manual reconciliation currently requires use of the OMS
  API outside this facade.
- Duplicate/unknown OMS updates increment `duplicate_updates`; invalid local
  transitions increment `out_of_order_updates`.
- Shutdown is orderly but does not promise that every previously accepted
  command reached a venue. Persist external intent if crash recovery requires
  stronger semantics.

See [`oms_runbook.md`](oms_runbook.md) for the lower-level adapter recovery
matrix.

## HFT-oriented profile and zero-allocation caveats

A reasonable latency-oriented starting profile is:

```yaml
oms:
  threading: single_thread
  idle_policy: busy_spin
  strategy_cpu: 4
  io_cpu: -1
  numa_node: 0
  event_budget: 64
  capacities:
    command_queue: 4096
    update_queue: 8192
    market_data_queue: 8192
    order_table: 8192
    position_table: 2048
    fill_dedup: 16384
    timer_table: 2048
```

Use `multi_io_thread` and a distinct `io_cpu` when isolating socket/state work
outperforms Inline on measured target hardware. Tune capacities, event budget,
CPU placement and idle policy from production-like bursts; no profile is an
SLO.

Current caveats:

- `strategy_cpu` binds the MDS/callback owner thread; strict mode fails startup
  if affinity cannot be applied. `io_cpu` is forwarded to the OMS I/O worker.
- `memory.lock_pages` invokes `mlockall`; `memory.strict` makes failure fatal.
  `numa_node` applies Linux `MPOL_BIND` to subsequent owner-thread allocations,
  and prefaulting touches 64 KiB of owner stack. Manager storage is allocated
  and zero-initialized during construction, before `run()`.
- `socket` settings are forwarded to every live REST, user-WebSocket and
  trading-WebSocket TCP connector. `TCP_NODELAY`, receive/send buffers and
  Linux busy-poll are best effort because kernels may clamp or reject them;
  deployment must inspect effective socket values. `market_data_queue` remains
  admission configuration only because the current owner directly polls SHM.
- `metric_sample_rate` controls sampled market-callback duration metrics;
  counters remain exact while clock/TSC reads occur only on sampled callbacks.
  The same rate samples receive-TSC-to-callback cycles, OMS-publish-to-manager
  dispatch nanoseconds, and context order-call duration. Queue high-water values
  come from the OMS fixed rings. Metrics are pull-based through the context;
  the hot path performs no logging or string formatting.
- Startup allocates vectors, strings, TLS/network objects and manager
  storage. Configuration parsing also allocates.
- The steady manager/timer/OMS ring paths are bounded, and market callback
  views avoid per-event ownership allocation, but this is not a blanket
  zero-allocation guarantee.
- Live MDS instrument discovery uses a fixed-capacity open-addressed table;
  self-hosted startup builds strings and sets, and replay queues allocate.
- Strategy callbacks can allocate, block or throw; StrategyFrame cannot
  prevent that.
- CPU isolation, IRQ placement, NIC queues, effective NUMA locality and socket
  values must still be verified by deployment tooling.

No latency, throughput, losslessness or availability claim is valid without
measurement on the target compiler, kernel, hardware, NIC and network.

## Deferred controls: do not assume they exist

The following remain outside the current StrategyFrame contract:

- balance and available-funds tracking;
- initial/maintenance margin and collateral;
- fees and funding accrual;
- realized or unrealized PnL;
- mark-price/liquidation state;
- pre-trade notional, position, order-rate, drawdown or kill-switch risk
  controls;
- durable intent/event storage and restart reconstruction;
- a public account snapshot/reconciliation API.

Until these are implemented and tested, strategies must enforce their own
conservative controls and should fail closed when required account state is
unavailable. `max_position` in the example YAML is only a strategy parameter;
the framework does not enforce it.

## Build, test and package

From `core`, StrategyFrame requires both MDS and OMS plus yaml-cpp:

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

Validate the example configuration and run the minimal strategy:

```bash
./build-strategyframe/strategyframe/strategyframe_config_tool \
  --config strategyframe/config/strategyframe.example.yaml \
  --validate-only
./build-strategyframe/strategyframe/strategyframe_minimal_example \
  strategyframe/config/strategyframe.example.yaml
```

The minimal example expects its configured external SHM segment to exist; the
validation command does not attach to it. The same directory contains
`cross_venue_arbitrage.cpp` and `market_maker_skeleton.cpp`, demonstrating
IOC/FOK, limit placement, cancellation, timers, status handling, and in-memory
order/position queries. They are operational skeletons, not profitable
strategies or risk controls.

Run the full offline matrix and write the loopback benchmark record:

```bash
JOBS=2 bash tools/validate_strategyframe.sh
```

The script runs Release/Werror, ASan+UBSan, TSan, package consumption,
`git diff --check`, the zero-allocation manager hot-path test, and a wide
callback p99 regression threshold. Override
`STRATEGYFRAME_MAX_CALLBACK_P99_NS` only with a reviewed host-specific
baseline. The benchmark reports `production_slo=false` and does not include
network latency.

Exercise install/export and downstream `find_package` consumption:

```bash
cmake -S . -B build-strategyframe-package -G Ninja \
  -DCMAKE_BUILD_TYPE=Release \
  -DSELF_QUANT_ENABLE_MDS=ON \
  -DSELF_QUANT_ENABLE_OMS=ON \
  -DSELF_QUANT_ENABLE_STRATEGYFRAME=ON \
  -DSELF_QUANT_BUILD_PACKAGE_TESTS=ON
cmake --build build-strategyframe-package -j2
ctest --test-dir build-strategyframe-package \
  -R 'strategyframe|install_package_smoke' --output-on-failure
```

Installed consumers link `self_quant::strategyframe`; yaml-cpp, Threads and
OpenSSL are discovered by the package configuration as applicable. Internal
StrategyFrame headers are intentionally not installed.

## Release checklist

1. Validate YAML and confirm every parsed-but-unwired field is handled by
   deployment policy.
2. Confirm MDS segment schema, ring mode, reader slots, lease timeout and
   admission budgets.
3. Verify MDS emits matching instrument metadata before expecting OMS
   readiness.
4. Confirm unique session epoch, capacity headroom and owner/I/O CPU policy.
5. Supply secrets only through environment references; never log values.
6. Gate places on adapter readiness and reconcile completion.
7. Exercise callback failure, queue-full, MDS gap, reconnect and shutdown
   paths.
8. Run Release/Werror, sanitizer, package and soak tests appropriate to the
   deployment.
9. Record external acceptance as **PENDING** until an authorized, redacted
   venue test supplies actual evidence. Never infer PASS from offline tests.

## External acceptance record

- Binance Spot testnet trading-WebSocket minimum place/cancel: **PENDING**.
- Binance USD-M testnet trading-WebSocket minimum place/cancel: **PENDING**.
- Polymarket explicitly authorized REST minimum place/cancel: **PENDING**.

No credentials or funds authorization are stored in this repository. Moving an
item from PENDING requires a redacted run record containing endpoint/product,
client order id, venue order id, final state, reconciliation result, timestamp,
and operator authorization reference.

