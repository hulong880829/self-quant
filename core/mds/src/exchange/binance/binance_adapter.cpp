#include "mds/exchange/binance/binance_adapter.h"

#include <bit>
#include <charconv>
#include <cstring>
#include <limits>
#include <type_traits>

#ifdef MDS_HAS_SIMDJSON
#include <simdjson.h>
#endif

namespace mds::exchange::binance {
namespace {

bool decimal_to_fixed(std::string_view text, std::int8_t scale,
                      std::int64_t &out) noexcept {
  bool negative = false;
  std::size_t position = 0;
  if (!text.empty() && text.front() == '-') {
    negative = true;
    position = 1;
  }
  std::int64_t value = 0;
  std::int8_t fractional = -1;
  for (; position < text.size(); ++position) {
    const char c = text[position];
    if (c == '.') {
      if (fractional >= 0) {
        return false;
      }
      fractional = 0;
      continue;
    }
    if (c < '0' || c > '9' || value > (INT64_MAX - 9) / 10) {
      return false;
    }
    if (fractional >= scale) {
      if (c != '0') {
        return false;
      }
      continue;
    }
    value = value * 10 + (c - '0');
    if (fractional >= 0) {
      ++fractional;
    }
  }
  if (fractional < 0) {
    fractional = 0;
  }
  while (fractional++ < scale) {
    if (value > INT64_MAX / 10) {
      return false;
    }
    value *= 10;
  }
  out = negative ? -value : value;
  return true;
}

template <std::size_t Capacity>
bool copy_symbol(std::string_view symbol, std::array<char, Capacity> &out,
                 std::string &error) {
  if (symbol.empty() || symbol.size() >= Capacity) {
    error = "Binance symbol is empty or exceeds fixed capacity";
    return false;
  }
  out.fill('\0');
  std::memcpy(out.data(), symbol.data(), symbol.size());
  return true;
}

template <typename T>
bool read_little_endian(std::span<const std::byte> message,
                        std::size_t offset, T &out) noexcept {
  static_assert(std::is_integral_v<T>);
  using Unsigned = std::make_unsigned_t<T>;
  if (offset > message.size() || sizeof(T) > message.size() - offset) {
    return false;
  }
  Unsigned value = 0;
  for (std::size_t index = 0; index < sizeof(T); ++index) {
    value |= static_cast<Unsigned>(
                 std::to_integer<unsigned char>(message[offset + index]))
             << (index * 8U);
  }
  if constexpr (std::is_signed_v<T>) {
    out = std::bit_cast<T>(value);
  } else {
    out = value;
  }
  return true;
}

struct SbeHeader {
  std::uint16_t block_length{};
  std::uint16_t template_id{};
  std::uint16_t schema_id{};
  std::uint16_t version{};
};

bool read_sbe_header(std::span<const std::byte> message, SbeHeader &header,
                     std::string &error) {
  if (!read_little_endian(message, 0, header.block_length) ||
      !read_little_endian(message, 2, header.template_id) ||
      !read_little_endian(message, 4, header.schema_id) ||
      !read_little_endian(message, 6, header.version)) {
    error = "truncated SBE message header";
    return false;
  }
  if (header.schema_id != SpotSbeDecoder::schema_id ||
      header.version != SpotSbeDecoder::schema_version) {
    error = "unsupported Binance stream SBE schema/version";
    return false;
  }
  return true;
}

} // namespace

Capability capability(Profile profile) noexcept {
  if (profile == Profile::Spot) {
    return {profile,
            "stream.binance.com",
            "api.binance.com",
            "/stream?streams=",
            "/api/v3/exchangeInfo",
            "/api/v3/depth",
            true,
            true,
            true,
            Availability::Available,
            false};
  }
  return {profile,
          "fstream.binance.com",
          "fapi.binance.com",
          "/stream?streams=",
          "/fapi/v1/exchangeInfo",
          "/fapi/v1/depth",
          true,
          true,
          true,
          Availability::Unavailable,
          true};
}

#ifdef MDS_HAS_SIMDJSON
struct JsonParser::Impl {
  simdjson::ondemand::parser parser{};
  std::vector<char> buffer;
  explicit Impl(std::size_t capacity = 1U << 20U)
      : buffer(capacity + simdjson::SIMDJSON_PADDING) {}

