#!/usr/bin/env bash
# DEPRECATED. Do not start this recorder while restart_recoder.sh's unified
# consumer is writing market_data.crypto_bbo. IBKR is a selector on that
# consumer. Kept so an operator can roll back to the independent writer.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bin="${MDS_RELEASE_BIN:-$root/core/build-release/mds}"
config_dir="$root/core/mds/config"
env_file="$root/backend/.env"
log_dir="${MDS_IBKR_LOG_DIR:-/tmp/mds-ibkr}"
ring_timeout="${MDS_IBKR_RING_TIMEOUT_S:-60}"

consumer_bin="$bin/mds_shm_consumer"
consumer_cfg="${MDS_IBKR_RECORDER_CONFIG:-$config_dir/mds_ibkr_clickhouse.example.yaml}"
recorder_pattern='mds_shm_consumer .*mds_ibkr_clickhouse'

die() { echo "error: $*" >&2; exit 1; }

[[ -x "$consumer_bin" ]] || die "missing binary $consumer_bin"
[[ -f "$consumer_cfg" ]] || die "missing config $consumer_cfg"
[[ -f "$env_file" ]] || die "missing $env_file (need CLICKHOUSE_PASSWORD)"

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

echo "validating IBKR ClickHouse recorder config"
"$consumer_bin" --config "$consumer_cfg" --clickhouse-bbo --validate-only

echo "waiting for IBKR ring"
ready=0
for ((i = 0; i < ring_timeout * 4; i++)); do
  if compgen -G "/dev/shm/selfquant.mds.ibkr.ibkr.equity.ticker.shard0.*" \
      >/dev/null; then
    ready=1
    break
  fi
  sleep 0.25
done
[[ "$ready" == 1 ]] || die "IBKR ring is not ready; start mds_ibkr_producer first"

stop_matching "$recorder_pattern"
mkdir -p "$log_dir"
set -a
# shellcheck disable=SC1090
source "$env_file"
set +a

log="$log_dir/mds_ibkr_recorder.log"
: >"$log"
echo "starting $consumer_bin --config $consumer_cfg --clickhouse-bbo (log $log)"
nohup "$consumer_bin" --config "$consumer_cfg" --clickhouse-bbo >>"$log" 2>&1 &
echo "  pid=$!"
sleep 0.5
if ! kill -0 "$!" 2>/dev/null; then
  die "IBKR recorder exited during startup; check $log"
fi
echo "IBKR recorder is running"
