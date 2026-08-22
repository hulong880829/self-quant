#include "mds/record/record_reader.h"

#include "mds/record/record_format.h"

#include <zstd.h>

#include <array>
#include <cerrno>
#include <cstring>
#include <fcntl.h>
#include <memory>
#include <type_traits>
#include <unistd.h>
#include <vector>

namespace mds::record {
namespace {

template <typename T>
T read_le(std::span<const std::uint8_t> bytes, std::size_t offset) {
  using U = std::make_unsigned_t<T>;
  U value{};
  for (std::size_t index = 0; index < sizeof(T); ++index) {
    value |= static_cast<U>(bytes[offset + index]) << (index * 8U);
  }
  return static_cast<T>(value);
}

class StreamingContainer {
 public:
  bool feed(std::span<const std::uint8_t> bytes,
            const RecordVisitor &visitor, ReadResult &result) {
    if (trailer_seen_ && !bytes.empty()) {
      result = failure(ReadError::Corrupt,
                       "data follows the container trailer");
      return false;
    }
    pending_.insert(pending_.end(), bytes.begin(), bytes.end());
    for (;;) {
      const auto available = pending_.size() - offset_;
      if (!header_seen_) {
        if (available < kHeaderSize) {
          return true;
        }
        header_.assign(pending_.begin() +
                           static_cast<std::ptrdiff_t>(offset_),
                       pending_.begin() +
                           static_cast<std::ptrdiff_t>(offset_ + kHeaderSize));
        auto probe = header_;
        const auto trailer = detail::container_trailer(0);
        probe.insert(probe.end(), trailer.begin(), trailer.end());
        const auto validated = detail::decode_container(probe, {});
        if (!validated) {
          result = validated;
          return false;
        }
        offset_ += kHeaderSize;
        header_seen_ = true;
        compact();
        continue;
      }
      if (available < sizeof(std::uint32_t)) {
        return true;
      }
      const auto view =
          std::span<const std::uint8_t>(pending_).subspan(offset_);
      const auto magic = read_le<std::uint32_t>(view, 0);
      if (magic == detail::kTrailerMagic) {
        if (available < kTrailerSize) {
          return true;
        }
        const auto trailer = view.first(kTrailerSize);
        const auto expected = read_le<std::uint64_t>(trailer, 4);
        const auto stored_crc = read_le<std::uint32_t>(trailer, 12);
        if (stored_crc != detail::crc32(trailer.first(12)) ||
            expected != records_) {
          result = failure(ReadError::Corrupt,
                           "invalid container trailer");
          return false;
        }
        offset_ += kTrailerSize;
        trailer_seen_ = true;
        if (offset_ != pending_.size()) {
          result = failure(ReadError::Corrupt,
                           "data follows the container trailer");
          return false;
        }
        pending_.clear();
        offset_ = 0;
        result = {.error = ReadError::Ok, .message = {}, .records = records_};
        return false;
      }
      if (magic != detail::kFrameMagic) {
        result = failure(ReadError::Corrupt, "invalid record frame");
        return false;
      }
      if (available < kFrameHeaderSize) {
        return true;
      }
      const auto length = read_le<std::uint32_t>(view, 4);
      if (length > kMaximumFrameBytes) {
        result = failure(ReadError::Corrupt,
                         "record frame exceeds configured maximum");
        return false;
      }
      const auto frame_size =
          kFrameHeaderSize + static_cast<std::size_t>(length);
      if (available < frame_size) {
        return true;
      }
      std::vector<std::uint8_t> single;
      single.reserve(kHeaderSize + frame_size + kTrailerSize);
      single.insert(single.end(), header_.begin(), header_.end());
      single.insert(single.end(),
                    pending_.begin() + static_cast<std::ptrdiff_t>(offset_),
                    pending_.begin() +
                        static_cast<std::ptrdiff_t>(offset_ + frame_size));
      const auto trailer = detail::container_trailer(1);
      single.insert(single.end(), trailer.begin(), trailer.end());
      bool keep_reading = true;
      const auto decoded = detail::decode_container(
          single, [&](const Record &record) {
            keep_reading = !visitor || visitor(record);
            return keep_reading;
          });
      if (!decoded) {
        result = decoded;
        result.records += records_;
        return false;
      }
      ++records_;
      offset_ += frame_size;
      if (!keep_reading) {
        result = {.error = ReadError::Ok,
                  .message = {},
                  .records = records_};
        return false;
      }
      compact();
    }
  }

