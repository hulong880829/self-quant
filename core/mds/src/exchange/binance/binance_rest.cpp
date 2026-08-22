#include "mds/exchange/binance/binance_rest.h"

#include <array>
#include <cstring>
#include <limits>
#include <unordered_map>

#ifdef MDS_HAS_SIMDJSON
#include <simdjson.h>
#endif

namespace mds::exchange::binance {
namespace {

#ifdef MDS_HAS_SIMDJSON
bool decimal_scale(std::string_view text, std::int8_t &scale) noexcept {
  if (text.empty() || text.front() == '-' || text.front() == '+') {
    return false;
  }
  const auto dot = text.find('.');
  if (dot != std::string_view::npos &&
      text.find('.', dot + 1) != std::string_view::npos) {
    return false;
  }
  for (const char value : text) {
    if (value != '.' && (value < '0' || value > '9')) {
      return false;
    }
  }
  const auto integer_digits =
      dot == std::string_view::npos ? text.size() : dot;
  if (integer_digits == 0) {
    return false;
  }
  std::size_t end = text.size();
  while (dot != std::string_view::npos && end > dot + 1 &&
         text[end - 1] == '0') {
    --end;
  }
  const auto digits =
      dot == std::string_view::npos || end == dot + 1 ? 0 : end - dot - 1;
  if (digits > 18) {
    return false;
  }
  scale = static_cast<std::int8_t>(digits);
  return true;
}

bool decimal_to_fixed(std::string_view text, std::int8_t scale,
                      std::int64_t &out) noexcept {
  if (scale < 0 || scale > 18 || text.empty() || text.front() == '-' ||
      text.front() == '+') {
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
    if (after_decimal && fractional_digits >= static_cast<std::size_t>(scale)) {
      if (character != '0') {
        return false;
      }
      continue;
    }
    const auto digit = static_cast<std::uint64_t>(character - '0');
    if (value >
        (static_cast<std::uint64_t>(std::numeric_limits<std::int64_t>::max()) -
         digit) /
            10U) {
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
  while (fractional_digits < static_cast<std::size_t>(scale)) {
    if (value > static_cast<std::uint64_t>(
                    std::numeric_limits<std::int64_t>::max() / 10)) {
      return false;
    }
    value *= 10U;
    ++fractional_digits;
  }
  out = static_cast<std::int64_t>(value);
  return true;
}

std::string_view required_string(simdjson::dom::object object,
                                 std::string_view name) {
  return object[name].get_string().value();
}

std::string optional_string(simdjson::dom::object object,
                            std::string_view name) {
  auto value = object[name].get_string();
  return value.error() ? std::string{} : std::string(value.value());
}

std::uint64_t optional_uint64(simdjson::dom::object object,
                              std::string_view name) {
  auto value = object[name].get_uint64();
  return value.error() ? 0 : value.value();
}

bool parse_precision(simdjson::dom::object object,
                     std::string_view name,
                     std::int8_t &precision) {
  auto value = object[name].get_uint64();
  if (value.error() || value.value() > 18) {
    return false;
  }
  precision = static_cast<std::int8_t>(value.value());
  return true;
}

bool parse_price_filter(simdjson::dom::object filter,
                        InstrumentMetadata &metadata,
                        bool derive_scale) {
  const auto tick = required_string(filter, "tickSize");
  if ((derive_scale && !decimal_scale(tick, metadata.price_scale)) ||
      !decimal_to_fixed(tick, metadata.price_scale,
                        metadata.price_filter.tick_size) ||
      metadata.price_filter.tick_size <= 0) {
    return false;
  }
  return decimal_to_fixed(required_string(filter, "minPrice"),
                          metadata.price_scale,
                          metadata.price_filter.min_price) &&
         decimal_to_fixed(required_string(filter, "maxPrice"),
                          metadata.price_scale,
                          metadata.price_filter.max_price) &&
         metadata.price_filter.min_price <= metadata.price_filter.max_price;
}

bool parse_lot_size(simdjson::dom::object filter,
                    InstrumentMetadata &metadata,
                    bool derive_scale) {
  const auto step = required_string(filter, "stepSize");
  if ((derive_scale && !decimal_scale(step, metadata.quantity_scale)) ||
      !decimal_to_fixed(step, metadata.quantity_scale,
                        metadata.lot_size.step_size) ||
      metadata.lot_size.step_size <= 0) {
    return false;
  }
  return decimal_to_fixed(required_string(filter, "minQty"),
                          metadata.quantity_scale,
                          metadata.lot_size.min_quantity) &&
         decimal_to_fixed(required_string(filter, "maxQty"),
                          metadata.quantity_scale,
                          metadata.lot_size.max_quantity) &&
         metadata.lot_size.min_quantity <= metadata.lot_size.max_quantity;
}

bool parse_exchange_symbol(Profile profile, simdjson::dom::object entry,
                           std::string_view symbol,
                           InstrumentMetadata &parsed,
                           std::string &error) {
  parsed = {};
  parsed.profile = profile;
  parsed.venue_symbol = std::string(symbol);
  parsed.base_asset = std::string(required_string(entry, "baseAsset"));
  parsed.quote_asset = std::string(required_string(entry, "quoteAsset"));
  parsed.status = std::string(required_string(entry, "status"));
  if (profile == Profile::Spot) {
    parsed.pair = parsed.venue_symbol;
    parsed.settle_asset = parsed.quote_asset;
  } else {
    parsed.pair = optional_string(entry, "pair");
    parsed.settle_asset = std::string(required_string(entry, "marginAsset"));
    parsed.contract_type =
        std::string(required_string(entry, "contractType"));
    parsed.underlying_type = optional_string(entry, "underlyingType");
    parsed.onboard_time_ms = optional_uint64(entry, "onboardDate");
    parsed.delivery_time_ms = optional_uint64(entry, "deliveryDate");
    if (!parse_precision(entry, "pricePrecision", parsed.price_scale) ||
        !parse_precision(entry, "quantityPrecision",
                         parsed.quantity_scale)) {
      error = "invalid Binance USD-M market data precision";
      return false;
    }
    auto contract_size = entry["contractSize"].get_string();
    if (!contract_size.error()) {
      const auto text = std::string_view(contract_size.value());
      if (!decimal_scale(text, parsed.contract_size_scale) ||
          !decimal_to_fixed(text, parsed.contract_size_scale,
                            parsed.contract_size) ||
          parsed.contract_size <= 0) {
        error = "invalid Binance contractSize";
        return false;
      }
    }
  }

  bool has_price_filter = false;
  bool has_lot_size = false;
  auto filters = entry["filters"].get_array().value();
  for (simdjson::dom::element raw_filter : filters) {
    auto filter = raw_filter.get_object().value();
    const auto type = required_string(filter, "filterType");
    if (type == "PRICE_FILTER") {
      if (has_price_filter ||
          !parse_price_filter(filter, parsed, profile == Profile::Spot)) {
        error = "invalid or duplicate Binance PRICE_FILTER";
        return false;
      }
      has_price_filter = true;
    } else if (type == "LOT_SIZE") {
      if (has_lot_size ||
          !parse_lot_size(filter, parsed, profile == Profile::Spot)) {
        error = "invalid or duplicate Binance LOT_SIZE";
        return false;
      }
      has_lot_size = true;
    }
  }
  if (!has_price_filter || !has_lot_size) {
    error = "Binance symbol is missing PRICE_FILTER or LOT_SIZE";
    return false;
  }
  return true;
}

bool parse_side(simdjson::dom::array side, std::int8_t price_scale,
                std::int8_t quantity_scale, std::size_t maximum_levels,
                std::vector<PriceLevel> &output,
                std::string_view &failure) {
  output.clear();
  failure = {};
  for (simdjson::dom::element raw_level : side) {
    if (output.size() == maximum_levels) {
      failure = "capacity";
      return false;
    }
    auto level = raw_level.get_array().value();
    PriceLevel parsed;
    std::size_t field = 0;
    for (simdjson::dom::element raw_value : level) {
      if (field >= 2) {
        failure = "format";
        return false;
      }
      const auto value = raw_value.get_string().value();
      if (!(field == 0
                ? decimal_to_fixed(value, price_scale, parsed.price)
                : decimal_to_fixed(value, quantity_scale, parsed.quantity))) {
        failure = "precision or overflow";
        return false;
      }
      ++field;
    }
    if (field != 2) {
      failure = "format";
      return false;
    }
    output.push_back(parsed);
  }
  return true;
}
#endif

} // namespace

#ifdef MDS_HAS_SIMDJSON
struct RestParser::Impl {
  simdjson::dom::parser parser;
  std::vector<char> buffer;

  explicit Impl(std::size_t capacity)
      : buffer(capacity + simdjson::SIMDJSON_PADDING) {}

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

RestParser::RestParser(std::size_t capacity)
#ifdef MDS_HAS_SIMDJSON
    : impl_(new Impl(capacity))
#else
    : capacity_(capacity)
#endif
{
}

RestParser::~RestParser() {
#ifdef MDS_HAS_SIMDJSON
  delete impl_;
#endif
}

bool RestParser::parse_exchange_info(Profile profile, std::string_view json,
                                     std::string_view symbol,
                                     InstrumentMetadata &out,
                                     std::string &error) {
  const std::array<std::string_view, 1> symbols{symbol};
  std::vector<InstrumentMetadata> parsed;
  if (!parse_exchange_info(profile, json, symbols, parsed, error)) {
    return false;
  }
  out = std::move(parsed.front());
  return true;
}

bool RestParser::parse_exchange_info(
    Profile profile, std::string_view json,
    std::span<const std::string_view> symbols,
    std::vector<InstrumentMetadata> &out, std::string &error) {
#ifndef MDS_HAS_SIMDJSON
  (void)profile;
  (void)json;
  (void)symbols;
  (void)out;
  error = "simdjson support was not compiled";
  return false;
#else
  if (symbols.empty()) {
    error = "Binance exchangeInfo requires at least one symbol";
    return false;
  }
  try {
    std::unordered_map<std::string_view, std::vector<std::size_t>> pending;
    pending.reserve(symbols.size());
    for (std::size_t index = 0; index < symbols.size(); ++index) {
      if (symbols[index].empty()) {
        error = "Binance exchangeInfo symbol is empty";
        return false;
      }
      pending[symbols[index]].push_back(index);
    }
    std::vector<InstrumentMetadata> parsed(symbols.size());
    auto exchange_symbols =
        impl_->parse(json)["symbols"].get_array().value();
    for (simdjson::dom::element raw_symbol : exchange_symbols) {
      auto entry = raw_symbol.get_object().value();
      const auto venue_symbol = required_string(entry, "symbol");
      const auto requested = pending.find(venue_symbol);
      if (requested == pending.end()) {
        continue;
      }
      InstrumentMetadata metadata;
      if (!parse_exchange_symbol(profile, entry, venue_symbol, metadata,
                                 error)) {
        return false;
      }
      for (const auto index : requested->second) {
        parsed[index] = metadata;
      }
      pending.erase(requested);
      if (pending.empty()) {
        break;
      }
    }
    if (!pending.empty()) {
      error = "Binance symbol was not found in exchangeInfo";
      return false;
    }
    out = std::move(parsed);
    error.clear();
    return true;
  } catch (const simdjson::simdjson_error &exception) {
    error = exception.what();
    return false;
  }
#endif
}

bool RestParser::parse_depth(Profile profile, std::string_view json,
                             const InstrumentMetadata &metadata,
                             DepthSnapshot &out, std::string &error) {
#ifndef MDS_HAS_SIMDJSON
  (void)profile;
  (void)json;
  (void)metadata;
  (void)out;
  error = "simdjson support was not compiled";
  return false;
#else
  if (metadata.profile != profile || metadata.price_scale < 0 ||
      metadata.quantity_scale < 0) {
    error = "snapshot profile does not match instrument metadata";
    return false;
  }
  try {
    auto document = impl_->parse(json);
    DepthSnapshot parsed;
    parsed.last_update_id = document["lastUpdateId"].get_uint64().value();
    parsed.price_exponent = static_cast<std::int8_t>(-metadata.price_scale);
    parsed.quantity_exponent =
        static_cast<std::int8_t>(-metadata.quantity_scale);
    const std::size_t maximum_levels =
        profile == Profile::Spot ? 5000U : 1000U;
    std::string_view failure;
    if (!parse_side(document["bids"].get_array().value(),
                    metadata.price_scale, metadata.quantity_scale,
                    maximum_levels, parsed.bids, failure)) {
      error = "invalid Binance bid depth level: ";
      error.append(failure);
      return false;
    }
    if (!parse_side(document["asks"].get_array().value(),
                    metadata.price_scale, metadata.quantity_scale,
                    maximum_levels, parsed.asks, failure)) {
      error = "invalid Binance ask depth level: ";
      error.append(failure);
      return false;
    }
    out = std::move(parsed);
    error.clear();
    return true;
  } catch (const simdjson::simdjson_error &exception) {
    error = exception.what();
    return false;
  }
#endif
}

} // namespace mds::exchange::binance
