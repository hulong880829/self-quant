#include "mds/exchange/gate/gate_adapter.h"
#include "mds/exchange/symbol_policy.h"

#include <algorithm>
#include <charconv>
#include <cstring>
#include <limits>
#include <string>
#include <unordered_map>
#include <utility>

#ifdef MDS_HAS_SIMDJSON
#include <simdjson.h>
#endif

namespace mds::exchange {
namespace {

constexpr std::size_t kMaximumJsonBytes = 4U * 1024U * 1024U;
constexpr std::size_t kRestSnapshotLevels = 100;
// Gate spot books can retain historical orders submitted under an older,
// finer amount precision. Six is the maximum representation scale currently
// advertised across Gate spot instruments; trading increments remain driven
// by each instrument's amount_precision.
constexpr std::uint8_t kSpotQuantityRepresentationScale = 6;

struct GateScales {
  std::uint8_t price{};
  std::uint8_t quantity{};
  std::uint8_t contract_size_scale{};
  std::int64_t contract_multiplier{1};
  bool contracts{};
};

enum class LevelParseError : std::uint8_t {
  None,
  Capacity,
  Shape,
  Scale,
  Price,
  Quantity,
};

std::string_view level_error_name(LevelParseError value) noexcept {
  switch (value) {
  case LevelParseError::None:
    return "none";
  case LevelParseError::Capacity:
    return "capacity";
  case LevelParseError::Shape:
    return "shape";
  case LevelParseError::Scale:
    return "scale";
  case LevelParseError::Price:
    return "price";
  case LevelParseError::Quantity:
    return "quantity";
  }
  return "unknown";
}

void set_level_error(std::string &error, std::string_view context,
                     std::string_view symbol, std::string_view side,
                     LevelParseError reason) {
  error = "invalid Gate ";
  error.append(context);
  error += " symbol=";
  error.append(symbol);
  error += " side=";
  error.append(side);
  error += " reason=";
  error.append(level_error_name(reason));
}

void append_capacity_details(std::string &error, std::size_t configured,
                             std::size_t observed_at_least,
                             std::string_view channel,
                             std::string_view action) {
  error += " configured=";
  error += std::to_string(configured);
  error += " observed_at_least=";
  error += std::to_string(observed_at_least);
  error += " channel=";
  error.append(channel);
  error += " action=";
  error.append(action);
}

std::string gate_subscription(std::string_view channel,
                              std::string_view symbol,
                              bool orderbook,
                              std::uint32_t update_interval_ms = 20,
                              bool futures = false) {
  std::string result;
  result.reserve(channel.size() + symbol.size() + 96U);
  result += "{\"channel\":\"";
  append_json_escaped(result, channel);
  result += "\",\"event\":\"subscribe\",\"payload\":[\"";
  append_json_escaped(result, symbol);
  result.push_back('"');
  if (orderbook) {
    result += ",\"" + std::to_string(update_interval_ms) + "ms\"";
    if (futures) {
      result += ",\"20\"";
    }
  }
  result += "]}";
  return result;
}

bool unsigned_decimal_to_fixed(std::string_view value, std::uint8_t scale,
                               std::int64_t &result) noexcept {
  if (value.empty() || scale > 18) {
    return false;
  }
  bool negative = false;
  std::size_t cursor = 0;
  if (value.front() == '-') {
    negative = true;
    cursor = 1;
  } else if (value.front() == '+') {
    return false;
  }
  bool decimal = false;
  bool digit = false;
  std::size_t fractional = 0;
  std::uint64_t parsed = 0;
  constexpr auto maximum =
      static_cast<std::uint64_t>(std::numeric_limits<std::int64_t>::max());
  for (; cursor < value.size(); ++cursor) {
    const char character = value[cursor];
    if (character == '.') {
      if (decimal || !digit) {
        return false;
      }
      decimal = true;
      continue;
    }
    if (character < '0' || character > '9') {
      return false;
    }
    digit = true;
    if (decimal && fractional >= scale) {
      if (character != '0') {
        return false;
      }
      continue;
    }
    const auto next = static_cast<std::uint64_t>(character - '0');
    if (parsed > (maximum - next) / 10U) {
      return false;
    }
    parsed = parsed * 10U + next;
    if (decimal) {
      ++fractional;
    }
  }
  if (!digit || (decimal && value.back() == '.')) {
    return false;
  }
  while (fractional < scale) {
    if (parsed > maximum / 10U) {
      return false;
    }
    parsed *= 10U;
    ++fractional;
  }
  result = negative ? -static_cast<std::int64_t>(parsed)
                    : static_cast<std::int64_t>(parsed);
  return true;
}

bool scale_for_decimal(std::string_view value, std::uint8_t &scale) noexcept {
  if (value.empty() || value.front() == '-' || value.front() == '+') {
    return false;
  }
  const auto dot = value.find('.');
  if (dot != std::string_view::npos &&
      value.find('.', dot + 1) != std::string_view::npos) {
    return false;
  }
  for (const char character : value) {
    if (character != '.' && (character < '0' || character > '9')) {
      return false;
    }
  }
  std::size_t end = value.size();
  while (dot != std::string_view::npos && end > dot + 1 &&
         value[end - 1] == '0') {
    --end;
  }
  const auto digits =
      dot == std::string_view::npos || end == dot + 1 ? 0 : end - dot - 1;
  if (digits > 18 || dot == 0) {
    return false;
  }
  scale = static_cast<std::uint8_t>(digits);
  return true;
}

#ifdef MDS_HAS_SIMDJSON
std::string_view decimal_text(simdjson::dom::element value) {
  auto string = value.get_string();
  return string.error() ? std::string_view{} : string.value();
}

bool empty_string_value(simdjson::dom::element value) {
  auto string = value.get_string();
  return !string.error() && string.value().empty();
}

std::string_view optional_string(simdjson::dom::object object,
                                 std::string_view key) {
  auto value = object[key].get_string();
  return value.error() ? std::string_view{} : value.value();
}

std::uint64_t optional_uint(simdjson::dom::object object,
                            std::string_view key) {
  auto value = object[key].get_uint64();
  return value.error() ? 0 : value.value();
}

bool integral_value(simdjson::dom::element value,
                    std::int64_t &result) noexcept {
  auto text = value.get_string();
  if (!text.error()) {
    return unsigned_decimal_to_fixed(text.value(), 0, result);
  }
  auto unsigned_value = value.get_uint64();
  if (!unsigned_value.error()) {
    if (unsigned_value.value() >
        static_cast<std::uint64_t>(
            std::numeric_limits<std::int64_t>::max())) {
      return false;
    }
    result = static_cast<std::int64_t>(unsigned_value.value());
    return true;
  }
  auto signed_value = value.get_int64();
  if (signed_value.error()) {
    return false;
  }
  result = signed_value.value();
  return true;
}

bool scaled_value(simdjson::dom::element value, std::uint8_t scale,
                  std::int64_t &result) noexcept {
  auto text = value.get_string();
  if (!text.error()) {
    return unsigned_decimal_to_fixed(text.value(), scale, result);
  }
  std::int64_t integral = 0;
  if (!integral_value(value, integral)) {
    return false;
  }
  for (std::uint8_t digit = 0; digit < scale; ++digit) {
    if (integral > std::numeric_limits<std::int64_t>::max() / 10 ||
        integral < std::numeric_limits<std::int64_t>::min() / 10) {
      return false;
    }
    integral *= 10;
  }
  result = integral;
  return true;
}

bool parse_quantity(simdjson::dom::element raw, const GateScales &scales,
                    std::int64_t &quantity) noexcept {
  if (!scales.contracts) {
    return unsigned_decimal_to_fixed(decimal_text(raw), scales.quantity,
                                     quantity);
  }
  std::int64_t contracts = 0;
  if (!scaled_value(raw, scales.contract_size_scale, contracts) ||
      contracts == std::numeric_limits<std::int64_t>::min()) {
    return false;
  }
  if (contracts < 0) {
    contracts = -contracts;
  }
  if (contracts > std::numeric_limits<std::int64_t>::max() /
                      scales.contract_multiplier) {
    return false;
  }
  quantity = contracts * scales.contract_multiplier;
  return true;
}

LevelParseError parse_gate_level(simdjson::dom::element raw,
                                 const GateScales &scales,
                                 utils::md::Level &level) {
  simdjson::dom::element price;
  simdjson::dom::element quantity;
  auto array = raw.get_array();
  if (!array.error()) {
    std::size_t field = 0;
    for (auto value : array.value()) {
      if (field == 0) {
        price = value;
      } else if (field == 1) {
        quantity = value;
      } else {
        return LevelParseError::Shape;
      }
      ++field;
    }
    if (field != 2) {
      return LevelParseError::Shape;
    }
  } else {
    auto object_result = raw.get_object();
    if (object_result.error()) {
      return LevelParseError::Shape;
    }
    auto object = object_result.value();
    auto price_result = object["p"];
    auto quantity_result = object["s"];
    if (price_result.error() || quantity_result.error()) {
      return LevelParseError::Shape;
    }
    price = price_result.value();
    quantity = quantity_result.value();
  }

  const auto price_text = decimal_text(price);
  if (!unsigned_decimal_to_fixed(price_text, scales.price, level.price)) {
    return decimal_scale_mismatch(price_text, scales.price)
               ? LevelParseError::Scale
               : LevelParseError::Price;
  }
  if (!parse_quantity(quantity, scales, level.quantity) ||
      level.quantity == std::numeric_limits<std::int64_t>::min()) {
    return LevelParseError::Quantity;
  }
  if (level.quantity < 0) {
    level.quantity = -level.quantity;
  }
  return LevelParseError::None;
}
#endif

class GateAdapter final : public VenueAdapter {
 public:
  GateAdapter(utils::md::ProductType product, std::size_t maximum_levels)
#ifdef MDS_HAS_SIMDJSON
      : json_buffer_(kMaximumJsonBytes + simdjson::SIMDJSON_PADDING),
        product_(product),
        maximum_levels_(maximum_levels)
#else
      : product_(product), maximum_levels_(maximum_levels)
#endif
  {
  }

