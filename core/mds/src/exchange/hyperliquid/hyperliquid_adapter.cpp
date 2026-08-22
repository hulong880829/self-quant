#include "mds/exchange/hyperliquid/hyperliquid_adapter.h"

#include <algorithm>
#include <cstring>
#include <limits>
#include <string>
#include <unordered_map>
#include <utility>
#include <vector>

#ifdef MDS_HAS_SIMDJSON
#include <simdjson.h>
#endif

namespace mds::exchange {
namespace {

constexpr std::size_t kMaximumJsonBytes = 4U * 1024U * 1024U;

struct HyperliquidScales {
  std::uint8_t price{};
  std::uint8_t quantity{};
};

bool valid_coin(std::string_view coin) noexcept {
  if (coin.empty() || coin.size() > 32) {
    return false;
  }
  for (const char character : coin) {
    const bool valid =
        (character >= 'A' && character <= 'Z') ||
        (character >= 'a' && character <= 'z') ||
        (character >= '0' && character <= '9') || character == '@' ||
        character == '/' || character == '-' || character == '_';
    if (!valid) {
      return false;
    }
  }
  return true;
}

std::string subscription(std::string_view type, std::string_view coin) {
  return "{\"method\":\"subscribe\",\"subscription\":{\"type\":\"" +
         std::string(type) + "\",\"coin\":\"" + std::string(coin) + "\"}}";
}

bool decimal_to_fixed_local(std::string_view value, std::uint8_t scale,
                            std::int64_t &result) noexcept {
  if (value.empty() || scale > 18 || value.front() == '+') {
    return false;
  }
  bool negative = false;
  std::size_t cursor = 0;
  if (value.front() == '-') {
    negative = true;
    cursor = 1;
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

std::string normalized_pair(std::string_view value) {
  std::string result;
  result.reserve(value.size());
  for (const char character : value) {
    if (character != '/' && character != '-' && character != '_') {
      result.push_back(character);
    }
  }
  return result;
}

#ifdef MDS_HAS_SIMDJSON
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

bool parse_level(simdjson::dom::element raw,
                 const HyperliquidScales &scales,
                 utils::md::Level &level) {
  auto object = raw.get_object().value();
  const auto price = object["px"].get_string().value();
  const auto quantity = object["sz"].get_string().value();
  return decimal_to_fixed_local(price, scales.price, level.price) &&
         decimal_to_fixed_local(quantity, scales.quantity, level.quantity);
}

struct SpotToken {
  std::string name;
  std::uint8_t quantity_scale{};
};
#endif

class HyperliquidAdapter final : public VenueAdapter {
 public:
  HyperliquidAdapter(utils::md::ProductType product,
                     std::size_t maximum_levels)
#ifdef MDS_HAS_SIMDJSON
      : json_buffer_(kMaximumJsonBytes + simdjson::SIMDJSON_PADDING),
        product_(product),
        maximum_levels_(std::min<std::size_t>(maximum_levels, 20))
#else
      : product_(product),
        maximum_levels_(std::min<std::size_t>(maximum_levels, 20))
#endif
  {
  }

  [[nodiscard]] utils::md::Venue venue() const noexcept override {
    return utils::md::Venue::Hyperliquid;
  }
  [[nodiscard]] utils::md::ProductType product() const noexcept override {
    return product_;
  }
  [[nodiscard]] HeartbeatSpec heartbeat() const override {
    return {HeartbeatKind::JsonPing, "{\"method\":\"ping\"}", 30'000};
  }

  bool build_subscription_batches(
      std::span<const StreamRequest> requests,
      std::vector<std::string> &batches, std::string &error) const override {
    batches.clear();
    for (const auto &request : requests) {
      if (!valid_coin(request.venue_symbol)) {
        error = "invalid Hyperliquid coin";
        return false;
      }
      if (request.ticker) {
        if (!request.ticker_channel.empty() &&
            request.ticker_channel != "bbo") {
          error = "unsupported Hyperliquid ticker channel";
          return false;
        }
        batches.push_back(subscription("bbo", request.venue_symbol));
      }
      if (request.orderbook) {
        if (!request.orderbook_channel.empty() &&
            request.orderbook_channel != "l2Book") {
          error = "unsupported Hyperliquid order-book channel";
          return false;
        }
        batches.push_back(subscription("l2Book", request.venue_symbol));
      }
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
      auto document = parse(json).get_object().value();
      const auto channel = optional_string(document, "channel");
      if (channel == "subscriptionResponse") {
        event.type = AdapterEventType::SubscribeAck;
        error.clear();
        return true;
      }
      if (channel == "error") {
        event.type = AdapterEventType::SubscribeError;
        error.clear();
        return true;
      }
      if (channel == "pong") {
        event.type = AdapterEventType::Pong;
        error.clear();
        return true;
      }
      if (channel != "bbo" && channel != "l2Book") {
        return true;
      }
      auto data = document["data"].get_object().value();
      const auto coin = optional_string(data, "coin");
      const auto *scales = find_scales(coin);
      if (scales == nullptr || !copy_symbol(coin, event)) {
        error = "Hyperliquid event has unknown or invalid coin";
        return false;
      }
      event.exchange_time_ms = optional_uint(data, "time");
      if (sequence_ == std::numeric_limits<std::uint64_t>::max()) {
        error = "Hyperliquid internal image sequence exhausted";
        return false;
      }
      event.first_sequence = ++sequence_;
      event.final_sequence = sequence_;

      if (channel == "bbo") {
        event.type = AdapterEventType::Bbo;
        std::size_t side = 0;
        auto bbo_result = data["bbo"].get_array();
        auto bbo = bbo_result.value();
        for (auto raw : bbo) {
          if (side >= 2) {
            error = "Hyperliquid BBO has too many sides";
            return false;
          }
          if (!raw.is_null()) {
            auto &output = side == 0 ? event.bid : event.ask;
            if (!parse_level(raw, *scales, output)) {
              error = "invalid Hyperliquid BBO level";
              return false;
            }
          }
          ++side;
        }
        if (side != 2) {
          error = "Hyperliquid BBO does not contain two sides";
          return false;
        }
        error.clear();
        return true;
      }

      event.type = AdapterEventType::BookSnapshot;
      event.sequence_reset = true;
      std::size_t side = 0;
      auto sides_result = data["levels"].get_array();
      auto sides = sides_result.value();
      for (auto raw_side : sides) {
        if (side >= 2) {
          error = "Hyperliquid l2Book has too many sides";
          return false;
        }
        auto &output = side == 0 ? event.bids : event.asks;
        auto levels_result = raw_side.get_array();
        auto levels = levels_result.value();
        for (auto raw_level : levels) {
          if (output.size() >= maximum_levels_) {
            error = "Hyperliquid l2Book exceeds configured capacity";
            return false;
          }
          utils::md::Level level;
          if (!parse_level(raw_level, *scales, level)) {
            error = "invalid Hyperliquid l2Book level";
            return false;
          }
          output.push_back(level);
        }
        ++side;
      }
      if (side != 2) {
        error = "Hyperliquid l2Book does not contain two sides";
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
    return {HttpRequestSpec::Method::Post, "/info", "application/json",
            product_ == utils::md::ProductType::Spot
                ? "{\"type\":\"spotMeta\"}"
                : "{\"type\":\"meta\"}"};
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
      auto document = parse(json).get_object().value();
      const bool parsed =
          product_ == utils::md::ProductType::Spot
              ? parse_spot_metadata(document, requests, metadata, error)
              : parse_perpetual_metadata(document, requests, metadata, error);
      if (!parsed) {
        metadata.clear();
        scales_.clear();
        return false;
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

 private:
  const HyperliquidScales *
  find_scales(std::string_view symbol) const noexcept {
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

  bool parse_perpetual_metadata(
      simdjson::dom::object document,
      std::span<const StreamRequest> requests,
      std::vector<InstrumentMetadata> &metadata, std::string &error) {
    auto universe = document["universe"].get_array().value();
    for (const auto &request : requests) {
      bool found = false;
      for (auto raw : universe) {
        auto entry = raw.get_object().value();
        const auto name = optional_string(entry, "name");
        const auto canonical = normalized_pair(request.canonical_symbol);
        const bool matches =
            name == request.venue_symbol ||
            canonical == normalized_pair(name) ||
            canonical == normalized_pair(std::string(name) + "_USDC") ||
            canonical == normalized_pair(std::string(name) + "_USD");
        if (!matches) {
          continue;
        }
        const auto decimals = entry["szDecimals"].get_uint64().value();
        if (decimals > 18) {
          error = "Hyperliquid szDecimals exceeds fixed-point capacity";
          return false;
        }
        HyperliquidScales scales;
        scales.quantity = static_cast<std::uint8_t>(decimals);
        scales.price =
            static_cast<std::uint8_t>(decimals >= 6 ? 0 : 6 - decimals);
        InstrumentMetadata parsed;
        parsed.canonical_symbol = request.canonical_symbol;
        parsed.venue_symbol = name;
        parsed.base_asset = name;
        parsed.quote_asset = "USDC";
        parsed.settle_asset = "USDC";
        parsed.price_scale = scales.price;
        parsed.quantity_scale = scales.quantity;
        parsed.tick_size = 1;
        parsed.lot_size = 1;
        parsed.contract_multiplier = 1;
        scales_.emplace(parsed.venue_symbol, scales);
        metadata.push_back(std::move(parsed));
        found = true;
        break;
      }
      if (!found) {
        error = "requested Hyperliquid perpetual was not found in meta";
        return false;
      }
    }
    return true;
  }

  bool parse_spot_metadata(
      simdjson::dom::object document,
      std::span<const StreamRequest> requests,
      std::vector<InstrumentMetadata> &metadata, std::string &error) {
    std::unordered_map<std::uint64_t, SpotToken> tokens;
    auto tokens_result = document["tokens"].get_array();
    auto token_entries = tokens_result.value();
    for (auto raw : token_entries) {
      auto token = raw.get_object().value();
      const auto index = token["index"].get_uint64().value();
      const auto decimals = token["szDecimals"].get_uint64().value();
      if (decimals > 18) {
        error = "Hyperliquid spot szDecimals exceeds fixed-point capacity";
        return false;
      }
      tokens.emplace(index, SpotToken{std::string(optional_string(token, "name")),
                                      static_cast<std::uint8_t>(decimals)});
    }

    auto universe = document["universe"].get_array().value();
    for (const auto &request : requests) {
      bool found = false;
      for (auto raw : universe) {
        auto entry = raw.get_object().value();
        const auto name = optional_string(entry, "name");
        const auto index = entry["index"].get_uint64().value();
        const std::string indexed_name = "@" + std::to_string(index);
        std::uint64_t base_index = 0;
        std::uint64_t quote_index = 0;
        std::size_t token_position = 0;
        auto pair_tokens_result = entry["tokens"].get_array();
        auto pair_tokens = pair_tokens_result.value();
        for (auto token : pair_tokens) {
          if (token_position == 0) {
            base_index = token.get_uint64().value();
          } else if (token_position == 1) {
            quote_index = token.get_uint64().value();
          } else {
            error = "Hyperliquid spot pair has more than two tokens";
            return false;
          }
          ++token_position;
        }
        if (token_position != 2 || !tokens.contains(base_index) ||
            !tokens.contains(quote_index)) {
          error = "Hyperliquid spot pair references unknown tokens";
          return false;
        }
        const auto &base = tokens.at(base_index);
        const auto &quote = tokens.at(quote_index);
        const std::string token_pair = base.name + "_" + quote.name;
        const auto canonical = normalized_pair(request.canonical_symbol);
        const bool matches =
            request.venue_symbol == name ||
            request.venue_symbol == indexed_name ||
            canonical == normalized_pair(token_pair) ||
            canonical == normalized_pair(name);
        if (!matches) {
          continue;
        }
        HyperliquidScales scales;
        scales.quantity = base.quantity_scale;
        scales.price = static_cast<std::uint8_t>(
            base.quantity_scale >= 8 ? 0 : 8 - base.quantity_scale);
        InstrumentMetadata parsed;
        parsed.canonical_symbol = request.canonical_symbol;
        parsed.venue_symbol =
            request.venue_symbol.starts_with('@')
                ? std::string(request.venue_symbol)
                : std::string(name);
        parsed.base_asset = base.name;
        parsed.quote_asset = quote.name;
        parsed.settle_asset = quote.name;
        parsed.price_scale = scales.price;
        parsed.quantity_scale = scales.quantity;
        parsed.tick_size = 1;
        parsed.lot_size = 1;
        parsed.contract_multiplier = 1;
        scales_.emplace(parsed.venue_symbol, scales);
        metadata.push_back(std::move(parsed));
        found = true;
        break;
      }
      if (!found) {
        error = "requested Hyperliquid spot pair was not found in spotMeta";
        return false;
      }
    }
    return true;
  }

  simdjson::dom::parser parser_;
  std::vector<char> json_buffer_;
#endif
  utils::md::ProductType product_;
  std::size_t maximum_levels_;
  std::uint64_t sequence_{};
  std::unordered_map<std::string, HyperliquidScales> scales_;
};

}  // namespace

std::unique_ptr<VenueAdapter>
make_hyperliquid_adapter(utils::md::ProductType product,
                         std::size_t max_levels_per_side) {
  if ((product != utils::md::ProductType::Spot &&
       product != utils::md::ProductType::Perpetual) ||
      max_levels_per_side == 0) {
    return {};
  }
  return std::make_unique<HyperliquidAdapter>(product,
                                               max_levels_per_side);
}

}  // namespace mds::exchange
