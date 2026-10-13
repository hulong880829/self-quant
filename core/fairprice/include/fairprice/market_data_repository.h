#pragma once

#include "fairprice/store.h"
#include "utils/md/wire.h"

#include <cstddef>
#include <cstdint>
#include <functional>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

namespace fairprice {

enum class MarketDataError : std::uint8_t {
  Ok = 0,
  Timeout,
  Http,
  Auth,
  Empty,
  ArrayLength,
  Parse,
};

[[nodiscard]] std::string_view market_data_error_name(MarketDataError error);

struct ClickHouseReadConfig {
  std::string host;
  std::string service{"8443"};
  std::string database{"market_data"};
  std::string user{"bbo_writer"};
  std::string password;
  std::string catalog_table{"aggregate_topic_catalog"};
  std::string bbo_table{"aggregate_bbo_history"};
  std::string orderbook_table{"aggregate_orderbook_history"};
  std::string catalog_scope{"prod-agg"};
  std::uint64_t request_timeout_ms{5000};
};

struct CatalogTopicRow {
  std::string profile;
  std::string symbol;
  std::string kind;
  std::string segment;
  bool active{};
  std::uint16_t schema_major{};
  std::uint16_t schema_minor{};
  std::uint64_t updated_at_ns{};
};

struct LatestBboRow {
  std::string segment;
  std::uint64_t sampled_at_ns{};
  std::uint64_t source_wall_ns{};
  std::uint64_t ring_epoch{};
  std::uint64_t ring_sequence{};
  std::uint64_t generation{};
  utils::md::wire::AggBboRecord record{};
};

struct LatestBookRow {
  std::string segment;
  std::uint64_t sampled_at_ns{};
  std::uint64_t source_wall_ns{};
  std::uint64_t ring_epoch{};
  std::uint64_t ring_sequence{};
  std::uint64_t generation{};
  utils::md::wire::AggOrderBookRecord record{};
};

struct HistoryBboRow {
  std::uint64_t sampled_at_ns{};
  std::int64_t raw_bid_price{};
  std::int64_t raw_ask_price{};
  std::int64_t gated_bid_price{};
  std::int64_t gated_ask_price{};
  std::int32_t cross_window_raw_min{};
  std::int32_t cross_window_raw_max{};
  std::int32_t cross_window_gated_min{};
  std::int32_t cross_window_gated_max{};
  std::uint64_t cross_window_samples{};
};

struct MarketDataQueryResult {
  MarketDataError error{MarketDataError::Ok};
  std::string message;
  std::string body;
};

using MarketDataQuery =
    std::function<MarketDataQueryResult(std::string_view sql)>;

template <typename T>
struct MarketDataResult {
  MarketDataError error{MarketDataError::Ok};
  std::string message;
  T value{};

  [[nodiscard]] explicit operator bool() const noexcept {
    return error == MarketDataError::Ok;
  }
};

struct HistoryOhlcRow {
  std::uint64_t bucket_start_ns{};
  double open{};
  double high{};
  double low{};
  double close{};
  double window_min{};
  double window_max{};
  std::uint64_t samples{};
};

class MarketDataRepository {
 public:
  MarketDataRepository(ClickHouseReadConfig config, MarketDataQuery query = {});

  [[nodiscard]] MarketDataResult<std::vector<CatalogTopicRow>>
  query_catalog() const;
  [[nodiscard]] MarketDataResult<std::vector<LatestBboRow>>
  query_latest_bbo(const std::vector<std::string> &segments) const;
  [[nodiscard]] MarketDataResult<std::vector<LatestBookRow>>
  query_latest_book(const std::vector<std::string> &segments) const;
  [[nodiscard]] MarketDataResult<std::vector<HistoryBboRow>>
  query_bbo_history(std::string_view segment, std::uint64_t start_ns,
                    std::uint64_t end_ns) const;
  [[nodiscard]] MarketDataResult<std::vector<HistoryOhlcRow>>
  query_bbo_history_ohlc(std::string_view segment, std::uint64_t start_ns,
                         std::uint64_t end_ns, std::uint64_t resolution_ns,
                         std::string_view bps_type) const;
  [[nodiscard]] MarketDataResult<std::vector<double>>
  query_bbo_history_distribution(std::string_view segment, std::uint64_t start_ns,
                                 std::uint64_t end_ns,
                                 std::string_view bps_type) const;
  [[nodiscard]] const ClickHouseReadConfig &config() const noexcept {
    return config_;
  }
  [[nodiscard]] const std::string &last_sql() const noexcept { return last_sql_; }
  [[nodiscard]] std::size_t query_count() const noexcept { return query_count_; }

 private:
  MarketDataQueryResult execute(std::string sql) const;
  MarketDataQueryResult http_query(std::string_view sql) const;

  ClickHouseReadConfig config_;
  MarketDataQuery query_;
  mutable std::string last_sql_;
  mutable std::size_t query_count_{};
};

LiveSample live_sample_from_history_row(const HistoryBboRow &row);

}  // namespace fairprice
