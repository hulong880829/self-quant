#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/restart_recoder.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

(
  unset MDS_BBO_RING_TIMEOUT_S MDS_IBKR_HEALTH_TIMEOUT_S
  # shellcheck disable=SC1090
  source "$script"
  [[ "$ring_timeout" == 120 ]]
  [[ "$ibkr_health_timeout" == 300 ]]
)
(
  export MDS_BBO_RING_TIMEOUT_S=45
  export MDS_IBKR_HEALTH_TIMEOUT_S=75
  # shellcheck disable=SC1090
  source "$script"
  [[ "$ring_timeout" == 45 ]]
  [[ "$ibkr_health_timeout" == 75 ]]
)

cat >"$tmp/config.yaml" <<'EOF'
shared_memory:
  prefix: /selfquant.mds.bbo
EOF
ring="$tmp/shm/selfquant.mds.bbo.ibkr.equity.ticker.shard0.leftover"
mkdir -p "$tmp/shm"
touch "$ring" "$tmp/shm/selfquant.mds.bbo.binance.spot.ticker.shard0.keep" \
  "$tmp/shm/other.ibkr.equity.ticker.shard0.keep"

(
  # shellcheck disable=SC1090
  source "$script"
  ibkr_cfg="$tmp/config.yaml"
  shm_root="$tmp/shm"
  matching_pids() { :; }
  remove_stopped_ibkr_ring
  remove_stopped_ibkr_ring
)
[[ ! -e "$ring" ]]
[[ -e "$tmp/shm/selfquant.mds.bbo.binance.spot.ticker.shard0.keep" ]]
[[ -e "$tmp/shm/other.ibkr.equity.ticker.shard0.keep" ]]

touch "$ring"
if (
  # shellcheck disable=SC1090
  source "$script"
  ibkr_cfg="$tmp/config.yaml"
  shm_root="$tmp/shm"
  matching_pids() { echo 99; }
  remove_stopped_ibkr_ring
  echo STARTED
) >"$tmp/running.out" 2>"$tmp/running.err"; then
  echo "ring removal continued while a process was running" >&2
  exit 1
fi
[[ -e "$ring" ]]
[[ "$(cat "$tmp/running.out")" != *STARTED* ]]

mkdir -p "$tmp/ro"
touch "$tmp/ro/selfquant.mds.bbo.ibkr.equity.ticker.shard0.stuck"
chmod a-w "$tmp/ro"
if (
  # shellcheck disable=SC1090
  source "$script"
  ibkr_cfg="$tmp/config.yaml"
  shm_root="$tmp/ro"
  matching_pids() { :; }
  remove_stopped_ibkr_ring
  echo STARTED
) >"$tmp/delete.out" 2>"$tmp/delete.err"; then
  echo "ring removal continued after delete failure" >&2
  exit 1
fi
[[ -e "$tmp/ro/selfquant.mds.bbo.ibkr.equity.ticker.shard0.stuck" ]]
[[ "$(cat "$tmp/delete.out")" != *STARTED* ]]
chmod u+w "$tmp/ro"

health="$tmp/ibkr.log"
cat >"$health" <<'EOF'
ibkr health ready=false selected=0 active=0 catalogs=0 bbo=0 session_generation=1 session_state=live reason=selected=0
EOF
if (
  # shellcheck disable=SC1090
  source "$script"
  latest_ibkr_health "$health"
  ibkr_health_is_ready
); then
  echo "zero selection was treated as ready" >&2
  exit 1
fi

cat >"$health" <<'EOF'
ibkr health ready=true selected=90 active=90 catalogs=90 bbo=0 session_generation=1 session_state=live
EOF
(
  # shellcheck disable=SC1090
  source "$script"
  latest_ibkr_health "$health"
  ibkr_health_is_ready || exit 1
  [[ "$ibkr_health_catalogs" == 90 && "$ibkr_health_bbo" == 0 ]]
)
printf '\n%s\n' \
  'ibkr health ready=true selected=90 active=90 catalogs=180 bbo=3 session_generation=1 session_state=live' \
  >>"$health"
(
  # shellcheck disable=SC1090
  source "$script"
  latest_ibkr_health "$health"
  [[ "$ibkr_health_catalogs" -gt 90 && "$ibkr_health_bbo" -gt 0 ]]
)

echo "IBKR ring cleanup checks passed"
