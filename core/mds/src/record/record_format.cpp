#include "mds/record/record_format.h"

#include "utils/md/wire_codec.h"

#include <algorithm>
#include <array>
#include <bit>
#include <limits>
#include <type_traits>

namespace mds::record::detail {
namespace {

class Encoder {
 public:
  template <typename T>
  void put(T value) {
    using U = std::make_unsigned_t<T>;
    const auto bits = static_cast<U>(value);
    for (std::size_t index = 0; index < sizeof(T); ++index) {
      bytes_.push_back(static_cast<std::uint8_t>(bits >> (index * 8U)));
    }
  }

  template <typename T, std::size_t Size>
  void put_array(const std::array<T, Size> &values) {
    for (const auto value : values) {
      put(value);
    }
  }

  void append(std::span<const std::uint8_t> bytes) {
    bytes_.insert(bytes_.end(), bytes.begin(), bytes.end());
  }

  [[nodiscard]] std::vector<std::uint8_t> take() { return std::move(bytes_); }

 private:
  std::vector<std::uint8_t> bytes_;
};

class Decoder {
 public:
  explicit Decoder(std::span<const std::uint8_t> bytes) : bytes_(bytes) {}

  template <typename T>
  bool get(T &value) {
    using U = std::make_unsigned_t<T>;
    if (remaining() < sizeof(T)) {
      return false;
    }
    std::uint64_t bits{};
    for (std::size_t index = 0; index < sizeof(T); ++index) {
      bits |= static_cast<std::uint64_t>(bytes_[offset_++]) << (index * 8U);
    }
    value = static_cast<T>(static_cast<U>(bits));
    return true;
  }

  template <typename T, std::size_t Size>
  bool get_array(std::array<T, Size> &values) {
    for (auto &value : values) {
      if (!get(value)) {
        return false;
      }
    }
    return true;
  }

  [[nodiscard]] std::size_t remaining() const noexcept {
    return bytes_.size() - offset_;
  }
  [[nodiscard]] std::size_t offset() const noexcept { return offset_; }

