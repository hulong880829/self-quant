#include "mds/record/recorder.h"

#include "mds/record/record_format.h"
#include "utils/md/wire.h"

#include <zstd.h>

#include <algorithm>
#include <array>
#include <cerrno>
#include <chrono>
#include <cstring>
#include <deque>
#include <fcntl.h>
#include <filesystem>
#include <fstream>
#include <iomanip>
#include <mutex>
#include <sstream>
#include <sys/stat.h>
#include <sys/types.h>
#include <thread>
#include <unistd.h>
#include <vector>

namespace mds::record {
namespace {

std::uint64_t wall_now_ns() noexcept {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::system_clock::now().time_since_epoch())
          .count());
}

std::uint64_t mono_now_ns() noexcept {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::steady_clock::now().time_since_epoch())
          .count());
}

struct Identity {
  std::string profile;
  std::string symbol;
  std::string stream;
};

Identity parse_identity(const std::string &segment) {
  Identity result;
  const auto schema = std::to_string(utils::md::wire::kSchemaMajor);
  const auto bbo_suffix_storage = ".aggbbo." + schema;
  const auto book_suffix_storage = ".aggorderbook." + schema;
  const std::string_view bbo_suffix = bbo_suffix_storage;
  const std::string_view book_suffix = book_suffix_storage;
  std::string_view suffix;
  if (segment.ends_with(bbo_suffix)) {
    suffix = bbo_suffix;
    result.stream = "aggbbo";
  } else if (segment.ends_with(book_suffix)) {
    suffix = book_suffix;
    result.stream = "aggorderbook";
  } else {
    return result;
  }
  const auto identity_size = segment.size() - suffix.size();
  const auto symbol_dot = segment.rfind('.', identity_size - 1);
  if (segment.empty() || segment.front() != '/' ||
      symbol_dot == std::string::npos || symbol_dot + 1 >= identity_size) {
    return {};
  }
  const auto profile_dot = segment.rfind('.', symbol_dot - 1);
  if (profile_dot == std::string::npos || profile_dot + 1 == symbol_dot) {
    return {};
  }
  // Publisher name components are sanitized before joining. Parsing from the
  // right keeps dots in the prefix from becoming part of the profile.
  result.profile =
      segment.substr(profile_dot + 1, symbol_dot - profile_dot - 1);
  result.symbol =
      segment.substr(symbol_dot + 1, identity_size - symbol_dot - 1);
  return result;
}

struct Hour {
  std::time_t start{};
  std::string date;
  std::string hour;
};

