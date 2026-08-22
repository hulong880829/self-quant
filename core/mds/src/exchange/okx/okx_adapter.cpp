#include "mds/exchange/okx/okx_adapter.h"

#include <array>
#include <charconv>
#include <cstdint>
#include <cstring>
#include <limits>
#include <string>
#include <utility>
#include <vector>

#ifdef MDS_HAS_SIMDJSON
#include <simdjson.h>
#endif

namespace mds::exchange {
namespace {

constexpr std::size_t kMaximumSubscriptionArguments = 64;
constexpr std::size_t kJsonCapacity = 8U << 20U;
__extension__ typedef __int128 Int128;

bool expected_symbol(utils::md::ProductType product,
                     std::string_view canonical_symbol,
                     std::string_view supplied_venue_symbol,
                     std::string_view &venue_symbol, std::string &error) {
  if (canonical_symbol.empty() || supplied_venue_symbol.empty() ||
      canonical_symbol.size() > 32 || supplied_venue_symbol.size() > 32) {
    error = "invalid OKX canonical or venue symbol";
    return false;
  }
  constexpr std::string_view swap_suffix = "-SWAP";
  const bool is_swap =
      supplied_venue_symbol.size() > swap_suffix.size() &&
      supplied_venue_symbol.ends_with(swap_suffix);
  if ((product == utils::md::ProductType::Spot && is_swap) ||
      (product == utils::md::ProductType::Perpetual && !is_swap)) {
    error = "OKX venue symbol does not match product type";
    return false;
  }
  venue_symbol = supplied_venue_symbol;
  return true;
}

bool append_argument(std::string_view channel, std::string_view symbol,
                     std::string &batch, std::size_t &argument_count,
                     std::vector<std::string> &batches) {
  if (argument_count == kMaximumSubscriptionArguments) {
    batch += "]}";
    batches.push_back(std::move(batch));
    batch = R"({"op":"subscribe","args":[)";
    argument_count = 0;
  }
  if (argument_count != 0) {
    batch.push_back(',');
  }
  batch += R"({"channel":")";
  batch.append(channel);
  batch += R"(","instId":")";
  batch.append(symbol);
  batch += R"("})";
  ++argument_count;
  return true;
}

bool parse_unsigned_text(std::string_view text, std::uint64_t &value) noexcept {
  if (text.empty()) {
    return false;
  }
  const auto result =
      std::from_chars(text.data(), text.data() + text.size(), value);
  return result.ec == std::errc{} &&
         result.ptr == text.data() + text.size();
}

bool parse_signed_text(std::string_view text, std::int64_t &value) noexcept {
  if (text.empty()) {
    return false;
  }
  const auto result =
      std::from_chars(text.data(), text.data() + text.size(), value);
  return result.ec == std::errc{} &&
         result.ptr == text.data() + text.size();
}

bool local_decimal_scale(std::string_view text,
                         std::uint8_t &scale) noexcept {
  if (text.empty() || text.front() == '-' || text.front() == '+') {
    return false;
  }
  const auto dot = text.find('.');
  if ((dot != std::string_view::npos &&
       text.find('.', dot + 1) != std::string_view::npos) ||
      dot == 0 || (!text.empty() && text.back() == '.')) {
    return false;
  }
  for (const char character : text) {
    if (character != '.' && (character < '0' || character > '9')) {
      return false;
    }
  }
  std::size_t end = text.size();
  while (dot != std::string_view::npos && end > dot + 1 &&
         text[end - 1] == '0') {
    --end;
  }
  const auto digits =
      dot == std::string_view::npos ? 0U : end - dot - 1U;
  if (digits > 18U) {
    return false;
  }
  scale = static_cast<std::uint8_t>(digits);
  return true;
}

