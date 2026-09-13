#include "mds/exchange/lighter/lighter_adapter.h"

#include "mds/exchange/symbol_policy.h"

#include <algorithm>
#include <charconv>
#include <cctype>
#include <cmath>
#include <cstring>
#include <limits>
#include <string>
#include <unordered_map>
#include <unordered_set>
#include <utility>
#include <vector>

#ifdef MDS_HAS_SIMDJSON
#include <simdjson.h>
#endif

namespace mds::exchange {
namespace {

constexpr std::size_t kMaximumJsonBytes = 32U << 20U;

std::string canonical_text(std::string_view value) {
  std::string result;
  result.reserve(value.size());
  for (char character : value) {
    const auto raw = static_cast<unsigned char>(character);
    if (std::isalnum(raw) == 0) {
      continue;
    }
    if (character >= 'a' && character <= 'z') {
      character = static_cast<char>(character - ('a' - 'A'));
    }
    result.push_back(character);
  }
  return result;
}

bool split_spot_symbol(std::string_view symbol, std::string &base,
                       std::string &quote) {
  const auto slash = symbol.find('/');
  if (slash == std::string_view::npos || slash == 0 ||
      slash + 1 >= symbol.size() ||
      symbol.find('/', slash + 1) != std::string_view::npos) {
    return false;
  }
  base = canonical_text(symbol.substr(0, slash));
  quote = canonical_text(symbol.substr(slash + 1));
  return !base.empty() && !quote.empty();
}

std::string message(std::string_view operation, std::string_view channel,
                    std::uint64_t market_id) {
  std::string result{"{\"type\":\""};
  result.append(operation);
  result.append("\",\"channel\":\"");
  result.append(channel);
  result.push_back('/');
  result.append(std::to_string(market_id));
  result.append("\"}");
  return result;
}

bool parse_channel_id(std::string_view channel, std::string_view prefix,
                      std::uint64_t &market_id) noexcept {
  if (!channel.starts_with(prefix) || channel.size() <= prefix.size()) {
    return false;
  }
  const auto number = channel.substr(prefix.size());
  const auto parsed =
      std::from_chars(number.data(), number.data() + number.size(), market_id);
  return parsed.ec == std::errc{} &&
         parsed.ptr == number.data() + number.size();
}

std::string normalized_decimal(std::string value) {
  const auto dot = value.find('.');
  if (dot == std::string::npos) {
    return value;
  }
  while (!value.empty() && value.back() == '0') {
    value.pop_back();
  }
  if (!value.empty() && value.back() == '.') {
    value.pop_back();
  }
  return value.empty() ? "0" : value;
}

#ifdef MDS_HAS_SIMDJSON
std::string_view optional_string(simdjson::dom::object object,
                                 std::string_view key) {
  auto value = object[key].get_string();
  return value.error() ? std::string_view{} : value.value();
}

bool required_uint(simdjson::dom::object object, std::string_view key,
                   std::uint64_t &output) {
  auto value = object[key].get_uint64();
  if (!value.error()) {
    output = value.value();
    return true;
  }
  auto signed_value = object[key].get_int64();
  if (!signed_value.error() && signed_value.value() >= 0) {
    output = static_cast<std::uint64_t>(signed_value.value());
    return true;
  }
  return false;
}

bool decimal_element_to_u64(simdjson::dom::element element,
                            std::uint64_t &output) {
  long double value{};
  auto floating = element.get_double();
  if (!floating.error()) {
    value = static_cast<long double>(floating.value());
  } else {
    auto text = element.get_string();
    if (text.error()) {
      return false;
    }
    try {
      std::size_t consumed{};
      const std::string owned(text.value());
      value = std::stold(owned, &consumed);
      if (consumed != owned.size()) {
        return false;
      }
    } catch (...) {
      return false;
    }
  }
  if (!std::isfinite(value) || value < 0 ||
      value >
          static_cast<long double>(std::numeric_limits<std::uint64_t>::max())) {
    return false;
  }
  output = static_cast<std::uint64_t>(value);
  return true;
}

bool parse_level(simdjson::dom::element raw, std::uint8_t price_scale,
                 std::uint8_t quantity_scale, utils::md::Level &level,
                 std::string &error) {
  auto object_result = raw.get_object();
  if (object_result.error()) {
    error = "Lighter level is not an object";
    return false;
  }
  auto object = object_result.value();
  auto price_result = object["price"].get_string();
  auto size_result = object["size"].get_string();
  if (price_result.error() || size_result.error()) {
    error = "Lighter level is missing price or size";
    return false;
  }
  const auto price = price_result.value();
  const auto size = size_result.value();
  if (!decimal_to_fixed(price, price_scale, level.price)) {
    error = decimal_scale_mismatch(price, price_scale)
                ? "invalid Lighter level reason=scale field=price"
                : "invalid Lighter level price";
    return false;
  }
  if (!decimal_to_fixed(size, quantity_scale, level.quantity)) {
    error = decimal_scale_mismatch(size, quantity_scale)
                ? "invalid Lighter level reason=scale field=quantity"
                : "invalid Lighter level quantity";
    return false;
  }
  return level.price > 0 && level.quantity >= 0;
}
#endif

struct LighterState {
  std::string canonical;
  std::string venue_symbol;
  std::uint64_t market_id{};
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
};

class LighterAdapter final : public VenueAdapter {
 public:
  LighterAdapter(utils::md::ProductType product,
                 std::size_t maximum_levels)
#ifdef MDS_HAS_SIMDJSON
      : json_buffer_(kMaximumJsonBytes + simdjson::SIMDJSON_PADDING),
        product_(product),
        maximum_levels_(maximum_levels)
#else
      : product_(product), maximum_levels_(maximum_levels)
#endif
  {}

