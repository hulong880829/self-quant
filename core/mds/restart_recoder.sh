#!/usr/bin/env bash
# Restart the unified ClickHouse BBO chain: crypto producers, the IBKR
# producer, then exactly one shm consumer. Never leave the old crypto
# consumer and this consumer writing crypto_bbo at the same time.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bin="${MDS_RELEASE_BIN:-$root/core/build-release/mds}"
config_dir="$root/core/mds/config"
env_file="$root/backend/.env"
log_dir="${MDS_BBO_LOG_DIR:-/tmp/mds-bbo}"
ring_timeout="${MDS_BBO_RING_TIMEOUT_S:-120}"
ibkr_health_timeout="${MDS_IBKR_HEALTH_TIMEOUT_S:-300}"
shm_root="${MDS_SHM_ROOT:-/dev/shm}"

producer_bin="$bin/mds_producer"
consumer_bin="$bin/mds_shm_consumer"
ibkr_producer_bin="$bin/mds_ibkr_producer"
binance_okx_cfg="$config_dir/binance-okx.yaml"
bitget_cfg="$config_dir/bitget-bybit-gate.yaml"
hl_aster_lighter_cfg="$config_dir/hyperliquid-aster-lighter.yaml"
ibkr_cfg="${MDS_IBKR_PRODUCER_CONFIG:-$config_dir/mds_ibkr.yaml}"
consumer_cfg="$config_dir/mds_clickhouse_bbo.example.yaml"

binance_okx_pattern='mds_producer .*mds/config/binance-okx[.]yaml'
bitget_pattern='mds_producer .*mds/config/bitget-bybit-gate[.]yaml'
hl_aster_lighter_pattern='mds_producer .*mds/config/hyperliquid-aster-lighter[.]yaml'
ibkr_producer_pattern='mds_ibkr_producer'
consumer_pattern='mds_shm_consumer .*mds_clickhouse_bbo'
ibkr_recorder_pattern='mds_shm_consumer .*mds_ibkr_clickhouse'
started_pids=()

die() { echo "error: $*" >&2; exit 1; }

require_files() {
  [[ -x "$producer_bin" ]] || die "missing binary $producer_bin"
  [[ -x "$consumer_bin" ]] || die "missing binary $consumer_bin"
  [[ -x "$ibkr_producer_bin" ]] ||
    die "missing binary $ibkr_producer_bin (enable SELF_QUANT_ENABLE_IBKR_MDS)"
  [[ -f "$binance_okx_cfg" ]] || die "missing config $binance_okx_cfg"
  [[ -f "$bitget_cfg" ]] || die "missing config $bitget_cfg"
  [[ -f "$hl_aster_lighter_cfg" ]] ||
    die "missing config $hl_aster_lighter_cfg"
  [[ -f "$ibkr_cfg" ]] || die "missing config $ibkr_cfg"
  [[ -f "$consumer_cfg" ]] || die "missing config $consumer_cfg"
  [[ -f "$env_file" ]] || die "missing $env_file (need CLICKHOUSE_PASSWORD)"
}

validate_configs() {
  echo "validating ClickHouse BBO configs"
  "$producer_bin" --config "$binance_okx_cfg" --validate-only
  "$producer_bin" --config "$bitget_cfg" --validate-only
  "$producer_bin" --config "$hl_aster_lighter_cfg" --validate-only
  "$ibkr_producer_bin" --config "$ibkr_cfg" --validate-only
  "$consumer_bin" --config "$consumer_cfg" --clickhouse-bbo --validate-only
}

matching_pids() {
  pgrep -f -- "$1" || true
}

ibkr_shm_prefix() {
  awk '/^shared_memory:/{found=1} found && $1=="prefix:"{print $2; exit}' \
    "$ibkr_cfg"
}

