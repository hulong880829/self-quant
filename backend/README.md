# SelfQuant backend

Go processes expose perpetual-futures funding data and account login/session
APIs plus product performance reports. PostgreSQL is the required infrastructure dependency; migrations are
embedded into `funding-service` and `account-service` and run idempotently at
startup. Redis is optionally provided by Compose for future hot-data workloads
(e.g. MDS ticker/orderbook recording); it is not used by services yet and runs
without persistence (RDB and AOF disabled).

## Run locally

Requirements: Docker Compose (default), or Go 1.25+ for host `go run` debugging.
PostgreSQL 17, Redis 7, and optional ClickHouse are started by Compose.
The frontend stays on the host (`web/`, typically `npm run dev` on :3000) and
keeps `NEXT_PUBLIC_*` pointed at the published gateway (`:8080`) and aggdata
(`:9093`) ports. C++ MDS (`mds_producer` / `mds_aggregator` / `mds_shm_consumer`)
also stays on the host. `aggdata-service` uses host networking so it can
reach the MDS gateway on `127.0.0.1:9443` and still publish `:9093`.

```bash
cp .env.example .env
# Set ACCOUNT_TOKEN_SECRET, ACCOUNT_CREDENTIALS_KEY, TRADER_INTERNAL_TOKEN,
# REPORT_INTERNAL_TOKEN, and MDS_GATEWAY_TOKEN before starting services.
cd backend
set -a; source .env; set +a
docker compose --env-file .env -f deploy/docker-compose.yml up -d --build
```

Compose rebuilds the shared `selfquant-backend:local` image and starts all nine
Go services plus Postgres, Redis, and ClickHouse. Network variables in `.env`
that still say `localhost` are overridden in `deploy/docker-compose.yml`.

Before the first Compose cutover, stop host processes that bind 8080 and
9090–9097 so the published ports are free. Confirm readiness with:

```bash
curl -sS http://127.0.0.1:8080/health/live
curl -sS http://127.0.0.1:8080/health/ready
curl -sS http://127.0.0.1:9093/healthz
# /readyz needs the host MDS gateway; it is not a container start gate.
curl -sS http://127.0.0.1:9093/readyz
```

Restart one Go service without touching databases:

```bash
docker compose --env-file .env -f deploy/docker-compose.yml restart aggdata-service
```

Restart every Go service and leave Postgres / Redis / ClickHouse running:

```bash
docker compose --env-file .env -f deploy/docker-compose.yml restart \
  funding-service account-service polymarket-service \
  report-service trader-service ai-service spread-service \
  aggdata-service api-gateway
```

Infrastructure only (the `infra` workflow for host `go run` debugging):

```bash
docker compose --env-file .env -f deploy/docker-compose.yml up -d postgres redis clickhouse
```

Then in additional terminals:

```bash
set -a; source .env; set +a
go run ./cmd/funding-service
```

```bash
set -a; source .env; set +a
go run ./cmd/account-service
```

```bash
set -a; source .env; set +a
go run ./cmd/api-gateway
```

```bash
set -a; source .env; set +a
go run ./cmd/report-service
```

```bash
set -a; source .env; set +a
go run ./cmd/trader-service
```

```bash
set -a; source .env; set +a
go run ./cmd/ai-service
```

ClickHouse first-time TLS setup for the host MDS recorder
(`mds_shm_consumer --clickhouse-bbo`):

```bash
./deploy/clickhouse/generate-certs.sh
export CLICKHOUSE_BBO_PASSWORD='your-password'
docker compose --env-file .env -f deploy/docker-compose.yml up -d clickhouse
```

See [`deploy/clickhouse/README.md`](deploy/clickhouse/README.md) for TLS trust,
health checks, and MDS wiring. MDS connects to `127.0.0.1:8443` over HTTPS.

Redis is ephemeral (`redis-server --save ""`, no volume); data is cleared when
the container is recreated.

HTTP endpoints:

