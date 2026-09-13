#include "mds/exchange/bybit/bybit_adapter.h"
#include "mds/exchange/symbol_policy.h"

#include <algorithm>
#include <array>
#include <cctype>
#include <charconv>
#include <cstring>
#include <limits>
#include <set>
#include <string>
#include <utility>

#ifdef MDS_HAS_SIMDJSON
#include <simdjson.h>
#endif

namespace mds::exchange::bybit {
namespace {

constexpr std::size_t kMaximumSubscriptionsPerRequest = 10;
constexpr std::size_t kMaximumSubscriptionBytes = 21'000;
constexpr std::size_t kMaximumTrackedSymbols = 4096;
constexpr std::size_t kMaximumJsonBytes = 2U << 20U;

bool valid_product(utils::md::ProductType product) noexcept {
  return product == utils::md::ProductType::Spot ||
         product == utils::md::ProductType::Perpetual;
}

std::string_view category(utils::md::ProductType product) noexcept {
  return product == utils::md::ProductType::Spot ? "spot" : "linear";
}

bool append_url_component(std::string &target,
                          std::string_view value) {
  if (value.empty()) {
    return false;
  }
  constexpr char hex[] = "0123456789ABCDEF";
  for (const char raw : value) {
    const auto character = static_cast<unsigned char>(raw);
    const bool unreserved =
        (character >= 'A' && character <= 'Z') ||
        (character >= 'a' && character <= 'z') ||
        (character >= '0' && character <= '9') ||
        raw == '-' || raw == '.' || raw == '_' || raw == '~';
    if (unreserved) {
      target.push_back(raw);
      continue;
    }
    target.push_back('%');
    target.push_back(hex[character >> 4U]);
    target.push_back(hex[character & 0x0fU]);
  }
  return true;
}

bool valid_encoded_cursor(std::string_view value) noexcept {
  for (std::size_t index = 0; index < value.size(); ++index) {
    const char raw = value[index];
    const auto character = static_cast<unsigned char>(raw);
    const bool unreserved =
        (character >= 'A' && character <= 'Z') ||
        (character >= 'a' && character <= 'z') ||
        (character >= '0' && character <= '9') ||
        raw == '-' || raw == '.' || raw == '_' || raw == '~';
    if (unreserved) {
      continue;
    }
    const auto is_hex = [](char value) {
      return (value >= '0' && value <= '9') ||
             (value >= 'A' && value <= 'F') ||
             (value >= 'a' && value <= 'f');
    };
    if (raw != '%' || index + 2 >= value.size() ||
        !is_hex(value[index + 1]) || !is_hex(value[index + 2])) {
      return false;
    }
    index += 2;
  }
  return true;
}

bool valid_topic_component(std::string_view value) noexcept {
  return mds::exchange::valid_utf8_symbol(value);
}

bool valid_orderbook_channel(std::string_view channel) noexcept {
  constexpr std::string_view prefix = "orderbook.";
  if (!channel.starts_with(prefix) || channel.size() == prefix.size()) {
    return false;
  }
  return std::all_of(channel.begin() + static_cast<std::ptrdiff_t>(prefix.size()),
                     channel.end(), [](char value) {
                       return value >= '0' && value <= '9';
                     });
}

bool channel_depth(std::string_view topic, std::size_t &depth) noexcept {
  constexpr std::string_view prefix = "orderbook.";
  if (!topic.starts_with(prefix)) {
    return false;
  }
  const auto separator = topic.find('.', prefix.size());
  if (separator == std::string_view::npos ||
      separator + 1U >= topic.size()) {
    return false;
  }
  const auto digits = topic.substr(prefix.size(), separator - prefix.size());
  const auto result =
      std::from_chars(digits.data(), digits.data() + digits.size(), depth);
  return result.ec == std::errc{} &&
         result.ptr == digits.data() + digits.size() && depth != 0;
}

bool topic_symbol(std::string_view topic, std::string_view &symbol) noexcept {
  std::size_t ignored_depth{};
  if (!channel_depth(topic, ignored_depth)) {
    return false;
  }
  const auto separator = topic.find('.', std::string_view("orderbook.").size());
  symbol = topic.substr(separator + 1U);
  return valid_topic_component(symbol);
}

std::uint8_t displayed_scale(std::string_view value) noexcept {
  const auto dot = value.find('.');
  if (dot == std::string_view::npos) {
    return 0;
  }
  std::size_t end = value.size();
  while (end > dot + 1U && value[end - 1U] == '0') {
    --end;
  }
  const auto digits = end > dot + 1U ? end - dot - 1U : 0U;
  return static_cast<std::uint8_t>(std::min<std::size_t>(digits, 255U));
}

struct SymbolState {
  std::array<char, 33> venue_symbol{};
  std::size_t venue_symbol_size{};
  std::array<char, 33> canonical_symbol{};
  std::size_t canonical_symbol_size{};
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
  bool has_scales{};
  std::uint64_t last_bbo_u{};
  std::uint64_t last_book_u{};
  std::uint64_t last_book_seq{};
  bool has_bbo_u{};
  bool has_book_sequence{};

