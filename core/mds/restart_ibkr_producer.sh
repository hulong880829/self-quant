#!/usr/bin/env bash
# The IBKR ring cannot be replaced while the unified consumer stays attached.
# Always restart the full ClickHouse BBO chain.
set -euo pipefail

echo "IBKR producer restarts only through the unified BBO chain" >&2
exec "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/restart_recoder.sh" "$@"
