#pragma once

#include <bit>
#include <cstddef>
#include <cstdint>
#include <type_traits>

#include "utils/md/types.h"

namespace utils::md::wire {

inline constexpr std::uint32_t kMagic = 0x444d5153U;  // Bytes: "SQMD".
inline constexpr std::uint16_t kSchemaMajor = 1;
inline constexpr std::uint16_t kSchemaMinor = 1;

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
  std::uint32_t instrument_id;    // 0x0c
  std::uint64_t bus_seq;          // 0x10
  std::uint64_t source_seq;       // 0x18
  std::uint64_t exchange_ts_ns;   // 0x20
  std::uint64_t receive_tsc;      // 0x28
  std::uint64_t publish_tsc;      // 0x30
  std::uint32_t book_generation;  // 0x38
  std::uint8_t state;             // 0x3c
  std::uint8_t source_id;         // 0x3d
  std::uint16_t flags;            // 0x3e
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

constexpr RecordHeader MakeHeader(MessageType type, std::uint16_t length) noexcept {
  return {kMagic, kSchemaMajor, kSchemaMinor, static_cast<std::uint16_t>(type),
          length, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0};
}

static_assert(sizeof(RecordHeader) == 64);
static_assert(offsetof(RecordHeader, magic) == 0x00);
static_assert(offsetof(RecordHeader, schema_major) == 0x04);
static_assert(offsetof(RecordHeader, message_type) == 0x08);
static_assert(offsetof(RecordHeader, instrument_id) == 0x0c);
static_assert(offsetof(RecordHeader, bus_seq) == 0x10);
static_assert(offsetof(RecordHeader, source_seq) == 0x18);
static_assert(offsetof(RecordHeader, exchange_ts_ns) == 0x20);
static_assert(offsetof(RecordHeader, receive_tsc) == 0x28);
static_assert(offsetof(RecordHeader, publish_tsc) == 0x30);
static_assert(offsetof(RecordHeader, book_generation) == 0x38);
static_assert(offsetof(RecordHeader, state) == 0x3c);
static_assert(sizeof(BboRecord) == 96);
static_assert(offsetof(BboRecord, bid_price) == 64);
static_assert(sizeof(TickerRecord) == 168);
static_assert(offsetof(TickerRecord, funding_rate) == 128);
static_assert(offsetof(TickerRecord, open_price) == 136);
static_assert(offsetof(TickerRecord, high_price) == 144);
static_assert(offsetof(TickerRecord, low_price) == 152);
static_assert(offsetof(TickerRecord, close_price) == 160);
static_assert(sizeof(DeltaRecord) == 88);
static_assert(offsetof(DeltaRecord, price) == 72);
static_assert(sizeof(SnapshotControlRecord) == 72);
static_assert(sizeof(SnapshotChunkRecord) == 456);
static_assert(offsetof(SnapshotChunkRecord, side) == 70);
static_assert(offsetof(SnapshotChunkRecord, levels) == 72);
static_assert(sizeof(InstrumentUpdateRecord) == 288);
static_assert(offsetof(InstrumentUpdateRecord, instrument) == 64);
static_assert(std::is_trivially_copyable_v<RecordHeader>);
static_assert(std::is_trivially_copyable_v<BboRecord>);
static_assert(std::is_trivially_copyable_v<TickerRecord>);
static_assert(std::is_trivially_copyable_v<DeltaRecord>);
static_assert(std::is_trivially_copyable_v<SnapshotControlRecord>);
static_assert(std::is_trivially_copyable_v<SnapshotChunkRecord>);
static_assert(std::is_trivially_copyable_v<InstrumentUpdateRecord>);
static_assert(std::is_standard_layout_v<InstrumentUpdateRecord>);

}  // namespace utils::md::wire