  ReadResult finish() const {
    if (trailer_seen_) {
      return {.error = ReadError::Ok, .message = {}, .records = records_};
    }
    return failure(ReadError::Truncated,
                   header_seen_ ? "container trailer is missing"
                                : "container header is truncated");
  }

  [[nodiscard]] bool complete() const noexcept { return trailer_seen_; }

 private:
  static constexpr std::size_t kHeaderSize = 16;
  static constexpr std::size_t kFrameHeaderSize = 12;
  static constexpr std::size_t kTrailerSize = 16;
  static constexpr std::uint32_t kMaximumFrameBytes = 1U << 20U;

  ReadResult failure(ReadError error, std::string message) const {
    return {.error = error, .message = std::move(message), .records = records_};
  }

  void compact() {
    if (offset_ >= 64U * 1024U && offset_ * 2 >= pending_.size()) {
      pending_.erase(
          pending_.begin(),
          pending_.begin() + static_cast<std::ptrdiff_t>(offset_));
      offset_ = 0;
    }
  }

  std::vector<std::uint8_t> pending_;
  std::vector<std::uint8_t> header_;
  std::size_t offset_{};
  std::uint64_t records_{};
  bool header_seen_{};
  bool trailer_seen_{};
};

}  // namespace

ReadResult read_file(const std::filesystem::path &path,
                     const RecordVisitor &visitor) {
  const int fd = open(path.c_str(), O_RDONLY | O_CLOEXEC);
  if (fd < 0) {
    return {.error = ReadError::Io,
            .message = "open failed: " + std::string(std::strerror(errno))};
  }
  struct Close {
    int fd;
    ~Close() { (void)close(fd); }
  } close_fd{fd};

#if defined(POSIX_FADV_SEQUENTIAL)
  (void)posix_fadvise(fd, 0, 0, POSIX_FADV_SEQUENTIAL);
#endif

  using Context = std::unique_ptr<ZSTD_DCtx, decltype(&ZSTD_freeDCtx)>;
  Context context(ZSTD_createDCtx(), &ZSTD_freeDCtx);
  if (!context) {
    return {.error = ReadError::Io,
            .message = "failed to allocate zstd context"};
  }

  std::array<std::uint8_t, 64 * 1024> input{};
  std::array<std::uint8_t, 64 * 1024> output{};
  StreamingContainer decoder;
  ReadResult decoded;
  std::size_t zstd_remaining = 1;
  bool saw_input = false;
  bool parsing = true;
  bool container_complete_seen = false;
  for (;;) {
    const auto count = read(fd, input.data(), input.size());
    if (count < 0) {
      if (errno == EINTR) {
        continue;
      }
      return {.error = ReadError::Io,
              .message = "read failed: " + std::string(std::strerror(errno))};
    }
    if (count == 0) {
      break;
    }
    saw_input = true;
    ZSTD_inBuffer source{input.data(), static_cast<std::size_t>(count), 0};
    while (source.pos < source.size && parsing) {
      ZSTD_outBuffer destination{output.data(), output.size(), 0};
      zstd_remaining =
          ZSTD_decompressStream(context.get(), &destination, &source);
      if (ZSTD_isError(zstd_remaining) != 0) {
        return {.error = ReadError::Corrupt,
                .message = std::string("zstd: ") +
                           ZSTD_getErrorName(zstd_remaining)};
      }
      if (destination.pos != 0) {
        parsing = decoder.feed(
            std::span<const std::uint8_t>(output).first(destination.pos),
            visitor, decoded);
        if (!parsing && decoded.error == ReadError::Ok &&
            decoder.complete() && !container_complete_seen) {
          container_complete_seen = true;
          parsing = true;
        }
      }
    }
    if (!parsing) {
      break;
    }
  }
  if (!parsing) {
    return decoded;
  }
  if (!saw_input || zstd_remaining != 0) {
    return {.error = ReadError::Truncated,
            .message = "compressed stream is truncated"};
  }
#if defined(POSIX_FADV_DONTNEED)
  (void)posix_fadvise(fd, 0, 0, POSIX_FADV_DONTNEED);
#endif
  return decoder.finish();
}

ReadResult validate_file(const std::filesystem::path &path) {
  return read_file(path, {});
}

}  // namespace mds::record
