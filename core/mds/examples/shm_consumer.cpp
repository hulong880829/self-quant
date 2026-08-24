#include "mds/consume/aggregate_dispatch.h"
#include "mds/gateway/gateway.h"
#include "mds/record/clickhouse_bbo_recorder.h"
#if defined(MDS_HAS_ZSTD)
#include "mds/record/recorder.h"
#endif
#include "mds/transport/shared_ring.h"
#include "aggregate_dashboard.h"
#include "consumer_config.h"
#include "consumer_sequence.h"
#include "utils/md/wire_codec.h"
#include "utils/runtime/timestamp.h"

#include <algorithm>
#include <array>
#include <atomic>
#include <chrono>
#include <charconv>
#include <csignal>
#include <cstdlib>
#include <cstdint>
#include <iostream>
#include <limits>
#include <map>
#include <memory>
#include <span>
#include <string>
#include <string_view>
#include <thread>
#include <type_traits>
#include <unordered_map>
#include <unordered_set>
#include <utility>
#include <vector>
#include <sys/ioctl.h>
#include <unistd.h>

namespace {

namespace md = utils::md;
namespace wire = utils::md::wire;
namespace transport = mds::transport;
namespace consume = mds::consume;

std::atomic<bool> stop_requested{false};

enum class IdleWait : std::uint8_t { Adaptive, Spin };

extern "C" void request_stop(int) noexcept {
  stop_requested.store(true, std::memory_order_relaxed);
}

struct Options {
  bool raw{};
  bool bbo_only{};
  std::size_t book_depth{};
  std::size_t live_book_depth{};
  std::size_t agg_depth{};
  bool live_agg_bbo{};
  bool refresh_explicit{};
  std::uint64_t refresh_ms{100};
  IdleWait idle_wait{IdleWait::Adaptive};
  std::uint64_t spin_count{1000};
  std::uint64_t idle_sleep_us{50};
  std::vector<std::string> segments;
  std::string config_path;
  bool gateway{};
  bool record{};
  bool clickhouse_bbo{};
  bool validate_only{};
};

inline void cpu_relax() noexcept {
#if defined(__x86_64__) || defined(__i386__)
  __asm__ __volatile__("pause");
#elif defined(__aarch64__) || defined(__arm__)
  __asm__ __volatile__("yield");
#else
  std::atomic_signal_fence(std::memory_order_seq_cst);
#endif
}

std::uint64_t wall_now_ns() noexcept {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::system_clock::now().time_since_epoch())
          .count());
}

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
    case md::MessageType::AggBbo:
      return "AggBbo";
    case md::MessageType::AggOrderBook:
      return "AggOrderBook";
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

template <typename Record>
const wire::RecordHeader &record_header(const Record &record) noexcept {
  return record.header;
}

const wire::RecordHeader &
record_header(const SnapshotBeginDecoded &record) noexcept {
  return record.value.header;
}

const wire::RecordHeader &
record_header(const SnapshotEndDecoded &record) noexcept {
  return record.value.header;
}

struct SnapshotView {
  utils::md::InstrumentId instrument_id{};
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
  mds::consumer::Selector selector;
  transport::SharedRing ring;
  transport::ReaderHandle reader;
  std::uint64_t epoch{};
  mds::examples::SequenceTracker sequences;
  std::unordered_map<md::InstrumentId, SnapshotView> snapshots;
  std::unordered_map<md::InstrumentId, md::Instrument> instruments;
  std::unordered_set<md::InstrumentId> selected_instruments;
  wire::AggBboRecord live_agg_bbo{};
  wire::AggOrderBookRecord live_agg_book{};
  std::uint64_t live_agg_bbo_ring_sequence{};
  std::uint64_t live_agg_book_ring_sequence{};
  bool have_live_agg_bbo{};
  bool have_live_agg_book{};
  consume::AggregateTopic aggregate_topic{consume::AggregateTopic::Unsupported};
  std::unique_ptr<consume::AggregateLatestState> latest_state;
  consume::AggregateWatchdog aggregate_watchdog;
  std::uint64_t last_validated_ring_sequence{};
  std::uint64_t last_heartbeat_ns{};
  std::uint64_t records_drained{};
  std::uint64_t drain_limit_hits{};
  std::uint64_t aggregate_catchups{};
  std::uint64_t aggregate_hard_resets{};
  std::uint64_t aggregate_stale_events{};
  std::uint64_t aggregate_window_gaps{};
};

SnapshotView &snapshot_for(Segment &segment,
                           md::InstrumentId instrument_id) {
  auto [found, inserted] =
      segment.snapshots.try_emplace(instrument_id);
  if (inserted) {
    found->second.instrument_id = instrument_id;
  }
  return found->second;
}

const SnapshotView *snapshot_for(const Segment &segment,
                                 md::InstrumentId instrument_id) {
  const auto found = segment.snapshots.find(instrument_id);
  return found == segment.snapshots.end() ? nullptr : &found->second;
}

bool selected(const Segment &segment, md::InstrumentId instrument_id) {
  return segment.selector.kind == mds::consumer::SelectorKind::All ||
         segment.selected_instruments.contains(instrument_id);
}

const md::Instrument *instrument_for(const Segment &segment,
                                     utils::md::InstrumentId instrument_id) {
  const auto found = segment.instruments.find(instrument_id);
  return found == segment.instruments.end() ? nullptr : &found->second;
}

