#!/usr/bin/env bash
# Initialize or preserve deploy secrets. Never print secret values.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=envlib.sh
source "${script_dir}/envlib.sh"

mode=""
env_file=".env"
secrets_dir="deploy/secrets"

usage() {
  echo "usage: init-auth-secrets.sh --mode new|upgrade|restore [--env-file FILE] [--secrets-dir DIR]" >&2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode)
      mode="${2:-}"
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
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
done

case "$mode" in
  new|upgrade|restore) ;;
  *)
    echo "mode must be new, upgrade, or restore" >&2
    exit 2
    ;;
esac

mkdir -p "$(dirname "$env_file")" "$secrets_dir"
touch "$env_file"

status_kept() {
  echo "kept $1"
}

status_generated() {
  echo "generated $1"
}

migrate_clickhouse_password() {
  local old new
  old="$(env_value "$env_file" CLICKHOUSE_BBO_PASSWORD)"
  new="$(env_value "$env_file" CLICKHOUSE_PASSWORD)"
  if [[ -n "$old" && -z "$new" ]]; then
    set_env_value "$env_file" CLICKHOUSE_PASSWORD "$old"
    echo "warning: CLICKHOUSE_BBO_PASSWORD is deprecated; copied it to CLICKHOUSE_PASSWORD" >&2
  elif [[ -n "$old" && "$old" == "$new" ]]; then
    echo "warning: CLICKHOUSE_BBO_PASSWORD is deprecated and matches CLICKHOUSE_PASSWORD" >&2
  elif [[ -n "$old" && -n "$new" && "$old" != "$new" ]]; then
    echo "CLICKHOUSE_PASSWORD and CLICKHOUSE_BBO_PASSWORD differ; refusing to guess" >&2
    exit 1
  fi
}

ensure_credentials_key() {
  local current
  current="$(env_value "$env_file" ACCOUNT_CREDENTIALS_KEY)"
  if is_legacy_credentials_key "$current"; then
    if [[ "$mode" == "new" ]]; then
      set_env_value "$env_file" ACCOUNT_CREDENTIALS_KEY "$(random_hex)"
      status_generated ACCOUNT_CREDENTIALS_KEY
      return
    fi
    echo "warning: ACCOUNT_CREDENTIALS_KEY is the published example value; kept it because existing credentials may depend on it" >&2
    status_kept ACCOUNT_CREDENTIALS_KEY
    return
  fi
  if is_placeholder "$current"; then
    if [[ "$mode" == "new" ]]; then
      set_env_value "$env_file" ACCOUNT_CREDENTIALS_KEY "$(random_hex)"
      status_generated ACCOUNT_CREDENTIALS_KEY
      return
    fi
    echo "ACCOUNT_CREDENTIALS_KEY is required in $mode mode and must not be generated" >&2
    exit 1
  fi
  if ! is_valid_credentials_key "$current"; then
    echo "ACCOUNT_CREDENTIALS_KEY is not a valid 32-byte AES key" >&2
    exit 1
  fi
  status_kept ACCOUNT_CREDENTIALS_KEY
}

