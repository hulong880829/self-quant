#include "utils/md/wire_codec.h"

#include <cstring>
#include <limits>
#include <type_traits>

namespace utils::md::wire {
namespace {

constexpr bool valid_state(std::uint8_t state) noexcept {
  return state <= static_cast<std::uint8_t>(BookState::NeedsRestart);
}

constexpr std::size_t expected_size(std::uint16_t type) noexcept {
  switch (static_cast<MessageType>(type)) {
    case MessageType::InstrumentUpdate:
      return sizeof(InstrumentUpdateRecord);
    case MessageType::InstrumentCatalog:
      return sizeof(InstrumentCatalogRecord);
    case MessageType::Bbo:
      return sizeof(BboRecord);
    case MessageType::BookDelta:
      return sizeof(DeltaRecord);
    case MessageType::SnapshotBegin:
    case MessageType::SnapshotEnd:
      return sizeof(SnapshotControlRecord);
    case MessageType::SnapshotChunk:
      return sizeof(SnapshotChunkRecord);
    case MessageType::Ticker:
      return sizeof(TickerRecord);
    case MessageType::AggBbo:
      return sizeof(AggBboRecord);
    case MessageType::AggOrderBook:
      return sizeof(AggOrderBookRecord);
  }
  return 0;
}

constexpr bool valid_flags(MessageType type, std::uint16_t flags) noexcept {
  if (type == MessageType::AggBbo) {
    constexpr std::uint16_t allowed =
        kAggSkewEnforced | kAggMemberDataError;
    return (flags & static_cast<std::uint16_t>(~allowed)) == 0;
  }
  if (type == MessageType::AggOrderBook) {
    return flags == 0;
  }
  return flags == 0;
}

RecordHeader make_header(MessageType type, std::size_t length,
                         const HeaderFields &fields) noexcept {
  auto header = MakeHeader(type, static_cast<std::uint16_t>(length));
  header.instrument_id = fields.instrument_id;
  header.bus_seq = fields.bus_seq;
  header.source_seq = fields.source_seq;
  header.exchange_ts_ns = fields.exchange_ts_ns;
  header.receive_tsc = fields.receive_tsc;
  header.publish_tsc = fields.publish_tsc;
  header.book_generation = fields.book_generation;
  header.state = static_cast<std::uint8_t>(fields.state);
  header.source_id = fields.source_id;
  header.flags = fields.flags;
  return header;
}

template <typename Record>
EncodeResult write_record(std::span<std::byte> destination,
                          const Record &record) noexcept {
  static_assert(std::is_trivially_copyable_v<Record>);
  if (destination.size() < sizeof(Record)) {
    return {CodecError::BufferTooSmall, 0};
  }
  std::memcpy(destination.data(), &record, sizeof(Record));
  return {CodecError::Ok, sizeof(Record)};
}

template <typename Record>
Record load_record(std::span<const std::byte> bytes) noexcept {
  Record record{};
  std::memcpy(&record, bytes.data(), sizeof(Record));
  return record;
}

CodecError validate_encode_fields(MessageType type,
                                  const HeaderFields &fields) noexcept {
  return valid_state(static_cast<std::uint8_t>(fields.state)) &&
                 valid_flags(type, fields.flags)
             ? CodecError::Ok
             : CodecError::InvalidField;
}

template <typename Record>
CodecError decode_record(std::span<const std::byte> bytes,
                         MessageType expected_type,
                         Record &record) noexcept {
  static_assert(std::is_trivially_copyable_v<Record>);
  if (bytes.size() < sizeof(RecordHeader)) {
    return CodecError::BufferTooSmall;
  }
  if (bytes.size() != sizeof(Record)) {
    return CodecError::LengthMismatch;
  }
  std::memcpy(&record, bytes.data(), sizeof(Record));
  const auto &header = record.header;
  if (header.magic != kMagic) {
    return CodecError::BadMagic;
  }
  if (header.schema_major != kSchemaMajor ||
      header.schema_minor < kSchemaMinor) {
    return CodecError::UnsupportedSchema;
  }
  if (header.message_type != static_cast<std::uint16_t>(expected_type)) {
    return expected_size(header.message_type) == 0
               ? CodecError::UnknownMessageType
               : CodecError::LengthMismatch;
  }
  if (header.record_length != sizeof(Record)) {
    return CodecError::LengthMismatch;
  }
  if (!valid_state(header.state) ||
      !valid_flags(expected_type, header.flags)) {
    return CodecError::InvalidField;
  }
  return CodecError::Ok;
}

bool valid_agg_bbo(const AggBboRecord &record) noexcept {
  const auto bytes_zero = [](const auto &bytes) noexcept {
    for (const auto byte : bytes) {
      if (byte != 0) {
        return false;
      }
    }
    return true;
  };
  const auto valid_side = [](const AggBboSide &side,
                             std::uint32_t live_mask) {
    if (side.quantity < 0 ||
        side.contributor_count > kAggVenueSlots ||
        side.venue_mask >= (1U << kAggVenueSlots) ||
        (side.venue_mask & ~live_mask) != 0 ||
        std::popcount(side.venue_mask) != side.contributor_count) {
      return false;
    }
    for (const auto byte : side.reserved) {
      if (byte != 0) {
        return false;
      }
    }
    std::int64_t total = 0;
    for (std::size_t slot = 0; slot < kAggVenueSlots; ++slot) {
      const auto quantity = side.venue_quantity[slot];
      if (quantity < 0 ||
          (((side.venue_mask >> slot) & 1U) == 0 && quantity != 0) ||
          total > std::numeric_limits<std::int64_t>::max() - quantity) {
        return false;
      }
      total += quantity;
    }
    if (total != side.quantity) {
      return false;
    }
    if (side.venue_mask == 0) {
      return side.price == 0 && side.quantity == 0 &&
             side.exchange_ts_ns == 0 && side.worst_ingress_age_us == 0 &&
             side.best_venue == 0 && side.timestamp_venue == 0 &&
             side.contributor_count == 0;
    }
    return side.price > 0 && side.best_venue < kAggVenueSlots &&
           side.timestamp_venue < kAggVenueSlots &&
           (side.venue_mask & (1U << side.best_venue)) != 0 &&
           (side.venue_mask & (1U << side.timestamp_venue)) != 0;
  };
  const auto valid_raw_side = [](const AggBboRawSide &side,
                                 std::uint32_t member_mask) noexcept {
    if (side.quantity < 0 || side.venue_mask >= (1U << kAggVenueSlots) ||
        (side.venue_mask & ~member_mask) != 0) {
      return false;
    }
    for (const auto byte : side.reserved) {
      if (byte != 0) {
        return false;
      }
    }
    if (side.venue_mask == 0) {
      return side.price == 0 && side.quantity == 0 &&
             side.best_venue == 0;
    }
    return side.price > 0 && side.best_venue < kAggVenueSlots &&
           (side.venue_mask & (1U << side.best_venue)) != 0;
  };
  if (record.member_count > kAggVenueSlots ||
      record.member_mask >= (1U << kAggVenueSlots) ||
      std::popcount(record.member_mask) != record.member_count ||
      record.live_mask >= (1U << kAggVenueSlots) ||
      (record.live_mask & ~record.member_mask) != 0 ||
      record.raw_cross_bps < 0 || record.gated_cross_bps < 0 ||
      !bytes_zero(record.identity_reserved) ||
      !bytes_zero(record.summary_reserved) ||
      !valid_side(record.gated_bid, record.live_mask) ||
      !valid_side(record.gated_ask, record.live_mask) ||
      !valid_raw_side(record.raw_bid, record.member_mask) ||
      !valid_raw_side(record.raw_ask, record.member_mask)) {
    return false;
  }
  for (std::size_t slot = 0; slot < kAggVenueSlots; ++slot) {
    const bool member = (record.member_mask & (1U << slot)) != 0;
    if (member == (record.venue_slot_ids[slot] == 0)) {
      return false;
    }
  }
  return record.cross_bid_venue == record.gated_bid.timestamp_venue &&
         record.cross_ask_venue == record.gated_ask.timestamp_venue;
}

bool valid_agg_level(const AggLevel &level,
                     std::uint32_t active_mask) noexcept {
  if (level.quantity < 0 ||
      level.contributor_count > kAggVenueSlots ||
      level.venue_mask >= (1U << kAggVenueSlots) ||
      (level.venue_mask & ~active_mask) != 0 ||
      std::popcount(level.venue_mask) != level.contributor_count ||
      level.venue_mask == 0 || level.price <= 0) {
    return false;
  }
  for (const auto byte : level.reserved) {
    if (byte != 0) {
      return false;
    }
  }
  std::int64_t total = 0;
  for (std::size_t slot = 0; slot < kAggVenueSlots; ++slot) {
    const auto quantity = level.venue_quantity[slot];
    if (quantity < 0 ||
        (((level.venue_mask >> slot) & 1U) == 0 && quantity != 0) ||
        total > std::numeric_limits<std::int64_t>::max() - quantity) {
      return false;
    }
    total += quantity;
  }
  return total == level.quantity;
}

bool valid_agg_orderbook(const AggOrderBookRecord &record) noexcept {
  if (record.member_count > kAggVenueSlots ||
      record.bid_count > kAggMaxLevelsPerSide ||
      record.ask_count > kAggMaxLevelsPerSide ||
      record.member_mask >= (1U << kAggVenueSlots) ||
      record.active_mask >= (1U << kAggVenueSlots) ||
      (record.active_mask & ~record.member_mask) != 0 ||
      std::popcount(record.member_mask) != record.member_count ||
      record.metadata_reserved != 0) {
    return false;
  }
  for (std::size_t slot = 0; slot < kAggVenueSlots; ++slot) {
    const bool member = (record.member_mask & (1U << slot)) != 0;
    if (member == (record.venue_slot_ids[slot] == 0)) {
      return false;
    }
  }
  for (std::size_t index = 0; index < record.bid_count; ++index) {
    if (!valid_agg_level(record.bids[index], record.active_mask) ||
        (index != 0 &&
         record.bids[index - 1].price <= record.bids[index].price)) {
      return false;
    }
  }
  for (std::size_t index = 0; index < record.ask_count; ++index) {
    if (!valid_agg_level(record.asks[index], record.active_mask) ||
        (index != 0 &&
         record.asks[index - 1].price >= record.asks[index].price)) {
      return false;
    }
  }
  return true;
}

}  // namespace

CodecError ValidateHeader(std::span<const std::byte> bytes,
                          RecordHeader *header) noexcept {
  if (bytes.size() < sizeof(RecordHeader)) {
    return CodecError::BufferTooSmall;
  }
  const auto decoded = load_record<RecordHeader>(bytes);
  if (decoded.magic != kMagic) {
    return CodecError::BadMagic;
  }
  if (decoded.schema_major != kSchemaMajor ||
      decoded.schema_minor < kSchemaMinor) {
    return CodecError::UnsupportedSchema;
  }
  const auto length = expected_size(decoded.message_type);
  if (length == 0) {
    if (decoded.record_length < sizeof(RecordHeader) ||
        (decoded.record_length % alignof(RecordHeader)) != 0 ||
        bytes.size() != decoded.record_length) {
      return CodecError::LengthMismatch;
    }
    if (header != nullptr) {
      *header = decoded;
    }
    return CodecError::UnknownMessageType;
  }
  if (decoded.record_length != length || bytes.size() != length) {
    return CodecError::LengthMismatch;
  }
  if (!valid_state(decoded.state) ||
      !valid_flags(static_cast<MessageType>(decoded.message_type),
                   decoded.flags)) {
    return CodecError::InvalidField;
  }
  if (header != nullptr) {
    *header = decoded;
  }
  return CodecError::Ok;
}

CodecError DecodeInstrument(std::span<const std::byte> bytes,
                            InstrumentUpdateRecord &record) noexcept {
  const auto result =
      decode_record(bytes, MessageType::InstrumentUpdate, record);
  if (result != CodecError::Ok) {
    return result;
  }
  return record.instrument.instrument_id == record.header.instrument_id &&
                 (record.instrument.flags &
                  ~kInstrumentRefineBookTick) == 0 &&
                 record.instrument.reserved0 == 0
             ? CodecError::Ok
             : CodecError::InvalidField;
}

CodecError DecodeInstrumentCatalog(
    std::span<const std::byte> bytes,
    InstrumentCatalogRecord &record) noexcept {
  const auto result =
      decode_record(bytes, MessageType::InstrumentCatalog, record);
  if (result != CodecError::Ok) {
    return result;
  }
  return record.catalog.instrument_id == record.header.instrument_id &&
                 record.catalog.instrument_id != 0
             ? CodecError::Ok
             : CodecError::InvalidField;
}

CodecError DecodeBbo(std::span<const std::byte> bytes,
                     BboRecord &record) noexcept {
  const auto result = decode_record(bytes, MessageType::Bbo, record);
  if (result != CodecError::Ok) {
    return result;
  }
  return record.bid_quantity >= 0 && record.ask_quantity >= 0
             ? CodecError::Ok
             : CodecError::InvalidField;
}

CodecError DecodeTicker(std::span<const std::byte> bytes,
                        TickerRecord &record) noexcept {
  const auto result = decode_record(bytes, MessageType::Ticker, record);
  if (result != CodecError::Ok) {
    return result;
  }
  return record.bid_quantity >= 0 && record.ask_quantity >= 0 &&
                 record.last_quantity >= 0
             ? CodecError::Ok
             : CodecError::InvalidField;
}

CodecError DecodeDelta(std::span<const std::byte> bytes,
                       DeltaRecord &record) noexcept {
  const auto result = decode_record(bytes, MessageType::BookDelta, record);
  if (result != CodecError::Ok) {
    return result;
  }
  if ((record.side != static_cast<std::uint8_t>(Side::Bid) &&
       record.side != static_cast<std::uint8_t>(Side::Ask)) ||
      record.quantity < 0) {
    return CodecError::InvalidField;
  }
  for (const auto byte : record.reserved) {
    if (byte != 0) {
      return CodecError::InvalidField;
    }
  }
  return CodecError::Ok;
}

CodecError DecodeSnapshotBegin(std::span<const std::byte> bytes,
                               SnapshotBeginRecord &record) noexcept {
  const auto result =
      decode_record(bytes, MessageType::SnapshotBegin, record);
  if (result != CodecError::Ok) {
    return result;
  }
  const auto chunks =
      (static_cast<std::size_t>(record.item_count) +
       kSnapshotLevelsPerChunk - 1) /
      kSnapshotLevelsPerChunk;
  return record.chunk_count_or_checksum >= chunks &&
                 (record.item_count == 0
                      ? record.chunk_count_or_checksum == 0
                      : record.chunk_count_or_checksum <= record.item_count)
             ? CodecError::Ok
             : CodecError::InvalidField;
}

CodecError DecodeSnapshotChunk(std::span<const std::byte> bytes,
                               SnapshotChunkRecord &record) noexcept {
  const auto result =
      decode_record(bytes, MessageType::SnapshotChunk, record);
  if (result != CodecError::Ok) {
    return result;
  }
  if (record.level_count == 0 ||
      record.level_count > kSnapshotLevelsPerChunk ||
      (record.side != static_cast<std::uint8_t>(Side::Bid) &&
       record.side != static_cast<std::uint8_t>(Side::Ask)) ||
      record.reserved != 0) {
    return CodecError::InvalidField;
  }
  for (std::size_t index = record.level_count;
       index < kSnapshotLevelsPerChunk; ++index) {
    if (record.levels[index].price != 0 ||
        record.levels[index].quantity != 0) {
      return CodecError::InvalidField;
    }
  }
  return CodecError::Ok;
}

CodecError DecodeSnapshotEnd(std::span<const std::byte> bytes,
                             SnapshotEndRecord &record) noexcept {
  return decode_record(bytes, MessageType::SnapshotEnd, record);
}

CodecError DecodeAggBbo(std::span<const std::byte> bytes,
                        AggBboRecord &record) noexcept {
  const auto result = decode_record(bytes, MessageType::AggBbo, record);
  if (result != CodecError::Ok) {
    return result;
  }
  return valid_agg_bbo(record) ? CodecError::Ok
                               : CodecError::InvalidField;
}

CodecError DecodeAggOrderBook(std::span<const std::byte> bytes,
                              AggOrderBookRecord &record) noexcept {
  const auto result =
      decode_record(bytes, MessageType::AggOrderBook, record);
  if (result != CodecError::Ok) {
    return result;
  }
  return valid_agg_orderbook(record) ? CodecError::Ok
                                     : CodecError::InvalidField;
}

EncodeResult EncodeInstrument(std::span<std::byte> destination,
                              const HeaderFields &header,
                              const Instrument &instrument) noexcept {
  if (validate_encode_fields(MessageType::InstrumentUpdate, header) !=
          CodecError::Ok ||
      header.instrument_id != instrument.instrument_id ||
      (instrument.flags & ~kInstrumentRefineBookTick) != 0 ||
      instrument.reserved0 != 0) {
    return {CodecError::InvalidField, 0};
  }
  InstrumentUpdateRecord record{};
  record.header =
      make_header(MessageType::InstrumentUpdate, sizeof(record), header);
  record.instrument = instrument;
  return write_record(destination, record);
}

EncodeResult EncodeInstrumentCatalog(
    std::span<std::byte> destination, const HeaderFields &header,
    const InstrumentCatalog &catalog) noexcept {
  if (validate_encode_fields(MessageType::InstrumentCatalog, header) !=
          CodecError::Ok ||
      header.instrument_id == 0 ||
      header.instrument_id != catalog.instrument_id) {
    return {CodecError::InvalidField, 0};
  }
  InstrumentCatalogRecord record{};
  record.header =
      make_header(MessageType::InstrumentCatalog, sizeof(record), header);
  record.catalog = catalog;
  return write_record(destination, record);
}

EncodeResult EncodeBbo(std::span<std::byte> destination,
                       const HeaderFields &header, const Level &bid,
                       const Level &ask) noexcept {
  if (validate_encode_fields(MessageType::Bbo, header) != CodecError::Ok ||
      bid.quantity < 0 || ask.quantity < 0) {
    return {CodecError::InvalidField, 0};
  }
  BboRecord record{};
  record.header = make_header(MessageType::Bbo, sizeof(record), header);
  record.bid_price = bid.price;
  record.bid_quantity = bid.quantity;
  record.ask_price = ask.price;
  record.ask_quantity = ask.quantity;
  return write_record(destination, record);
}

EncodeResult EncodeTicker(std::span<std::byte> destination,
                          const HeaderFields &header,
                          const TickerEvent &event) noexcept {
  if (validate_encode_fields(MessageType::Ticker, header) !=
          CodecError::Ok ||
      event.bid.quantity < 0 || event.ask.quantity < 0 ||
      event.last_quantity < 0) {
    return {CodecError::InvalidField, 0};
  }
  TickerRecord record{};
  record.header = make_header(MessageType::Ticker, sizeof(record), header);
  record.bid_price = event.bid.price;
  record.bid_quantity = event.bid.quantity;
  record.ask_price = event.ask.price;
  record.ask_quantity = event.ask.quantity;
  record.last_price = event.last_price;
  record.last_quantity = event.last_quantity;
  record.mark_price = event.mark_price;
  record.index_price = event.index_price;
  record.funding_rate = event.funding_rate;
  record.open_price = event.open_price;
  record.high_price = event.high_price;
  record.low_price = event.low_price;
  record.close_price = event.close_price;
  return write_record(destination, record);
}

EncodeResult EncodeDelta(std::span<std::byte> destination,
                         const HeaderFields &header, Side side,
                         const Level &level) noexcept {
  if (validate_encode_fields(MessageType::BookDelta, header) !=
          CodecError::Ok ||
      (side != Side::Bid && side != Side::Ask) || level.quantity < 0) {
    return {CodecError::InvalidField, 0};
  }
  DeltaRecord record{};
  record.header = make_header(MessageType::BookDelta, sizeof(record), header);
  record.side = static_cast<std::uint8_t>(side);
  record.price = level.price;
  record.quantity = level.quantity;
  return write_record(destination, record);
}

EncodeResult EncodeSnapshotBegin(std::span<std::byte> destination,
                                 const HeaderFields &header,
                                 std::uint32_t level_count,
                                 std::uint32_t chunk_count) noexcept {
  const auto expected_chunks =
      (static_cast<std::size_t>(level_count) +
       kSnapshotLevelsPerChunk - 1) /
      kSnapshotLevelsPerChunk;
  if (validate_encode_fields(MessageType::SnapshotBegin, header) !=
          CodecError::Ok ||
      chunk_count < expected_chunks ||
      (level_count == 0 ? chunk_count != 0 : chunk_count > level_count)) {
    return {CodecError::InvalidField, 0};
  }
  SnapshotBeginRecord record{};
  record.header =
      make_header(MessageType::SnapshotBegin, sizeof(record), header);
  record.item_count = level_count;
  record.chunk_count_or_checksum = chunk_count;
  return write_record(destination, record);
}

EncodeResult EncodeSnapshotChunk(std::span<std::byte> destination,
                                 const HeaderFields &header,
                                 std::uint32_t chunk_index,
                                 Side side,
                                 std::span<const Level> levels) noexcept {
  if (validate_encode_fields(MessageType::SnapshotChunk, header) !=
          CodecError::Ok ||
      levels.empty() ||
      levels.size() > kSnapshotLevelsPerChunk ||
      (side != Side::Bid && side != Side::Ask)) {
    return {CodecError::InvalidField, 0};
  }
  SnapshotChunkRecord record{};
  record.header =
      make_header(MessageType::SnapshotChunk, sizeof(record), header);
  record.chunk_index = chunk_index;
  record.level_count = static_cast<std::uint16_t>(levels.size());
  record.side = static_cast<std::uint8_t>(side);
  for (std::size_t index = 0; index < levels.size(); ++index) {
    if (levels[index].quantity < 0) {
      return {CodecError::InvalidField, 0};
    }
    record.levels[index] = levels[index];
  }
  return write_record(destination, record);
}

EncodeResult EncodeSnapshotEnd(std::span<std::byte> destination,
                               const HeaderFields &header,
                               std::uint32_t received_levels,
                               std::uint32_t checksum) noexcept {
  if (validate_encode_fields(MessageType::SnapshotEnd, header) !=
      CodecError::Ok) {
    return {CodecError::InvalidField, 0};
  }
  SnapshotEndRecord record{};
  record.header =
      make_header(MessageType::SnapshotEnd, sizeof(record), header);
  record.item_count = received_levels;
  record.chunk_count_or_checksum = checksum;
  return write_record(destination, record);
}

EncodeResult EncodeAggBbo(std::span<std::byte> destination,
                          const HeaderFields &header,
                          const AggBboRecord &value) noexcept {
  if (validate_encode_fields(MessageType::AggBbo, header) !=
          CodecError::Ok ||
      !valid_agg_bbo(value)) {
    return {CodecError::InvalidField, 0};
  }
  AggBboRecord record = value;
  record.header =
      make_header(MessageType::AggBbo, sizeof(record), header);
  return write_record(destination, record);
}

EncodeResult EncodeAggOrderBook(
    std::span<std::byte> destination, const HeaderFields &header,
    const AggOrderBookRecord &value) noexcept {
  if (validate_encode_fields(MessageType::AggOrderBook, header) !=
          CodecError::Ok ||
      !valid_agg_orderbook(value)) {
    return {CodecError::InvalidField, 0};
  }
  if (destination.size() < sizeof(AggOrderBookRecord)) {
    return {CodecError::BufferTooSmall, 0};
  }
  std::memcpy(destination.data(), &value, sizeof(value));
  const auto wire_header =
      make_header(MessageType::AggOrderBook, sizeof(value), header);
  std::memcpy(destination.data(), &wire_header, sizeof(wire_header));
  return {CodecError::Ok, sizeof(AggOrderBookRecord)};
}

CodecError Decode(std::span<const std::byte> bytes,
                  RecordVisitor &visitor) noexcept {
  RecordHeader header{};
  const auto result = ValidateHeader(bytes, &header);
  if (result != CodecError::Ok) {
    return result;
  }
  switch (static_cast<MessageType>(header.message_type)) {
    case MessageType::InstrumentUpdate: {
      InstrumentUpdateRecord record{};
      const auto decoded = DecodeInstrument(bytes, record);
      if (decoded != CodecError::Ok) {
        return decoded;
      }
      return visitor.OnInstrument(record) ? CodecError::Ok
                                          : CodecError::InvalidField;
    }
    case MessageType::InstrumentCatalog: {
      InstrumentCatalogRecord record{};
      const auto decoded = DecodeInstrumentCatalog(bytes, record);
      if (decoded != CodecError::Ok) {
        return decoded;
      }
      return visitor.OnInstrumentCatalog(record) ? CodecError::Ok
                                                 : CodecError::InvalidField;
    }
    case MessageType::Bbo: {
      BboRecord record{};
      const auto decoded = DecodeBbo(bytes, record);
      if (decoded != CodecError::Ok) {
        return decoded;
      }
      return visitor.OnBbo(record) ? CodecError::Ok
                                   : CodecError::InvalidField;
    }
    case MessageType::Ticker: {
      TickerRecord record{};
      const auto decoded = DecodeTicker(bytes, record);
      if (decoded != CodecError::Ok) {
        return decoded;
      }
      return visitor.OnTicker(record) ? CodecError::Ok
                                      : CodecError::InvalidField;
    }
    case MessageType::BookDelta: {
      DeltaRecord record{};
      const auto decoded = DecodeDelta(bytes, record);
      if (decoded != CodecError::Ok) {
        return decoded;
      }
      return visitor.OnDelta(record) ? CodecError::Ok
                                     : CodecError::InvalidField;
    }
    case MessageType::SnapshotBegin: {
      SnapshotBeginRecord record{};
      const auto decoded = DecodeSnapshotBegin(bytes, record);
      if (decoded != CodecError::Ok) {
        return decoded;
      }
      return visitor.OnSnapshotBegin(record) ? CodecError::Ok
                                             : CodecError::InvalidField;
    }
    case MessageType::SnapshotChunk: {
      SnapshotChunkRecord record{};
      const auto decoded = DecodeSnapshotChunk(bytes, record);
      if (decoded != CodecError::Ok) {
        return decoded;
      }
      return visitor.OnSnapshotChunk(record) ? CodecError::Ok
                                             : CodecError::InvalidField;
    }
    case MessageType::SnapshotEnd: {
      SnapshotEndRecord record{};
      const auto decoded = DecodeSnapshotEnd(bytes, record);
      if (decoded != CodecError::Ok) {
        return decoded;
      }
      return visitor.OnSnapshotEnd(record) ? CodecError::Ok
                                           : CodecError::InvalidField;
    }
    case MessageType::AggBbo:
    case MessageType::AggOrderBook:
      break;
  }
  return CodecError::UnknownMessageType;
}

}  // namespace utils::md::wire