void print_identity(const Segment &segment,
                    utils::md::InstrumentId instrument_id) {
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

std::string scaled_or_raw(const Segment &segment,
                          utils::md::InstrumentId instrument_id,
                          std::int64_t value, bool price) {
  const auto *instrument = instrument_for(segment, instrument_id);
  if (instrument == nullptr) {
    return std::string("raw:") + std::to_string(value);
  }
  return decimal_text(
      value, price ? instrument->price_scale : instrument->quantity_scale);
}

std::string_view venue_name(std::uint8_t venue) noexcept {
  switch (static_cast<md::Venue>(venue)) {
    case md::Venue::Binance:
      return "binance";
    case md::Venue::Okx:
      return "okx";
    case md::Venue::Bybit:
      return "bybit";
    case md::Venue::Gate:
      return "gate";
    case md::Venue::Bitget:
      return "bitget";
    case md::Venue::Polymarket:
      return "polymarket";
    case md::Venue::Sse:
      return "sse";
    case md::Venue::Hyperliquid:
      return "hyperliquid";
    case md::Venue::Unknown:
      return "unknown";
  }
  return "unknown";
}

void print_venue_slots(
    const std::array<std::uint8_t, wire::kAggVenueSlots> &venue_slot_ids,
    std::uint8_t member_count) {
  std::cout << '{';
  for (std::size_t slot = 0; slot < member_count; ++slot) {
    if (slot != 0) {
      std::cout << ',';
    }
    const auto venue = venue_slot_ids[slot];
    std::cout << slot << ':' << venue_name(venue) << '('
              << static_cast<unsigned>(venue) << ')';
  }
  std::cout << '}';
}

template <typename Quantities>
void print_venue_quantities(
    const std::array<std::uint8_t, wire::kAggVenueSlots> &venue_slot_ids,
    const Quantities &quantities, std::uint8_t quantity_scale) {
  std::cout << '{';
  bool first = true;
  for (std::size_t slot = 0; slot < wire::kAggVenueSlots; ++slot) {
    if (quantities[slot] == 0) {
      continue;
    }
    if (!first) {
      std::cout << ',';
    }
    first = false;
    std::cout << slot << ':' << venue_name(venue_slot_ids[slot]) << '='
              << decimal_text(quantities[slot], quantity_scale);
  }
  std::cout << '}';
}

void print_agg_raw_side(std::string_view label,
                        const wire::AggBboRawSide &side,
                        const std::array<std::uint8_t,
                                         wire::kAggVenueSlots>
                            &venue_slot_ids,
                        std::uint8_t price_scale,
                        std::uint8_t quantity_scale) {
  const auto venue =
      side.best_venue < venue_slot_ids.size()
          ? venue_slot_ids[side.best_venue]
          : static_cast<std::uint8_t>(md::Venue::Unknown);
  std::cout << ' ' << label << '=' << decimal_text(side.price, price_scale)
            << '@' << decimal_text(side.quantity, quantity_scale)
            << " mask=" << side.venue_mask
            << " best_slot=" << static_cast<unsigned>(side.best_venue)
            << ':' << venue_name(venue);
}

void print_agg_bbo_side(
    std::string_view label, const wire::AggBboSide &side,
    const std::array<std::uint8_t, wire::kAggVenueSlots> &venue_slot_ids,
    std::uint8_t price_scale, std::uint8_t quantity_scale) {
  const auto best_venue =
      side.best_venue < venue_slot_ids.size()
          ? venue_slot_ids[side.best_venue]
          : static_cast<std::uint8_t>(md::Venue::Unknown);
  const auto timestamp_venue =
      side.timestamp_venue < venue_slot_ids.size()
          ? venue_slot_ids[side.timestamp_venue]
          : static_cast<std::uint8_t>(md::Venue::Unknown);
  std::cout << ' ' << label << '=' << decimal_text(side.price, price_scale)
            << '@' << decimal_text(side.quantity, quantity_scale)
            << " venues=";
  print_venue_quantities(venue_slot_ids, side.venue_quantity, quantity_scale);
  std::cout << " mask=" << side.venue_mask
            << " contributors="
            << static_cast<unsigned>(side.contributor_count)
            << " best_slot=" << static_cast<unsigned>(side.best_venue)
            << ':' << venue_name(best_venue)
            << " exchange_ts_ns=" << side.exchange_ts_ns
            << " timestamp_slot="
            << static_cast<unsigned>(side.timestamp_venue) << ':'
            << venue_name(timestamp_venue)
            << " age_us=" << side.worst_ingress_age_us;
}

void print_agg_level(
    std::string_view side, std::size_t index, const wire::AggLevel &level,
    const std::array<std::uint8_t, wire::kAggVenueSlots> &venue_slot_ids,
    std::uint8_t price_scale, std::uint8_t quantity_scale) {
  std::cout << ' ' << side << '[' << index
            << "]=" << decimal_text(level.price, price_scale) << '@'
            << decimal_text(level.quantity, quantity_scale) << " venues=";
  print_venue_quantities(venue_slot_ids, level.venue_quantity, quantity_scale);
  std::cout << " mask=" << level.venue_mask
            << " contributors="
            << static_cast<unsigned>(level.contributor_count);
}

std::size_t side_slot(std::uint8_t side) noexcept {
  return side == static_cast<std::uint8_t>(md::Side::Bid) ? 0U : 1U;
}

void print_local_bbo(const Segment &segment,
                     utils::md::InstrumentId instrument_id) {
  const auto *view = snapshot_for(segment, instrument_id);
  std::cout << " local_bbo=";
  if (view == nullptr || view->bids.empty()) {
    std::cout << "none";
  } else {
    const auto &[price, quantity] = *view->bids.begin();
    std::cout << scaled_or_raw(segment, instrument_id, price, true) << '@'
              << scaled_or_raw(segment, instrument_id, quantity, false);
  }
  std::cout << '/';
  if (view == nullptr || view->asks.empty()) {
    std::cout << "none";
  } else {
    const auto &[price, quantity] = *view->asks.begin();
    std::cout << scaled_or_raw(segment, instrument_id, price, true) << '@'
              << scaled_or_raw(segment, instrument_id, quantity, false);
  }
}

void print_book_levels(const Segment &segment,
                       utils::md::InstrumentId instrument_id,
                       std::size_t depth) {
  if (depth == 0) {
    return;
  }
  const auto *snapshot = snapshot_for(segment, instrument_id);
  if (snapshot == nullptr) {
    return;
  }
  std::size_t printed = 0;
  for (const auto &[price, quantity] : snapshot->bids) {
    if (printed++ >= depth) {
      break;
    }
    std::cout << " bid[" << (printed - 1) << "]="
              << scaled_or_raw(segment, instrument_id, price, true) << '@'
              << scaled_or_raw(segment, instrument_id, quantity, false);
  }
  printed = 0;
  for (const auto &[price, quantity] : snapshot->asks) {
    if (printed++ >= depth) {
      break;
    }
    std::cout << " ask[" << (printed - 1) << "]="
              << scaled_or_raw(segment, instrument_id, price, true) << '@'
              << scaled_or_raw(segment, instrument_id, quantity, false);
  }
}

void report_snapshot_error(Segment &segment,
                           const transport::RecordView &outer,
                           const wire::RecordHeader &header,
                           std::string_view reason, const Options &options) {
  snapshot_for(segment, header.instrument_id).discard();
  if (!options.bbo_only) {
    print_header(segment, outer, header);
    std::cout << " view_state=Invalid reason=" << reason << '\n';
  }
}

template <typename Record>
void process_record(Segment &segment, const transport::RecordView &outer,
                    const Record &record, const Options &options) {
        if constexpr (std::is_same_v<
                          Record, wire::InstrumentCatalogRecord>) {
          if (segment.selector.matches(record.catalog)) {
            segment.selected_instruments.insert(record.header.instrument_id);
          } else {
            segment.selected_instruments.erase(record.header.instrument_id);
            segment.snapshots.erase(record.header.instrument_id);
            segment.instruments.erase(record.header.instrument_id);
          }
        } else if constexpr (std::is_same_v<
                          Record, wire::InstrumentUpdateRecord>) {
          if (segment.selector.matches(record.instrument)) {
            segment.selected_instruments.insert(record.header.instrument_id);
          } else {
            segment.selected_instruments.erase(record.header.instrument_id);
            segment.snapshots.erase(record.header.instrument_id);
            segment.instruments.erase(record.header.instrument_id);
            return;
          }
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
          const auto *snapshot =
              snapshot_for(std::as_const(segment),
                           record.header.instrument_id);
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
                    << " view_state="
                    << state_name(static_cast<std::uint8_t>(
                           snapshot == nullptr ? md::BookState::Empty
                                               : snapshot->state));
          if (options.live_book_depth > 0 && !options.bbo_only &&
              snapshot != nullptr && snapshot->state == md::BookState::Live) {
            print_book_levels(segment, record.header.instrument_id,
                              options.live_book_depth);
          }
          std::cout << '\n';
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
        } else if constexpr (std::is_same_v<Record, wire::AggBboRecord>) {
          if (options.live_agg_bbo) {
            segment.live_agg_bbo = record;
            segment.live_agg_bbo_ring_sequence = outer.sequence;
            segment.have_live_agg_bbo = true;
            return;
          }
          print_header(segment, outer, record.header);
          std::cout << " aggregate=" << fixed_text(record.base_asset) << '/'
                    << fixed_text(record.quote_asset)
                    << " flags=" << record.header.flags
                    << " skew_enforced="
                    << ((record.header.flags & wire::kAggSkewEnforced) != 0)
                    << " member_data_error="
                    << ((record.header.flags &
                         wire::kAggMemberDataError) != 0)
                    << " member_count="
                    << static_cast<unsigned>(record.member_count)
                    << " member_mask=" << record.member_mask
                    << " live_mask=" << record.live_mask
                    << " venue_slots=";
          print_venue_slots(record.venue_slot_ids, record.member_count);
          print_agg_bbo_side("gated_bid", record.gated_bid,
                             record.venue_slot_ids,
                             record.price_scale, record.quantity_scale);
          print_agg_bbo_side("gated_ask", record.gated_ask,
                             record.venue_slot_ids,
                             record.price_scale, record.quantity_scale);
          print_agg_raw_side("raw_bid", record.raw_bid,
                             record.venue_slot_ids, record.price_scale,
                             record.quantity_scale);
          print_agg_raw_side("raw_ask", record.raw_ask,
                             record.venue_slot_ids, record.price_scale,
                             record.quantity_scale);
          const auto cross_bid_venue =
              record.cross_bid_venue < record.venue_slot_ids.size()
                  ? record.venue_slot_ids[record.cross_bid_venue]
                  : static_cast<std::uint8_t>(md::Venue::Unknown);
          const auto cross_ask_venue =
              record.cross_ask_venue < record.venue_slot_ids.size()
                  ? record.venue_slot_ids[record.cross_ask_venue]
                  : static_cast<std::uint8_t>(md::Venue::Unknown);
          std::cout << " raw_cross_bps=" << record.raw_cross_bps
                    << " gated_cross_bps=" << record.gated_cross_bps
                    << " skew_us=" << record.skew_us
                    << " skew_threshold_us="
                    << record.cross_skew_threshold_us
                    << " cross_bid_slot="
                    << static_cast<unsigned>(record.cross_bid_venue) << ':'
                    << venue_name(cross_bid_venue)
                    << " cross_ask_slot="
                    << static_cast<unsigned>(record.cross_ask_venue) << ':'
                    << venue_name(cross_ask_venue)
                    << " fx_age_us=" << record.fx_age_us
                    << " fx_venue=" << venue_name(record.fx_venue) << '('
                    << static_cast<unsigned>(record.fx_venue) << ")\n";
        } else if constexpr (std::is_same_v<Record,
                                             wire::AggOrderBookRecord>) {
          if (options.live_agg_bbo) {
            if (!options.bbo_only) {
              segment.live_agg_book = record;
              segment.live_agg_book_ring_sequence = outer.sequence;
              segment.have_live_agg_book = true;
            }
            return;
          }
          if (options.bbo_only) {
            return;
          }
          print_header(segment, outer, record.header);
          std::cout << " aggregate=" << fixed_text(record.base_asset) << '/'
                    << fixed_text(record.quote_asset)
                    << " flags=" << record.header.flags
                    << " member_count="
                    << static_cast<unsigned>(record.member_count)
                    << " member_mask=" << record.member_mask
                    << " active_mask=" << record.active_mask
                    << " bids=" << record.bid_count
                    << " asks=" << record.ask_count
                    << " venue_slots=";
          print_venue_slots(record.venue_slot_ids, record.member_count);
          const auto bid_depth =
              std::min<std::size_t>(options.agg_depth, record.bid_count);
          const auto ask_depth =
              std::min<std::size_t>(options.agg_depth, record.ask_count);
          for (std::size_t index = 0; index < bid_depth; ++index) {
            print_agg_level("bid", index, record.bids[index],
                            record.venue_slot_ids, record.price_scale,
                            record.quantity_scale);
          }
          for (std::size_t index = 0; index < ask_depth; ++index) {
            print_agg_level("ask", index, record.asks[index],
                            record.venue_slot_ids, record.price_scale,
                            record.quantity_scale);
          }
          std::cout << '\n';
        } else if constexpr (std::is_same_v<Record, wire::DeltaRecord>) {
          auto &snapshot =
              snapshot_for(segment, record.header.instrument_id);
          if (record.header.book_generation < snapshot.generation) {
            if (options.raw && !options.bbo_only) {
              print_header(segment, outer, record.header);
              std::cout << " ignored=stale-generation\n";
            }
            return;
          }
          if (record.header.book_generation > snapshot.generation ||
              snapshot.state != md::BookState::Live) {
            snapshot.generation = record.header.book_generation;
            snapshot.discard(md::BookState::NeedsRestart);
            return;
          }
          if (record.header.state !=
              static_cast<std::uint8_t>(md::BookState::Live)) {
            snapshot.discard(
                static_cast<md::BookState>(record.header.state));
            return;
          }
          if (record.side == static_cast<std::uint8_t>(md::Side::Bid)) {
            if (record.quantity == 0) {
              snapshot.bids.erase(record.price);
            } else {
              snapshot.bids[record.price] = record.quantity;
            }
          } else {
            if (record.quantity == 0) {
              snapshot.asks.erase(record.price);
            } else {
              snapshot.asks[record.price] = record.quantity;
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
                             snapshot.state));
            print_local_bbo(segment, record.header.instrument_id);
            std::cout
                      << '\n';
          }
        } else if constexpr (std::is_same_v<Record, SnapshotBeginDecoded>) {
          const auto &begin = record.value;
          auto &snapshot =
              snapshot_for(segment, begin.header.instrument_id);
          if (begin.header.book_generation < snapshot.generation) {
            if (options.raw && !options.bbo_only) {
              print_header(segment, outer, begin.header);
              std::cout << " ignored=stale-generation\n";
            }
            return;
          }
          snapshot.discard(md::BookState::Building);
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
          auto &snapshot =
              snapshot_for(segment, record.header.instrument_id);
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
          auto &snapshot =
              snapshot_for(segment, end.header.instrument_id);
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
            print_book_levels(segment, end.header.instrument_id,
                              options.book_depth);
            std::cout << '\n';
          }
        }
}

