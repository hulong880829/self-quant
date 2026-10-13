#pragma once

#include <string>
#include <string_view>

namespace mds::record {

inline constexpr std::string_view kAggregateBboHistoryBody = R"SQL((
    sampled_at DateTime64(9, 'UTC'),
    source_wall_ns UInt64,
    mono_ns UInt64,
    profile LowCardinality(String),
    symbol String,
    segment String,
    ring_epoch UInt64,
    ring_sequence UInt64,
    generation UInt64,
    record_flags UInt16,
    reset_generation UInt64,
    header_flags UInt16,
    header_exchange_ts_ns UInt64,
    base_asset String,
    quote_asset String,
    venue_slot_ids Array(UInt8),
    price_scale UInt8,
    quantity_scale UInt8,
    member_count UInt8,
    member_mask UInt32,
    live_mask UInt32,
    gated_bid_price Int64,
    gated_bid_quantity Int64,
    gated_bid_venue_quantity Array(Int64),
    gated_bid_exchange_ts_ns UInt64,
    gated_bid_venue_mask UInt32,
    gated_bid_worst_ingress_age_us UInt32,
    gated_bid_best_venue UInt8,
    gated_bid_timestamp_venue UInt8,
    gated_bid_contributor_count UInt8,
    gated_ask_price Int64,
    gated_ask_quantity Int64,
    gated_ask_venue_quantity Array(Int64),
    gated_ask_exchange_ts_ns UInt64,
    gated_ask_venue_mask UInt32,
    gated_ask_worst_ingress_age_us UInt32,
    gated_ask_best_venue UInt8,
    gated_ask_timestamp_venue UInt8,
    gated_ask_contributor_count UInt8,
    raw_bid_price Int64,
    raw_bid_quantity Int64,
    raw_bid_venue_mask UInt32,
    raw_bid_best_venue UInt8,
    raw_ask_price Int64,
    raw_ask_quantity Int64,
    raw_ask_venue_mask UInt32,
    raw_ask_best_venue UInt8,
    raw_cross_bps Int32,
    gated_cross_bps Int32,
    skew_us UInt32,
    cross_skew_threshold_us UInt32,
    fx_age_us UInt32,
    cross_bid_venue UInt8,
    cross_ask_venue UInt8,
    fx_venue UInt8,
    cross_window_start_mono_ns UInt64,
    cross_window_end_mono_ns UInt64,
    cross_window_raw_min Int32,
    cross_window_raw_max Int32,
    cross_window_gated_min Int32,
    cross_window_gated_max Int32,
    cross_window_samples UInt64
)
ENGINE = MergeTree
PARTITION BY toDate(sampled_at)
ORDER BY (profile, symbol, sampled_at)
TTL toDateTime(sampled_at) + INTERVAL 24 HOUR)SQL";

inline constexpr std::string_view kAggregateOrderBookHistoryBody = R"SQL((
    sampled_at DateTime64(9, 'UTC'),
    source_wall_ns UInt64,
    mono_ns UInt64,
    profile LowCardinality(String),
    symbol String,
    segment String,
    ring_epoch UInt64,
    ring_sequence UInt64,
    generation UInt64,
    record_flags UInt16,
    reset_generation UInt64,
    header_flags UInt16,
    exchange_ts_ns UInt64,
    book_generation UInt32,
    base_asset String,
    quote_asset String,
    venue_slot_ids Array(UInt8),
    price_scale UInt8,
    quantity_scale UInt8,
    member_count UInt8,
    member_mask UInt32,
    active_mask UInt32,
    bid_count UInt32,
    ask_count UInt32,
    bid_price Array(Int64),
    bid_quantity Array(Int64),
    bid_venue_mask Array(UInt32),
    bid_contributor_count Array(UInt8),
    bid_venue_quantity Array(Array(Int64)),
    ask_price Array(Int64),
    ask_quantity Array(Int64),
    ask_venue_mask Array(UInt32),
    ask_contributor_count Array(UInt8),
    ask_venue_quantity Array(Array(Int64))
)
ENGINE = MergeTree
PARTITION BY toDate(sampled_at)
ORDER BY (profile, symbol, sampled_at)
TTL toDateTime(sampled_at) + INTERVAL 24 HOUR)SQL";

inline constexpr std::string_view kAggregateTopicCatalogBody = R"SQL((
    catalog_scope LowCardinality(String),
    service_instance String,
    profile LowCardinality(String),
    symbol String,
    kind LowCardinality(String),
    segment String,
    active UInt8,
    schema_major UInt16,
    schema_minor UInt16,
    updated_at DateTime64(9, 'UTC'),
    updated_at_ns UInt64
)
ENGINE = ReplacingMergeTree(updated_at_ns)
ORDER BY (catalog_scope, segment))SQL";

inline constexpr std::string_view kDefaultAggregateBboTable =
    "aggregate_bbo_history";
inline constexpr std::string_view kDefaultAggregateOrderBookTable =
    "aggregate_orderbook_history";
inline constexpr std::string_view kDefaultAggregateCatalogTable =
    "aggregate_topic_catalog";
inline constexpr std::string_view kDefaultAggregateDatabase = "market_data";

inline std::string clickhouse_aggregate_create_table(
    std::string_view database, std::string_view table,
    std::string_view body) {
  std::string sql = "CREATE TABLE IF NOT EXISTS ";
  sql.append(database);
  sql.push_back('.');
  sql.append(table);
  sql.push_back('\n');
  sql.append(body);
  return sql;
}

inline std::string clickhouse_aggregate_bbo_ddl(std::string_view database,
                                                std::string_view table) {
  return clickhouse_aggregate_create_table(database, table,
                                           kAggregateBboHistoryBody);
}

inline std::string clickhouse_aggregate_orderbook_ddl(
    std::string_view database, std::string_view table) {
  return clickhouse_aggregate_create_table(database, table,
                                           kAggregateOrderBookHistoryBody);
}

inline std::string clickhouse_aggregate_catalog_ddl(std::string_view database,
                                                    std::string_view table) {
  return clickhouse_aggregate_create_table(database, table,
                                           kAggregateTopicCatalogBody);
}

}  // namespace mds::record
