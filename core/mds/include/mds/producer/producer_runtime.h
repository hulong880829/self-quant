#pragma once

#include "mds/api/mds_api.h"

#include <chrono>
#include <cstddef>
#include <memory>
#include <ostream>
#include <span>
#include <string>
#include <string_view>

namespace mds::producer {

struct ProducerRuntimeOptions {
  std::string config_path;
  bool validate_only{};
  bool discover_only{};
  std::ostream *output{};
  std::ostream *error_output{};
};

struct ResolvedSegment {
  std::string name;
  std::size_t ring_bytes{};
  std::size_t max_record_bytes{};
};

[[nodiscard]] std::chrono::system_clock::time_point
next_daily_discovery_utc(
    std::chrono::system_clock::time_point now) noexcept;

class ProducerRuntime {
 public:
  explicit ProducerRuntime(ProducerRuntimeOptions options);
  ~ProducerRuntime();

  [[nodiscard]] static std::unique_ptr<ProducerRuntime>
  create(std::string config_path, std::string &error) noexcept;

  ProducerRuntime(ProducerRuntime &&) noexcept;
  ProducerRuntime &operator=(ProducerRuntime &&) noexcept;
  ProducerRuntime(const ProducerRuntime &) = delete;
  ProducerRuntime &operator=(const ProducerRuntime &) = delete;

  [[nodiscard]] api::Result<void> start() noexcept;
  int run_once(int timeout_ms = 100) noexcept;
  void stop() noexcept;

  [[nodiscard]] std::span<const ResolvedSegment>
  resolved_segments() const noexcept;
  [[nodiscard]] bool failed() const noexcept;
  [[nodiscard]] std::string_view error() const noexcept;

 private:
  class Impl;
  std::unique_ptr<Impl> impl_;
};

}  // namespace mds::producer