  [[nodiscard]] std::string_view venue() const noexcept {
    return {venue_symbol.data(), venue_symbol_size};
  }
  [[nodiscard]] std::string_view canonical() const noexcept {
    return {canonical_symbol.data(), canonical_symbol_size};
  }
};

bool copy_fixed(std::string_view source, std::array<char, 33> &destination,
                std::size_t &size) noexcept {
  if (source.empty() || source.size() >= destination.size()) {
    return false;
  }
  destination.fill('\0');
  std::memcpy(destination.data(), source.data(), source.size());
  size = source.size();
  return true;
}

#ifdef MDS_HAS_SIMDJSON
std::string_view required_string(simdjson::dom::object object,
                                 std::string_view field) {
  return object[field].get_string().value();
}

bool response_code_ok(simdjson::dom::element document) {
  auto code = document["retCode"].get_int64();
  return !code.error() && code.value() == 0;
}

bool explicit_symbol_unavailable_message(std::string_view message) {
  std::string normalized;
  normalized.reserve(message.size());
  for (const char value : message) {
    normalized.push_back(static_cast<char>(
        std::tolower(static_cast<unsigned char>(value))));
  }
  constexpr std::array<std::string_view, 8> phrases{
      "invalid symbol", "symbol invalid", "symbol is invalid",
      "unknown symbol", "symbol not found", "symbol is not found",
      "symbol does not exist", "symbol doesn't exist"};
  return std::any_of(
      phrases.begin(), phrases.end(),
      [&normalized](std::string_view phrase) {
        return normalized.find(phrase) != std::string::npos;
      });
}

bool parse_level_scales(simdjson::dom::array levels,
                        std::uint8_t &price_scale,
                        std::uint8_t &quantity_scale) {
  for (auto raw_level : levels) {
    auto level = raw_level.get_array().value();
    auto iterator = level.begin();
    if (iterator == level.end()) {
      return false;
    }
    const auto price = (*iterator).get_string().value();
    ++iterator;
    if (iterator == level.end()) {
      return false;
    }
    const auto quantity = (*iterator).get_string().value();
    ++iterator;
    if (iterator != level.end()) {
      return false;
    }
    price_scale = std::max(price_scale, displayed_scale(price));
    quantity_scale = std::max(quantity_scale, displayed_scale(quantity));
  }
  return price_scale <= 18 && quantity_scale <= 18;
}

bool parse_levels(simdjson::dom::array levels, std::uint8_t price_scale,
                  std::uint8_t quantity_scale, std::size_t maximum,
                  std::vector<utils::md::Level> &output,
                  std::string &error) {
  output.clear();
  for (auto raw_level : levels) {
    if (output.size() >= maximum || output.size() >= output.capacity()) {
      error = "Bybit order book exceeds configured event capacity";
      return false;
    }
    auto level = raw_level.get_array().value();
    auto iterator = level.begin();
    if (iterator == level.end()) {
      error = "invalid Bybit order book level";
      return false;
    }
    const auto price = std::string_view((*iterator).get_string().value());
    ++iterator;
    if (iterator == level.end()) {
      error = "invalid Bybit order book level";
      return false;
    }
    const auto quantity = std::string_view((*iterator).get_string().value());
    ++iterator;
    if (iterator != level.end()) {
      error = "invalid Bybit order book level";
      return false;
    }
    utils::md::Level parsed;
    if (!mds::exchange::decimal_to_fixed(
            price, price_scale, parsed.price)) {
      error = decimal_scale_mismatch(price, price_scale)
                  ? "invalid Bybit order book reason=scale field=price"
                  : "invalid or imprecise Bybit order book level";
      return false;
    }
    if (!mds::exchange::decimal_to_fixed(
            quantity, quantity_scale, parsed.quantity)) {
      error = decimal_scale_mismatch(quantity, quantity_scale)
                  ? "invalid Bybit order book reason=scale field=quantity"
                  : "invalid or imprecise Bybit order book level";
      return false;
    }
    output.push_back(parsed);
  }
  return true;
}
#endif

class BybitAdapter final : public VenueAdapter {
 public:
  BybitAdapter(utils::md::ProductType product,
               std::size_t max_levels_per_side)
      : product_(product),
        max_levels_per_side_(max_levels_per_side == 0
                                 ? 1
                                 : max_levels_per_side)
#ifdef MDS_HAS_SIMDJSON
        ,
        parser_(kMaximumJsonBytes),
        json_buffer_(kMaximumJsonBytes + simdjson::SIMDJSON_PADDING)
#endif
  {}

