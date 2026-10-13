#!/usr/bin/env bash
# Static contract between Compose required variables and env templates.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
backend_dir="$(cd -- "${script_dir}/../.." && pwd)"
compose_file="${1:-${backend_dir}/deploy/docker-compose.yml}"
example_file="${2:-${backend_dir}/.env.example}"
production_file="${3:-${backend_dir}/.env.production.example}"

python3 - "$compose_file" "$example_file" "$production_file" <<'PY'
import re, sys
compose_path, example_path, production_path = sys.argv[1:]
compose = open(compose_path, encoding="utf-8").read()
errors = []
if "CLICKHOUSE_BBO_PASSWORD" in compose:
    errors.append("compose still references CLICKHOUSE_BBO_PASSWORD")
if "${DATABASE_URL" in compose:
    errors.append("compose interpolates DATABASE_URL")
required = []
for match in re.finditer(r"\$\{([A-Za-z_][A-Za-z0-9_]*):\?", compose):
    name = match.group(1)
    if name not in required:
        required.append(name)

def keys(path):
    found = []
    duplicates = []
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            if not line or line.startswith("#") or "=" not in line:
                continue
            key = line.split("=", 1)[0]
            if key in found:
                duplicates.append(key)
            else:
                found.append(key)
    return found, duplicates

example_keys, example_dupes = keys(example_path)
if example_dupes:
    errors.append("duplicate keys in .env.example: " + ", ".join(example_dupes))
missing = [name for name in required if name not in example_keys]
if missing:
    errors.append("compose required variables missing from .env.example: " + ", ".join(missing))
if "CLICKHOUSE_BBO_PASSWORD" in example_keys:
    errors.append(".env.example still defines CLICKHOUSE_BBO_PASSWORD")

for path in (example_path, production_path):
    text = open(path, encoding="utf-8").read()
    for name in ("POSTGRES_PASSWORD",):
        if not re.search(rf"^{name}=", text, re.M):
            errors.append(f"{path} is missing {name}=")
    if re.search(r"^ACCOUNT_CREDENTIALS_KEY=0123456789abcdef0123456789abcdef$", text, re.M):
        errors.append(f"{path} still contains the published ACCOUNT_CREDENTIALS_KEY example")
if errors:
    print("\n".join(errors), file=sys.stderr)
    sys.exit(1)
print(f"env contract ok ({len(required)} required variables)")
PY
