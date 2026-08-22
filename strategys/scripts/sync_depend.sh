#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
build="${root}/core/build-strategy-sdk"
prefix="${root}/strategys/depend"

cmake -E remove_directory "${build}"
cmake -E remove_directory "${prefix}"
cmake -S "${root}/core" -B "${build}" \
  -DCMAKE_BUILD_TYPE=RelWithDebInfo \
  -DSELF_QUANT_ENABLE_MDS=ON \
  -DSELF_QUANT_ENABLE_OMS=ON \
  -DSELF_QUANT_ENABLE_STRATEGYFRAME=ON \
  -DSELF_QUANT_INSTALL=ON \
  -DSELF_QUANT_BUILD_PACKAGE_TESTS=OFF \
  -DCMAKE_INSTALL_PREFIX="${prefix}"
cmake --build "${build}" -j "${BUILD_JOBS:-4}"
cmake --install "${build}"
