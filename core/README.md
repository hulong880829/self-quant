# self-quant core

C++20 low-latency market-data foundation split into:

- `utils`: stable market-data schemas, symbol normalization, lock-free queues,
  order-book ladder, timestamp service, and hardware topology support.
- `mds`: Binance Spot/USDⓈ-M adapters, epoll/TLS/WebSocket ingestion,
  sequence reconciliation, and shared-memory publication.
- `docs`: requirements, wire protocol, Binance adapter, vertical slice, and
  validation specifications.

Trading, order management, accounts, and risk are intentionally outside the
first release.

## Build

Required toolchain:

- CMake 3.20+
- A C++20 compiler
- OpenSSL development package
- simdjson development package when JSON adapters are enabled

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

The current host must be provisioned with a compiler and CMake before running
these commands.

## Production gates

1. Freeze and review the byte-level wire protocol and public API.
2. Pass the Binance `BTCUSDT` Spot vertical slice and deterministic replay.
3. Complete full Binance Spot and USDⓈ-M sequence/recovery validation.
4. Pass long-running, fault-injection, and 2x peak-load tests.

No latency or availability claim is valid until it is measured on the target
hardware and network.
