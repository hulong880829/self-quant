#include "mds/record/record_reader.h"
#include "mds/record/recorder.h"
#include "mds/record/record_format.h"
#include "mds/publish/wire_publisher.h"

#include <zstd.h>

#include <algorithm>
#include <array>
#include <atomic>
#include <cassert>
#include <chrono>
#include <filesystem>
#include <fstream>
#include <stdexcept>
#include <thread>
#include <type_traits>
#include <unistd.h>
#include <vector>

namespace {

std::filesystem::path temporary_directory() {
  const auto path = std::filesystem::temp_directory_path() /
                    ("mds-recording-" + std::to_string(getpid()));
  std::filesystem::remove_all(path);
  std::filesystem::create_directories(path);
  return path;
}

std::vector<std::filesystem::path> shards(const std::filesystem::path &root) {
  std::vector<std::filesystem::path> result;
  for (const auto &entry :
       std::filesystem::recursive_directory_iterator(root)) {
    if (entry.is_regular_file() &&
        entry.path().filename().string().ends_with(".sqrec.zst")) {
      result.push_back(entry.path());
    }
  }
  return result;
}

std::string read_text(const std::filesystem::path &path) {
  std::ifstream input(path);
  return {std::istreambuf_iterator<char>(input),
          std::istreambuf_iterator<char>()};
}

template <typename T>
void put_le(std::vector<std::uint8_t> &output, T value) {
  using U = std::make_unsigned_t<T>;
  const auto bits = static_cast<U>(value);
  for (std::size_t index = 0; index < sizeof(T); ++index) {
    output.push_back(
        static_cast<std::uint8_t>(bits >> (index * 8U)));
  }
}

std::vector<std::uint8_t> legacy_v1_container(
    const mds::record::Record &record) {
  namespace detail = mds::record::detail;
  namespace wire = utils::md::wire;
  constexpr std::size_t kFrameHeader = 12;
  constexpr std::size_t kMetadataBytes = 44;
  const auto current_frame = detail::encode_record(record);
  const auto current_payload = std::span<const std::uint8_t>(
      current_frame.data() + kFrameHeader,
      current_frame.size() - kFrameHeader);

  std::vector<std::uint8_t> payload;
  payload.insert(payload.end(), current_payload.begin(),
                 current_payload.begin() + kMetadataBytes);
  const auto &header = record.bbo.header;
  put_le(payload, header.magic);
  put_le(payload, std::uint16_t{1});
  put_le(payload, std::uint16_t{1});
  put_le(payload, header.message_type);
  put_le(payload, std::uint16_t{408});
  put_le(payload, static_cast<std::uint32_t>(header.instrument_id));
  put_le(payload, header.bus_seq);
  put_le(payload, header.source_seq);
  put_le(payload, header.exchange_ts_ns);
  put_le(payload, header.receive_tsc);
  put_le(payload, header.publish_tsc);
  put_le(payload, header.book_generation);
  put_le(payload, header.state);
  put_le(payload, header.source_id);
  put_le(payload, header.flags);
  payload.insert(payload.end(),
                 current_payload.begin() + kMetadataBytes +
                     sizeof(wire::RecordHeader),
                 current_payload.end());

  std::vector<std::uint8_t> container;
  put_le(container, detail::kContainerMagic);
  put_le(container, std::uint16_t{1});
  put_le(container, std::uint16_t{0});
  put_le(container, std::uint32_t{0x01020304U});
  put_le(container, detail::crc32(container));
  put_le(container, detail::kFrameMagic);
  put_le(container, static_cast<std::uint32_t>(payload.size()));
  put_le(container, detail::crc32(payload));
  container.insert(container.end(), payload.begin(), payload.end());
  const auto trailer_start = container.size();
  put_le(container, detail::kTrailerMagic);
  put_le(container, std::uint64_t{1});
  put_le(container, detail::crc32(
                        std::span<const std::uint8_t>(container)
                            .subspan(trailer_start)));
  return container;
}

}  // namespace