Hour utc_hour(std::uint64_t wall_ns) {
  const auto seconds = static_cast<std::time_t>(wall_ns / 1'000'000'000ULL);
  const auto start = seconds - seconds % 3600;
  std::tm value{};
  gmtime_r(&start, &value);
  std::array<char, 16> date{};
  std::array<char, 4> hour{};
  (void)std::strftime(date.data(), date.size(), "%Y-%m-%d", &value);
  (void)std::strftime(hour.data(), hour.size(), "%H", &value);
  return {.start = start, .date = date.data(), .hour = hour.data()};
}

bool write_all(int fd, std::span<const std::uint8_t> bytes,
               std::string &error) {
  std::size_t offset = 0;
  while (offset < bytes.size()) {
    const auto count =
        write(fd, bytes.data() + offset, bytes.size() - offset);
    if (count < 0) {
      if (errno == EINTR) {
        continue;
      }
      error = std::strerror(errno);
      return false;
    }
    if (count == 0) {
      error = "write returned zero bytes";
      return false;
    }
    offset += static_cast<std::size_t>(count);
  }
  return true;
}

bool sync_directory(const std::filesystem::path &directory) {
  const int fd = open(directory.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC);
  if (fd < 0) {
    return false;
  }
  const bool ok = fsync(fd) == 0;
  (void)close(fd);
  return ok;
}

std::string json_escape(std::string_view value) {
  std::string output;
  for (const char character : value) {
    if (character == '\\' || character == '"') {
      output.push_back('\\');
    }
    output.push_back(character);
  }
  return output;
}

class ZstdShard {
 public:
  ZstdShard() = default;
  ~ZstdShard() { abandon(); }
  ZstdShard(const ZstdShard &) = delete;
  ZstdShard &operator=(const ZstdShard &) = delete;

  bool open_at(const std::filesystem::path &final_path, int level,
               std::time_t hour, std::string &error) {
    final_path_ = final_path;
    temp_path_ = final_path;
    temp_path_ += ".tmp." + std::to_string(getpid());
    hour_ = hour;
    std::error_code filesystem_error;
    std::filesystem::create_directories(final_path.parent_path(),
                                        filesystem_error);
    if (filesystem_error) {
      error = filesystem_error.message();
      return false;
    }
    fd_ = ::open(temp_path_.c_str(),
                 O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC, 0644);
    if (fd_ < 0) {
      error = std::strerror(errno);
      return false;
    }
    stream_ = ZSTD_createCStream();
    if (stream_ == nullptr) {
      error = "failed to allocate zstd stream";
      return false;
    }
    const auto status = ZSTD_initCStream(stream_, level);
    if (ZSTD_isError(status) != 0) {
      error = ZSTD_getErrorName(status);
      return false;
    }
    records_ = 0;
    start_wall_ns_ = wall_now_ns();
    end_wall_ns_ = 0;
    return append(detail::container_header(), ZSTD_e_continue, error);
  }

  bool append_record(const Record &record, std::string &error) {
    if (!append(detail::encode_record(record), ZSTD_e_continue, error)) {
      return false;
    }
    ++records_;
    end_wall_ns_ = record.metadata.wall_ns;
    return flush(error);
  }

  bool finalize(std::string &error) {
    if (fd_ < 0) {
      return true;
    }
    if (!append(detail::container_trailer(records_), ZSTD_e_end, error)) {
      return false;
    }
    if (fsync(fd_) != 0) {
      error = std::strerror(errno);
      return false;
    }
    if (close(fd_) != 0) {
      fd_ = -1;
      error = std::strerror(errno);
      return false;
    }
    fd_ = -1;
    ZSTD_freeCStream(stream_);
    stream_ = nullptr;
    if (rename(temp_path_.c_str(), final_path_.c_str()) != 0) {
      error = std::strerror(errno);
      return false;
    }
    if (!sync_directory(final_path_.parent_path())) {
      error = "failed to sync shard directory";
      return false;
    }
    return true;
  }

  [[nodiscard]] bool open() const noexcept { return fd_ >= 0; }
  [[nodiscard]] std::time_t hour() const noexcept { return hour_; }

 private:
  bool append(std::span<const std::uint8_t> bytes, ZSTD_EndDirective mode,
              std::string &error) {
    ZSTD_inBuffer input{bytes.data(), bytes.size(), 0};
    std::array<std::uint8_t, 64 * 1024> output{};
    std::size_t remaining = 1;
    do {
      ZSTD_outBuffer destination{output.data(), output.size(), 0};
      remaining = ZSTD_compressStream2(stream_, &destination, &input, mode);
      if (ZSTD_isError(remaining) != 0) {
        error = ZSTD_getErrorName(remaining);
        return false;
      }
      if (!write_all(fd_,
                     std::span<const std::uint8_t>(output.data(),
                                                   destination.pos),
                     error)) {
        return false;
      }
    } while (input.pos < input.size ||
             (mode != ZSTD_e_continue && remaining != 0));
    return true;
  }

  bool flush(std::string &error) {
    if (!append({}, ZSTD_e_flush, error)) {
      return false;
    }
    if (fdatasync(fd_) != 0) {
      error = std::strerror(errno);
      return false;
    }
#if defined(POSIX_FADV_DONTNEED)
    if (posix_fadvise(fd_, 0, 0, POSIX_FADV_DONTNEED) != 0) {
      // Advisory cache eviction is best-effort.
    }
#endif
    return true;
  }

  void abandon() noexcept {
    if (stream_ != nullptr) {
      ZSTD_freeCStream(stream_);
      stream_ = nullptr;
    }
    if (fd_ >= 0) {
      (void)close(fd_);
      fd_ = -1;
    }
  }

  int fd_{-1};
  ZSTD_CStream *stream_{};
  std::filesystem::path final_path_;
  std::filesystem::path temp_path_;
  std::time_t hour_{};
  std::uint64_t records_{};
  std::uint64_t start_wall_ns_{};
  std::uint64_t end_wall_ns_{};
};

}  // namespace