  [[nodiscard]] utils::md::Venue venue() const noexcept override {
    return utils::md::Venue::Gate;
  }
  [[nodiscard]] utils::md::ProductType product() const noexcept override {
    return product_;
  }
  [[nodiscard]] HeartbeatSpec heartbeat() const override {
    return {HeartbeatKind::Rfc6455Ping, {}, 15'000};
  }

  bool build_subscription_batches(
      std::span<const StreamRequest> requests,
      std::vector<std::string> &batches, std::string &error) const override {
    batches.clear();
    std::size_t accepted = 0;
    for (const auto &request : requests) {
      if (!valid_utf8_symbol(request.venue_symbol)) {
        continue;
      }
      ++accepted;
      const std::string prefix =
          product_ == utils::md::ProductType::Spot ? "spot." : "futures.";
      if (request.ticker) {
        const auto channel = request.ticker_channel.empty()
                                 ? prefix + "book_ticker"
                                 : std::string(request.ticker_channel);
        if (channel != prefix + "book_ticker") {
          error = "unsupported Gate ticker channel";
          return false;
        }
        batches.push_back(
            gate_subscription(channel, request.venue_symbol, false));
      }
      if (request.orderbook) {
        const auto channel =
            request.orderbook_channel.empty()
                ? prefix + "order_book_update"
                : std::string(request.orderbook_channel);
        if (channel != prefix + "order_book_update") {
          error = "unsupported Gate order-book channel";
          return false;
        }
        batches.push_back(
            gate_subscription(
                channel, request.venue_symbol, true,
                request.update_interval_ms == 0
                    ? 20
                    : request.update_interval_ms,
                product_ == utils::md::ProductType::Perpetual));
      }
    }
    if (!requests.empty() && accepted == 0) {
      error = "no valid Gate venue symbols";
      return false;
    }
    error.clear();
    return true;
  }

  bool parse_ws(std::string_view json, NormalizedEvent &event,
                std::string &error) override {
#ifndef MDS_HAS_SIMDJSON
    (void)json;
    (void)event;
    error = "simdjson support was not compiled";
    return false;
#else
    event.reset();
    try {
      auto document = parse(json);
      auto object = document.get_object().value();
      const auto event_name = optional_string(object, "event");
      const auto channel = optional_string(object, "channel");
      if (event_name == "subscribe") {
        auto result = object["result"].get_object();
        if (!result.error() &&
            optional_string(result.value(), "status") != "success") {
          event.type = AdapterEventType::SubscribeError;
          const auto symbol = optional_string(result.value(), "s");
          if (!symbol.empty() && !copy_symbol(symbol, event)) {
            error = "Gate rejection symbol exceeds fixed capacity";
            return false;
          }
        } else {
          event.type = AdapterEventType::SubscribeAck;
        }
        error.clear();
        return true;
      }
      if (event_name == "error") {
        event.type = AdapterEventType::SubscribeError;
        error.clear();
        return true;
      }
      if (channel.ends_with(".pong")) {
        event.type = AdapterEventType::Pong;
        error.clear();
        return true;
      }
      if (event_name != "update") {
        return true;
      }

      auto result = object["result"].get_object().value();
      const auto symbol = optional_string(result, "s");
      const auto *found = find_scales(symbol);
      if (found == nullptr || !copy_symbol(symbol, event)) {
        error = "Gate event has unknown or invalid symbol";
        return false;
      }
      const auto &scales = *found;
      event.exchange_time_ms = optional_uint(result, "t");
      if (event.exchange_time_ms == 0) {
        event.exchange_time_ms = optional_uint(result, "E");
      }
      event.final_sequence = optional_uint(result, "u");
      event.first_sequence = optional_uint(result, "U");
      if (event.first_sequence == 0) {
        event.first_sequence = event.final_sequence;
      }
      if (channel.ends_with(".book_ticker")) {
        const auto bid_raw = result["b"].value();
        const auto bid_quantity_raw = result["B"].value();
        const auto ask_raw = result["a"].value();
        const auto ask_quantity_raw = result["A"].value();
        const auto bid_text = decimal_text(bid_raw);
        const auto ask_text = decimal_text(ask_raw);
        if (bid_text.empty() || ask_text.empty() ||
            empty_string_value(bid_quantity_raw) ||
            empty_string_value(ask_quantity_raw)) {
          event.reset();
          error.clear();
          return true;
        }
        if (!unsigned_decimal_to_fixed(
                bid_text, scales.price, event.bid.price) ||
            !unsigned_decimal_to_fixed(
                ask_text, scales.price, event.ask.price)) {
          error =
              decimal_scale_mismatch(bid_text, scales.price) ||
                      decimal_scale_mismatch(ask_text, scales.price)
                  ? "invalid Gate BBO reason=scale field=price"
                  : "invalid Gate BBO price";
          return false;
        }
        utils::md::Level bid_quantity;
        utils::md::Level ask_quantity;
        const auto bid_result = parse_gate_level_pair(
            bid_raw, bid_quantity_raw, scales, bid_quantity);
        if (bid_result != LevelParseError::None) {
          set_level_error(error, "BBO", symbol, "bid", bid_result);
          return false;
        }
        const auto ask_result = parse_gate_level_pair(
            ask_raw, ask_quantity_raw, scales, ask_quantity);
        if (ask_result != LevelParseError::None) {
          set_level_error(error, "BBO", symbol, "ask", ask_result);
          return false;
        }
        event.bid.quantity = bid_quantity.quantity;
        event.ask.quantity = ask_quantity.quantity;
        if (event.bid.price <= 0 || event.bid.quantity <= 0 ||
            event.ask.price <= 0 || event.ask.quantity <= 0) {
          event.reset();
          error.clear();
          return true;
        }
        event.type = AdapterEventType::Bbo;
        error.clear();
        return true;
      }
      if (!channel.ends_with(".order_book_update")) {
        return true;
      }
      auto full = result["full"].get_bool();
      const bool full_snapshot = !full.error() && full.value();
      event.type = full_snapshot ? AdapterEventType::BookSnapshot
                                 : AdapterEventType::BookDelta;
      event.sequence_reset = full_snapshot;
      auto raw_bids = result["b"].get_array();
      if (raw_bids.error()) {
        set_level_error(error, "order-book update", symbol, "bid",
                        LevelParseError::Shape);
        return false;
      }
      const auto bid_result =
          parse_side(raw_bids.value(), scales, event.bids);
      if (bid_result != LevelParseError::None) {
        set_level_error(error, "order-book update", symbol, "bid",
                        bid_result);
        if (bid_result == LevelParseError::Capacity) {
          append_capacity_details(error, maximum_levels_,
                                  event.bids.size() + 1, channel, "update");
        }
        return false;
      }
      auto raw_asks = result["a"].get_array();
      if (raw_asks.error()) {
        set_level_error(error, "order-book update", symbol, "ask",
                        LevelParseError::Shape);
        return false;
      }
      const auto ask_result =
          parse_side(raw_asks.value(), scales, event.asks);
      if (ask_result != LevelParseError::None) {
        set_level_error(error, "order-book update", symbol, "ask",
                        ask_result);
        if (ask_result == LevelParseError::Capacity) {
          append_capacity_details(error, maximum_levels_,
                                  event.asks.size() + 1, channel, "update");
        }
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
                ? "/api/v4/spot/currency_pairs"
                : "/api/v4/futures/usdt/contracts",
            {}, {}};
  }

  [[nodiscard]] HttpRequestSpec
  discovery_turnover_request() const override {
    return {HttpRequestSpec::Method::Get,
            product_ == utils::md::ProductType::Spot
                ? "/api/v4/spot/tickers"
                : "/api/v4/futures/usdt/tickers",
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
      auto entries = parse(json).get_array().value();
      std::vector<std::uint64_t> turnovers(metadata.size());
      std::vector<bool> matched(metadata.size());
      const std::string_view symbol_field =
          product_ == utils::md::ProductType::Spot ? "currency_pair"
                                                   : "contract";
      const std::string_view turnover_field =
          product_ == utils::md::ProductType::Spot ? "quote_volume"
                                                   : "volume_24h_quote";
      for (auto raw_entry : entries) {
        auto entry = raw_entry.get_object().value();
        auto symbol_result = entry[symbol_field].get_string();
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
          error = "duplicate Gate ticker symbol: ";
          error.append(symbol);
          return false;
        }
        matched[index] = true;
        auto turnover = entry[turnover_field].get_string();
        if (turnover.error() ||
            !mds::exchange::decimal_to_turnover(turnover.value(),
                                                turnovers[index])) {
          error = "invalid Gate ticker turnover: ";
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
      error = "malformed Gate tickers response: ";
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
    metadata.clear();
    scales_.clear();
    try {
      auto entries = parse(json).get_array().value();
      for (const auto &request : requests) {
        bool found = false;
        for (auto raw : entries) {
          auto entry = raw.get_object().value();
          const auto venue_symbol =
              optional_string(entry, product_ == utils::md::ProductType::Spot
                                         ? "id"
                                         : "name");
          if (venue_symbol != request.venue_symbol) {
            continue;
          }
          InstrumentMetadata parsed;
          parsed.canonical_symbol = request.canonical_symbol;
          parsed.venue_symbol = venue_symbol;
          GateScales scales;
          if (product_ == utils::md::ProductType::Spot) {
            parsed.base_asset = optional_string(entry, "base");
            parsed.quote_asset = optional_string(entry, "quote");
            parsed.settle_asset = parsed.quote_asset;
            auto price_precision = entry["precision"].get_uint64().value();
            auto amount_precision =
                entry["amount_precision"].get_uint64().value();
            if (price_precision > 18 || amount_precision > 18) {
              error = "Gate precision exceeds fixed-point capacity";
              return false;
            }
            scales.price = static_cast<std::uint8_t>(price_precision);
            scales.quantity = std::max(
                static_cast<std::uint8_t>(amount_precision),
                kSpotQuantityRepresentationScale);
            parsed.tick_size = 1;
            parsed.lot_size = 1;
            for (std::uint8_t digit =
                     static_cast<std::uint8_t>(amount_precision);
                 digit < scales.quantity; ++digit) {
              if (parsed.lot_size >
                  std::numeric_limits<std::int64_t>::max() / 10) {
                error = "Gate spot lot size overflows";
                return false;
              }
              parsed.lot_size *= 10;
            }
          } else {
            const auto separator = venue_symbol.find('_');
            if (separator == std::string_view::npos) {
              error = "invalid Gate futures contract name";
              return false;
            }
            parsed.base_asset = venue_symbol.substr(0, separator);
            parsed.quote_asset = venue_symbol.substr(separator + 1);
            parsed.settle_asset = "USDT";
            const auto tick = decimal_text(entry["order_price_round"].value());
            const auto multiplier =
                decimal_text(entry["quanto_multiplier"].value());
            std::uint8_t multiplier_scale = 0;
            if (!scale_for_decimal(tick, scales.price) ||
                !unsigned_decimal_to_fixed(tick, scales.price,
                                           parsed.tick_size) ||
                !scale_for_decimal(multiplier, multiplier_scale) ||
                !unsigned_decimal_to_fixed(multiplier, multiplier_scale,
                                           scales.contract_multiplier) ||
                scales.contract_multiplier <= 0) {
              error = "invalid Gate futures price or contract multiplier";
              return false;
            }
            scales.contracts = true;
            parsed.contract_multiplier = scales.contract_multiplier;
            parsed.contract_multiplier_scale = multiplier_scale;
            auto enable_decimal_field = entry["enable_decimal"].get_bool();
            const bool enable_decimal =
                !enable_decimal_field.error() &&
                enable_decimal_field.value();
            std::int64_t minimum_contracts = 1;
            std::uint8_t contract_size_scale = 0;
            auto minimum = entry["order_size_min"];
            if (!minimum.error()) {
              auto minimum_text = minimum.value().get_string();
              if (!minimum_text.error() && enable_decimal) {
                if (!scale_for_decimal(minimum_text.value(),
                                       contract_size_scale) ||
                    !unsigned_decimal_to_fixed(
                        minimum_text.value(), contract_size_scale,
                        minimum_contracts)) {
                  error = "invalid Gate minimum order size";
                  return false;
                }
              } else {
                if (!integral_value(minimum.value(), minimum_contracts)) {
                  error = "invalid Gate minimum order size";
                  return false;
                }
                if (minimum_contracts == 0) {
                  if (!enable_decimal) {
                    error = "invalid Gate minimum order size";
                    return false;
                  }
                  contract_size_scale = 1;
                  minimum_contracts = 1;
                }
              }
            }
            if (static_cast<unsigned>(contract_size_scale) +
                    multiplier_scale >
                18U) {
              error = "Gate quantity scale overflows";
              return false;
            }
            scales.contract_size_scale = contract_size_scale;
            scales.quantity = static_cast<std::uint8_t>(
                multiplier_scale + contract_size_scale);
            if (minimum_contracts <= 0 ||
                minimum_contracts >
                    std::numeric_limits<std::int64_t>::max() /
                        scales.contract_multiplier) {
              error = "Gate minimum order size overflows";
              return false;
            }
            parsed.lot_size =
                minimum_contracts * scales.contract_multiplier;
          }
          parsed.price_scale = scales.price;
          parsed.quantity_scale = scales.quantity;
          if (parsed.tick_size <= 0 || parsed.lot_size <= 0) {
            error = "invalid Gate instrument increments";
            return false;
          }
          scales_.emplace(parsed.venue_symbol, scales);
          metadata.push_back(std::move(parsed));
          found = true;
          break;
        }
        if (!found) {
          continue;
        }
      }
      error.clear();
      return true;
    } catch (const simdjson::simdjson_error &exception) {
      error = exception.what();
      metadata.clear();
      scales_.clear();
      return false;
    }
#endif
  }

  bool upsert_metadata(
      std::string_view json, std::span<const StreamRequest> requests,
      std::vector<InstrumentMetadata> &metadata,
      std::string &error) override {
    auto existing = std::move(scales_);
    std::vector<InstrumentMetadata> parsed;
    const bool ok = parse_metadata(json, requests, parsed, error);
    auto refreshed = std::move(scales_);
    scales_ = std::move(existing);
    if (!ok) {
      return false;
    }
    for (auto &[symbol, scales] : refreshed) {
      scales_.insert_or_assign(std::move(symbol), scales);
    }
    metadata = std::move(parsed);
    error.clear();
    return true;
  }

  [[nodiscard]] bool needs_rest_snapshot() const noexcept override {
    return true;
  }

  [[nodiscard]] HttpRequestSpec
  snapshot_request(std::string_view venue_symbol,
                   std::size_t depth) const override {
    const auto limited =
        depth == 0 ? kRestSnapshotLevels
                   : std::min(depth, kRestSnapshotLevels);
    std::string target =
        product_ == utils::md::ProductType::Spot
            ? "/api/v4/spot/order_book?currency_pair="
            : "/api/v4/futures/usdt/order_book?contract=";
    target += venue_symbol;
    target += "&limit=" + std::to_string(limited) + "&with_id=true";
    return {HttpRequestSpec::Method::Get, std::move(target), {}, {}};
  }

  bool parse_snapshot(std::string_view json, std::string_view venue_symbol,
                      NormalizedEvent &event, std::string &error) override {
#ifndef MDS_HAS_SIMDJSON
    (void)json;
    (void)venue_symbol;
    (void)event;
    error = "simdjson support was not compiled";
    return false;
#else
    event.reset();
    const auto *found = find_scales(venue_symbol);
    if (found == nullptr || !copy_symbol(venue_symbol, event)) {
      error = "Gate snapshot has unknown or invalid symbol";
      return false;
    }
    try {
      auto object = parse(json).get_object().value();
      event.type = AdapterEventType::BookSnapshot;
      event.first_sequence = object["id"].get_uint64().value();
      event.final_sequence = event.first_sequence;
      event.exchange_time_ms = optional_uint(object, "current");
      event.sequence_reset = true;
      auto raw_bids = object["bids"].get_array();
      if (raw_bids.error()) {
        set_level_error(error, "order-book snapshot", venue_symbol, "bid",
                        LevelParseError::Shape);
        return false;
      }
      const auto bid_result =
          parse_side(raw_bids.value(), *found, event.bids);
      if (bid_result != LevelParseError::None) {
        set_level_error(error, "order-book snapshot", venue_symbol, "bid",
                        bid_result);
        if (bid_result == LevelParseError::Capacity) {
          append_capacity_details(
              error, maximum_levels_, event.bids.size() + 1,
              product_ == utils::md::ProductType::Spot
                  ? "spot.order_book"
                  : "futures.order_book",
              "snapshot");
        }
        return false;
      }
      auto raw_asks = object["asks"].get_array();
      if (raw_asks.error()) {
        set_level_error(error, "order-book snapshot", venue_symbol, "ask",
                        LevelParseError::Shape);
        return false;
      }
      const auto ask_result =
          parse_side(raw_asks.value(), *found, event.asks);
      if (ask_result != LevelParseError::None) {
        set_level_error(error, "order-book snapshot", venue_symbol, "ask",
                        ask_result);
        if (ask_result == LevelParseError::Capacity) {
          append_capacity_details(
              error, maximum_levels_, event.asks.size() + 1,
              product_ == utils::md::ProductType::Spot
                  ? "spot.order_book"
                  : "futures.order_book",
              "snapshot");
        }
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

 protected:
  void classify_parse_failure(std::string_view,
                              const NormalizedEvent &event,
                              ParseFailure &failure) const noexcept override {
    if (classify_scale_mismatch(event, failure)) {
      return;
    }
    const auto diagnostic = failure.diagnostic_view();
    if (event.symbol_view().empty()) {
      return;
    }
    ParseFailureCode code{ParseFailureCode::None};
    if (diagnostic.find("reason=quantity") != std::string_view::npos) {
      code = ParseFailureCode::InvalidQuantity;
    } else if (diagnostic.find("reason=price") !=
                   std::string_view::npos ||
               diagnostic.find("invalid Gate BBO price") !=
                   std::string_view::npos) {
      code = ParseFailureCode::InvalidPrice;
    } else if (diagnostic.find("reason=capacity") !=
               std::string_view::npos) {
      code = ParseFailureCode::CapacityExceeded;
    } else if (diagnostic.find("reason=shape") !=
               std::string_view::npos) {
      code = ParseFailureCode::MalformedPayload;
    }
    if (code == ParseFailureCode::None) {
      return;
    }
    failure.category = ParseFailureCategory::DirtyData;
    failure.scope = ParseFailureScope::Symbol;
    failure.code = code;
    (void)failure.set_symbol(event.symbol_view());
  }

 private:
  const GateScales *find_scales(std::string_view symbol) const noexcept {
    for (const auto &entry : scales_) {
      if (entry.first == symbol) {
        return &entry.second;
      }
    }
    return nullptr;
  }

#ifdef MDS_HAS_SIMDJSON
  simdjson::dom::element parse(std::string_view json) {
    if (json.size() > kMaximumJsonBytes) {
      throw simdjson::simdjson_error(simdjson::CAPACITY);
    }
    std::memcpy(json_buffer_.data(), json.data(), json.size());
    std::memset(json_buffer_.data() + json.size(), 0,
                simdjson::SIMDJSON_PADDING);
    return parser_
        .parse(json_buffer_.data(), json.size(), json_buffer_.size())
        .value();
  }

  LevelParseError parse_side(
      simdjson::dom::array raw, const GateScales &scales,
      std::vector<utils::md::Level> &output) const {
    output.clear();
    for (auto value : raw) {
      if (output.size() >= maximum_levels_ ||
          output.size() >= output.capacity()) {
        return LevelParseError::Capacity;
      }
      utils::md::Level level;
      const auto parsed = parse_gate_level(value, scales, level);
      if (parsed != LevelParseError::None) {
        return parsed;
      }
      output.push_back(level);
    }
    return LevelParseError::None;
  }

  static LevelParseError parse_gate_level_pair(
      simdjson::dom::element price, simdjson::dom::element quantity,
      const GateScales &scales, utils::md::Level &level) {
    const auto price_text = decimal_text(price);
    if (!unsigned_decimal_to_fixed(price_text, scales.price, level.price)) {
      return decimal_scale_mismatch(price_text, scales.price)
                 ? LevelParseError::Scale
                 : LevelParseError::Price;
    }
    if (!parse_quantity(quantity, scales, level.quantity)) {
      return LevelParseError::Quantity;
    }
    return LevelParseError::None;
  }

  simdjson::dom::parser parser_;
  std::vector<char> json_buffer_;
#endif
  utils::md::ProductType product_;
  std::size_t maximum_levels_;
  std::unordered_map<std::string, GateScales> scales_;
};

}  // namespace

std::unique_ptr<VenueAdapter>
make_gate_adapter(utils::md::ProductType product,
                  std::size_t max_levels_per_side) {
  if ((product != utils::md::ProductType::Spot &&
       product != utils::md::ProductType::Perpetual) ||
      max_levels_per_side == 0) {
    return {};
  }
  return std::make_unique<GateAdapter>(product, max_levels_per_side);
}

}  // namespace mds::exchange
