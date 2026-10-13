#!/usr/bin/env bash
# Resolve new or upgrade from Compose volumes. Restore is never selected here.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
requested="auto"
env_file=""
compose_file=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --requested)
      requested="${2:-}"
      shift 2
      ;;
    --env-file)
      env_file="${2:-}"
      shift 2
      ;;
    --compose-file)
      compose_file="${2:-}"
      shift 2
      ;;
    *)
      echo "unknown argument: $1" >&2
      exit 2
      ;;
  esac
done

case "$requested" in
  auto|new|upgrade) ;;
  *)
    echo "requested mode must be auto, new, or upgrade" >&2
    exit 2
    ;;
esac

args=()
if [[ -n "$env_file" ]]; then
  args+=(--env-file "$env_file")
fi
if [[ -n "$compose_file" ]]; then
  args+=(--compose-file "$compose_file")
fi

report="$("${script_dir}/compose-volumes.sh" "${args[@]}")"
postgres_state="$(printf '%s\n' "$report" | awk -F= '$1=="selfquant-postgres_state" {print $2}')"
clickhouse_state="$(printf '%s\n' "$report" | awk -F= '$1=="clickhouse-data_state" {print $2}')"
postgres_name="$(printf '%s\n' "$report" | awk -F= '$1=="selfquant-postgres_name" {print $2}')"
clickhouse_name="$(printf '%s\n' "$report" | awk -F= '$1=="clickhouse-data_name" {print $2}')"

incomplete() {
  local present="" missing=""
  if [[ "$postgres_state" == "present" ]]; then
    present="${postgres_name}"
  else
    missing="${postgres_name}"
  fi
  if [[ "$clickhouse_state" == "present" ]]; then
    present="${present:+$present }${clickhouse_name}"
  else
    missing="${missing:+$missing }${clickhouse_name}"
  fi
  echo "incomplete compose volumes:" >&2
  echo "  present: ${present:-none}" >&2
  echo "  missing: ${missing:-none}" >&2
  echo "Inspect this environment, restore the missing volume, or remove the orphan volume only after confirming it is disposable." >&2
  echo "Refusing to guess a bootstrap mode." >&2
  exit 1
}

both_present=false
both_missing=false
if [[ "$postgres_state" == "present" && "$clickhouse_state" == "present" ]]; then
  both_present=true
elif [[ "$postgres_state" == "missing" && "$clickhouse_state" == "missing" ]]; then
  both_missing=true
fi

case "$requested" in
  auto)
    if [[ "$both_missing" == true ]]; then
      echo "new"
    elif [[ "$both_present" == true ]]; then
      echo "upgrade"
    else
      incomplete
    fi
    ;;
  new)
    if [[ "$both_missing" != true ]]; then
      echo "refusing new because an existing volume was found:" >&2
      echo "  selfquant-postgres=${postgres_state} (${postgres_name})" >&2
      echo "  clickhouse-data=${clickhouse_state} (${clickhouse_name})" >&2
      exit 1
    fi
    echo "new"
    ;;
  upgrade)
    if [[ "$both_present" != true ]]; then
      incomplete
    fi
    echo "upgrade"
    ;;
esac