bool parse_options(int argc, char **argv, Options &options) {
  for (int index = 1; index < argc; ++index) {
    const std::string_view argument(argv[index]);
    if (argument == "--gateway") {
      options.gateway = true;
    } else if (argument == "--record") {
      options.record = true;
    } else if (argument == "--clickhouse-bbo") {
      options.clickhouse_bbo = true;
    } else if (argument == "--validate-only") {
      options.validate_only = true;
    } else if (argument == "--config") {
      if (++index == argc || !options.config_path.empty()) {
        return false;
      }
      options.config_path = argv[index];
    } else if (argument == "--raw") {
      options.raw = true;
    } else if (argument == "--bbo-only") {
      options.bbo_only = true;
    } else if (argument == "--live-agg-bbo") {
      options.live_agg_bbo = true;
    } else if (argument == "--idle-wait") {
      if (++index == argc) {
        return false;
      }
      const std::string_view value(argv[index]);
      if (value == "adaptive") {
        options.idle_wait = IdleWait::Adaptive;
      } else if (value == "spin") {
        options.idle_wait = IdleWait::Spin;
      } else {
        return false;
      }
    } else if (argument == "--book-depth" ||
               argument == "--live-book-depth" ||
               argument == "--agg-depth" ||
               argument == "--refresh-ms" ||
               argument == "--spin-count" ||
               argument == "--idle-sleep-us") {
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
      if (argument == "--book-depth") {
        options.book_depth = static_cast<std::size_t>(parsed);
      } else if (argument == "--live-book-depth") {
        options.live_book_depth = static_cast<std::size_t>(parsed);
      } else if (argument == "--agg-depth") {
        options.agg_depth = static_cast<std::size_t>(parsed);
      } else if (argument == "--refresh-ms") {
        options.refresh_ms = parsed;
        options.refresh_explicit = true;
      } else if (argument == "--spin-count") {
        options.spin_count = parsed;
      } else {
        options.idle_sleep_us = parsed;
      }
    } else if (!argument.empty() && argument.front() == '-') {
      return false;
    } else {
      options.segments.emplace_back(argument);
    }
  }
  return (!options.segments.empty() || !options.config_path.empty()) &&
         (options.config_path.empty() || options.segments.empty()) &&
         (!options.validate_only || !options.config_path.empty()) &&
         ((!options.gateway && !options.record && !options.clickhouse_bbo) ||
          !options.config_path.empty()) &&
         !(options.clickhouse_bbo && (options.gateway || options.record)) &&
         (!options.live_agg_bbo || (!options.raw && options.refresh_ms != 0)) &&
         (!options.refresh_explicit || options.live_agg_bbo);
}

