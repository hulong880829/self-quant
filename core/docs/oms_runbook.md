# OMS adapter runbook

## Offline release gate

Run from `core`:

```bash
JOBS=2 ./tools/validate_oms_phase4.sh
```

The script performs Release/Werror OMS-only and MDS+OMS builds, ASan+UBSan,
TSan under `setarch "$(uname -m)" -R`, all registered tests, and
`git diff --check`. It never contacts an exchange.

## Startup checks

1. Construct the owning `ExecutionChannel` with venue endpoint overrides and a
   credential provider/reference. The channel copies and clears secrets and
   owns adapters, transports, sockets and the OMS runtime. Do not put secrets
   in YAML, command arguments or logs.
2. Use direct `AdapterRuntimeConfig` injection only for deterministic tests or
   custom transport extensions whose lifetime is managed by the caller.
3. Call `initialize_lane` with a nonzero, process-unique session epoch.
4. For Polymarket, configure signature type 0 (EOA) or type 3
   (browser/proxy wallet). Public OMS construction rejects type 1 and every
   other value.
5. Poll `adapter_status`. Do not submit places until the exact venue/product
   adapter reports `Ready`.
6. Treat `Connecting`, `Authenticating`, `Reconnecting` and `Reconciling` as
   unavailable for new places. Bounded cancels may still be accepted by the
   adapter contract.

## Recovery

- `Backpressured`: drain lane updates first. The owner retains the current
  venue event and resumes without consuming additional wire data.
- `Reconnecting`: keep draining updates. The runtime retries Binance reconnect
  no faster than once per second; Polymarket waits for the injected transport's
  session event.
- `Reconciling`: do not retry an uncertain place. Wait for
  `ReconcileComplete`.
- `Failed`: stop new order flow, preserve logs that contain no secrets, and
  recreate the adapter/runtime after diagnosing transport or authentication.
- Manual recovery: call `reconcile(AdapterKind)`. The call posts owner work and
  is asynchronous.

## Safe external acceptance skeleton

External acceptance is **PENDING** until an authorized environment supplies
test credentials and endpoint access. Never run this gate against production
funds.

```bash
# The current executable is credential-presence/preflight only. Dry-run does
# not contact a venue or prove order acceptance.
./build/oms/oms_acceptance --venue binance-spot --dry-run
./build/oms/oms_acceptance --venue polymarket --dry-run
```

Live flags currently terminate after preflight and do not submit an order.
Without authorized credentials, external acceptance remains **PENDING** rather
than PASS or BLOCKED.

Required evidence: binary hash, dirty status, endpoint, account identifier hash,
adapter status transitions, HTTP/WS status classes, request IDs, reconciliation
result, and redacted logs. Never record credential values, signatures, private
keys, passphrases or full authorization headers.

## StrategyFrame smoke and offline benchmark

Build and run the public-facade example and the local queue benchmark:

```bash
cmake --build build --target oms_strategy_frame_example oms_benchmark -j2
./build/oms/oms_strategy_frame_example
./build/oms/oms_benchmark --samples 5000 --warmup 250 --idle-ms 50
```

Inline StrategyFrame code calls `service_io` on the channel-creating thread.
Dedicated-I/O code waits on the lane `notification_fd`; updates are always
delivered synchronously on the thread calling `drain_updates`. Stop submissions,
drain published updates, and explicitly call `shutdown`.

The benchmark output is labeled `offline_fake_loopback` and
`production_slo:false`. Its p50/p95/p99/p99.9 and idle observations are local
regression data only, not venue latency or a production SLO.

## Failure matrix

| Fault | Expected OMS behavior | Automated coverage |
| --- | --- | --- |
| Lane update ring full | Retain the current adapter event and stop wire reads | runtime/dual-mode tests |
| REST slot exhaustion | Return `WouldBlock` without mutating order state | request-pool/adapter tests |
| Binance 429/418 | Honor bounded `Retry-After`, block new places, keep cancels prioritized | Binance adapter tests |
| Binance timestamp skew | Refresh server-time offset before resuming signed requests | protocol/adapter tests |
| User WebSocket loss | Publish non-ready status, reconnect, then reconcile before `Ready` | adapter tests |
| Polymarket liveness loss | Close the stale session and enter `Reconnecting` | Polymarket adapter tests |
| REST timeout or uncertain place | Enter reconciliation; never issue a blind duplicate place | adapter/deadline tests |
| Duplicate/out-of-order venue update | Deduplicate or reject the invalid state transition deterministically | core/adapter tests |
| Shutdown with queued work | Stop submissions and join the owner without retaining callbacks/fds | runtime tests |

## Validation record (2026-08-18)

- Release OMS-only Werror build: PASS, 11/11 tests.
- Release combined MDS+OMS Werror build: PASS, 34/34 tests.
- ASan+UBSan OMS-only build: PASS, 11/11 tests.
- TSan OMS-only build: PASS, 11/11 tests.
- Install/package export with public `ExecutionChannel` header: PASS.
- StrategyFrame facade example: PASS.
- Offline benchmark smoke (200 measured, 20 warmup): PASS. Inline p50
  3,761 ns/p99 138,814 ns; Dedicated-I/O p50 13,667 ns/p99 155,990 ns.
  These host-specific fake-loopback numbers are regression evidence only.
- External Binance Spot/USD-M testnet and Polymarket minimum-order acceptance:
  PENDING because this run had no credentials or explicit trading
  authorization. No live order was attempted.
