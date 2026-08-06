#!/usr/bin/env bash
set -euo pipefail

GO_VERSION="1.25.1"
NODE_VERSION="24.19.0"
MIN_CMAKE_VERSION="3.20.0"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"

SKIP_DOCKER=false
SKIP_DEPS=false
CREATE_ENV_FILES=true

usage() {
  cat <<'EOF'
Usage: ./scripts/bootstrap.sh [options]

Install the self-quant development environment on Ubuntu 24.04.

Options:
  --skip-docker   Do not install Docker Engine and Compose.
  --skip-deps     Do not run go mod download or npm ci.
  --no-env-files  Do not create local env files from templates.
  -h, --help      Show this help.
EOF
}

while (($# > 0)); do
  case "$1" in
    --skip-docker) SKIP_DOCKER=true ;;
    --skip-deps) SKIP_DEPS=true ;;
    --no-env-files) CREATE_ENV_FILES=false ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

if [[ ! -r /etc/os-release ]]; then
  echo "Cannot identify the operating system: /etc/os-release is missing." >&2
  exit 1
fi
# shellcheck disable=SC1091
source /etc/os-release
if [[ "${ID:-}" != "ubuntu" || "${VERSION_ID:-}" != "24.04" ]]; then
  echo "Ubuntu 24.04 LTS is required; found ${PRETTY_NAME:-unknown}." >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64)
    GO_ARCH="amd64"
    NODE_ARCH="x64"
    ;;
  aarch64|arm64)
    GO_ARCH="arm64"
    NODE_ARCH="arm64"
    ;;
  *)
    echo "Unsupported architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

if ((EUID == 0)); then
  SUDO=()
  TARGET_USER="${SUDO_USER:-root}"
else
  if ! command -v sudo >/dev/null 2>&1; then
    echo "sudo is required to install system packages." >&2
    exit 1
  fi
  SUDO=(sudo)
  TARGET_USER="${USER}"
fi

export DEBIAN_FRONTEND=noninteractive
"${SUDO[@]}" apt-get update
"${SUDO[@]}" apt-get install -y --no-install-recommends \
  bash build-essential ca-certificates cmake coreutils curl git \
  libsimdjson-dev libssl-dev ninja-build pkg-config python3