  [[nodiscard]] utils::md::Venue venue() const noexcept override {
    return utils::md::Venue::Lighter;
  }

  [[nodiscard]] utils::md::ProductType product() const noexcept override {
    return product_;
  }

  [[nodiscard]] HeartbeatSpec heartbeat() const override {
    return {HeartbeatKind::JsonPing, "{\"type\":\"ping\"}", 60'000};
  }

  [[nodiscard]] std::size_t expected_subscription_acks(
      std::string_view) const noexcept override {
    // Lighter's subscribed/* frame is the first data image, not a separate ACK.
    return 0;
  }

  [[nodiscard]] std::size_t subscription_send_window()
      const noexcept override {
    return lighter::kMaximumInflightMessages;
  }

  bool build_subscription_batches(
      std::span<const StreamRequest> requests,
      std::vector<std::string> &batches, std::string &error) const override {
    return build_batches("subscribe", requests, batches, error);
  }

  bool build_unsubscription_batches(
      std::span<const StreamRequest> requests,
      std::vector<std::string> &batches, std::string &error) const override {
    return build_batches("unsubscribe", requests, batches, error);
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
      auto document = parse(json).get_object().value();
      const auto type = optional_string(document, "type");
      if (type == "connected" || type == "unsubscribed") {
        error.clear();
        return true;
      }
      if (type == "pong") {
        event.type = AdapterEventType::Pong;
        error.clear();
        return true;
      }
      if (type == "error") {
        event.type = AdapterEventType::SubscribeError;
        error = "Lighter subscription rejected";
        const auto message_text = optional_string(document, "message");
        if (!message_text.empty()) {
          error.append(": ");
          error.append(message_text);
        }
        return true;
      }

      const auto channel = optional_string(document, "channel");
      const bool ticker =
          type == "subscribed/ticker" || type == "update/ticker";
      const bool snapshot = type == "subscribed/order_book";
      const bool delta = type == "update/order_book";
      if (!ticker && !snapshot && !delta) {
        error.clear();
        return true;
      }

      std::uint64_t market_id{};
      const auto prefix =
          ticker ? std::string_view{"ticker:"}
                 : std::string_view{"order_book:"};
      if (!parse_channel_id(channel, prefix, market_id)) {
        error = "Lighter event has an invalid channel";
        return false;
      }
      const auto *state = find_market(market_id);
      if (state == nullptr || !copy_symbol(state->venue_symbol, event)) {
        error = "Lighter event references an unknown market_id";
        return false;
      }

      std::uint64_t timestamp{};
      (void)required_uint(document, "timestamp", timestamp);
      event.exchange_time_ms = timestamp;

      if (ticker) {
        auto ticker_result = document["ticker"].get_object();
        if (ticker_result.error()) {
          error = "Lighter ticker payload is missing";
          return false;
        }
        auto ticker_object = ticker_result.value();
        const auto payload_symbol = optional_string(ticker_object, "s");
        if (payload_symbol != state->venue_symbol) {
          error = "Lighter ticker channel and payload symbol disagree";
          return false;
        }
        auto bid_result = ticker_object["b"].get_object();
        auto ask_result = ticker_object["a"].get_object();
        if (bid_result.error() || ask_result.error()) {
          // An empty side is not publishable BBO data.
          error.clear();
          return true;
        }
        const auto parse_top = [&](simdjson::dom::object side,
                                   utils::md::Level &level) {
          auto price = side["price"].get_string();
          auto size = side["size"].get_string();
          if (price.error() || size.error()) {
            return false;
          }
          if (!decimal_to_fixed(price.value(), state->price_scale,
                                level.price)) {
            error = decimal_scale_mismatch(price.value(), state->price_scale)
                        ? "invalid Lighter ticker reason=scale field=price"
                        : "invalid Lighter ticker price";
            return false;
          }
          if (!decimal_to_fixed(size.value(), state->quantity_scale,
                                level.quantity)) {
            error =
                decimal_scale_mismatch(size.value(), state->quantity_scale)
                    ? "invalid Lighter ticker reason=scale field=quantity"
                    : "invalid Lighter ticker quantity";
            return false;
          }
          return true;
        };
        if (!parse_top(bid_result.value(), event.bid) ||
            !parse_top(ask_result.value(), event.ask)) {
          return false;
        }
        if (event.bid.price <= 0 || event.bid.quantity <= 0 ||
            event.ask.price <= 0 || event.ask.quantity <= 0 ||
            event.bid.price > event.ask.price) {
          error.clear();
          event.reset();
          return true;
        }
        std::uint64_t nonce{};
        if (!required_uint(document, "nonce", nonce)) {
          error = "Lighter ticker is missing nonce";
          return false;
        }
        event.type = AdapterEventType::Bbo;
        event.first_sequence = nonce;
        event.final_sequence = nonce;
        error.clear();
        return true;
      }

      auto book_result = document["order_book"].get_object();
      if (book_result.error()) {
        error = "Lighter order_book payload is missing";
        return false;
      }
      auto book = book_result.value();
      std::uint64_t nonce{};
      if (!required_uint(book, "nonce", nonce) || nonce == 0) {
        error = "Lighter order_book is missing nonce";
        return false;
      }
      event.first_sequence = nonce;
      event.final_sequence = nonce;
      if (delta) {
        if (!required_uint(book, "begin_nonce", event.previous_sequence) ||
            event.previous_sequence == 0) {
          error = "Lighter order_book delta is missing begin_nonce";
          return false;
        }
        event.strict_previous_sequence = true;
      }

      const auto parse_side =
          [&](std::string_view name, std::vector<utils::md::Level> &output,
              InputSide input_side) {
            auto values_result = book[name].get_array();
            if (values_result.error()) {
              error = "Lighter order_book is missing a side";
              return false;
            }
            for (auto raw : values_result.value()) {
              if (output.size() >= maximum_levels_) {
                event.type = AdapterEventType::BookGap;
                event.input_side = input_side;
                event.input_capacity = maximum_levels_;
                error = "Lighter order_book exceeds configured capacity";
                return true;
              }
              utils::md::Level level;
              if (!parse_level(raw, state->price_scale,
                               state->quantity_scale, level, error)) {
                return false;
              }
              output.push_back(level);
            }
            return true;
          };
      if (!parse_side("bids", event.bids, InputSide::Bid) ||
          event.type == AdapterEventType::BookGap ||
          !parse_side("asks", event.asks, InputSide::Ask) ||
          event.type == AdapterEventType::BookGap) {
        return event.type == AdapterEventType::BookGap;
      }
      event.type =
          snapshot ? AdapterEventType::BookSnapshot
                   : AdapterEventType::BookDelta;
      event.sequence_reset = snapshot;
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
                ? "/api/v1/orderBookDetails?filter=spot"
                : "/api/v1/orderBookDetails?filter=perp",
            {}, {}};
  }

