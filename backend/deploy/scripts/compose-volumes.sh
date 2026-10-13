#!/usr/bin/env bash
# Report whether the Compose project's Postgres and ClickHouse volumes exist.
# Names come from `docker compose config` for this same project, never a fixed directory prefix.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
backend_dir="$(cd -- "${script_dir}/../.." && pwd)"
env_file="${backend_dir}/.env"
compose_file="${backend_dir}/deploy/docker-compose.yml"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --env-file)
      env_file="${2:-}"
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

if [[ ! -f "$compose_file" ]]; then
  echo "missing compose file: $compose_file" >&2
  exit 1
fi

probe_env="$(mktemp)"
chmod 0600 "$probe_env"
cleanup() {
  rm -f "$probe_env"
}
trap cleanup EXIT

if [[ -f "$env_file" ]]; then
  cp "$env_file" "$probe_env"
fi
python3 - "$compose_file" "$probe_env" <<'PY'
import re, sys
compose_path, env_path = sys.argv[1], sys.argv[2]
data = open(env_path, "rb").read()
if data and not data.endswith(b"\n"):
    data += b"\n"
    open(env_path, "wb").write(data)
text = open(compose_path, encoding="utf-8").read()
required = []
for match in re.finditer(r"\$\{([A-Za-z_][A-Za-z0-9_]*):\?", text):
    name = match.group(1)
    if name not in required:
        required.append(name)
values = {}
with open(env_path, encoding="utf-8") as handle:
    for line in handle:
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        values[key] = value.rstrip("\r\n")
with open(env_path, "a", encoding="utf-8") as handle:
    for name in required:
        if not values.get(name, ""):
            handle.write(f"{name}=volume-probe\n")
PY

render_config() {
  docker compose --env-file "$probe_env" -f "$compose_file" config --format json
}

volume_exists() {
  docker volume inspect "$1" >/dev/null 2>&1
}

declare -A rendered=()
config_json=""
if config_json="$(render_config)"; then
  while IFS=$'\t' read -r key name; do
    rendered["$key"]="$name"
  done < <(printf '%s\n' "$config_json" | python3 -c '
import json, sys
document = json.load(sys.stdin)
volumes = document.get("volumes") or {}
for key in ("selfquant-postgres", "clickhouse-data"):
    name = (volumes.get(key) or {}).get("name") or ""
    if not name:
        print(f"compose config did not render a volume name for {key}", file=sys.stderr)
        sys.exit(1)
    print(f"{key}\t{name}")
')
else
  echo "warning: compose config could not be rendered; using Compose volume labels" >&2
  abs_compose="$(python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$compose_file")"
  for key in selfquant-postgres clickhouse-data; do
    match=""
    while IFS= read -r candidate; do
      [[ -n "$candidate" ]] || continue
      files="$(docker volume inspect --format '{{ index .Labels "com.docker.compose.project.config_files" }}' "$candidate" 2>/dev/null || true)"
      logical="$(docker volume inspect --format '{{ index .Labels "com.docker.compose.volume" }}' "$candidate" 2>/dev/null || true)"
      if [[ "$logical" == "$key" && "$files" == *"$abs_compose"* ]]; then
        if [[ -n "$match" ]]; then
          echo "multiple volumes match ${key} for ${abs_compose}" >&2
          exit 1
        fi
        match="$candidate"
      fi
    done < <(docker volume ls -q --filter "label=com.docker.compose.volume=${key}" || true)
    rendered["$key"]="$match"
  done
fi

for key in selfquant-postgres clickhouse-data; do
  name="${rendered[$key]:-}"
  if [[ -z "$name" ]]; then
    echo "could not resolve the Compose volume name for ${key}" >&2
    exit 1
  fi
  state="missing"
  if volume_exists "$name"; then
    state="present"
  fi
  printf '%s_name=%s\n' "$key" "$name"
  printf '%s_state=%s\n' "$key" "$state"
done