- `GET /health/live`
- `GET /health/ready`
- `POST /api/v1/auth/login`
- `GET /api/v1/auth/session`
- `POST /api/v1/auth/logout`
- `GET /api/v1/trading-accounts`
- `POST /api/v1/trading-accounts`
- `DELETE /api/v1/trading-accounts/{id}`
- `GET /api/v1/funding-rates`
- `GET /api/v1/funding-rates/{exchange}/{exchangeSymbol}/history?limit=10`
- `GET /api/v1/reports/products`
- `GET /api/v1/reports/products/{id}`
- `GET|POST /api/v1/ai/conversations`
- `POST /api/v1/ai/conversations/{id}/rename`
- `DELETE /api/v1/ai/conversations/{id}`
- `GET /api/v1/ai/conversations/{id}/messages`
- `POST /api/v1/ai/chat` (SSE)
- `GET /api/v1/reports/products/{id}/daily?from=YYYY-MM-DD&to=YYYY-MM-DD`
- `GET /api/v1/reports/products/{id}/cash-flows`
- `POST /api/v1/reports/products/{id}/cash-flows`
- `POST /api/v1/reports/products/{id}/recompute`
- `GET /api/v1/trader/accounts/{accountId}/instruments?type=spot|perpetual`
- `POST /api/v1/trader/orders`
- `GET /api/v1/trader/orders?accountId=`
- `GET /api/v1/trader/orders/{id}`
- `DELETE /api/v1/trader/orders/{id}`
- `GET /api/v1/ai/credentials/openrouter`
- `POST /api/v1/ai/credentials/openrouter`
- `DELETE /api/v1/ai/credentials/openrouter`
- `POST /api/v1/ai/credentials/openrouter/test`
- `POST /api/v1/ai/chat` (SSE)

Auth uses an HttpOnly signed cookie (`ACCOUNT_SESSION_COOKIE`, default
`sq_session`). Login validates username/password against bcrypt hashes in
PostgreSQL; sessions are HMAC-signed tokens with TTL (`ACCOUNT_TOKEN_TTL`).
Trading-account API keys are encrypted at rest with AES-256-GCM
(`ACCOUNT_CREDENTIALS_KEY`) and never returned in list responses. Trading
account routes require a valid session cookie. Set
`ACCOUNT_COOKIE_SECURE=true` behind HTTPS. CORS must use an explicit
`GATEWAY_CORS_ORIGIN` and allows credentials.

The funding endpoint returns `{ "data": [...], "meta": {...} }` using the wire
shape consumed by the frontend. Decimal values are encoded as strings. The
response is a complete snapshot; filtering and sorting are performed in the
browser. The list omits settled history, supports gzip and returns an `ETag`;
clients can use `If-None-Match` to receive `304 Not Modified`. Recent settled
history is loaded from the per-instrument history endpoint on demand.

The internal API exposes `funding.v1.FundingService/ListFundingRates` and
`funding.v1.FundingService/GetFundingHistory`. Standard gRPC health is always
enabled; reflection is enabled only when `DEVELOPMENT=true`.

`report-service` exposes `report.v1.ReportService`. It reuses account-service
session validation for every user request and encodes all decimal values as
strings. Confirmed manual cash flows are included in daily PnL. In
`Asia/Shanghai`, scheduled equity samples run once per minute from 08:55
through 08:59 and the daily snapshot is idempotently finalized after 09:00:05.
Set the same dedicated `REPORT_INTERNAL_TOKEN` on account-service and
report-service. The sampler uses token-protected internal account RPCs, so it
does not depend on an expiring user session. Trade fills are incrementally
synchronized before each daily finalize.

`trader-service` exposes `trader.v1.TraderService` on `:9095` for CEX manual
orders. It asks account-service for owner-scoped decrypted credentials, then
places or cancels unified-account spot and linear perpetual orders on
Binance Portfolio Margin, OKX, Bybit UTA, Bitget UTA, and Gate unified
accounts. Orders are persisted with a unique `Idempotency-Key` and append-only
events. A timeout never blindly retries a place call; the service queries by
client order ID and may leave the order `unknown`. Each venue adapter paces
requests at 80ms. API keys must have read+trade permission, IP allowlists
where the venue supports them, and must not allow withdrawals. Start
Postgres and account-service before trader-service, then api-gateway. Gateway
readiness includes the trader health check.