  bool parse_metadata(std::string_view json,
                      std::span<const StreamRequest> requests,
                      std::vector<InstrumentMetadata> &metadata,
                      std::string &error) override {
    std::vector<LighterState> states;
    if (!parse_metadata_impl(json, requests, metadata, states, error)) {
      return false;
    }
    states_ = std::move(states);
    return true;
  }

  bool upsert_metadata(
      std::string_view json, std::span<const StreamRequest> requests,
      std::vector<InstrumentMetadata> &metadata,
      std::string &error) override {
    std::vector<LighterState> refreshed;
    if (!parse_metadata_impl(json, requests, metadata, refreshed, error)) {
      return false;
    }
    auto merged = states_;
    for (auto &entry : refreshed) {
      std::erase_if(merged, [&entry](const LighterState &current) {
        return current.market_id == entry.market_id ||
               current.venue_symbol == entry.venue_symbol;
      });
      merged.push_back(std::move(entry));
    }
    states_ = std::move(merged);
    return true;
  }

  bool parse_discovery_metadata(
      std::string_view json, std::span<const StreamRequest> requests,
      std::vector<InstrumentMetadata> &metadata,
      std::string &error) override {
    std::vector<LighterState> ignored;
    return parse_metadata_impl(json, requests, metadata, ignored, error);
  }

