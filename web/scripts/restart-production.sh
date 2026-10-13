#!/usr/bin/env bash
# Production build and restart for this Next app.
# All production builds MUST go through this script.
# Do not run `npm run build` while Next is serving: flock cannot
# stop out-of-band builds from corrupting .next.

set -euo pipefail

WEB_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$WEB_ROOT"

LOCK_FILE=/tmp/self-quant-web-deploy.lock
BUILD_LOG=/tmp/web-next-build.log
START_LOG=/tmp/web-next.log
START_LOG_PREV=/tmp/web-next.log.prev
BUILD_ID_FILE=/tmp/web-next.build-id
BUILD_STARTED_MARKER=/tmp/web-next-build.started
PID_FILE=/tmp/web-next.pid
PORT=3000
READY_WAIT_SECS=30
STOP_WAIT_SECS=15
MIN_DISK_KB=$((5 * 1024 * 1024))
MIN_MEM_KB=$((2 * 1024 * 1024))

exec 9>"$LOCK_FILE"
if ! flock -n 9; then
  echo "another deploy holds ${LOCK_FILE}" >&2
  exit 1
fi

require_count() {
  local file=$1 pattern=$2 expect=$3 label=$4
  local n
  n=$(grep -cE "$pattern" "$file" || true)
  if [[ "$n" != "$expect" ]]; then
    echo "source gate failed: ${label} in ${file} want ${expect} got ${n}" >&2
    exit 1
  fi
}

