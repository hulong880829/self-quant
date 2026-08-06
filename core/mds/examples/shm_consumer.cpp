#include "mds/transport/shared_ring.h"
#include "utils/md/wire_codec.h"
#include "utils/runtime/timestamp.h"

#include <algorithm>
#include <array>
#include <atomic>
#include <chrono>
#include <charconv>
#include <csignal>
#include <cstdint>
#include <iostream>
#include <limits>
#include <map>
#include <span>
#include <string>
#include <string_view>
#include <thread>
#include <type_traits>
#include <unordered_map>
#include <utility>
#include <variant>
#include <vector>
#include <unistd.h>

namespace {

namespace md = utils::md;
namespace wire = utils::md::wire;
namespace transport = mds::transport;

std::atomic<bool> stop_requested{false};

extern "C" void request_stop(int) noexcept {
  stop_requested.store(true, std::memory_order_relaxed);
}

struct Options {
  bool raw{};
  bool bbo_only{};
  std::size_t book_depth{};
  std::vector<std::string> segments;
};

std::string_view state_name(std::uint8_t state) noexcept {
  switch (static_cast<md::BookState>(state)) {
    case md::BookState::Empty:
      return "Empty";
    case md::BookState::Building:
      return "Building";
    case md::BookState::Live:
      return "Live";
    case md::BookState::Invalid:
      return "Invalid";
    case md::BookState::NeedsRestart:
      return "NeedsRestart";
  }
  return "Unknown";
}

std::string_view type_name(std::uint16_t type) noexcept {
  switch (static_cast<md::MessageType>(type)) {
    case md::MessageType::Bbo:
      return "BBO";
    case md::MessageType::Ticker:
      return "Ticker";
    case md::MessageType::BookDelta:
      return "Delta";
    case md::MessageType::SnapshotBegin:
      return "SnapshotBegin";
    case md::MessageType::SnapshotChunk:
      return "SnapshotChunk";
    case md::MessageType::SnapshotEnd:
      return "SnapshotEnd";
    case md::MessageType::InstrumentUpdate:
      return "Instrument";
  }
  return "Unknown";
}

std::string_view codec_error_name(wire::CodecError error) noexcept {
  switch (error) {
    case wire::CodecError::Ok:
      return "ok";
    case wire::CodecError::BufferTooSmall:
      return "buffer-too-small";
    case wire::CodecError::BadMagic:
      return "bad-magic";
    case wire::CodecError::UnsupportedSchema:
      return "unsupported-schema";
    case wire::CodecError::UnknownMessageType:
      return "unknown-type";
    case wire::CodecError::LengthMismatch:
      return "length-mismatch";
    case wire::CodecError::InvalidField:
      return "invalid-field";
  }
  return "unknown-error";
}

template <std::size_t Size>
std::string fixed_text(const std::array<char, Size> &value) {
  std::size_t length = 0;
  while (length < Size && value[length] != '\0') {
    ++length;
  }
  return std::string(value.data(), length);
}

std::string decimal_text(std::int64_t mantissa, std::uint8_t scale) {
  const bool negative = mantissa < 0;
  const std::uint64_t magnitude =
      negative ? std::uint64_t{0} - static_cast<std::uint64_t>(mantissa)
               : static_cast<std::uint64_t>(mantissa);
  std::string digits = std::to_string(magnitude);
  const std::size_t places = scale;
  if (places != 0) {
    if (digits.size() <= places) {
      digits.insert(0, places + 1 - digits.size(), '0');
    }
    digits.insert(digits.size() - places, 1, '.');
  }
  if (negative) {
    digits.insert(0, 1, '-');
  }
  return digits;
}

std::string profile_text(const md::Instrument &instrument) {
  std::string profile;
  switch (instrument.venue) {
    case md::Venue::Binance:
      profile = "binance/";
      break;
    case md::Venue::Okx:
      profile = "okx/";
      break;
    case md::Venue::Bybit:
      profile = "bybit/";
      break;
    case md::Venue::Gate:
      profile = "gate/";
      break;
    case md::Venue::Bitget:
      profile = "bitget/";
      break;
    case md::Venue::Polymarket:
      profile = "polymarket/";
      break;
    case md::Venue::Sse:
      profile = "sse/";
      break;
    case md::Venue::Unknown:
      profile = "unknown/";
      break;
  }
  switch (instrument.product_type) {
    case md::ProductType::Spot:
      return profile + "spot";
    case md::ProductType::Perpetual:
      return profile + "perpetual";
    case md::ProductType::Future:
      return profile + "future";
    case md::ProductType::BinaryOption:
      return profile + "binary-option";
    case md::ProductType::Equity:
      return profile + "equity";
    case md::ProductType::Unknown:
      return profile + "unknown";
  }
  return profile + "unknown";
}

struct SnapshotBeginDecoded {
  wire::SnapshotBeginRecord value;
};

struct SnapshotEndDecoded {
  wire::SnapshotEndRecord value;
};

using DecodedRecord = std::variant<
    std::monostate, wire::InstrumentUpdateRecord, wire::BboRecord,
    wire::TickerRecord, wire::DeltaRecord, SnapshotBeginDecoded,
    wire::SnapshotChunkRecord, SnapshotEndDecoded>;

class Decoder final : public wire::RecordVisitor {
 public:
  bool OnInstrument(
      const wire::InstrumentUpdateRecord &record) noexcept override {
    record_ = record;
    return true;
  }
  bool OnBbo(const wire::BboRecord &record) noexcept override {
    record_ = record;
    return true;
  }
  bool OnTicker(const wire::TickerRecord &record) noexcept override {
    record_ = record;
    return true;
  }
  bool OnDelta(const wire::DeltaRecord &record) noexcept override {
    record_ = record;
    return true;
  }
  bool OnSnapshotBegin(
      const wire::SnapshotBeginRecord &record) noexcept override {
    record_ = SnapshotBeginDecoded{record};
    return true;
  }
  bool OnSnapshotChunk(
      const wire::SnapshotChunkRecord &record) noexcept override {
    record_ = record;
    return true;
  }
  bool OnSnapshotEnd(
      const wire::SnapshotEndRecord &record) noexcept override {
    record_ = SnapshotEndDecoded{record};
    return true;
  }