  [[nodiscard]] HttpRequestSpec
  discovery_turnover_request() const override {
    return metadata_request();
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
      auto document = parse(json).get_object().value();
      const auto field =
          product_ == utils::md::ProductType::Spot
              ? std::string_view{"spot_order_book_details"}
              : std::string_view{"order_book_details"};
      auto entries = document[field].get_array().value();
      std::unordered_set<std::string> matched;
      for (auto raw : entries) {
        auto entry = raw.get_object().value();
        const auto symbol = optional_string(entry, "symbol");
        const auto found = std::find_if(
            metadata.begin(), metadata.end(),
            [symbol](const InstrumentMetadata &instrument) {
              return instrument.venue_symbol == symbol;
            });
        if (found == metadata.end()) {
          continue;
        }
        if (!matched.emplace(symbol).second) {
          error = "duplicate Lighter turnover symbol";
          return false;
        }
        if (!decimal_element_to_u64(entry["daily_quote_token_volume"],
                                    found->turnover_24h)) {
          error = "invalid Lighter daily quote turnover";
          return false;
        }
      }
      for (const auto &instrument : metadata) {
        if (!matched.contains(instrument.venue_symbol)) {
          error = "Lighter turnover did not match discovered symbol";
          return false;
        }
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
    (void)classify_scale_mismatch(event, failure);
  }

 private:
  bool build_batches(std::string_view operation,
                     std::span<const StreamRequest> requests,
                     std::vector<std::string> &batches,
                     std::string &error) const {
    batches.clear();
    if (requests.size() >
        lighter::kMaximumSubscriptionsPerConnection) {
      error = "Lighter request exceeds per-connection subscription limit";
      return false;
    }
    // Ticker channels are deliberately emitted before books so BBO recovers
    // first when both streams share one strategy process.
    for (const bool ticker_pass : {true, false}) {
      for (const auto &request : requests) {
        if (!valid_utf8_symbol(request.venue_symbol)) {
          error = "invalid Lighter venue symbol";
          return false;
        }
        const bool enabled =
            ticker_pass ? request.ticker : request.orderbook;
        if (!enabled) {
          continue;
        }
        const auto *state = find_request(request);
        if (state == nullptr) {
          error = "Lighter subscription has no market_id mapping";
          return false;
        }
        if (ticker_pass) {
          if (!request.ticker_channel.empty() &&
              request.ticker_channel != "ticker") {
            error = "unsupported Lighter ticker channel";
            return false;
          }
          batches.push_back(message(operation, "ticker", state->market_id));
        } else {
          if (!request.orderbook_channel.empty() &&
              request.orderbook_channel != "order_book") {
            error = "unsupported Lighter order-book channel";
            return false;
          }
          batches.push_back(
              message(operation, "order_book", state->market_id));
        }
      }
    }
    if (batches.empty()) {
      error = "at least one Lighter stream must be requested";
      return false;
    }
    if (batches.size() >
        lighter::kMaximumSubscriptionsPerConnection) {
      error = "Lighter request exceeds per-connection subscription limit";
      batches.clear();
      return false;
    }
    error.clear();
    return true;
  }

  bool parse_metadata_impl(
      std::string_view json, std::span<const StreamRequest> requests,
      std::vector<InstrumentMetadata> &metadata,
      std::vector<LighterState> &states, std::string &error) {
    metadata.clear();
    states.clear();
#ifndef MDS_HAS_SIMDJSON
    (void)json;
    (void)requests;
    error = "simdjson support was not compiled";
    return false;
#else
    try {
      auto document = parse(json).get_object().value();
      std::uint64_t code{};
      if (!required_uint(document, "code", code) || code != 200) {
        error = "Lighter metadata response has non-success code";
        return false;
      }
      const auto field =
          product_ == utils::md::ProductType::Spot
              ? std::string_view{"spot_order_book_details"}
              : std::string_view{"order_book_details"};
      auto entries_result = document[field].get_array();
      if (entries_result.error()) {
        error = "Lighter metadata response is missing product array";
        return false;
      }

      std::unordered_set<std::string> seen_symbols;
      std::unordered_set<std::string> seen_canonicals;
      std::unordered_set<std::uint64_t> seen_market_ids;
      for (auto raw : entries_result.value()) {
        auto entry = raw.get_object().value();
        const auto symbol = optional_string(entry, "symbol");
        const auto status = optional_string(entry, "status");
        const auto market_type = optional_string(entry, "market_type");
        if (symbol.empty() || status != "active" ||
            market_type !=
                (product_ == utils::md::ProductType::Spot ? "spot"
                                                          : "perp")) {
          continue;
        }
        std::uint64_t market_id{};
        std::uint64_t price_scale{};
        std::uint64_t quantity_scale{};
        if (!required_uint(entry, "market_id", market_id) ||
            !required_uint(entry, "supported_price_decimals",
                           price_scale) ||
            !required_uint(entry, "supported_size_decimals",
                           quantity_scale) ||
            price_scale > 18 || quantity_scale > 18) {
          error = "invalid Lighter market metadata";
          return false;
        }
        if (!seen_symbols.emplace(symbol).second ||
            !seen_market_ids.emplace(market_id).second) {
          error = "duplicate Lighter symbol or market_id";
          return false;
        }

        std::string base;
        std::string quote;
        if (product_ == utils::md::ProductType::Spot) {
          if (!split_spot_symbol(symbol, base, quote)) {
            error = "invalid Lighter spot symbol";
            return false;
          }
        } else {
          base = canonical_text(symbol);
          quote = "USDC";
        }
        const auto canonical = base + quote;
        if (!seen_canonicals.emplace(canonical).second) {
          error = "duplicate Lighter canonical symbol";
          return false;
        }
        const auto requested = std::find_if(
            requests.begin(), requests.end(),
            [&](const StreamRequest &request) {
              const bool venue_matches =
                  request.venue_symbol.empty() ||
                  request.venue_symbol == symbol;
              const bool canonical_matches =
                  request.canonical_symbol.empty() ||
                  canonical_text(request.canonical_symbol) == canonical;
              return venue_matches && canonical_matches;
            });
        if (requested == requests.end()) {
          continue;
        }

        InstrumentMetadata instrument;
        instrument.canonical_symbol = canonical;
        instrument.venue_symbol = std::string(symbol);
        instrument.base_asset = base;
        instrument.quote_asset = quote;
        instrument.settle_asset = "USDC";
        instrument.price_scale = static_cast<std::uint8_t>(price_scale);
        instrument.quantity_scale =
            static_cast<std::uint8_t>(quantity_scale);
        instrument.tick_size = 1;
        instrument.lot_size = 1;
        instrument.contract_multiplier = 1;
        if (product_ == utils::md::ProductType::Perpetual) {
          auto multiplier_result = entry["multiplier"].get_string();
          if (multiplier_result.error()) {
            error = "Lighter perpetual metadata is missing multiplier";
            return false;
          }
          const auto multiplier =
              normalized_decimal(std::string(multiplier_result.value()));
          std::uint8_t multiplier_scale{};
          if (!decimal_scale(multiplier, multiplier_scale) ||
              !decimal_to_fixed(multiplier, multiplier_scale,
                                instrument.contract_multiplier) ||
              instrument.contract_multiplier <= 0) {
            error = "invalid Lighter contract multiplier";
            return false;
          }
          instrument.contract_multiplier_scale = multiplier_scale;
        }
        if (!decimal_element_to_u64(entry["daily_quote_token_volume"],
                                    instrument.turnover_24h)) {
          error = "invalid Lighter daily quote turnover";
          return false;
        }
        metadata.push_back(instrument);
        states.push_back(
            {canonical, std::string(symbol), market_id,
             static_cast<std::uint8_t>(price_scale),
             static_cast<std::uint8_t>(quantity_scale)});
      }
      error.clear();
      return true;
    } catch (const simdjson::simdjson_error &exception) {
      error = exception.what();
      metadata.clear();
      states.clear();
      return false;
    }
#endif
  }

  const LighterState *find_market(std::uint64_t market_id) const noexcept {
    const auto found = std::find_if(
        states_.begin(), states_.end(),
        [market_id](const LighterState &state) {
          return state.market_id == market_id;
        });
    return found == states_.end() ? nullptr : &*found;
  }

  const LighterState *find_request(
      const StreamRequest &request) const {
    const auto canonical = canonical_text(request.canonical_symbol);
    const auto found = std::find_if(
        states_.begin(), states_.end(),
        [&](const LighterState &state) {
          const bool venue_matches =
              request.venue_symbol.empty() ||
              state.venue_symbol == request.venue_symbol;
          const bool canonical_matches =
              canonical.empty() || state.canonical == canonical;
          return venue_matches && canonical_matches;
        });
    return found == states_.end() ? nullptr : &*found;
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

  simdjson::dom::parser parser_;
  std::vector<char> json_buffer_;
#endif
  utils::md::ProductType product_;
  std::size_t maximum_levels_;
  std::vector<LighterState> states_;
};

}  // namespace

std::unique_ptr<VenueAdapter>
make_lighter_adapter(utils::md::ProductType product,
                     std::size_t max_levels_per_side) {
  if ((product != utils::md::ProductType::Spot &&
       product != utils::md::ProductType::Perpetual) ||
      max_levels_per_side == 0 || max_levels_per_side > 5000) {
    return nullptr;
  }
  return std::make_unique<LighterAdapter>(product, max_levels_per_side);
}

}  // namespace mds::exchange