The same service executes persistent TWAP jobs. Each job is bound to one
trading account and venue, uses deterministic child-order IDs, and is resumed
from PostgreSQL after restart. Maker slices use live venue BBO with post-only
orders and timeout cancellation; protected market slices use IOC limits.
`TRADER_TWAP_SCHEDULE_*` controls leasing and workers. Terminal jobs are kept
for `TRADER_TWAP_RETENTION` (seven days by default) and removed in bounded
batches configured by `TRADER_TWAP_CLEANUP_*`; child orders remain for audit.

Trader also runs persistent, bidirectional cross-venue arbitrage combinations.
The service shares public BBO WebSocket subscriptions across combinations,
rejects stale quotes, and supports `maker_then_hedge` and
`simultaneous_market`. Executions and child-order relationships are durable;
private account WebSockets provide the primary order/fill reports for Binance,
OKX, Bybit, Bitget, and Gate spot and linear perpetual orders. REST remains the
source of recovery after disconnects, uncertain submissions, and periodic
audits. After restart, non-terminal orders are reconciled by client order ID
before any new submit. Closing stops new triggers, cancels a remaining maker,
hedges actual base fills, and retains the combination audit trail for seven
days. Set `TRADER_ARBITRAGE_ORDER_STREAM_ENABLED=false` only as an operational
fallback to the legacy REST observer.
`TRADER_ORDER_STREAM_RECONNECT_*`, `TRADER_ORDER_STREAM_HEARTBEAT`,
`TRADER_ORDER_STREAM_STALE`, `TRADER_ORDER_STREAM_REST_AUDIT`, and
`TRADER_ORDER_STREAM_SESSION_IDLE` control reconnect and fallback behavior.
The `*_ORDER_WS_URL` variables are optional testnet/region overrides; leaving
them empty selects the product-correct private endpoint for each venue.

`TRADER_ARBITRAGE_DRY_RUN` defaults to `true`. In this mode the scheduler
subscribes, computes both spreads, and records edge-triggered signals without
claiming executions or creating orders. Keep it enabled through restart and
stale/reconnect validation. A live rollout must explicitly set it to `false`
and start with one combination on withdrawal-disabled test accounts at the
venue minimum `orderNotional`; verify partial-fill hedging and residual delta
before increasing `TRADER_ARBITRAGE_MAX_ACTIVE_*`.

`ai-service` exposes `ai.v1.AIService` on `:9096`. Per-user OpenRouter keys are
encrypted by account-service with the existing `ACCOUNT_CREDENTIALS_KEY`;
ai-service receives a key only while validating or calling the model. It owns
user-scoped conversation metadata and messages in PostgreSQL, retains active
conversations for seven days after their latest message, and deletes expired
rows hourly. The first provider alias `free-general` maps to `openrouter/free`.
Chat uses a bounded funding-rate snapshot plus a rolling conversation summary
and recent complete message pairs, then streams deltas through the gateway.
Configure retention and token budgets with the `AI_*` variables in
`.env.example`; each user may keep at most five active conversations and each
conversation stores at most 1,000 raw messages. Messages covered by a successful
rolling summary are deleted transactionally. User API keys never belong in
environment variables.

Official unified-account endpoints used by v1 (do not fall back to classic
account APIs):

| Venue | API | Place |
| --- | --- | --- |
| Binance Portfolio Margin | PAPI | spot `POST /papi/v1/margin/order` (`sideEffectType=NO_SIDE_EFFECT`); linear `POST /papi/v1/um/order` |
| OKX | v5 | `POST /api/v5/trade/order` (`tdMode=cash` spot, `tdMode=cross` swap) |
| Bybit | V5 UTA | `POST /v5/order/create` (`category=spot\|linear`; spot market `marketUnit=baseCoin`) |
| Bitget | UTA v3 | `POST /api/v3/trade/place-order` (`SPOT`, `USDT-FUTURES`, `USDC-FUTURES`) |
| Gate | v4 unified | spot `POST /api/v4/spot/orders` (`account=unified`); USDT linear `POST /api/v4/futures/usdt/orders` |

Override venue hosts with `BINANCE_PORTFOLIO_API_URL`, `OKX_ACCOUNT_API_URL`,
`BYBIT_ACCOUNT_API_URL`, `BITGET_ACCOUNT_API_URL`, and `GATE_ACCOUNT_API_URL`.
`TRADER_HTTP_TIMEOUT` defaults to `12s`. Demo/testnet hosts must be set
explicitly; unit tests use `httptest.Server` and never touch a live account.