  DecodedRecord record_;
};

struct SnapshotView {
  std::uint32_t instrument_id{};
  std::uint32_t generation{};
  std::uint32_t expected_levels{};
  std::uint32_t expected_chunks{};
  std::array<std::uint32_t, 2> next_chunk{};
  std::uint32_t received_chunks{};
  std::uint32_t received_levels{};
  md::BookState state{md::BookState::Empty};
  bool active{};
  std::map<std::int64_t, std::int64_t, std::greater<>> bids;
  std::map<std::int64_t, std::int64_t> asks;
  std::map<std::int64_t, std::int64_t, std::greater<>> staging_bids;
  std::map<std::int64_t, std::int64_t> staging_asks;

  void discard(md::BookState next_state = md::BookState::Invalid) {
    expected_levels = 0;
    expected_chunks = 0;
    next_chunk = {};
    received_chunks = 0;
    received_levels = 0;
    state = next_state;
    active = false;
    bids.clear();
    asks.clear();
    staging_bids.clear();
    staging_asks.clear();
  }
};

struct Segment {
  std::string name;
  transport::SharedRing ring;
  transport::ReaderHandle reader;
  std::uint64_t epoch{};
  SnapshotView snapshot;
  std::unordered_map<std::uint32_t, md::Instrument> instruments;
};

const md::Instrument *instrument_for(const Segment &segment,
                                     std::uint32_t instrument_id) {
  const auto found = segment.instruments.find(instrument_id);
  return found == segment.instruments.end() ? nullptr : &found->second;
}

void print_identity(const Segment &segment, std::uint32_t instrument_id) {
  const auto *instrument = instrument_for(segment, instrument_id);
  if (instrument == nullptr) {
    std::cout << " instrument=" << instrument_id
              << " symbol=? profile=?";
    return;
  }
  auto symbol = fixed_text(instrument->canonical_symbol);
  if (symbol.empty()) {
    symbol = fixed_text(instrument->venue_symbol);
  }
  std::cout << " instrument=" << instrument_id << " symbol=" << symbol
            << " profile=" << profile_text(*instrument);
}

void print_header(const Segment &segment, const transport::RecordView &outer,
                  const wire::RecordHeader &header) {
  std::cout << "segment=" << segment.name << " epoch=" << outer.epoch
            << " ring_seq=" << outer.sequence
            << " type=" << type_name(header.message_type);
  print_identity(segment, header.instrument_id);
  std::cout << " generation=" << header.book_generation
            << " state=" << state_name(header.state)
            << " bus_seq=" << header.bus_seq
            << " source_seq=" << header.source_seq
            << " exchange_ts_ns=" << header.exchange_ts_ns
            << " receive_tsc=" << header.receive_tsc
            << " publish_tsc=" << header.publish_tsc;
}

std::string scaled_or_raw(const Segment &segment, std::uint32_t instrument_id,
                          std::int64_t value, bool price) {
  const auto *instrument = instrument_for(segment, instrument_id);
  if (instrument == nullptr) {
    return std::string("raw:") + std::to_string(value);
  }
  return decimal_text(
      value, price ? instrument->price_scale : instrument->quantity_scale);
}

std::size_t side_slot(std::uint8_t side) noexcept {
  return side == static_cast<std::uint8_t>(md::Side::Bid) ? 0U : 1U;
}

void print_local_bbo(const Segment &segment, std::uint32_t instrument_id) {
  const auto &snapshot = segment.snapshot;
  std::cout << " local_bbo=";
  if (snapshot.bids.empty()) {
    std::cout << "none";
  } else {
    const auto &[price, quantity] = *snapshot.bids.begin();
    std::cout << scaled_or_raw(segment, instrument_id, price, true) << '@'
              << scaled_or_raw(segment, instrument_id, quantity, false);
  }
  std::cout << '/';
  if (snapshot.asks.empty()) {
    std::cout << "none";
  } else {
    const auto &[price, quantity] = *snapshot.asks.begin();
    std::cout << scaled_or_raw(segment, instrument_id, price, true) << '@'
              << scaled_or_raw(segment, instrument_id, quantity, false);
  }
}

void report_snapshot_error(Segment &segment,
                           const transport::RecordView &outer,
                           const wire::RecordHeader &header,
                           std::string_view reason, const Options &options) {
  segment.snapshot.discard();
  if (!options.bbo_only) {
    print_header(segment, outer, header);
    std::cout << " view_state=Invalid reason=" << reason << '\n';
  }
}

void process_record(Segment &segment, const transport::RecordView &outer,
                    const DecodedRecord &decoded, const Options &options) {
  std::visit(
      [&](const auto &record) {
        using Record = std::decay_t<decltype(record)>;
        if constexpr (std::is_same_v<Record, std::monostate>) {
          return;
        } else if constexpr (std::is_same_v<
                                 Record, wire::InstrumentUpdateRecord>) {
          segment.instruments[record.header.instrument_id] = record.instrument;
          if (!options.bbo_only) {
            print_header(segment, outer, record.header);
            std::cout << " price_scale="
                      << static_cast<unsigned>(record.instrument.price_scale)
                      << " quantity_scale="
                      << static_cast<unsigned>(record.instrument.quantity_scale)
                      << " tick="
                      << decimal_text(record.instrument.tick_size,
                                      record.instrument.price_scale)
                      << " lot="
                      << decimal_text(record.instrument.lot_size,
                                      record.instrument.quantity_scale)
                      << '\n';
          }
        } else if constexpr (std::is_same_v<Record, wire::BboRecord>) {
          print_header(segment, outer, record.header);
          std::cout << " bid="
                    << scaled_or_raw(segment, record.header.instrument_id,
                                     record.bid_price, true)
                    << '@'
                    << scaled_or_raw(segment, record.header.instrument_id,
                                     record.bid_quantity, false)
                    << " ask="
                    << scaled_or_raw(segment, record.header.instrument_id,
                                     record.ask_price, true)
                    << '@'
                    << scaled_or_raw(segment, record.header.instrument_id,
                                     record.ask_quantity, false)
                    << " view_state=" << state_name(
                           static_cast<std::uint8_t>(segment.snapshot.state))
                    << '\n';
        } else if constexpr (std::is_same_v<Record, wire::TickerRecord>) {
          print_header(segment, outer, record.header);
          std::cout << " open="
                    << scaled_or_raw(segment, record.header.instrument_id,
                                     record.open_price, true)
                    << " high="
                    << scaled_or_raw(segment, record.header.instrument_id,
                                     record.high_price, true)
                    << " low="
                    << scaled_or_raw(segment, record.header.instrument_id,
                                     record.low_price, true)
                    << " close="
                    << scaled_or_raw(segment, record.header.instrument_id,
                                     record.close_price, true)
                    << '\n';
        } else if constexpr (std::is_same_v<Record, wire::DeltaRecord>) {
          if (record.header.book_generation < segment.snapshot.generation) {
            if (options.raw && !options.bbo_only) {
              print_header(segment, outer, record.header);
              std::cout << " ignored=stale-generation\n";
            }
            return;
          }
          if (record.header.book_generation > segment.snapshot.generation ||
              segment.snapshot.state != md::BookState::Live) {
            segment.snapshot.generation = record.header.book_generation;
            segment.snapshot.instrument_id = record.header.instrument_id;
            segment.snapshot.discard(md::BookState::NeedsRestart);
            return;
          }
          if (record.header.state !=
              static_cast<std::uint8_t>(md::BookState::Live)) {
            segment.snapshot.discard(
                static_cast<md::BookState>(record.header.state));
            return;
          }
          if (record.side == static_cast<std::uint8_t>(md::Side::Bid)) {
            if (record.quantity == 0) {
              segment.snapshot.bids.erase(record.price);
            } else {
              segment.snapshot.bids[record.price] = record.quantity;
            }
          } else {
            if (record.quantity == 0) {
              segment.snapshot.asks.erase(record.price);
            } else {
              segment.snapshot.asks[record.price] = record.quantity;
            }
          }
          if (options.raw && !options.bbo_only) {
            print_header(segment, outer, record.header);
            std::cout << " side="
                      << (record.side == static_cast<std::uint8_t>(md::Side::Bid)
                              ? "bid"
                              : "ask")
                      << " price="
                      << scaled_or_raw(segment, record.header.instrument_id,
                                       record.price, true)
                      << " quantity="
                      << scaled_or_raw(segment, record.header.instrument_id,
                                       record.quantity, false)
                      << " view_state=" << state_name(static_cast<std::uint8_t>(
                             segment.snapshot.state));
            print_local_bbo(segment, record.header.instrument_id);
            std::cout
                      << '\n';
          }
        } else if constexpr (std::is_same_v<Record, SnapshotBeginDecoded>) {
          const auto &begin = record.value;
          if (begin.header.book_generation < segment.snapshot.generation) {
            if (options.raw && !options.bbo_only) {
              print_header(segment, outer, begin.header);
              std::cout << " ignored=stale-generation\n";
            }
            return;
          }
          auto &snapshot = segment.snapshot;
          snapshot.discard(md::BookState::Building);
          snapshot.instrument_id = begin.header.instrument_id;
          snapshot.generation = begin.header.book_generation;
          snapshot.expected_levels = begin.item_count;
          snapshot.expected_chunks = begin.chunk_count_or_checksum;
          snapshot.active = true;
          if (!options.bbo_only) {
            print_header(segment, outer, begin.header);
            std::cout << " view_state=Building levels=" << begin.item_count
                      << " chunks=" << begin.chunk_count_or_checksum << '\n';
          }
        } else if constexpr (std::is_same_v<
                                 Record, wire::SnapshotChunkRecord>) {
          auto &snapshot = segment.snapshot;
          const auto slot = side_slot(record.side);
          if (!snapshot.active ||
              snapshot.instrument_id != record.header.instrument_id ||
              snapshot.generation != record.header.book_generation ||
              record.chunk_index != snapshot.next_chunk[slot] ||
              snapshot.received_chunks >= snapshot.expected_chunks ||
              snapshot.received_levels + record.level_count >
                  snapshot.expected_levels) {
            report_snapshot_error(segment, outer, record.header,
                                  "non-contiguous-chunk", options);
            return;
          }
          for (std::size_t index = 0; index < record.level_count; ++index) {
            const auto &level = record.levels[index];
            if (record.side == static_cast<std::uint8_t>(md::Side::Bid)) {
              snapshot.staging_bids[level.price] = level.quantity;
            } else {
              snapshot.staging_asks[level.price] = level.quantity;
            }
          }
          ++snapshot.next_chunk[slot];
          ++snapshot.received_chunks;
          snapshot.received_levels += record.level_count;
          if (options.raw && !options.bbo_only) {
            print_header(segment, outer, record.header);
            std::cout << " view_state=Building chunk=" << record.chunk_index
                      << " chunk_levels=" << record.level_count
                      << " received=" << snapshot.received_levels << '\n';
          }
        } else if constexpr (std::is_same_v<Record, SnapshotEndDecoded>) {
          const auto &end = record.value;
          auto &snapshot = segment.snapshot;
          const bool complete =
              snapshot.active &&
              snapshot.instrument_id == end.header.instrument_id &&
              snapshot.generation == end.header.book_generation &&
              snapshot.received_chunks == snapshot.expected_chunks &&
              snapshot.received_levels == snapshot.expected_levels &&
              end.item_count == snapshot.received_levels;
          if (!complete) {
            report_snapshot_error(segment, outer, end.header,
                                  "incomplete-snapshot", options);
            return;
          }
          snapshot.active = false;
          snapshot.state = static_cast<md::BookState>(end.header.state);
          if (snapshot.state == md::BookState::Live) {
            snapshot.bids = std::move(snapshot.staging_bids);
            snapshot.asks = std::move(snapshot.staging_asks);
          } else {
            snapshot.bids.clear();
            snapshot.asks.clear();
            snapshot.staging_bids.clear();
            snapshot.staging_asks.clear();
          }
          if (!options.bbo_only) {
            print_header(segment, outer, end.header);
            std::cout << " view_state="
                      << state_name(static_cast<std::uint8_t>(snapshot.state))
                      << " levels=" << snapshot.received_levels
                      << " checksum=" << end.chunk_count_or_checksum;
            print_local_bbo(segment, end.header.instrument_id);
            std::size_t printed = 0;
            for (const auto &[price, quantity] : snapshot.bids) {
              if (printed++ >= options.book_depth) {
                break;
              }
              std::cout << " bid[" << (printed - 1) << "]="
                        << scaled_or_raw(segment, end.header.instrument_id,
                                         price, true)
                        << '@'
                        << scaled_or_raw(segment, end.header.instrument_id,
                                         quantity, false);
            }
            printed = 0;
            for (const auto &[price, quantity] : snapshot.asks) {
              if (printed++ >= options.book_depth) {
                break;
              }
              std::cout << " ask[" << (printed - 1) << "]="
                        << scaled_or_raw(segment, end.header.instrument_id,
                                         price, true)
                        << '@'
                        << scaled_or_raw(segment, end.header.instrument_id,
                                         quantity, false);
            }
            std::cout << '\n';
          }
        }
      },
      decoded);
}

bool parse_options(int argc, char **argv, Options &options) {
  for (int index = 1; index < argc; ++index) {
    const std::string_view argument(argv[index]);
    if (argument == "--raw") {
      options.raw = true;
    } else if (argument == "--bbo-only") {
      options.bbo_only = true;
    } else if (argument == "--book-depth") {
      if (++index == argc) {
        return false;
      }
      const std::string_view value(argv[index]);
      std::uint64_t parsed{};
      const auto result =
          std::from_chars(value.data(), value.data() + value.size(), parsed);
      if (result.ec != std::errc{} || result.ptr != value.data() + value.size() ||
          parsed > std::numeric_limits<std::size_t>::max()) {
        return false;
      }
      options.book_depth = static_cast<std::size_t>(parsed);
    } else if (!argument.empty() && argument.front() == '-') {
      return false;
    } else {
      options.segments.emplace_back(argument);
    }
  }
  return !options.segments.empty();
}

bool register_reader(Segment &segment, std::uint64_t marker) {
  auto registered =
      segment.ring.register_reader(marker, utils::runtime::Timestamp::NowMono());
  if (!registered) {
    std::cerr << segment.name << ": " << registered.message << '\n';
    return false;
  }
  segment.reader = registered.value;
  segment.epoch = segment.ring.epoch();
  return true;
}

bool recover_reader(Segment &segment, std::uint64_t marker,
                    bool epoch_changed) {
  (void)segment.ring.unregister_reader(segment.reader);
  segment.snapshot.discard(md::BookState::NeedsRestart);
  segment.instruments.clear();
  if (!register_reader(segment, marker)) {
    return false;
  }
  std::cerr << segment.name << ": discarded "
            << (epoch_changed ? "epoch-stale" : "overrun/invalid")
            << " view and resumed at epoch " << segment.epoch << '\n';
  return true;
}

}  // namespace

