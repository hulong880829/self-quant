#pragma once

#include <bit>
#include <cstddef>
#include <cstdint>
#include <type_traits>

#include "utils/md/types.h"

namespace utils::md::wire {

inline constexpr std::uint32_t kMagic = 0x444d5153U;  // Bytes: "SQMD".
inline constexpr std::uint16_t kSchemaMajor = 2;
inline constexpr std::uint16_t kSchemaMinor = 0;
inline constexpr std::size_t kAggVenueSlots = 8;
inline constexpr std::size_t kAggMaxLevelsPerSide = 80;
inline constexpr std::uint16_t kAggSkewEnforced = 1U << 0U;
inline constexpr std::uint16_t kAggMemberDataError = 1U << 1U;

// The wire ABI is intentionally rejected on big-endian targets. Multi-byte
// fields are stored in native little-endian representation.
static_assert(std::endian::native == std::endian::little,
              "utils market-data wire schema requires little-endian");

struct alignas(8) RecordHeader {
  std::uint32_t magic;            // 0x00
  std::uint16_t schema_major;     // 0x04
  std::uint16_t schema_minor;     // 0x06
  std::uint16_t message_type;     // 0x08
  std::uint16_t record_length;    // 0x0a
  std::uint32_t reserved;         // 0x0c
  InstrumentId instrument_id;     // 0x10
  std::uint64_t bus_seq;          // 0x18
  std::uint64_t source_seq;       // 0x20
  std::uint64_t exchange_ts_ns;   // 0x28
  std::uint64_t receive_tsc;      // 0x30
  std::uint64_t publish_tsc;      // 0x38
  std::uint32_t book_generation;  // 0x40
  std::uint8_t state;             // 0x44
  std::uint8_t source_id;         // 0x45
  std::uint16_t flags;            // 0x46
};

struct alignas(8) BboRecord {
  RecordHeader header;
  std::int64_t bid_price;
  std::int64_t bid_quantity;
  std::int64_t ask_price;
  std::int64_t ask_quantity;
};

struct alignas(8) TickerRecord {
  RecordHeader header;
  std::int64_t bid_price;
  std::int64_t bid_quantity;
  std::int64_t ask_price;
  std::int64_t ask_quantity;
  std::int64_t last_price;
  std::int64_t last_quantity;
  std::int64_t mark_price;
  std::int64_t index_price;
  std::int64_t funding_rate;
  std::int64_t open_price;
  std::int64_t high_price;
  std::int64_t low_price;
  std::int64_t close_price;
};

struct alignas(8) DeltaRecord {
  RecordHeader header;
  std::uint8_t side;
  std::uint8_t reserved[7];
  std::int64_t price;
  std::int64_t quantity;
};

struct alignas(8) SnapshotControlRecord {
  RecordHeader header;
  std::uint32_t item_count;
  std::uint32_t chunk_count_or_checksum;
};

using SnapshotBeginRecord = SnapshotControlRecord;
using SnapshotEndRecord = SnapshotControlRecord;

struct alignas(8) SnapshotChunkRecord {
  RecordHeader header;
  std::uint32_t chunk_index;
  std::uint16_t level_count;
  std::uint8_t side;
  std::uint8_t reserved;
  std::array<Level, kSnapshotLevelsPerChunk> levels;
};

struct alignas(8) InstrumentUpdateRecord {
  RecordHeader header;
  Instrument instrument;
};

struct alignas(8) InstrumentCatalogRecord {
  RecordHeader header;
  InstrumentCatalog catalog;
};

struct alignas(8) AggBboSide {
  std::int64_t price;
  std::int64_t quantity;
  std::array<std::int64_t, kAggVenueSlots> venue_quantity;
  std::uint64_t exchange_ts_ns;
  std::uint32_t venue_mask;
  std::uint32_t worst_ingress_age_us;
  std::uint8_t best_venue;
  std::uint8_t timestamp_venue;
  std::uint8_t contributor_count;
  std::uint8_t reserved[5];
};

struct alignas(8) AggBboRawSide {
  std::int64_t price;
  std::int64_t quantity;
  std::uint32_t venue_mask;
  std::uint8_t best_venue;
  std::uint8_t reserved[3];
};

struct alignas(8) AggBboRecord {
  RecordHeader header;
  AggBboSide gated_bid;
  AggBboSide gated_ask;
  AggBboRawSide raw_bid;
  AggBboRawSide raw_ask;
  std::array<char, 16> base_asset;
  std::array<char, 16> quote_asset;
  std::array<std::uint8_t, kAggVenueSlots> venue_slot_ids;
  std::uint8_t price_scale;
  std::uint8_t quantity_scale;
  std::uint8_t member_count;
  std::uint8_t identity_reserved[5];
  std::uint32_t member_mask;
  std::uint32_t live_mask;
  std::int32_t raw_cross_bps;
  std::int32_t gated_cross_bps;
  std::uint32_t skew_us;
  std::uint32_t cross_skew_threshold_us;
  std::uint32_t fx_age_us;
  std::uint8_t cross_bid_venue;
  std::uint8_t cross_ask_venue;
  std::uint8_t fx_venue;
  std::uint8_t summary_reserved[9];
};

struct alignas(8) AggLevel {
  std::int64_t price;
  std::int64_t quantity;
  std::array<std::int64_t, kAggVenueSlots> venue_quantity;
  std::uint32_t venue_mask;
  std::uint8_t contributor_count;
  std::uint8_t reserved[3];
};

struct alignas(8) AggOrderBookRecord {
  RecordHeader header;
  std::array<char, 16> base_asset;
  std::array<char, 16> quote_asset;
  std::array<std::uint8_t, kAggVenueSlots> venue_slot_ids;
  std::uint8_t price_scale;
  std::uint8_t quantity_scale;
  std::uint8_t member_count;
  std::uint8_t metadata_reserved;
  std::uint32_t member_mask;
  std::uint32_t active_mask;
  std::uint16_t bid_count;
  std::uint16_t ask_count;
  std::array<AggLevel, kAggMaxLevelsPerSide> bids;
  std::array<AggLevel, kAggMaxLevelsPerSide> asks;
};

constexpr RecordHeader MakeHeader(MessageType type,
                                  std::uint16_t length) noexcept {
  RecordHeader header{};
  header.magic = kMagic;
  header.schema_major = kSchemaMajor;
  header.schema_minor = kSchemaMinor;
  header.message_type = static_cast<std::uint16_t>(type);
  header.record_length = length;
  return header;
}

static_assert(sizeof(RecordHeader) == 72);
static_assert(offsetof(RecordHeader, magic) == 0x00);
static_assert(offsetof(RecordHeader, schema_major) == 0x04);
static_assert(offsetof(RecordHeader, message_type) == 0x08);
static_assert(offsetof(RecordHeader, reserved) == 0x0c);
static_assert(offsetof(RecordHeader, instrument_id) == 0x10);
static_assert(offsetof(RecordHeader, bus_seq) == 0x18);
static_assert(offsetof(RecordHeader, source_seq) == 0x20);
static_assert(offsetof(RecordHeader, exchange_ts_ns) == 0x28);
static_assert(offsetof(RecordHeader, receive_tsc) == 0x30);
static_assert(offsetof(RecordHeader, publish_tsc) == 0x38);
static_assert(offsetof(RecordHeader, book_generation) == 0x40);
static_assert(offsetof(RecordHeader, state) == 0x44);
static_assert(sizeof(BboRecord) == 104);
static_assert(offsetof(BboRecord, bid_price) == 72);
static_assert(sizeof(TickerRecord) == 176);
static_assert(offsetof(TickerRecord, funding_rate) == 136);
static_assert(offsetof(TickerRecord, open_price) == 144);
static_assert(offsetof(TickerRecord, high_price) == 152);
static_assert(offsetof(TickerRecord, low_price) == 160);
static_assert(offsetof(TickerRecord, close_price) == 168);
static_assert(sizeof(DeltaRecord) == 96);
static_assert(offsetof(DeltaRecord, price) == 80);
static_assert(sizeof(SnapshotControlRecord) == 80);
static_assert(sizeof(SnapshotChunkRecord) == 464);
static_assert(offsetof(SnapshotChunkRecord, side) == 78);
static_assert(offsetof(SnapshotChunkRecord, levels) == 80);
static_assert(sizeof(InstrumentUpdateRecord) == 296);
static_assert(offsetof(InstrumentUpdateRecord, instrument) == 72);
static_assert(sizeof(InstrumentCatalogRecord) == 472);
static_assert(sizeof(AggBboSide) == 104);
static_assert(sizeof(AggBboRawSide) == 24);
static_assert(sizeof(AggBboRecord) == 416);
static_assert(sizeof(AggLevel) == 88);
static_assert(sizeof(AggOrderBookRecord) == 14208);
static_assert(std::is_trivially_copyable_v<RecordHeader>);
static_assert(std::is_trivially_copyable_v<BboRecord>);
static_assert(std::is_trivially_copyable_v<TickerRecord>);
static_assert(std::is_trivially_copyable_v<DeltaRecord>);
static_assert(std::is_trivially_copyable_v<SnapshotControlRecord>);
static_assert(std::is_trivially_copyable_v<SnapshotChunkRecord>);
static_assert(std::is_trivially_copyable_v<InstrumentUpdateRecord>);
static_assert(std::is_trivially_copyable_v<InstrumentCatalogRecord>);
static_assert(std::is_trivially_copyable_v<AggBboRecord>);
static_assert(std::is_trivially_copyable_v<AggOrderBookRecord>);
static_assert(std::is_standard_layout_v<InstrumentUpdateRecord>);

}  // namespace utils::md::wire
