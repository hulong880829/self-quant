#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
CERT_DIR="${SCRIPT_DIR}/certs"
mkdir -p "${CERT_DIR}"

openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "${CERT_DIR}/server.key" \
  -out "${CERT_DIR}/server.crt" \
  -days 825 \
  -subj "/CN=localhost" \
  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"

echo "Generated ${CERT_DIR}/server.crt and ${CERT_DIR}/server.key"
echo
echo "ClickHouse in docker-compose uses these files for HTTPS on port 8443."
echo "MDS mds_shm_consumer --clickhouse-bbo verifies TLS; install the cert once:"
echo "  sudo cp ${CERT_DIR}/server.crt /usr/local/share/ca-certificates/clickhouse-localhost.crt"
echo "  sudo update-ca-certificates"
