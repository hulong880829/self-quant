#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
DEPS_ROOT="${IBKR_DEPS_ROOT:-${REPO_ROOT}/core/.deps/ibkr}"
BUILD_DIR="${IBKR_BUILD_DIR:-${REPO_ROOT}/core/build-release}"
MANIFEST="${DEPS_ROOT}/manifest.env"
JOBS="$(nproc)"
RUN_TESTS=true
CLEAN=false

usage() {
  cat <<'EOF'
Usage: ./scripts/build-ibkr-mds.sh [options]

Build the TWS 10.45 IBKR producer and unified MDS consumer into
core/build-release/mds.

Options:
  --clean       Remove core/build-release before configuring.
  --jobs N      Set parallel build jobs.
  --skip-tests  Do not run the IBKR config, universe, and runtime tests.
  -h, --help    Show this help.
EOF
}

while (($# > 0)); do
  case "$1" in
    --clean) CLEAN=true ;;
    --skip-tests) RUN_TESTS=false ;;
    --jobs)
      shift
      if (($# == 0)) || [[ ! "$1" =~ ^[1-9][0-9]*$ ]]; then
        echo "--jobs requires a positive integer." >&2
        exit 2
      fi
      JOBS="$1"
      ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

if [[ ! -f "${MANIFEST}" ]]; then
  echo "Missing ${MANIFEST}." >&2
  echo "Run ./scripts/bootstrap-ibkr-deps.sh --accept-ib-license first." >&2
  exit 1
fi
# shellcheck disable=SC1090
source "${MANIFEST}"
: "${IBKR_TWS_API_ROOT:?manifest is missing IBKR_TWS_API_ROOT}"
: "${IBKR_DECIMAL_LIBRARY:?manifest is missing IBKR_DECIMAL_LIBRARY}"

[[ -f "${IBKR_TWS_API_ROOT}/API_VersionNum.txt" ]] || {
  echo "TWS API root is invalid: ${IBKR_TWS_API_ROOT}" >&2
  exit 1
}
[[ -f "${IBKR_DECIMAL_LIBRARY}" ]] || {
  echo "Intel decimal library is missing: ${IBKR_DECIMAL_LIBRARY}" >&2
  exit 1
}

if [[ "${CLEAN}" == true ]]; then
  rm -rf "${BUILD_DIR}"
fi

cmake_args=()
if [[ ! -f "${BUILD_DIR}/CMakeCache.txt" ]] &&
   command -v ninja >/dev/null 2>&1; then
  cmake_args=(-G Ninja)
fi
cmake -S "${REPO_ROOT}/core" -B "${BUILD_DIR}" "${cmake_args[@]}" \
  -DCMAKE_BUILD_TYPE=Release \
  -DSELF_QUANT_ENABLE_MDS=ON \
  -DSELF_QUANT_ENABLE_IBKR_MDS=ON \
  -DIBKR_DEPS_ROOT="${DEPS_ROOT}" \
  -DIBKR_TWS_API_VERSION=10.45 \
  -DIBKR_TWS_API_ROOT="${IBKR_TWS_API_ROOT}" \
  -DIBKR_DECIMAL_LIBRARY="${IBKR_DECIMAL_LIBRARY}"

cmake --build "${BUILD_DIR}" --parallel "${JOBS}" --target \
  mds_ibkr_producer mds_shm_consumer \
  mds_ibkr_config_tests mds_ibkr_universe_tests mds_ibkr_runtime_tests

if [[ "${RUN_TESTS}" == true ]]; then
  ctest --test-dir "${BUILD_DIR}" \
    -R 'mds_ibkr_(config|universe|runtime)_tests' --output-on-failure
fi

echo "IBKR MDS build completed."
echo "  Producer: ${BUILD_DIR}/mds/mds_ibkr_producer"
echo "  Consumer: ${BUILD_DIR}/mds/mds_shm_consumer"
