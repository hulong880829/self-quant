#!/usr/bin/env bash
set -euo pipefail

TWS_VERSION="10.45.01"
TWS_ARCHIVE="twsapi_macunix.${TWS_VERSION}.zip"
TWS_URL="https://interactivebrokers.github.io/downloads/${TWS_ARCHIVE}"
# Fingerprint of API_VersionNum.txt plus the original C++ client/proto sources.
# Generated *.pb.{h,cc,cpp} files are intentionally excluded.
TWS_SOURCE_SHA256="652d5869c4091e9adc59dea1a98d194f6387cea004ebba9593658de49343a2f6"
RDFP_VERSION="20U5"
RDFP_ARCHIVE="IntelRDFPMathLib${RDFP_VERSION}.tar.gz"
RDFP_URL="https://www.netlib.org/misc/intel/${RDFP_ARCHIVE}"
RDFP_SHA256="85dafd70f0fe2a8da218ade4233fca9d3228b0b04cd6d8527f7499926be01037"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
DEPS_ROOT="${IBKR_DEPS_ROOT:-${REPO_ROOT}/core/.deps/ibkr}"
DOWNLOAD_DIR="${DEPS_ROOT}/downloads"
SOURCE_DIR="${DEPS_ROOT}/src"
PREFIX_DIR="${DEPS_ROOT}/prefix"
TWS_INSTALL_DIR="${SOURCE_DIR}/twsapi-${TWS_VERSION}"
TWS_ROOT="${TWS_INSTALL_DIR}/IBJts"
DECIMAL_LIBRARY="${PREFIX_DIR}/lib/libbid.a"
VENDORED_RDFP="${REPO_ROOT}/third_party/intel-rdfp"

ACCEPT_LICENSE=false
FORCE=false
TWS_ARCHIVE_INPUT=""
TWS_SOURCE_INPUT=""
LIBBID_SOURCE="auto"
RDFP_ARCHIVE_INPUT=""

usage() {
  cat <<'EOF'
Usage: ./scripts/bootstrap-ibkr-deps.sh --accept-ib-license [options]

Prepare the pinned TWS API 10.45 and Intel decimal library under
core/.deps/ibkr. Downloading or using the TWS API means you accept the
applicable Interactive Brokers API license.

Options:
  --accept-ib-license     Required explicit license acknowledgement.
  --tws-archive PATH     Use a locally downloaded official 10.45 zip.
  --tws-source PATH      Import an existing official 10.45 IBJts directory.
  --libbid-source SOURCE auto, apt, netlib, or vendored (default: auto).
                         auto downloads the pinned Intel RDFP archive.
  --rdfp-archive PATH    Use a local Intel RDFP tar.gz instead of downloading.
  --force                Revalidate and rebuild dependency outputs.
  -h, --help             Show this help.
EOF
}

