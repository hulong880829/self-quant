#!/usr/bin/env bash
set -euo pipefail

GO_VERSION="1.25.1"
NODE_VERSION="24.19.0"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"

BUILD_CORE=true
BUILD_BACKEND=true
BUILD_WEB=true
SELECTED_COMPONENT=""
RUN_TESTS=true
CLEAN=false
BUILD_TYPE="Release"
JOBS="$(nproc)"
CORE_CMAKE_ARGS=()

usage() {
  cat <<'EOF'
Usage: ./scripts/build.sh [options]

Build and validate all self-quant components.

Options:
  --core-only       Build only the C++ core.
  --backend-only    Build only the Go backend.
  --web-only        Build only the Next.js frontend.
  --skip-tests      Skip CTest, Go test/vet, and Web lint/typecheck.
  --clean           Remove selected generated build directories first.
  --debug           Build the C++ core in Debug mode.
  --jobs N          Set parallel C++ build jobs.
  -h, --help        Show this help.
EOF
}

select_component() {
  if [[ -n "${SELECTED_COMPONENT}" ]]; then
    echo "Only one of --core-only, --backend-only, or --web-only is allowed." >&2
    exit 2
  fi
  SELECTED_COMPONENT="$1"
}

while (($# > 0)); do
  case "$1" in
    --core-only) select_component core ;;
    --backend-only) select_component backend ;;
    --web-only) select_component web ;;
    --skip-tests) RUN_TESTS=false ;;
    --clean) CLEAN=true ;;
    --debug) BUILD_TYPE="Debug" ;;
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

if [[ -n "${SELECTED_COMPONENT}" ]]; then
  BUILD_CORE=false
  BUILD_BACKEND=false
  BUILD_WEB=false
  case "${SELECTED_COMPONENT}" in
    core) BUILD_CORE=true ;;
    backend) BUILD_BACKEND=true ;;
    web) BUILD_WEB=true ;;
  esac
fi

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Missing required command '$1'. Run ./scripts/bootstrap.sh first." >&2
    exit 1
  fi
}

if [[ "${BUILD_CORE}" == true ]]; then
  require_command cmake
  require_command ninja
  if command -v c++ >/dev/null 2>&1; then
    require_command pkg-config
    if ! pkg-config --exists openssl; then
      echo "OpenSSL development files are missing. Run bootstrap first." >&2
      exit 1
    fi
    if ! dpkg-query -W -f='${Status}' libsimdjson-dev 2>/dev/null |
         grep -q "install ok installed"; then
      echo "libsimdjson-dev is missing. Run bootstrap first." >&2
      exit 1
    fi
  elif python3 -c 'import ziglang' >/dev/null 2>&1 &&
       [[ -x "${REPO_ROOT}/core/tools/zig-cxx" &&
          -d "${REPO_ROOT}/core/.deps/sysroot" ]]; then
    case "$(uname -m)" in
      x86_64) multiarch="x86_64-linux-gnu" ;;
      aarch64|arm64) multiarch="aarch64-linux-gnu" ;;
      *) echo "Unsupported Zig fallback architecture: $(uname -m)" >&2; exit 1 ;;
    esac
    sysroot="${REPO_ROOT}/core/.deps/sysroot"
    CORE_CMAKE_ARGS+=(
      "-DCMAKE_CXX_COMPILER=${REPO_ROOT}/core/tools/zig-cxx"
      "-DCMAKE_AR=${REPO_ROOT}/core/tools/zig-ar"
      "-DCMAKE_RANLIB=${REPO_ROOT}/core/tools/zig-ranlib"
      "-DCMAKE_CXX_FLAGS=-isystem ${sysroot}/usr/include/${multiarch}"
      "-DOPENSSL_INCLUDE_DIR=${sysroot}/usr/include"
      "-DOPENSSL_SSL_LIBRARY=/usr/lib/${multiarch}/libssl.so.3"
      "-DOPENSSL_CRYPTO_LIBRARY=/usr/lib/${multiarch}/libcrypto.so.3"
      "-Dsimdjson_DIR=${sysroot}/usr/local/lib/cmake/simdjson")
    echo "System C++ compiler not found; using the validated Zig fallback."
  else
    echo "No usable C++20 compiler was found. Run bootstrap first." >&2
    exit 1
  fi