bool local_decimal_to_fixed(std::string_view text, std::uint8_t scale,
                            std::int64_t &result) noexcept {
  if (scale > 18U || text.empty()) {
    return false;
  }
  bool negative = false;
  std::size_t position = 0;
  if (text.front() == '-' || text.front() == '+') {
    negative = text.front() == '-';
    position = 1;
  }
  if (position == text.size()) {
    return false;
  }

  std::uint64_t value = 0;
  std::size_t fractional_digits = 0;
  bool after_decimal = false;
  bool saw_digit = false;
  const auto positive_limit =
      static_cast<std::uint64_t>(std::numeric_limits<std::int64_t>::max());
  const auto limit = negative ? positive_limit + 1U : positive_limit;
  for (; position < text.size(); ++position) {
    const char character = text[position];
    if (character == '.') {
      if (after_decimal || !saw_digit || position + 1U == text.size()) {
        return false;
      }
      after_decimal = true;
      continue;
    }
    if (character < '0' || character > '9') {
      return false;
    }
    saw_digit = true;
    if (after_decimal && fractional_digits >= scale) {
      if (character != '0') {
        return false;
      }
      continue;
    }
    const auto digit = static_cast<std::uint64_t>(character - '0');
    if (value > (limit - digit) / 10U) {
      return false;
    }
    value = value * 10U + digit;
    if (after_decimal) {
      ++fractional_digits;
    }
  }
  while (fractional_digits < scale) {
    if (value > limit / 10U) {
      return false;
    }
    value *= 10U;
    ++fractional_digits;
  }
  if (negative) {
    result = value == positive_limit + 1U
                 ? std::numeric_limits<std::int64_t>::min()
                 : -static_cast<std::int64_t>(value);
  } else {
    result = static_cast<std::int64_t>(value);
  }
  return saw_digit;
}

#ifdef MDS_HAS_SIMDJSON

bool read_string(simdjson::dom::object object, std::string_view name,
                 std::string_view &value) {
  auto field = object[name].get_string();
  if (field.error()) {
    return false;
  }
  value = field.value();
  return true;
}

std::string_view optional_string(simdjson::dom::object object,
                                 std::string_view name) {
  auto field = object[name].get_string();
  return field.error() ? std::string_view{} : std::string_view(field.value());
}

bool read_int64(simdjson::dom::element element, std::int64_t &value) {
  auto number = element.get_int64();
  if (!number.error()) {
    value = number.value();
    return true;
  }
  auto text = element.get_string();
  return !text.error() && parse_signed_text(text.value(), value);
}

bool read_uint64(simdjson::dom::element element, std::uint64_t &value) {
  auto number = element.get_uint64();
  if (!number.error()) {
    value = number.value();
    return true;
  }
  auto text = element.get_string();
  return !text.error() && parse_unsigned_text(text.value(), value);
}

bool parse_level(simdjson::dom::element raw_level, std::uint8_t price_scale,
                 std::uint8_t contract_quantity_scale,
                 std::int64_t contract_multiplier,
                 utils::md::Level &level) {
  auto array_result = raw_level.get_array();
  if (array_result.error()) {
    return false;
  }
  auto values = array_result.value();
  auto iterator = values.begin();
  if (iterator == values.end()) {
    return false;
  }
  auto price = (*iterator).get_string();
  if (price.error()) {
    return false;
  }
  ++iterator;
  if (iterator == values.end()) {
    return false;
  }
  auto quantity = (*iterator).get_string();
  std::int64_t contracts{};
  if (quantity.error() ||
      !local_decimal_to_fixed(price.value(), price_scale, level.price) ||
      !local_decimal_to_fixed(quantity.value(), contract_quantity_scale,
                              contracts)) {
    return false;
  }
  const auto converted = static_cast<Int128>(contracts) *
                         static_cast<Int128>(contract_multiplier);
  if (converted < 0 ||
      converted > std::numeric_limits<std::int64_t>::max()) {
    return false;
  }
  level.quantity = static_cast<std::int64_t>(converted);
  return true;
}

bool parse_side(simdjson::dom::element raw_side, std::uint8_t price_scale,
                std::uint8_t contract_quantity_scale,
                std::int64_t contract_multiplier,
                std::size_t configured_capacity,
                std::vector<utils::md::Level> &levels,
                std::string_view side_name, std::string &error) {
  auto side_result = raw_side.get_array();
  if (side_result.error()) {
    error = "invalid OKX book side";
    return false;
  }
  for (auto raw_level : side_result.value()) {
    if (levels.size() >= configured_capacity ||
        levels.size() >= levels.capacity()) {
      error = "OKX ";
      error.append(side_name);
      error += " levels exceed event capacity configured=";
      error += std::to_string(
          std::min(configured_capacity, levels.capacity()));
      error += " observed_at_least=";
      error += std::to_string(levels.size() + 1);
      return false;
    }
    utils::md::Level level;
    if (!parse_level(raw_level, price_scale, contract_quantity_scale,
                     contract_multiplier, level)) {
      error = "invalid or imprecise OKX book level";
      return false;
    }
    levels.push_back(level);
  }
  return true;
}

#endif

