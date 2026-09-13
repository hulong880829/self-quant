# Aster and Lighter MDS protocol contract

Verified on 2026-08-28 against the public production endpoints. Test fixtures
under `core/mds/tests/fixtures` contain reduced, representative payloads; they
preserve field names and sequence semantics but are not full market dumps.

## Aster

### Endpoints

- Spot: REST `https://sapi.asterdex.com`, WebSocket
  `wss://sstream.asterdex.com/ws`, metadata `/api/v3/exchangeInfo`, snapshot
  `/api/v3/depth`, turnover `/api/v3/ticker/24hr`.
- Perpetual: REST `https://fapi.asterdex.com`, WebSocket
  `wss://fstream.asterdex.com/ws`, metadata `/fapi/v3/exchangeInfo`, snapshot
  `/fapi/v3/depth`, turnover `/fapi/v3/ticker/24hr`.

Both products acknowledge dynamic subscriptions with
`{"id":<request-id>,"result":null}`. Stream names are lowercase:

- native BBO: `<symbol>@bookTicker`
- full book deltas: `<symbol>@depth@100ms`

`bookTicker` maps `s/u/E/T/b/B/a/A` to symbol, source sequence, event and
transaction timestamps, bid price/size, and ask price/size.

`depthUpdate` maps `s/U/u/pu/E/T/b/a` to symbol, first/final/previous sequence,
timestamps, and absolute bid/ask quantities (zero deletes a level). Live Spot
traffic includes `pu` and can have `U != previous u + 1`; therefore both Spot
and Perpetual use `pu == previous u` when `pu` is present. The REST snapshot
`lastUpdateId` is bridged to buffered deltas using the Binance snapshot
algorithm.

Connection limits from the public docs are 24 hours per connection and at most
200 streams per Perpetual connection. Spot documents at most 1024 streams and
five client messages per second. The server uses RFC6455 ping frames.

## Lighter

### Endpoints and limits

- REST metadata:
  `https://mainnet.zklighter.elliot.ai/api/v1/orderBookDetails?filter=spot|perp`
- WebSocket: `wss://mainnet.zklighter.elliot.ai/stream`
- limits per IP: 255 connections, 500 subscriptions per connection,
  255 new connections per minute, 200 client messages per rolling minute,
  and 50 inflight messages.
- keepalive: send at least one frame every two minutes. `{"type":"ping"}`
  receives `{"type":"pong"}`.

The producer uses a configured safety budget below the hard 200-message limit.
All Lighter Spot and Perpetual shards in one producer share that budget.
Startup rejects configurations requiring more than 255 WebSocket shards, and
all reconnect attempts share a rolling 255-new-connections/minute budget.

### Metadata and identity

Perpetual entries are in `order_book_details`; Spot entries are in
`spot_order_book_details`. Both expose `market_id`, `symbol`, `market_type`,
`status`, supported price/size decimals, minimum amounts, and daily volumes.

- Perpetual venue symbols are bare bases such as `BTC`; canonical symbols append
  `USDC`, such as `BTCUSDC`.
- Spot venue symbols contain the pair, such as `ETH/USDC`; canonical symbols
  remove the separator, such as `ETHUSDC`.
- `market_id` is a product-local adapter routing key and never replaces the
  canonical symbol.

### WebSocket events

Subscriptions are one client message per channel:

- `{"type":"subscribe","channel":"ticker/<market_id>"}`
- `{"type":"subscribe","channel":"order_book/<market_id>"}`

There is no separate subscription ACK. The first ticker frame has type
`subscribed/ticker`; the first complete book image has type
`subscribed/order_book`. Subsequent frames use `update/ticker` and
`update/order_book`. Unsubscribe is acknowledged as
`{"type":"unsubscribed","channel":"ticker:<market_id>"}` (and similarly for
the order book).

Ticker payloads map `ticker.s`, `ticker.b.price/size`,
`ticker.a.price/size`, `nonce`, and timestamps to native BBO events.

The initial order-book frame is a complete snapshot. Later frames carry
absolute size changes; zero size deletes a level. Continuity is valid only when
the current `order_book.begin_nonce` equals the prior `order_book.nonce`.
`offset` is observable but not a continuity key because it can jump after
reconnection.

At the configured 150-message safety budget, sending `N` client frames takes at
most `ceil(N / 150) * 60` seconds under a conservative full-window model.
Deployment configuration must set `recovery_deadline_ms` above this computed
bound and `max_continuous_recovery_ms` above the recovery deadline.
