#pragma once

#include "mds/api/mds_api.h"

#include <atomic>
#include <cstddef>
#include <cstdint>
#include <span>
#include <string>
#include <string_view>

namespace mds::transport {

inline constexpr std::uint64_t kRingMagic = 0x5344514d44535247ULL;
inline constexpr std::uint32_t kRingSchemaMajor = 5;
inline constexpr std::uint32_t kPaddingType = 0;
inline constexpr std::size_t kMaxReaders = 64;

enum class ReaderState : std::uint32_t { Free, Initializing, Active, Suspect };

struct alignas(64) ReaderSlot {
  std::atomic<std::uint32_t> state{static_cast<std::uint32_t>(ReaderState::Free)};
  std::atomic<std::uint32_t> pid{0};
  std::atomic<std::uint64_t> process_start_marker{0};
  std::atomic<std::uint64_t> lease_token{0};
  std::atomic<std::uint64_t> cursor{0};
  std::atomic<std::uint64_t> heartbeat_ns{0};
  std::byte reserved[24]{};
};
static_assert(sizeof(ReaderSlot) == 64);

struct alignas(64) SegmentControl {
  std::uint32_t mode{static_cast<std::uint32_t>(
      api::RingMode::OverwriteOldest)};
  std::byte reserved[60]{};
};
static_assert(sizeof(SegmentControl) == 64);

struct alignas(64) SegmentHeader {
  // Creation publishes magic last with release ordering. Attachers acquire it
  // before reading any of the immutable layout fields below.
  std::atomic<std::uint64_t> magic{0};
  std::uint32_t schema_major{kRingSchemaMajor};
  std::uint32_t header_bytes{};
  std::uint64_t ring_bytes{};
  std::uint64_t max_record_bytes{};
  std::uint64_t epoch{};
  std::atomic<std::uint64_t> writer_cursor{0};
  std::atomic<std::uint64_t> next_sequence{1};
  std::uint32_t max_readers{};
  // Reuses the former reserved word, preserving the schema-3 layout. Readers
  // increment it after publishing registry state transitions.
  std::atomic<std::uint32_t> registry_generation{0};
  SegmentControl control{};
  ReaderSlot readers[kMaxReaders]{};
};
static_assert(sizeof(SegmentHeader) == 4224);
static_assert(offsetof(SegmentHeader, registry_generation) == 60);
static_assert(offsetof(SegmentHeader, control) == 64);
static_assert(offsetof(SegmentHeader, readers) == 128);

struct alignas(8) RecordHeader {
  std::uint32_t length{};
  std::uint32_t type{};
  std::uint32_t payload_bytes{};
  std::uint32_t payload_crc32c{};
  std::uint64_t epoch{};
  std::uint64_t sequence{};
  std::atomic<std::uint64_t> commit_sequence{0};
};
static_assert(sizeof(RecordHeader) == 40);

struct RingOptions {
  api::ShmBackend backend{api::ShmBackend::PosixShm};
  api::RingMode mode{api::RingMode::OverwriteOldest};
  std::string name{};
  std::string hugetlbfs_mount{"/dev/hugepages"};
  std::size_t ring_bytes{8U << 20U};
  std::size_t max_record_bytes{64U << 10U};
  std::size_t max_readers{32};
  bool create{true};
  bool allow_hugepage_fallback{false};
  bool unlink_on_close{false};
};

struct ReaderHandle {
  std::uint32_t slot{kMaxReaders};
  std::uint64_t lease_token{};
  std::uint64_t epoch{};
  [[nodiscard]] explicit operator bool() const noexcept {
    return slot < kMaxReaders && lease_token != 0;
  }
};

struct RecordView {
  std::uint32_t type{};
  std::uint64_t sequence{};
  std::uint64_t epoch{};
  std::span<const std::byte> payload{};
};

class SharedRing;

// A view remains valid until commit(), release(), or destruction. commit()
// consumes the record; release() abandons it so the next read returns it again.
// A slow reader (including an outstanding lease) applies backpressure: publish
// returns QuotaExceeded instead of overwriting unread bytes.
class ReadLease {
public:
  ReadLease() = default;
  ~ReadLease();
  ReadLease(const ReadLease &) = delete;
  ReadLease &operator=(const ReadLease &) = delete;
  ReadLease(ReadLease &&other) noexcept;
  ReadLease &operator=(ReadLease &&other) noexcept;

