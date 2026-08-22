#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <type_traits>

namespace utils::md {

using InstrumentId = std::uint64_t;

enum class Venue : std::uint16_t {
  Unknown = 0,
  Binance = 1,
  Okx = 2,
  Bybit = 3,
  Gate = 4,
  Bitget = 5,
  Polymarket = 6,
  Sse = 7,
  Hyperliquid = 8
};
enum class ProductType : std::uint8_t { Unknown = 0, Spot = 1, Perpetual = 2, Future = 3, BinaryOption = 4, Equity = 5 };
enum class Side : std::uint8_t { Bid = 1, Ask = 2 };
enum class BookState : std::uint8_t { Empty = 0, Building = 1, Live = 2, Invalid = 3, NeedsRestart = 4 };
enum class MessageType : std::uint16_t {
  Bbo = 1, Ticker = 2, BookDelta = 3, SnapshotBegin = 4,
  SnapshotChunk = 5, SnapshotEnd = 6, InstrumentUpdate = 7,
  AggBbo = 8, AggOrderBook = 9, InstrumentCatalog = 10
};

constexpr std::uint8_t kInstrumentRefineBookTick = 1U << 0U;

struct Fixed {
  std::int64_t mantissa{};
  std::int8_t scale{};
  std::array<std::uint8_t, 7> reserved{};
};
using Price = Fixed;
using Quantity = Fixed;

struct Instrument {
  InstrumentId instrument_id{};
  Venue venue{};
  ProductType product_type{};
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
  std::uint8_t contract_multiplier_scale{};
  std::uint8_t flags{};
  std::uint8_t reserved0{};
  std::int64_t tick_size{};
  std::int64_t lot_size{};
  std::int64_t contract_multiplier{};
  std::uint32_t expiry_yyyymmdd{};
  std::array<char, 16> base_asset{};
  std::array<char, 16> quote_asset{};
  std::array<char, 16> settle_asset{};
  std::array<char, 32> canonical_symbol{};
  std::array<char, 32> venue_symbol{};
  std::array<char, 64> instrument_key{};
};

struct EventHeader {
  InstrumentId instrument_id{};
  std::uint32_t book_generation{};
  std::uint64_t source_seq{};
  std::uint64_t bus_seq{};
  std::uint64_t exchange_ts_ns{};
  std::uint64_t receive_tsc{};
  std::uint64_t publish_tsc{};
  BookState state{};
  std::uint8_t source_id{};
  std::uint16_t validity{};
  std::array<std::uint8_t, 4> reserved{};
};

struct Level { std::int64_t price{}; std::int64_t quantity{}; };
struct BboEvent { EventHeader header{}; Level bid{}; Level ask{}; };
struct TickerEvent {
  EventHeader header{};
  Level bid{};
  Level ask{};
  std::int64_t last_price{};
  std::int64_t last_quantity{};
  std::int64_t mark_price{};
  std::int64_t index_price{};
  std::int64_t funding_rate{};
  std::int64_t open_price{};
  std::int64_t high_price{};
  std::int64_t low_price{};
  std::int64_t close_price{};
};
struct BookDelta { EventHeader header{}; Side side{}; std::array<std::uint8_t, 7> reserved{}; Level level{}; };
struct BookSnapshotBegin { EventHeader header{}; std::uint32_t level_count{}; std::uint32_t chunk_count{}; };
inline constexpr std::size_t kSnapshotLevelsPerChunk = 24;
struct BookSnapshotChunk {
  EventHeader header{};
  std::uint32_t chunk_index{};
  std::uint16_t level_count{};
  Side side{};
  std::uint8_t reserved{};
  std::array<Level, kSnapshotLevelsPerChunk> levels{};
};
struct BookSnapshotEnd { EventHeader header{}; std::uint32_t received_levels{}; std::uint32_t checksum{}; };
struct InstrumentUpdate { EventHeader header{}; Instrument instrument{}; };

struct InstrumentCatalog {
  InstrumentId instrument_id{};
  Venue venue{};
  ProductType product_type{};
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
  std::uint8_t contract_multiplier_scale{};
  std::uint8_t flags{};
  std::uint8_t signature_type{};
  std::uint8_t negative_risk{};
  std::array<std::uint8_t, 5> reserved{};
  std::int64_t tick_size{};
  std::int64_t lot_size{};
  std::int64_t contract_multiplier{};
  std::uint64_t expiry_unix_ns{};
  std::array<char, 16> base_asset{};
  std::array<char, 16> quote_asset{};
  std::array<char, 16> settle_asset{};
  std::array<char, 64> canonical_symbol{};
  std::array<char, 64> market_slug{};
  std::array<char, 80> venue_symbol{};
  std::array<char, 72> condition_id{};
  std::array<char, 16> outcome{};
};

static_assert(sizeof(Fixed) == 16);
static_assert(sizeof(EventHeader) == 64);
static_assert(static_cast<std::uint16_t>(MessageType::Bbo) == 1);
static_assert(static_cast<std::uint16_t>(MessageType::Ticker) == 2);
static_assert(static_cast<std::uint16_t>(MessageType::BookDelta) == 3);
static_assert(static_cast<std::uint16_t>(MessageType::SnapshotBegin) == 4);
static_assert(static_cast<std::uint16_t>(MessageType::SnapshotChunk) == 5);
static_assert(static_cast<std::uint16_t>(MessageType::SnapshotEnd) == 6);
static_assert(static_cast<std::uint16_t>(MessageType::InstrumentUpdate) == 7);
static_assert(static_cast<std::uint16_t>(MessageType::InstrumentCatalog) == 10);
static_assert(std::is_trivially_copyable_v<Instrument>);
static_assert(std::is_trivially_copyable_v<BboEvent>);
static_assert(std::is_trivially_copyable_v<TickerEvent>);
static_assert(std::is_trivially_copyable_v<BookDelta>);
static_assert(std::is_trivially_copyable_v<BookSnapshotBegin>);
static_assert(std::is_trivially_copyable_v<BookSnapshotChunk>);
static_assert(std::is_trivially_copyable_v<BookSnapshotEnd>);
static_assert(std::is_trivially_copyable_v<InstrumentUpdate>);
static_assert(std::is_trivially_copyable_v<InstrumentCatalog>);
static_assert(std::is_standard_layout_v<BookSnapshotChunk>);

}  // namespace utils::md
