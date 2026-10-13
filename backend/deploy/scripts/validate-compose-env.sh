#!/usr/bin/env bash
# Read-only compose environment checks. Never print secret values.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
backend_dir="$(cd -- "${script_dir}/../.." && pwd)"
# shellcheck source=envlib.sh
source "${script_dir}/envlib.sh"

level="final"
env_file="${backend_dir}/.env"
compose_file="${backend_dir}/deploy/docker-compose.yml"
shared_env="${backend_dir}/deploy/env/container.shared.env"
secrets_dir="${backend_dir}/deploy/secrets"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --level)
      level="${2:-}"
      shift 2
      ;;
    --env-file)
      env_file="${2:-}"
      shift 2
      ;;
    --compose-file)
      compose_file="${2:-}"
      shift 2
      ;;
    --shared-env)
      shared_env="${2:-}"
      shift 2
      ;;
    --secrets-dir)
      secrets_dir="${2:-}"
      shift 2
      ;;
    *)
      echo "unknown argument: $1" >&2
      exit 2
      ;;
  esac
done

case "$level" in
  structural|final) ;;
  *)
    echo "level must be structural or final" >&2
    exit 2
    ;;
esac

errors=()
fail() {
  errors+=("$1")
}

if [[ ! -f "$env_file" ]]; then
  echo ".env is missing" >&2
  exit 1
fi
if [[ ! -f "$compose_file" ]]; then
  echo "compose file is missing" >&2
  exit 1
fi

value() {
  env_value "$env_file" "$1"
}

required_names="$(python3 - "$compose_file" <<'PY'
import re, sys
text = open(sys.argv[1], encoding="utf-8").read()
found = []
for match in re.finditer(r"\$\{([A-Za-z_][A-Za-z0-9_]*):\?", text):
    name = match.group(1)
    if name not in found:
        found.append(name)
print("\n".join(found))
PY
)"

while IFS= read -r name; do
  [[ -n "$name" ]] || continue
  current="$(value "$name")"
  if is_placeholder "$current"; then
    fail "${name} is missing or still a placeholder"
  fi
done <<<"$required_names"

if grep -q '\${DATABASE_URL' "$compose_file"; then
  fail "compose must not interpolate the host DATABASE_URL"
fi
if grep -q 'CLICKHOUSE_BBO_PASSWORD' "$compose_file"; then
  fail "compose still references CLICKHOUSE_BBO_PASSWORD"
fi

credentials="$(value ACCOUNT_CREDENTIALS_KEY)"
if is_legacy_credentials_key "$credentials"; then
  echo "warning: ACCOUNT_CREDENTIALS_KEY is the published example value; left unchanged" >&2
elif is_placeholder "$credentials" || ! is_valid_credentials_key "$credentials"; then
  fail "ACCOUNT_CREDENTIALS_KEY is missing or not a 32-byte AES key"
fi

token_secret="$(value ACCOUNT_TOKEN_SECRET)"
if is_placeholder "$token_secret" || [[ ${#token_secret} -lt 32 ]]; then
  fail "ACCOUNT_TOKEN_SECRET is missing or shorter than 32 bytes"
fi

postgres_password="$(value POSTGRES_PASSWORD)"
clickhouse_password="$(value CLICKHOUSE_PASSWORD)"
if is_placeholder "$postgres_password"; then
  fail "POSTGRES_PASSWORD is missing or still a placeholder"
fi
if is_placeholder "$clickhouse_password"; then
  fail "CLICKHOUSE_PASSWORD is missing or still a placeholder"
fi

database_url="$(value DATABASE_URL)"
python3 - "$database_url" "$postgres_password" <<'PY' || fail "DATABASE_URL must use @localhost:5432 and the same password as POSTGRES_PASSWORD"
import sys
from urllib.parse import urlparse
url, password = sys.argv[1], sys.argv[2]
parsed = urlparse(url)
if parsed.hostname != "localhost" or parsed.port != 5432 or (parsed.password or "") != password:
    sys.exit(1)
PY

python3 - "$env_file" <<'PY' || fail "internal service tokens must be pairwise distinct"
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
if len(values) != 4 or any(not values[key] for key in keys):
    sys.exit(1)
if len(set(values.values())) != 4:
    sys.exit(1)
PY

dry_run="$(value TRADER_ARBITRAGE_DRY_RUN)"
case "$dry_run" in
  true|false) ;;
  *) fail "TRADER_ARBITRAGE_DRY_RUN must be true or false" ;;
esac

development="$(value DEVELOPMENT)"
cors="$(value GATEWAY_CORS_ORIGIN)"
cookie="$(value ACCOUNT_COOKIE_SECURE)"
if [[ "$development" == "false" && "$cors" == *example.com* ]]; then
  if [[ "$level" == "structural" ]]; then
    echo "warning: GATEWAY_CORS_ORIGIN still uses example.com; change it before the final validation" >&2
  else
    fail "GATEWAY_CORS_ORIGIN still uses the example.com placeholder"
  fi
fi
if [[ "$level" == "final" && "$development" == "false" ]]; then
  if [[ "$cookie" != "true" ]]; then
    fail "ACCOUNT_COOKIE_SECURE must be true when DEVELOPMENT=false"
  fi
  if [[ "$cors" == *localhost* || "$cors" == *127.0.0.1* ]]; then
    fail "GATEWAY_CORS_ORIGIN must not use localhost when DEVELOPMENT=false"
  fi
fi

priv="${secrets_dir}/broker-jwt-ed25519.pem"
pub="${secrets_dir}/broker-jwt-ed25519.pub"
if [[ ! -f "$priv" || ! -f "$pub" ]]; then
  fail "broker jwt private and public keys are required"
else
  derived="$(mktemp)"
  normalized="$(mktemp)"
  openssl pkey -in "$priv" -pubout -out "$derived" 2>/dev/null || fail "broker jwt private key is unreadable"
  openssl pkey -pubin -in "$pub" -pubout -out "$normalized" 2>/dev/null || fail "broker jwt public key is unreadable"
  if [[ -s "$derived" && -s "$normalized" ]] && ! cmp -s "$derived" "$normalized"; then
    fail "broker jwt public key does not match the private key"
  fi
  rm -f "$derived" "$normalized"
fi

if [[ ! -f "$shared_env" ]]; then
  fail "container.shared.env is missing; run bootstrap before validation"
else
  if awk -F= '
    /^[[:space:]]*#/ || NF == 0 { next }
    {
      key=$1
      if (key ~ /(TOKEN|SECRET|KEY|PASSWORD)$/) {
        print key
        exit 1
      }
    }
  ' "$shared_env"; then
    :
  else
    fail "container.shared.env contains a sensitive variable"
  fi
fi

if [[ ${#errors[@]} -gt 0 ]]; then
  printf '%s\n' "${errors[@]}" >&2
  exit 1
fi

docker compose --env-file "$env_file" -f "$compose_file" config --quiet
docker compose --env-file "$env_file" -f "$compose_file" config --format json | python3 -c '
import json, sys
from urllib.parse import urlparse
document = json.load(sys.stdin)
for name, service in (document.get("services") or {}).items():
    environment = service.get("environment") or {}
    if isinstance(environment, list):
        mapped = {}
        for item in environment:
            if "=" in item:
                key, value = item.split("=", 1)
                mapped[key] = value
        environment = mapped
    url = environment.get("DATABASE_URL")
    if not url:
        continue
    if urlparse(url).hostname != "postgres":
        print(f"{name} DATABASE_URL must use the postgres service", file=sys.stderr)
        sys.exit(1)
'
echo "validated compose env (${level})"
