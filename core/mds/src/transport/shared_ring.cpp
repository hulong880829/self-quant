#include "mds/transport/shared_ring.h"

#include <algorithm>
#include <cerrno>
#include <chrono>
#include <csignal>
#include <cstring>
#include <fcntl.h>
#include <fstream>
#include <limits>
#include <new>
#include <random>
#include <sstream>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

namespace mds::transport {
namespace {

constexpr std::size_t align8(std::size_t value) noexcept {
  return (value + 7U) & ~std::size_t{7U};
}

constexpr bool is_power_of_two(std::uint64_t value) noexcept {
  return value != 0 && (value & (value - 1U)) == 0;
}

std::uint32_t crc32c(std::span<const std::byte> bytes) noexcept {
  // Castagnoli polynomial, reflected representation. This portable path is
  // deterministic across processes; a hardware-accelerated implementation
  // may replace it only if golden-vector tests remain identical.
  std::uint32_t crc = 0xffffffffU;
  for (const auto byte : bytes) {
    crc ^= std::to_integer<std::uint8_t>(byte);
    for (unsigned bit = 0; bit < 8; ++bit) {
      const std::uint32_t mask =
          0U - static_cast<std::uint32_t>(crc & 1U);
      crc = (crc >> 1U) ^ (0x82f63b78U & mask);
    }
  }
  return ~crc;
}

bool lock_free_atomics(const SegmentHeader &header) noexcept {
  RecordHeader record;
  return header.magic.is_lock_free() && header.writer_cursor.is_lock_free() &&
         header.next_sequence.is_lock_free() &&
         header.registry_generation.is_lock_free() &&
         header.readers[0].state.is_lock_free() &&
         header.readers[0].pid.is_lock_free() &&
         header.readers[0].process_start_marker.is_lock_free() &&
         header.readers[0].lease_token.is_lock_free() &&
         header.readers[0].cursor.is_lock_free() &&
         header.readers[0].heartbeat_ns.is_lock_free() &&
         record.commit_sequence.is_lock_free();
}

api::Result<SharedRing> open_error(api::ErrorCode code, std::string message) {
  return {.value = {}, .error = code, .message = std::move(message)};
}

std::string sanitize(std::string_view value) {
  std::string out;
  out.reserve(value.size());
  for (const char raw : value) {
    const auto c = static_cast<unsigned char>(raw);
    if ((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
        (c >= '0' && c <= '9') || c == '-' || c == '_') {
      out.push_back(static_cast<char>(c));
    } else {
      out.push_back('_');
    }
  }
  return out;
}

std::uint64_t new_token() noexcept {
  const auto now = std::chrono::steady_clock::now().time_since_epoch().count();
  std::uint64_t x = static_cast<std::uint64_t>(now) ^
                    (static_cast<std::uint64_t>(::getpid()) << 32U);
  x ^= x >> 12U;
  x ^= x << 25U;
  x ^= x >> 27U;
  return (x * 0x2545F4914F6CDD1DULL) | 1ULL;
}

} // namespace

SharedRing::~SharedRing() { close(); }

ReadLease::ReadLease(SharedRing *owner, ReaderHandle handle,
                     std::uint64_t record_cursor,
                     std::uint64_t next_cursor, RecordView view) noexcept
    : owner_(owner), handle_(handle), record_cursor_(record_cursor),
      next_cursor_(next_cursor), view_(view) {}

ReadLease::~ReadLease() { release(); }

ReadLease::ReadLease(ReadLease &&other) noexcept { *this = std::move(other); }

ReadLease &ReadLease::operator=(ReadLease &&other) noexcept {
  if (this != &other) {
    release();
    owner_ = std::exchange(other.owner_, nullptr);
    handle_ = other.handle_;
    record_cursor_ = other.record_cursor_;
    next_cursor_ = other.next_cursor_;
    view_ = other.view_;
  }
  return *this;
}

api::Result<void> ReadLease::commit() noexcept {
  if (!owner_) {
    return {.error = api::ErrorCode::InvalidHandle,
            .message = "read lease is no longer active"};
  }
  auto *owner = std::exchange(owner_, nullptr);
  return owner->commit_read(handle_, record_cursor_, next_cursor_,
                            view_.sequence);
}

void ReadLease::release() noexcept {
  owner_ = nullptr;
  view_ = {};
}

SharedRing::SharedRing(SharedRing &&other) noexcept { *this = std::move(other); }

SharedRing &SharedRing::operator=(SharedRing &&other) noexcept {
  if (this == &other) {
    return *this;
  }
  close();
  fd_ = std::exchange(other.fd_, -1);
  mapping_ = std::exchange(other.mapping_, nullptr);
  mapping_bytes_ = std::exchange(other.mapping_bytes_, 0);
  header_ = std::exchange(other.header_, nullptr);
  name_ = std::move(other.name_);
  owner_ = std::exchange(other.owner_, false);
  posix_ = std::exchange(other.posix_, false);
  unlink_on_close_ = std::exchange(other.unlink_on_close_, false);
  return *this;
}

api::Result<SharedRing> SharedRing::open(const RingOptions &options) {
  const auto mode = static_cast<std::uint32_t>(options.mode);
  if (options.name.empty() || options.max_readers == 0 ||
      options.max_readers > kMaxReaders ||
      (mode != static_cast<std::uint32_t>(
                   api::RingMode::OverwriteOldest) &&
       mode != static_cast<std::uint32_t>(api::RingMode::Lossless)) ||
      !is_power_of_two(options.ring_bytes) ||
      options.max_record_bytes < sizeof(RecordHeader) ||
      options.max_record_bytes >
          std::numeric_limits<std::uint32_t>::max() ||
      options.max_record_bytes > options.ring_bytes / 8U) {
    return open_error(api::ErrorCode::InvalidConfig,
                      "ring size must be power-of-two and record <= ring/8");
  }

  SharedRing result;
  result.owner_ = options.create;
  result.unlink_on_close_ = options.unlink_on_close;
  result.posix_ = options.backend == api::ShmBackend::PosixShm;
  result.name_ = options.name;

  const int flags = O_RDWR | (options.create ? (O_CREAT | O_EXCL) : 0);
  if (result.posix_) {
    std::string shm_name = options.name.front() == '/' ? options.name
                                                       : "/" + options.name;
    result.name_ = shm_name;
    result.fd_ = ::shm_open(shm_name.c_str(), flags, 0660);
  } else {
    const std::string path =
        options.hugetlbfs_mount + "/" + sanitize(options.name);
    result.name_ = path;
    result.fd_ = ::open(path.c_str(), flags, 0660);
    if (result.fd_ < 0 && options.allow_hugepage_fallback) {
      result.posix_ = true;
      result.name_ = options.name.front() == '/' ? options.name
                                                 : "/" + options.name;
      result.fd_ = ::shm_open(result.name_.c_str(), flags, 0660);
    }
  }
  if (result.fd_ < 0) {
    return open_error(options.backend == api::ShmBackend::Hugetlbfs
                          ? api::ErrorCode::HugepageUnavailable
                          : api::ErrorCode::ShmCreateFailed,
                      std::strerror(errno));
  }

  if (options.create) {
    result.mapping_bytes_ =
        align8(sizeof(SegmentHeader)) + options.ring_bytes;
    if (::ftruncate(result.fd_, static_cast<off_t>(result.mapping_bytes_)) != 0) {
      return open_error(api::ErrorCode::ShmCreateFailed, std::strerror(errno));
    }
  } else {
    struct stat status {};
    if (::fstat(result.fd_, &status) != 0 ||
        status.st_size < static_cast<off_t>(sizeof(SegmentHeader))) {
      return open_error(api::ErrorCode::ShmCreateFailed, "invalid segment");
    }
    result.mapping_bytes_ = static_cast<std::size_t>(status.st_size);
  }

  result.mapping_ = ::mmap(nullptr, result.mapping_bytes_,
                           PROT_READ | PROT_WRITE, MAP_SHARED, result.fd_, 0);
  if (result.mapping_ == MAP_FAILED) {
    result.mapping_ = nullptr;
    return open_error(api::ErrorCode::ShmCreateFailed, std::strerror(errno));
  }
  result.header_ = static_cast<SegmentHeader *>(result.mapping_);
  if (options.create) {
    std::memset(result.mapping_, 0, result.mapping_bytes_);
    new (result.header_) SegmentHeader();
    if (!lock_free_atomics(*result.header_)) {
      return open_error(api::ErrorCode::InvalidConfig,
                        "shared-memory atomics are not lock-free");
    }
    result.header_->header_bytes =
        static_cast<std::uint32_t>(align8(sizeof(SegmentHeader)));
    result.header_->ring_bytes = options.ring_bytes;
    result.header_->max_record_bytes = options.max_record_bytes;
    result.header_->max_readers =
        static_cast<std::uint32_t>(options.max_readers);
    result.header_->control.mode =
        static_cast<std::uint32_t>(options.mode);
    result.header_->epoch = new_token();
    result.header_->magic.store(kRingMagic, std::memory_order_release);
  } else {
    const auto magic = result.header_->magic.load(std::memory_order_acquire);
    const auto header_bytes = result.header_->header_bytes;
    const auto ring_bytes = result.header_->ring_bytes;
    const auto max_record_bytes = result.header_->max_record_bytes;
    const auto max_readers = result.header_->max_readers;
    const auto mode = result.header_->control.mode;
    const bool valid =
        magic == kRingMagic &&
        result.header_->schema_major == kRingSchemaMajor &&
        result.header_->epoch != 0 &&
        header_bytes == align8(sizeof(SegmentHeader)) &&
        header_bytes % alignof(SegmentHeader) == 0 &&
        header_bytes % alignof(RecordHeader) == 0 &&
        is_power_of_two(ring_bytes) &&
        max_record_bytes >= sizeof(RecordHeader) &&
        max_record_bytes <= std::numeric_limits<std::uint32_t>::max() &&
        max_record_bytes <= ring_bytes / 8U && max_readers != 0 &&
        max_readers <= kMaxReaders && header_bytes <= result.mapping_bytes_ &&
        ring_bytes <= result.mapping_bytes_ - header_bytes &&
        (mode == static_cast<std::uint32_t>(
                     api::RingMode::OverwriteOldest) ||
         mode == static_cast<std::uint32_t>(api::RingMode::Lossless)) &&
        reinterpret_cast<std::uintptr_t>(result.mapping_) %
                alignof(SegmentHeader) ==
            0 &&
        lock_free_atomics(*result.header_);
    if (!valid) {
      return open_error(api::ErrorCode::InvalidConfig,
                        "segment header is incompatible or corrupt");
    }
  }
  return {.value = std::move(result)};
}

api::Result<std::uint64_t>
SharedRing::publish(std::uint32_t type,
                    std::span<const std::byte> payload) noexcept {
  if (!header_ || payload.size() >
                      header_->max_record_bytes - sizeof(RecordHeader)) {
    return {.error = api::ErrorCode::InvalidConfig,
            .message = "record exceeds configured maximum"};
  }
  const std::size_t length = align8(sizeof(RecordHeader) + payload.size());
  if (type == kPaddingType || length > header_->max_record_bytes) {
    return {.error = api::ErrorCode::InvalidConfig,
            .message = "record exceeds configured maximum"};
  }

  std::uint64_t cursor = header_->writer_cursor.load(std::memory_order_relaxed);
  std::size_t offset = cursor & (header_->ring_bytes - 1U);
  const std::size_t remaining = header_->ring_bytes - offset;
  const std::uint64_t advance =
      static_cast<std::uint64_t>(remaining < length ? remaining + length
                                                    : length);
  if (mode() == api::RingMode::Lossless) {
    std::uint64_t oldest = cursor;
    for (std::uint32_t i = 0; i < header_->max_readers; ++i) {
      auto &slot = header_->readers[i];
      const auto state = static_cast<ReaderState>(
          slot.state.load(std::memory_order_acquire));
      if (state == ReaderState::Initializing ||
          state == ReaderState::Suspect) {
        return {.error = api::ErrorCode::QuotaExceeded,
                .message = "reader state transition applies backpressure"};
      }
      if (state == ReaderState::Active) {
        oldest =
            std::min(oldest, slot.cursor.load(std::memory_order_acquire));
      }
    }
    if (cursor < oldest || advance > header_->ring_bytes ||
        cursor - oldest > header_->ring_bytes - advance) {
      return {.error = api::ErrorCode::QuotaExceeded,
              .message = "slow reader prevents ring overwrite"};
    }
  }
  if (remaining < length) {
    if (remaining >= sizeof(RecordHeader)) {
      auto *padding = new (ring_data() + offset) RecordHeader();
      padding->length = static_cast<std::uint32_t>(remaining);
      padding->type = kPaddingType;
      padding->payload_bytes = 0;
      padding->epoch = header_->epoch;
      padding->sequence = 0;
      padding->commit_sequence.store(1, std::memory_order_release);
    }
    cursor += remaining;
    offset = 0;
  }

  const std::uint64_t sequence =
      header_->next_sequence.fetch_add(1, std::memory_order_relaxed);
  auto *record = new (ring_data() + offset) RecordHeader();
  record->length = static_cast<std::uint32_t>(length);
  record->type = type;
  record->payload_bytes = static_cast<std::uint32_t>(payload.size());
  record->payload_crc32c = crc32c(payload);
  record->epoch = header_->epoch;
  record->sequence = sequence;
  record->commit_sequence.store(0, std::memory_order_relaxed);
  std::memcpy(record + 1, payload.data(), payload.size());
  if (length > sizeof(RecordHeader) + payload.size()) {
    std::memset(reinterpret_cast<std::byte *>(record + 1) + payload.size(), 0,
                length - sizeof(RecordHeader) - payload.size());
  }
  record->commit_sequence.store(sequence, std::memory_order_release);
  header_->writer_cursor.store(cursor + length, std::memory_order_release);
  return {.value = sequence};
}

api::Result<ReaderHandle>
SharedRing::register_reader(std::uint64_t start_marker,
                            std::uint64_t now_ns) noexcept {
  if (!header_) {
    return {.error = api::ErrorCode::NotInitialized};
  }
  for (std::uint32_t i = 0; i < header_->max_readers; ++i) {
    auto &slot = header_->readers[i];
    std::uint32_t expected = static_cast<std::uint32_t>(ReaderState::Free);
    if (!slot.state.compare_exchange_strong(
            expected, static_cast<std::uint32_t>(ReaderState::Initializing),
            std::memory_order_acq_rel)) {
      continue;
    }
    const auto token = new_token();
    slot.pid.store(static_cast<std::uint32_t>(::getpid()),
                   std::memory_order_relaxed);
    slot.process_start_marker.store(start_marker, std::memory_order_relaxed);
    slot.lease_token.store(token, std::memory_order_relaxed);
    slot.cursor.store(header_->writer_cursor.load(std::memory_order_acquire),
                      std::memory_order_relaxed);
    slot.heartbeat_ns.store(now_ns, std::memory_order_relaxed);
    slot.state.store(static_cast<std::uint32_t>(ReaderState::Active),
                     std::memory_order_release);
    header_->registry_generation.fetch_add(1, std::memory_order_release);
    return {.value = {i, token, header_->epoch}};
  }
  return {.error = api::ErrorCode::QuotaExceeded,
          .message = "reader registry is full"};
}

bool SharedRing::valid_reader(const ReaderHandle &handle) const noexcept {
  return header_ && handle.slot < header_->max_readers &&
         handle.epoch == header_->epoch &&
         header_->readers[handle.slot].lease_token.load(
             std::memory_order_relaxed) == handle.lease_token &&
         header_->readers[handle.slot].state.load(std::memory_order_acquire) ==
             static_cast<std::uint32_t>(ReaderState::Active);
}

api::Result<void>
SharedRing::unregister_reader(ReaderHandle handle) noexcept {
  if (!valid_reader(handle)) {
    return {.error = api::ErrorCode::InvalidHandle};
  }
  auto &slot = header_->readers[handle.slot];
  slot.lease_token.store(0, std::memory_order_relaxed);
  slot.state.store(static_cast<std::uint32_t>(ReaderState::Free),
                   std::memory_order_release);
  header_->registry_generation.fetch_add(1, std::memory_order_release);
  return {};
}

api::Result<void> SharedRing::heartbeat(ReaderHandle handle,
                                        std::uint64_t now_ns) noexcept {
  if (!valid_reader(handle)) {
    return {.error = api::ErrorCode::InvalidHandle};
  }
  header_->readers[handle.slot].heartbeat_ns.store(now_ns,
                                                   std::memory_order_release);
  return {};
}

api::Result<void>
SharedRing::resync_to_latest(ReaderHandle &handle) noexcept {
  if (!valid_reader(handle)) {
    return {.error = api::ErrorCode::InvalidHandle};
  }
  auto &slot = header_->readers[handle.slot];
  slot.cursor.store(header_->writer_cursor.load(std::memory_order_acquire),
                    std::memory_order_release);
  header_->registry_generation.fetch_add(1, std::memory_order_release);
  return {};
}

api::ErrorCode SharedRing::try_read(ReaderHandle &handle,
                                    ReadLease &lease) noexcept {
  if (!valid_reader(handle)) {
    return api::ErrorCode::InvalidHandle;
  }
  auto &slot = header_->readers[handle.slot];
  std::uint64_t cursor = slot.cursor.load(std::memory_order_relaxed);
  const std::uint64_t writer =
      header_->writer_cursor.load(std::memory_order_acquire);
  if (cursor == writer) {
    return api::ErrorCode::QuotaExceeded;
  }
  if (writer - cursor > header_->ring_bytes) {
    return api::ErrorCode::SubscriptionRejected;
  }

  for (;;) {
    const std::size_t offset = cursor & (header_->ring_bytes - 1U);
    const std::size_t remaining = header_->ring_bytes - offset;
    if (remaining < sizeof(RecordHeader)) {
      cursor += remaining;
      continue;
    }
    auto *record =
        reinterpret_cast<const RecordHeader *>(ring_data() + offset);
    const std::uint64_t committed =
        record->commit_sequence.load(std::memory_order_acquire);
    const auto overwritten = [&]() noexcept {
      const auto writer_now =
          header_->writer_cursor.load(std::memory_order_acquire);
      return record->commit_sequence.load(std::memory_order_acquire) !=
                 committed ||
             writer_now < cursor ||
             writer_now - cursor > header_->ring_bytes;
    };
    const auto length = record->length;
    const auto type = record->type;
    if (length < sizeof(RecordHeader) || length > remaining ||
        length > header_->max_record_bytes) {
      if (overwritten()) {
        return api::ErrorCode::RecordOverwritten;
      }
      return api::ErrorCode::InternalError;
    }
    if (type == kPaddingType) {
      if (overwritten()) {
        return api::ErrorCode::RecordOverwritten;
      }
      cursor += length;
      continue;
    }
    if (committed == 0 || committed != record->sequence) {
      if (overwritten()) {
        return api::ErrorCode::RecordOverwritten;
      }
      return api::ErrorCode::QuotaExceeded;
    }
    const auto epoch = record->epoch;
    const auto sequence = record->sequence;
    const auto payload_bytes = record->payload_bytes;
    const auto payload_crc32c = record->payload_crc32c;
    if (epoch != header_->epoch ||
        payload_bytes > length - sizeof(RecordHeader)) {
      if (overwritten()) {
        return api::ErrorCode::RecordOverwritten;
      }
      return api::ErrorCode::InternalError;
    }
    const std::span<const std::byte> payload{
        reinterpret_cast<const std::byte *>(record + 1),
        payload_bytes};
    const auto actual_crc32c = crc32c(payload);
    if (overwritten()) {
      return api::ErrorCode::RecordOverwritten;
    }
    if (actual_crc32c != payload_crc32c) {
      return api::ErrorCode::InternalError;
    }
    RecordView view{type, sequence, epoch, payload};
    lease = ReadLease(this, handle, cursor, cursor + length, view);
    return api::ErrorCode::Ok;
  }
}

api::Result<ReadLease> SharedRing::read(ReaderHandle &handle) noexcept {
  ReadLease lease;
  const auto error = try_read(handle, lease);
  if (error == api::ErrorCode::Ok) {
    return {.value = std::move(lease)};
  }
  const char *message = "shared ring read failed";
  switch (error) {
  case api::ErrorCode::QuotaExceeded:
    message = "no record available";
    break;
  case api::ErrorCode::SubscriptionRejected:
    message = "reader overrun; snapshot rebuild required";
    break;
  case api::ErrorCode::RecordOverwritten:
    message = "record changed while reading";
    break;
  case api::ErrorCode::InvalidHandle:
    message = "invalid shared ring reader handle";
    break;
  case api::ErrorCode::InternalError:
    message = "shared ring record is corrupt";
    break;
  default:
    break;
  }
  return {.error = error, .message = message};
}

api::Result<void>
SharedRing::commit_read(const ReaderHandle &handle,
                        std::uint64_t record_cursor,
                        std::uint64_t next_cursor,
                        std::uint64_t sequence) noexcept {
  if (!valid_reader(handle)) {
    return {.error = api::ErrorCode::InvalidHandle};
  }
  auto &slot = header_->readers[handle.slot];
  const auto current = slot.cursor.load(std::memory_order_relaxed);
  const auto writer = header_->writer_cursor.load(std::memory_order_acquire);
  if (writer < record_cursor ||
      writer - record_cursor > header_->ring_bytes) {
    return {.error = api::ErrorCode::RecordOverwritten,
            .message = "read lease was overwritten"};
  }
  const auto offset = record_cursor & (header_->ring_bytes - 1U);
  const auto *record =
      reinterpret_cast<const RecordHeader *>(ring_data() + offset);
  if (record->commit_sequence.load(std::memory_order_acquire) != sequence) {
    return {.error = api::ErrorCode::RecordOverwritten,
            .message = "read lease sequence changed before commit"};
  }
  if (next_cursor <= current || next_cursor > writer ||
      next_cursor - current > header_->ring_bytes) {
    return {.error = api::ErrorCode::InvalidHandle,
            .message = "read lease cursor is stale"};
  }
  slot.cursor.store(next_cursor, std::memory_order_release);
  return {};
}

std::size_t SharedRing::reclaim_stale(
    std::uint64_t now_ns, std::uint64_t lease_timeout_ns,
    bool (*process_alive)(std::uint32_t, std::uint64_t)) noexcept {
  std::size_t reclaimed = 0;
  for (std::uint32_t i = 0; header_ && i < header_->max_readers; ++i) {
    auto &slot = header_->readers[i];
    auto state = static_cast<ReaderState>(
        slot.state.load(std::memory_order_acquire));
    if (state != ReaderState::Active &&
        state != ReaderState::Initializing) {
      continue;
    }
    const auto heartbeat = slot.heartbeat_ns.load(std::memory_order_acquire);
    if (now_ns < heartbeat || now_ns - heartbeat <= lease_timeout_ns) {
      continue;
    }
    auto expected = static_cast<std::uint32_t>(state);
    if (!slot.state.compare_exchange_strong(
            expected, static_cast<std::uint32_t>(ReaderState::Suspect),
            std::memory_order_acq_rel)) {
      continue;
    }
    const auto refreshed = slot.heartbeat_ns.load(std::memory_order_acquire);
    if (state == ReaderState::Active &&
        (now_ns < refreshed || now_ns - refreshed <= lease_timeout_ns)) {
      slot.state.store(static_cast<std::uint32_t>(ReaderState::Active),
                       std::memory_order_release);
      continue;
    }
    const auto pid = slot.pid.load(std::memory_order_relaxed);
    const auto marker =
        slot.process_start_marker.load(std::memory_order_relaxed);
    if (process_alive && !process_alive(pid, marker)) {
      slot.lease_token.store(0, std::memory_order_relaxed);
      slot.state.store(static_cast<std::uint32_t>(ReaderState::Free),
                       std::memory_order_release);
      header_->registry_generation.fetch_add(1, std::memory_order_release);
      ++reclaimed;
    } else {
      slot.state.store(static_cast<std::uint32_t>(state),
                       std::memory_order_release);
    }
  }
  return reclaimed;
}

std::uint64_t SharedRing::epoch() const noexcept {
  return header_ ? header_->epoch : 0;
}

api::RingMode SharedRing::mode() const noexcept {
  return header_ ? static_cast<api::RingMode>(header_->control.mode)
                 : api::RingMode::OverwriteOldest;
}

std::uint32_t SharedRing::registry_generation() const noexcept {
  return header_
             ? header_->registry_generation.load(std::memory_order_acquire)
             : 0;
}

std::uint32_t SharedRing::active_reader_count() const noexcept {
  std::uint32_t count = 0;
  for (std::uint32_t index = 0; header_ && index < header_->max_readers;
       ++index) {
    if (header_->readers[index].state.load(std::memory_order_acquire) ==
        static_cast<std::uint32_t>(ReaderState::Active)) {
      ++count;
    }
  }
  return count;
}

std::byte *SharedRing::ring_data() const noexcept {
  return static_cast<std::byte *>(mapping_) + header_->header_bytes;
}

void SharedRing::close() noexcept {
  if (mapping_) {
    ::munmap(mapping_, mapping_bytes_);
  }
  if (fd_ >= 0) {
    ::close(fd_);
  }
  if (owner_ && unlink_on_close_ && !name_.empty()) {
    if (posix_) {
      ::shm_unlink(name_.c_str());
    } else {
      ::unlink(name_.c_str());
    }
  }
  mapping_ = nullptr;
  header_ = nullptr;
  fd_ = -1;
}

std::string make_segment_name(std::string_view exchange,
                              std::string_view product,
                              std::string_view symbol,
                              std::string_view stream,
                              std::uint16_t schema_major,
                              std::uint32_t depth) {
  std::string name = "/selfquant.mds." + sanitize(exchange) + "." +
                     sanitize(product) + "." + sanitize(symbol) + "." +
                     sanitize(stream);
  if (depth != 0) {
    name += ".d" + std::to_string(depth);
  }
  name += "." + std::to_string(schema_major);
  if (name.size() > 240) {
    return {};
  }
  return name;
}

std::uint64_t process_start_marker(std::uint32_t pid) noexcept {
  std::ifstream input("/proc/" + std::to_string(pid) + "/stat");
  std::string line;
  if (!std::getline(input, line)) {
    return 0;
  }
  const auto end = line.rfind(')');
  if (end == std::string::npos) {
    return 0;
  }
  std::istringstream fields(line.substr(end + 2));
  std::string ignored;
  for (int field = 3; field < 22; ++field) {
    if (!(fields >> ignored)) {
      return 0;
    }
  }
  std::uint64_t marker = 0;
  fields >> marker;
  return marker;
}

bool process_identity_alive(std::uint32_t pid, std::uint64_t marker) noexcept {
  return marker != 0 && ::kill(static_cast<pid_t>(pid), 0) == 0 &&
         process_start_marker(pid) == marker;
}

} // namespace mds::transport