bool register_reader(Segment &segment, std::uint64_t marker) {
  const auto now = utils::runtime::Timestamp::NowMono();
  auto registered = segment.ring.register_reader(marker, now);
  if (!registered) {
    std::cerr << segment.name << ": " << registered.message << '\n';
    return false;
  }
  segment.reader = registered.value;
  segment.epoch = segment.ring.epoch();
  segment.last_heartbeat_ns = now;
  segment.sequences.reset();
  return true;
}

void clear_live_aggregate(Segment &segment) noexcept {
  segment.have_live_agg_bbo = false;
  segment.have_live_agg_book = false;
  segment.live_agg_bbo_ring_sequence = 0;
  segment.live_agg_book_ring_sequence = 0;
  if (segment.latest_state != nullptr) {
    segment.aggregate_watchdog.on_hard_reset();
    segment.latest_state->reset(
        segment.aggregate_topic,
        {.ring_epoch = segment.ring.epoch(),
         .ring_sequence = segment.last_validated_ring_sequence,
         .receive_mono_ns = utils::runtime::Timestamp::NowMono(),
         .receive_wall_ns = wall_now_ns()});
  }
  segment.last_validated_ring_sequence = 0;
}

bool catch_up_aggregate(Segment &segment, std::string_view reason) {
  if (segment.aggregate_topic == consume::AggregateTopic::Unsupported ||
      segment.latest_state == nullptr) {
    return false;
  }
  segment.sequences.reset();
  if (segment.aggregate_topic == consume::AggregateTopic::AggBbo) {
    segment.latest_state->interrupt_bbo_window();
    ++segment.aggregate_window_gaps;
  }
  const auto resynced = segment.ring.resync_to_latest(segment.reader);
  if (!resynced) {
    return false;
  }
  ++segment.aggregate_catchups;
  std::cerr << segment.name << ": aggregate catch-up reason=" << reason
            << " retained=last-good\n";
  return true;
}

bool recover_reader(Segment &segment, std::uint64_t marker,
                    bool epoch_changed) {
  (void)segment.ring.unregister_reader(segment.reader);
  segment.snapshots.clear();
  segment.instruments.clear();
  segment.selected_instruments.clear();
  clear_live_aggregate(segment);
  if (segment.latest_state != nullptr) {
    ++segment.aggregate_hard_resets;
  }
  if (!register_reader(segment, marker)) {
    return false;
  }
  std::cerr << segment.name << ": discarded "
            << (epoch_changed ? "epoch-stale" : "overrun/invalid")
            << " view and resumed at epoch " << segment.epoch << '\n';
  return true;
}

