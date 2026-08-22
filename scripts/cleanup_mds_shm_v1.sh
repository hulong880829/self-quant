#!/usr/bin/env bash
set -euo pipefail

if [[ "${1:-}" != "--confirm-stopped" ]]; then
  echo "usage: $0 --confirm-stopped [segment-prefix]" >&2
  exit 2
fi

prefix="${2:-selfquant.mds}"
if pgrep -f 'mds_(producer|aggregator|shm_consumer)|poly-mm' >/dev/null; then
  echo "refusing to remove SHM while an MDS C++ process is running" >&2
  exit 1
fi

shopt -s nullglob
segments=(/dev/shm/"${prefix}"*)
if ((${#segments[@]} == 0)); then
  echo "no matching SHM segments"
  exit 0
fi

rm -- "${segments[@]}"
echo "removed ${#segments[@]} old-schema SHM segment(s)"