int main() {
  using namespace std::chrono_literals;
  namespace record = mds::record;
  namespace consume = mds::consume;
  namespace wire = utils::md::wire;

  const auto root = temporary_directory();
  const auto stale = root / "2000-01-01/00/profile/symbol/aggbbo.sqrec.zst";
  std::filesystem::create_directories(stale.parent_path());
  std::ofstream(stale) << "old";
  std::filesystem::last_write_time(
      stale, std::filesystem::file_time_type::clock::now() - 48h);
  const auto stale_temp =
      root / "2000-01-01/00/profile/symbol/aggbbo.sqrec.zst.tmp.123";
  std::ofstream(stale_temp) << "incomplete";
  std::ofstream(root / "manifest.v1.json.tmp") << "incomplete";

  consume::AggregateLatestState bbo_state;
  consume::AggregateLatestState book_state;
  const auto bbo_segment = mds::publish::make_publisher_segment_name(
      "/test.prefix.with.dots", "agg.profile_with_dot", "BTC.USDT", "aggbbo");
  const auto book_segment = mds::publish::make_publisher_segment_name(
      "/test.prefix.with.dots", "agg.profile_with_dot", "BTC.USDT",
      "aggorderbook");
  assert(bbo_segment ==
         "/test.prefix.with.dots.agg_profile_with_dot.btc_usdt.aggbbo.2");
  std::vector<record::Topic> topics{
      {.segment = bbo_segment,
       .kind = consume::AggregateTopic::AggBbo,
       .latest = &bbo_state},
      {.segment = book_segment,
       .kind = consume::AggregateTopic::AggOrderBook,
       .latest = &book_state}};
  const auto current_hour =
      std::chrono::time_point_cast<std::chrono::hours>(
          std::chrono::system_clock::now());
  std::atomic<std::uint64_t> sample_wall_ns{
      static_cast<std::uint64_t>(
          std::chrono::duration_cast<std::chrono::nanoseconds>(
              current_hour.time_since_epoch())
              .count()) +
      1'000'000'000ULL};
  record::Recorder recorder(
      {.output_directory = root,
       .sample_interval_ms = 500,
       .retention_hours = 24,
       .depth = 2,
       .zstd_level = 1,
       .min_free_disk_bytes = 0,
       .wall_clock_ns = [&] {
         return sample_wall_ns.load(std::memory_order_relaxed);
       }},
      topics);
  if (!recorder.start()) {
    throw std::runtime_error("recorder start failed: " + recorder.error());
  }

  wire::AggBboRecord bbo{};
  bbo.header =
      wire::MakeHeader(utils::md::MessageType::AggBbo, sizeof(bbo));
  bbo.raw_cross_bps = 7;
  bbo.gated_cross_bps = 4;
  bbo_state.publish(bbo, {.ring_epoch = 1,
                          .ring_sequence = 1,
                          .receive_mono_ns = 10,
                          .receive_wall_ns = 20});
  bbo.raw_cross_bps = 3;
  bbo.gated_cross_bps = 8;
  bbo_state.publish(bbo, {.ring_epoch = 1,
                          .ring_sequence = 2,
                          .receive_mono_ns = 11,
                          .receive_wall_ns = 21});

  wire::AggOrderBookRecord book{};
  book.header =
      wire::MakeHeader(utils::md::MessageType::AggOrderBook, sizeof(book));
  book.header.state =
      static_cast<std::uint8_t>(utils::md::BookState::Live);
  book.member_count = 1;
  book.member_mask = 1;
  book.active_mask = 1;
  book.venue_slot_ids[0] = 1;
  book.bid_count = 4;
  book.ask_count = 3;
  const auto set_level = [](wire::AggLevel &level, std::int64_t price,
                            std::int64_t quantity) {
    level.price = price;
    level.quantity = quantity;
    level.venue_quantity[0] = quantity;
    level.venue_mask = 1;
    level.contributor_count = 1;
  };
  set_level(book.bids[0], 100, 2);
  set_level(book.bids[1], 99, 3);
  set_level(book.bids[2], 98, 4);
  set_level(book.bids[3], 97, 5);
  set_level(book.asks[0], 101, 3);
  set_level(book.asks[1], 102, 2);
  set_level(book.asks[2], 103, 4);
  book_state.publish(book, {.ring_epoch = 2, .ring_sequence = 8});

  std::this_thread::sleep_for(260ms);
  const auto active_manifest = read_text(root / "manifest.v1.json");
  assert(active_manifest.find("\"state\":\"recording\"") !=
         std::string::npos);
  assert(active_manifest.find(bbo_segment) != std::string::npos);
  assert(active_manifest.find(book_segment) != std::string::npos);
  sample_wall_ns.fetch_add(3'600'000'000'000ULL,
                           std::memory_order_relaxed);
  bbo_state.reset(consume::AggregateTopic::AggBbo,
                  {.ring_epoch = 1, .ring_sequence = 2});
  bbo.header =
      wire::MakeHeader(utils::md::MessageType::AggBbo, sizeof(bbo));
  bbo.raw_cross_bps = 1;
  bbo.gated_cross_bps = 2;
  bbo_state.publish(bbo, {.ring_epoch = 1,
                          .ring_sequence = 10,
                          .receive_mono_ns = 30,
                          .receive_wall_ns = 40});
  std::this_thread::sleep_for(560ms);
  recorder.stop();
  assert(!recorder.failed());
  assert(!std::filesystem::exists(stale));
  assert(!std::filesystem::exists(stale_temp));
  assert(std::filesystem::is_regular_file(root / "manifest.v1.json"));
  assert(!std::filesystem::exists(root / "manifest.v1.json.tmp"));
  const auto stopped_manifest = read_text(root / "manifest.v1.json");
  assert(stopped_manifest.find("\"state\":\"stopped\"") !=
         std::string::npos);
  assert(stopped_manifest.find("\"active\":[]") != std::string::npos);

  const auto files = shards(root);
  assert(files.size() == 4);
  for (const auto &file : files) {
    const auto text = file.generic_string();
    assert(text.find("/agg_profile_with_dot/btc_usdt/") != std::string::npos);
  }
  bool saw_bbo = false;
  bool saw_book = false;
  bool saw_reset_gap = false;
  std::uint64_t book_generation = 0;
  std::size_t book_records = 0;
  for (const auto &file : files) {
    const auto validated = record::validate_file(file);
    assert(validated);
    assert(validated.records == 1);
    const auto read = record::read_file(file, [&](const record::Record &value) {
      if (value.metadata.kind == record::Kind::AggBbo) {
        saw_bbo = true;
        assert((value.bbo.header.flags & wire::kBboOriginMask) == 0);
        if (value.metadata.ring_sequence == 2) {
          assert((value.metadata.flags & record::kGap) == 0);
          assert(value.cross_window.raw_min == 3);
          assert(value.cross_window.raw_max == 7);
          assert(value.cross_window.gated_min == 4);
          assert(value.cross_window.gated_max == 8);
          assert(value.cross_window.samples == 2);
        }
        if (value.metadata.ring_sequence == 10) {
          saw_reset_gap =
              (value.metadata.flags & (record::kReset | record::kGap)) ==
              (record::kReset | record::kGap);
        }
      } else {
        saw_book = true;
        assert((value.order_book.header.flags & wire::kBboOriginMask) == 0);
        ++book_records;
        assert(value.metadata.ring_sequence == 8);
        if (book_generation == 0) {
          book_generation = value.metadata.generation;
        }
        assert(value.metadata.generation == book_generation);
        assert(value.order_book.bid_count == 2);
        assert(value.order_book.ask_count == 2);
        assert(value.order_book.bids[1].price == 99);
        assert(value.order_book.asks[1].price == 102);
        assert(value.order_book.bids[2].price == 0);
      }
      return true;
    });
    assert(read);
  }
  assert(saw_bbo && saw_book && saw_reset_gap);
  assert(book_records == 2);

  record::Record legacy_source{};
  legacy_source.metadata = {.wall_ns = 1,
                            .mono_ns = 1,
                            .ring_epoch = 1,
                            .ring_sequence = 1,
                            .generation = 1,
                            .kind = record::Kind::AggBbo};
  legacy_source.bbo = bbo;
  legacy_source.bbo.header =
      wire::MakeHeader(utils::md::MessageType::AggBbo,
                       sizeof(wire::AggBboRecord));
  legacy_source.bbo.header.instrument_id = 42;
  bool decoded_legacy = false;
  const auto legacy = legacy_v1_container(legacy_source);
  const auto legacy_result = record::detail::decode_container(
      legacy, [&](const record::Record &value) {
        decoded_legacy =
            value.bbo.header.schema_major == wire::kSchemaMajor &&
            value.bbo.header.instrument_id == 42 &&
            value.bbo.header.record_length == sizeof(wire::AggBboRecord);
        return true;
      });
  assert(legacy_result && decoded_legacy);

  const auto source = files.front();
  const auto truncated = root / "truncated.sqrec.zst";
  const auto corrupt = root / "corrupt.sqrec.zst";
  std::ifstream input(source, std::ios::binary);
  std::vector<char> bytes((std::istreambuf_iterator<char>(input)),
                          std::istreambuf_iterator<char>());
  assert(bytes.size() > 16);
  std::ofstream(truncated, std::ios::binary)
      .write(bytes.data(), static_cast<std::streamsize>(bytes.size() - 8));
  bytes[bytes.size() / 2] ^= 0x40;
  std::ofstream(corrupt, std::ios::binary)
      .write(bytes.data(), static_cast<std::streamsize>(bytes.size()));
  assert(!record::validate_file(truncated));
  assert(!record::validate_file(corrupt));

  const auto before_invalid = shards(root);
  wire::AggOrderBookRecord invalid_book = book;
  invalid_book.bid_count = 1;
  invalid_book.ask_count = 1;
  invalid_book.bids[0].venue_quantity[0] = 1;
  book_state.publish(invalid_book, {.ring_epoch = 2, .ring_sequence = 9});
  const std::array invalid_topic{
      record::Topic{.segment = book_segment,
                    .kind = consume::AggregateTopic::AggOrderBook,
                    .latest = &book_state}};
  record::Recorder invalid_recorder(
      {.output_directory = root,
       .sample_interval_ms = 200,
       .retention_hours = 24,
       .depth = 2,
       .zstd_level = 1,
       .min_free_disk_bytes = 0},
      invalid_topic);
  assert(invalid_recorder.start());
  std::this_thread::sleep_for(230ms);
  invalid_recorder.stop();
  assert(!invalid_recorder.failed());
  const auto after_invalid = shards(root);
  assert(after_invalid.size() == before_invalid.size() + 1);
  bool rejected_invalid_compact = false;
  for (const auto &file : after_invalid) {
    if (std::find(before_invalid.begin(), before_invalid.end(), file) ==
        before_invalid.end()) {
      const auto result = record::validate_file(file);
      rejected_invalid_compact =
          result.error == record::ReadError::InvalidRecord;
    }
  }
  assert(rejected_invalid_compact);

  record::Record sample{};
  sample.metadata = {.wall_ns = 1,
                     .mono_ns = 1,
                     .ring_epoch = 1,
                     .ring_sequence = 1,
                     .generation = 1,
                     .kind = record::Kind::AggBbo};
  sample.bbo = bbo;
  auto uncompressed = record::detail::container_header();
  constexpr std::size_t large_record_count = 512;
  for (std::size_t index = 0; index < large_record_count; ++index) {
    sample.metadata.ring_sequence = index + 1;
    const auto frame = record::detail::encode_record(sample);
    uncompressed.insert(uncompressed.end(), frame.begin(), frame.end());
  }
  const auto trailer = record::detail::container_trailer(large_record_count);
  uncompressed.insert(uncompressed.end(), trailer.begin(), trailer.end());
  std::vector<std::uint8_t> compressed(ZSTD_compressBound(uncompressed.size()));
  const auto compressed_size =
      ZSTD_compress(compressed.data(), compressed.size(), uncompressed.data(),
                    uncompressed.size(), 1);
  assert(ZSTD_isError(compressed_size) == 0);
  compressed.resize(compressed_size);
  const auto large = root / "large.sqrec.zst";
  std::ofstream(large, std::ios::binary)
      .write(reinterpret_cast<const char *>(compressed.data()),
             static_cast<std::streamsize>(compressed.size()));
  const auto large_validated = record::validate_file(large);
  assert(large_validated && large_validated.records == large_record_count);
  std::size_t early_records = 0;
  const auto early = record::read_file(large, [&](const record::Record &) {
    return ++early_records < 3;
  });
  assert(early && early.records == 3 && early_records == 3);

  std::filesystem::remove_all(root);
  return 0;
}
