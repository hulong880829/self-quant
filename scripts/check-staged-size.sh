#!/usr/bin/env bash
# Reject newly staged files larger than 1 MiB unless they are allowlisted.
set -euo pipefail

limit=$((1024 * 1024))
allowlist=(
)

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo "not a git repository" >&2
  exit 1
fi

allowed() {
  local path="$1"
  local item
  if ((${#allowlist[@]} == 0)); then
    return 1
  fi
  for item in "${allowlist[@]}"; do
    if [[ "$path" == "$item" ]]; then
      return 0
    fi
  done
  return 1
}

failed=0
while IFS= read -r path; do
  [[ -n "$path" ]] || continue
  if allowed "$path"; then
    continue
  fi
  if [[ ! -f "$path" ]]; then
    continue
  fi
  size="$(wc -c <"$path" | tr -d ' ')"
  if ((size > limit)); then
    echo "refusing new staged file over 1 MiB: ${path} (${size} bytes)" >&2
    failed=1
  fi
done < <(git diff --cached --name-only --diff-filter=A)

if ((failed != 0)); then
  exit 1
fi
echo "staged file size check ok"
