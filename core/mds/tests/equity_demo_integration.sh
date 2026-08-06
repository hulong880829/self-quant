#!/usr/bin/env bash
set -euo pipefail

publisher=$1
strategy=$2
work=$(mktemp -d)
segment="/self-quant.equity.integration.$$.4"
producer_pid=

cleanup() {
  if [[ -n "${producer_pid}" ]]; then
    kill "${producer_pid}" 2>/dev/null || true
    wait "${producer_pid}" 2>/dev/null || true
  fi
  rm -rf "${work}"
}
trap cleanup EXIT

expect_usage_error() {
  local output=$1
  shift
  local status=0
  "${publisher}" "$@" >"${output}" 2>&1 || status=$?
  [[ ${status} -eq 2 ]]
}

expect_usage_error "${work}/zero-min.log" \
  --min-interval-ms 0 --max-interval-ms 3
expect_usage_error "${work}/reversed-range.log" \
  --min-interval-ms 4 --max-interval-ms 3
"${publisher}" --segment "${segment}.fixed" --ring-bytes 4096 \
  --max-record-bytes 512 --symbols 1 --min-interval-ms 2 \
  --max-interval-ms 2 --seed 7 --duration 1 \
  >"${work}/fixed-range.log" 2>&1

"${publisher}" --segment "${segment}" --ring-bytes 4096 \
  --max-record-bytes 512 --min-interval-ms 2 --max-interval-ms 4 \
  --seed 20260805 --duration 4 \
  >"${work}/publisher.log" 2>&1 &
producer_pid=$!
sleep 0.2

run_strategy() {
  local name=$1
  local output=$2
  local status=0
  timeout --signal=TERM 1.2 "${strategy}" --segment "${segment}" \
    --process-ms 90 --name "${name}" >"${output}" 2>&1 || status=$?
  [[ ${status} -eq 0 || ${status} -eq 124 ]]
}

run_strategy before-restart "${work}/before.log"
sleep 0.2
run_strategy after-restart "${work}/after.log"
wait "${producer_pid}"
producer_pid=

awk '
  /idle_wait=BUSY_SPIN/ { found = 1 }
  END { exit found ? 0 : 1 }
' "${work}/before.log"

awk '
  / lost=[1-9][0-9]*/ { found = 1 }
  END { exit found ? 0 : 1 }
' "${work}/before.log"

before_last=$(awk '
  {
    for (i = 1; i <= NF; ++i) {
      if ($i ~ /^bus_seq=/) {
        split($i, value, "=")
        sequence = value[2]
      }
    }
  }
  END { if (sequence == "") exit 1; print sequence }
' "${work}/before.log")
after_first=$(awk '
  {
    for (i = 1; i <= NF; ++i) {
      if ($i ~ /^bus_seq=/) {
        split($i, value, "=")
        print value[2]
        exit
      }
    }
  }
' "${work}/after.log")

[[ -n "${after_first}" && ${after_first} -gt ${before_last} ]]