  [[nodiscard]] utils::md::Venue venue() const noexcept override {
    return utils::md::Venue::Bybit;
  }

  [[nodiscard]] utils::md::ProductType product() const noexcept override {
    return product_;
  }

  [[nodiscard]] HeartbeatSpec heartbeat() const override {
    return {HeartbeatKind::JsonPing, R"({"op":"ping"})", 20'000};
  }

  void reset_connection_state() noexcept override {
    for (auto &state : states_) {
      state.last_bbo_u = 0;
      state.last_book_u = 0;
      state.last_book_seq = 0;
      state.has_bbo_u = false;
      state.has_book_sequence = false;
    }
  }

  bool build_subscription_batches(std::span<const StreamRequest> requests,
                                  std::vector<std::string> &batches,
                                  std::string &error) const override {
    if (!valid_product(product_)) {
      error = "Bybit supports only spot and perpetual products";
      return false;
    }

    std::vector<std::string> topics;
    topics.reserve(requests.size() * 2U);
    for (const auto &request : requests) {
      if (!valid_topic_component(request.venue_symbol) ||
          !mds::exchange::valid_utf8_symbol(request.canonical_symbol)) {
        continue;
      }
      if (!remember(request.venue_symbol, request.canonical_symbol, error)) {
        return false;
      }
      auto append_topic = [&](std::string_view configured,
                              std::string_view fallback) -> bool {
        const auto channel = configured.empty() ? fallback : configured;
        if (!valid_orderbook_channel(channel)) {
          error = "invalid Bybit order book channel";
          return false;
        }
        std::string topic;
        topic.reserve(channel.size() + request.venue_symbol.size() + 1U);
        topic.append(channel);
        topic.push_back('.');
        topic.append(request.venue_symbol);
        if (std::find(topics.begin(), topics.end(), topic) == topics.end()) {
          topics.push_back(std::move(topic));
        }
        return true;
      };
      if (request.ticker &&
          !append_topic(request.ticker_channel, "orderbook.1")) {
        return false;
      }
      if (request.orderbook &&
          !append_topic(request.orderbook_channel, "orderbook.50")) {
        return false;
      }
    }
    if (topics.empty()) {
      error = "no valid Bybit subscription topics";
      return false;
    }

    std::vector<std::string> built;
    for (std::size_t offset = 0; offset < topics.size();) {
      std::string batch{"{\"op\":\"subscribe\",\"args\":["};
      std::size_t count = 0;
      while (offset < topics.size() &&
             count < kMaximumSubscriptionsPerRequest) {
        const auto addition = topics[offset].size() + 2U + (count ? 1U : 0U);
        constexpr std::size_t closing_bytes = 2;
        if (batch.size() + addition + closing_bytes >
            kMaximumSubscriptionBytes) {
          if (count == 0) {
            error = "Bybit subscription topic exceeds request size limit";
            return false;
          }
          break;
        }
        if (count != 0) {
          batch.push_back(',');
        }
        batch.push_back('"');
        mds::exchange::append_json_escaped(batch, topics[offset]);
        batch.push_back('"');
        ++offset;
        ++count;
      }
      batch.append("]}");
      built.push_back(std::move(batch));
    }
    batches = std::move(built);
    error.clear();
    return true;
  }

  bool parse_ws(std::string_view json, NormalizedEvent &event,
                std::string &error) override {
    event.reset();
#ifndef MDS_HAS_SIMDJSON
    (void)json;
    error = "simdjson support was not compiled";
    return false;
#else
    try {
      auto document = parse(json);

      auto operation = document["op"].get_string();
      if (!operation.error()) {
        if (operation.value() == "pong") {
          event.type = AdapterEventType::Pong;
          error.clear();
          return true;
        }
        if (operation.value() == "subscribe" ||
            operation.value() == "unsubscribe") {
          auto success = document["success"].get_bool();
          event.type = !success.error() && success.value()
                           ? AdapterEventType::SubscribeAck
                           : AdapterEventType::SubscribeError;
          error.clear();
          return true;
        }
      }

      auto topic_result = document["topic"].get_string();
      if (topic_result.error()) {
        error.clear();
        return true;
      }
      const auto topic = std::string_view(topic_result.value());
      std::string_view venue_symbol;
      std::size_t depth{};
      if (!channel_depth(topic, depth) || !topic_symbol(topic, venue_symbol)) {
        error = "invalid Bybit order book topic";
        return false;
      }
      auto *state = find_or_add(venue_symbol, venue_symbol, error);
      if (state == nullptr) {
        return false;
      }

      const auto type = std::string_view(document["type"].get_string().value());
      const bool snapshot = type == "snapshot";
      if (!snapshot && type != "delta") {
        error = "unsupported Bybit order book message type";
        return false;
      }
      auto data = document["data"].get_object().value();
      const auto data_symbol = required_string(data, "s");
      if (data_symbol != venue_symbol) {
        error = "Bybit topic and payload symbols do not match";
        return false;
      }
      if (!set_symbol(*state, event, error)) {
        return false;
      }
      const auto update_id = data["u"].get_uint64().value();
      const auto sequence = data["seq"].get_uint64().value();

      if (depth == 1) {
        if (!snapshot) {
          error = "Bybit orderbook.1 must be a snapshot";
          return false;
        }
        if (state->has_bbo_u && update_id == state->last_bbo_u) {
          error.clear();
          return true;
        }
        auto bids = data["b"].get_array().value();
        auto asks = data["a"].get_array().value();
        if (!ensure_scales(*state, bids, asks, error) ||
            !parse_levels(bids, state->price_scale, state->quantity_scale, 1,
                          event.bids, error) ||
            !parse_levels(asks, state->price_scale, state->quantity_scale, 1,
                          event.asks, error) ||
            event.bids.size() != 1 || event.asks.size() != 1) {
          if (error.empty()) {
            error = "Bybit orderbook.1 snapshot must contain one bid and ask";
          }
          return false;
        }
        if (!set_symbol(*state, event, error)) {
          return false;
        }
        event.type = AdapterEventType::Bbo;
        event.first_sequence = update_id;
        event.final_sequence = update_id;
        event.exchange_time_ms = optional_timestamp(document);
        event.bid = event.bids.front();
        event.ask = event.asks.front();
        event.bids.clear();
        event.asks.clear();
        state->last_bbo_u = update_id;
        state->has_bbo_u = true;
        error.clear();
        return true;
      }

      const bool reset = snapshot || update_id == 1;
      if (!reset && state->has_book_sequence) {
        if (update_id == state->last_book_u ||
            update_id < state->last_book_u ||
            sequence < state->last_book_seq) {
          error.clear();
          return true;
        }
      }

      auto bids = data["b"].get_array().value();
      auto asks = data["a"].get_array().value();
      if (!ensure_scales(*state, bids, asks, error) ||
          !parse_levels(bids, state->price_scale, state->quantity_scale,
                        max_levels_per_side_, event.bids, error) ||
          !parse_levels(asks, state->price_scale, state->quantity_scale,
                        max_levels_per_side_, event.asks, error) ||
          !set_symbol(*state, event, error)) {
        return false;
      }
      event.type = snapshot ? AdapterEventType::BookSnapshot
                            : AdapterEventType::BookDelta;
      event.first_sequence = update_id;
      event.final_sequence = update_id;
      event.exchange_time_ms = optional_timestamp(document);
      event.sequence_reset = reset;
      state->last_book_u = update_id;
      state->last_book_seq = sequence;
      state->has_book_sequence = true;
      error.clear();
      return true;
    } catch (const simdjson::simdjson_error &exception) {
      event.reset();
      error = exception.what();
      return false;
    }
#endif
  }

  [[nodiscard]] HttpRequestSpec metadata_request() const override {
    HttpRequestSpec request;
    request.target = "/v5/market/instruments-info?category=";
    request.target.append(category(product_));
    return request;
  }

  [[nodiscard]] HttpRequestSpec
  discovery_metadata_request(std::string_view cursor) const override {
    auto request = metadata_request();
    if (product_ != utils::md::ProductType::Perpetual) {
      return request;
    }
    request.target.append("&limit=1000");
    if (!cursor.empty()) {
      request.target.append("&cursor=");
      request.target.append(cursor);
    }
    return request;
  }

  bool discovery_metadata_next_cursor(
      std::string_view json, std::string &cursor,
      std::string &error) override {
    cursor.clear();
    if (product_ != utils::md::ProductType::Perpetual) {
      error.clear();
      return true;
    }
#ifndef MDS_HAS_SIMDJSON
    (void)json;
    error = "simdjson support was not compiled";
    return false;
#else
    try {
      auto document = parse(json);
      if (!response_code_ok(document)) {
        error = "Bybit instruments-info pagination returned an error";
        return false;
      }
      auto result = document["result"].get_object();
      if (result.error()) {
        error = "Bybit instruments-info pagination result is missing";
        return false;
      }
      auto returned_category = result.value()["category"].get_string();
      if (returned_category.error() ||
          std::string_view(returned_category.value()) != category(product_)) {
        error = "Bybit instruments-info pagination category mismatch";
        return false;
      }
      auto next = result.value()["nextPageCursor"].get_string();
      if (next.error()) {
        error = "Bybit instruments-info nextPageCursor is missing or invalid";
        return false;
      }
      const std::string_view value(next.value());
      if (!valid_encoded_cursor(value)) {
        error = "Bybit instruments-info nextPageCursor is not URL encoded";
        return false;
      }
      cursor.assign(value);
      error.clear();
      return true;
    } catch (const simdjson::simdjson_error &exception) {
      error = "malformed Bybit instruments-info pagination response: ";
      error.append(exception.what());
      return false;
    }
#endif
  }

  [[nodiscard]] HttpRequestSpec
  discovery_turnover_request() const override {
    HttpRequestSpec request;
    request.target = "/v5/market/tickers?category=";
    request.target.append(category(product_));
    return request;
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
      auto document = parse(json);
      auto code = document["retCode"].get_int64();
      if (code.error() || code.value() != 0) {
        error = "Bybit tickers returned an error";
        return false;
      }
      auto result = document["result"].get_object().value();
      auto entries = result["list"].get_array().value();
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
          error = "duplicate Bybit ticker symbol: ";
          error.append(symbol);
          return false;
        }
        matched[index] = true;
        auto turnover = entry["turnover24h"].get_string();
        if (turnover.error() ||
            !mds::exchange::decimal_to_turnover(turnover.value(),
                                                turnovers[index])) {
          error = "invalid Bybit turnover24h: ";
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
      error = "malformed Bybit tickers response: ";
      error.append(exception.what());
      return false;
    }
#endif
  }

