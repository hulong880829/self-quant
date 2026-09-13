#!/usr/bin/env bash
# Restart only the isolated Aster/Lighter perpetual BBO recording chain.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bin="${MDS_RELEASE_BIN:-$root/core/build-release/mds}"
config_dir="$root/core/mds/config"
env_file="$root/backend/.env"
log_dir="${MDS_ASTER_LIGHTER_LOG_DIR:-/tmp/mds-aster-lighter}"

producer_bin="$bin/mds_producer"
consumer_bin="$bin/mds_shm_consumer"
producer_cfg="$config_dir/aster-lighter.yaml"
consumer_cfg="$config_dir/aster-lighter-clickhouse.yaml"

producer_pattern='mds_producer .*mds/config/aster-lighter.yaml'
consumer_pattern='mds_shm_consumer .*mds/config/aster-lighter-clickhouse.yaml'
book_pattern='mds_producer .*mds/config/aster-lighter-book.yaml'

die() { echo "error: $*" >&2; exit 1; }

matching_pids() {
  pgrep -f -- "$1" || true
}

stop_matching() {
  local pattern=$1
  local pids
  pids="$(matching_pids "$pattern")"
  [[ -z "$pids" ]] && return 0
  echo "stopping $pattern (pid: ${pids//$'\n'/ })"
  # shellcheck disable=SC2086
  kill $pids 2>/dev/null || true
  local i
  for ((i = 0; i < 40; i++)); do
    [[ -z "$(matching_pids "$pattern")" ]] && return 0
    sleep 0.25
  done
  pids="$(matching_pids "$pattern")"
  if [[ -n "$pids" ]]; then
    echo "force killing $pattern (pid: ${pids//$'\n'/ })"
    # shellcheck disable=SC2086
    kill -9 $pids 2>/dev/null || true
  fi
}

start_logged() {
  local log=$1
  shift
  mkdir -p "$(dirname "$log")"
  : >"$log"
  echo "starting $* (log $log)"
  nohup "$@" >>"$log" 2>&1 &
  echo "  pid=$!"
}

wait_alive() {
  local pattern=$1
  local seconds=${2:-20}
  local i
  for ((i = 0; i < seconds * 4; i++)); do
    [[ -n "$(matching_pids "$pattern")" ]] && return 0
    sleep 0.25
  done
  die "process did not stay up: $pattern"
}

[[ -x "$producer_bin" ]] || die "missing binary $producer_bin"
[[ -x "$consumer_bin" ]] || die "missing binary $consumer_bin"
[[ -f "$producer_cfg" ]] || die "missing config $producer_cfg"
[[ -f "$consumer_cfg" ]] || die "missing config $consumer_cfg"
[[ -f "$env_file" ]] || die "missing $env_file (need CLICKHOUSE_BBO_PASSWORD)"
[[ -z "$(matching_pids "$book_pattern")" ]] ||
  die "Aster/Lighter book producer is running; use a separate outbound IP"

set -a
# shellcheck disable=SC1090
source "$env_file"
set +a
[[ -n "${CLICKHOUSE_BBO_PASSWORD:-}" ]] ||
  die "CLICKHOUSE_BBO_PASSWORD is empty"

stop_matching "$consumer_pattern"
stop_matching "$producer_pattern"

start_logged "$log_dir/producer.log" \
  "$producer_bin" --config "$producer_cfg" --duration 0
wait_alive "$producer_pattern"

consumer_delay="${MDS_ASTER_LIGHTER_CONSUMER_START_DELAY_S:-30}"
echo "waiting ${consumer_delay}s for Aster/Lighter ticker rings"
sleep "$consumer_delay"
start_logged "$log_dir/clickhouse-bbo.log" \
  "$consumer_bin" --config "$consumer_cfg" --clickhouse-bbo
wait_alive "$consumer_pattern"

echo "Aster/Lighter BBO recording chain is up"
echo "  producer $(matching_pids "$producer_pattern" | tr '\n' ' ')"
echo "  consumer $(matching_pids "$consumer_pattern" | tr '\n' ' ')"