bool invalidate_and_resync(Segment &segment, std::string_view reason) {
  segment.snapshots.clear();
  segment.instruments.clear();
  segment.selected_instruments.clear();
  segment.sequences.reset();
  clear_live_aggregate(segment);
  if (segment.latest_state != nullptr) {
    ++segment.aggregate_hard_resets;
  }
  const auto resynced = segment.ring.resync_to_latest(segment.reader);
  if (!resynced) {
    return false;
  }
  std::cerr << segment.name << ": discarded invalid view reason=" << reason
            << " and resumed at latest\n";
  return true;
}

bool resync_after_overrun(Segment &segment, std::string_view reason) {
  if (segment.aggregate_topic != consume::AggregateTopic::Unsupported) {
    return catch_up_aggregate(segment, reason);
  }
  return invalidate_and_resync(segment, reason);
}

std::string_view sequence_error_name(
    mds::examples::SequenceError error) noexcept {
  switch (error) {
    case mds::examples::SequenceError::None:
      return {};
    case mds::examples::SequenceError::Ring:
      return "ring-sequence";
    case mds::examples::SequenceError::Bus:
      return "bus-sequence";
    case mds::examples::SequenceError::Source:
      return "source-sequence";
  }
  return "unknown-sequence";
}

std::size_t terminal_columns() noexcept {
  winsize size{};
  if (ioctl(STDOUT_FILENO, TIOCGWINSZ, &size) == 0 && size.ws_col != 0) {
    return size.ws_col;
  }
  return 120;
}

class TerminalScreen {
 public:
  explicit TerminalScreen(bool enabled) : enabled_(enabled) {
    if (enabled_) {
      std::cout << "\033[?1049h\033[?25l\033[H\033[J" << std::flush;
    }
  }

  ~TerminalScreen() {
    if (enabled_) {
      std::cout << "\033[0m\033[?25h\033[?1049l" << std::flush;
    }
  }

  TerminalScreen(const TerminalScreen &) = delete;
  TerminalScreen &operator=(const TerminalScreen &) = delete;

 private:
  bool enabled_{};
};

void render_live_aggregate(const std::vector<Segment> &segments,
                           const Options &options) {
  mds::examples::AggregateDashboardView view;
  view.depth = options.agg_depth;
  view.terminal_width = terminal_columns();
  view.color = true;
  view.expect_bbo = false;
  view.expect_book = false;
  for (const auto &segment : segments) {
    view.expect_bbo =
        view.expect_bbo ||
        segment.name.find(".aggbbo.") != std::string::npos;
    view.expect_book =
        view.expect_book ||
        segment.name.find(".aggorderbook.") != std::string::npos;
  }
  for (const auto &segment : segments) {
    if (view.bbo == nullptr && segment.have_live_agg_bbo) {
      view.bbo = &segment.live_agg_bbo;
      view.bbo_segment = segment.name;
      view.bbo_ring_sequence = segment.live_agg_bbo_ring_sequence;
    }
  }
  for (const auto &segment : segments) {
    if (view.book != nullptr || !segment.have_live_agg_book) {
      continue;
    }
    const bool identity_matches =
        view.bbo == nullptr ||
        (fixed_text(view.bbo->base_asset) ==
             fixed_text(segment.live_agg_book.base_asset) &&
         fixed_text(view.bbo->quote_asset) ==
             fixed_text(segment.live_agg_book.quote_asset));
    if (identity_matches) {
      view.book = &segment.live_agg_book;
      view.book_segment = segment.name;
      view.book_ring_sequence = segment.live_agg_book_ring_sequence;
    }
  }
  std::cout << "\033[H\033[J"
            << mds::examples::render_aggregate_dashboard(view) << std::flush;
}

}  // namespace

