#include "mds/consume/segment_reader.h"

#include "utils/runtime/timestamp.h"

#include <unistd.h>

#include <utility>

namespace mds::consume {

BorrowedSegmentRecord::BorrowedSegmentRecord(
    transport::ReadLease &&lease) noexcept
    : lease_(std::move(lease)) {}

BorrowedSegmentRecord::BorrowedSegmentRecord(
    BorrowedSegmentRecord &&other) noexcept
    : lease_(std::move(other.lease_)) {}

BorrowedSegmentRecord &BorrowedSegmentRecord::operator=(
    BorrowedSegmentRecord &&other) noexcept {
  if (this != &other) {
    lease_ = std::move(other.lease_);
  }
  return *this;
}

BorrowedSegmentRecord::~BorrowedSegmentRecord() = default;

std::uint32_t BorrowedSegmentRecord::type() const noexcept {
  return lease_ ? lease_.view().type : 0;
}

std::uint64_t BorrowedSegmentRecord::sequence() const noexcept {
  return lease_ ? lease_.view().sequence : 0;
}

std::uint64_t BorrowedSegmentRecord::epoch() const noexcept {
  return lease_ ? lease_.view().epoch : 0;
}

std::span<const std::byte> BorrowedSegmentRecord::payload() const noexcept {
  return lease_ ? lease_.view().payload : std::span<const std::byte>{};
}

BorrowedSegmentRecord::operator bool() const noexcept {
  return static_cast<bool>(lease_);
}

api::Result<void> BorrowedSegmentRecord::commit() noexcept {
  return lease_.commit();
}

SegmentReader::~SegmentReader() { close(); }

SegmentReader::SegmentReader(SegmentReader &&other) noexcept
    : ring_(std::move(other.ring_)),
      reader_(other.reader_),
      options_(std::move(other.options_)),
      marker_(other.marker_),
      epoch_(other.epoch_),
      last_heartbeat_ns_(other.last_heartbeat_ns_) {
  other.reader_ = {};
}

SegmentReader &SegmentReader::operator=(SegmentReader &&other) noexcept {
  if (this != &other) {
    close();
    ring_ = std::move(other.ring_);
    reader_ = other.reader_;
    options_ = std::move(other.options_);
    marker_ = other.marker_;
    epoch_ = other.epoch_;
    last_heartbeat_ns_ = other.last_heartbeat_ns_;
    other.reader_ = {};
  }
  return *this;
}

api::Result<SegmentReader>
SegmentReader::open(std::string_view segment_name,
                    const SegmentReaderOptions &options) {
  if (segment_name.empty()) {
    return {.value = {},
            .error = api::ErrorCode::InvalidConfig,
            .message = "segment name must not be empty"};
  }
  transport::RingOptions ring_options;
  ring_options.backend = options.backend;
  ring_options.name = std::string(segment_name);
  ring_options.hugetlbfs_mount = options.hugetlbfs_mount;
  ring_options.create = false;
  ring_options.allow_hugepage_fallback = options.allow_hugepage_fallback;
  auto opened = transport::SharedRing::open(ring_options);
  if (!opened) {
    return {.value = {},
            .error = opened.error,
            .message = std::move(opened.message)};
  }

  SegmentReader result;
  result.ring_ = std::move(opened.value);
  result.options_ = options;
  result.marker_ = transport::process_start_marker(
      static_cast<std::uint32_t>(::getpid()));
  auto registered = result.register_reader();
  if (!registered) {
    return {.value = {},
            .error = registered.error,
            .message = std::move(registered.message)};
  }
  return {.value = std::move(result)};
}

api::Result<transport::ReadLease> SegmentReader::acquire_lease() {
  if (!reader_) {
    return {.value = {},
            .error = api::ErrorCode::InvalidHandle,
            .message = "segment reader is not open"};
  }
  if (ring_.epoch() != epoch_) {
    (void)ring_.unregister_reader(reader_);
    reader_ = {};
    auto registered = register_reader();
    return {.value = {},
            .error = registered ? api::ErrorCode::RecordOverwritten
                                : registered.error,
            .message = registered
                           ? "producer epoch changed; reader re-registered"
                           : std::move(registered.message)};
  }

  const auto now = utils::runtime::Timestamp::NowMono();
  if (now - last_heartbeat_ns_ >= options_.heartbeat_interval_ns) {
    auto heartbeat = ring_.heartbeat(reader_, now);
    if (!heartbeat) {
      return {.value = {},
              .error = heartbeat.error,
              .message = std::move(heartbeat.message)};
    }
    last_heartbeat_ns_ = now;
  }

  transport::ReadLease lease;
  const auto error = ring_.try_read(reader_, lease);
  if (error == api::ErrorCode::QuotaExceeded) {
    return {};
  }
  if (error != api::ErrorCode::Ok) {
    return {.value = {},
            .error = error,
            .message = "shared-memory read failed"};
  }
  return {.value = std::move(lease)};
}

api::Result<std::optional<SegmentRecord>> SegmentReader::try_read() {
  auto leased = acquire_lease();
  if (!leased) {
    return {.value = std::nullopt,
            .error = leased.error,
            .message = std::move(leased.message)};
  }
  if (!leased.value) {
    return {.value = std::nullopt};
  }
  const auto &view = leased.value.view();
  SegmentRecord record;
  record.type = view.type;
  record.sequence = view.sequence;
  record.epoch = view.epoch;
  record.storage.assign(view.payload.begin(), view.payload.end());
  auto committed = leased.value.commit();
  if (!committed) {
    return {.value = std::nullopt,
            .error = committed.error,
            .message = std::move(committed.message)};
  }
  return {.value = std::move(record)};
}

api::Result<std::optional<BorrowedSegmentRecord>>
SegmentReader::try_read_view() {
  auto leased = acquire_lease();
  if (!leased) {
    return {.value = std::nullopt,
            .error = leased.error,
            .message = std::move(leased.message)};
  }
  if (!leased.value) {
    return {.value = std::nullopt};
  }
  return {.value = BorrowedSegmentRecord(std::move(leased.value))};
}

std::string_view SegmentReader::segment_name() const noexcept {
  return ring_.name();
}

api::Result<void> SegmentReader::resync_to_latest() noexcept {
  if (!reader_) {
    return {.error = api::ErrorCode::InvalidHandle,
            .message = "segment reader is not open"};
  }
  return ring_.resync_to_latest(reader_);
}

api::Result<void> SegmentReader::register_reader() noexcept {
  const auto now = utils::runtime::Timestamp::NowMono();
  auto registered = ring_.register_reader(marker_, now);
  if (!registered) {
    return {.error = registered.error,
            .message = std::move(registered.message)};
  }
  reader_ = registered.value;
  epoch_ = ring_.epoch();
  last_heartbeat_ns_ = now;
  return {};
}

void SegmentReader::close() noexcept {
  if (reader_) {
    (void)ring_.unregister_reader(reader_);
    reader_ = {};
  }
}

}  // namespace mds::consume