while (($# > 0)); do
  case "$1" in
    --accept-ib-license) ACCEPT_LICENSE=true ;;
    --tws-archive)
      shift
      (($# > 0)) || { echo "--tws-archive requires a path" >&2; exit 2; }
      TWS_ARCHIVE_INPUT="$1"
      ;;
    --tws-source)
      shift
      (($# > 0)) || { echo "--tws-source requires a path" >&2; exit 2; }
      TWS_SOURCE_INPUT="$1"
      ;;
    --libbid-source)
      shift
      (($# > 0)) || { echo "--libbid-source requires a value" >&2; exit 2; }
      LIBBID_SOURCE="$1"
      ;;
    --rdfp-archive)
      shift
      (($# > 0)) || { echo "--rdfp-archive requires a path" >&2; exit 2; }
      RDFP_ARCHIVE_INPUT="$1"
      ;;
    --force) FORCE=true ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

if [[ "${ACCEPT_LICENSE}" != true ]]; then
  echo "TWS API license acknowledgement is required." >&2
  echo "Re-run with --accept-ib-license after reviewing the IB API license." >&2
  exit 2
fi
case "${LIBBID_SOURCE}" in
  auto|apt|netlib|vendored) ;;
  *) echo "Invalid --libbid-source: ${LIBBID_SOURCE}" >&2; exit 2 ;;
esac

for command in awk curl find make nm sha256sum sort tar unzip xargs cc c++; do
  command -v "${command}" >/dev/null 2>&1 || {
    echo "Missing ${command}; run ./scripts/bootstrap.sh first." >&2
    exit 1
  }
done

mkdir -p "${DOWNLOAD_DIR}" "${SOURCE_DIR}" "${PREFIX_DIR}/lib"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

tws_fingerprint() {
  local root=$1
  (
    cd "${root}"
    find API_VersionNum.txt source/cppclient/client source/proto \
      -type f \
      ! -name '*.pb.cpp' ! -name '*.pb.cc' ! -name '*.pb.h' \
      -print0 |
      sort -z |
      xargs -0 sha256sum
  ) | sha256sum | awk '{print $1}'
}

validate_tws_root() {
  local root=$1
  [[ -f "${root}/API_VersionNum.txt" ]] || return 1
  [[ "$(tr -d '\r\n' < "${root}/API_VersionNum.txt")" == \
     "API_Version=${TWS_VERSION}" ]] || return 1
  [[ -f "${root}/source/cppclient/client/EClientSocket.h" ]] || return 1
  [[ -d "${root}/source/proto" ]] || return 1
  [[ "$(tws_fingerprint "${root}")" == "${TWS_SOURCE_SHA256}" ]]
}

install_tws() {
  if [[ "${FORCE}" != true ]] && validate_tws_root "${TWS_ROOT}"; then
    echo "TWS API ${TWS_VERSION} is already prepared."
    return
  fi

  local extracted="${TMP_DIR}/tws"
  mkdir -p "${extracted}"
  if [[ -n "${TWS_SOURCE_INPUT}" ]]; then
    [[ -d "${TWS_SOURCE_INPUT}" ]] || {
      echo "TWS source directory does not exist: ${TWS_SOURCE_INPUT}" >&2
      exit 1
    }
    cp -a "${TWS_SOURCE_INPUT}" "${extracted}/IBJts"
  else
    local archive="${DOWNLOAD_DIR}/${TWS_ARCHIVE}"
    if [[ -n "${TWS_ARCHIVE_INPUT}" ]]; then
      [[ -f "${TWS_ARCHIVE_INPUT}" ]] || {
        echo "TWS archive does not exist: ${TWS_ARCHIVE_INPUT}" >&2
        exit 1
      }
      cp "${TWS_ARCHIVE_INPUT}" "${archive}"
    elif [[ ! -f "${archive}" ]]; then
      echo "Downloading official TWS API ${TWS_VERSION}..."
      if ! curl --fail --location --retry 3 "${TWS_URL}" -o "${archive}.tmp"; then
        rm -f "${archive}.tmp"
        cat >&2 <<EOF
The official fixed-version URL is no longer serving TWS ${TWS_VERSION}.
Download ${TWS_ARCHIVE} from your authorized IBKR source and place it at:
  ${archive}
or import an existing official installation with:
  $0 --accept-ib-license --tws-source /path/to/IBJts
No third-party TWS mirror will be used.
EOF
        exit 1
      fi
      mv "${archive}.tmp" "${archive}"
    fi
    unzip -q "${archive}" -d "${extracted}"
  fi

  local root="${extracted}/IBJts"
  if [[ ! -d "${root}" ]]; then
    root="$(find "${extracted}" -type f -name API_VersionNum.txt \
      -print -quit | xargs -r dirname)"
  fi
  if [[ -z "${root}" ]] || ! validate_tws_root "${root}"; then
    echo "TWS input is not the pinned official ${TWS_VERSION} source payload." >&2
    exit 1
  fi
  rm -rf "${TWS_INSTALL_DIR}"
  mkdir -p "${TWS_INSTALL_DIR}"
  cp -a "${root}" "${TWS_ROOT}"
  find "${TWS_ROOT}/source/cppclient/client" -type f \
    \( -name '*.pb.cpp' -o -name '*.pb.cc' -o -name '*.pb.h' \) -delete
  echo "Prepared TWS API ${TWS_VERSION} at ${TWS_ROOT}"
}

validate_decimal_library() {
  local library=$1
  [[ -f "${library}" ]] || return 1
  local symbols
  symbols="$(nm -g --defined-only "${library}" 2>/dev/null)" || return 1
  [[ "${symbols}" == *"__bid64_to_string"* &&
     "${symbols}" == *"__bid64_from_string"* ]] || return 1
  cat >"${TMP_DIR}/libbid-link.cpp" <<'EOF'
extern "C" void __bid64_to_string(char *, unsigned long long);
extern "C" unsigned long long __bid64_from_string(
    char *, unsigned int *, unsigned int *);
int main() {
  auto *to_string = &__bid64_to_string;
  auto *from_string = &__bid64_from_string;
  return to_string == nullptr || from_string == nullptr;
}
EOF
  c++ "${TMP_DIR}/libbid-link.cpp" "${library}" -o \
    "${TMP_DIR}/libbid-link" >/dev/null 2>&1
}

install_decimal_library() {
  local library=$1 source=$2 version=$3
  validate_decimal_library "${library}" || return 1
  ln -sfn "${library}" "${DECIMAL_LIBRARY}"
  cat >"${DEPS_ROOT}/manifest.env" <<EOF
IBKR_TWS_API_VERSION=${TWS_VERSION}
IBKR_TWS_API_ROOT=${TWS_ROOT}
IBKR_DECIMAL_SOURCE=${source}
IBKR_DECIMAL_VERSION=${version}
IBKR_DECIMAL_LIBRARY=${DECIMAL_LIBRARY}
EOF
  echo "Prepared libbid from ${source} (${version}) at ${DECIMAL_LIBRARY}"
}

try_apt_libbid() {
  command -v dpkg-query >/dev/null 2>&1 || return 1
  if ! dpkg-query -W -f='${Status}' libintelrdfpmath-dev 2>/dev/null |
       awk '/install ok installed/ { found=1 } END { exit !found }'; then
    command -v apt-get >/dev/null 2>&1 || return 1
    local sudo_command=()
    if ((EUID != 0)); then
      command -v sudo >/dev/null 2>&1 || return 1
      sudo_command=(sudo)
    fi
    "${sudo_command[@]}" apt-get update || return 1
    "${sudo_command[@]}" apt-get install -y --no-install-recommends \
      libintelrdfpmath-dev || return 1
  fi
  local library version
  library="$(dpkg -L libintelrdfpmath-dev |
    awk '/[/]libbidgcc000[.]a$/ { print; exit }')"
  [[ -n "${library}" ]] || return 1
  version="$(dpkg-query -W -f='${Version}' libintelrdfpmath-dev)"
  install_decimal_library "${library}" apt "${version}"
}

build_rdfp_tree() {
  local source_root=$1 source_name=$2 version=$3
  local work="${TMP_DIR}/rdfp-${source_name}"
  rm -rf "${work}"
  cp -a "${source_root}/LIBRARY" "${work}"
  make -C "${work}" clean >/dev/null
  make -C "${work}" -j"$(nproc)" CC=cc \
    CALL_BY_REF=0 GLOBAL_RND=0 GLOBAL_FLAGS=0 \
    UNCHANGED_BINARY_FLAGS=0 >/dev/null
  cp "${work}/libbid.a" "${PREFIX_DIR}/lib/libbid-${source_name}.a"
  install_decimal_library "${PREFIX_DIR}/lib/libbid-${source_name}.a" \
    "${source_name}" "${version}"
}

try_netlib_libbid() {
  local archive="${DOWNLOAD_DIR}/${RDFP_ARCHIVE}"
  if [[ -n "${RDFP_ARCHIVE_INPUT}" ]]; then
    [[ -f "${RDFP_ARCHIVE_INPUT}" ]] || {
      echo "Intel RDFP archive not found: ${RDFP_ARCHIVE_INPUT}" >&2
      return 1
    }
    cp "${RDFP_ARCHIVE_INPUT}" "${archive}"
  fi
  if [[ ! -f "${archive}" ]]; then
    curl --fail --location --retry 3 "${RDFP_URL}" -o "${archive}.tmp" ||
      return 1
    mv "${archive}.tmp" "${archive}"
  fi
  printf '%s  %s\n' "${RDFP_SHA256}" "${archive}" |
    sha256sum --check - >/dev/null || return 1
  local extracted="${SOURCE_DIR}/IntelRDFPMathLib${RDFP_VERSION}"
  if [[ "${FORCE}" == true || ! -d "${extracted}/LIBRARY" ]]; then
    rm -rf "${extracted}"
    tar -xzf "${archive}" -C "${SOURCE_DIR}" || return 1
  fi
  build_rdfp_tree "${extracted}" netlib "${RDFP_VERSION}"
}

try_vendored_libbid() {
  [[ -d "${VENDORED_RDFP}/LIBRARY" &&
     -f "${VENDORED_RDFP}/eula.txt" ]] || return 1
  build_rdfp_tree "${VENDORED_RDFP}" vendored "${RDFP_VERSION}"
}

install_tws
if [[ "${FORCE}" != true ]] && validate_decimal_library "${DECIMAL_LIBRARY}"; then
  echo "Compatible libbid is already prepared at ${DECIMAL_LIBRARY}"
elif [[ "${LIBBID_SOURCE}" == apt ]]; then
  try_apt_libbid || { echo "Compatible apt libbid was not found." >&2; exit 1; }
elif [[ "${LIBBID_SOURCE}" == netlib ]]; then
  try_netlib_libbid || { echo "Failed to build netlib libbid." >&2; exit 1; }
elif [[ "${LIBBID_SOURCE}" == vendored ]]; then
  try_vendored_libbid || { echo "Failed to build vendored libbid." >&2; exit 1; }
else
  try_netlib_libbid || {
    echo "Failed to download or build Intel RDFP from ${RDFP_URL}." >&2
    echo "Checksum: ${RDFP_SHA256}" >&2
    echo "If the network is unavailable, download ${RDFP_ARCHIVE} separately and re-run with:" >&2
    echo "  --rdfp-archive /path/to/${RDFP_ARCHIVE}" >&2
    echo "The default path does not use third_party/intel-rdfp." >&2
    exit 1
  }
fi

echo "IBKR dependencies are ready."
echo "  TWS:    ${TWS_ROOT}"
echo "  libbid: ${DECIMAL_LIBRARY}"
