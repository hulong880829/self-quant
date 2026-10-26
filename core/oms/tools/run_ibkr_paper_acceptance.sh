#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
compose="$root/backend/deploy/docker-compose.yml"
env_file="${SELF_QUANT_ENV_FILE:-$root/backend/.env}"
binary="${OMS_IBKR_ACCEPTANCE_BIN:-$root/core/build-release/oms/oms_ibkr_acceptance}"
compose_args=(-f "$compose")
if [[ -f "$env_file" ]]; then
  compose_args=(--env-file "$env_file" "${compose_args[@]}")
fi

if [[ -z "${IBKR_PAPER_TWS_USERID:-}" ||
      -z "${IBKR_PAPER_TWS_PASSWORD:-}" ||
      -z "${IBKR_PAPER_ACCOUNT:-}" ||
      -z "${IBKR_PAPER_CON_ID:-}" ]]; then
  echo "SKIPPED: paper credentials, DU account, or con-id are unavailable"
  exit 0
fi

if [[ "${IBKR_PAPER_ACCOUNT}" != DU* ]]; then
  echo "error: IBKR_PAPER_ACCOUNT must be a DU paper account" >&2
  exit 2
fi
if [[ ! -x "$binary" ]]; then
  echo "error: missing acceptance binary $binary" >&2
  exit 2
fi

cleanup() {
  docker compose "${compose_args[@]}" --profile ibkr-paper rm -sf ibkr-paper \
    >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

cleanup
docker compose "${compose_args[@]}" --profile ibkr-paper up -d ibkr-paper

ready=false
for _ in $(seq 1 120); do
  if (exec 3<>/dev/tcp/127.0.0.1/4102) 2>/dev/null; then
    exec 3>&-
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  echo "SKIPPED: paper gateway did not expose port 4102 (login/2FA may be pending)"
  exit 0
fi

"$binary" \
  --host 127.0.0.1 \
  --service 4102 \
  --account "$IBKR_PAPER_ACCOUNT" \
  --con-id "$IBKR_PAPER_CON_ID" \
  --security "${IBKR_PAPER_SECURITY:-STK}" \
  --client-id "${IBKR_PAPER_CLIENT_ID:-32}" \
  --limit "${IBKR_PAPER_LIMIT:-0.01}"
