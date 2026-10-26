#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  cat <<'EOF'
Usage:
  ./build.sh                  Build/install the SDK, then build tradefi-perps-mm
  ./build.sh --strategy-only  Build tradefi-perps-mm with the existing SDK only
  ./build.sh --help           Show this help
EOF
}

mode=all
case "${1:-}" in
  "") ;;
  --strategy-only) mode=strategy-only ;;
  --help|-h) usage; exit 0 ;;
  *) usage >&2; exit 2 ;;
esac

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "${script_dir}/../.." && pwd)"
core_dir="${repo_root}/core"
sdk_dir="${repo_root}/strategys/depend"
core_build_dir="${CORE_BUILD_DIR:-${core_dir}/build-strategy-sdk}"
strategy_build_dir="${STRATEGY_BUILD_DIR:-${script_dir}/build}"
jobs="${JOBS:-$(nproc)}"

if [[ "${mode}" == "all" ]]; then
  echo "==> Building self_quant SDK"
  cmake -S "${core_dir}" -B "${core_build_dir}" \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_INSTALL_PREFIX="${sdk_dir}" \
    -DCMAKE_PREFIX_PATH="${sdk_dir}" \
    -DBUILD_TESTING=OFF \
    -DSELF_QUANT_ENABLE_MDS=ON \
    -DSELF_QUANT_ENABLE_OMS=ON \
    -DSELF_QUANT_ENABLE_STRATEGYFRAME=ON \
    -DSELF_QUANT_INSTALL=ON \
    -DSELF_QUANT_BUILD_BENCHMARKS=OFF \
    -DSELF_QUANT_BUILD_PACKAGE_TESTS=OFF \
    -DSELF_QUANT_UTILS_BUILD_TESTS=OFF \
    -DMDS_BUILD_TESTS=OFF \
    -DMDS_BUILD_EXAMPLES=OFF \
    -DMDS_ENABLE_SIMDJSON=ON \
    -DOMS_BUILD_TESTS=OFF \
    -DOMS_BUILD_ACCEPTANCE=OFF \
    -DSTRATEGYFRAME_BUILD_TESTS=OFF \
    -DSTRATEGYFRAME_BUILD_EXAMPLES=OFF
  cmake --build "${core_build_dir}" --parallel "${jobs}"
  cmake --install "${core_build_dir}" --prefix "${sdk_dir}"
fi

sdk_config="${sdk_dir}/lib/cmake/self_quant/self_quant-config.cmake"
if [[ ! -f "${sdk_config}" ]]; then
  echo "self_quant SDK not found: ${sdk_config}" >&2
  exit 1
fi

echo "==> Building tradefi-perps-mm with SDK: ${sdk_dir}"
cmake -S "${script_dir}" -B "${strategy_build_dir}" \
  -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_PREFIX_PATH="${sdk_dir}" \
  -Dself_quant_DIR="${sdk_dir}/lib/cmake/self_quant"
cmake --build "${strategy_build_dir}" --parallel "${jobs}"
ctest --test-dir "${strategy_build_dir}" --output-on-failure
echo "Done: ${strategy_build_dir}/tradefi-perps-mm"