int main(int argc, char **argv) {
  std::signal(SIGINT, request_stop);
  std::signal(SIGTERM, request_stop);
  Options command_line;
  mds::consumer::ConsumerConfig consumer_config;
  if (!parse_options(argc, argv, command_line)) {
    std::cerr << "usage: mds_shm_consumer [--config PATH] [--validate-only] "
                 "[--gateway] [--record] [--clickhouse-bbo] [--raw] "
                 "[--bbo-only] "
                 "[--book-depth N] [--live-book-depth N] [--agg-depth N] "
                 "[--live-agg-bbo] [--refresh-ms N] "
                 "[--idle-wait adaptive|spin] [--spin-count N] "
                 "[--idle-sleep-us N] "
                 "<segment-name> [segment-name ...]\n";
    return 2;
  }
  if (!command_line.config_path.empty()) {
    auto loaded = mds::consumer::load_config(
        command_line.config_path, command_line.gateway, command_line.record,
        command_line.clickhouse_bbo);
    if (!loaded) {
      std::cerr << loaded.message << '\n';
      return 2;
    }
    consumer_config = std::move(loaded.value);
    command_line.segments.clear();
    if (command_line.clickhouse_bbo) {
      command_line.segments = consumer_config.clickhouse_bbo.segments;
    } else {
      command_line.segments.reserve(consumer_config.segments.size());
      for (const auto &segment : consumer_config.segments) {
        command_line.segments.push_back(segment.name);
      }
    }
    if (command_line.validate_only) {
      std::cout << "validated segments=" << command_line.segments.size()
                << " gateway=" << (command_line.gateway ? "on" : "off")
                << " recording=" << (command_line.record ? "on" : "off")
                << " clickhouse_bbo="
                << (command_line.clickhouse_bbo ? "on" : "off")
                << '\n';
      return 0;
    }
  }
#if !defined(MDS_HAS_ZSTD)
  if (command_line.record) {
    std::cerr << "recording is unavailable: install libzstd-dev and rebuild "
                 "with -DMDS_ENABLE_RECORDING=ON\n";
    return 2;
  }
#endif
  if ((command_line.gateway || command_line.record ||
       command_line.clickhouse_bbo) &&
      command_line.idle_wait == IdleWait::Spin) {
    std::cerr << "--idle-wait spin is not allowed with service modes "
                 "service modes\n";
    return 2;
  }
  if (command_line.live_agg_bbo && isatty(STDOUT_FILENO) == 0) {
    std::cerr << "--live-agg-bbo requires an interactive terminal\n";
    return 2;
  }

  const auto marker =
      transport::process_start_marker(static_cast<std::uint32_t>(getpid()));
  std::vector<Segment> segments;
  segments.reserve(command_line.segments.size());
  for (std::size_t index = 0; index < command_line.segments.size(); ++index) {
    const auto &name = command_line.segments[index];
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
    if (!command_line.config_path.empty() && !command_line.clickhouse_bbo) {
      segment.selector = consumer_config.segments[index].selector;
    }
    segment.aggregate_topic = consume::aggregate_topic_from_segment(name);
    if (segment.aggregate_topic != consume::AggregateTopic::Unsupported) {
      segment.aggregate_watchdog = consume::AggregateWatchdog(
          consumer_config.ingestion.stale_after_ms * 1'000'000ULL,
          consumer_config.ingestion.hard_reset_after_ms * 1'000'000ULL);
      const auto window_interval_ns =
          command_line.record
              ? consumer_config.recording.sample_interval_ms * 1'000'000ULL
              : 0;
      segment.latest_state = std::make_unique<consume::AggregateLatestState>(
          window_interval_ns);
    }
    segment.ring = std::move(opened.value);
    if (!register_reader(segment, marker)) {
      return 1;
    }
    segments.push_back(std::move(segment));
  }

  std::unique_ptr<mds::gateway::Gateway> gateway;
  if (command_line.gateway) {
    std::vector<mds::gateway::Topic> topics;
    topics.reserve(segments.size());
    for (auto &segment : segments) {
      if (segment.latest_state != nullptr) {
        topics.push_back({.segment = segment.name,
                          .kind = segment.aggregate_topic,
                          .latest = segment.latest_state.get()});
      }
    }
    const auto &configured = consumer_config.gateway;
    std::string bearer_token;
    if (!configured.auth_token_env.empty()) {
      const char *value = std::getenv(configured.auth_token_env.c_str());
      if (value == nullptr || *value == '\0') {
        std::cerr << "gateway auth token environment variable is missing: "
                  << configured.auth_token_env << '\n';
        for (auto &segment : segments) {
          (void)segment.ring.unregister_reader(segment.reader);
        }
        return 1;
      }
      bearer_token = value;
    }
    gateway = std::make_unique<mds::gateway::Gateway>(
        mds::gateway::GatewayOptions{
            .listen_address = configured.listen_address,
            .port = configured.port,
            .bearer_token = std::move(bearer_token),
            .max_clients = configured.max_clients,
            .max_subscriptions_per_client =
                configured.max_subscriptions_per_client,
            .publish_interval_ms = configured.publish_interval_ms,
            .ping_interval_ms = configured.ping_interval_ms,
            .pong_timeout_ms = configured.pong_timeout_ms,
            .slow_client_timeout_ms = configured.slow_client_timeout_ms,
            .depth = configured.depth,
            .max_control_frame_bytes = 4096,
            .reuse_port = configured.reuse_port},
        std::move(topics));
    if (!gateway->start()) {
      std::cerr << "gateway failed to start: " << gateway->error() << '\n';
      for (auto &segment : segments) {
        (void)segment.ring.unregister_reader(segment.reader);
      }
      return 1;
    }
    std::cerr << "gateway listening on ws://" << configured.listen_address
              << ':' << gateway->port() << "/v1/market-data\n";
  }

#if defined(MDS_HAS_ZSTD)
  std::unique_ptr<mds::record::Recorder> recorder;
  if (command_line.record) {
    std::vector<mds::record::Topic> topics;
    topics.reserve(segments.size());
    for (auto &segment : segments) {
      topics.push_back({.segment = segment.name,
                        .kind = segment.aggregate_topic,
                        .latest = segment.latest_state.get()});
    }
    const auto &recording = consumer_config.recording;
    recorder = std::make_unique<mds::record::Recorder>(
        mds::record::RecorderOptions{
            .output_directory = recording.output_directory,
            .sample_interval_ms = recording.sample_interval_ms,
            .retention_hours = recording.retention_hours,
            .depth = recording.depth,
            .zstd_level = recording.zstd_level,
            .min_free_disk_bytes = recording.min_free_disk_bytes},
        topics);
    if (!recorder->start()) {
      std::cerr << "recorder failed to start: " << recorder->error() << '\n';
      for (auto &segment : segments) {
        (void)segment.ring.unregister_reader(segment.reader);
      }
      return 1;
    }
  }
#endif

  std::unique_ptr<mds::record::ClickHouseBboRecorder> clickhouse_recorder;
  if (command_line.clickhouse_bbo) {
    auto configured = consumer_config.clickhouse_bbo.options;
    const auto &password_env = consumer_config.clickhouse_bbo.password_env;
    if (!password_env.empty()) {
      const char *value = std::getenv(password_env.c_str());
      if (value == nullptr) {
        std::cerr << "ClickHouse password environment variable is missing: "
                  << password_env << '\n';
        for (auto &segment : segments) {
          (void)segment.ring.unregister_reader(segment.reader);
        }
        return 1;
      }
      configured.password = value;
    }
    clickhouse_recorder =
        std::make_unique<mds::record::ClickHouseBboRecorder>(
            std::move(configured));
    if (!clickhouse_recorder->start()) {
      std::cerr << "ClickHouse BBO recorder failed to start: "
                << clickhouse_recorder->error() << '\n';
      for (auto &segment : segments) {
        (void)segment.ring.unregister_reader(segment.reader);
      }
      return 1;
    }
  }

  TerminalScreen terminal_screen(command_line.live_agg_bbo);
  std::uint64_t idle_rounds = 0;
  std::uint64_t poll_rounds = 0;
  std::uint64_t last_heartbeat_ns = utils::runtime::Timestamp::NowMono();
  bool recorder_failure_reported = false;
  bool gateway_failure_reported = false;
  bool service_failed = false;
  auto last_refresh = std::chrono::steady_clock::now() -
                      std::chrono::milliseconds(command_line.refresh_ms);
  while (!stop_requested.load(std::memory_order_relaxed)) {
#if defined(MDS_HAS_ZSTD)
    if (recorder != nullptr && recorder->failed() &&
        !recorder_failure_reported) {
      std::cerr << "recording disabled after failure: " << recorder->error()
                << "; shared-memory consumption continues\n";
      recorder_failure_reported = true;
      service_failed = true;
    }
#endif
    if (gateway != nullptr && gateway->failed() &&
        !gateway_failure_reported) {
      std::cerr << "gateway disabled after failure: " << gateway->error()
                << "; shared-memory consumption continues\n";
      gateway_failure_reported = true;
      service_failed = true;
    }
    bool consumed = false;
    ++poll_rounds;
    const bool check_heartbeat = (poll_rounds & 1023U) == 0;
    const auto now =
        check_heartbeat ? utils::runtime::Timestamp::NowMono() : 0;
    const bool heartbeat_due =
        check_heartbeat && now - last_heartbeat_ns >= 500'000'000ULL;
    for (auto &segment : segments) {
      const auto current_epoch = segment.ring.epoch();
      if (current_epoch != segment.epoch) {
        if (!recover_reader(segment, marker, true)) {
          return 1;
        }
        continue;
      }
      if (heartbeat_due) {
        const auto heartbeat = segment.ring.heartbeat(segment.reader, now);
        if (!heartbeat) {
          if (!recover_reader(segment, marker, false)) {
            return 1;
          }
          continue;
        }
        segment.last_heartbeat_ns = now;
      }
      if (check_heartbeat && segment.latest_state != nullptr) {
        const auto watchdog_action = segment.aggregate_watchdog.poll(now);
        if (watchdog_action == consume::AggregateWatchdogAction::Stale) {
          ++segment.aggregate_stale_events;
          std::cerr << segment.name << ": aggregate stream is stale\n";
        } else if (watchdog_action ==
                   consume::AggregateWatchdogAction::HardReset) {
          clear_live_aggregate(segment);
          ++segment.aggregate_hard_resets;
          std::cerr << segment.name
                    << ": aggregate stream stalled; published hard reset\n";
        }
        if (segment.aggregate_topic == consume::AggregateTopic::AggBbo) {
          segment.latest_state->service_bbo_window_rollover(
              {.ring_epoch = segment.epoch,
               .ring_sequence = segment.last_validated_ring_sequence,
               .receive_mono_ns = now,
               .receive_wall_ns = wall_now_ns()});
        }
      }
      std::size_t drained = 0;
      for (; drained < consumer_config.ingestion.max_drain_records; ++drained) {
        if (drained != 0 && (drained & 63U) == 0) {
          const auto drain_now = utils::runtime::Timestamp::NowMono();
          if (drain_now - segment.last_heartbeat_ns >= 500'000'000ULL) {
            const auto heartbeat =
                segment.ring.heartbeat(segment.reader, drain_now);
            if (!heartbeat) {
              if (!recover_reader(segment, marker, false)) {
                return 1;
              }
              break;
            }
            segment.last_heartbeat_ns = drain_now;
          }
        }
      transport::ReadLease lease;
      const auto read_error = segment.ring.try_read(segment.reader, lease);
      if (read_error != mds::api::ErrorCode::Ok) {
        if (read_error == mds::api::ErrorCode::SubscriptionRejected ||
            read_error == mds::api::ErrorCode::RecordOverwritten) {
          if (!resync_after_overrun(segment, "ring-read")) {
            return 1;
          }
        } else if (read_error == mds::api::ErrorCode::InternalError) {
          if (!invalidate_and_resync(segment, "ring-corrupt")) {
            return 1;
          }
        } else if (read_error == mds::api::ErrorCode::InvalidHandle) {
          if (!recover_reader(segment, marker, false)) {
            return 1;
          }
        } else if (read_error != mds::api::ErrorCode::QuotaExceeded) {
          std::cerr << segment.name << ": read failed error="
                    << static_cast<unsigned>(read_error) << '\n';
          return 1;
        }
        break;
      }

      consumed = true;
      const auto &view = lease.view();
      std::vector<std::byte> clickhouse_payload;
      const auto incoming_type = static_cast<md::MessageType>(view.type);
      if (clickhouse_recorder != nullptr &&
          (incoming_type == md::MessageType::InstrumentCatalog ||
           incoming_type == md::MessageType::Bbo ||
           incoming_type == md::MessageType::Ticker)) {
        clickhouse_payload.assign(view.payload.begin(), view.payload.end());
      }
      transport::RecordView stable_view{view.type, view.sequence, view.epoch,
                                        {}};
      bool segment_resynced = false;
      const auto finish = [&](const auto &record,
                              wire::CodecError decode_error) -> bool {
        const auto &header = record_header(record);
        std::string_view invalid_reason;
        if (decode_error == wire::CodecError::Ok) {
          invalid_reason = sequence_error_name(
              segment.sequences.check(stable_view.sequence, header));
        }
        const auto committed = lease.commit();
        if (!committed) {
          if (committed.error == mds::api::ErrorCode::InvalidHandle) {
            segment_resynced = true;
            return recover_reader(segment, marker, false);
          }
          if (committed.error == mds::api::ErrorCode::RecordOverwritten) {
            segment_resynced = true;
            return resync_after_overrun(segment, "ring-commit");
          }
          segment_resynced = true;
          return invalidate_and_resync(segment, "ring-commit");
        }
        if (decode_error != wire::CodecError::Ok) {
          std::cerr << segment.name << ": rejected ring_seq="
                    << stable_view.sequence
                    << " wire=" << codec_error_name(decode_error) << '\n';
          segment_resynced = true;
          return invalidate_and_resync(segment, "wire-decode");
        }
        if (!invalid_reason.empty()) {
          std::cerr << segment.name << ": rejected ring_seq="
                    << stable_view.sequence << " bus_seq="
                    << header.bus_seq << " reason=" << invalid_reason
                    << '\n';
          segment_resynced = true;
          return invalidate_and_resync(segment, invalid_reason);
        }
        segment.sequences.accept(stable_view.sequence, header);
        segment.last_validated_ring_sequence = stable_view.sequence;
        const consume::AggregateReceiveInfo receive{
            .ring_epoch = stable_view.epoch,
            .ring_sequence = stable_view.sequence,
            .receive_mono_ns = utils::runtime::Timestamp::NowMono(),
            .receive_wall_ns = wall_now_ns()};
        if (clickhouse_recorder != nullptr && !clickhouse_payload.empty()) {
          clickhouse_recorder->consume(clickhouse_payload,
                                       receive.receive_mono_ns);
        }
        using DecodedRecord = std::remove_cvref_t<decltype(record)>;
        if constexpr (std::is_same_v<DecodedRecord, wire::AggBboRecord>) {
          if (segment.latest_state != nullptr &&
              segment.aggregate_topic == consume::AggregateTopic::AggBbo) {
            segment.latest_state->publish(record, receive);
            segment.aggregate_watchdog.on_ready(receive.receive_mono_ns);
          }
        } else if constexpr (std::is_same_v<DecodedRecord,
                                            wire::AggOrderBookRecord>) {
          if (segment.latest_state != nullptr &&
              segment.aggregate_topic ==
                  consume::AggregateTopic::AggOrderBook) {
            segment.latest_state->publish(record, receive);
            segment.aggregate_watchdog.on_ready(receive.receive_mono_ns);
          }
        }
        const auto message_type =
            static_cast<md::MessageType>(header.message_type);
        const bool metadata =
            message_type == md::MessageType::InstrumentCatalog ||
            message_type == md::MessageType::InstrumentUpdate;
        if (!command_line.gateway && !command_line.record &&
            !command_line.clickhouse_bbo &&
            (metadata || selected(segment, header.instrument_id))) {
          process_record(segment, stable_view, record, command_line);
        }
        return true;
      };

      bool handled = false;
      switch (static_cast<md::MessageType>(view.type)) {
        case md::MessageType::InstrumentCatalog: {
          wire::InstrumentCatalogRecord record{};
          handled =
              finish(record, wire::DecodeInstrumentCatalog(view.payload,
                                                           record));
          break;
        }
        case md::MessageType::InstrumentUpdate: {
          wire::InstrumentUpdateRecord record{};
          handled = finish(record,
                           wire::DecodeInstrument(view.payload, record));
          break;
        }
        case md::MessageType::Bbo: {
          wire::BboRecord record{};
          handled = finish(record, wire::DecodeBbo(view.payload, record));
          break;
        }
        case md::MessageType::Ticker: {
          wire::TickerRecord record{};
          handled = finish(record, wire::DecodeTicker(view.payload, record));
          break;
        }
        case md::MessageType::BookDelta: {
          wire::DeltaRecord record{};
          handled = finish(record, wire::DecodeDelta(view.payload, record));
          break;
        }
        case md::MessageType::SnapshotBegin: {
          wire::SnapshotBeginRecord record{};
          const auto decoded = wire::DecodeSnapshotBegin(view.payload, record);
          handled = finish(SnapshotBeginDecoded{record}, decoded);
          break;
        }
        case md::MessageType::SnapshotChunk: {
          wire::SnapshotChunkRecord record{};
          handled =
              finish(record, wire::DecodeSnapshotChunk(view.payload, record));
          break;
        }
        case md::MessageType::SnapshotEnd: {
          wire::SnapshotEndRecord record{};
          const auto decoded = wire::DecodeSnapshotEnd(view.payload, record);
          handled = finish(SnapshotEndDecoded{record}, decoded);
          break;
        }
        case md::MessageType::AggBbo: {
          wire::AggBboRecord record{};
          handled = finish(record, wire::DecodeAggBbo(view.payload, record));
          break;
        }
        case md::MessageType::AggOrderBook: {
          wire::AggOrderBookRecord record{};
          handled =
              finish(record, wire::DecodeAggOrderBook(view.payload, record));
          break;
        }
        default: {
          wire::RecordHeader header{};
          const auto decoded = wire::ValidateHeader(view.payload, &header);
          const auto sequence_error =
              decoded == wire::CodecError::UnknownMessageType
                  ? segment.sequences.check_transport(stable_view.sequence,
                                                      header)
                  : mds::examples::SequenceError::None;
          const auto committed = lease.commit();
          if (!committed) {
            segment_resynced = true;
            if (committed.error == mds::api::ErrorCode::InvalidHandle) {
              handled = recover_reader(segment, marker, false);
            } else if (committed.error ==
                       mds::api::ErrorCode::RecordOverwritten) {
              handled = resync_after_overrun(segment, "ring-commit");
            } else {
              handled = invalidate_and_resync(segment, "ring-commit");
            }
            break;
          }
          if (decoded != wire::CodecError::UnknownMessageType ||
              view.type != header.message_type) {
            segment_resynced = true;
            handled = invalidate_and_resync(segment, "wire-decode");
            break;
          }
          if (sequence_error != mds::examples::SequenceError::None) {
            segment_resynced = true;
            handled = invalidate_and_resync(
                segment, sequence_error_name(sequence_error));
            break;
          }
          segment.sequences.accept_transport(stable_view.sequence, header);
          segment.last_validated_ring_sequence = stable_view.sequence;
          handled = true;
          break;
        }
      }
      if (!handled) {
        return 1;
      }
      ++segment.records_drained;
      if (segment_resynced) {
        break;
      }
      }
      if (drained == consumer_config.ingestion.max_drain_records) {
        ++segment.drain_limit_hits;
      }
    }
    if (heartbeat_due) {
      last_heartbeat_ns = now;
    }
    if (check_heartbeat && clickhouse_recorder != nullptr) {
      clickhouse_recorder->sample(wall_now_ns(), now);
    }
    if (command_line.live_agg_bbo) {
      const auto render_now = std::chrono::steady_clock::now();
      if (render_now - last_refresh >=
          std::chrono::milliseconds(command_line.refresh_ms)) {
        render_live_aggregate(segments, command_line);
        last_refresh = render_now;
      }
    }
    if (!consumed) {
      if (command_line.idle_wait == IdleWait::Spin ||
          idle_rounds < command_line.spin_count) {
        cpu_relax();
      } else if (idle_rounds == command_line.spin_count) {
        std::this_thread::yield();
      } else if (command_line.idle_sleep_us != 0) {
        std::this_thread::sleep_for(
            std::chrono::microseconds(command_line.idle_sleep_us));
      } else {
        cpu_relax();
      }
      if (idle_rounds != std::numeric_limits<std::uint64_t>::max()) {
        ++idle_rounds;
      }
    } else {
      idle_rounds = 0;
    }
  }
  if (gateway != nullptr) {
    gateway->stop();
    if (gateway->failed()) {
      service_failed = true;
    }
  }
  if (clickhouse_recorder != nullptr) {
    clickhouse_recorder->stop();
    const auto metrics = clickhouse_recorder->metrics();
    std::cerr << "ClickHouse BBO recorder stopped rows_enqueued="
              << metrics.rows_enqueued << " rows_written="
              << metrics.rows_written << " queue_drops="
              << metrics.queue_drops << " unresolved_drops="
              << metrics.unresolved_instrument_drops << " http_failures="
              << metrics.http_failures << " rows_requeued="
              << metrics.rows_requeued << " shutdown_drops="
              << metrics.shutdown_drops << " stale_skips="
              << metrics.stale_skips << '\n';
  }
  for (auto &segment : segments) {
    std::cerr << segment.name << ": consumption metrics records_drained="
              << segment.records_drained
              << " drain_limit_hits=" << segment.drain_limit_hits
              << " aggregate_catchups=" << segment.aggregate_catchups
              << " aggregate_stale=" << segment.aggregate_stale_events
              << " aggregate_hard_resets=" << segment.aggregate_hard_resets
              << " aggregate_window_gaps=" << segment.aggregate_window_gaps
              << '\n';
    (void)segment.ring.unregister_reader(segment.reader);
  }
#if defined(MDS_HAS_ZSTD)
  if (recorder != nullptr) {
    recorder->stop();
    if (recorder->failed()) {
      service_failed = true;
      if (!recorder_failure_reported) {
        std::cerr << "recording disabled after failure: " << recorder->error()
                  << "; shared-memory consumption completed normally\n";
      }
    }
  }
#endif
  return service_failed ? 1 : 0;
}
