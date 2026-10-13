# Backup and restore

A usable recovery needs the database and the key that decrypts account credentials. Either piece alone is not a backup.

## What to copy

- PostgreSQL dump
- ClickHouse data directory or volume snapshot
- `backend/.env`
- `ACCOUNT_CREDENTIALS_KEY`
- `ACCOUNT_TOKEN_SECRET`
- `GATEWAY_INTERNAL_TOKEN`, `ADMIN_INTERNAL_TOKEN`, `REPORT_INTERNAL_TOKEN`, and `TRADER_INTERNAL_TOKEN`
- Broker JWT private key at `backend/deploy/secrets/broker-jwt-ed25519.pem`
- `POSTGRES_PASSWORD` and `CLICKHOUSE_PASSWORD`

PostgreSQL rows plus `ACCOUNT_CREDENTIALS_KEY` are what make stored trading-account credentials readable again. Do not generate a new credentials key during restore.

Changing `POSTGRES_PASSWORD` in `.env` does not change the password inside an existing Postgres volume. Rotate it only as a separate step: `ALTER ROLE` in the database first, then update `.env`. The same constraint applies to an existing ClickHouse user password.

## Restore order

1. Stop the business services.
2. Restore PostgreSQL.
3. Restore the original `ACCOUNT_CREDENTIALS_KEY`.
4. Restore the Broker JWT private key.
5. Restore or migrate ClickHouse.
6. From `backend/`, run `make bootstrap-compose-restore`.
7. Let ClickHouse migration run. It is idempotent and does not delete or rewrite existing rows.
8. Start with `TRADER_ARBITRAGE_DRY_RUN=true` and check accounts, portfolios, and positions.
9. Turn on live arbitrage order execution only by setting `TRADER_ARBITRAGE_DRY_RUN=false`.

`make bootstrap-compose` never selects restore. If only one of the Postgres and ClickHouse volumes exists, bootstrap fails and prints which volume is present and which is missing. Repair or remove that orphan manually. Do not expect another bootstrap target to skip the check.

## What Compose does not deploy

- C++ MDS and Fairprice stay on the host. Build them with `./scripts/bootstrap.sh` and `./scripts/build.sh`.
- IBKR dependencies need `./scripts/bootstrap-ibkr-deps.sh --accept-ib-license`. The default Intel decimal library is downloaded and checked; it is not read from `third_party/intel-rdfp`.
- MDS and Fairprice local YAML is prepared on the host. `password_env` is `CLICKHOUSE_PASSWORD`.
- The web profile is optional. Without `--profile web`, run the frontend on the host. Browser-facing `NEXT_PUBLIC_*` URLs must be reachable by the browser, including Aggdata and Fairprice.
- A host that only has an IP address and HTTP should stay on the development template. `ACCOUNT_COOKIE_SECURE=true` requires HTTPS.

`make validate-compose` is read-only. `make bootstrap-compose` does not start containers and does not flip live trading on.