  [[nodiscard]] const RecordView &view() const noexcept { return view_; }
  [[nodiscard]] const RecordView *operator->() const noexcept { return &view_; }
  [[nodiscard]] explicit operator bool() const noexcept { return owner_ != nullptr; }
  api::Result<void> commit() noexcept;
  void release() noexcept;

private:
  friend class SharedRing;
  ReadLease(SharedRing *owner, ReaderHandle handle,
            std::uint64_t record_cursor, std::uint64_t next_cursor,
            RecordView view) noexcept;

  SharedRing *owner_{};
  ReaderHandle handle_{};
  std::uint64_t record_cursor_{};
  std::uint64_t next_cursor_{};
  RecordView view_{};
};

class SharedRing {
public:
  SharedRing() = default;
  ~SharedRing();
  SharedRing(const SharedRing &) = delete;
  SharedRing &operator=(const SharedRing &) = delete;
  SharedRing(SharedRing &&other) noexcept;
  SharedRing &operator=(SharedRing &&other) noexcept;

  static api::Result<SharedRing> open(const RingOptions &options);
  // The ring has one producer. This call never waits: an unread record or
  // reader registry transition reports QuotaExceeded.
  api::Result<std::uint64_t> publish(std::uint32_t type,
                                     std::span<const std::byte> payload) noexcept;
  api::Result<ReaderHandle> register_reader(std::uint64_t start_marker,
                                            std::uint64_t now_ns) noexcept;
  api::Result<void> unregister_reader(ReaderHandle handle) noexcept;
  api::Result<void> heartbeat(ReaderHandle handle,
                              std::uint64_t now_ns) noexcept;
  api::Result<void> resync_to_latest(ReaderHandle &handle) noexcept;
  // Records never straddle the physical ring end. Larger logical snapshots
  // must be fragmented by the producer protocol; every fragment is an
  // independently leased record and is committed in order.
  // Hot-path variant: returns only an error code and never constructs an error
  // string. The output lease is changed only when Ok is returned.
  api::ErrorCode try_read(ReaderHandle &handle, ReadLease &lease) noexcept;
  api::Result<ReadLease> read(ReaderHandle &handle) noexcept;
  std::size_t reclaim_stale(std::uint64_t now_ns,
                            std::uint64_t lease_timeout_ns,
                            bool (*process_alive)(std::uint32_t,
                                                  std::uint64_t)) noexcept;

  [[nodiscard]] std::uint64_t epoch() const noexcept;
  [[nodiscard]] api::RingMode mode() const noexcept;
  [[nodiscard]] std::uint32_t registry_generation() const noexcept;
  [[nodiscard]] std::uint32_t active_reader_count() const noexcept;
  [[nodiscard]] std::uint64_t ring_bytes() const noexcept;
  [[nodiscard]] std::uint64_t max_record_bytes() const noexcept;
  [[nodiscard]] std::string_view name() const noexcept { return name_; }

private:
  friend class ReadLease;
  void close() noexcept;
  api::Result<void> commit_read(const ReaderHandle &handle,
                                std::uint64_t record_cursor,
                                std::uint64_t next_cursor,
                                std::uint64_t sequence) noexcept;
  bool valid_reader(const ReaderHandle &handle) const noexcept;
  std::byte *ring_data() const noexcept;

  int fd_{-1};
  void *mapping_{nullptr};
  std::size_t mapping_bytes_{};
  SegmentHeader *header_{nullptr};
  std::string name_{};
  bool owner_{false};
  bool posix_{false};
  bool unlink_on_close_{false};
};

std::string make_segment_name(std::string_view exchange,
                              std::string_view product,
                              std::string_view symbol,
                              std::string_view stream,
                              std::uint16_t schema_major,
                              std::uint32_t depth = 0);
std::uint64_t process_start_marker(std::uint32_t pid) noexcept;
bool process_identity_alive(std::uint32_t pid, std::uint64_t marker) noexcept;

} // namespace mds::transport
