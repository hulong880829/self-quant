#!/usr/bin/env bash
set -euo pipefail

database="${CLICKHOUSE_DATABASE:-market_data}"
raw_table="${CLICKHOUSE_RAW_TABLE:-crypto_bbo}"
minute_table="${CLICKHOUSE_MINUTE_TABLE:-crypto_bbo_minute}"
days="${BACKFILL_DAYS:-10}"
chunk_hours="${BACKFILL_CHUNK_HOURS:-6}"
sleep_seconds="${BACKFILL_SLEEP_SECONDS:-2}"

ch() {
  if [[ -n "${CLICKHOUSE_DOCKER_CONTAINER:-}" ]]; then
    docker exec "${CLICKHOUSE_DOCKER_CONTAINER}" clickhouse-client "$@"
    return
  fi
  clickhouse-client "$@"
}

end_epoch="${BACKFILL_TO_EPOCH:-$(date -u +%s)}"
start_epoch="${BACKFILL_FROM_EPOCH:-$((end_epoch - days * 86400))}"
chunk_seconds="$((chunk_hours * 3600))"

for ((from_epoch=start_epoch; from_epoch<end_epoch; from_epoch+=chunk_seconds)); do
  to_epoch="$((from_epoch + chunk_seconds))"
  if ((to_epoch > end_epoch)); then
    to_epoch="$end_epoch"
  fi
  printf 'backfill [%s, %s)\n' \
    "$(date -u -d "@${from_epoch}" +%FT%TZ)" \
    "$(date -u -d "@${to_epoch}" +%FT%TZ)"
  ch --query "
    INSERT INTO ${database}.${minute_table}
    SELECT
      toStartOfMinute(ts) AS bucket,
      venue,
      product,
      canonical_symbol,
      argMaxState(tuple(bid_price, price_scale, ask_price, price_scale), ts)
    FROM ${database}.${raw_table}
    WHERE ts >= fromUnixTimestamp64Nano(${from_epoch}000000000)
      AND ts < fromUnixTimestamp64Nano(${to_epoch}000000000)
    GROUP BY bucket, venue, product, canonical_symbol
    SETTINGS max_threads = 2, max_insert_threads = 1
  "
  sleep "$sleep_seconds"
done