remove_stopped_ibkr_ring() {
  if [[ -n "$(matching_pids "$consumer_pattern")" ||
        -n "$(matching_pids "$ibkr_recorder_pattern")" ||
        -n "$(matching_pids "$ibkr_producer_pattern")" ]]; then
    die "refusing to remove the IBKR ring while a consumer or producer is running"
  fi
  local prefix shm_name matches path
  prefix="$(ibkr_shm_prefix)"
  [[ -n "$prefix" && "$prefix" == /* ]] ||
    die "producer config has no shared_memory.prefix"
  shm_name="${prefix#/}"
  [[ "$shm_name" != */* && "$shm_name" != *'*'* && "$shm_name" != *'?'* &&
     "$shm_name" != *'['* ]] ||
    die "refusing unsafe IBKR shared-memory prefix"
  shopt -s nullglob
  matches=( "$shm_root/${shm_name}.ibkr.equity.ticker.shard0."* )
  shopt -u nullglob
  for path in "${matches[@]}"; do
    rm -f -- "$path" || die "failed to remove IBKR ring $path"
    [[ ! -e "$path" ]] || die "IBKR ring still exists: $path"
  done
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
  started_pids+=("$started_pid")
}

process_alive() {
  kill -0 "$1" 2>/dev/null
}

require_alive() {
  local pid=$1
  local label=$2
  process_alive "$pid" || die "$label exited during startup; check $log_dir/$label.log"
}

configured_selectors() {
  awk '
    $1 == "-" && $2 == "venue:" { venue = $3 }
    $1 == "product:" { product = $2 }
    $1 == "shard:" { print venue, product, $2 }
  ' "$consumer_cfg"
}

ring_present() {
  local venue=$1 product=$2 shard=$3
  compgen -G \
    "/dev/shm/selfquant.mds.bbo.${venue}.${product}.ticker.shard${shard}.*" \
    >/dev/null
}

selectors_ready() {
  local skip_venue=${1:-}
  local venue product shard
  while read -r venue product shard; do
    [[ -n "$skip_venue" && "$venue" == "$skip_venue" ]] && continue
    ring_present "$venue" "$product" "$shard" || return 1
  done < <(configured_selectors)
}

selector_count() {
  local skip_venue=${1:-}
  configured_selectors | awk -v skip="$skip_venue" '$1 != skip { count++ } END { print count + 0 }'
}

wait_for_selectors() {
  local skip_venue=${1:-}
  local label=$2
  local expected
  expected="$(selector_count "$skip_venue")"
  local deadline=$((SECONDS + ring_timeout))
  echo "waiting up to ${ring_timeout}s for ${expected} ${label} rings"
  while ((SECONDS < deadline)); do
    require_alive "$binance_okx_pid" "binance-okx"
    require_alive "$bitget_pid" "bitget-bybit-gate"
    require_alive "$hl_aster_lighter_pid" "hyperliquid-aster-lighter"
    if [[ -n "${ibkr_pid:-}" ]]; then
      require_alive "$ibkr_pid" "ibkr"
    fi
    if selectors_ready "$skip_venue"; then
      echo "${expected} ${label} rings are ready"
      return 0
    fi
    sleep 1
  done
  die "timed out waiting for ${expected} ${label} rings"
}

stop_started() {
  local pid
  for pid in "${started_pids[@]}"; do
    if process_alive "$pid"; then
      echo "stopping pid $pid started by this run" >&2
      kill "$pid" 2>/dev/null || true
    fi
  done
}

restart_in_progress=false
cleanup_on_exit() {
  local status=$?
  if ((status == 0)) || [[ "$restart_in_progress" != true ]]; then
    return
  fi
  trap - EXIT
  echo "restart failed; stopping only processes started by this run" >&2
  stop_started
  exit "$status"
}
trap cleanup_on_exit EXIT

parse_ibkr_health_line() {
  local line=$1
  [[ "$line" =~ ibkr[[:space:]]health[[:space:]]ready=(true|false)[[:space:]]selected=([0-9]+)[[:space:]]active=([0-9]+)[[:space:]]catalogs=([0-9]+)[[:space:]]bbo=([0-9]+) ]] ||
    return 1
  ibkr_health_ready="${BASH_REMATCH[1]}"
  ibkr_health_selected="${BASH_REMATCH[2]}"
  ibkr_health_active="${BASH_REMATCH[3]}"
  ibkr_health_catalogs="${BASH_REMATCH[4]}"
  ibkr_health_bbo="${BASH_REMATCH[5]}"
}

latest_ibkr_health() {
  local file=$1
  [[ -f "$file" ]] || return 1
  local line
  line="$(grep 'ibkr health ' "$file" | tail -n 1 || true)"
  [[ -n "$line" ]] || return 1
  parse_ibkr_health_line "$line"
}

