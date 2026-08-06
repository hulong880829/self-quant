# SelfQuant funding backend

Two Go processes expose perpetual-futures funding data from Binance, OKX, Bybit,
Bitget, Gate.io and Hyperliquid. PostgreSQL is the only infrastructure
dependency; migrations are embedded into `funding-service` and run
idempotently at startup.

## Run locally

Requirements: Go 1.25+ and PostgreSQL 17+.

```bash
cp .env.example .env
set -a; source .env; set +a
go run ./cmd/funding-service
```

In a second terminal:

```bash
set -a; source .env; set +a
go run ./cmd/api-gateway
```

The optional `deploy/docker-compose.yml` starts PostgreSQL only:

```bash
docker compose -f deploy/docker-compose.yml up -d
```

HTTP endpoints:

- `GET /health/live`
- `GET /health/ready`
- `GET /api/v1/funding-rates`

The funding endpoint returns `{ "data": [...], "meta": {...} }` using the wire
shape consumed by the frontend. Decimal values are encoded as strings. The
response is a complete snapshot; filtering and sorting are performed in the
browser.

The internal API contains exactly one business RPC,
`funding.v1.FundingService/ListFundingRates`. Standard gRPC health is always
enabled; reflection is enabled only when `DEVELOPMENT=true`.

## Protobuf generation

Generated Go files are committed. To regenerate without a system `protoc`:

```bash
GOBIN="$HOME/.local/bin" go install github.com/bufbuild/buf/cmd/buf@latest
GOBIN="$HOME/.local/bin" go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
GOBIN="$HOME/.local/bin" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
PATH="$HOME/.local/bin:$PATH" buf generate
```

## Synchronization and storage

At startup the service synchronizes instruments, current rates and enough
settled records to calculate seven-day cumulative funding (the API returns the
latest ten). Current snapshots refresh every 10 seconds by default; instrument
metadata and history refresh hourly. A failed exchange retains its last
successful rows, which are exposed as stale rather than deleting the snapshot.
Adapter clients use bounded concurrency, retries with jittered backoff, and a
configurable timeout.

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

The frontend should set `NEXT_PUBLIC_API_BASE_URL=http://localhost:8080` when
it is not proxying `/api` to the Gateway.
