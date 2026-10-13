#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
backend_dir="$(cd -- "${script_dir}/../.." && pwd)"
root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT

bin="${root}/bin"
mkdir -p "$bin"
present="${root}/volumes"
: >"$present"

cat >"${bin}/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "compose" ]]; then
  project="${COMPOSE_PROJECT_NAME:-deploy}"
  env_file=""
  args=("$@")
  for ((i = 0; i < ${#args[@]}; i++)); do
    if [[ "${args[$i]}" == "-p" ]]; then
      project="${args[$((i + 1))]}"
    fi
    if [[ "${args[$i]}" == "--env-file" ]]; then
      env_file="${args[$((i + 1))]}"
    fi
  done
  if [[ "$*" == *"config"* ]]; then
    if [[ -z "$env_file" ]] || ! python3 - "$env_file" <<'PY'
import sys
value = None
with open(sys.argv[1], encoding="utf-8") as handle:
    for line in handle:
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, raw = line.split("=", 1)
        if key == "POSTGRES_PASSWORD":
            value = raw.rstrip("\r\n")
if value is None or value == "":
    sys.exit(1)
PY
    then
      echo "POSTGRES_PASSWORD is missing or empty" >&2
      exit 1
    fi
    if [[ "$*" == *"--quiet"* ]]; then
      exit 0
    fi
    cat <<JSON
{"name":"${project}","volumes":{"selfquant-postgres":{"name":"${project}_selfquant-postgres"},"clickhouse-data":{"name":"${project}_clickhouse-data"}},"services":{"funding-service":{"environment":{"DATABASE_URL":"postgres://selfquant:secret@postgres:5432/selfquant?sslmode=disable"}}}}
JSON
    exit 0
  fi
  echo "unexpected compose invocation: $*" >&2
  exit 1
fi
if [[ "${1:-}" == "volume" && "${2:-}" == "inspect" ]]; then
  if grep -qx "$3" "${FAKE_PRESENT_VOLUMES:?}"; then
    exit 0
  fi
  exit 1
fi
if [[ "${1:-}" == "volume" && "${2:-}" == "ls" ]]; then
  exit 0
fi
echo "unexpected docker invocation: $*" >&2
exit 1
EOF
chmod 755 "${bin}/docker"

export PATH="${bin}:${PATH}"
export FAKE_PRESENT_VOLUMES="$present"
export COMPOSE_PROJECT_NAME="ci-bootstrap"

compose_file="${backend_dir}/deploy/docker-compose.yml"
env_file="${root}/.env"
secrets="${root}/secrets"

assert_eq() {
  local got="$1" want="$2" label="$3"
  if [[ "$got" != "$want" ]]; then
    echo "${label}: got ${got}, want ${want}" >&2
    exit 1
  fi
}

mode_of() {
  "${script_dir}/resolve-compose-mode.sh" \
    --requested "$1" \
    --env-file "$env_file" \
    --compose-file "$compose_file"
}

echo "postgres=missing" >"$env_file"
report="$("${script_dir}/compose-volumes.sh" --env-file "$env_file" --compose-file "$compose_file")"
assert_eq "$(printf '%s\n' "$report" | awk -F= '$1=="selfquant-postgres_name"{print $2}')" \
  "ci-bootstrap_selfquant-postgres" "rendered postgres volume"
if printf '%s\n' "$report" | grep -q 'deploy_selfquant-postgres'; then
  echo "volume detection hardcoded the deploy_ prefix" >&2
  exit 1
fi
assert_eq "$(mode_of auto)" "new" "empty volumes"

printf '%s\n' "ci-bootstrap_selfquant-postgres" >"$present"
set +e
mixed="$(mode_of auto 2>"${root}/mixed.err")"
mixed_status=$?
set -e
if [[ "$mixed_status" -eq 0 ]]; then
  echo "single volume auto mode succeeded: ${mixed}" >&2
  exit 1
fi
grep -q "ci-bootstrap_selfquant-postgres" "${root}/mixed.err"
grep -q "ci-bootstrap_clickhouse-data" "${root}/mixed.err"
if grep -Eq 'bootstrap-compose-new|bootstrap-compose-upgrade' "${root}/mixed.err"; then
  echo "single volume error suggested another bootstrap target" >&2
  exit 1
fi
set +e
mode_of new >"${root}/new.out" 2>"${root}/new.err"
new_status=$?
mode_of upgrade >"${root}/upgrade.out" 2>"${root}/upgrade.err"
upgrade_status=$?
set -e
[[ "$new_status" -ne 0 && "$upgrade_status" -ne 0 ]]
grep -q "ci-bootstrap_selfquant-postgres" "${root}/upgrade.err"
if grep -Eq 'bootstrap-compose-new|bootstrap-compose-upgrade' "${root}/upgrade.err"; then
  echo "explicit upgrade error suggested another bootstrap target" >&2
  exit 1
fi

printf '%s\n' "ci-bootstrap_selfquant-postgres" "ci-bootstrap_clickhouse-data" >"$present"
assert_eq "$(mode_of auto)" "upgrade" "both volumes"

nonewline="${root}/nonewline.env"
printf '%s' 'DATABASE_URL=postgres://selfquant:selfquant@localhost:5432/selfquant?sslmode=disable' >"$nonewline"
nonewline_sum="$(sha256sum "$nonewline" | awk '{print $1}')"
nonewline_report="$("${script_dir}/compose-volumes.sh" --env-file "$nonewline" --compose-file "$compose_file")"
assert_eq "$(printf '%s\n' "$nonewline_report" | awk -F= '$1=="selfquant-postgres_name"{print $2}')" \
  "ci-bootstrap_selfquant-postgres" "newline probe postgres volume"
assert_eq "$(printf '%s\n' "$nonewline_report" | awk -F= '$1=="clickhouse-data_name"{print $2}')" \
  "ci-bootstrap_clickhouse-data" "newline probe clickhouse volume"
assert_eq "$("${script_dir}/resolve-compose-mode.sh" --requested auto --env-file "$nonewline" --compose-file "$compose_file")" \
  "upgrade" "newline probe upgrade"
assert_eq "$(sha256sum "$nonewline" | awk '{print $1}')" "$nonewline_sum" "newline probe rewrote env"
if [[ "$(tail -c 1 "$nonewline")" == $'\n' ]]; then
  echo "newline probe added a newline to the original env" >&2
  exit 1
fi

empty_password="${root}/empty-password.env"
printf 'POSTGRES_PASSWORD=\n' >"$empty_password"
"${script_dir}/compose-volumes.sh" --env-file "$empty_password" --compose-file "$compose_file" >/dev/null

kept_password="${root}/kept-password.env"
printf 'POSTGRES_PASSWORD=%s\n' 'kept-probe-secret' >"$kept_password"
kept_sum="$(sha256sum "$kept_password" | awk '{print $1}')"
kept_report="$("${script_dir}/compose-volumes.sh" --env-file "$kept_password" --compose-file "$compose_file" 2>&1)"
assert_eq "$(sha256sum "$kept_password" | awk '{print $1}')" "$kept_sum" "nonempty probe rewrote env"
if printf '%s\n' "$kept_report" | grep -q 'kept-probe-secret'; then
  echo "probe output contained the password" >&2
  exit 1
fi

: >"$present"

cat >"$env_file" <<'EOF'
DATABASE_URL=postgres://selfquant:selfquant@localhost:5432/selfquant?sslmode=disable
POSTGRES_PASSWORD=
CLICKHOUSE_PASSWORD=
ACCOUNT_CREDENTIALS_KEY=0123456789abcdef0123456789abcdef
ACCOUNT_TOKEN_SECRET=
GATEWAY_INTERNAL_TOKEN=
ADMIN_INTERNAL_TOKEN=
REPORT_INTERNAL_TOKEN=
TRADER_INTERNAL_TOKEN=
TRADER_ARBITRAGE_DRY_RUN=true
DEVELOPMENT=true
ACCOUNT_COOKIE_SECURE=false
GATEWAY_CORS_ORIGIN=http://localhost:3000
EOF

"${script_dir}/init-auth-secrets.sh" --mode new --env-file "$env_file" --secrets-dir "$secrets" \
  >"${root}/new-init.out"
if grep -q '0123456789abcdef0123456789abcdef' "$env_file"; then
  echo "new mode kept the published credentials key" >&2
  exit 1
fi
checksum="$(sha256sum "$env_file" "${secrets}/broker-jwt-ed25519.pem" | sha256sum)"
"${script_dir}/init-auth-secrets.sh" --mode new --env-file "$env_file" --secrets-dir "$secrets" \
  >"${root}/new-init-2.out"
again="$(sha256sum "$env_file" "${secrets}/broker-jwt-ed25519.pem" | sha256sum)"
assert_eq "$again" "$checksum" "second new run"
grep -q '^POSTGRES_PASSWORD=.' "$env_file"
grep -q '@localhost:5432' "$env_file"
if grep -q '@postgres:5432' "$env_file"; then
  echo "host DATABASE_URL was pointed at the compose service" >&2
  exit 1
fi

legacy="${root}/legacy.env"
mkdir -p "${root}/legacy-secrets" "${root}/session-secrets"
openssl genpkey -algorithm Ed25519 -out "${root}/legacy-secrets/broker-jwt-ed25519.pem"
openssl pkey -in "${root}/legacy-secrets/broker-jwt-ed25519.pem" -pubout -out "${root}/legacy-secrets/broker-jwt-ed25519.pub"
cp "${root}/legacy-secrets/broker-jwt-ed25519.pem" "${root}/session-secrets/broker-jwt-ed25519.pem"
cp "${root}/legacy-secrets/broker-jwt-ed25519.pub" "${root}/session-secrets/broker-jwt-ed25519.pub"
cat >"$legacy" <<'EOF'
DATABASE_URL=postgres://selfquant:selfquant@localhost:5432/selfquant?sslmode=disable
POSTGRES_PASSWORD=
CLICKHOUSE_BBO_PASSWORD=legacy-clickhouse-password
CLICKHOUSE_PASSWORD=
ACCOUNT_CREDENTIALS_KEY=0123456789abcdef0123456789abcdef
ACCOUNT_TOKEN_SECRET=0123456789abcdef0123456789abcdef
GATEWAY_INTERNAL_TOKEN=gateway-token-gateway-token-gateway
ADMIN_INTERNAL_TOKEN=admin-token-admin-token-admin-token
REPORT_INTERNAL_TOKEN=report-token-report-token-report-token
TRADER_INTERNAL_TOKEN=trader-token-trader-token-trader-token
TRADER_ARBITRAGE_DRY_RUN=true
EOF
"${script_dir}/init-auth-secrets.sh" --mode upgrade --env-file "$legacy" --secrets-dir "${root}/legacy-secrets" \
  >"${root}/legacy.out" 2>"${root}/legacy.err"
grep -q '^CLICKHOUSE_PASSWORD=legacy-clickhouse-password$' "$legacy"
grep -q '^POSTGRES_PASSWORD=selfquant$' "$legacy"
grep -q '^ACCOUNT_CREDENTIALS_KEY=0123456789abcdef0123456789abcdef$' "$legacy"
if grep -q '0123456789abcdef0123456789abcdef' "${root}/legacy.err"; then
  echo "upgrade printed the credentials key" >&2
  exit 1
fi
legacy_sum="$(sha256sum "$legacy" | awk '{print $1}')"
"${script_dir}/init-auth-secrets.sh" --mode upgrade --env-file "$legacy" --secrets-dir "${root}/legacy-secrets" \
  >/dev/null
assert_eq "$(sha256sum "$legacy" | awk '{print $1}')" "$legacy_sum" "repeat upgrade"

conflict="${root}/conflict.env"
cat >"$conflict" <<'EOF'
DATABASE_URL=postgres://selfquant:selfquant@localhost:5432/selfquant?sslmode=disable
POSTGRES_PASSWORD=selfquant
CLICKHOUSE_BBO_PASSWORD=one-password
CLICKHOUSE_PASSWORD=other-password
ACCOUNT_CREDENTIALS_KEY=0123456789abcdef0123456789abcdef
ACCOUNT_TOKEN_SECRET=0123456789abcdef0123456789abcdef
GATEWAY_INTERNAL_TOKEN=gateway-token-gateway-token-gateway
ADMIN_INTERNAL_TOKEN=admin-token-admin-token-admin-token
REPORT_INTERNAL_TOKEN=report-token-report-token-report-token
TRADER_INTERNAL_TOKEN=trader-token-trader-token-trader-token
EOF
set +e
"${script_dir}/init-auth-secrets.sh" --mode upgrade --env-file "$conflict" --secrets-dir "${root}/conflict-secrets" \
  >"${root}/conflict.out" 2>"${root}/conflict.err"
conflict_status=$?
set -e
[[ "$conflict_status" -ne 0 ]]
grep -q "differ" "${root}/conflict.err"

invalid="${root}/invalid.env"
cp "$legacy" "$invalid"
python3 - "$invalid" <<'PY'
import sys
path = sys.argv[1]
text = open(path, encoding="utf-8").read().replace(
    "ACCOUNT_CREDENTIALS_KEY=0123456789abcdef0123456789abcdef",
    "ACCOUNT_CREDENTIALS_KEY=not-a-valid-key",
)
open(path, "w", encoding="utf-8").write(text)
PY
for bad_mode in new upgrade restore; do
  set +e
  "${script_dir}/init-auth-secrets.sh" --mode "$bad_mode" --env-file "$invalid" --secrets-dir "${root}/invalid-secrets" \
    >/dev/null 2>"${root}/invalid-${bad_mode}.err"
  status=$?
  set -e
  [[ "$status" -ne 0 ]]
  grep -q "ACCOUNT_CREDENTIALS_KEY" "${root}/invalid-${bad_mode}.err"
done

restore_env="${root}/restore.env"
cp "$legacy" "$restore_env"
python3 - "$restore_env" <<'PY'
import sys
path = sys.argv[1]
text = open(path, encoding="utf-8").read().replace("ACCOUNT_TOKEN_SECRET=0123456789abcdef0123456789abcdef\n", "ACCOUNT_TOKEN_SECRET=\n")
open(path, "w", encoding="utf-8").write(text)
PY
set +e
"${script_dir}/init-auth-secrets.sh" --mode restore --env-file "$restore_env" --secrets-dir "${root}/restore-secrets" \
  >/dev/null 2>"${root}/restore.err"
restore_status=$?
set -e
[[ "$restore_status" -ne 0 ]]
grep -q "ACCOUNT_TOKEN_SECRET" "${root}/restore.err"

session="${root}/session.env"
cp "$legacy" "$session"
python3 - "$session" <<'PY'
import sys
path = sys.argv[1]
text = open(path, encoding="utf-8").read().replace(
    "ACCOUNT_TOKEN_SECRET=0123456789abcdef0123456789abcdef",
    "ACCOUNT_TOKEN_SECRET=replace-with-a-long-random-secret",
)
open(path, "w", encoding="utf-8").write(text)
PY
"${script_dir}/init-auth-secrets.sh" --mode upgrade --env-file "$session" --secrets-dir "${root}/session-secrets" \
  >/dev/null 2>"${root}/session.err"
grep -q "existing login sessions will be invalidated" "${root}/session.err"
if grep -q '^ACCOUNT_TOKEN_SECRET=replace-with-' "$session"; then
  echo "upgrade left the session secret placeholder" >&2
  exit 1
fi

cat >"${bin}/clickhouse-client" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${FAKE_CLIENT_LOG:?}"
file=""
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  if [[ "${args[$i]}" == "--queries-file" ]]; then
    file="${args[$((i + 1))]}"
  fi
done
if [[ -n "$file" ]] && grep -q FAIL "$file"; then
  exit 1
fi
EOF
chmod 755 "${bin}/clickhouse-client"
export FAKE_CLIENT_LOG="${root}/client.log"
migrations="${root}/migrations"
mkdir -p "$migrations"
echo "FAIL" >"${migrations}/001.sql"
echo "SELECT 1" >"${migrations}/002.sql"
set +e
CLICKHOUSE_HOST=clickhouse CLICKHOUSE_USER=bbo_writer CLICKHOUSE_PASSWORD=hidden-password \
  CLICKHOUSE_MIGRATION_DIR="$migrations" \
  bash "${backend_dir}/deploy/clickhouse/migrate.sh" >"${root}/migrate.out" 2>"${root}/migrate.err"
migrate_status=$?
set -e
[[ "$migrate_status" -ne 0 ]]
[[ "$(grep -c -- '--queries-file' "$FAKE_CLIENT_LOG")" -eq 1 ]]
if grep -q 'hidden-password' "$FAKE_CLIENT_LOG" "${root}/migrate.out" "${root}/migrate.err"; then
  echo "migration printed the password" >&2
  exit 1
fi
grep -q -- '--host clickhouse' "$FAKE_CLIENT_LOG"
grep -q -- '--multiquery' "$FAKE_CLIENT_LOG"

before="$(sha256sum "$env_file")"
example="${root}/example.env"
example_secrets="${root}/example-secrets"
cp "${backend_dir}/.env.example" "$example"
"${script_dir}/init-auth-secrets.sh" --mode new --env-file "$example" --secrets-dir "$example_secrets" >/dev/null
shared="${root}/shared.env"
bash "${script_dir}/prepare-compose-env.sh" "$example" "$shared"
before="$(sha256sum "$example" "$example_secrets/broker-jwt-ed25519.pem")"
bash "${script_dir}/validate-compose-env.sh" \
  --level structural \
  --env-file "$example" \
  --compose-file "$compose_file" \
  --shared-env "$shared" \
  --secrets-dir "$example_secrets" >/dev/null
after="$(sha256sum "$example" "$example_secrets/broker-jwt-ed25519.pem")"
assert_eq "$after" "$before" "validate rewrote env"

set_dry_run() {
  local dest="$1" value="$2"
  cp "$example" "$dest"
  python3 - "$dest" "$value" <<'PY'
import sys
path, value = sys.argv[1], sys.argv[2]
lines = open(path, encoding="utf-8").read().splitlines()
found = False
for index, line in enumerate(lines):
    if line.startswith("TRADER_ARBITRAGE_DRY_RUN="):
        lines[index] = f"TRADER_ARBITRAGE_DRY_RUN={value}"
        found = True
        break
if not found:
    sys.exit(1)
open(path, "w", encoding="utf-8").write("\n".join(lines) + "\n")
PY
}

validate_copy() {
  bash "${script_dir}/validate-compose-env.sh" \
    --level structural \
    --env-file "$1" \
    --compose-file "$compose_file" \
    --shared-env "$shared" \
    --secrets-dir "$example_secrets"
}

set_dry_run "${root}/dry-run-false.env" "false"
validate_copy "${root}/dry-run-false.env" >/dev/null

for bad in "" yes; do
  set_dry_run "${root}/dry-run-bad.env" "$bad"
  set +e
  validate_copy "${root}/dry-run-bad.env" >/dev/null 2>"${root}/dry-run-bad.err"
  bad_status=$?
  set -e
  [[ "$bad_status" -ne 0 ]]
  grep -qx "TRADER_ARBITRAGE_DRY_RUN must be true or false" "${root}/dry-run-bad.err"
done

echo "bootstrap security checks passed"
