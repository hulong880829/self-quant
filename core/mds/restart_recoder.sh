#!/usr/bin/env bash
# Restart the eight-venue ClickHouse BBO recording chain: three multiplex
# producers plus one shm consumer that writes --clickhouse-bbo.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bin="${MDS_RELEASE_BIN:-$root/core/build-release/mds}"
config_dir="$root/core/mds/config"
env_file="$root/backend/.env"
log_dir="${MDS_BBO_LOG_DIR:-/tmp/mds-bbo}"
ring_timeout="${MDS_BBO_RING_TIMEOUT_S:-120}"

producer_bin="$bin/mds_producer"
consumer_bin="$bin/mds_shm_consumer"
binance_okx_cfg="$config_dir/binance-okx.yaml"
bitget_cfg="$config_dir/bitget-bybit-gate.yaml"
hl_aster_lighter_cfg="$config_dir/hyperliquid-aster-lighter.yaml"
consumer_cfg="$config_dir/mds_clickhouse_bbo.example.yaml"

binance_okx_pattern='mds_producer .*mds/config/binance-okx[.]yaml'
bitget_pattern='mds_producer .*mds/config/bitget-bybit-gate[.]yaml'
hl_aster_lighter_pattern='mds_producer .*mds/config/hyperliquid-aster-lighter[.]yaml'
consumer_pattern='mds_shm_consumer .*mds_clickhouse_bbo'

die() { echo "error: $*" >&2; exit 1; }

require_files() {
  [[ -x "$producer_bin" ]] || die "missing binary $producer_bin"
  [[ -x "$consumer_bin" ]] || die "missing binary $consumer_bin"
  [[ -f "$binance_okx_cfg" ]] || die "missing config $binance_okx_cfg"
  [[ -f "$bitget_cfg" ]] || die "missing config $bitget_cfg"
  [[ -f "$hl_aster_lighter_cfg" ]] ||
    die "missing config $hl_aster_lighter_cfg"
  [[ -f "$consumer_cfg" ]] || die "missing config $consumer_cfg"
  [[ -f "$env_file" ]] || die "missing $env_file (need CLICKHOUSE_BBO_PASSWORD)"
}

validate_configs() {
  echo "validating ClickHouse BBO configs"
  "$producer_bin" --config "$binance_okx_cfg" --validate-only
  "$producer_bin" --config "$bitget_cfg" --validate-only
  "$producer_bin" --config "$hl_aster_lighter_cfg" --validate-only
  "$consumer_bin" --config "$consumer_cfg" --clickhouse-bbo --validate-only
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
  started_pid=$!
}

process_alive() {
  kill -0 "$1" 2>/dev/null
}

require_alive() {
  local pid=$1
  local label=$2
  process_alive "$pid" || die "$label exited during startup; check $log_dir/$label.log"
}

rings_ready() {
  local venue_product venue product shard
  local venue_products=(
    "binance spot"
    "binance perpetual"
    "okx spot"
    "okx perpetual"
    "bitget spot"
    "bitget perpetual"
    "bybit spot"
    "bybit perpetual"
    "gate spot"
    "gate perpetual"
    "hyperliquid spot"
    "hyperliquid perpetual"
    "aster perpetual"
    "lighter perpetual"
  )
  for venue_product in "${venue_products[@]}"; do
    read -r venue product <<<"$venue_product"
    for shard in 0 1 2 3; do
      compgen -G \
        "/dev/shm/selfquant.mds.bbo.${venue}.${product}.ticker.shard${shard}.*" \
        >/dev/null || return 1
    done
  done
}

wait_for_rings() {
  local deadline=$((SECONDS + ring_timeout))
  echo "waiting up to ${ring_timeout}s for 56 shared-memory rings"
  while ((SECONDS < deadline)); do
    require_alive "$binance_okx_pid" "binance-okx"
    require_alive "$bitget_pid" "bitget-bybit-gate"
    require_alive "$hl_aster_lighter_pid" "hyperliquid-aster-lighter"
    if rings_ready; then
      echo "all 56 shared-memory rings are ready"
      return 0
    fi
    sleep 1
  done
  die "timed out waiting for 56 shared-memory rings"
}

stop_unified_chain() {
  stop_matching "$consumer_pattern"
  stop_matching "$binance_okx_pattern"
  stop_matching "$bitget_pattern"
  stop_matching "$hl_aster_lighter_pattern"
}

restart_in_progress=false
cleanup_on_exit() {
  local status=$?
  if ((status == 0)) || [[ "$restart_in_progress" != true ]]; then
    return
  fi
  trap - EXIT
  echo "restart failed; stopping the partially started unified BBO chain" >&2
  stop_unified_chain
  exit "$status"
}
trap cleanup_on_exit EXIT

require_files
set -a
# shellcheck disable=SC1090
source "$env_file"
set +a
[[ -n "${CLICKHOUSE_BBO_PASSWORD:-}" ]] || die "CLICKHOUSE_BBO_PASSWORD is empty"
validate_configs
mkdir -p "$log_dir"

restart_in_progress=true
echo "stopping ClickHouse BBO chain"
stop_matching "$consumer_pattern"
stop_matching "$binance_okx_pattern"
stop_matching "$bitget_pattern"
stop_matching "$hl_aster_lighter_pattern"
sleep 1

echo "starting ClickHouse BBO producers"
start_logged "$log_dir/binance-okx.log" \
  "$producer_bin" --config "$binance_okx_cfg" --duration 0
binance_okx_pid=$started_pid
start_logged "$log_dir/bitget-bybit-gate.log" \
  "$producer_bin" --config "$bitget_cfg" --duration 0
bitget_pid=$started_pid
start_logged "$log_dir/hyperliquid-aster-lighter.log" \
  "$producer_bin" --config "$hl_aster_lighter_cfg" --duration 0
hl_aster_lighter_pid=$started_pid

sleep 1
require_alive "$binance_okx_pid" "binance-okx"
require_alive "$bitget_pid" "bitget-bybit-gate"
require_alive "$hl_aster_lighter_pid" "hyperliquid-aster-lighter"
wait_for_rings

echo "starting ClickHouse BBO consumer"
start_logged "$log_dir/clickhouse-bbo.log" \
  "$consumer_bin" --config "$consumer_cfg" --clickhouse-bbo
consumer_pid=$started_pid
sleep 2
require_alive "$consumer_pid" "clickhouse-bbo"
require_alive "$binance_okx_pid" "binance-okx"
require_alive "$bitget_pid" "bitget-bybit-gate"
require_alive "$hl_aster_lighter_pid" "hyperliquid-aster-lighter"

restart_in_progress=false
echo "ClickHouse BBO chain is up"
echo "  binance-okx                  $binance_okx_pid"
echo "  bitget-bybit-gate            $bitget_pid"
echo "  hyperliquid-aster-lighter    $hl_aster_lighter_pid"
echo "  clickhouse-bbo               $consumer_pid"
