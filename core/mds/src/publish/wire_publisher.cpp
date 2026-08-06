#include "mds/publish/wire_publisher.h"

#include "utils/md/wire.h"

#include <algorithm>
#include <array>
#include <cstring>
#include <limits>
#include <utility>
#include <vector>

namespace mds::publish {
namespace {

using utils::md::MessageType;
using utils::md::wire::CodecError;
using utils::md::wire::HeaderFields;

HeaderFields header_fields(const utils::md::EventHeader &header) noexcept {
  return {.instrument_id = header.instrument_id,
          .bus_seq = 0,
          .source_seq = header.source_seq,
          .exchange_ts_ns = header.exchange_ts_ns,
          .receive_tsc = header.receive_tsc,
          .publish_tsc = header.publish_tsc,
          .book_generation = header.book_generation,
          .state = header.state,
          .source_id = header.source_id,
          .flags = 0};
}

api::ErrorCode map_codec_error(CodecError error) noexcept {
  switch (error) {
  case CodecError::Ok:
    return api::ErrorCode::Ok;
  case CodecError::BufferTooSmall:
    return api::ErrorCode::InternalError;
  case CodecError::InvalidField:
    return api::ErrorCode::InvalidConfig;
  case CodecError::BadMagic:
  case CodecError::UnsupportedSchema:
  case CodecError::UnknownMessageType:
  case CodecError::LengthMismatch:
    return api::ErrorCode::InternalError;
  }
  return api::ErrorCode::InternalError;
}

const char *codec_error_message(CodecError error) noexcept {
  switch (error) {
  case CodecError::Ok:
    return "";
  case CodecError::BufferTooSmall:
    return "wire encoder buffer is too small";
  case CodecError::BadMagic:
    return "wire encoder produced bad magic";
  case CodecError::UnsupportedSchema:
    return "wire encoder produced unsupported schema";
  case CodecError::UnknownMessageType:
    return "wire encoder produced unknown message type";
  case CodecError::LengthMismatch:
    return "wire encoder produced an invalid record length";
  case CodecError::InvalidField:
    return "market-data event contains an invalid wire field";
  }
  return "unknown wire codec error";
}

api::Result<void> void_error(const api::Result<std::uint64_t> &result) {
  return {.error = result.error, .message = result.message};
}

std::string sanitize_name_part(std::string_view value) {
  std::string result;
  result.reserve(value.size());
  for (const char raw : value) {
    const auto c = static_cast<unsigned char>(raw);
    if (c >= 'A' && c <= 'Z') {
      result.push_back(static_cast<char>(c - 'A' + 'a'));
    } else if ((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
               c == '-' || c == '_') {
      result.push_back(static_cast<char>(c));
    } else {
      result.push_back('_');
    }
  }
  return result;
}

} // namespace

WirePublisher::WirePublisher(transport::SharedRing &ring) noexcept
    : ring_(&ring), observed_registry_generation_(ring.registry_generation()) {}

WirePublisher::WirePublisher(transport::SharedRing &&ring) noexcept
    : owned_ring_(std::move(ring)), ring_(&*owned_ring_),
      observed_registry_generation_(ring_->registry_generation()) {}

WirePublisher::WirePublisher(WirePublisher &&other) noexcept {
  *this = std::move(other);
}

WirePublisher &WirePublisher::operator=(WirePublisher &&other) noexcept {
  if (this == &other) {
    return *this;
  }
  const bool owns_ring =
      other.owned_ring_.has_value() && other.ring_ == &*other.owned_ring_;
  auto *borrowed_ring = owns_ring ? nullptr : other.ring_;
  owned_ring_ = std::move(other.owned_ring_);
  ring_ = owns_ring ? &*owned_ring_ : borrowed_ring;
  current_bus_seq_ = other.current_bus_seq_;
  bus_seq_exhausted_ = other.bus_seq_exhausted_;
  observed_registry_generation_ = other.observed_registry_generation_;
  reader_change_context_ = other.reader_change_context_;
  reader_change_hook_ = other.reader_change_hook_;
  other.ring_ = nullptr;
  other.reader_change_context_ = nullptr;
  other.reader_change_hook_ = nullptr;
  return *this;
}

std::uint64_t WirePublisher::producer_epoch() const noexcept {
  return ring_ ? ring_->epoch() : 0;
}

std::string_view WirePublisher::segment_name() const noexcept {
  return ring_ ? ring_->name() : std::string_view{};
}

std::uint32_t WirePublisher::reader_registry_generation() const noexcept {
  return ring_ ? ring_->registry_generation() : 0;
}

std::size_t WirePublisher::reclaim_stale_readers(
    std::uint64_t now_ns, std::uint64_t lease_timeout_ns) noexcept {
  return ring_ ? ring_->reclaim_stale(now_ns, lease_timeout_ns,
                                     &transport::process_identity_alive)
               : 0;
}

void WirePublisher::set_reader_change_hook(void *context,
                                           ReaderChangeHook hook) noexcept {
  reader_change_context_ = context;
  reader_change_hook_ = hook;
  observed_registry_generation_ = reader_registry_generation();
}

bool WirePublisher::poll_reader_change() noexcept {
  const auto generation = reader_registry_generation();
  if (generation == observed_registry_generation_) {
    return false;
  }
  observed_registry_generation_ = generation;
  if (reader_change_hook_) {
    reader_change_hook_(reader_change_context_, *this);
  }
  return true;
}

bool WirePublisher::assign_bus_seq(
    const utils::md::EventHeader &header, HeaderFields &fields) noexcept {
  if (bus_seq_exhausted_) {
    return false;
  }
  fields = header_fields(header);
  if (current_bus_seq_ == std::numeric_limits<std::uint64_t>::max()) {
    bus_seq_exhausted_ = true;
    return false;
  }
  ++current_bus_seq_;
  fields.bus_seq = current_bus_seq_;
  return true;
}

api::Result<std::uint64_t> WirePublisher::publish_encoded(
    MessageType type, const utils::md::wire::EncodeResult &encoded,
    std::span<const std::byte> bytes) noexcept {
  if (!ring_) {
    return {.error = api::ErrorCode::NotInitialized,
            .message = "wire publisher has no shared ring"};
  }
  if (!encoded) {
    return {.error = map_codec_error(encoded.error),
            .message = codec_error_message(encoded.error)};
  }
  if (encoded.size > bytes.size()) {
    return {.error = api::ErrorCode::InternalError,
            .message = "wire encoder returned an invalid size"};
  }
  const auto payload = bytes.first(encoded.size);
  utils::md::wire::RecordHeader inner{};
  const auto validated = utils::md::wire::ValidateHeader(payload, &inner);
  if (validated != CodecError::Ok) {
    return {.error = map_codec_error(validated),
            .message = codec_error_message(validated)};
  }
  const auto outer_type = static_cast<std::uint32_t>(type);
  if (inner.message_type != static_cast<std::uint16_t>(type)) {
    return {.error = api::ErrorCode::InternalError,
            .message = "outer and inner wire message types differ"};
  }
  return ring_->publish(outer_type, payload);
}

api::Result<std::uint64_t> WirePublisher::publish_instrument(
    const utils::md::EventHeader &header,
    const utils::md::Instrument &instrument) noexcept {
  std::array<std::byte, sizeof(utils::md::wire::InstrumentUpdateRecord)> bytes{};
  HeaderFields fields;
  if (!assign_bus_seq(header, fields)) {
    return {.error = api::ErrorCode::QuotaExceeded,
            .message = "wire bus sequence is exhausted"};
  }
  const auto encoded = utils::md::wire::EncodeInstrument(
      bytes, fields, instrument);
  return publish_encoded(MessageType::InstrumentUpdate, encoded, bytes);
}

api::Result<std::uint64_t>
WirePublisher::publish_bbo(const utils::md::BboEvent &event) noexcept {
  std::array<std::byte, sizeof(utils::md::wire::BboRecord)> bytes{};
  HeaderFields fields;
  if (!assign_bus_seq(event.header, fields)) {
    return {.error = api::ErrorCode::QuotaExceeded,
            .message = "wire bus sequence is exhausted"};
  }
  const auto encoded = utils::md::wire::EncodeBbo(
      bytes, fields, event.bid, event.ask);
  return publish_encoded(MessageType::Bbo, encoded, bytes);
}

api::Result<std::uint64_t>
WirePublisher::publish_ticker(const utils::md::TickerEvent &event) noexcept {
  std::array<std::byte, sizeof(utils::md::wire::TickerRecord)> bytes{};
  HeaderFields fields;
  if (!assign_bus_seq(event.header, fields)) {
    return {.error = api::ErrorCode::QuotaExceeded,
            .message = "wire bus sequence is exhausted"};
  }
  const auto encoded =
      utils::md::wire::EncodeTicker(bytes, fields, event);
  return publish_encoded(MessageType::Ticker, encoded, bytes);
}

api::Result<std::uint64_t>
WirePublisher::publish_delta(const utils::md::BookDelta &event) noexcept {
  std::array<std::byte, sizeof(utils::md::wire::DeltaRecord)> bytes{};
  HeaderFields fields;
  if (!assign_bus_seq(event.header, fields)) {
    return {.error = api::ErrorCode::QuotaExceeded,
            .message = "wire bus sequence is exhausted"};
  }
  const auto encoded = utils::md::wire::EncodeDelta(
      bytes, fields, event.side, event.level);
  return publish_encoded(MessageType::BookDelta, encoded, bytes);
}

api::Result<std::uint64_t> WirePublisher::publish_snapshot_begin(
    const utils::md::EventHeader &header,
    std::uint32_t level_count, std::uint32_t chunk_count) noexcept {
  std::array<std::byte, sizeof(utils::md::wire::SnapshotBeginRecord)> bytes{};
  HeaderFields fields;
  if (!assign_bus_seq(header, fields)) {
    return {.error = api::ErrorCode::QuotaExceeded,
            .message = "wire bus sequence is exhausted"};
  }
  const auto encoded = utils::md::wire::EncodeSnapshotBegin(
      bytes, fields, level_count, chunk_count);
  return publish_encoded(MessageType::SnapshotBegin, encoded, bytes);
}

api::Result<std::uint64_t> WirePublisher::publish_snapshot_chunk(
    const utils::md::EventHeader &header, std::uint32_t chunk_index,
    utils::md::Side side,
    std::span<const utils::md::Level> levels) noexcept {
  std::array<std::byte, sizeof(utils::md::wire::SnapshotChunkRecord)> bytes{};
  HeaderFields fields;
  if (!assign_bus_seq(header, fields)) {
    return {.error = api::ErrorCode::QuotaExceeded,
            .message = "wire bus sequence is exhausted"};
  }
  const auto encoded = utils::md::wire::EncodeSnapshotChunk(
      bytes, fields, chunk_index, side, levels);
  return publish_encoded(MessageType::SnapshotChunk, encoded, bytes);
}

api::Result<std::uint64_t> WirePublisher::publish_snapshot_end(
    const utils::md::EventHeader &header, std::uint32_t received_levels,
    std::uint32_t checksum) noexcept {
  std::array<std::byte, sizeof(utils::md::wire::SnapshotEndRecord)> bytes{};
  HeaderFields fields;
  if (!assign_bus_seq(header, fields)) {
    return {.error = api::ErrorCode::QuotaExceeded,
            .message = "wire bus sequence is exhausted"};
  }
  const auto encoded = utils::md::wire::EncodeSnapshotEnd(
      bytes, fields, received_levels, checksum);
  return publish_encoded(MessageType::SnapshotEnd, encoded, bytes);
}

api::Result<void> WirePublisher::publish_snapshot(
    const utils::md::EventHeader &header, utils::md::Side side,
    std::span<const utils::md::Level> levels,
    std::uint32_t checksum) noexcept {
  if (side != utils::md::Side::Bid && side != utils::md::Side::Ask) {
    return {.error = api::ErrorCode::InvalidConfig,
            .message = "snapshot side is invalid"};
  }
  return side == utils::md::Side::Bid
             ? publish_snapshot(header, levels, {}, checksum)
             : publish_snapshot(header, {}, levels, checksum);
}

api::Result<void> WirePublisher::publish_snapshot(
    const utils::md::EventHeader &header,
    std::span<const utils::md::Level> bids,
    std::span<const utils::md::Level> asks,
    std::uint32_t checksum) noexcept {
  if (bids.size() > std::numeric_limits<std::uint32_t>::max() ||
      asks.size() > std::numeric_limits<std::uint32_t>::max() ||
      bids.size() + asks.size() >
          std::numeric_limits<std::uint32_t>::max()) {
    return {.error = api::ErrorCode::InvalidConfig,
            .message = "snapshot contains too many levels"};
  }
  const auto chunk_count = [](std::size_t size) {
    return size / utils::md::kSnapshotLevelsPerChunk +
           (size % utils::md::kSnapshotLevelsPerChunk != 0U ? 1U : 0U);
  };
  const auto total_chunks = chunk_count(bids.size()) + chunk_count(asks.size());
  if (total_chunks > std::numeric_limits<std::uint32_t>::max()) {
    return {.error = api::ErrorCode::InvalidConfig,
            .message = "snapshot contains too many chunks"};
  }
  const auto total_levels =
      static_cast<std::uint32_t>(bids.size() + asks.size());
  auto published = publish_snapshot_begin(
      header, total_levels, static_cast<std::uint32_t>(total_chunks));
  if (!published) {
    return void_error(published);
  }
  const auto publish_side =
      [&](utils::md::Side side,
          std::span<const utils::md::Level> levels) -> api::Result<void> {
    for (std::size_t offset = 0, chunk_index = 0; offset < levels.size();
         offset += utils::md::kSnapshotLevelsPerChunk, ++chunk_index) {
      const auto count =
          std::min(utils::md::kSnapshotLevelsPerChunk, levels.size() - offset);
      const auto result = publish_snapshot_chunk(
          header, static_cast<std::uint32_t>(chunk_index), side,
          levels.subspan(offset, count));
      if (!result) {
        return void_error(result);
      }
    }
    return {};
  };
  auto side_result = publish_side(utils::md::Side::Bid, bids);
  if (!side_result) {
    return side_result;
  }
  side_result = publish_side(utils::md::Side::Ask, asks);
  if (!side_result) {
    return side_result;
  }
  published = publish_snapshot_end(header, total_levels, checksum);
  return published ? api::Result<void>{} : void_error(published);
}

api::Result<void>
WirePublisher::publish_snapshot(const utils::md::EventHeader &header,
                                const utils::md::OrderBook &book,
                                std::uint32_t checksum) {
  std::vector<utils::md::Level> bids;
  std::vector<utils::md::Level> asks;
  bids.reserve(book.bids().capacity());
  asks.reserve(book.asks().capacity());
  for (std::size_t index = book.bids().capacity(); index > 0; --index) {
    if (const auto level = book.bids().At(index - 1)) {
      bids.push_back(*level);
    }
  }
  for (std::size_t index = 0; index < book.asks().capacity(); ++index) {
    if (const auto level = book.asks().At(index)) {
      asks.push_back(*level);
    }
  }
  return publish_snapshot(header, bids, asks, checksum);
}

TickerOrderBookPublishers::TickerOrderBookPublishers(
    WirePublisher &&ticker, WirePublisher &&order_book) noexcept
    : ticker_(std::move(ticker)), order_book_(std::move(order_book)) {}

api::Result<TickerOrderBookPublishers> TickerOrderBookPublishers::open(
    std::string_view profile, std::string_view symbol,
    const transport::RingOptions &options) {
  auto ticker_options = options;
  ticker_options.name =
      make_publisher_segment_name(profile, symbol, "ticker");
  auto order_book_options = options;
  order_book_options.name =
      make_publisher_segment_name(profile, symbol, "orderbook");
  if (ticker_options.name.empty() || order_book_options.name.empty()) {
    return {.value = {},
            .error = api::ErrorCode::InvalidConfig,
            .message = "profile and symbol must produce valid segment names"};
  }

  auto ticker_ring = transport::SharedRing::open(ticker_options);
  if (!ticker_ring) {
    return {.value = {},
            .error = ticker_ring.error,
            .message = std::move(ticker_ring.message)};
  }
  auto order_book_ring = transport::SharedRing::open(order_book_options);
  if (!order_book_ring) {
    return {.value = {},
            .error = order_book_ring.error,
            .message = std::move(order_book_ring.message)};
  }
  return {.value = TickerOrderBookPublishers(
              WirePublisher(std::move(ticker_ring.value)),
              WirePublisher(std::move(order_book_ring.value)))};
}

std::string make_publisher_segment_name(std::string_view profile,
                                        std::string_view symbol,
                                        std::string_view stream) {
  return make_publisher_segment_name("/selfquant.mds", profile, symbol, stream);
}

std::string make_publisher_segment_name(std::string_view prefix,
                                        std::string_view profile,
                                        std::string_view symbol,
                                        std::string_view stream) {
  if (prefix.empty() || prefix.front() != '/' || profile.empty() ||
      symbol.empty() || stream.empty()) {
    return {};
  }
  while (prefix.size() > 1 && prefix.back() == '.') {
    prefix.remove_suffix(1);
  }
  if (prefix == "/") {
    return {};
  }
  const auto canonical_stream = sanitize_name_part(stream);
  if (canonical_stream != "ticker" && canonical_stream != "orderbook") {
    return {};
  }
  auto name = std::string(prefix) + "." + sanitize_name_part(profile) + "." +
              sanitize_name_part(symbol) + "." + canonical_stream + "." +
              std::to_string(utils::md::wire::kSchemaMajor);
  return name.size() <= 240 ? name : std::string{};
}

} // namespace mds::publish