  bool build_metadata_request_batches(
      std::span<const StreamRequest> requests,
      std::vector<MetadataRequestBatch> &batches,
      std::string &error) const override {
    if (requests.empty()) {
      error = "Bybit metadata request requires at least one symbol";
      return false;
    }
    std::vector<MetadataRequestBatch> built;
    built.reserve(requests.size());
    for (std::size_t index = 0; index < requests.size(); ++index) {
      auto request = metadata_request();
      request.target.append("&symbol=");
      if (!append_url_component(request.target,
                                requests[index].venue_symbol)) {
        error = "invalid Bybit metadata symbol";
        return false;
      }
      built.push_back({std::move(request), index, 1, false, {}});
    }
    batches = std::move(built);
    error.clear();
    return true;
  }

  bool build_bootstrap_metadata_request_batches(
      std::span<const StreamRequest> requests,
      std::vector<MetadataRequestBatch> &batches,
      std::string &error) const override {
    if (product_ == utils::md::ProductType::Perpetual &&
        requests.size() > 1) {
      batches = {{bootstrap_instruments_info_request({}), 0, requests.size(),
                  true, {}}};
      error.clear();
      return true;
    }
    return build_metadata_request_batches(requests, batches, error);
  }

  bool apply_metadata_page_cursor(MetadataRequestBatch &batch,
                                  std::string_view cursor,
                                  std::string &error) const override {
    if (!batch.cursor_paginated) {
      error = "Bybit metadata batch is not cursor paginated";
      return false;
    }
    if (!cursor.empty() && !valid_encoded_cursor(cursor)) {
      error = "Bybit instruments-info nextPageCursor is not URL encoded";
      return false;
    }
    batch.page_cursor.assign(cursor);
    batch.http = bootstrap_instruments_info_request(cursor);
    error.clear();
    return true;
  }

