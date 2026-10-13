#!/usr/bin/env bash
# Prepare a host env file and shared container env. Does not start containers.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
backend_dir="$(cd -- "${script_dir}/../.." && pwd)"
# shellcheck source=envlib.sh
source "${script_dir}/envlib.sh"

requested="auto"
env_file="${backend_dir}/.env"
secrets_dir="${backend_dir}/deploy/secrets"
compose_file="${backend_dir}/deploy/docker-compose.yml"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode)
      requested="${2:-}"
      shift 2
      ;;
    --env-file)
      env_file="${2:-}"
      shift 2
      ;;
    --secrets-dir)
      secrets_dir="${2:-}"
      shift 2
      ;;
    --compose-file)
      compose_file="${2:-}"
      shift 2
      ;;
    *)
      echo "unknown argument: $1" >&2
      exit 2
      ;;
  esac
done

case "$requested" in
  auto|new|upgrade|restore) ;;
  *)
    echo "mode must be auto, new, upgrade, or restore" >&2
    exit 2
    ;;
esac

for command in docker openssl python3; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "missing required command: ${command}" >&2
    exit 1
  }
done
docker compose version >/dev/null 2>&1 || {
  echo "missing required command: docker compose" >&2
  exit 1
}
if [[ ! -f "$env_file" ]]; then
  echo "missing ${env_file}; copy .env.example or a backup first" >&2
  exit 1
fi

if [[ "$requested" == "restore" ]]; then
  mode="restore"
else
  mode="$("${script_dir}/resolve-compose-mode.sh" \
    --requested "$requested" \
    --env-file "$env_file" \
    --compose-file "$compose_file")"
fi
echo "selected mode: ${mode}"

if [[ "$mode" == "new" ]]; then
  dry_run="$(env_value "$env_file" TRADER_ARBITRAGE_DRY_RUN)"
  if [[ "$dry_run" != "true" ]]; then
    echo "new deployments require TRADER_ARBITRAGE_DRY_RUN=true" >&2
    exit 1
  fi
fi

bash "${script_dir}/init-auth-secrets.sh" \
  --mode "$mode" \
  --env-file "$env_file" \
  --secrets-dir "$secrets_dir"
bash "${script_dir}/prepare-compose-env.sh" \
  "$env_file" \
  "$(dirname "$compose_file")/env/container.shared.env"

cert_dir="$(dirname "$compose_file")/clickhouse/certs"
if [[ ! -f "${cert_dir}/server.crt" || ! -f "${cert_dir}/server.key" ]]; then
  bash "$(dirname "$compose_file")/clickhouse/generate-certs.sh"
else
  echo "kept ClickHouse certificates"
fi

bash "${script_dir}/validate-compose-env.sh" \
  --level structural \
  --env-file "$env_file" \
  --compose-file "$compose_file" \
  --shared-env "$(dirname "$compose_file")/env/container.shared.env" \
  --secrets-dir "$secrets_dir"

cat <<'EOF'
Bootstrap finished without starting services and without changing live trading.
Next:
  make validate-compose
  docker compose --env-file .env -f deploy/docker-compose.yml up -d --build
Do not start the stack until validate-compose passes.
A machine with only HTTP and an IP address should keep the development template;
ACCOUNT_COOKIE_SECURE=true requires HTTPS.
EOF
