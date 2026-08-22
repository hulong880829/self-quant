#pragma once

#include "mds/consume/aggregate_dispatch.h"
#include "mds/record/record.h"

#include <atomic>
#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <functional>
#include <memory>
#include <span>
#include <string>

namespace mds::record {

struct RecorderOptions {
  std::filesystem::path output_directory;
  std::uint64_t sample_interval_ms{200};
  std::uint32_t retention_hours{24};
  std::size_t depth{50};
  int zstd_level{1};
  std::uint64_t min_free_disk_bytes{1ULL << 30U};
  std::function<std::uint64_t()> wall_clock_ns;
};

struct Topic {
  std::string segment;
  consume::AggregateTopic kind{consume::AggregateTopic::Unsupported};
  consume::AggregateLatestState *latest{};
};

class Recorder {
 public:
  Recorder(RecorderOptions options, std::span<const Topic> topics);
  ~Recorder();
  Recorder(const Recorder &) = delete;
  Recorder &operator=(const Recorder &) = delete;

  [[nodiscard]] bool start();
  void stop() noexcept;
  [[nodiscard]] bool failed() const noexcept;
  [[nodiscard]] std::string error() const;

 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};

}  // namespace mds::record