int main(int argc, char **argv) {
  std::signal(SIGINT, request_stop);
  std::signal(SIGTERM, request_stop);
  Options command_line;
  if (!parse_options(argc, argv, command_line)) {
    std::cerr << "usage: mds_shm_consumer [--raw] [--bbo-only] "
                 "[--book-depth N] <segment-name> [segment-name ...]\n";
    return 2;
  }

  const auto marker =
      transport::process_start_marker(static_cast<std::uint32_t>(getpid()));
  std::vector<Segment> segments;
  segments.reserve(command_line.segments.size());
  for (const auto &name : command_line.segments) {
    transport::RingOptions options;
    options.name = name;
    options.create = false;
    auto opened = transport::SharedRing::open(options);
    if (!opened) {
      std::cerr << name << ": " << opened.message << '\n';
      return 1;
    }
    Segment segment;
    segment.name = name;
    segment.ring = std::move(opened.value);
    if (!register_reader(segment, marker)) {
      return 1;
    }
    segments.push_back(std::move(segment));
  }

  while (!stop_requested.load(std::memory_order_relaxed)) {
    bool consumed = false;
    const auto now = utils::runtime::Timestamp::NowMono();
    for (auto &segment : segments) {
      const auto current_epoch = segment.ring.epoch();
      if (current_epoch != segment.epoch) {
        if (!recover_reader(segment, marker, true)) {
          return 1;
        }
        continue;
      }
      const auto heartbeat = segment.ring.heartbeat(segment.reader, now);
      if (!heartbeat) {
        if (!recover_reader(segment, marker, false)) {
          return 1;
        }
        continue;
      }
      auto record = segment.ring.read(segment.reader);
      if (!record) {
        if (record.error == mds::api::ErrorCode::SubscriptionRejected ||
            record.error == mds::api::ErrorCode::RecordOverwritten) {
          if (!segment.ring.resync_to_latest(segment.reader)) {
            return 1;
          }
        } else if (record.error != mds::api::ErrorCode::QuotaExceeded) {
          std::cerr << segment.name << ": read failed: " << record.message
                    << '\n';
          return 1;
        }
        continue;
      }

      consumed = true;
      const auto &view = record.value.view();
      transport::RecordView stable_view{view.type, view.sequence, view.epoch,
                                        {}};
      wire::RecordHeader header{};
      const auto header_result = wire::ValidateHeader(view.payload, &header);
      const bool type_matches =
          header_result == wire::CodecError::Ok &&
          view.type == header.message_type;
      Decoder decoder;
      const auto decode_result =
          type_matches ? wire::Decode(view.payload, decoder)
                       : wire::CodecError::UnknownMessageType;
      const auto committed = record.value.commit();
      if (!committed) {
        if (committed.error == mds::api::ErrorCode::RecordOverwritten) {
          if (!segment.ring.resync_to_latest(segment.reader)) {
            return 1;
          }
          continue;
        }
        std::cerr << segment.name
                  << ": failed to commit shared-memory read: "
                  << committed.message << '\n';
        return 1;
      }
      if (header_result != wire::CodecError::Ok) {
        std::cerr << segment.name << ": rejected ring_seq="
                  << stable_view.sequence
                  << " wire=" << codec_error_name(header_result) << '\n';
      } else if (!type_matches) {
        std::cerr << segment.name << ": rejected ring_seq="
                  << stable_view.sequence << " outer-type=" << stable_view.type
                  << " wire-type=" << header.message_type << '\n';
      } else if (decode_result != wire::CodecError::Ok) {
        std::cerr << segment.name << ": rejected ring_seq="
                  << stable_view.sequence
                  << " wire=" << codec_error_name(decode_result) << '\n';
      } else {
        process_record(segment, stable_view, decoder.record_, command_line);
      }
    }
    if (!consumed) {
      std::this_thread::sleep_for(std::chrono::milliseconds(1));
    }
  }
  for (auto &segment : segments) {
    (void)segment.ring.unregister_reader(segment.reader);
  }
  return 0;
}
