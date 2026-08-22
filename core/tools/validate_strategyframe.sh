#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
jobs="${JOBS:-2}"

configure_build_test() {
  local name="$1"
  shift
  cmake -S "${root}" -B "${root}/build/${name}" -G Ninja "$@"
  cmake --build "${root}/build/${name}" -j"${jobs}"
  ctest --test-dir "${root}/build/${name}" --output-on-failure --timeout 90
}

common=(
  -DSELF_QUANT_ENABLE_MDS=ON
  -DSELF_QUANT_ENABLE_OMS=ON
  -DSELF_QUANT_ENABLE_STRATEGYFRAME=ON
  -DSELF_QUANT_ENABLE_WERROR=ON
  -DSELF_QUANT_INSTALL=ON)

configure_build_test strategyframe-release \
  -DCMAKE_BUILD_TYPE=Release "${common[@]}"
STRATEGYFRAME_MAX_CALLBACK_P99_NS="${STRATEGYFRAME_MAX_CALLBACK_P99_NS:-100000}" \
  "${root}/build/strategyframe-release/strategyframe/strategyframe_benchmark" \
  | tee "${root}/build/strategyframe-release/strategyframe-benchmark.json"

configure_build_test strategyframe-asan \
  -DCMAKE_BUILD_TYPE=Debug "${common[@]}" \
  -DCMAKE_CXX_FLAGS="-fsanitize=address,undefined -fno-omit-frame-pointer" \
  -DCMAKE_EXE_LINKER_FLAGS="-fsanitize=address,undefined"

cmake -S "${root}" -B "${root}/build/strategyframe-tsan" -G Ninja \
  -DCMAKE_BUILD_TYPE=Debug "${common[@]}" \
  -DCMAKE_CXX_FLAGS="-fsanitize=thread -fno-omit-frame-pointer" \
  -DCMAKE_EXE_LINKER_FLAGS="-fsanitize=thread"
cmake --build "${root}/build/strategyframe-tsan" -j"${jobs}"
setarch "$(uname -m)" -R \
  ctest --test-dir "${root}/build/strategyframe-tsan" \
  --output-on-failure --timeout 90

bash "${root}/tests/install_package_smoke.sh" \
  "${root}" "${root}/build/strategyframe-package-smoke" ON ON ON

git -C "${root}/.." diff --check -- \
  core/CMakeLists.txt core/cmake core/mds core/net core/oms \
  core/strategyframe core/tests core/docs core/README.md

printf '%s\n' \
  "StrategyFrame offline validation complete." \
  "External Binance/Polymarket acceptance: PENDING unless explicitly authorized."
