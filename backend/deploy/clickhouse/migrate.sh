#!/usr/bin/env bash
# Apply ClickHouse migrations in filename order. Do not print the password.
set -euo pipefail

host="${CLICKHOUSE_HOST:-}"
user="${CLICKHOUSE_USER:-}"
password="${CLICKHOUSE_PASSWORD:-}"
migration_dir="${CLICKHOUSE_MIGRATION_DIR:-/migrations}"

if [[ "$host" != "clickhouse" ]]; then
  echo "clickhouse-migrate requires CLICKHOUSE_HOST=clickhouse" >&2
  exit 1
fi
if [[ -z "$user" || -z "$password" ]]; then
  echo "clickhouse-migrate requires CLICKHOUSE_USER and CLICKHOUSE_PASSWORD" >&2
  exit 1
fi

shopt -s nullglob
files=("$migration_dir"/*.sql)
if [[ ${#files[@]} -eq 0 ]]; then
  echo "no ClickHouse migrations found" >&2
  exit 1
fi
mapfile -t files < <(printf '%s\n' "${files[@]}" | sort)

xml_escape() {
  local value="$1"
  value="${value//&/&amp;}"
  value="${value//</&lt;}"
  value="${value//>/&gt;}"
  value="${value//\"/&quot;}"
  printf '%s' "$value"
}

client_config="$(mktemp)"
chmod 0600 "$client_config"
cleanup() {
  rm -f "$client_config"
}
trap cleanup EXIT

cat >"$client_config" <<EOF
<config>
  <host>$(xml_escape "$host")</host>
  <user>$(xml_escape "$user")</user>
  <password>$(xml_escape "$password")</password>
</config>
EOF

for file in "${files[@]}"; do
  echo "applying $(basename "$file")"
  clickhouse-client \
    --config-file "$client_config" \
    --host clickhouse \
    --user "$user" \
    --multiquery \
    --queries-file "$file"
done
