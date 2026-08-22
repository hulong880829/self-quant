#pragma once

#include "mds/api/mds_api.h"
#include "mds/transport/shared_ring.h"

#include <cstddef>
#include <cstdint>
#include <optional>
#include <span>
#include <string>
#include <string_view>
#include <vector>

namespace mds::consume {

struct SegmentRecord {
  std::uint32_t type{};
  std::uint64_t sequence{};
  std::uint64_t epoch{};
  std::vector<std::byte> storage{};

  [[nodiscard]] std::span<const std::byte> payload() const noexcept {
    return storage;
  }
};

struct SegmentReaderOptions {
  api::ShmBackend backend{api::ShmBackend::PosixShm};
  std::string hugetlbfs_mount{"/dev/hugepages"};
  bool allow_hugepage_fallback{};
  std::uint64_t heartbeat_interval_ns{500'000'000ULL};
};

class SegmentReader {
 public:
  SegmentReader() = default;
  ~SegmentReader();
  SegmentReader(const SegmentReader &) = delete;
  SegmentReader &operator=(const SegmentReader &) = delete;
  SegmentReader(SegmentReader &&other) noexcept;
  SegmentReader &operator=(SegmentReader &&other) noexcept;

  static api::Result<SegmentReader>
  open(std::string_view segment_name,
       const SegmentReaderOptions &options = {});

  // A successful empty optional means that no record is currently available.
  // Returned records own their payload and remain valid across later reads.
  [[nodiscard]] api::Result<std::optional<SegmentRecord>> try_read();
  [[nodiscard]] std::string_view segment_name() const noexcept;
  [[nodiscard]] std::uint64_t epoch() const noexcept { return epoch_; }
  [[nodiscard]] bool valid() const noexcept { return bool(reader_); }
  api::Result<void> resync_to_latest() noexcept;

 private:
  api::Result<void> register_reader() noexcept;
  void close() noexcept;

  transport::SharedRing ring_{};
  transport::ReaderHandle reader_{};
  SegmentReaderOptions options_{};
  std::uint64_t marker_{};
  std::uint64_t epoch_{};
  std::uint64_t last_heartbeat_ns_{};
};

}  // namespace mds::consume