version_ge() {
  [[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" == "$2" ]]
}

cmake_version="$(cmake --version | awk 'NR == 1 {print $3}')"
if ! version_ge "${cmake_version}" "${MIN_CMAKE_VERSION}"; then
  echo "CMake ${MIN_CMAKE_VERSION}+ is required; found ${cmake_version}." >&2
  exit 1
fi

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

install_go() {
  if command -v go >/dev/null 2>&1 &&
     [[ "$(go version | awk '{print $3}')" == "go${GO_VERSION}" ]]; then
    echo "Go ${GO_VERSION} is already installed."
    return
  fi

  local archive="go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
  curl --fail --location --retry 3 \
    "https://go.dev/dl/${archive}" -o "${TMP_DIR}/${archive}"
  curl --fail --location --retry 3 \
    "https://go.dev/dl/${archive}.sha256" -o "${TMP_DIR}/${archive}.sha256"
  printf '%s  %s\n' "$(tr -d '[:space:]' < "${TMP_DIR}/${archive}.sha256")" \
    "${TMP_DIR}/${archive}" | sha256sum --check -

  "${SUDO[@]}" rm -rf /usr/local/go
  "${SUDO[@]}" tar -C /usr/local -xzf "${TMP_DIR}/${archive}"
  "${SUDO[@]}" ln -sfn /usr/local/go/bin/go /usr/local/bin/go
  "${SUDO[@]}" ln -sfn /usr/local/go/bin/gofmt /usr/local/bin/gofmt
}

install_node() {
  if command -v node >/dev/null 2>&1 &&
     [[ "$(node --version)" == "v${NODE_VERSION}" ]]; then
    echo "Node.js ${NODE_VERSION} is already installed."
    return
  fi

  local directory="node-v${NODE_VERSION}-linux-${NODE_ARCH}"
  local archive="${directory}.tar.xz"
  curl --fail --location --retry 3 \
    "https://nodejs.org/dist/v${NODE_VERSION}/${archive}" \
    -o "${TMP_DIR}/${archive}"
  curl --fail --location --retry 3 \
    "https://nodejs.org/dist/v${NODE_VERSION}/SHASUMS256.txt" \
    -o "${TMP_DIR}/node-SHASUMS256.txt"
  (
    cd "${TMP_DIR}"
    grep " ${archive}\$" node-SHASUMS256.txt | sha256sum --check -
  )

  "${SUDO[@]}" install -d /usr/local/lib/nodejs
  "${SUDO[@]}" rm -rf "/usr/local/lib/nodejs/${directory}"
  "${SUDO[@]}" tar -C /usr/local/lib/nodejs -xJf "${TMP_DIR}/${archive}"
  for executable in node npm npx corepack; do
    "${SUDO[@]}" ln -sfn \
      "/usr/local/lib/nodejs/${directory}/bin/${executable}" \
      "/usr/local/bin/${executable}"
  done
}

install_docker() {
  "${SUDO[@]}" install -m 0755 -d /etc/apt/keyrings
  curl --fail --location --retry 3 \
    https://download.docker.com/linux/ubuntu/gpg |
    "${SUDO[@]}" tee /etc/apt/keyrings/docker.asc >/dev/null
  "${SUDO[@]}" chmod a+r /etc/apt/keyrings/docker.asc

  local docker_arch
  docker_arch="$(dpkg --print-architecture)"
  printf '%s\n' \
    "deb [arch=${docker_arch} signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" |
    "${SUDO[@]}" tee /etc/apt/sources.list.d/docker.list >/dev/null
  "${SUDO[@]}" apt-get update
  "${SUDO[@]}" apt-get install -y --no-install-recommends \
    containerd.io docker-buildx-plugin docker-ce docker-ce-cli \
    docker-compose-plugin

  if [[ "${TARGET_USER}" != "root" ]]; then
    "${SUDO[@]}" usermod -aG docker "${TARGET_USER}"
  fi
}

install_go
install_node
hash -r
if [[ "${SKIP_DOCKER}" == false ]]; then
  install_docker
fi

if [[ "${SKIP_DEPS}" == false ]]; then
  (
    cd "${REPO_ROOT}/backend"
    go mod download
  )
  (
    cd "${REPO_ROOT}/web"
    npm ci
  )
fi

if [[ "${CREATE_ENV_FILES}" == true ]]; then
  if [[ ! -e "${REPO_ROOT}/backend/.env" ]]; then
    cp "${REPO_ROOT}/backend/.env.example" "${REPO_ROOT}/backend/.env"
  fi
  if [[ ! -e "${REPO_ROOT}/web/.env.local" ]]; then
    cp "${REPO_ROOT}/web/.env.example" "${REPO_ROOT}/web/.env.local"
  fi
fi

echo
echo "self-quant bootstrap completed."
echo "  CMake:  $(cmake --version | awk 'NR == 1 {print $3}')"
echo "  C++:    $(c++ --version | head -n1)"
echo "  Go:     $(go version)"
echo "  Node:   $(node --version)"
echo "  npm:    $(npm --version)"
echo "  OpenSSL: $(pkg-config --modversion openssl)"
echo "  simdjson package: $(dpkg-query -W -f='${Version}' libsimdjson-dev)"
if [[ "${SKIP_DOCKER}" == false ]]; then
  echo "  Docker: $(docker --version)"
  echo "  Compose: $(docker compose version)"
  if [[ "${TARGET_USER}" != "root" ]]; then
    echo
    echo "Log out and back in (or run 'newgrp docker') before using Docker."
  fi
fi
echo
echo "Next: ${REPO_ROOT}/scripts/build.sh"