class OkxAdapter final : public VenueAdapter {
 public:
  OkxAdapter(utils::md::ProductType product, std::size_t max_levels_per_side)
      : product_(product), max_levels_per_side_(max_levels_per_side)
#ifdef MDS_HAS_SIMDJSON
        ,
        impl_(std::make_unique<Impl>())
#endif
  {
  }

  [[nodiscard]] utils::md::Venue venue() const noexcept override {
    return utils::md::Venue::Okx;
  }

  [[nodiscard]] utils::md::ProductType product() const noexcept override {
    return product_;
  }

  [[nodiscard]] HeartbeatSpec heartbeat() const override {
    return {HeartbeatKind::TextPing, "ping", 25'000};
  }

  bool build_subscription_batches(
      std::span<const StreamRequest> requests,
      std::vector<std::string> &batches, std::string &error) const override {
    batches.clear();
    std::string batch = R"({"op":"subscribe","args":[)";
    std::size_t argument_count = 0;
    for (const auto &request : requests) {
      std::string_view symbol;
      if (!expected_symbol(product_, request.canonical_symbol,
                           request.venue_symbol, symbol, error)) {
        batches.clear();
        return false;
      }
      if (!request.ticker && !request.orderbook) {
        error = "at least one OKX stream must be requested";
        batches.clear();
        return false;
      }
      if (request.ticker) {
        const auto channel = request.ticker_channel.empty()
                                 ? std::string_view{"bbo-tbt"}
                                 : request.ticker_channel;
        if (channel != "bbo-tbt") {
          error = "unsupported OKX ticker channel";
          batches.clear();
          return false;
        }
        append_argument(channel, symbol, batch, argument_count, batches);
      }
      if (request.orderbook) {
        const auto channel = request.orderbook_channel.empty()
                                 ? std::string_view{"books-l2-tbt"}
                                 : request.orderbook_channel;
        if (channel != "books-l2-tbt" && channel != "books") {
          error = "unsupported OKX order book channel";
          batches.clear();
          return false;
        }
        append_argument(channel, symbol, batch, argument_count, batches);
      }
    }
    if (argument_count == 0) {
      error = "no OKX subscription arguments were produced";
      batches.clear();
      return false;
    }
    batch += "]}";
    batches.push_back(std::move(batch));
    error.clear();
    return true;
  }

  bool parse_ws(std::string_view json, NormalizedEvent &event,
                std::string &error) override {
    event.reset();
    if (json == "pong") {
      event.type = AdapterEventType::Pong;
      error.clear();
      return true;
    }
#ifndef MDS_HAS_SIMDJSON
    (void)json;
    error = "simdjson support was not compiled";
    return false;
#else
    try {
      auto root = impl_->parse(json).get_object().value();
      auto event_name = root["event"].get_string();
      if (!event_name.error()) {
        const std::string_view name = event_name.value();
        if (name == "subscribe" || name == "unsubscribe" ||
            name == "login") {
          const auto code = optional_string(root, "code");
          if (name == "login" && !code.empty() && code != "0") {
            event.type = AdapterEventType::SubscribeError;
            error = "OKX public WebSocket login failed with code ";
            error.append(code);
            const auto message = optional_string(root, "msg");
            if (!message.empty()) {
              error += ": ";
              error.append(message);
            }
            return true;
          }
          event.type = AdapterEventType::SubscribeAck;
          auto argument = root["arg"].get_object();
          if (!argument.error()) {
            const auto symbol = optional_string(argument.value(), "instId");
            if (!symbol.empty() && !copy_symbol(symbol, event)) {
              error = "OKX acknowledgement symbol exceeds fixed capacity";
              return false;
            }
          }
          error.clear();
          return true;
        }
        if (name == "error") {
          event.type = AdapterEventType::SubscribeError;
          const auto code = optional_string(root, "code");
          const auto message = optional_string(root, "msg");
          error = "OKX subscription error";
          if (!code.empty()) {
            error += " ";
            error.append(code);
          }
          if (!message.empty()) {
            error += ": ";
            error.append(message);
          }
          return true;
        }
        error.clear();
        return true;
      }

      auto argument_result = root["arg"].get_object();
      if (argument_result.error()) {
        error = "OKX data message is missing arg";
        return false;
      }
      auto argument = argument_result.value();
      std::string_view channel;
      std::string_view symbol;
      if (!read_string(argument, "channel", channel) ||
          !read_string(argument, "instId", symbol) ||
          !copy_symbol(symbol, event)) {
        error = "invalid OKX data routing fields";
        return false;
      }
      const auto *scale = find_scale(symbol);
      if (scale == nullptr) {
        error = "OKX instrument metadata must be parsed before market data";
        return false;
      }

      auto data_result = root["data"].get_array();
      if (data_result.error()) {
        error = "OKX data message is missing data";
        return false;
      }
      auto data = data_result.value();
      auto iterator = data.begin();
      if (iterator == data.end()) {
        error = "OKX data array is empty";
        return false;
      }
      auto payload_result = (*iterator).get_object();
      if (payload_result.error()) {
        error = "invalid OKX data payload";
        return false;
      }
      ++iterator;
      if (iterator != data.end()) {
        error = "OKX message contains multiple data payloads";
        return false;
      }
      auto payload = payload_result.value();
      std::uint64_t timestamp = 0;
      auto timestamp_field = payload["ts"];
      if (!timestamp_field.error() &&
          !read_uint64(timestamp_field.value(), timestamp)) {
        error = "invalid OKX exchange timestamp";
        return false;
      }
      event.exchange_time_ms = timestamp;

      if (channel == "bbo-tbt") {
        return parse_bbo(payload, *scale, event, error);
      }
      if (channel == "books-l2-tbt" || channel == "books") {
        auto action = root["action"].get_string();
        if (action.error()) {
          error = "OKX book message is missing action";
          return false;
        }
        const auto action_value = action.value();
        if (!parse_book(payload, action_value, *scale, event, error)) {
          if (error.find("levels exceed event capacity") !=
              std::string::npos) {
            error += " symbol=";
            error.append(symbol);
            error += " channel=";
            error.append(channel);
            error += " action=";
            error.append(action_value);
          }
          return false;
        }
        return true;
      }
      event.reset();
      error.clear();
      return true;
    } catch (const simdjson::simdjson_error &exception) {
      error = exception.what();
      return false;
    }
#endif
  }

  [[nodiscard]] HttpRequestSpec metadata_request() const override {
    return {HttpRequestSpec::Method::Get,
            product_ == utils::md::ProductType::Spot
                ? "/api/v5/public/instruments?instType=SPOT"
                : "/api/v5/public/instruments?instType=SWAP",
            {}, {}};
  }

  bool parse_metadata(std::string_view json,
                      std::span<const StreamRequest> requests,
                      std::vector<InstrumentMetadata> &metadata,
                      std::string &error) override {
    return parse_metadata_impl(json, requests, metadata, error, false);
  }

  bool parse_discovery_metadata(
      std::string_view json, std::span<const StreamRequest> requests,
      std::vector<InstrumentMetadata> &metadata,
      std::string &error) override {
    return parse_metadata_impl(json, requests, metadata, error, true);
  }

 private:
  struct ScaleEntry {
    std::string venue_symbol;
    std::uint8_t price_scale{};
    std::uint8_t contract_quantity_scale{};
    std::int64_t contract_multiplier{1};
  };

#ifdef MDS_HAS_SIMDJSON
  struct Impl {
    simdjson::dom::parser parser;
    std::vector<char> buffer;

    Impl() : buffer(kJsonCapacity + simdjson::SIMDJSON_PADDING) {
      const auto allocated = parser.allocate(kJsonCapacity);
      (void)allocated;
    }

    simdjson::dom::element parse(std::string_view json) {
      if (json.size() + simdjson::SIMDJSON_PADDING > buffer.size()) {
        throw simdjson::simdjson_error(simdjson::CAPACITY);
      }
      std::memcpy(buffer.data(), json.data(), json.size());
      std::memset(buffer.data() + json.size(), 0, simdjson::SIMDJSON_PADDING);
      return parser.parse(buffer.data(), json.size(), false).value();
    }
  };
#endif

  bool parse_metadata_impl(std::string_view json,
                           std::span<const StreamRequest> requests,
                           std::vector<InstrumentMetadata> &metadata,
                           std::string &error, bool discovery) {
#ifndef MDS_HAS_SIMDJSON
    (void)json;
    (void)requests;
    (void)metadata;
    (void)discovery;
    error = "simdjson support was not compiled";
    return false;
#else
    try {
      auto root = impl_->parse(json).get_object().value();
      const auto code = optional_string(root, "code");
      if (!code.empty() && code != "0") {
        error = "OKX instruments request failed with code ";
        error.append(code);
        const auto message = optional_string(root, "msg");
        if (!message.empty()) {
          error += ": ";
          error.append(message);
        }
        return false;
      }
      auto data_result = root["data"].get_array();
      if (data_result.error()) {
        error = "OKX instruments response is missing data";
        return false;
      }
      const auto data = data_result.value();
      std::vector<InstrumentMetadata> parsed;
      parsed.reserve(requests.size());
      std::vector<ScaleEntry> parsed_scales;
      parsed_scales.reserve(requests.size());

      for (const auto &request : requests) {
        std::string_view venue_symbol;
        if (!expected_symbol(product_, request.canonical_symbol,
                             request.venue_symbol, venue_symbol, error)) {
          return false;
        }
        bool found = false;
        for (auto raw_entry : data) {
          auto entry_result = raw_entry.get_object();
          if (entry_result.error()) {
            continue;
          }
          auto entry = entry_result.value();
          if (optional_string(entry, "instId") != venue_symbol) {
            continue;
          }
          const auto state = optional_string(entry, "state");
          if (discovery && !state.empty() && state != "live") {
            found = true;
            break;
          }
          InstrumentMetadata instrument;
          instrument.canonical_symbol = std::string(request.canonical_symbol);
          instrument.venue_symbol = std::string(venue_symbol);
          instrument.base_asset = std::string(optional_string(entry, "baseCcy"));
          instrument.quote_asset =
              std::string(optional_string(entry, "quoteCcy"));
          instrument.settle_asset =
              std::string(optional_string(entry, "settleCcy"));
          fill_missing_assets(instrument, venue_symbol);

          const auto tick_size = optional_string(entry, "tickSz");
          const auto lot_size = optional_string(entry, "lotSz");
          const auto contract_value = optional_string(entry, "ctVal");
          std::uint8_t contract_quantity_scale{};
          std::uint8_t multiplier_scale{};
          std::int64_t contract_lot{};
          std::int64_t multiplier{1};
          const bool perpetual =
              product_ == utils::md::ProductType::Perpetual;
          if (instrument.base_asset.empty() ||
              instrument.quote_asset.empty() ||
              instrument.settle_asset.empty() || tick_size.empty() ||
              lot_size.empty() ||
              !local_decimal_scale(tick_size, instrument.price_scale) ||
              !local_decimal_scale(lot_size, contract_quantity_scale) ||
              !local_decimal_to_fixed(tick_size, instrument.price_scale,
                                      instrument.tick_size) ||
              !local_decimal_to_fixed(lot_size, contract_quantity_scale,
                                      contract_lot) ||
              (perpetual &&
               (contract_value.empty() ||
                !local_decimal_scale(contract_value, multiplier_scale) ||
                !local_decimal_to_fixed(contract_value, multiplier_scale,
                                        multiplier))) ||
              static_cast<unsigned>(contract_quantity_scale) +
                      static_cast<unsigned>(multiplier_scale) >
                  18U ||
              instrument.tick_size <= 0 || contract_lot <= 0 ||
              multiplier <= 0) {
            error = "invalid OKX instrument metadata";
            return false;
          }
          const auto base_lot =
              static_cast<Int128>(contract_lot) *
              static_cast<Int128>(multiplier);
          if (base_lot > std::numeric_limits<std::int64_t>::max()) {
            error = "OKX contract lot size overflows base quantity";
            return false;
          }
          instrument.quantity_scale = static_cast<std::uint8_t>(
              static_cast<unsigned>(contract_quantity_scale) +
              static_cast<unsigned>(multiplier_scale));
          instrument.lot_size = static_cast<std::int64_t>(base_lot);
          instrument.contract_multiplier = multiplier;
          instrument.contract_multiplier_scale = multiplier_scale;
          parsed.push_back(instrument);
          parsed_scales.push_back(
              ScaleEntry{instrument.venue_symbol, instrument.price_scale,
                         contract_quantity_scale, multiplier});
          found = true;
          break;
        }
        if (!found) {
          error = "requested OKX instrument was not found";
          return false;
        }
      }

      metadata.reserve(metadata.size() + parsed.size());
      for (auto &instrument : parsed) {
        metadata.push_back(std::move(instrument));
      }
      scales_ = std::move(parsed_scales);
      error.clear();
      return true;
    } catch (const simdjson::simdjson_error &exception) {
      error = exception.what();
      return false;
    }
#endif
  }

#ifdef MDS_HAS_SIMDJSON
  const ScaleEntry *find_scale(std::string_view symbol) const noexcept {
    for (const auto &scale : scales_) {
      if (scale.venue_symbol == symbol) {
        return &scale;
      }
    }
    return nullptr;
  }

  bool parse_bbo(simdjson::dom::object payload, const ScaleEntry &scale,
                 NormalizedEvent &event, std::string &error) {
    std::uint64_t sequence = 0;
    auto sequence_field = payload["seqId"];
    if (sequence_field.error() ||
        !read_uint64(sequence_field.value(), sequence)) {
      error = "invalid OKX BBO sequence";
      return false;
    }
    auto bids_result = payload["bids"].get_array();
    auto asks_result = payload["asks"].get_array();
    if (bids_result.error() || asks_result.error()) {
      error = "OKX BBO payload is missing sides";
      return false;
    }
    auto bids = bids_result.value();
    auto asks = asks_result.value();
    auto bid = bids.begin();
    auto ask = asks.begin();
    if (bid == bids.end() || ask == asks.end() ||
        !parse_level(*bid, scale.price_scale,
                     scale.contract_quantity_scale,
                     scale.contract_multiplier, event.bid) ||
        !parse_level(*ask, scale.price_scale,
                     scale.contract_quantity_scale,
                     scale.contract_multiplier, event.ask)) {
      error = "invalid OKX BBO level";
      return false;
    }
    event.type = AdapterEventType::Bbo;
    event.first_sequence = sequence;
    event.final_sequence = sequence;
    error.clear();
    return true;
  }

  bool parse_book(simdjson::dom::object payload, std::string_view action,
                  const ScaleEntry &scale, NormalizedEvent &event,
                  std::string &error) {
    std::int64_t previous = 0;
    std::int64_t sequence = 0;
    auto previous_field = payload["prevSeqId"];
    auto sequence_field = payload["seqId"];
    if (previous_field.error() || sequence_field.error() ||
        !read_int64(previous_field.value(), previous) ||
        !read_int64(sequence_field.value(), sequence) || previous < -1 ||
        sequence < 0 || previous >= sequence) {
      error = "invalid OKX book sequence range";
      return false;
    }
    if (action == "snapshot") {
      event.type = AdapterEventType::BookSnapshot;
      event.sequence_reset = true;
    } else if (action == "update") {
      event.type = AdapterEventType::BookDelta;
      event.strict_previous_sequence = true;
    } else {
      error = "unsupported OKX book action";
      return false;
    }
    event.first_sequence = static_cast<std::uint64_t>(previous + 1);
    event.final_sequence = static_cast<std::uint64_t>(sequence);

    auto bids = payload["bids"];
    auto asks = payload["asks"];
    if (bids.error() || asks.error() ||
        !parse_side(bids.value(), scale.price_scale,
                    scale.contract_quantity_scale,
                    scale.contract_multiplier, max_levels_per_side_,
                    event.bids, "bid", error) ||
        !parse_side(asks.value(), scale.price_scale,
                    scale.contract_quantity_scale,
                    scale.contract_multiplier, max_levels_per_side_,
                    event.asks, "ask", error)) {
      if (error.empty()) {
        error = "OKX book payload is missing sides";
      }
      return false;
    }
    error.clear();
    return true;
  }
#endif

  void fill_missing_assets(InstrumentMetadata &instrument,
                           std::string_view venue_symbol) const {
    const auto first = venue_symbol.find('-');
    const auto second =
        first == std::string_view::npos
            ? std::string_view::npos
            : venue_symbol.find('-', first + 1);
    if (first != std::string_view::npos) {
      if (instrument.base_asset.empty()) {
        instrument.base_asset.assign(venue_symbol.substr(0, first));
      }
      if (instrument.quote_asset.empty()) {
        instrument.quote_asset.assign(
            venue_symbol.substr(
                first + 1,
                (second == std::string_view::npos
                     ? venue_symbol.size()
                     : second) -
                    first - 1));
      }
    }
    if (instrument.settle_asset.empty()) {
      instrument.settle_asset = instrument.quote_asset;
    }
  }

  utils::md::ProductType product_;
  std::size_t max_levels_per_side_;
  std::vector<ScaleEntry> scales_;
#ifdef MDS_HAS_SIMDJSON
  std::unique_ptr<Impl> impl_;
#endif
};

}  // namespace

std::unique_ptr<VenueAdapter>
make_okx_adapter(utils::md::ProductType product,
                 std::size_t max_levels_per_side) {
  if ((product != utils::md::ProductType::Spot &&
       product != utils::md::ProductType::Perpetual) ||
      max_levels_per_side == 0) {
    return nullptr;
  }
  return std::make_unique<OkxAdapter>(product, max_levels_per_side);
}

}  // namespace mds::exchange
