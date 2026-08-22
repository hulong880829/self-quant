# Fair-price latency baseline

Measured on 2026-08-19 against the running local Go `/v1/stream` endpoint,
using C++ `net::WebSocketClient` over plain WebSocket:

- samples: 26 over 15 seconds
- p50: 4.815 ms
- p95: 28.989 ms
- p99: 36.346 ms
- max: 479.706 ms
- parser errors / reconnects / queue drops: 0 / 0 / 0

This validates the data path, but the observed tail does not yet justify
enabling taker orders unconditionally. Keep the live venue disabled until a
longer sample establishes a stable p99 and the measured edge comfortably
exceeds spread, execution latency, and the configured `min_edge_ticks`.