struct Recorder::Impl {
  struct TopicState {
    Topic topic;
    Identity identity;
    ZstdShard shard;
    std::uint64_t last_sequence{};
    std::uint64_t last_epoch{};
    std::uint64_t last_reset_generation{};
    std::uint64_t last_snapshot_generation{};
    std::uint64_t bbo_rollover_request{};
    bool pending_reset{};
  };

  RecorderOptions options;
  std::deque<TopicState> topics;
  std::thread writer;
  std::atomic<bool> stop_requested{};
  std::atomic<bool> failed{};
  std::atomic<bool> started{};
  mutable std::mutex error_mutex;
  std::string error_message;
  std::string_view manifest_state{"starting"};

  void fail(std::string message) {
    {
      std::lock_guard lock(error_mutex);
      error_message = std::move(message);
    }
    failed.store(true, std::memory_order_release);
  }

  std::filesystem::path shard_path(const TopicState &topic,
                                   const Hour &hour) const {
    const auto directory = options.output_directory / hour.date / hour.hour /
                           topic.identity.profile / topic.identity.symbol;
    auto path = directory / (topic.identity.stream + ".sqrec.zst");
    std::error_code filesystem_error;
    if (std::filesystem::exists(path, filesystem_error)) {
      path = directory /
             (topic.identity.stream + "." + std::to_string(wall_now_ns()) +
              ".sqrec.zst");
    }
    return path;
  }

  bool ensure_shard(TopicState &topic, const Hour &hour, std::string &error) {
    if (topic.shard.open() && topic.shard.hour() == hour.start) {
      return true;
    }
    if (topic.shard.open()) {
      if (!topic.shard.finalize(error) || !cleanup_retention(error) ||
          !update_manifest(error)) {
        return false;
      }
    }
    if (!topic.shard.open_at(shard_path(topic, hour), options.zstd_level,
                             hour.start, error)) {
      return false;
    }
    return true;
  }

  bool enough_disk(std::string &error) const {
    std::error_code filesystem_error;
    const auto space =
        std::filesystem::space(options.output_directory, filesystem_error);
    if (filesystem_error) {
      error = filesystem_error.message();
      return false;
    }
    if (space.available < options.min_free_disk_bytes) {
      error = "recording disk low-water threshold reached";
      return false;
    }
    return true;
  }

  static void compact_book(const utils::md::wire::AggOrderBookRecord &source,
                           std::size_t depth,
                           utils::md::wire::AggOrderBookRecord &target) {
    target = source;
    target.bids = {};
    target.asks = {};
    target.bid_count = 0;
    target.ask_count = 0;
    const auto bid_limit =
        std::min<std::size_t>({depth, source.bid_count, kMaximumDepth});
    const auto source_bid_count = std::min<std::size_t>(
        source.bid_count, utils::md::wire::kAggMaxLevelsPerSide);
    for (std::size_t index = 0;
         index < source_bid_count && target.bid_count < bid_limit; ++index) {
      if (source.bids[index].price > 0 && source.bids[index].quantity > 0) {
        target.bids[target.bid_count++] = source.bids[index];
      }
    }
    const auto ask_limit =
        std::min<std::size_t>({depth, source.ask_count, kMaximumDepth});
    const auto source_ask_count = std::min<std::size_t>(
        source.ask_count, utils::md::wire::kAggMaxLevelsPerSide);
    for (std::size_t index = 0;
         index < source_ask_count && target.ask_count < ask_limit; ++index) {
      if (source.asks[index].price > 0 && source.asks[index].quantity > 0) {
        target.asks[target.ask_count++] = source.asks[index];
      }
    }
  }

