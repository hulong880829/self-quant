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

// Zero-copy view of a leased SHM record. payload() is valid until commit(),
// release via destruction, or move. Callers that reject a record must commit()
// before resetting local sequence state so the same payload is not reread.
class BorrowedSegmentRecord {
 public:
  BorrowedSegmentRecord() = default;
  explicit BorrowedSegmentRecord(transport::ReadLease &&lease) noexcept;
  BorrowedSegmentRecord(const BorrowedSegmentRecord &) = delete;
  BorrowedSegmentRecord &operator=(const BorrowedSegmentRecord &) = delete;
  BorrowedSegmentRecord(BorrowedSegmentRecord &&other) noexcept;
  BorrowedSegmentRecord &operator=(BorrowedSegmentRecord &&other) noexcept;
  ~BorrowedSegmentRecord();

  [[nodiscard]] std::uint32_t type() const noexcept;
  [[nodiscard]] std::uint64_t sequence() const noexcept;
  [[nodiscard]] std::uint64_t epoch() const noexcept;
  [[nodiscard]] std::span<const std::byte> payload() const noexcept;
  [[nodiscard]] explicit operator bool() const noexcept;
  api::Result<void> commit() noexcept;

 private:
  transport::ReadLease lease_{};
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
  // Same availability rules as try_read(), but the payload is a SHM view.
  [[nodiscard]] api::Result<std::optional<BorrowedSegmentRecord>>
  try_read_view();
  [[nodiscard]] std::string_view segment_name() const noexcept;
  [[nodiscard]] std::uint64_t epoch() const noexcept { return epoch_; }
  [[nodiscard]] bool valid() const noexcept { return bool(reader_); }
  api::Result<void> resync_to_latest() noexcept;

 private:
  api::Result<transport::ReadLease> acquire_lease();
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
