#!/usr/bin/env bash
# Shared .env helpers. Never print secret values.
set -euo pipefail

legacy_credentials_key() {
  printf '%s' '0123456789abcdef0123456789abcdef'
}

is_placeholder() {
  local value="$1"
  if [[ -z "$value" ]]; then
    return 0
  fi
  case "$value" in
    replace-with-* | changeme | CHANGE_ME*) return 0 ;;
  esac
  return 1
}

is_legacy_credentials_key() {
  [[ "$1" == "$(legacy_credentials_key)" ]]
}

is_valid_credentials_key() {
  python3 - "$1" <<'PY'
import base64, binascii, sys
value = sys.argv[1]
if len(value) == 64 and all(c in "0123456789abcdefABCDEF" for c in value):
    try:
        if len(binascii.unhexlify(value)) == 32:
            sys.exit(0)
    except binascii.Error:
        pass
for decoder in (base64.standard_b64decode, base64.urlsafe_b64decode):
    try:
        decoded = decoder(value)
    except Exception:
        continue
    if len(decoded) == 32:
        sys.exit(0)
if "=" not in value:
    try:
        pad = "=" * ((4 - len(value) % 4) % 4)
        decoded = base64.b64decode(value + pad, validate=False)
    except Exception:
        decoded = b""
    if len(decoded) == 32:
        sys.exit(0)
if len(value) == 32:
    sys.exit(0)
sys.exit(1)
PY
}

env_value() {
  local file="$1"
  local key="$2"
  python3 - "$file" "$key" <<'PY'
import sys
path, key = sys.argv[1], sys.argv[2]
prefix = key + "="
with open(path, encoding="utf-8") as handle:
    for line in handle:
        if line.startswith(prefix):
            print(line[len(prefix):].rstrip("\n"))
            break
PY
}

set_env_value() {
  local file="$1"
  local key="$2"
  local value="$3"
  python3 - "$file" "$key" "$value" <<'PY'
import sys
path, key, value = sys.argv[1], sys.argv[2], sys.argv[3]
prefix = key + "="
lines = []
found = False
with open(path, encoding="utf-8") as handle:
    for line in handle:
        if line.startswith(prefix):
            lines.append(f"{key}={value}\n")
            found = True
        else:
            lines.append(line)
if not found:
    if lines and not lines[-1].endswith("\n"):
        lines[-1] += "\n"
    lines.append(f"{key}={value}\n")
with open(path, "w", encoding="utf-8") as handle:
    handle.writelines(lines)
PY
}

random_hex() {
  openssl rand -hex 32
}
