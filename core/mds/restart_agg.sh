#!/usr/bin/env bash
# Restart the 5-venue aggregate chain: producer -> aggregator ->
# shm consumer (gateway + recorder). Start any process that is not running.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bin="${MDS_RELEASE_BIN:-$root/core/build-release/mds}"
config_dir="$root/core/mds/config"
env_file="$root/backend/.env"
log_dir="${MDS_AGG_LOG_DIR:-/tmp/mds-agg}"
record_dir="${AGGDATA_RECORDING_DIR:-/tmp/marketdata/crypto}"

producer_bin="$bin/mds_producer"
aggregator_bin="$bin/mds_aggregator"
consumer_bin="$bin/mds_shm_consumer"
producer_cfg="$config_dir/mds_producer.yaml"
aggregator_cfg="$config_dir/5venues_aggregator.yaml"
consumer_cfg="$config_dir/mds_shm_consumer.yaml"

producer_pattern='mds_producer .*mds_producer.yaml'
aggregator_pattern='mds_aggregator .*5venues_aggregator.yaml'
consumer_pattern='mds_shm_consumer .*mds_shm_consumer.yaml'

die() { echo "error: $*" >&2; exit 1; }

require_files() {
  [[ -x "$producer_bin" ]] || die "missing binary $producer_bin"
  [[ -x "$aggregator_bin" ]] || die "missing binary $aggregator_bin"
  [[ -x "$consumer_bin" ]] || die "missing binary $consumer_bin"
  [[ -f "$producer_cfg" ]] || die "missing config $producer_cfg"
  [[ -f "$aggregator_cfg" ]] || die "missing config $aggregator_cfg"
  [[ -f "$consumer_cfg" ]] || die "missing config $consumer_cfg"
  [[ -f "$env_file" ]] || die "missing $env_file (need MDS_GATEWAY_TOKEN)"
}

matching_pids() {
  pgrep -f -- "$1" || true
}

stop_matching() {
  local pattern=$1
  local pids
  pids="$(matching_pids "$pattern")"
  if [[ -z "$pids" ]]; then
    echo "not running: $pattern"
    return 0
  fi
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
  echo "starting $*  (log $log)"
  nohup "$@" >>"$log" 2>&1 &
  echo "  pid=$!"
}

wait_alive() {
  local pattern=$1
  local seconds=${2:-15}
  local i
  for ((i = 0; i < seconds * 4; i++)); do
    if [[ -n "$(matching_pids "$pattern")" ]]; then
      return 0
    fi
    sleep 0.25
  done
  die "process did not stay up: $pattern"
}

require_files
set -a
# shellcheck disable=SC1090
source "$env_file"
set +a
[[ -n "${MDS_GATEWAY_TOKEN:-}" ]] || die "MDS_GATEWAY_TOKEN is empty"
mkdir -p "$log_dir" "$record_dir"

echo "stopping aggregate chain"
stop_matching "$consumer_pattern"
stop_matching "$aggregator_pattern"
stop_matching "$producer_pattern"
sleep 1

echo "starting aggregate chain"
start_logged "$log_dir/producer.log" \
  "$producer_bin" --config "$producer_cfg" --duration 0
wait_alive "$producer_pattern"
sleep 3

start_logged "$log_dir/aggregator.log" \
  "$aggregator_bin" --config "$aggregator_cfg" --duration 0
wait_alive "$aggregator_pattern"
consumer_delay="${MDS_CONSUMER_START_DELAY_S:-30}"
echo "waiting ${consumer_delay}s for shared-memory segments before consumer"
sleep "$consumer_delay"

start_logged "$log_dir/consumer.log" \
  "$consumer_bin" --config "$consumer_cfg" --gateway --record
wait_alive "$consumer_pattern"

echo "aggregate chain is up"
echo "  producer    $(matching_pids "$producer_pattern" | tr '\n' ' ')"
echo "  aggregator  $(matching_pids "$aggregator_pattern" | tr '\n' ' ')"
echo "  consumer    $(matching_pids "$consumer_pattern" | tr '\n' ' ')"
