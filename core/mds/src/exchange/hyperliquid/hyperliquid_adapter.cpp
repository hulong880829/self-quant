#include "mds/exchange/hyperliquid/hyperliquid_adapter.h"
#include "mds/exchange/symbol_policy.h"

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

std::string subscription(std::string_view type, std::string_view coin) {
  std::string result{"{\"method\":\"subscribe\",\"subscription\":{\"type\":\""};
  append_json_escaped(result, type);
  result += "\",\"coin\":\"";
  append_json_escaped(result, coin);
  result += "\"}}";
  return result;
}

bool decimal_to_fixed_local(std::string_view value, std::uint8_t scale,
                            std::int64_t &result,
                            bool *normalized_tail = nullptr) noexcept {
  if (normalized_tail != nullptr) {
    *normalized_tail = false;
  }
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
  std::size_t tail_start = std::string_view::npos;
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
      if (tail_start == std::string_view::npos) {
        tail_start = cursor;
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
  if (tail_start != std::string_view::npos) {
    const auto tail = value.substr(tail_start);
    const bool all_zero = std::all_of(
        tail.begin(), tail.end(), [](char character) {
          return character == '0';
        });
    if (!all_zero) {
      if (tail.size() < 8U) {
        return false;
      }
      const auto prefix = tail.substr(0, 8);
      const bool near_zero = std::all_of(
          prefix.begin(), prefix.end(), [](char character) {
            return character == '0';
          });
      const bool near_one = std::all_of(
          prefix.begin(), prefix.end(), [](char character) {
            return character == '9';
          });
      if (!near_zero && !near_one) {
        return false;
      }
      if (near_one) {
        if (parsed == maximum) {
          return false;
        }
        ++parsed;
      }
      if (normalized_tail != nullptr) {
        *normalized_tail = true;
      }
    }
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

std::string global_symbol(std::string_view base, std::string_view quote) {
  std::string result;
  result.reserve(base.size() + quote.size());
  const auto append = [&result](std::string_view value) {
    for (char character : value) {
      if (character == '-' || character == '_' || character == '/' ||
          character == ':') {
        continue;
      }
      if (character >= 'a' && character <= 'z') {
        character = static_cast<char>(character - ('a' - 'A'));
      }
      result.push_back(character);
    }
  };
  append(base);
  append(quote);
  return result;
}

bool is_xyz_coin(std::string_view coin) noexcept {
  return coin.starts_with("xyz:");
}

std::string_view unqualified_coin(std::string_view coin) noexcept {
  const auto separator = coin.find(':');
  return separator == std::string_view::npos ? coin
                                             : coin.substr(separator + 1);
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

bool optional_bool(simdjson::dom::object object, std::string_view key) {
  auto value = object[key].get_bool();
  return value.error() ? false : value.value();
}

bool parse_level(simdjson::dom::element raw,
                 const HyperliquidScales &scales,
                 utils::md::Level &level,
                 std::string *error = nullptr,
                 std::uint8_t *normalized_tail_fields = nullptr) {
  auto object = raw.get_object().value();
  const auto price = object["px"].get_string().value();
  const auto quantity = object["sz"].get_string().value();
  bool normalized_price = false;
  bool normalized_quantity = false;
  if (!decimal_to_fixed_local(
          price, scales.price, level.price, &normalized_price)) {
    if (error != nullptr &&
        decimal_scale_mismatch(price, scales.price)) {
      *error = "invalid Hyperliquid level reason=scale field=price";
    }
    return false;
  }
  if (!decimal_to_fixed_local(
          quantity, scales.quantity, level.quantity, &normalized_quantity)) {
    if (error != nullptr &&
        decimal_scale_mismatch(quantity, scales.quantity)) {
      *error = "invalid Hyperliquid level reason=scale field=quantity";
    }
    return false;
  }
  if (normalized_tail_fields != nullptr) {
    const auto total =
        static_cast<unsigned>(*normalized_tail_fields) +
        static_cast<unsigned>(normalized_price) +
        static_cast<unsigned>(normalized_quantity);
    *normalized_tail_fields = static_cast<std::uint8_t>(total);
  }
  return true;
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
  [[nodiscard]] std::size_t subscription_send_window()
      const noexcept override {
    return 16;
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
    if (!requests.empty() && accepted == 0) {
      error = "no valid Hyperliquid coins";
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
      auto document = parse(json).get_object().value();
      const auto channel = optional_string(document, "channel");
      if (channel == "subscriptionResponse") {
        event.type = AdapterEventType::SubscribeAck;
        auto data = document["data"].get_object();
        if (!data.error()) {
          auto subscribed = data.value()["subscription"].get_object();
          if (!subscribed.error()) {
            const auto coin =
                optional_string(subscribed.value(), "coin");
            if (!coin.empty()) {
              (void)copy_symbol(coin, event);
            }
          }
        }
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

      if (channel == "bbo") {
        std::size_t side = 0;
        bool complete = true;
        auto bbo_result = data["bbo"].get_array();
        auto bbo = bbo_result.value();
        for (auto raw : bbo) {
          if (side >= 2) {
            error = "Hyperliquid BBO has too many sides";
            return false;
          }
          if (raw.is_null()) {
            complete = false;
          } else {
            auto &output = side == 0 ? event.bid : event.ask;
            if (!parse_level(raw, *scales, output, &error,
                             &event.normalized_tail_fields)) {
              if (error.empty()) {
                error = "invalid Hyperliquid BBO level";
              }
              return false;
            }
          }
          ++side;
        }
        if (side != 2) {
          error = "Hyperliquid BBO does not contain two sides";
          return false;
        }
        if (!complete || event.bid.price <= 0 ||
            event.bid.quantity <= 0 || event.ask.price <= 0 ||
            event.ask.quantity <= 0) {
          error.clear();
          return true;
        }
        if (sequence_ == std::numeric_limits<std::uint64_t>::max()) {
          error = "Hyperliquid internal image sequence exhausted";
          return false;
        }
        event.type = AdapterEventType::Bbo;
        event.first_sequence = ++sequence_;
        event.final_sequence = sequence_;
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
          if (!parse_level(raw_level, *scales, level, &error,
                           &event.normalized_tail_fields)) {
            if (error.empty()) {
              error = "invalid Hyperliquid l2Book level";
            }
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
      if (sequence_ == std::numeric_limits<std::uint64_t>::max()) {
        error = "Hyperliquid internal image sequence exhausted";
        return false;
      }
      event.first_sequence = ++sequence_;
      event.final_sequence = sequence_;
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

  [[nodiscard]] HttpRequestSpec
  discovery_metadata_request(std::string_view cursor) const override {
    if (product_ == utils::md::ProductType::Spot) {
      return metadata_request();
    }
    return {HttpRequestSpec::Method::Post, "/info", "application/json",
            cursor == "xyz"
                ? "{\"type\":\"metaAndAssetCtxs\",\"dex\":\"xyz\"}"
                : "{\"type\":\"metaAndAssetCtxs\"}"};
  }

  bool discovery_metadata_next_cursor(
      std::string_view request_cursor, std::string_view,
      std::string &cursor, std::string &error) override {
    cursor = product_ == utils::md::ProductType::Perpetual &&
                     request_cursor.empty()
                 ? "xyz"
                 : "";
    error.clear();
    return true;
  }

  [[nodiscard]] bool discovery_page_is_optional(
      std::string_view cursor) const noexcept override {
    return product_ == utils::md::ProductType::Perpetual &&
           cursor == "xyz";
  }

  [[nodiscard]] bool
  discovery_metadata_includes_turnover() const noexcept override {
    return product_ == utils::md::ProductType::Perpetual;
  }

  bool build_metadata_request_batches(
      std::span<const StreamRequest> requests,
      std::vector<MetadataRequestBatch> &batches,
      std::string &error) const override {
    if (requests.empty()) {
      error = "metadata request requires at least one symbol";
      return false;
    }
    std::vector<MetadataRequestBatch> built;
    std::size_t offset = 0;
    while (offset < requests.size()) {
      const bool xyz = product_ == utils::md::ProductType::Perpetual &&
                       is_xyz_coin(requests[offset].venue_symbol);
      std::size_t end = offset + 1;
      while (end < requests.size() &&
             (product_ == utils::md::ProductType::Perpetual &&
              is_xyz_coin(requests[end].venue_symbol)) == xyz) {
        ++end;
      }
      HttpRequestSpec request = metadata_request();
      if (xyz) {
        request.body = "{\"type\":\"meta\",\"dex\":\"xyz\"}";
      }
      built.push_back({std::move(request), offset, end - offset, false, {}});
      offset = end;
    }
    batches = std::move(built);
    error.clear();
    return true;
  }

  [[nodiscard]] HttpRequestSpec
  discovery_turnover_request() const override {
    return {HttpRequestSpec::Method::Post, "/info", "application/json",
            product_ == utils::md::ProductType::Spot
                ? "{\"type\":\"spotMetaAndAssetCtxs\"}"
                : "{\"type\":\"metaAndAssetCtxs\"}"};
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
      auto sections = parse(json).get_array().value();
      std::unordered_map<std::size_t, std::string>
          venue_symbols_by_context;
      std::vector<std::uint64_t> turnovers(metadata.size());
      std::vector<bool> matched(metadata.size(), false);
      std::size_t section = 0;
      std::size_t context_count = 0;
      for (auto raw_section : sections) {
        if (section == 0) {
          auto document = raw_section.get_object().value();
          auto universe = document["universe"].get_array().value();
          std::size_t universe_position = 0;
          for (auto raw_entry : universe) {
            auto entry = raw_entry.get_object().value();
            std::size_t context_index = universe_position;
            if (product_ == utils::md::ProductType::Spot) {
              context_index = static_cast<std::size_t>(
                  entry["index"].get_uint64().value());
            }
            const auto [ignored, inserted] =
                venue_symbols_by_context.emplace(
                    context_index,
                    std::string(optional_string(entry, "name")));
            (void)ignored;
            if (!inserted) {
              error =
                  "duplicate Hyperliquid asset context index";
              return false;
            }
            ++universe_position;
          }
        } else if (section == 1) {
          auto contexts = raw_section.get_array().value();
          for (auto raw_context : contexts) {
            auto context = raw_context.get_object().value();
            std::string_view venue_symbol;
            if (product_ == utils::md::ProductType::Spot) {
              venue_symbol = optional_string(context, "coin");
            }
            const auto indexed_symbol =
                venue_symbols_by_context.find(context_count);
            if (venue_symbol.empty() &&
                indexed_symbol != venue_symbols_by_context.end()) {
              venue_symbol = indexed_symbol->second;
            }
            ++context_count;
            if (venue_symbol.empty()) {
              continue;
            }
            const auto found = std::find_if(
                metadata.begin(), metadata.end(),
                [&venue_symbol](const InstrumentMetadata &instrument) {
                  return instrument.venue_symbol == venue_symbol ||
                         unqualified_coin(instrument.venue_symbol) ==
                             venue_symbol;
                });
            if (found == metadata.end()) {
              continue;
            }
            const auto index =
                static_cast<std::size_t>(found - metadata.begin());
            auto turnover = context["dayNtlVlm"].get_string();
            if (turnover.error() ||
                !decimal_to_turnover(turnover.value(),
                                     turnovers[index])) {
              error = "invalid Hyperliquid 24h turnover for symbol: ";
              error.append(venue_symbol);
              return false;
            }
            matched[index] = true;
          }
        } else {
          error =
              "Hyperliquid metadata and contexts response has extra sections";
          return false;
        }
        ++section;
      }
      if (section != 2 ||
          (product_ == utils::md::ProductType::Perpetual &&
           context_count != venue_symbols_by_context.size())) {
        error =
            "Hyperliquid metadata and contexts response is misaligned";
        return false;
      }
      for (std::size_t index = 0; index < metadata.size(); ++index) {
        if (!matched[index]) {
          error =
              "Hyperliquid turnover did not match discovered symbol: ";
          error.append(metadata[index].venue_symbol);
          return false;
        }
        metadata[index].turnover_24h = turnovers[index];
      }
      error.clear();
      return true;
    } catch (const simdjson::simdjson_error &exception) {
      error = "malformed Hyperliquid metadata and contexts response: ";
      error.append(exception.what());
      return false;
    }
#endif
  }

  bool parse_discovery_metadata(
      std::string_view json, std::span<const StreamRequest> requests,
      std::vector<InstrumentMetadata> &metadata,
      std::string &error) override {
#ifndef MDS_HAS_SIMDJSON
    (void)json;
    (void)requests;
    (void)metadata;
    error = "simdjson support was not compiled";
    return false;
#else
    const auto first = json.find_first_not_of(" \t\r\n");
    if (first == std::string_view::npos || json[first] != '[') {
      return upsert_metadata(json, requests, metadata, error);
    }

    auto existing = std::move(scales_);
    scales_.clear();
    metadata.clear();
    bool parsed = false;
    try {
      auto sections = parse(json).get_array().value();
      auto section = sections.begin();
      if (section == sections.end()) {
        error =
            "Hyperliquid metadata and contexts response has no metadata";
      } else {
        auto document = (*section).get_object().value();
        parsed =
            product_ == utils::md::ProductType::Spot
                ? parse_spot_metadata(document, requests, metadata, error)
                : parse_perpetual_metadata(document, requests, metadata,
                                           error);
      }
    } catch (const simdjson::simdjson_error &exception) {
      error = "malformed Hyperliquid metadata and contexts response: ";
      error.append(exception.what());
    }
    auto refreshed = std::move(scales_);
    scales_ = std::move(existing);
    if (!parsed ||
        !enrich_discovery_turnover(json, metadata, error)) {
      metadata.clear();
      return false;
    }
    for (auto &[symbol, scales] : refreshed) {
      scales_.insert_or_assign(std::move(symbol), scales);
    }
    error.clear();
    return true;
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

 protected:
  void classify_parse_failure(std::string_view,
                              const NormalizedEvent &event,
                              ParseFailure &failure) const noexcept override {
    (void)classify_scale_mismatch(event, failure);
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
        if (optional_bool(entry, "isDelisted")) {
          continue;
        }
        const auto canonical = normalized_pair(request.canonical_symbol);
        const auto qualified_name =
            is_xyz_coin(request.venue_symbol) &&
                    name.find(':') == std::string_view::npos
                ? request.venue_symbol
                : name;
        const bool matches =
            name == request.venue_symbol ||
            name == unqualified_coin(request.venue_symbol) ||
            canonical == normalized_pair(name) ||
            canonical == normalized_pair(std::string(name) + "_USDC") ||
            canonical == normalized_pair(std::string(name) + "_USD") ||
            canonical == normalized_pair(
                             global_symbol(qualified_name, "USDC"));
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
        parsed.canonical_symbol =
            global_symbol(qualified_name, "USDC");
        parsed.venue_symbol = qualified_name;
        parsed.base_asset = unqualified_coin(qualified_name);
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
        continue;
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
        parsed.canonical_symbol =
            global_symbol(base.name, quote.name);
        parsed.venue_symbol = name;
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
        continue;
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
