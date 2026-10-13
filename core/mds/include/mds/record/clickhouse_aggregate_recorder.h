#pragma once

#include "mds/consume/aggregate_dispatch.h"
#include "mds/record/clickhouse_aggregate_schema.h"
#include "mds/record/clickhouse_bbo_recorder.h"
#include "utils/md/wire.h"

#include <cstddef>
#include <cstdint>
#include <functional>
#include <memory>
#include <span>
#include <string>
#include <string_view>
#include <vector>

namespace mds::record {

inline constexpr std::size_t kMaximumDepth = 50;

enum RecordFlags : std::uint16_t {
  kGap = 1U << 0U,
  kReset = 1U << 1U,
};

using ClickHouseQuery = std::function<bool(
    std::string_view sql, std::string &body, std::string &error)>;

struct ClickHouseAggregateTopic {
  std::string segment;
  consume::AggregateTopic kind{consume::AggregateTopic::Unsupported};
  consume::AggregateLatestState *latest{};
};

struct ClickHouseAggregateOptions {
  std::string host;
  std::string service{"8443"};
  std::string database{"market_data"};
  std::string user{"bbo_writer"};
  std::string password;
  std::string bbo_table{"aggregate_bbo_history"};
  std::string orderbook_table{"aggregate_orderbook_history"};
  std::string catalog_table{"aggregate_topic_catalog"};
  std::string catalog_scope;
  std::string service_instance;
  std::uint64_t sample_interval_ms{200};
  std::size_t depth{50};
  std::uint64_t flush_interval_ms{1000};
  std::uint64_t request_timeout_ms{5000};
  std::size_t batch_rows{1000};
  std::size_t bbo_queue_rows{4096};
  std::size_t orderbook_queue_rows{2048};
  std::size_t catalog_queue_rows{256};
  std::size_t max_queued_bytes{67'108'864};
  bool enable_sampler{true};
  ClickHouseRequest request;
  ClickHouseQuery query;
  std::vector<ClickHouseAggregateTopic> topics;
};

struct ClickHouseAggregateMetrics {
  std::uint64_t samples_seen{};
  std::uint64_t rows_enqueued{};
  std::uint64_t rows_written{};
  std::uint64_t bbo_queue_depth{};
  std::uint64_t orderbook_queue_depth{};
  std::uint64_t catalog_queue_depth{};
  std::uint64_t queue_drops{};
  std::uint64_t bbo_queue_drops{};
  std::uint64_t orderbook_queue_drops{};
  std::uint64_t catalog_queue_drops{};
  std::uint64_t queued_bytes{};
  std::uint64_t retry_rows{};
  std::uint64_t http_failures{};
  std::uint64_t stale_skips{};
  std::uint64_t encode_failures{};
  std::uint64_t last_success_at{};
  std::uint64_t write_lag_ms{};
  std::uint64_t shutdown_drops{};
  std::uint64_t catalog_reconcile_ok{};
  std::uint64_t catalog_reconcile_failures{};
};

class ClickHouseAggregateRecorder {
 public:
  explicit ClickHouseAggregateRecorder(ClickHouseAggregateOptions options);
  ~ClickHouseAggregateRecorder();
  ClickHouseAggregateRecorder(const ClickHouseAggregateRecorder &) = delete;
  ClickHouseAggregateRecorder &operator=(const ClickHouseAggregateRecorder &) =
      delete;

  [[nodiscard]] bool start();
  void stop() noexcept;
  void sample(std::uint64_t wall_ns, std::uint64_t mono_ns) noexcept;
  [[nodiscard]] ClickHouseAggregateMetrics metrics() const noexcept;
  [[nodiscard]] std::string error() const;

 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};

}  // namespace mds::record
