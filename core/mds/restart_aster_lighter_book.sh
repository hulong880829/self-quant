#!/usr/bin/env bash
# Manage the non-production Aster/Lighter full-order-book producer.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bin="${MDS_RELEASE_BIN:-$root/core/build-release/mds}"
config="$root/core/mds/config/aster-lighter-book.yaml"
log_dir="${MDS_ASTER_LIGHTER_BOOK_LOG_DIR:-/tmp/mds-aster-lighter-book}"
producer_bin="$bin/mds_producer"

book_pattern='mds_producer .*mds/config/aster-lighter-book.yaml'
ticker_pattern='mds_producer .*mds/config/aster-lighter.yaml'

die() { echo "error: $*" >&2; exit 1; }

matching_pids() {
  pgrep -f -- "$1" || true
}

stop_book() {
  local pids
  pids="$(matching_pids "$book_pattern")"
  if [[ -z "$pids" ]]; then
    echo "Aster/Lighter book producer is not running"
    return 0
  fi
  echo "stopping Aster/Lighter book producer (pid: ${pids//$'\n'/ })"
  # shellcheck disable=SC2086
  kill $pids 2>/dev/null || true
  local i
  for ((i = 0; i < 40; i++)); do
    [[ -z "$(matching_pids "$book_pattern")" ]] && return 0
    sleep 0.25
  done
  die "book producer did not stop cleanly"
}

start_book() {
  [[ -x "$producer_bin" ]] || die "missing binary $producer_bin"
  [[ -f "$config" ]] || die "missing config $config"
  [[ -z "$(matching_pids "$ticker_pattern")" ]] ||
    die "ticker producer is running; use a separate outbound IP for book tests"
  if [[ -n "$(matching_pids "$book_pattern")" ]]; then
    echo "Aster/Lighter book producer is already running"
    return 0
  fi
  mkdir -p "$log_dir"
  : >"$log_dir/producer.log"
  nohup "$producer_bin" --config "$config" --duration 0 \
    >>"$log_dir/producer.log" 2>&1 &
  echo "started Aster/Lighter book producer pid=$! (log $log_dir/producer.log)"
}

case "${1:-restart}" in
  start)
    start_book
    ;;
  stop)
    stop_book
    ;;
  restart)
    stop_book
    start_book
    ;;
  *)
    die "usage: $0 [start|stop|restart]"
    ;;
esac