ibkr_health_is_ready() {
  [[ "${ibkr_health_ready:-false}" == true &&
     "${ibkr_health_selected:-0}" -gt 0 &&
     "${ibkr_health_active:-0}" -gt 0 ]]
}

wait_for_ibkr_health() {
  local file=$1
  local mode=$2
  local baseline=${3:-0}
  local deadline=$((SECONDS + ibkr_health_timeout))
  while ((SECONDS < deadline)); do
    if [[ -n "${ibkr_pid:-}" ]]; then
      require_alive "$ibkr_pid" "ibkr"
    fi
    if latest_ibkr_health "$file"; then
      case "$mode" in
        ready)
          if ibkr_health_is_ready; then
            return 0
          fi
          ;;
        catalog)
          if ((ibkr_health_catalogs > baseline)); then
            return 0
          fi
          ;;
        bbo)
          if ((ibkr_health_bbo > 0)); then
            return 0
          fi
          ;;
        *)
          die "unknown IBKR health wait mode: $mode"
          ;;
      esac
    fi
    sleep 1
  done
  die "timed out waiting for IBKR health mode=$mode"
}

if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then
  return 0
fi

require_files
set -a
# shellcheck disable=SC1090
source "$env_file"
set +a
[[ -n "${CLICKHOUSE_PASSWORD:-}" ]] || die "CLICKHOUSE_PASSWORD is empty"
validate_configs
mkdir -p "$log_dir"

restart_in_progress=true
echo "stopping old consumers before any producer restart"
stop_matching "$consumer_pattern"
stop_matching "$ibkr_recorder_pattern"
stop_matching "$binance_okx_pattern"
stop_matching "$bitget_pattern"
stop_matching "$hl_aster_lighter_pattern"
stop_matching "$ibkr_producer_pattern"
sleep 1
if [[ -n "$(matching_pids "$consumer_pattern")" ||
      -n "$(matching_pids "$ibkr_recorder_pattern")" ||
      -n "$(matching_pids "$ibkr_producer_pattern")" ]]; then
  die "an old ClickHouse consumer or IBKR producer is still running"
fi
remove_stopped_ibkr_ring

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
wait_for_selectors ibkr "crypto"

echo "starting IBKR producer"
start_logged "$log_dir/ibkr.log" \
  "$ibkr_producer_bin" --config "$ibkr_cfg"
ibkr_pid=$started_pid
sleep 1
require_alive "$ibkr_pid" "ibkr"
wait_for_selectors "" "configured"
echo "waiting for IBKR producer health"
wait_for_ibkr_health "$log_dir/ibkr.log" ready
ibkr_catalog_baseline="$ibkr_health_catalogs"

echo "starting the only ClickHouse BBO consumer"
start_logged "$log_dir/clickhouse-bbo.log" \
  "$consumer_bin" --config "$consumer_cfg" --clickhouse-bbo
consumer_pid=$started_pid
sleep 2
require_alive "$consumer_pid" "clickhouse-bbo"
echo "waiting for IBKR catalog republish after consumer attach"
wait_for_ibkr_health "$log_dir/ibkr.log" catalog "$ibkr_catalog_baseline"
if [[ "${MDS_IBKR_REQUIRE_BBO:-0}" == 1 ]]; then
  echo "waiting for the first IBKR BBO"
  wait_for_ibkr_health "$log_dir/ibkr.log" bbo
fi
require_alive "$binance_okx_pid" "binance-okx"
require_alive "$bitget_pid" "bitget-bybit-gate"
require_alive "$hl_aster_lighter_pid" "hyperliquid-aster-lighter"
require_alive "$ibkr_pid" "ibkr"
if [[ -n "$(matching_pids "$ibkr_recorder_pattern")" ]]; then
  die "independent IB recorder is running beside the unified consumer"
fi

restart_in_progress=false
echo "ClickHouse BBO chain is up"
echo "  binance-okx                  $binance_okx_pid"
echo "  bitget-bybit-gate            $bitget_pid"
echo "  hyperliquid-aster-lighter    $hl_aster_lighter_pid"
echo "  ibkr                         $ibkr_pid"
echo "  clickhouse-bbo               $consumer_pid"