Controlled smoke (human-gated, not part of `go test`):

1. Point the five `*_API_URL` values at demo/testnet or a minimum-privilege
   test account. Confirm unified-account mode, read+trade only, no withdraw.
2. From the manual trading page, place and then get/cancel one order per
   venue on testnet.
3. Only after that, and only with separately confirmed mainnet credentials,
   place the smallest allowed size for spot market, spot limit+cancel,
   perpetual market, and perpetual limit+cancel. Do not automate this step.

## Protobuf generation

Generated Go files are committed. To regenerate without a system `protoc`:

```bash
GOBIN="$HOME/.local/bin" go install github.com/bufbuild/buf/cmd/buf@latest
GOBIN="$HOME/.local/bin" go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
GOBIN="$HOME/.local/bin" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
PATH="$HOME/.local/bin:$PATH" buf generate
```

## Synchronization and storage

At startup the service hydrates its cache from PostgreSQL, refreshes live spot
and perpetual instruments, and backfills the available settled funding window
up to one year. Current funding and aggregates refresh every 10 minutes by
default (`SYNC_INTERVAL=10m`); instruments refresh independently every 8 hours
(`INSTRUMENT_SYNC_INTERVAL=8h`). A failed exchange/market retains its last
successful instrument set. API reads use an in-memory snapshot and never query
PostgreSQL. Adapter clients use proactive request pacing, cancellable batch
waits, `Retry-After`, jittered backoff, and a configurable timeout.

`global_symbol` is the uppercase concatenation of the real base and quote
assets after removing separators. It deliberately preserves `USDT`, `USDC`,
`USD`, and other quote distinctions.

## Verification

```bash
buf lint
buf generate
go test ./...
go vet ./...
```

Frontend verification from `web/`:

```bash
npm test
npm run lint
npm run build
```

The frontend should set `NEXT_PUBLIC_API_BASE_URL=http://localhost:8080` when
it is not proxying `/api` to the Gateway.

## Aggregate market-data service

`aggdata-service` reads `manifest.v1.json` and finalized SQREC shards from
`AGGDATA_RECORDING_DIR`, then maintains one authenticated connection to the MDS
Gateway. It exposes:

- `GET /healthz` and `GET /readyz`
- `GET /v1/markets`
- `GET /v1/markets/{symbol}/snapshot?profile={profile}&depth=20`
- `GET /v1/markets/{symbol}/spread-history?profile={profile}&range=24h&resolution=1m&type=gated`
- `WS /v1/stream`

`profile` is optional while a symbol belongs to only one aggregate profile.
Clients must provide it when spot and perpetual profiles share the same symbol.

Compose starts `aggdata-service` after the MDS consumer has created the
recording directory (`AGGDATA_RECORDING_DIR`, bind-mounted into the container).
For host debugging:

```bash
set -a; source .env; set +a
go run ./cmd/aggdata-service
```

The host `go run` default listener is `127.0.0.1:9093`; Compose binds `:9093`.
`MDS_GATEWAY_TOKEN` is used only on
the private upstream connection. If `AGGDATA_BROWSER_TOKEN` is set, REST
requests require `Authorization: Bearer ...`; browser WebSocket clients can
also pass `?token=...`. WebSocket origins must exactly match
`AGGDATA_ALLOWED_ORIGINS`. `AGGDATA_STREAM_INTERVAL` defaults to `50ms` and
accepts values down to `10ms`; it does not change the recorder's 200 ms sampling.

When `AGGDATA_FAIRPRICE_ENABLED=true`, each aggorderbook update also produces a
generic in-memory fair price. It does not read BBO and does not contain
Polymarket probability conversion, YES/NO pairing, inventory skew, or quoting
logic. Defaults are 20 levels, `0.10/bp` exponential depth decay, imbalance
alpha 1, and a 500 ms price-space EWMA. A crossed book is deterministically
virtually uncrossed before pricing; successful uncrossing is published with
`degraded=true` and `book_crossed`, while exhausted depth sends a reset.

Fair prices use their own 5 ms writer ticker
(`AGGDATA_FAIRPRICE_STREAM_INTERVAL`, minimum 1 ms). This bounds aggdata's
additional conflation delay, not end-to-end market freshness: the upstream MDS
Gateway currently publishes its latest image at up to 50 ms intervals.