  bool metadata_page_list_empty(std::string_view json, bool &empty,
                                std::string &error) override {
    empty = false;
#ifndef MDS_HAS_SIMDJSON
    (void)json;
    error = "simdjson support was not compiled";
    return false;
#else
    try {
      auto document = parse(json);
      if (!response_code_ok(document)) {
        error = "Bybit instruments-info returned an error";
        return false;
      }
      auto result = document["result"].get_object();
      if (result.error()) {
        error = "Bybit instruments-info pagination result is missing";
        return false;
      }
      auto returned_category = result.value()["category"].get_string();
      if (returned_category.error() ||
          std::string_view(returned_category.value()) != category(product_)) {
        error = "Bybit instruments-info pagination category mismatch";
        return false;
      }
      auto list = result.value()["list"].get_array();
      if (list.error()) {
        error = "Bybit instruments-info list is missing or invalid";
        return false;
      }
      empty = true;
      for (auto entry : list.value()) {
        (void)entry;
        empty = false;
        break;
      }
      error.clear();
      return true;
    } catch (const simdjson::simdjson_error &exception) {
      error = "malformed Bybit instruments-info pagination response: ";
      error.append(exception.what());
      return false;
    }
#endif
  }

  bool metadata_page_repeated_venue_symbol(std::string_view json,
                                           std::string &symbol,
                                           std::string &error) override {
    symbol.clear();
#ifndef MDS_HAS_SIMDJSON
    (void)json;
    error = "simdjson support was not compiled";
    return false;
#else
    try {
      auto document = parse(json);
      if (!response_code_ok(document)) {
        error = "Bybit instruments-info returned an error";
        return false;
      }
      auto result = document["result"].get_object();
      if (result.error()) {
        error = "Bybit instruments-info pagination result is missing";
        return false;
      }
      auto list = result.value()["list"].get_array();
      if (list.error()) {
        error = "Bybit instruments-info list is missing or invalid";
        return false;
      }
      std::set<std::string> seen;
      for (auto raw_instrument : list.value()) {
        auto instrument = raw_instrument.get_object().value();
        auto name = instrument["symbol"].get_string();
        if (name.error()) {
          continue;
        }
        if (product_ == utils::md::ProductType::Perpetual) {
          auto contract_type = instrument["contractType"].get_string();
          auto status = instrument["status"].get_string();
          if (contract_type.error() || status.error() ||
              std::string_view(contract_type.value()) != "LinearPerpetual" ||
              std::string_view(status.value()) != "Trading") {
            continue;
          }
        }
        const std::string venue_symbol(name.value());
        if (!seen.insert(venue_symbol).second) {
          symbol = venue_symbol;
          error.clear();
          return true;
        }
      }
      error.clear();
      return true;
    } catch (const simdjson::simdjson_error &exception) {
      error = "malformed Bybit instruments-info pagination response: ";
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
      auto document = parse(json);
      if (!response_code_ok(document)) {
        error = "Bybit instruments-info returned an error";
        if (requests.size() == 1) {
          error.append(": ");
          error.append(requests.front().venue_symbol);
        }
        return false;
      }
      auto result = document["result"].get_object().value();
      const auto returned_category = required_string(result, "category");
      if (returned_category != category(product_)) {
        error = "Bybit instruments-info category does not match product";
        return false;
      }

      std::vector<InstrumentMetadata> parsed;
      parsed.reserve(requests.size());
      auto instruments = result["list"].get_array().value();
      for (const auto &request : requests) {
        bool found = false;
        for (auto raw_instrument : instruments) {
          auto instrument = raw_instrument.get_object().value();
          if (required_string(instrument, "symbol") != request.venue_symbol) {
            continue;
          }
          if (product_ == utils::md::ProductType::Perpetual) {
            auto contract_type = instrument["contractType"].get_string();
            auto status = instrument["status"].get_string();
            if (contract_type.error() || status.error() ||
                std::string_view(contract_type.value()) !=
                    "LinearPerpetual" ||
                std::string_view(status.value()) != "Trading") {
              continue;
            }
          }
          InstrumentMetadata value;
          value.canonical_symbol = request.canonical_symbol;
          value.venue_symbol = request.venue_symbol;
          value.base_asset = required_string(instrument, "baseCoin");
          value.quote_asset = required_string(instrument, "quoteCoin");
          auto settle = instrument["settleCoin"].get_string();
          value.settle_asset =
              settle.error() ? value.quote_asset : std::string(settle.value());

          auto price_filter_result = instrument["priceFilter"].get_object();
          auto lot_filter_result = instrument["lotSizeFilter"].get_object();
          if (price_filter_result.error() || lot_filter_result.error()) {
            error = "Bybit ";
            error.append(category(product_));
            error += " metadata is missing priceFilter or lotSizeFilter: ";
            error.append(request.venue_symbol);
            return false;
          }
          auto price_filter = price_filter_result.value();
          auto lot_filter = lot_filter_result.value();
          auto tick_size_result = price_filter["tickSize"].get_string();
          const std::string_view quantity_field =
              product_ == utils::md::ProductType::Spot ? "basePrecision"
                                                       : "qtyStep";
          auto quantity_step_result =
              lot_filter[quantity_field].get_string();
          if (tick_size_result.error() || quantity_step_result.error()) {
            error = "Bybit ";
            error.append(category(product_));
            error += " metadata is missing tickSize or ";
            error.append(quantity_field);
            error += ": ";
            error.append(request.venue_symbol);
            return false;
          }
          const auto tick_size =
              std::string_view(tick_size_result.value());
          const auto quantity_step =
              std::string_view(quantity_step_result.value());
          if (!mds::exchange::decimal_scale(tick_size, value.price_scale) ||
              !mds::exchange::decimal_to_fixed(
                  tick_size, value.price_scale, value.tick_size) ||
              !mds::exchange::decimal_scale(quantity_step,
                                            value.quantity_scale) ||
              !mds::exchange::decimal_to_fixed(
                  quantity_step, value.quantity_scale, value.lot_size) ||
              value.tick_size <= 0 || value.lot_size <= 0) {
            error = "invalid Bybit tickSize or qtyStep";
            return false;
          }
          value.contract_multiplier = 1;
          auto *state =
              find_or_add(request.venue_symbol, request.canonical_symbol, error);
          if (state == nullptr) {
            return false;
          }
          state->price_scale = value.price_scale;
          state->quantity_scale = value.quantity_scale;
          state->has_scales = true;
          parsed.push_back(std::move(value));
          found = true;
          break;
        }
        if (!found) {
          continue;
        }
      }
      metadata = std::move(parsed);
      error.clear();
      return true;
    } catch (const simdjson::simdjson_error &exception) {
      error = exception.what();
      return false;
    }
#endif
  }