  template <typename Snapshot>
  std::uint16_t flags_for(TopicState &topic, const Snapshot &snapshot) {
    std::uint16_t flags{};
    if (topic.pending_reset ||
        snapshot.status.reset_generation != topic.last_reset_generation) {
      flags |= kReset;
    }
    const auto &receive = snapshot.status.receive;
    if (topic.last_snapshot_generation != 0 &&
        snapshot.status.generation != topic.last_snapshot_generation &&
        (receive.ring_epoch != topic.last_epoch ||
         receive.ring_sequence != topic.last_sequence + 1)) {
      flags |= kGap;
    }
    topic.pending_reset = false;
    topic.last_reset_generation = snapshot.status.reset_generation;
    topic.last_snapshot_generation = snapshot.status.generation;
    topic.last_sequence = receive.ring_sequence;
    topic.last_epoch = receive.ring_epoch;
    return flags;
  }

  bool sample(TopicState &topic, std::uint64_t sample_wall,
              std::uint64_t sample_mono, std::string &error) {
    Record record;
    record.metadata.wall_ns = sample_wall;
    record.metadata.mono_ns = sample_mono;
    if (topic.topic.kind == consume::AggregateTopic::AggBbo) {
      consume::AggBboSnapshot snapshot{};
      if (!topic.topic.latest->snapshot(snapshot)) {
        return true;
      }
      if (!snapshot.status.ready) {
        topic.pending_reset = true;
        topic.last_reset_generation = snapshot.status.reset_generation;
        topic.last_sequence = snapshot.status.receive.ring_sequence;
        topic.last_epoch = snapshot.status.receive.ring_epoch;
        return true;
      }
      record.metadata.kind = Kind::AggBbo;
      record.metadata.flags = flags_for(topic, snapshot);
      record.metadata.ring_epoch = snapshot.status.receive.ring_epoch;
      record.metadata.ring_sequence = snapshot.status.receive.ring_sequence;
      record.metadata.generation = snapshot.status.generation;
      record.bbo = snapshot.record;
      auto cross_window = snapshot.cross_window;
      if (topic.bbo_rollover_request != 0) {
        consume::AggBboWindowSnapshot completed{};
        if (topic.topic.latest->snapshot_bbo_window(
                topic.bbo_rollover_request, completed)) {
          cross_window = completed.cross_window;
        }
      }
      record.cross_window = {
          .start_mono_ns = cross_window.start_mono_ns,
          .end_mono_ns = cross_window.end_mono_ns,
          .raw_min = cross_window.raw_min,
          .raw_max = cross_window.raw_max,
          .gated_min = cross_window.gated_min,
          .gated_max = cross_window.gated_max,
          .samples = cross_window.sample_count};
      topic.bbo_rollover_request =
          topic.topic.latest->request_bbo_window_rollover();
    } else {
      consume::AggOrderBookSnapshot snapshot{};
      if (!topic.topic.latest->snapshot(snapshot)) {
        return true;
      }
      if (!snapshot.status.ready) {
        topic.pending_reset = true;
        topic.last_reset_generation = snapshot.status.reset_generation;
        topic.last_sequence = snapshot.status.receive.ring_sequence;
        topic.last_epoch = snapshot.status.receive.ring_epoch;
        return true;
      }
      record.metadata.kind = Kind::AggOrderBook;
      record.metadata.flags = flags_for(topic, snapshot);
      record.metadata.ring_epoch = snapshot.status.receive.ring_epoch;
      record.metadata.ring_sequence = snapshot.status.receive.ring_sequence;
      record.metadata.generation = snapshot.status.generation;
      compact_book(snapshot.record, options.depth, record.order_book);
    }
    const auto hour = utc_hour(sample_wall);
    return ensure_shard(topic, hour, error) &&
           topic.shard.append_record(record, error);
  }

