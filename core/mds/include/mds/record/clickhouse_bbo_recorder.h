#pragma once

#include "utils/md/types.h"

#include <cstddef>
#include <cstdint>
#include <functional>
#include <memory>
#include <span>
#include <string>
#include <string_view>
#include <vector>

namespace mds::record {

struct ClickHouseSelector {
  utils::md::Venue venue{utils::md::Venue::Unknown};
  utils::md::ProductType product{utils::md::ProductType::Unknown};
};

using ClickHouseRequest =
    std::function<bool(std::string_view, std::span<const std::byte>,
                       std::string &)>;

struct ClickHouseBboOptions {
  std::string host;
  std::string service{"8443"};
  std::string database{"default"};
  std::string table{"crypto_bbo"};
  std::string user{"default"};
  std::string password;
  std::vector<ClickHouseSelector> selectors;
  std::uint64_t sample_interval_ms{1000};
  std::uint64_t stale_cutoff_ms{5000};
  std::uint64_t flush_interval_ms{1000};
  std::uint64_t request_timeout_ms{5000};
  std::size_t batch_rows{1000};
  std::size_t queue_rows{10000};
  std::size_t unresolved_rows{1024};
  ClickHouseRequest request;
};

struct ClickHouseBboMetrics {
  std::uint64_t rows_enqueued{};
  std::uint64_t rows_written{};
  std::uint64_t queue_drops{};
  std::uint64_t unresolved_instrument_drops{};
  std::uint64_t http_failures{};
  std::uint64_t rows_requeued{};
  std::uint64_t shutdown_drops{};
  std::uint64_t stale_skips{};
  std::uint64_t decode_failed{};
};

class ClickHouseBboRecorder {
 public:
  explicit ClickHouseBboRecorder(ClickHouseBboOptions options);
  ~ClickHouseBboRecorder();
  ClickHouseBboRecorder(const ClickHouseBboRecorder &) = delete;
  ClickHouseBboRecorder &operator=(const ClickHouseBboRecorder &) = delete;

  [[nodiscard]] bool start();
  void stop() noexcept;
  void consume(std::span<const std::byte> payload,
               std::uint64_t receive_mono_ns) noexcept;
  void sample(std::uint64_t wall_ns, std::uint64_t mono_ns) noexcept;
  [[nodiscard]] ClickHouseBboMetrics metrics() const noexcept;
  [[nodiscard]] std::string error() const;

 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};

}  // namespace mds::record