  MetadataResponse parse_metadata_response(
      std::string_view json, std::span<const StreamRequest> requests,
      std::vector<InstrumentMetadata> &metadata) override {
#ifndef MDS_HAS_SIMDJSON
    (void)json;
    (void)requests;
    (void)metadata;
    return {MetadataResponseKind::ConnectionFailure,
            "simdjson support was not compiled"};
#else
    try {
      auto document = parse(json);
      auto code = document["retCode"].get_int64();
      if (code.error()) {
        return {MetadataResponseKind::ConnectionFailure,
                "Bybit instruments-info retCode is missing or invalid"};
      }
      if (code.value() != 0) {
        auto message = document["retMsg"].get_string();
        const std::string_view text =
            message.error() ? std::string_view{} :
                              std::string_view(message.value());
        if (requests.size() == 1 &&
            explicit_symbol_unavailable_message(text)) {
          std::string reason{"Bybit exact metadata lookup rejected symbol"};
          if (!text.empty()) {
            reason.append(": ");
            reason.append(text);
          }
          return {MetadataResponseKind::SymbolUnavailable,
                  std::move(reason)};
        }
        std::string reason{"Bybit instruments-info returned retCode="};
        reason.append(std::to_string(code.value()));
        if (!text.empty()) {
          reason.append(" retMsg=");
          reason.append(text);
        }
        return {MetadataResponseKind::ConnectionFailure,
                std::move(reason)};
      }
    } catch (const simdjson::simdjson_error &exception) {
      std::string reason{"malformed Bybit instruments-info response: "};
      reason.append(exception.what());
      return {MetadataResponseKind::ConnectionFailure,
              std::move(reason)};
    }

    auto outcome = VenueAdapter::parse_metadata_response(
        json, requests, metadata);
    if (outcome.kind == MetadataResponseKind::SymbolUnavailable &&
        product_ == utils::md::ProductType::Perpetual &&
        requests.size() == 1) {
      try {
        auto document = parse(json);
        auto instruments =
            document["result"]["list"].get_array().value();
        for (auto raw_instrument : instruments) {
          auto instrument = raw_instrument.get_object().value();
          auto symbol = instrument["symbol"].get_string();
          if (!symbol.error() &&
              std::string_view(symbol.value()) ==
                  requests.front().venue_symbol) {
            outcome.reason =
                "Bybit instrument is not a Trading LinearPerpetual: ";
            outcome.reason.append(requests.front().venue_symbol);
            break;
          }
        }
      } catch (const simdjson::simdjson_error &exception) {
        std::string reason{"malformed Bybit instruments-info response: "};
        reason.append(exception.what());
        return {MetadataResponseKind::ConnectionFailure,
                std::move(reason)};
      }
    }
    return outcome;
#endif
  }