  bool update_manifest(std::string &error) {
    std::vector<std::filesystem::path> shards;
    std::error_code filesystem_error;
    for (std::filesystem::recursive_directory_iterator iterator(
             options.output_directory, filesystem_error),
         end;
         !filesystem_error && iterator != end; iterator.increment(filesystem_error)) {
      if (iterator->is_regular_file() &&
          iterator->path().extension() == ".zst" &&
          iterator->path().filename().string().ends_with(".sqrec.zst")) {
        shards.push_back(std::filesystem::relative(iterator->path(),
                                                   options.output_directory));
      }
    }
    if (filesystem_error) {
      error = filesystem_error.message();
      return false;
    }
    std::sort(shards.begin(), shards.end());
    const auto temporary = options.output_directory / "manifest.v1.json.tmp";
    const auto final = options.output_directory / "manifest.v1.json";
    const int fd =
        open(temporary.c_str(), O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC, 0644);
    if (fd < 0) {
      error = std::strerror(errno);
      return false;
    }
    std::ostringstream json;
    json << "{\"version\":1,\"state\":\"" << manifest_state
         << "\",\"sample_interval_ms\":" << options.sample_interval_ms
         << ",\"retention_hours\":" << options.retention_hours
         << ",\"depth\":" << options.depth << ",\"active\":[";
    if (manifest_state == "recording") {
      for (std::size_t index = 0; index < topics.size(); ++index) {
        const auto &topic = topics[index];
        json << (index == 0 ? "" : ",")
             << "{\"segment\":\"" << json_escape(topic.topic.segment)
             << "\",\"kind\":\""
             << (topic.topic.kind == consume::AggregateTopic::AggBbo
                     ? "aggbbo"
                     : "aggorderbook")
             << "\"}";
      }
    }
    json << "],\"shards\":[";
    for (std::size_t index = 0; index < shards.size(); ++index) {
      json << (index == 0 ? "" : ",") << '"'
           << json_escape(shards[index].generic_string()) << '"';
    }
    json << "]}\n";
    const auto text = json.str();
    const bool wrote =
        write_all(fd,
                  {reinterpret_cast<const std::uint8_t *>(text.data()),
                   text.size()},
                  error);
    const bool synced = wrote && fsync(fd) == 0;
    const bool closed = close(fd) == 0;
    if (!synced || !closed) {
      if (error.empty()) {
        error = std::strerror(errno);
      }
      return false;
    }
    if (rename(temporary.c_str(), final.c_str()) != 0 ||
        !sync_directory(options.output_directory)) {
      error = std::strerror(errno);
      return false;
    }
    return true;
  }

  bool cleanup_retention(std::string &error) {
    const auto cutoff =
        std::chrono::system_clock::now() -
        std::chrono::hours(options.retention_hours);
    const auto current_hour =
        std::chrono::time_point_cast<std::chrono::hours>(
            std::chrono::system_clock::now());
    std::error_code filesystem_error;
    for (std::filesystem::recursive_directory_iterator iterator(
             options.output_directory, filesystem_error),
         end;
         !filesystem_error && iterator != end;
         iterator.increment(filesystem_error)) {
      if (!iterator->is_regular_file() ||
          !iterator->path().filename().string().ends_with(".sqrec.zst")) {
        continue;
      }
      const auto modified = iterator->last_write_time(filesystem_error);
      if (filesystem_error) {
        break;
      }
      const auto system_modified =
          std::chrono::time_point_cast<std::chrono::system_clock::duration>(
              modified - decltype(modified)::clock::now() +
              std::chrono::system_clock::now());
      if (system_modified < cutoff && system_modified < current_hour) {
        std::filesystem::remove(iterator->path(), filesystem_error);
      }
    }
    if (filesystem_error) {
      error = filesystem_error.message();
      return false;
    }
    return true;
  }

  bool startup_cleanup(std::string &error) {
    std::error_code filesystem_error;
    std::filesystem::create_directories(options.output_directory,
                                        filesystem_error);
    if (filesystem_error) {
      error = filesystem_error.message();
      return false;
    }
    for (std::filesystem::recursive_directory_iterator iterator(
             options.output_directory, filesystem_error),
         end;
         !filesystem_error && iterator != end; iterator.increment(filesystem_error)) {
      if (!iterator->is_regular_file()) {
        continue;
      }
      const auto name = iterator->path().filename().string();
      if (name.find(".sqrec.zst.tmp.") != std::string::npos ||
          name == "manifest.v1.json.tmp") {
        std::filesystem::remove(iterator->path(), filesystem_error);
      }
      if (filesystem_error) {
        break;
      }
    }
    if (filesystem_error) {
      error = filesystem_error.message();
      return false;
    }
    return cleanup_retention(error) && update_manifest(error);
  }

