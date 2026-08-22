# OMS trade-adapter integration

## Scope

`OmsApi` has one state owner in both execution modes. Inline mode assigns that
owner to the creating thread; Dedicated-I/O mode assigns it to the worker.
Commands, adapter I/O, deadlines, reconciliation and `StateEngine` mutation are
all executed by that owner.

`TradeAdapter` is the common boundary for the deterministic fake transport,
Binance Spot/USD-M and Polymarket. `AdapterRouter` selects by venue and product
type after resolving the request's instrument ID. A configured Fake adapter is
only a fallback for otherwise unsupported venue/product pairs; Unknown
instrument identity is rejected.

Before a place or cancel mutates `StateEngine`, the owner:

1. verifies worst-case lane update capacity;
2. resolves the instrument and adapter;
3. reserves one bounded adapter send slot;
4. performs the state transition;
5. cancels the adapter reservation if the state transition or GTD scheduling
   fails; otherwise it commits the command.

Adapter events are offered through a bounded sink. `WouldBlock` means no event
ownership transferred: the adapter retains the decoded event and stops reading
more transport input. The integrated tests exercise this with a two-slot output
ring in Inline and Dedicated-I/O modes.

## Public initialization and control

The original three-argument `OmsApi::Create` remains source-compatible and
constructs the deterministic Fake adapter. The four-argument overload accepts
`AdapterRuntimeConfig`; its adapter pointers are externally owned and must
outlive `OmsApi`.

Credentials and sockets are injected when constructing each concrete adapter,
before it is passed to `Create`. Credentials are fixed views/buffers and no OMS
logging path prints request headers, signatures, private keys or secrets.
`adapter_status()` returns the owner-published status snapshot.
`reconcile()` posts a control request; Dedicated-I/O callers never invoke the
adapter directly.

Polymarket instrument metadata accepts signature type 0 (EOA) and type 3
(browser/proxy wallet) only. Type 1 is rejected by `ExecutionChannel::Create`
and `OmsApi::Create`; callers must not rely on lower-level crypto helpers that
can describe type 1 because the public OMS adapter does not support it.

An adapter without required credentials or transport callbacks is `Failed`.
Polymarket remains `Connecting` until its injected transport reports
`SessionReady`. Binance remains `Authenticating` until listen-key creation
succeeds. No adapter reports `Ready` merely because configuration exists.

## StrategyFrame facade contract

StrategyFrame code should own `oms::api::ExecutionChannel`, initialize one lane
with a nonzero session epoch, and submit only through `place_order` and
`cancel_order`. The complete compilable example is
[`../oms/examples/strategy_frame_execution.cpp`](../oms/examples/strategy_frame_execution.cpp).

- Inline mode makes the creating thread the owner. That thread must call
  `service_io`; a different thread is rejected.
- Dedicated-I/O mode owns state and adapters on its worker. StrategyFrame must
  not call `service_io`; it waits on `notification_fd(lane_id)` instead.
- `drain_updates` invokes the callback synchronously on the draining strategy
  thread in both modes. Callback state therefore follows StrategyFrame's
  threading rules, not the I/O worker's.
- Stop new submissions, drain already-published lane updates, then call
  `shutdown`. Do not retain the notification fd or callbacks after shutdown.

## Offline queue benchmark

`oms_benchmark` serially measures place-call start through observation of the
matching `Submitted` update for Inline and Dedicated-I/O. It reports
p50/p95/p99/p99.9/max plus a short process-CPU/wall-time idle observation:

```bash
./build/oms/oms_benchmark --samples 5000 --warmup 250 --idle-ms 50
```

This is a fake-adapter, in-process offline loopback. It includes scheduler,
clock, polling and callback costs, does not exercise venue networking, and is
not evidence for a production SLO.

## Sessions, deadlines and recovery

- Binance owns listen-key create/keepalive/close state. Keepalive deadlines are
  returned to the runtime. Timestamp/uncertain-place failures enter
  reconciliation. Cancels are accepted in bounded slots during reconnect and
  are selected before other queued work.
- Polymarket consumes explicit `SessionReady`/`SessionLost` events. Reconnect
  requires a paginated open-order reconciliation before returning to `Ready`.
  In-flight REST requests receive a fixed timeout when first serviced; the
  runtime invokes the adapter deadline path.
- Fake uses the same reservation and event-retention interface. Disconnect and
  reconnect are session controls, not a copied state-engine path.
- GTD remains supported. Local deadline bookkeeping is canceled on a terminal
  venue event.

## Cryptography boundary

Polymarket request buffers, canonical EIP-712 data and HMAC buffers are fixed
capacity. Keccak and serialization do not allocate. Each Polymarket adapter
constructs one `OrderSigningContext` and reuses its `EC_GROUP`, `BN_CTX`,
`BIGNUM` and `EC_POINT` objects for type 0/type 3 signatures. RFC6979, low-s,
recovery-byte and existing golden vectors remain unchanged.

This removes per-order construction of those OpenSSL objects, but it does not
claim that OpenSSL internals perform zero heap allocations. Queue/state/request
builder paths and OpenSSL internals must be measured separately by allocator
instrumentation; TLS handshake allocation is outside the OMS queue hot path.

## Acceptance status

Offline parser/signing goldens, adapter contracts, routing, REST pool behavior,
backpressure retention, deadline/reconnect/reconcile behavior and runtime
mode parity are automated.

External Binance and Polymarket acceptance is **PENDING**. This repository run
did not have credentials and did not contact live/testnet venues. The injected
transport boundary is production-configurable, but no live readiness or order
acceptance claim follows from offline tests.