  bool upsert_metadata(
      std::string_view json, std::span<const StreamRequest> requests,
      std::vector<InstrumentMetadata> &metadata,
      std::string &error) override {
    const auto existing_states = states_;
    const auto existing_state_count = state_count_;
    std::vector<InstrumentMetadata> parsed;
    if (!parse_metadata(json, requests, parsed, error)) {
      states_ = existing_states;
      state_count_ = existing_state_count;
      return false;
    }
    metadata = std::move(parsed);
    return true;
  }

 protected:
  void classify_parse_failure(std::string_view,
                              const NormalizedEvent &event,
                              ParseFailure &failure) const noexcept override {
    (void)classify_scale_mismatch(event, failure);
  }

 private:
  [[nodiscard]] HttpRequestSpec bootstrap_instruments_info_request(
      std::string_view cursor) const {
    auto request = metadata_request();
    request.target.append("&status=Trading&limit=1000");
    if (!cursor.empty()) {
      request.target.append("&cursor=");
      request.target.append(cursor);
    }
    return request;
  }

  bool remember(std::string_view venue_symbol, std::string_view canonical_symbol,
                std::string &error) const {
    return find_or_add(venue_symbol, canonical_symbol, error) != nullptr;
  }

  SymbolState *find_or_add(std::string_view venue_symbol,
                           std::string_view canonical_symbol,
                           std::string &error) const {
    for (std::size_t index = 0; index < state_count_; ++index) {
      if (states_[index].venue() == venue_symbol) {
        if (!canonical_symbol.empty() &&
            states_[index].canonical() != canonical_symbol &&
            !copy_fixed(canonical_symbol, states_[index].canonical_symbol,
                        states_[index].canonical_symbol_size)) {
          error = "Bybit canonical symbol exceeds fixed capacity";
          return nullptr;
        }
        return &states_[index];
      }
    }
    if (state_count_ == states_.size()) {
      error = "Bybit symbol state capacity exhausted";
      return nullptr;
    }
    auto &state = states_[state_count_];
    if (!copy_fixed(venue_symbol, state.venue_symbol,
                    state.venue_symbol_size) ||
        !copy_fixed(canonical_symbol.empty() ? venue_symbol : canonical_symbol,
                    state.canonical_symbol, state.canonical_symbol_size)) {
      error = "Bybit symbol exceeds fixed capacity";
      return nullptr;
    }
    ++state_count_;
    return &state;
  }