 private:
  std::span<const std::uint8_t> bytes_;
  std::size_t offset_{};
};

void put_header(Encoder &out, const utils::md::wire::RecordHeader &header) {
  out.put(header.magic);
  out.put(header.schema_major);
  out.put(header.schema_minor);
  out.put(header.message_type);
  out.put(header.record_length);
  out.put(header.reserved);
  out.put(header.instrument_id);
  out.put(header.bus_seq);
  out.put(header.source_seq);
  out.put(header.exchange_ts_ns);
  out.put(header.receive_tsc);
  out.put(header.publish_tsc);
  out.put(header.book_generation);
  out.put(header.state);
  out.put(header.source_id);
  out.put(header.flags);
}

bool get_header(Decoder &in, utils::md::wire::RecordHeader &header,
                std::uint16_t format_major) {
  if (!in.get(header.magic) || !in.get(header.schema_major) ||
      !in.get(header.schema_minor) || !in.get(header.message_type) ||
      !in.get(header.record_length)) {
    return false;
  }
  if (format_major == 1) {
    std::uint32_t legacy_instrument_id{};
    if (!in.get(legacy_instrument_id)) {
      return false;
    }
    header.reserved = 0;
    header.instrument_id = legacy_instrument_id;
    header.schema_major = utils::md::wire::kSchemaMajor;
    header.schema_minor = utils::md::wire::kSchemaMinor;
    header.record_length =
        header.message_type ==
                static_cast<std::uint16_t>(utils::md::MessageType::AggBbo)
            ? sizeof(utils::md::wire::AggBboRecord)
            : sizeof(utils::md::wire::AggOrderBookRecord);
  } else if (!in.get(header.reserved) || !in.get(header.instrument_id)) {
    return false;
  }
  return
         in.get(header.bus_seq) && in.get(header.source_seq) &&
         in.get(header.exchange_ts_ns) && in.get(header.receive_tsc) &&
         in.get(header.publish_tsc) && in.get(header.book_generation) &&
         in.get(header.state) && in.get(header.source_id) &&
         in.get(header.flags);
}

void put_bbo_side(Encoder &out, const utils::md::wire::AggBboSide &side) {
  out.put(side.price);
  out.put(side.quantity);
  out.put_array(side.venue_quantity);
  out.put(side.exchange_ts_ns);
  out.put(side.venue_mask);
  out.put(side.worst_ingress_age_us);
  out.put(side.best_venue);
  out.put(side.timestamp_venue);
  out.put(side.contributor_count);
}

bool get_bbo_side(Decoder &in, utils::md::wire::AggBboSide &side) {
  return in.get(side.price) && in.get(side.quantity) &&
         in.get_array(side.venue_quantity) && in.get(side.exchange_ts_ns) &&
         in.get(side.venue_mask) && in.get(side.worst_ingress_age_us) &&
         in.get(side.best_venue) && in.get(side.timestamp_venue) &&
         in.get(side.contributor_count);
}

void put_raw_side(Encoder &out, const utils::md::wire::AggBboRawSide &side) {
  out.put(side.price);
  out.put(side.quantity);
  out.put(side.venue_mask);
  out.put(side.best_venue);
}

bool get_raw_side(Decoder &in, utils::md::wire::AggBboRawSide &side) {
  return in.get(side.price) && in.get(side.quantity) &&
         in.get(side.venue_mask) && in.get(side.best_venue);
}

void put_level(Encoder &out, const utils::md::wire::AggLevel &level) {
  out.put(level.price);
  out.put(level.quantity);
  out.put_array(level.venue_quantity);
  out.put(level.venue_mask);
  out.put(level.contributor_count);
}

bool get_level(Decoder &in, utils::md::wire::AggLevel &level) {
  return in.get(level.price) && in.get(level.quantity) &&
         in.get_array(level.venue_quantity) && in.get(level.venue_mask) &&
         in.get(level.contributor_count);
}

bool valid_header(const utils::md::wire::RecordHeader &header, Kind kind) {
  const auto expected =
      kind == Kind::AggBbo ? utils::md::MessageType::AggBbo
                           : utils::md::MessageType::AggOrderBook;
  const auto expected_length =
      kind == Kind::AggBbo ? sizeof(utils::md::wire::AggBboRecord)
                           : sizeof(utils::md::wire::AggOrderBookRecord);
  return header.magic == utils::md::wire::kMagic &&
         header.schema_major == utils::md::wire::kSchemaMajor &&
         header.schema_minor >= utils::md::wire::kSchemaMinor &&
         header.message_type == static_cast<std::uint16_t>(expected) &&
         header.record_length == expected_length;
}

template <typename Value>
std::span<const std::byte> wire_bytes(const Value &value) {
  return std::as_bytes(std::span<const Value>(&value, 1));
}

bool valid_bbo_semantics(const utils::md::wire::AggBboRecord &value) {
  utils::md::wire::AggBboRecord decoded{};
  return utils::md::wire::DecodeAggBbo(wire_bytes(value), decoded) ==
         utils::md::wire::CodecError::Ok;
}

bool valid_book_semantics(const utils::md::wire::AggOrderBookRecord &value) {
  utils::md::wire::AggOrderBookRecord decoded{};
  return utils::md::wire::DecodeAggOrderBook(wire_bytes(value), decoded) ==
         utils::md::wire::CodecError::Ok;
}

ReadResult failure(ReadError error, std::string message,
                   std::uint64_t records = 0) {
  return {.error = error, .message = std::move(message), .records = records};
}

}  // namespace

std::uint32_t crc32(std::span<const std::uint8_t> bytes) noexcept {
  std::uint32_t crc = 0xffffffffU;
  for (const auto byte : bytes) {
    crc ^= byte;
    for (unsigned bit = 0; bit < 8; ++bit) {
      const auto mask = 0U - (crc & 1U);
      crc = (crc >> 1U) ^ (0xedb88320U & mask);
    }
  }
  return ~crc;
}

std::vector<std::uint8_t> container_header() {
  Encoder out;
  out.put(kContainerMagic);
  out.put(kFormatMajor);
  out.put(kFormatMinor);
  out.put(std::uint32_t{0x01020304U});
  auto bytes = out.take();
  Encoder complete;
  complete.append(bytes);
  complete.put(crc32(bytes));
  return complete.take();
}

std::vector<std::uint8_t> encode_record(const Record &record) {
  Encoder body;
  body.put(static_cast<std::uint8_t>(record.metadata.kind));
  body.put(std::uint8_t{0});
  body.put(record.metadata.flags);
  body.put(record.metadata.wall_ns);
  body.put(record.metadata.mono_ns);
  body.put(record.metadata.ring_epoch);
  body.put(record.metadata.ring_sequence);
  body.put(record.metadata.generation);
  if (record.metadata.kind == Kind::AggBbo) {
    const auto &value = record.bbo;
    put_header(body, value.header);
    put_bbo_side(body, value.gated_bid);
    put_bbo_side(body, value.gated_ask);
    put_raw_side(body, value.raw_bid);
    put_raw_side(body, value.raw_ask);
    body.put_array(value.base_asset);
    body.put_array(value.quote_asset);
    body.put_array(value.venue_slot_ids);
    body.put(value.price_scale);
    body.put(value.quantity_scale);
    body.put(value.member_count);
    body.put(value.member_mask);
    body.put(value.live_mask);
    body.put(value.raw_cross_bps);
    body.put(value.gated_cross_bps);
    body.put(value.skew_us);
    body.put(value.cross_skew_threshold_us);
    body.put(value.fx_age_us);
    body.put(value.cross_bid_venue);
    body.put(value.cross_ask_venue);
    body.put(value.fx_venue);
    body.put(record.cross_window.start_mono_ns);
    body.put(record.cross_window.end_mono_ns);
    body.put(record.cross_window.raw_min);
    body.put(record.cross_window.raw_max);
    body.put(record.cross_window.gated_min);
    body.put(record.cross_window.gated_max);
    body.put(record.cross_window.samples);
  } else {
    const auto &value = record.order_book;
    put_header(body, value.header);
    body.put_array(value.base_asset);
    body.put_array(value.quote_asset);
    body.put_array(value.venue_slot_ids);
    body.put(value.price_scale);
    body.put(value.quantity_scale);
    body.put(value.member_count);
    body.put(value.member_mask);
    body.put(value.active_mask);
    body.put(value.bid_count);
    body.put(value.ask_count);
    for (std::size_t index = 0; index < value.bid_count; ++index) {
      put_level(body, value.bids[index]);
    }
    for (std::size_t index = 0; index < value.ask_count; ++index) {
      put_level(body, value.asks[index]);
    }
  }
  auto payload = body.take();
  Encoder frame;
  frame.put(kFrameMagic);
  frame.put(static_cast<std::uint32_t>(payload.size()));
  frame.put(crc32(payload));
  frame.append(payload);
  return frame.take();
}

std::vector<std::uint8_t> container_trailer(std::uint64_t records) {
  Encoder out;
  out.put(kTrailerMagic);
  out.put(records);
  auto bytes = out.take();
  Encoder complete;
  complete.append(bytes);
  complete.put(crc32(bytes));
  return complete.take();
}

ReadResult decode_container(std::span<const std::uint8_t> bytes,
                            const RecordVisitor &visitor) {
  constexpr std::size_t header_size = 16;
  constexpr std::size_t trailer_size = 16;
  if (bytes.size() < header_size + trailer_size) {
    return failure(ReadError::Truncated, "container is truncated");
  }
  Decoder header(bytes.first(header_size));
  std::uint32_t magic{}, endian{}, stored_crc{};
  std::uint16_t major{}, minor{};
  if (!header.get(magic) || !header.get(major) || !header.get(minor) ||
      !header.get(endian) || !header.get(stored_crc)) {
    return failure(ReadError::Truncated, "container header is truncated");
  }
  if (magic != kContainerMagic || endian != 0x01020304U) {
    return failure(ReadError::Corrupt, "invalid container header");
  }
  if (major != 1 && major != kFormatMajor) {
    return failure(ReadError::UnsupportedVersion,
                   "unsupported recording major version");
  }
  if (major == kFormatMajor && minor > kFormatMinor) {
    return failure(ReadError::UnsupportedVersion,
                   "unsupported recording minor version");
  }
  if (stored_crc != crc32(bytes.first(header_size - 4))) {
    return failure(ReadError::Corrupt, "container header CRC mismatch");
  }

  std::size_t offset = header_size;
  std::uint64_t records = 0;
  while (offset + trailer_size <= bytes.size()) {
    Decoder marker(bytes.subspan(offset));
    std::uint32_t frame_magic{};
    if (!marker.get(frame_magic)) {
      break;
    }
    if (frame_magic == kTrailerMagic) {
      std::uint64_t expected{};
      std::uint32_t trailer_crc{};
      if (!marker.get(expected) || !marker.get(trailer_crc) ||
          offset + trailer_size != bytes.size()) {
        return failure(ReadError::Corrupt, "invalid container trailer",
                       records);
      }
      if (trailer_crc != crc32(bytes.subspan(offset, trailer_size - 4))) {
        return failure(ReadError::Corrupt, "container trailer CRC mismatch",
                       records);
      }
      if (expected != records) {
        return failure(ReadError::Corrupt, "record count mismatch", records);
      }
      return {.error = ReadError::Ok, .message = {}, .records = records};
    }
    std::uint32_t length{}, frame_crc{};
    if (frame_magic != kFrameMagic || !marker.get(length) ||
        !marker.get(frame_crc)) {
      return failure(ReadError::Corrupt, "invalid record frame", records);
    }
    constexpr std::size_t frame_header = 12;
    if (length > 1U << 20U ||
        offset + frame_header + length > bytes.size()) {
      return failure(ReadError::Truncated, "record frame is truncated",
                     records);
    }
    const auto payload = bytes.subspan(offset + frame_header, length);
    if (frame_crc != crc32(payload)) {
      return failure(ReadError::Corrupt, "record frame CRC mismatch", records);
    }
    Decoder in(payload);
    Record record{};
    std::uint8_t kind{}, reserved{};
    if (!in.get(kind) || !in.get(reserved) ||
        !in.get(record.metadata.flags) || !in.get(record.metadata.wall_ns) ||
        !in.get(record.metadata.mono_ns) ||
        !in.get(record.metadata.ring_epoch) ||
        !in.get(record.metadata.ring_sequence) ||
        !in.get(record.metadata.generation) ||
        (kind != static_cast<std::uint8_t>(Kind::AggBbo) &&
         kind != static_cast<std::uint8_t>(Kind::AggOrderBook))) {
      return failure(ReadError::InvalidRecord, "invalid record metadata",
                     records);
    }
    if (reserved != 0 ||
        (record.metadata.flags & ~(kGap | kReset)) != 0 ||
        record.metadata.wall_ns == 0 || record.metadata.mono_ns == 0) {
      return failure(ReadError::InvalidRecord, "invalid record metadata",
                     records);
    }
    record.metadata.kind = static_cast<Kind>(kind);
    if (record.metadata.kind == Kind::AggBbo) {
      auto &value = record.bbo;
      if (!get_header(in, value.header, major) ||
          !get_bbo_side(in, value.gated_bid) ||
          !get_bbo_side(in, value.gated_ask) ||
          !get_raw_side(in, value.raw_bid) ||
          !get_raw_side(in, value.raw_ask) ||
          !in.get_array(value.base_asset) || !in.get_array(value.quote_asset) ||
          !in.get_array(value.venue_slot_ids) || !in.get(value.price_scale) ||
          !in.get(value.quantity_scale) || !in.get(value.member_count) ||
          !in.get(value.member_mask) || !in.get(value.live_mask) ||
          !in.get(value.raw_cross_bps) || !in.get(value.gated_cross_bps) ||
          !in.get(value.skew_us) ||
          !in.get(value.cross_skew_threshold_us) || !in.get(value.fx_age_us) ||
          !in.get(value.cross_bid_venue) || !in.get(value.cross_ask_venue) ||
          !in.get(value.fx_venue) ||
          !in.get(record.cross_window.start_mono_ns) ||
          !in.get(record.cross_window.end_mono_ns) ||
          !in.get(record.cross_window.raw_min) ||
          !in.get(record.cross_window.raw_max) ||
          !in.get(record.cross_window.gated_min) ||
          !in.get(record.cross_window.gated_max) ||
          !in.get(record.cross_window.samples) ||
          !valid_header(value.header, record.metadata.kind) ||
          !valid_bbo_semantics(value) ||
          (record.cross_window.samples != 0 &&
           (record.cross_window.start_mono_ns >
                record.cross_window.end_mono_ns ||
            record.cross_window.raw_min > record.cross_window.raw_max ||
            record.cross_window.gated_min > record.cross_window.gated_max))) {
        return failure(ReadError::InvalidRecord, "invalid AggBbo record",
                       records);
      }
    } else {
      auto &value = record.order_book;
      if (!get_header(in, value.header, major) ||
          !in.get_array(value.base_asset) || !in.get_array(value.quote_asset) ||
          !in.get_array(value.venue_slot_ids) || !in.get(value.price_scale) ||
          !in.get(value.quantity_scale) || !in.get(value.member_count) ||
          !in.get(value.member_mask) || !in.get(value.active_mask) ||
          !in.get(value.bid_count) || !in.get(value.ask_count) ||
          value.bid_count > kMaximumDepth || value.ask_count > kMaximumDepth ||
          value.member_count > utils::md::wire::kAggVenueSlots ||
          !valid_header(value.header, record.metadata.kind)) {
        return failure(ReadError::InvalidRecord,
                       "invalid AggOrderBook metadata", records);
      }
      for (std::size_t index = 0; index < value.bid_count; ++index) {
        if (!get_level(in, value.bids[index]) ||
            value.bids[index].price <= 0 || value.bids[index].quantity <= 0) {
          return failure(ReadError::InvalidRecord, "invalid bid level",
                         records);
        }
      }
      for (std::size_t index = 0; index < value.ask_count; ++index) {
        if (!get_level(in, value.asks[index]) ||
            value.asks[index].price <= 0 || value.asks[index].quantity <= 0) {
          return failure(ReadError::InvalidRecord, "invalid ask level",
                         records);
        }
      }
      if (!valid_book_semantics(value)) {
        return failure(ReadError::InvalidRecord,
                       "invalid AggOrderBook semantics", records);
      }
    }
    if (in.remaining() != 0) {
      return failure(ReadError::InvalidRecord,
                     "record has unexpected trailing fields", records);
    }
    ++records;
    if (visitor && !visitor(record)) {
      return {.error = ReadError::Ok, .message = {}, .records = records};
    }
    offset += frame_header + length;
  }
  return failure(ReadError::Truncated, "container trailer is missing", records);
}

}  // namespace mds::record::detail