WebSocket control messages are JSON:

```json
{"op":"subscribe","symbol":"BTCUSDT","channel":"orderbook","depth":20}
```

Fair price subscriptions omit depth:

```json
{"op":"subscribe","profile":"agg_binance","symbol":"BTCUSDT","channel":"fairprice"}
```

`profile` is optional when the symbol exists in exactly one profile and is
required to disambiguate duplicate symbols.

After the acknowledgement, the current image is sent as a text JSON frame and
is repeated as a heartbeat (default 1 second) when no new book arrives:

```json
{
  "type": "data",
  "channel": "fairprice",
  "profile": "agg_binance",
  "symbol": "BTCUSDT",
  "model_id": "fp-v1:...",
  "ring_epoch": "18446744073709551615",
  "seq": "42",
  "generation": "42",
  "wall_ns": "17865120000000001",
  "exchange_ts_ns": "17865119999900000",
  "price": {"mantissa": "6812345678", "scale": 5},
  "price_raw": {"mantissa": "6812345600", "scale": 5},
  "mid": {"mantissa": "6812345500", "scale": 5},
  "microprice": {"mantissa": "6812345580", "scale": 5},
  "degraded": false,
  "degraded_reasons": []
}
```

All `uint64` identifiers and nanosecond timestamps are JSON strings. Prices are
fixed-point objects. The full frame also includes original/effective best
prices, imbalance, spread/depth metrics, optional impact prices, active venue
mask, and crossed-book provenance. Reset frames use
`{"op":"reset","channel":"fairprice","symbol":"...","reason":"..."}`.
Subscribers should use local receive time for connection silence and `wall_ns`
for source age. A ring epoch change is new data even when `seq` restarts lower.

Ready persisted samples are available from:

```http
GET /v1/markets/BTCUSDT/fair-price-history?profile=agg_binance&start=2026-08-13T13:40:00Z&end=2026-08-13T13:45:00Z&resolution=auto
```

History is limited to 24 hours and 2,000 ascending points. Supported
resolutions are `auto`, `1s`, `5s`, `15s`, and `1m`; downsampling selects the
latest sample in each PostgreSQL time bucket and only returns the current
`model_id`. A database outage returns `503` for this endpoint without changing
real-time WebSocket or readiness behavior.

With `AGGDATA_FAIRPRICE_PERSIST_ENABLED=true`, aggdata samples each ready fair
price once per UTC second into `fair_price_snapshots` and retains 24 hours by
default. Quiet markets intentionally repeat the same `(ring_epoch,ring_sequence)`
each second; a missing row means fair price was not ready. PostgreSQL
`NUMERIC(38,18)` values are generated directly from mantissa/scale without a
`float64` round trip. `DATABASE_URL` is reused, and its role needs DDL
permission for embedded migrations. Database connection, migration, writes,
and cleanup run in a retrying background manager; failures never affect
`/healthz`, `/readyz`, fair computation, or WebSocket publishing. Set
`AGGDATA_FAIRPRICE_PERSIST_ENABLED=false` to avoid any database connection.

Data images use a compact little-endian `SQAB` binary frame. The 42-byte header
is: magic `u32`, version `u8,u8`, kind `u8`, flags `u8`, header bytes `u16`,
payload bytes `u32`, symbol length `u8`, price scale `u8`, quantity scale `u8`,
venue count `u8`, ring sequence `u64`, generation `u64`, and wall time `u64`.
The payload begins with symbol bytes and venue IDs. BBO sides and book levels
store price `i64`, quantity `i64`, venue mask `u32`, contributor count `u8`,
then only mask-selected `(slot u8, quantity i64)` pairs. BBO frames append raw
sides and raw/gated spread BPS as little-endian `f64`; book frames append
bid/ask counts and levels.
Mantissas are paired with the header decimal scales.

Terminate TLS at a reverse proxy; never expose the plaintext MDS Gateway:

```nginx
location /aggdata/ {
    proxy_pass http://127.0.0.1:9093/;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
}
```

For coexistence with latency-sensitive MDS processes, keep
`AGGDATA_HISTORY_WORKERS` low (the default is one) and set `GOMEMLIMIT`/`GOGC`
for the deployment's memory budget.
