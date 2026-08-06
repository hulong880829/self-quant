#include "utils/md/wire_codec.h"

#include <cstring>
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
  }
  return 0;
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

CodecError validate_encode_fields(const HeaderFields &fields) noexcept {
  return valid_state(static_cast<std::uint8_t>(fields.state)) &&
                 fields.flags == 0
             ? CodecError::Ok
             : CodecError::InvalidField;
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
      decoded.schema_minor != kSchemaMinor) {
    return CodecError::UnsupportedSchema;
  }
  const auto length = expected_size(decoded.message_type);
  if (length == 0) {
    return CodecError::UnknownMessageType;
  }
  if (decoded.record_length != length || bytes.size() != length) {
    return CodecError::LengthMismatch;
  }
  if (!valid_state(decoded.state) || decoded.flags != 0) {
    return CodecError::InvalidField;
  }
  if (header != nullptr) {
    *header = decoded;
  }
  return CodecError::Ok;
}

EncodeResult EncodeInstrument(std::span<std::byte> destination,
                              const HeaderFields &header,
                              const Instrument &instrument) noexcept {
  if (validate_encode_fields(header) != CodecError::Ok ||
      header.instrument_id != instrument.instrument_id) {
    return {CodecError::InvalidField, 0};
  }
  InstrumentUpdateRecord record{};
  record.header =
      make_header(MessageType::InstrumentUpdate, sizeof(record), header);
  record.instrument = instrument;
  return write_record(destination, record);
}

EncodeResult EncodeBbo(std::span<std::byte> destination,
                       const HeaderFields &header, const Level &bid,
                       const Level &ask) noexcept {
  if (validate_encode_fields(header) != CodecError::Ok ||
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
  if (validate_encode_fields(header) != CodecError::Ok ||
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
  if (validate_encode_fields(header) != CodecError::Ok ||
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
  if (validate_encode_fields(header) != CodecError::Ok ||
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
  if (validate_encode_fields(header) != CodecError::Ok || levels.empty() ||
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
  if (validate_encode_fields(header) != CodecError::Ok) {
    return {CodecError::InvalidField, 0};
  }
  SnapshotEndRecord record{};
  record.header =
      make_header(MessageType::SnapshotEnd, sizeof(record), header);
  record.item_count = received_levels;
  record.chunk_count_or_checksum = checksum;
  return write_record(destination, record);
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
      const auto record = load_record<InstrumentUpdateRecord>(bytes);
      if (record.instrument.instrument_id != header.instrument_id) {
        return CodecError::InvalidField;
      }
      return visitor.OnInstrument(record) ? CodecError::Ok
                                          : CodecError::InvalidField;
    }
    case MessageType::Bbo:
    {
      const auto record = load_record<BboRecord>(bytes);
      if (record.bid_quantity < 0 || record.ask_quantity < 0) {
        return CodecError::InvalidField;
      }
      return visitor.OnBbo(record) ? CodecError::Ok
                                   : CodecError::InvalidField;
    }
    case MessageType::Ticker: {
      const auto record = load_record<TickerRecord>(bytes);
      if (record.bid_quantity < 0 || record.ask_quantity < 0 ||
          record.last_quantity < 0) {
        return CodecError::InvalidField;
      }
      return visitor.OnTicker(record) ? CodecError::Ok
                                      : CodecError::InvalidField;
    }
    case MessageType::BookDelta: {
      const auto record = load_record<DeltaRecord>(bytes);
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
      return visitor.OnDelta(record) ? CodecError::Ok
                                     : CodecError::InvalidField;
    }
    case MessageType::SnapshotBegin: {
      const auto record = load_record<SnapshotBeginRecord>(bytes);
      const auto chunks =
          (static_cast<std::size_t>(record.item_count) +
           kSnapshotLevelsPerChunk - 1) /
          kSnapshotLevelsPerChunk;
      if (record.chunk_count_or_checksum < chunks ||
          (record.item_count == 0
               ? record.chunk_count_or_checksum != 0
               : record.chunk_count_or_checksum > record.item_count)) {
        return CodecError::InvalidField;
      }
      return visitor.OnSnapshotBegin(record) ? CodecError::Ok
                                             : CodecError::InvalidField;
    }
    case MessageType::SnapshotChunk: {
      const auto record = load_record<SnapshotChunkRecord>(bytes);
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
      return visitor.OnSnapshotChunk(record) ? CodecError::Ok
                                             : CodecError::InvalidField;
    }
    case MessageType::SnapshotEnd:
      return visitor.OnSnapshotEnd(load_record<SnapshotEndRecord>(bytes))
                 ? CodecError::Ok
                 : CodecError::InvalidField;
  }
  return CodecError::UnknownMessageType;
}

}  // namespace utils::md::wire