ensure_token_secret() {
  local current
  current="$(env_value "$env_file" ACCOUNT_TOKEN_SECRET)"
  if is_placeholder "$current"; then
    case "$mode" in
      new)
        set_env_value "$env_file" ACCOUNT_TOKEN_SECRET "$(random_hex)"
        status_generated ACCOUNT_TOKEN_SECRET
        ;;
      upgrade)
        set_env_value "$env_file" ACCOUNT_TOKEN_SECRET "$(random_hex)"
        echo "warning: generated ACCOUNT_TOKEN_SECRET; existing login sessions will be invalidated" >&2
        status_generated ACCOUNT_TOKEN_SECRET
        ;;
      restore)
        echo "ACCOUNT_TOKEN_SECRET must be restored from backup and must not be generated" >&2
        exit 1
        ;;
    esac
    return
  fi
  if [[ ${#current} -lt 32 ]]; then
    echo "ACCOUNT_TOKEN_SECRET must be at least 32 bytes" >&2
    exit 1
  fi
  status_kept ACCOUNT_TOKEN_SECRET
}

ensure_internal_token() {
  local key="$1"
  local current
  current="$(env_value "$env_file" "$key")"
  if is_placeholder "$current"; then
    local value=""
    local attempt
    for attempt in 1 2 3 4 5 6 7 8; do
      value="$(random_hex)"
      local other key_name other_value
      local distinct=true
      for key_name in GATEWAY_INTERNAL_TOKEN ADMIN_INTERNAL_TOKEN REPORT_INTERNAL_TOKEN TRADER_INTERNAL_TOKEN; do
        if [[ "$key_name" == "$key" ]]; then
          continue
        fi
        other_value="$(env_value "$env_file" "$key_name")"
        if [[ -n "$other_value" && "$other_value" == "$value" ]]; then
          distinct=false
        fi
      done
      if [[ "$distinct" == true ]]; then
        break
      fi
      value=""
    done
    if [[ -z "$value" ]]; then
      echo "failed to generate a distinct $key" >&2
      exit 1
    fi
    set_env_value "$env_file" "$key" "$value"
    if [[ "$mode" == "restore" ]]; then
      echo "warning: generated $key; every caller must use this env file" >&2
    fi
    status_generated "$key"
    return
  fi
  if [[ ${#current} -lt 32 ]]; then
    echo "$key must be at least 32 bytes" >&2
    exit 1
  fi
  status_kept "$key"
}

ensure_postgres_password() {
  local current
  current="$(env_value "$env_file" POSTGRES_PASSWORD)"
  if is_placeholder "$current"; then
    case "$mode" in
      new)
        set_env_value "$env_file" POSTGRES_PASSWORD "$(random_hex)"
        status_generated POSTGRES_PASSWORD
        ;;
      upgrade)
        set_env_value "$env_file" POSTGRES_PASSWORD "selfquant"
        echo "warning: POSTGRES_PASSWORD was unset; preserved the current PostgreSQL volume password" >&2
        status_generated POSTGRES_PASSWORD
        ;;
      restore)
        echo "POSTGRES_PASSWORD must be restored from backup and must not be generated" >&2
        exit 1
        ;;
    esac
    return
  fi
  status_kept POSTGRES_PASSWORD
}

ensure_clickhouse_password() {
  local current
  current="$(env_value "$env_file" CLICKHOUSE_PASSWORD)"
  if is_placeholder "$current"; then
    if [[ "$mode" == "new" ]]; then
      set_env_value "$env_file" CLICKHOUSE_PASSWORD "$(random_hex)"
      status_generated CLICKHOUSE_PASSWORD
      return
    fi
    echo "CLICKHOUSE_PASSWORD is required in $mode mode and must not be generated" >&2
    exit 1
  fi
  status_kept CLICKHOUSE_PASSWORD
}

sync_database_url() {
  local url password
  url="$(env_value "$env_file" DATABASE_URL)"
  password="$(env_value "$env_file" POSTGRES_PASSWORD)"
  if [[ -z "$url" ]]; then
    echo "DATABASE_URL is required" >&2
    exit 1
  fi
  local result
  if ! result="$(python3 - "$url" "$password" <<'PY'
import sys
from urllib.parse import quote, urlparse, urlunparse
url, password = sys.argv[1], sys.argv[2]
parsed = urlparse(url)
if parsed.hostname != "localhost" or parsed.port != 5432:
    print("DATABASE_URL must use @localhost:5432 on the host", file=sys.stderr)
    sys.exit(2)
current = parsed.password or ""
if parsed.username == "selfquant" and current == "selfquant":
    netloc = f"selfquant:{quote(password, safe='')}@localhost:{parsed.port}"
    print(urlunparse(parsed._replace(netloc=netloc)))
    sys.exit(0)
if current != password:
    print("DATABASE_URL password does not match POSTGRES_PASSWORD", file=sys.stderr)
    sys.exit(3)
print(url)
PY
)"; then
    exit 1
  fi
  if [[ "$result" != "$url" ]]; then
    set_env_value "$env_file" DATABASE_URL "$result"
    echo "updated DATABASE_URL host password segment"
  fi
}

require_distinct_tokens() {
  python3 - "$env_file" <<'PY'
import sys
path = sys.argv[1]
keys = [
    "GATEWAY_INTERNAL_TOKEN",
    "ADMIN_INTERNAL_TOKEN",
    "REPORT_INTERNAL_TOKEN",
    "TRADER_INTERNAL_TOKEN",
]
values = {}
with open(path, encoding="utf-8") as handle:
    for line in handle:
        for key in keys:
            prefix = key + "="
            if line.startswith(prefix):
                values[key] = line[len(prefix):].rstrip("\n")
missing = [key for key in keys if not values.get(key)]
if missing:
    print("missing internal tokens: " + ", ".join(missing), file=sys.stderr)
    sys.exit(1)
seen = {}
for key, value in values.items():
    if value in seen:
        print(f"{key} duplicates {seen[value]}", file=sys.stderr)
        sys.exit(1)
    seen[value] = key
PY
}

ensure_jwt() {
  local priv="${secrets_dir}/broker-jwt-ed25519.pem"
  local pub="${secrets_dir}/broker-jwt-ed25519.pub"
  if [[ ! -f "$priv" ]]; then
    if [[ "$mode" != "new" ]]; then
      echo "broker jwt private key is required in $mode mode" >&2
      exit 1
    fi
    openssl genpkey -algorithm Ed25519 -out "$priv"
    status_generated "broker jwt private key"
  else
    status_kept "broker jwt private key"
  fi
  if [[ ! -f "$pub" ]]; then
    openssl pkey -in "$priv" -pubout -out "$pub"
    status_generated "broker jwt public key"
  else
    local derived
    derived="$(mktemp)"
    openssl pkey -in "$priv" -pubout -out "$derived"
    if ! cmp -s "$derived" "$pub"; then
      rm -f "$derived"
      echo "broker jwt public key does not match the private key" >&2
      exit 1
    fi
    rm -f "$derived"
    status_kept "broker jwt public key"
  fi
  chmod 0600 "$priv"
  chmod 0644 "$pub"
}

migrate_clickhouse_password
ensure_credentials_key
ensure_token_secret
ensure_internal_token GATEWAY_INTERNAL_TOKEN
ensure_internal_token ADMIN_INTERNAL_TOKEN
ensure_internal_token REPORT_INTERNAL_TOKEN
ensure_internal_token TRADER_INTERNAL_TOKEN
ensure_postgres_password
ensure_clickhouse_password
sync_database_url
require_distinct_tokens
ensure_jwt
