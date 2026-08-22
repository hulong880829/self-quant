#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
jobs="${JOBS:-2}"

configure_build_test() {
  local name="$1"
  shift
  cmake -S "${root}" -B "${root}/build/${name}" -G Ninja "$@"
  cmake --build "${root}/build/${name}" -j"${jobs}"
  ctest --test-dir "${root}/build/${name}" --output-on-failure --timeout 60
}

configure_build_test phase4-release-oms \
  -DCMAKE_BUILD_TYPE=Release \
  -DSELF_QUANT_ENABLE_MDS=OFF \
  -DSELF_QUANT_ENABLE_OMS=ON \
  -DSELF_QUANT_ENABLE_WERROR=ON \
  -DSELF_QUANT_INSTALL=OFF

configure_build_test phase4-release-all \
  -DCMAKE_BUILD_TYPE=Release \
  -DSELF_QUANT_ENABLE_MDS=ON \
  -DSELF_QUANT_ENABLE_OMS=ON \
  -DSELF_QUANT_ENABLE_WERROR=ON \
  -DSELF_QUANT_INSTALL=OFF

configure_build_test phase4-asan \
  -DCMAKE_BUILD_TYPE=Debug \
  -DSELF_QUANT_ENABLE_MDS=OFF \
  -DSELF_QUANT_ENABLE_OMS=ON \
  -DSELF_QUANT_ENABLE_WERROR=ON \
  -DSELF_QUANT_INSTALL=OFF \
  -DCMAKE_CXX_FLAGS="-fsanitize=address,undefined -fno-omit-frame-pointer" \
  -DCMAKE_EXE_LINKER_FLAGS="-fsanitize=address,undefined"

cmake -S "${root}" -B "${root}/build/phase4-tsan" -G Ninja \
  -DCMAKE_BUILD_TYPE=Debug \
  -DSELF_QUANT_ENABLE_MDS=OFF \
  -DSELF_QUANT_ENABLE_OMS=ON \
  -DSELF_QUANT_ENABLE_WERROR=ON \
  -DSELF_QUANT_INSTALL=OFF \
  -DCMAKE_CXX_FLAGS="-fsanitize=thread -fno-omit-frame-pointer" \
  -DCMAKE_EXE_LINKER_FLAGS="-fsanitize=thread"
cmake --build "${root}/build/phase4-tsan" -j"${jobs}"
setarch "$(uname -m)" -R \
  ctest --test-dir "${root}/build/phase4-tsan" \
  --output-on-failure --timeout 60

git -C "${root}/.." diff --check -- \
  core/CMakeLists.txt core/cmake core/net core/oms core/docs

printf '%s\n' \
  "Offline phase-4 validation complete." \
  "External Binance/Polymarket acceptance: PENDING (no credentials/live access)."