fi

if [[ "${BUILD_BACKEND}" == true ]]; then
  require_command go
  actual_go="$(go env GOVERSION)"
  if [[ "${actual_go}" != "go${GO_VERSION}" ]]; then
    echo "Go ${GO_VERSION} is required; found ${actual_go}." >&2
    exit 1
  fi
fi

if [[ "${BUILD_WEB}" == true ]]; then
  require_command node
  require_command npm
  if [[ "$(node --version)" != "v${NODE_VERSION}" ]]; then
    echo "Node ${NODE_VERSION} is required; found $(node --version)." >&2
    exit 1
  fi
fi

if [[ "${CLEAN}" == true ]]; then
  [[ "${BUILD_CORE}" == false ]] ||
    rm -rf "${REPO_ROOT}/core/build/release-bootstrap" \
           "${REPO_ROOT}/core/build/debug-bootstrap" \
           "${REPO_ROOT}/strategys/poly-mm/build-bootstrap"
  [[ "${BUILD_BACKEND}" == false ]] ||
    rm -rf "${REPO_ROOT}/backend/build"
  [[ "${BUILD_WEB}" == false ]] ||
    rm -rf "${REPO_ROOT}/web/.next"
fi

if [[ "${BUILD_CORE}" == true ]]; then
  core_build_name="release-bootstrap"
  if [[ "${BUILD_TYPE}" == "Debug" ]]; then
    core_build_name="debug-bootstrap"
  fi
  core_build_dir="${REPO_ROOT}/core/build/${core_build_name}"
  core_install_dir="${core_build_dir}/install"
  strategy_build_dir="${REPO_ROOT}/strategys/poly-mm/build-bootstrap"
  cmake -S "${REPO_ROOT}/core" -B "${core_build_dir}" -G Ninja \
    "-DCMAKE_BUILD_TYPE=${BUILD_TYPE}" \
    -DSELF_QUANT_ENABLE_WERROR=ON \
    -DSELF_QUANT_ENABLE_OMS=ON \
    -DSELF_QUANT_ENABLE_STRATEGYFRAME=ON \
    -DSELF_QUANT_INSTALL=ON \
    "-DCMAKE_INSTALL_PREFIX=${core_install_dir}" \
    "${CORE_CMAKE_ARGS[@]}"
  cmake --build "${core_build_dir}" --parallel "${JOBS}"
  if [[ "${RUN_TESTS}" == true ]]; then
    ctest --test-dir "${core_build_dir}" --output-on-failure
  fi
  cmake --install "${core_build_dir}"
  cmake -S "${REPO_ROOT}/strategys/poly-mm" -B "${strategy_build_dir}" \
    -G Ninja \
    "-DCMAKE_BUILD_TYPE=${BUILD_TYPE}" \
    "-DCMAKE_PREFIX_PATH=${core_install_dir}"
  cmake --build "${strategy_build_dir}" --parallel "${JOBS}"
  if [[ "${RUN_TESTS}" == true ]]; then
    ctest --test-dir "${strategy_build_dir}" --output-on-failure
  fi
fi

if [[ "${BUILD_BACKEND}" == true ]]; then
  (
    cd "${REPO_ROOT}/backend"
    go mod download
    mkdir -p build/bin
    go build -o build/bin/funding-service ./cmd/funding-service
    go build -o build/bin/account-service ./cmd/account-service
    go build -o build/bin/polymarket-service ./cmd/polymarket-service
    go build -o build/bin/report-service ./cmd/report-service
    go build -o build/bin/trader-service ./cmd/trader-service
    go build -o build/bin/ai-service ./cmd/ai-service
    go build -o build/bin/aggdata-service ./cmd/aggdata-service
    go build -o build/bin/spread-service ./cmd/spread-service
    go build -o build/bin/api-gateway ./cmd/api-gateway
    if [[ "${RUN_TESTS}" == true ]]; then
      go test ./...
      go vet ./...
    fi
  )
fi

if [[ "${BUILD_WEB}" == true ]]; then
  (
    cd "${REPO_ROOT}/web"
    npm ci
    if [[ "${RUN_TESTS}" == true ]]; then
      npm run lint
      npx tsc --noEmit
    fi
    npm run build
  )
fi

echo "self-quant build completed successfully."
