#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "Usage: run_fairprice.sh --config PATH [--validate-only]" >&2
  exit 2
}

config_path=""
validate_only=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --config)
      [ "$#" -ge 2 ] || usage
      config_path="$2"
      shift 2
      ;;
    --validate-only)
      validate_only="--validate-only"
      shift
      ;;
    --help|-h)
      usage
      ;;
    *)
      usage
      ;;
  esac
done

[ -n "$config_path" ] || usage
[ -f "$config_path" ] || {
  echo "missing config $config_path" >&2
  exit 1
}

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$script_dir/../.." && pwd)"

bin="${FAIRPRICE_BIN:-}"
if [ -z "$bin" ]; then
  for candidate in \
    "$root/core/build-release/fairprice/fairprice" \
    "$root/core/build/fairprice-release/fairprice/fairprice" \
    "$root/core/build/fairprice-release/fairprice" \
    "$root/core/build/fairprice/fairprice" \
    "$(command -v fairprice || true)"
  do
    if [ -n "$candidate" ] && [ -x "$candidate" ]; then
      bin="$candidate"
      break
    fi
  done
fi
if [ -z "$bin" ] || [ ! -x "$bin" ]; then
  echo "fairprice binary not found; build core/fairprice or set FAIRPRICE_BIN" >&2
  exit 1
fi

# shellcheck disable=SC2086
exec "$bin" --config "$config_path" $validate_only