  void run() {
    std::string error;
    if (!startup_cleanup(error) || !enough_disk(error)) {
      fail(std::move(error));
      return;
    }
    manifest_state = "recording";
    if (!update_manifest(error)) {
      fail(std::move(error));
      return;
    }
    const auto interval =
        std::chrono::milliseconds(options.sample_interval_ms);
    while (!stop_requested.load(std::memory_order_acquire)) {
      const auto next = std::chrono::steady_clock::now() + interval;
      const auto wall =
          options.wall_clock_ns ? options.wall_clock_ns() : wall_now_ns();
      const auto mono = mono_now_ns();
      if (!enough_disk(error)) {
        fail(std::move(error));
        break;
      }
      for (auto &topic : topics) {
        if (!sample(topic, wall, mono, error)) {
          fail(topic.topic.segment + ": " + error);
          break;
        }
      }
      if (failed.load(std::memory_order_acquire)) {
        break;
      }
      std::this_thread::sleep_until(next);
    }
    if (!failed.load(std::memory_order_acquire)) {
      for (auto &topic : topics) {
        if (topic.shard.open() && !topic.shard.finalize(error)) {
          fail(topic.topic.segment + ": " + error);
          break;
        }
      }
      manifest_state =
          failed.load(std::memory_order_acquire) ? "failed" : "stopped";
      if (!update_manifest(error)) {
        fail(std::move(error));
      }
    } else {
      manifest_state = "failed";
      std::string ignored;
      (void)update_manifest(ignored);
    }
  }
};

Recorder::Recorder(RecorderOptions options, std::span<const Topic> topics)
    : impl_(std::make_unique<Impl>()) {
  impl_->options = std::move(options);
  for (const auto &topic : topics) {
    impl_->topics.emplace_back();
    impl_->topics.back().topic = topic;
    impl_->topics.back().identity = parse_identity(topic.segment);
  }
}

Recorder::~Recorder() { stop(); }

bool Recorder::start() {
  if (impl_->started.exchange(true, std::memory_order_acq_rel)) {
    impl_->fail("recorder has already been started");
    return false;
  }
  if (impl_->options.output_directory.empty() ||
      impl_->options.sample_interval_ms < 200 ||
      impl_->options.retention_hours == 0 ||
      impl_->options.retention_hours > 24 || impl_->options.depth == 0 ||
      impl_->options.depth > kMaximumDepth || impl_->options.zstd_level < -7 ||
      impl_->options.zstd_level > 22 || impl_->topics.empty()) {
    impl_->fail("invalid recorder options");
    return false;
  }
  for (const auto &topic : impl_->topics) {
    if (topic.topic.latest == nullptr || topic.identity.profile.empty() ||
        topic.identity.symbol.empty() || topic.identity.stream.empty() ||
        (topic.identity.stream == "aggbbo" &&
         topic.topic.kind != consume::AggregateTopic::AggBbo) ||
        (topic.identity.stream == "aggorderbook" &&
         topic.topic.kind != consume::AggregateTopic::AggOrderBook)) {
      impl_->fail("invalid recording topic");
      return false;
    }
  }
  impl_->writer = std::thread([this] { impl_->run(); });
  return true;
}

void Recorder::stop() noexcept {
  impl_->stop_requested.store(true, std::memory_order_release);
  if (impl_->writer.joinable()) {
    impl_->writer.join();
  }
}

bool Recorder::failed() const noexcept {
  return impl_->failed.load(std::memory_order_acquire);
}

std::string Recorder::error() const {
  std::lock_guard lock(impl_->error_mutex);
  return impl_->error_message;
}

}  // namespace mds::record