listener_pids() {
  local pids=""
  if command -v lsof >/dev/null 2>&1; then
    pids=$(lsof -nP -iTCP:"$PORT" -sTCP:LISTEN -t 2>/dev/null || true)
  fi
  if [[ -z "${pids}" ]]; then
    pids=$(ss -ltnp 2>/dev/null | awk -v port=":${PORT} " '
      $0 ~ port {
        s=$0
        while (match(s, /pid=[0-9]+/)) {
          print substr(s, RSTART+4, RLENGTH-4)
          s=substr(s, RSTART+RLENGTH)
        }
      }')
  fi
  printf '%s\n' ${pids:-} | awk 'NF && !seen[$1]++'
}

is_web_next() {
  local pid=$1
  local cwd cmd
  cwd=$(readlink "/proc/${pid}/cwd" 2>/dev/null || true)
  [[ "$cwd" == "$WEB_ROOT" ]] || return 1
  cmd=$(tr '\0' ' ' < "/proc/${pid}/cmdline" 2>/dev/null || true)
  [[ "$cmd" == *next-server* || "$cmd" == *"next start"* ]]
}

stop_pid() {
  local pid=$1
  if ! kill -0 "$pid" 2>/dev/null; then
    return 0
  fi
  echo "sending SIGTERM to verified pid ${pid}"
  kill -TERM "$pid" 2>/dev/null || true
  local i
  for ((i = 0; i < STOP_WAIT_SECS * 2; i++)); do
    if ! kill -0 "$pid" 2>/dev/null; then
      return 0
    fi
    sleep 0.5
  done
  echo "sending SIGKILL to verified pid ${pid}"
  kill -KILL "$pid" 2>/dev/null || true
  sleep 0.5
}

stop_verified_listeners() {
  local pid
  local found=0
  while read -r pid; do
    [[ -n "$pid" ]] || continue
    if is_web_next "$pid"; then
      stop_pid "$pid"
      found=1
    else
      echo "port ${PORT} occupied by unrelated pid ${pid}; refusing to kill" >&2
      exit 1
    fi
  done < <(listener_pids)
  if (( found == 0 )); then
    echo "no Next listener on ${PORT}"
  fi
}

assert_port_free() {
  local leftover
  leftover=$(listener_pids)
  if [[ -n "${leftover}" ]]; then
    echo "port ${PORT} still in use: ${leftover}" >&2
    exit 1
  fi
}

env_nonempty() {
  local key=$1
  local line val
  line=$(grep -E "^${key}=" .env.local | tail -n 1 || true)
  if [[ -z "$line" ]]; then
    echo "missing ${key} in .env.local" >&2
    exit 1
  fi
  val="${line#*=}"
  val="${val%\"}"
  val="${val#\"}"
  if [[ -z "$val" ]]; then
    echo "empty ${key} in .env.local" >&2
    exit 1
  fi
}

fail_keep_artifacts() {
  echo "$1" >&2
  if [[ -f "$START_LOG" ]]; then
    echo "---- ${START_LOG} tail ----" >&2
    tail -n 80 "$START_LOG" >&2 || true
  fi
  local pid
  while read -r pid; do
    [[ -n "$pid" ]] || continue
    if is_web_next "$pid"; then
      stop_pid "$pid"
    fi
  done < <(listener_pids)
  exit 1
}

if [[ "$(basename "$WEB_ROOT")" != "web" ]]; then
  echo "cwd must be web, got ${WEB_ROOT}" >&2
  exit 1
fi

echo "deploy lock acquired, cwd=${WEB_ROOT}"

require_count src/components/layout/app-shell.tsx '"use client";' 1 '"use client"'
require_count src/components/layout/app-shell.tsx 'export function NavigationLinks' 1 'NavigationLinks'
require_count src/components/layout/app-shell.tsx 'export function AdminStandaloneShell' 1 'AdminStandaloneShell'
require_count src/components/layout/app-shell.tsx 'export function AppShell' 1 'AppShell export'
require_count src/app/funding/jev-arb/page.tsx 'export const metadata' 1 'metadata'
require_count src/app/funding/jev-arb/page.tsx 'export default' 1 'default export'

if ! ./node_modules/.bin/eslint \
  src/components/layout/app-shell.tsx \
  src/app/funding/jev-arb/page.tsx; then
  echo "eslint failed on deduped files" >&2
  exit 1
fi

pkg_next=$(node -p "require('./package.json').dependencies.next")
mod_next=$(node -p "require('./node_modules/next/package.json').version")
if [[ "$pkg_next" != "16.3.0" || "$mod_next" != "16.3.0" ]]; then
  echo "Next version mismatch package=${pkg_next} node_modules=${mod_next}" >&2
  exit 1
fi
if [[ ! -f .env.local ]]; then
  echo ".env.local is missing" >&2
  exit 1
fi
env_nonempty NEXT_PUBLIC_API_BASE_URL
env_nonempty NEXT_PUBLIC_AGGDATA_BASE_URL
env_nonempty NEXT_PUBLIC_AGGDATA_WS_URL

disk_kb=$(df -Pk "$WEB_ROOT" | awk 'NR==2 {print $4}')
mem_kb=$(awk '/MemAvailable:/ {print $2}' /proc/meminfo)
if (( disk_kb < MIN_DISK_KB )); then
  echo "disk ${disk_kb} KB below ${MIN_DISK_KB} KB" >&2
  exit 1
fi
if (( mem_kb < MIN_MEM_KB )); then
  echo "MemAvailable ${mem_kb} KB below ${MIN_MEM_KB} KB" >&2
  exit 1
fi

stop_verified_listeners
assert_port_free

if [[ -e .next ]]; then
  failed_name=".next.failed-$(date +%Y%m%d%H%M%S)"
  mv .next "$failed_name"
  echo "isolated incomplete .next as ${failed_name}"
fi

if ! ./node_modules/.bin/tsc --noEmit --incremental false; then
  echo "tsc --noEmit --incremental false failed" >&2
  exit 1
fi

touch "$BUILD_STARTED_MARKER"

if npm run build >"$BUILD_LOG" 2>&1; then
  rc=0
else
  rc=$?
fi

if (( rc != 0 )); then
  echo "next build failed, see ${BUILD_LOG}"
  tail -n 80 "$BUILD_LOG" >&2 || true
  exit "$rc"
fi

if [[ ! -s .next/BUILD_ID ]]; then
  echo "missing or empty .next/BUILD_ID" >&2
  exit 1
fi
if [[ ! -d .next/server || -z "$(ls -A .next/server 2>/dev/null || true)" ]]; then
  echo "missing .next/server artifacts" >&2
  exit 1
fi
if [[ ! -d .next/static || -z "$(ls -A .next/static 2>/dev/null || true)" ]]; then
  echo "missing .next/static artifacts" >&2
  exit 1
fi
if [[ ! .next/BUILD_ID -nt "$BUILD_STARTED_MARKER" ]]; then
  echo ".next/BUILD_ID is not newer than ${BUILD_STARTED_MARKER}" >&2
  exit 1
fi

shopt -s nullglob
for leftover in .next.failed-*; do
  rm -rf "$leftover"
done
shopt -u nullglob

cp .next/BUILD_ID "$BUILD_ID_FILE"
echo "build ok BUILD_ID=$(tr -d '\n' < .next/BUILD_ID)"

if [[ -f "$START_LOG" ]]; then
  mv -f "$START_LOG" "$START_LOG_PREV"
fi

nohup ./node_modules/.bin/next start -H 0.0.0.0 -p "$PORT" \
  >>"$START_LOG" 2>&1 </dev/null 9>&- &
launcher_pid=$!

verified_pid=""
deadline=$((SECONDS + READY_WAIT_SECS))
while (( SECONDS < deadline )); do
  if ! kill -0 "$launcher_pid" 2>/dev/null; then
    wait "$launcher_pid" || true
  fi
  while read -r pid; do
    [[ -n "$pid" ]] || continue
    if is_web_next "$pid"; then
      verified_pid=$pid
      break
    fi
  done < <(listener_pids)
  if [[ -n "$verified_pid" ]]; then
    break
  fi
  if ! kill -0 "$launcher_pid" 2>/dev/null && [[ -z "$(listener_pids)" ]]; then
    fail_keep_artifacts "next start exited before listening on ${PORT}"
  fi
  sleep 0.5
done

if [[ -z "$verified_pid" ]]; then
  fail_keep_artifacts "next start did not listen on ${PORT} within ${READY_WAIT_SECS}s"
fi

echo "$verified_pid" >"$PID_FILE"

listeners=$(listener_pids | awk 'NF' | wc -l)
if (( listeners != 1 )); then
  fail_keep_artifacts "expected one listener on ${PORT}, found ${listeners}"
fi
if ! is_web_next "$verified_pid"; then
  fail_keep_artifacts "verified pid ${verified_pid} is no longer a web Next process"
fi
if [[ ! "/proc/${verified_pid}" -nt "$BUILD_STARTED_MARKER" ]]; then
  fail_keep_artifacts "process ${verified_pid} is not newer than build marker"
fi

disk_id=$(tr -d '\n' < .next/BUILD_ID)
recorded_id=$(tr -d '\n' < "$BUILD_ID_FILE")
if [[ "$disk_id" != "$recorded_id" ]]; then
  fail_keep_artifacts "BUILD_ID mismatch disk=${disk_id} recorded=${recorded_id}"
fi

if grep -E 'Could not find a production build|production-start-no-build-id' "$START_LOG" >/dev/null 2>&1; then
  fail_keep_artifacts "start log reports missing production build"
fi

log_mark=0
if [[ -f "$START_LOG" ]]; then
  log_mark=$(wc -c <"$START_LOG")
fi

check_page() {
  local path=$1
  local code
  code=$(curl -sS -o /tmp/web-next-curl.body -w '%{http_code}' --max-time 8 \
    "http://127.0.0.1:${PORT}${path}" || true)
  if [[ "$code" != "200" ]]; then
    fail_keep_artifacts "GET ${path} returned ${code}"
  fi
}

check_page /
cp /tmp/web-next-curl.body /tmp/web-next-home.html
check_page /funding
check_page /orderbook

mapfile -t chunks < <(grep -oE '/_next/static/[A-Za-z0-9._/-]+' /tmp/web-next-home.html | awk '!seen[$0]++')
if (( ${#chunks[@]} == 0 )); then
  fail_keep_artifacts "homepage HTML has no /_next/static assets"
fi
for chunk in "${chunks[@]}"; do
  code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 8 \
    "http://127.0.0.1:${PORT}${chunk}" || true)
  if [[ "$code" != "200" ]]; then
    fail_keep_artifacts "GET ${chunk} returned ${code}"
  fi
done

if [[ -f "$START_LOG" ]]; then
  new_errors=$(tail -c +"$((log_mark + 1))" "$START_LOG" | \
    grep -c 'Server Reference ID did not match' || true)
  if (( new_errors >= 3 )); then
    fail_keep_artifacts "batch Server Reference errors during local page fetch (${new_errors})"
  fi
fi

echo "production next is up pid=${verified_pid} port=${PORT} BUILD_ID=${disk_id}"
