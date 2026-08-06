#!/usr/bin/env bash
set -euo pipefail

source_dir=${1:?usage: install_package_smoke.sh SOURCE_DIR WORK_DIR [MDS_ON_OR_OFF]}
work_dir=${2:?usage: install_package_smoke.sh SOURCE_DIR WORK_DIR [MDS_ON_OR_OFF]}
mds_mode=${3:-ON}

case "${mds_mode}" in
  ON|OFF) ;;
  *) echo "MDS mode must be ON or OFF" >&2; exit 2 ;;
esac

build_dir="${work_dir}/producer-${mds_mode}"
prefix_dir="${work_dir}/prefix-${mds_mode}"
consumer_dir="${work_dir}/consumer-${mds_mode}"
cmake -E remove_directory "${build_dir}"
cmake -E remove_directory "${prefix_dir}"
cmake -E remove_directory "${consumer_dir}"

cmake_args=(-G Ninja)
if [[ -x "${source_dir}/tools/zig-cxx" ]] &&
   python3 -c 'import ziglang' >/dev/null 2>&1; then
  cmake_args+=(
    "-DCMAKE_CXX_COMPILER=${source_dir}/tools/zig-cxx"
    "-DCMAKE_AR=${source_dir}/tools/zig-ar"
    "-DCMAKE_RANLIB=${source_dir}/tools/zig-ranlib")
fi

sysroot="${source_dir}/.deps/sysroot"
if [[ -d "${sysroot}/usr/include/x86_64-linux-gnu" ]]; then
  cmake_args+=(
    "-DCMAKE_CXX_FLAGS=-isystem ${sysroot}/usr/include/x86_64-linux-gnu")
fi
if [[ -f /usr/lib/x86_64-linux-gnu/libssl.so.3 &&
      -d "${sysroot}/usr/include/openssl" ]]; then
  cmake_args+=(
    "-DOPENSSL_INCLUDE_DIR=${sysroot}/usr/include"
    "-DOPENSSL_SSL_LIBRARY=/usr/lib/x86_64-linux-gnu/libssl.so.3"
    "-DOPENSSL_CRYPTO_LIBRARY=/usr/lib/x86_64-linux-gnu/libcrypto.so.3")
fi
if [[ -d "${sysroot}/usr/local/lib/cmake/simdjson" ]]; then
  cmake_args+=(
    "-Dsimdjson_DIR=${sysroot}/usr/local/lib/cmake/simdjson")
fi

cmake -S "${source_dir}" -B "${build_dir}" "${cmake_args[@]}" \
  -DCMAKE_BUILD_TYPE=Release \
  -DSELF_QUANT_ENABLE_MDS="${mds_mode}" \
  -DSELF_QUANT_UTILS_BUILD_TESTS=OFF \
  -DMDS_BUILD_TESTS=OFF \
  -DMDS_BUILD_EXAMPLES=OFF \
  -DCMAKE_INSTALL_PREFIX="${prefix_dir}"
cmake --build "${build_dir}" --parallel 2
cmake --install "${build_dir}"

cmake -S "${source_dir}/tests/package_smoke" -B "${consumer_dir}" \
  "${cmake_args[@]}" \
  -DCMAKE_BUILD_TYPE=Release \
  "-DCMAKE_PREFIX_PATH=${prefix_dir}"
cmake --build "${consumer_dir}" --parallel 2
"${consumer_dir}/utils_package_consumer"
if [[ "${mds_mode}" == ON ]]; then
  "${consumer_dir}/mds_package_consumer"
fi