  simdjson::ondemand::document parse(std::string_view json) {
    if (json.size() + simdjson::SIMDJSON_PADDING > buffer.size()) {
      throw simdjson::simdjson_error(simdjson::CAPACITY);
    }
    std::memcpy(buffer.data(), json.data(), json.size());
    std::memset(buffer.data() + json.size(), 0, simdjson::SIMDJSON_PADDING);
    return parser.iterate(buffer.data(), json.size(), buffer.size());
  }
};
#endif

JsonParser::JsonParser(std::int8_t price_scale, std::int8_t quantity_scale)
    : price_scale_(price_scale), quantity_scale_(quantity_scale)
#ifdef MDS_HAS_SIMDJSON
      ,
      impl_(new Impl())
#endif
{
}

JsonParser::~JsonParser() {
#ifdef MDS_HAS_SIMDJSON
  delete impl_;
#endif
}

bool JsonParser::parse_book_ticker(std::string_view json, BookTicker &out,
                                   std::string &error) {
#ifndef MDS_HAS_SIMDJSON
  (void)json;
  (void)out;
  error = "simdjson support was not compiled";
  return false;
#else
  try {
    auto document = impl_->parse(json);
    out.update_id = std::uint64_t(document["u"]);
    const auto bid = std::string_view(document["b"].get_string().value());
    if (!decimal_to_fixed(bid, price_scale_, out.bid_price)) {
      throw simdjson::simdjson_error(simdjson::NUMBER_ERROR);
    }
    const auto bid_qty = std::string_view(document["B"].get_string().value());
    const auto ask = std::string_view(document["a"].get_string().value());
    const auto ask_qty = std::string_view(document["A"].get_string().value());
    if (!decimal_to_fixed(bid_qty, quantity_scale_, out.bid_quantity) ||
        !decimal_to_fixed(ask, price_scale_, out.ask_price) ||
        !decimal_to_fixed(ask_qty, quantity_scale_, out.ask_quantity)) {
      throw simdjson::simdjson_error(simdjson::NUMBER_ERROR);
    }
    auto event_time = document["E"].get_uint64();
    out.event_time_ms = event_time.error() ? 0 : event_time.value();
    auto transaction_time = document["T"].get_uint64();
    out.transaction_time_ms =
        transaction_time.error() ? 0 : transaction_time.value();
    const auto symbol = std::string_view(document["s"].get_string().value());
    if (!copy_symbol(symbol, out.symbol, error)) {
      return false;
    }
    out.price_exponent = static_cast<std::int8_t>(-price_scale_);
    out.quantity_exponent = static_cast<std::int8_t>(-quantity_scale_);
    return true;
  } catch (const simdjson::simdjson_error &exception) {
    error = exception.what();
    return false;
  }
#endif
}

bool JsonParser::parse_depth(std::string_view json, DepthUpdate &out,
                             std::string &error) {
#ifndef MDS_HAS_SIMDJSON
  (void)json;
  (void)out;
  error = "simdjson support was not compiled";
  return false;
#else
  try {
    auto document = impl_->parse(json);
    out.first_update_id = std::uint64_t(document["U"]);
    out.final_update_id = std::uint64_t(document["u"]);
    auto previous = document["pu"].get_uint64();
    out.previous_final_update_id = previous.error() ? 0 : previous.value();
    auto event_time = document["E"].get_uint64();
    out.event_time_ms = event_time.error() ? 0 : event_time.value();
    auto transaction_time = document["T"].get_uint64();
    out.transaction_time_ms =
        transaction_time.error() ? 0 : transaction_time.value();
    out.price_exponent = static_cast<std::int8_t>(-price_scale_);
    out.quantity_exponent = static_cast<std::int8_t>(-quantity_scale_);
    const auto symbol = std::string_view(document["s"].get_string().value());
    if (!copy_symbol(symbol, out.symbol, error)) {
      return false;
    }
    out.bids.clear();
    out.asks.clear();
    for (auto level : document["b"].get_array()) {
      auto values = level.get_array();
      auto iterator = values.begin();
      const auto price = std::string_view((*iterator).get_string().value());
      ++iterator;
      const auto quantity =
          std::string_view((*iterator).get_string().value());
      PriceLevel parsed{};
      if (!decimal_to_fixed(price, price_scale_, parsed.price) ||
          !decimal_to_fixed(quantity, quantity_scale_, parsed.quantity)) {
        throw simdjson::simdjson_error(simdjson::NUMBER_ERROR);
      }
      out.bids.push_back(parsed);
    }
    for (auto level : document["a"].get_array()) {
      auto values = level.get_array();
      auto iterator = values.begin();
      const auto price = std::string_view((*iterator).get_string().value());
      ++iterator;
      const auto quantity =
          std::string_view((*iterator).get_string().value());
      PriceLevel parsed{};
      if (!decimal_to_fixed(price, price_scale_, parsed.price) ||
          !decimal_to_fixed(quantity, quantity_scale_, parsed.quantity)) {
        throw simdjson::simdjson_error(simdjson::NUMBER_ERROR);
      }
      out.asks.push_back(parsed);
    }
    return true;
  } catch (const simdjson::simdjson_error &exception) {
    error = exception.what();
    return false;
  }
#endif
}

bool SpotSbeDecoder::decode_book_ticker(
    std::span<const std::byte> message, BookTicker &out,
    std::string &error) const noexcept {
  // Layout is generated directly from Binance's official
  // sbe/schemas/stream_1_0.xml (schema 1, version 0). SBE fields are packed;
  // no native C++ struct is overlaid on the wire.
  constexpr std::size_t kHeaderBytes = 8;
  constexpr std::uint16_t kRootBlockLength = 50;
  SbeHeader header;
  if (!read_sbe_header(message, header, error)) {
    return false;
  }
  if (header.template_id != best_bid_ask_template_id ||
      header.block_length < kRootBlockLength ||
      message.size() < kHeaderBytes + header.block_length + 1U) {
    error = "invalid BestBidAskStreamEvent header or block length";
    return false;
  }

  const std::size_t root = kHeaderBytes;
  std::int64_t event_time_us{};
  std::int64_t update_id{};
  if (!read_little_endian(message, root, event_time_us) ||
      !read_little_endian(message, root + 8U, update_id) ||
      !read_little_endian(message, root + 16U, out.price_exponent) ||
      !read_little_endian(message, root + 17U, out.quantity_exponent) ||
      !read_little_endian(message, root + 18U, out.bid_price) ||
      !read_little_endian(message, root + 26U, out.bid_quantity) ||
      !read_little_endian(message, root + 34U, out.ask_price) ||
      !read_little_endian(message, root + 42U, out.ask_quantity) ||
      event_time_us < 0 || update_id < 0) {
    error = "invalid BestBidAskStreamEvent payload";
    return false;
  }

  const std::size_t symbol_length_offset = root + header.block_length;
  const auto symbol_length = std::to_integer<std::uint8_t>(
      message[symbol_length_offset]);
  if (static_cast<std::size_t>(symbol_length) >
      message.size() - symbol_length_offset - 1U) {
    error = "truncated BestBidAskStreamEvent symbol";
    return false;
  }
  const std::string_view symbol{
      reinterpret_cast<const char *>(message.data() + symbol_length_offset + 1U),
      symbol_length};
  if (!copy_symbol(symbol, out.symbol, error)) {
    return false;
  }

  out.update_id = static_cast<std::uint64_t>(update_id);
  out.event_time_ms = static_cast<std::uint64_t>(event_time_us) / 1000U;
  out.transaction_time_ms = 0;
  error.clear();
  return true;
}

bool SpotSbeDecoder::decode_depth(std::span<const std::byte> message,
                                  DepthUpdate &out,
                                  std::string &error) const {
  constexpr std::size_t kHeaderBytes = 8;
  constexpr std::uint16_t kRootBlockLength = 26;
  constexpr std::size_t kGroupDimensionsBytes = 4;
  constexpr std::uint16_t kLevelBlockLength = 16;
  constexpr std::uint16_t kMaxLevelsPerSide = 5000;

  SbeHeader header;
  if (!read_sbe_header(message, header, error)) {
    return false;
  }
  if (header.template_id != depth_diff_template_id ||
      header.block_length < kRootBlockLength ||
      message.size() < kHeaderBytes + header.block_length) {
    error = "invalid DepthDiffStreamEvent header or block length";
    return false;
  }

  const std::size_t root = kHeaderBytes;
  std::int64_t event_time_us{};
  std::int64_t first_update_id{};
  std::int64_t final_update_id{};
  std::int8_t price_exponent{};
  std::int8_t quantity_exponent{};
  if (!read_little_endian(message, root, event_time_us) ||
      !read_little_endian(message, root + 8U, first_update_id) ||
      !read_little_endian(message, root + 16U, final_update_id) ||
      !read_little_endian(message, root + 24U, price_exponent) ||
      !read_little_endian(message, root + 25U, quantity_exponent) ||
      event_time_us < 0 || first_update_id < 0 || final_update_id < 0 ||
      first_update_id > final_update_id) {
    error = "invalid DepthDiffStreamEvent root payload";
    return false;
  }

  std::size_t cursor = root + header.block_length;
  auto decode_group = [&](std::vector<PriceLevel> &levels) -> bool {
    std::uint16_t block_length{};
    std::uint16_t count{};
    if (!read_little_endian(message, cursor, block_length) ||
        !read_little_endian(message, cursor + 2U, count) ||
        block_length < kLevelBlockLength || count > kMaxLevelsPerSide) {
      error = "invalid SBE depth group dimensions";
      return false;
    }
    cursor += kGroupDimensionsBytes;
    const auto group_bytes =
        static_cast<std::size_t>(block_length) * static_cast<std::size_t>(count);
    if (cursor > message.size() || group_bytes > message.size() - cursor) {
      error = "truncated SBE depth group";
      return false;
    }
    levels.clear();
    levels.reserve(count);
    for (std::uint16_t index = 0; index < count; ++index) {
      PriceLevel level;
      const auto offset =
          cursor + static_cast<std::size_t>(index) * block_length;
      if (!read_little_endian(message, offset, level.price) ||
          !read_little_endian(message, offset + 8U, level.quantity)) {
        error = "truncated SBE depth level";
        return false;
      }
      levels.push_back(level);
    }
    cursor += group_bytes;
    return true;
  };

  if (!decode_group(out.bids) || !decode_group(out.asks) ||
      cursor >= message.size()) {
    if (error.empty()) {
      error = "missing DepthDiffStreamEvent symbol";
    }
    return false;
  }
  const auto symbol_length =
      std::to_integer<std::uint8_t>(message[cursor++]);
  if (static_cast<std::size_t>(symbol_length) > message.size() - cursor) {
    error = "truncated DepthDiffStreamEvent symbol";
    return false;
  }
  const std::string_view symbol{
      reinterpret_cast<const char *>(message.data() + cursor), symbol_length};
  if (!copy_symbol(symbol, out.symbol, error)) {
    return false;
  }

  out.first_update_id = static_cast<std::uint64_t>(first_update_id);
  out.final_update_id = static_cast<std::uint64_t>(final_update_id);
  out.previous_final_update_id = 0;
  out.event_time_ms = static_cast<std::uint64_t>(event_time_us) / 1000U;
  out.transaction_time_ms = 0;
  out.price_exponent = price_exponent;
  out.quantity_exponent = quantity_exponent;
  error.clear();
  return true;
}

void DepthSynchronizer::reset() noexcept {
  state_ = BookSyncState::WaitingSnapshot;
  last_update_id_ = 0;
  buffered_.clear();
}

void DepthSynchronizer::inject_snapshot(
    std::uint64_t last_update_id) noexcept {
  last_update_id_ = last_update_id;
  state_ = BookSyncState::Bridging;
}

SyncAction DepthSynchronizer::drain_buffered(
    void *context, ApplyBuffered apply) noexcept {
  if (state_ != BookSyncState::Bridging || apply == nullptr) {
    state_ = BookSyncState::Invalid;
    buffered_.clear();
    return SyncAction::Resnapshot;
  }
  bool applied = false;
  while (!buffered_.empty()) {
    const auto &update = buffered_.front();
    if (update.first_update_id > update.final_update_id) {
      state_ = BookSyncState::Invalid;
      buffered_.clear();
      return SyncAction::Resnapshot;
    }
    const bool stale =
        profile_ == Profile::Spot
            ? update.final_update_id <= last_update_id_
            : update.final_update_id < last_update_id_;
    if (stale) {
      buffered_.pop_front();
      continue;
    }
    const bool valid = applied ? continuous(update) : bridge(update);
    if (!valid || !apply(context, update)) {
      state_ = BookSyncState::Invalid;
      buffered_.clear();
      return SyncAction::Resnapshot;
    }
    last_update_id_ = update.final_update_id;
    applied = true;
    buffered_.pop_front();
  }
  if (!applied) {
    return SyncAction::Buffer;
  }
  state_ = BookSyncState::Live;
  return SyncAction::BecameLive;
}

SyncAction DepthSynchronizer::on_update(
    const DepthUpdate &update) noexcept {
  if (update.first_update_id > update.final_update_id) {
    state_ = BookSyncState::Invalid;
    buffered_.clear();
    return SyncAction::Resnapshot;
  }
  if (state_ == BookSyncState::WaitingSnapshot) {
    if (buffered_.size() >= max_buffered_updates_) {
      state_ = BookSyncState::Invalid;
      buffered_.clear();
      return SyncAction::Resnapshot;
    }
    buffered_.push_back(update);
    return SyncAction::Buffer;
  }
  if (state_ == BookSyncState::Invalid) {
    return SyncAction::Resnapshot;
  }
  const bool stale =
      state_ == BookSyncState::Bridging && profile_ == Profile::UsdM
          ? update.final_update_id < last_update_id_
          : update.final_update_id <= last_update_id_;
  if (stale) {
    return SyncAction::Drop;
  }
  if (state_ == BookSyncState::Bridging) {
    if (!bridge(update)) {
      state_ = BookSyncState::Invalid;
      return SyncAction::Resnapshot;
    }
    last_update_id_ = update.final_update_id;
    state_ = BookSyncState::Live;
    return SyncAction::BecameLive;
  }

  if (!continuous(update)) {
    state_ = BookSyncState::Invalid;
    return SyncAction::Resnapshot;
  }
  last_update_id_ = update.final_update_id;
  return SyncAction::Apply;
}

bool DepthSynchronizer::bridge(const DepthUpdate &update) noexcept {
  if (profile_ == Profile::Spot &&
      last_update_id_ == std::numeric_limits<std::uint64_t>::max()) {
    return false;
  }
  const auto target =
      profile_ == Profile::Spot ? last_update_id_ + 1 : last_update_id_;
  return update.first_update_id <= target &&
         update.final_update_id >= target;
}

bool DepthSynchronizer::continuous(const DepthUpdate &update) const noexcept {
  if (profile_ == Profile::Spot) {
    if (last_update_id_ == std::numeric_limits<std::uint64_t>::max()) {
      return false;
    }
    const auto next = last_update_id_ + 1;
    return update.first_update_id <= next && update.final_update_id >= next;
  }
  return update.previous_final_update_id == last_update_id_;
}

bool ConnectionHealth::pong_due(std::uint64_t now_ms,
                                std::uint64_t timeout_ms) const noexcept {
  return last_ping_ms_ != 0 && last_pong_ms_ < last_ping_ms_ &&
         now_ms - last_ping_ms_ >= timeout_ms;
}

bool ConnectionHealth::rotation_due(std::uint64_t now_ms) const noexcept {
  constexpr std::uint64_t kRotateBefore24HoursMs =
      23ULL * 60ULL * 60ULL * 1000ULL + 50ULL * 60ULL * 1000ULL;
  return now_ms - opened_ms_ >= kRotateBefore24HoursMs;
}

} // namespace mds::exchange::binance
