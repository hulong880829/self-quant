#include "mds/exchange/bitget/bitget_adapter.h"

#include <algorithm>
#include <charconv>
#include <cstring>
#include <limits>
#include <memory>
#include <string>
#include <utility>

#ifdef MDS_HAS_SIMDJSON
#include <simdjson.h>
#endif

namespace mds::exchange {
namespace {

constexpr std::size_t kMaximumSubscriptionBytes = 4096;
constexpr std::size_t kJsonCapacity = 8U * 1024U * 1024U;
// Bitget books can retain orders at a finer precision than the symbol's
// current pricePlace. Use the maximum representation scale advertised across
// USDT futures so reconnects do not change the fixed-point representation.
constexpr std::uint8_t kContractPriceRepresentationScale = 10;

bool scale_positive_integer(std::int64_t value, std::uint8_t exponent,
                            std::int64_t &result) noexcept {
  if (value <= 0) {
    return false;
  }
  while (exponent != 0) {
    if (value > std::numeric_limits<std::int64_t>::max() / 10) {
      return false;
    }
    value *= 10;
    --exponent;
  }
  result = value;
  return true;
}

bool append_json_string(std::string_view value, std::string &output) {
  output.push_back('"');
  constexpr char hex[] = "0123456789abcdef";
  for (const char raw_character : value) {
    const auto character =
        static_cast<unsigned char>(raw_character);
    switch (character) {
    case '"':
      output += "\\\"";
      break;
    case '\\':
      output += "\\\\";
      break;
    case '\b':
      output += "\\b";
      break;
    case '\f':
      output += "\\f";
      break;
    case '\n':
      output += "\\n";
      break;
    case '\r':
      output += "\\r";
      break;
    case '\t':
      output += "\\t";
      break;
    default:
      if (character < 0x20U) {
        output += "\\u00";
        output.push_back(hex[character >> 4U]);
        output.push_back(hex[character & 0x0fU]);
      } else {
        output.push_back(static_cast<char>(character));
      }
    }
  }
  output.push_back('"');
  return true;
}

bool decimal_to_fixed_local(std::string_view text, std::uint8_t scale,
                            std::int64_t &result) noexcept {
  if (text.empty() || text.front() == '-' || text.front() == '+' ||
      scale > 18) {
    return false;
  }
  std::uint64_t value = 0;
  std::size_t fractional_digits = 0;
  bool after_decimal = false;
  bool saw_digit = false;
  for (const char character : text) {
    if (character == '.') {
      if (after_decimal || !saw_digit) {
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
    constexpr auto maximum =
        static_cast<std::uint64_t>(std::numeric_limits<std::int64_t>::max());
    if (value > (maximum - digit) / 10U) {
      return false;
    }
    value = value * 10U + digit;
    if (after_decimal) {
      ++fractional_digits;
    }
  }
  if (!saw_digit || (after_decimal && text.back() == '.')) {
    return false;
  }
  while (fractional_digits < scale) {
    if (value >
        static_cast<std::uint64_t>(
            std::numeric_limits<std::int64_t>::max() / 10)) {
      return false;
    }
    value *= 10U;
    ++fractional_digits;
  }
  result = static_cast<std::int64_t>(value);
  return true;
}

bool parse_unsigned_text(std::string_view text, std::uint64_t &result) {
  if (text.empty()) {
    return false;
  }
  const auto parsed =
      std::from_chars(text.data(), text.data() + text.size(), result);
  return parsed.ec == std::errc{} &&
         parsed.ptr == text.data() + text.size();
}

struct SymbolScale {
  std::string venue_symbol;
  std::uint8_t price{};
  std::uint8_t quantity{};
};

#ifdef MDS_HAS_SIMDJSON
class JsonParser {
 public:
  JsonParser() : buffer_(kJsonCapacity + simdjson::SIMDJSON_PADDING) {
    const auto allocated = parser_.allocate(kJsonCapacity);
    (void)allocated;
  }

  simdjson::dom::element parse(std::string_view json) {
    if (json.size() > kJsonCapacity) {
      throw simdjson::simdjson_error(simdjson::CAPACITY);
    }
    std::memcpy(buffer_.data(), json.data(), json.size());
    std::memset(buffer_.data() + json.size(), 0, simdjson::SIMDJSON_PADDING);
    return parser_.parse(buffer_.data(), json.size(), false).value();
  }

 private:
  simdjson::dom::parser parser_;
  std::vector<char> buffer_;
};

bool read_uint64(simdjson::dom::element value, std::uint64_t &result) {
  auto integer = value.get_uint64();
  if (!integer.error()) {
    result = integer.value();
    return true;
  }
  auto text = value.get_string();
  return !text.error() && parse_unsigned_text(text.value(), result);
}

bool read_uint8(simdjson::dom::object object, std::string_view field,
                std::uint8_t &result) {
  std::uint64_t parsed{};
  if (!read_uint64(object[field], parsed) ||
      parsed > std::numeric_limits<std::uint8_t>::max()) {
    return false;
  }
  result = static_cast<std::uint8_t>(parsed);
  return true;
}

bool read_positive_integer(simdjson::dom::element value,
                           std::int64_t &result) {
  std::uint64_t parsed{};
  if (!read_uint64(value, parsed) || parsed == 0 ||
      parsed >
          static_cast<std::uint64_t>(
              std::numeric_limits<std::int64_t>::max())) {
    return false;
  }
  result = static_cast<std::int64_t>(parsed);
  return true;
}

bool read_level(simdjson::dom::element raw, std::uint8_t price_scale,
                std::uint8_t quantity_scale, utils::md::Level &level,
                std::string *error = nullptr) {
  auto values_result = raw.get_array();
  if (values_result.error()) {
    if (error != nullptr) {
      *error = "invalid Bitget order book level shape";
    }
    return false;
  }
  auto values = values_result.value();
  auto iterator = values.begin();
  if (iterator == values.end()) {
    if (error != nullptr) {
      *error = "empty Bitget order book level";
    }
    return false;
  }
  auto price = (*iterator).get_string();
  ++iterator;
  if (price.error() || iterator == values.end()) {
    if (error != nullptr) {
      *error = "invalid Bitget order book price field";
    }
    return false;
  }
  auto quantity = (*iterator).get_string();
  if (quantity.error()) {
    if (error != nullptr) {
      *error = "invalid Bitget order book quantity field";
    }
    return false;
  }
  if (!decimal_to_fixed_local(price.value(), price_scale, level.price)) {
    if (error != nullptr) {
      *error = "invalid Bitget order book price value=";
      error->append(price.value());
      error->append(" scale=");
      error->append(std::to_string(price_scale));
    }
    return false;
  }
  if (!decimal_to_fixed_local(quantity.value(), quantity_scale,
                              level.quantity)) {
    if (error != nullptr) {
      *error = "invalid Bitget order book quantity value=";
      error->append(quantity.value());
      error->append(" scale=");
      error->append(std::to_string(quantity_scale));
    }
    return false;
  }
  return true;
}
#endif

enum class SideParseResult : std::uint8_t {
  Ok,
  CapacityExceeded,
  Invalid,
};

class BitgetAdapter final : public VenueAdapter {
 public:
  BitgetAdapter(utils::md::ProductType product,
                std::size_t max_levels_per_side)
      : product_(product), max_levels_per_side_(max_levels_per_side)
#ifdef MDS_HAS_SIMDJSON
        ,
        parser_(std::make_unique<JsonParser>())
#endif
  {
  }

  [[nodiscard]] utils::md::Venue venue() const noexcept override {
    return utils::md::Venue::Bitget;
  }

  [[nodiscard]] utils::md::ProductType product() const noexcept override {
    return product_;
  }

  [[nodiscard]] HeartbeatSpec heartbeat() const override {
    return {HeartbeatKind::TextPing, "ping", 30'000};
  }

  bool build_subscription_batches(
      std::span<const StreamRequest> requests,
      std::vector<std::string> &batches, std::string &error) const override {
    constexpr std::string_view prefix = R"({"op":"subscribe","args":[)";
    constexpr std::string_view suffix = "]}";
    std::vector<std::string> arguments;
    arguments.reserve(requests.size() * 2U);
    const std::string_view instrument_type =
        product_ == utils::md::ProductType::Spot ? "SPOT" : "USDT-FUTURES";

    for (const auto &request : requests) {
      if (request.venue_symbol.empty()) {
        error = "Bitget subscription symbol is empty";
        return false;
      }
      auto add_argument = [&](std::string_view channel) {
        std::string argument =
            R"({"instType":")" + std::string(instrument_type) +
            R"(","channel":)";
        append_json_string(channel, argument);
        argument += R"(,"instId":)";
        append_json_string(request.venue_symbol, argument);
        argument.push_back('}');
        arguments.push_back(std::move(argument));
      };
      if (request.ticker) {
        if (!request.ticker_channel.empty() &&
            request.ticker_channel != "books1") {
          error = "Bitget BBO channel must be books1";
          return false;
        }
        add_argument("books1");
      }
      if (request.orderbook) {
        const auto channel = request.orderbook_channel.empty()
                                 ? std::string_view{"books"}
                                 : request.orderbook_channel;
        if (channel != "books" && channel != "books15") {
          error = "Bitget order book channel must be books or books15";
          return false;
        }
        add_argument(channel);
      }
    }
    if (arguments.empty()) {
      error = "at least one Bitget stream must be requested";
      return false;
    }

    std::vector<std::string> parsed_batches;
    std::string batch(prefix);
    for (const auto &argument : arguments) {
      const std::size_t separator = batch.size() == prefix.size() ? 0U : 1U;
      if (prefix.size() + argument.size() + suffix.size() >
          kMaximumSubscriptionBytes) {
        error = "a Bitget subscription argument exceeds 4096 bytes";
        return false;
      }
      if (batch.size() + separator + argument.size() + suffix.size() >
          kMaximumSubscriptionBytes) {
        batch += suffix;
        parsed_batches.push_back(std::move(batch));
        batch.assign(prefix);
      }
      if (batch.size() != prefix.size()) {
        batch.push_back(',');
      }
      batch += argument;
    }
    batch += suffix;
    parsed_batches.push_back(std::move(batch));
    batches = std::move(parsed_batches);
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
      auto document = parser_->parse(json);
      auto object = document.get_object().value();
      auto event_name = object["event"].get_string();
      if (!event_name.error()) {
        if (event_name.value() == "subscribe" ||
            event_name.value() == "unsubscribe") {
          event.type = AdapterEventType::SubscribeAck;
          error.clear();
          return true;
        }
        if (event_name.value() == "error") {
          event.type = AdapterEventType::SubscribeError;
          std::string message = "Bitget subscription error";
          auto code = object["code"].get_string();
          auto text = object["msg"].get_string();
          if (!code.error()) {
            message += " ";
            message.append(code.value());
          }
          if (!text.error()) {
            message += ": ";
            message.append(text.value());
          }
          error = std::move(message);
          return true;
        }
        event.type = AdapterEventType::Ignored;
        error.clear();
        return true;
      }

      auto argument = object["arg"].get_object().value();
      const auto channel = argument["channel"].get_string().value();
      const auto symbol = argument["instId"].get_string().value();
      const auto action = object["action"].get_string().value();
      if (!copy_symbol(symbol, event)) {
        error = "Bitget symbol is empty or exceeds fixed capacity";
        return false;
      }
      const auto *scales = find_scales(symbol);
      if (scales == nullptr) {
        error = "Bitget symbol metadata has not been parsed";
        return false;
      }

      auto data = object["data"].get_array().value();
      auto iterator = data.begin();
      if (iterator == data.end()) {
        error = "Bitget market-data payload is empty";
        return false;
      }
      auto update = (*iterator).get_object().value();
      ++iterator;
      if (iterator != data.end()) {
        error = "Bitget market-data payload contains multiple updates";
        return false;
      }
      if (!read_uint64(update["seq"], event.final_sequence)) {
        error = "Bitget update has an invalid seq";
        return false;
      }
      auto timestamp = update["ts"];
      if (!timestamp.error() &&
          !read_uint64(timestamp.value(), event.exchange_time_ms)) {
        error = "Bitget update has an invalid timestamp";
        return false;
      }

      const bool snapshot = action == "snapshot";
      if (snapshot) {
        event.first_sequence = event.final_sequence;
        event.sequence_reset = true;
      } else if (action == "update") {
        std::uint64_t previous{};
        if (!read_uint64(update["pseq"], previous) ||
            previous == std::numeric_limits<std::uint64_t>::max() ||
            previous >= event.final_sequence) {
          error = "Bitget update has invalid seq/pseq continuity";
          return false;
        }
        event.first_sequence = previous + 1U;
        event.strict_previous_sequence = true;
      } else {
        error = "unsupported Bitget order book action";
        return false;
      }

      if (channel == "books1") {
        if (!snapshot) {
          error = "Bitget books1 message is not a snapshot";
          return false;
        }
        auto bids = update["bids"].get_array().value();
        auto asks = update["asks"].get_array().value();
        auto bid = bids.begin();
        auto ask = asks.begin();
        if (bid == bids.end() || ask == asks.end() ||
            !read_level(*bid, scales->price, scales->quantity, event.bid,
                        &error) ||
            !read_level(*ask, scales->price, scales->quantity, event.ask,
                        &error)) {
          if (error.empty()) {
            error = "invalid Bitget books1 level";
          }
          error.append(" symbol=");
          error.append(symbol);
          return false;
        }
        event.type = AdapterEventType::Bbo;
      } else if (channel == "books" || channel == "books15") {
        if (channel == "books15" && !snapshot) {
          error = "Bitget books15 message is not a snapshot";
          return false;
        }
        const auto bids_result =
            parse_side(update["bids"], scales->price, scales->quantity,
                       event.bids, error);
        if (bids_result == SideParseResult::CapacityExceeded) {
          event.type = AdapterEventType::BookGap;
          event.input_side = InputSide::Bid;
          event.input_capacity =
              std::min(max_levels_per_side_, event.bids.capacity());
          event.bids.clear();
          event.asks.clear();
          error.clear();
          return true;
        }
        if (bids_result == SideParseResult::Invalid) {
          error.append(" symbol=");
          error.append(symbol);
          error.append(" side=bid");
          return false;
        }
        const auto asks_result =
            parse_side(update["asks"], scales->price, scales->quantity,
                       event.asks, error);
        if (asks_result == SideParseResult::CapacityExceeded) {
          event.type = AdapterEventType::BookGap;
          event.input_side = InputSide::Ask;
          event.input_capacity =
              std::min(max_levels_per_side_, event.asks.capacity());
          event.bids.clear();
          event.asks.clear();
          error.clear();
          return true;
        }
        if (asks_result == SideParseResult::Invalid) {
          error.append(" symbol=");
          error.append(symbol);
          error.append(" side=ask");
          return false;
        }
        event.type = snapshot || channel == "books15"
                         ? AdapterEventType::BookSnapshot
                         : AdapterEventType::BookDelta;
      } else {
        error = "unsupported Bitget public channel";
        return false;
      }
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
                ? "/api/v2/spot/public/symbols"
                : "/api/v2/mix/market/contracts?productType=USDT-FUTURES",
            {}, {}};
  }

  [[nodiscard]] HttpRequestSpec
  discovery_turnover_request() const override {
    return {HttpRequestSpec::Method::Get,
            product_ == utils::md::ProductType::Spot
                ? "/api/v2/spot/market/tickers"
                : "/api/v2/mix/market/tickers?productType=USDT-FUTURES",
            {}, {}};
  }

  bool enrich_discovery_turnover(
      std::string_view json, std::span<InstrumentMetadata> metadata,
      std::string &error) override {
#ifndef MDS_HAS_SIMDJSON
    (void)json;
    (void)metadata;
    error = "simdjson support was not compiled";
    return false;
#else
    try {
      auto document = parser_->parse(json);
      auto root = document.get_object().value();
      auto code = root["code"].get_string();
      if (code.error() || code.value() != "00000") {
        error = "Bitget tickers returned an error";
        return false;
      }
      auto entries = root["data"].get_array().value();
      std::vector<std::uint64_t> turnovers(metadata.size());
      std::vector<bool> matched(metadata.size());
      for (auto raw_entry : entries) {
        auto entry = raw_entry.get_object().value();
        auto symbol_result = entry["symbol"].get_string();
        if (symbol_result.error()) {
          continue;
        }
        const auto symbol = std::string_view(symbol_result.value());
        const auto found =
            std::find_if(metadata.begin(), metadata.end(),
                         [symbol](const InstrumentMetadata &instrument) {
                           return instrument.venue_symbol == symbol;
                         });
        if (found == metadata.end()) {
          continue;
        }
        const auto index =
            static_cast<std::size_t>(found - metadata.begin());
        if (matched[index]) {
          error = "duplicate Bitget ticker symbol: ";
          error.append(symbol);
          return false;
        }
        matched[index] = true;

        auto turnover_field = entry["quoteVolume"];
        if (turnover_field.error() == simdjson::NO_SUCH_FIELD) {
          turnover_field = entry["usdtVolume"];
        }
        auto turnover = turnover_field.get_string();
        if (turnover.error() ||
            !mds::exchange::decimal_to_turnover(turnover.value(),
                                                turnovers[index])) {
          error = "invalid Bitget ticker turnover: ";
          error.append(symbol);
          return false;
        }
      }
      for (std::size_t index = 0; index < metadata.size(); ++index) {
        metadata[index].turnover_24h = turnovers[index];
      }
      error.clear();
      return true;
    } catch (const simdjson::simdjson_error &exception) {
      error = "malformed Bitget tickers response: ";
      error.append(exception.what());
      return false;
    }
#endif
  }

  bool parse_metadata(std::string_view json,
                      std::span<const StreamRequest> requests,
                      std::vector<InstrumentMetadata> &metadata,
                      std::string &error) override {
#ifndef MDS_HAS_SIMDJSON
    (void)json;
    (void)requests;
    (void)metadata;
    error = "simdjson support was not compiled";
    return false;
#else
    try {
      auto document = parser_->parse(json);
      auto root = document.get_object().value();
      auto code = root["code"].get_string();
      if (!code.error() && code.value() != "00000") {
        error = "Bitget metadata request failed with code " +
                std::string(code.value());
        return false;
      }
      auto entries = root["data"].get_array().value();
      std::vector<InstrumentMetadata> parsed;
      std::vector<SymbolScale> parsed_scales;
      parsed.reserve(requests.size());
      parsed_scales.reserve(requests.size());
      for (const auto &request : requests) {
        bool found = false;
        for (auto raw_entry : entries) {
          auto entry = raw_entry.get_object().value();
          const auto symbol = entry["symbol"].get_string().value();
          if (symbol != request.venue_symbol) {
            continue;
          }
          InstrumentMetadata instrument;
          instrument.canonical_symbol = request.canonical_symbol;
          instrument.venue_symbol = request.venue_symbol;
          instrument.base_asset = entry["baseCoin"].get_string().value();
          instrument.quote_asset = entry["quoteCoin"].get_string().value();
          instrument.settle_asset = instrument.quote_asset;
          instrument.contract_multiplier = 1;
          if (product_ == utils::md::ProductType::Spot) {
            if (!read_uint8(entry, "pricePrecision",
                            instrument.price_scale) ||
                !read_uint8(entry, "quantityPrecision",
                            instrument.quantity_scale) ||
                instrument.price_scale > 18 ||
                instrument.quantity_scale > 18) {
              error = "invalid Bitget spot symbol precision";
              return false;
            }
            instrument.tick_size = 1;
            instrument.lot_size = 1;
          } else {
            std::uint8_t native_price_scale{};
            std::int64_t native_tick_size{};
            if (!read_uint8(entry, "pricePlace", native_price_scale) ||
                !read_uint8(entry, "volumePlace",
                            instrument.quantity_scale) ||
                native_price_scale > kContractPriceRepresentationScale ||
                instrument.quantity_scale > 18 ||
                !read_positive_integer(entry["priceEndStep"],
                                       native_tick_size) ||
                !scale_positive_integer(
                    native_tick_size,
                    static_cast<std::uint8_t>(
                        kContractPriceRepresentationScale -
                        native_price_scale),
                    instrument.tick_size)) {
              error = "invalid Bitget contract price/quantity precision";
              return false;
            }
            instrument.price_scale = kContractPriceRepresentationScale;
            instrument.refine_book_tick = true;
            const auto multiplier =
                entry["sizeMultiplier"].get_string().value();
            if (!decimal_to_fixed_local(multiplier,
                                        instrument.quantity_scale,
                                        instrument.lot_size) ||
                instrument.lot_size <= 0) {
              error = "invalid Bitget contract sizeMultiplier";
              return false;
            }
          }
          parsed_scales.push_back(
              {instrument.venue_symbol, instrument.price_scale,
               instrument.quantity_scale});
          parsed.push_back(std::move(instrument));
          found = true;
          break;
        }
        if (!found) {
          error = "Bitget symbol was not found in metadata response: " +
                  std::string(request.venue_symbol);
          return false;
        }
      }
      metadata = std::move(parsed);
      symbol_scales_ = std::move(parsed_scales);
      error.clear();
      return true;
    } catch (const simdjson::simdjson_error &exception) {
      error = exception.what();
      return false;
    }
#endif
  }

 private:
  const SymbolScale *find_scales(std::string_view symbol) const noexcept {
    const auto found =
        std::find_if(symbol_scales_.begin(), symbol_scales_.end(),
                     [symbol](const SymbolScale &entry) {
                       return entry.venue_symbol == symbol;
                     });
    return found == symbol_scales_.end() ? nullptr : &*found;
  }

#ifdef MDS_HAS_SIMDJSON
  SideParseResult
  parse_side(simdjson::dom::element side, std::uint8_t price_scale,
             std::uint8_t quantity_scale,
             std::vector<utils::md::Level> &output,
             std::string &error) const {
    auto raw_levels = side.get_array();
    if (raw_levels.error()) {
      error = "invalid Bitget order book side";
      return SideParseResult::Invalid;
    }
    const std::size_t capacity =
        std::min(max_levels_per_side_, output.capacity());
    for (auto raw_level : raw_levels.value()) {
      if (output.size() >= capacity) {
        return SideParseResult::CapacityExceeded;
      }
      utils::md::Level level;
      if (!read_level(raw_level, price_scale, quantity_scale, level,
                      &error)) {
        return SideParseResult::Invalid;
      }
      output.push_back(level);
    }
    return SideParseResult::Ok;
  }
#endif

  utils::md::ProductType product_;
  std::size_t max_levels_per_side_;
  std::vector<SymbolScale> symbol_scales_;
#ifdef MDS_HAS_SIMDJSON
  std::unique_ptr<JsonParser> parser_;
#endif
};

}  // namespace

std::unique_ptr<VenueAdapter>
make_bitget_adapter(utils::md::ProductType product,
                    std::size_t max_levels_per_side) {
  if ((product != utils::md::ProductType::Spot &&
       product != utils::md::ProductType::Perpetual) ||
      max_levels_per_side == 0) {
    return nullptr;
  }
  return std::make_unique<BitgetAdapter>(product, max_levels_per_side);
}

}  // namespace mds::exchange