  bool set_symbol(const SymbolState &state, NormalizedEvent &event,
                  std::string &error) const {
    if (!mds::exchange::copy_symbol(state.canonical(), event)) {
      error = "Bybit event symbol exceeds fixed capacity";
      return false;
    }
    return true;
  }

#ifdef MDS_HAS_SIMDJSON
  simdjson::dom::element parse(std::string_view json) {
    if (json.size() > kMaximumJsonBytes) {
      throw simdjson::simdjson_error(simdjson::CAPACITY);
    }
    std::memcpy(json_buffer_.data(), json.data(), json.size());
    std::memset(json_buffer_.data() + json.size(), 0,
                simdjson::SIMDJSON_PADDING);
    return parser_.parse(json_buffer_.data(), json.size(), false).value();
  }

  bool ensure_scales(SymbolState &state, simdjson::dom::array bids,
                     simdjson::dom::array asks, std::string &error) {
    if (state.has_scales) {
      return true;
    }
    std::uint8_t price_scale{};
    std::uint8_t quantity_scale{};
    if (!parse_level_scales(bids, price_scale, quantity_scale) ||
        !parse_level_scales(asks, price_scale, quantity_scale)) {
      error = "unable to infer Bybit order book scales";
      return false;
    }
    state.price_scale = price_scale;
    state.quantity_scale = quantity_scale;
    state.has_scales = true;
    return true;
  }

  static std::uint64_t optional_timestamp(simdjson::dom::element document) {
    auto timestamp = document["ts"].get_uint64();
    return timestamp.error() ? 0 : timestamp.value();
  }
#endif

  utils::md::ProductType product_;
  std::size_t max_levels_per_side_;
  mutable std::array<SymbolState, kMaximumTrackedSymbols> states_{};
  mutable std::size_t state_count_{};
#ifdef MDS_HAS_SIMDJSON
  simdjson::dom::parser parser_;
  std::vector<char> json_buffer_;
#endif
};

}  // namespace

std::unique_ptr<VenueAdapter>
make_bybit_adapter(utils::md::ProductType product,
                   std::size_t max_levels_per_side) {
  if (!valid_product(product)) {
    return nullptr;
  }
  return std::make_unique<BybitAdapter>(product, max_levels_per_side);
}

}  // namespace mds::exchange::bybit
