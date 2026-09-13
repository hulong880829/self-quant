#include "mds/exchange/polymarket/polymarket_adapter.h"

#include <algorithm>
#include <array>
#include <charconv>
#include <chrono>
#include <cctype>
#include <cstring>
#include <utility>

#ifdef MDS_HAS_SIMDJSON
#include <simdjson.h>
#endif

namespace mds::exchange::polymarket {
namespace {

constexpr std::int64_t kWindowSeconds = 300;
constexpr std::size_t kMaximumJsonBytes = 2U << 20U;
constexpr std::size_t kMaximumPendingEvents = 64;
constexpr std::size_t kMaximumBookLevels = 1000;
constexpr std::size_t kMaximumTokens = 16;
constexpr std::uint8_t kPriceScale = 6;
constexpr std::uint8_t kQuantityScale = 6;

bool iequals(std::string_view left, std::string_view right) noexcept {
  if (left.size() != right.size()) {
    return false;
  }
  for (std::size_t index = 0; index < left.size(); ++index) {
    if (std::tolower(static_cast<unsigned char>(left[index])) !=
        std::tolower(static_cast<unsigned char>(right[index]))) {
      return false;
    }
  }
  return true;
}

bool decimal_token(std::string_view value) noexcept {
  return !value.empty() && value.size() <= 78 &&
         std::all_of(value.begin(), value.end(), [](char character) {
           return character >= '0' && character <= '9';
         });
}

std::optional<Outcome> logical_outcome(std::string_view symbol) noexcept {
  if (iequals(symbol, "BTC5MUP") || iequals(symbol, "BTC-5M-UP") ||
      iequals(symbol, "BTC_5M_UP")) {
    return Outcome::Up;
  }
  if (iequals(symbol, "BTC5MDOWN") || iequals(symbol, "BTC-5M-DOWN") ||
      iequals(symbol, "BTC_5M_DOWN")) {
    return Outcome::Down;
  }
  return std::nullopt;
}

template <std::size_t Capacity>
bool append_integer(FixedText<Capacity> &target, std::int64_t value) noexcept {
  auto *begin = target.bytes.data() + target.size;
  auto *end = target.bytes.data() + Capacity;
  const auto result = std::to_chars(begin, end, value);
  if (result.ec != std::errc{}) {
    return false;
  }
  target.size = static_cast<std::size_t>(result.ptr - target.bytes.data());
  target.bytes[target.size] = '\0';
  return true;
}

#ifdef MDS_HAS_SIMDJSON

std::string_view optional_string(simdjson::dom::object object,
                                 std::string_view first,
                                 std::string_view second = {}) {
  auto result = object[first].get_string();
  if (!result.error()) {
    return result.value();
  }
  if (!second.empty()) {
    result = object[second].get_string();
    if (!result.error()) {
      return result.value();
    }
  }
  return {};
}

bool decimal_fixed(simdjson::dom::element value, std::uint8_t scale,
                   std::int64_t &output) {
  auto text = value.get_string();
  if (!text.error()) {
    return mds::exchange::decimal_to_fixed(text.value(), scale, output);
  }
  std::array<char, 64> buffer{};
  auto integer = value.get_int64();
  std::to_chars_result converted;
  if (!integer.error()) {
    converted = std::to_chars(buffer.data(),
                              buffer.data() + buffer.size(),
                              integer.value());
  } else {
    auto number = value.get_double();
    if (number.error()) {
      return false;
    }
    converted = std::to_chars(
        buffer.data(), buffer.data() + buffer.size(), number.value(),
        std::chars_format::fixed, scale);
  }
  return converted.ec == std::errc{} &&
         mds::exchange::decimal_to_fixed(
             {buffer.data(), static_cast<std::size_t>(
                                 converted.ptr - buffer.data())},
             scale, output);
}

std::uint64_t unsigned_value(simdjson::dom::element value) noexcept {
  auto number = value.get_uint64();
  if (!number.error()) {
    return number.value();
  }
  auto text = value.get_string();
  if (text.error()) {
    return 0;
  }
  std::uint64_t parsed{};
  const auto view = std::string_view(text.value());
  const auto result =
      std::from_chars(view.data(), view.data() + view.size(), parsed);
  return result.ec == std::errc{} &&
                 result.ptr == view.data() + view.size()
             ? parsed
             : 0;
}

bool optional_bool(simdjson::dom::object object, std::string_view name,
                   bool fallback = false) noexcept {
  auto result = object[name].get_bool();
  return result.error() ? fallback : result.value();
}

struct StringList {
  std::array<std::string_view, 8> values{};
  std::size_t size{};
};

bool parse_encoded_string_list(std::string_view encoded,
                               StringList &output) noexcept {
  output = {};
  std::size_t cursor = 0;
  while (cursor < encoded.size() &&
         std::isspace(static_cast<unsigned char>(encoded[cursor]))) {
    ++cursor;
  }
  if (cursor == encoded.size() || encoded[cursor++] != '[') {
    return false;
  }
  while (cursor < encoded.size()) {
    while (cursor < encoded.size() &&
           (std::isspace(static_cast<unsigned char>(encoded[cursor])) ||
            encoded[cursor] == ',')) {
      ++cursor;
    }
    if (cursor < encoded.size() && encoded[cursor] == ']') {
      return output.size != 0;
    }
    if (cursor == encoded.size() || encoded[cursor++] != '"' ||
        output.size == output.values.size()) {
      return false;
    }
    const auto begin = cursor;
    while (cursor < encoded.size() && encoded[cursor] != '"') {
      if (encoded[cursor] == '\\') {
        return false;
      }
      ++cursor;
    }
    if (cursor == encoded.size()) {
      return false;
    }
    output.values[output.size++] = encoded.substr(begin, cursor - begin);
    ++cursor;
  }
  return false;
}

bool parse_string_list(simdjson::dom::element element,
                       StringList &output) {
  output = {};
  auto array = element.get_array();
  if (!array.error()) {
    for (auto raw : array.value()) {
      auto value = raw.get_string();
      if (value.error() || output.size == output.values.size()) {
        return false;
      }
      output.values[output.size++] = value.value();
    }
    return output.size != 0;
  }
  auto encoded = element.get_string();
  return !encoded.error() &&
         parse_encoded_string_list(encoded.value(), output);
}

bool parse_window_from_slug(std::string_view slug,
                            MarketWindow &window) noexcept {
  constexpr std::string_view prefix{"btc-updown-5m-"};
  if (!slug.starts_with(prefix)) {
    return false;
  }
  std::int64_t start{};
  const auto suffix = slug.substr(prefix.size());
  const auto parsed =
      std::from_chars(suffix.data(), suffix.data() + suffix.size(), start);
  if (parsed.ec != std::errc{} ||
      parsed.ptr != suffix.data() + suffix.size() || start < 0 ||
      start % kWindowSeconds != 0) {
    return false;
  }
  window = {start, start + kWindowSeconds};
  return true;
}

bool parse_gamma_object(simdjson::dom::object object,
                        std::string_view expected_slug, GammaMarket &market,
                        std::string &error) {
  const auto slug = optional_string(object, "slug");
  if (slug != expected_slug) {
    return false;
  }
  const auto condition = optional_string(object, "conditionId", "condition_id");
  StringList outcomes;
  StringList tokens;
  auto raw_outcomes = object["outcomes"];
  auto raw_tokens = object["clobTokenIds"];
  if (raw_tokens.error()) {
    raw_tokens = object["clob_token_ids"];
  }
  if (condition.empty() || raw_outcomes.error() || raw_tokens.error() ||
      !parse_string_list(raw_outcomes.value(), outcomes) ||
      !parse_string_list(raw_tokens.value(), tokens) ||
      outcomes.size != tokens.size) {
    error = "Gamma market is missing condition/outcome/token fields";
    return false;
  }

  std::string_view up;
  std::string_view down;
  for (std::size_t index = 0; index < outcomes.size; ++index) {
    if (iequals(outcomes.values[index], "up") ||
        iequals(outcomes.values[index], "yes")) {
      up = tokens.values[index];
    } else if (iequals(outcomes.values[index], "down") ||
               iequals(outcomes.values[index], "no")) {
      down = tokens.values[index];
    }
  }
  MarketWindow window;
  GammaMarket parsed;
  if (!parse_window_from_slug(slug, window) || !decimal_token(up) ||
      !decimal_token(down) || !parsed.slug.assign(slug) ||
      !parsed.condition_id.assign(condition) ||
      !parsed.up_token_id.assign(up) || !parsed.down_token_id.assign(down)) {
    error = "Gamma BTC 5m market contains invalid or oversized identifiers";
    return false;
  }
  parsed.window = window;
  auto minimum = object["orderMinSize"];
  if (minimum.error()) {
    minimum = object["order_min_size"];
  }
  if (!minimum.error() &&
      (!decimal_fixed(minimum.value(), kQuantityScale,
                      parsed.minimum_order_size) ||
       parsed.minimum_order_size <= 0)) {
    error = "Gamma market contains an invalid minimum order size";
    return false;
  }
  auto signature = object["signatureType"];
  if (!signature.error()) {
    auto signature_element = signature.value();
    const auto value = unsigned_value(signature_element);
    if (value != 0 && value != 3) {
      error = "Gamma market contains an unsupported signature type";
      return false;
    }
    parsed.signature_type = static_cast<std::uint8_t>(value);
  }
  parsed.negative_risk =
      optional_bool(object, "negRisk",
                    optional_bool(object, "negativeRisk"));
  parsed.closed = optional_bool(object, "closed");
  parsed.active = optional_bool(object, "active") && !parsed.closed;
  market = parsed;
  error.clear();
  return true;
}

bool search_gamma(simdjson::dom::element element,
                  std::string_view expected_slug, GammaMarket &market,
                  std::string &error, unsigned depth = 0) {
  if (depth > 3) {
    return false;
  }
  auto array = element.get_array();
  if (!array.error()) {
    for (auto child : array.value()) {
      if (search_gamma(child, expected_slug, market, error, depth + 1)) {
        return true;
      }
    }
    return false;
  }
  auto object_result = element.get_object();
  if (object_result.error()) {
    return false;
  }
  auto object = object_result.value();
  if (parse_gamma_object(object, expected_slug, market, error)) {
    return true;
  }
  auto markets = object["markets"];
  if (!markets.error() &&
      search_gamma(markets.value(), expected_slug, market, error, depth + 1)) {
    return true;
  }
  auto data = object["data"];
  return !data.error() &&
         search_gamma(data.value(), expected_slug, market, error, depth + 1);
}

#endif

}  // namespace

MarketWindow btc_five_minute_window(std::int64_t unix_seconds,
                                    std::int32_t offset) noexcept {
  auto start = unix_seconds >= 0
                   ? (unix_seconds / kWindowSeconds) * kWindowSeconds
                   : ((unix_seconds - (kWindowSeconds - 1)) / kWindowSeconds) *
                         kWindowSeconds;
  start += static_cast<std::int64_t>(offset) * kWindowSeconds;
  return {start, start + kWindowSeconds};
}

bool btc_five_minute_slug(MarketWindow window,
                          FixedText<64> &slug) noexcept {
  slug = {};
  constexpr std::string_view prefix{"btc-updown-5m-"};
  if (window.start_unix < 0 ||
      window.end_unix - window.start_unix != kWindowSeconds ||
      window.start_unix % kWindowSeconds != 0 ||
      !slug.assign(prefix)) {
    return false;
  }
  return append_integer(slug, window.start_unix);
}

bool parse_gamma_market_response(std::string_view json,
                                 std::string_view expected_slug,
                                 GammaMarket &market, std::string &error) {
#ifndef MDS_HAS_SIMDJSON
  (void)json;
  (void)expected_slug;
  (void)market;
  error = "simdjson support was not compiled";
  return false;
#else
  if (json.empty() || json.size() > kMaximumJsonBytes) {
    error = "Gamma response is empty or exceeds capacity";
    return false;
  }
  try {
    simdjson::dom::parser parser;
    simdjson::padded_string padded(json);
    auto document = parser.parse(padded);
    error.clear();
    if (search_gamma(document.value(), expected_slug, market, error)) {
      return true;
    }
    if (error.empty()) {
      error = "Gamma response does not contain the requested exact slug";
    }
    return false;
  } catch (const simdjson::simdjson_error &exception) {
    error = exception.what();
    return false;
  }
#endif
}

void RollingMarketResolver::set_entry(Entry &value, ResolveSlot slot,
                                      MarketWindow window) noexcept {
  value = {};
  value.request.slot = slot;
  value.request.window = window;
  if (!btc_five_minute_slug(window, value.request.slug)) {
    return;
  }
  if (!value.request.target.assign("/events?slug=")) {
    value = {};
    return;
  }
  const auto slug = value.request.slug.view();
  if (value.request.target.size + slug.size() >
      value.request.target.bytes.size() - 1) {
    value = {};
    return;
  }
  std::copy(slug.begin(), slug.end(),
            value.request.target.bytes.begin() + value.request.target.size);
  value.request.target.size += slug.size();
  value.request.target.bytes[value.request.target.size] = '\0';
  value.status = ResolveStatus::RequestReady;
}

RollingMarketResolver::Entry &RollingMarketResolver::entry(
    ResolveSlot slot) noexcept {
  return slot == ResolveSlot::Current ? current_ : next_;
}

const RollingMarketResolver::Entry &RollingMarketResolver::entry(
    ResolveSlot slot) const noexcept {
  return slot == ResolveSlot::Current ? current_ : next_;
}

void RollingMarketResolver::reset(std::int64_t unix_seconds) noexcept {
  set_entry(current_, ResolveSlot::Current,
            btc_five_minute_window(unix_seconds));
  set_entry(next_, ResolveSlot::Next,
            btc_five_minute_window(unix_seconds, 1));
}

void RollingMarketResolver::advance(std::int64_t unix_seconds) noexcept {
  const auto desired = btc_five_minute_window(unix_seconds);
  if (current_.request.window.start_unix == desired.start_unix) {
    return;
  }
  if (next_.request.window.start_unix == desired.start_unix) {
    current_ = next_;
    current_.request.slot = ResolveSlot::Current;
  } else {
    set_entry(current_, ResolveSlot::Current, desired);
  }
  set_entry(next_, ResolveSlot::Next,
            {desired.start_unix + kWindowSeconds,
             desired.end_unix + kWindowSeconds});
}

std::optional<GammaRequest> RollingMarketResolver::next_request() const
    noexcept {
  if (current_.status == ResolveStatus::RequestReady) {
    return current_.request;
  }
  if (next_.status == ResolveStatus::RequestReady) {
    return next_.request;
  }
  return std::nullopt;
}

bool RollingMarketResolver::mark_requested(ResolveSlot slot) noexcept {
  auto &value = entry(slot);
  if (value.status != ResolveStatus::RequestReady) {
    return false;
  }
  value.status = ResolveStatus::AwaitingResponse;
  return true;
}

void RollingMarketResolver::retry(ResolveSlot slot) noexcept {
  auto &value = entry(slot);
  if (value.status == ResolveStatus::AwaitingResponse ||
      value.status == ResolveStatus::NotFound) {
    value.status = ResolveStatus::RequestReady;
  }
}

bool RollingMarketResolver::apply_result(ResolveSlot slot,
                                         std::string_view response,
                                         std::string &error) {
  auto &value = entry(slot);
  if (value.status != ResolveStatus::AwaitingResponse &&
      value.status != ResolveStatus::RequestReady) {
    error = "Gamma result does not match an outstanding resolver request";
    return false;
  }
  GammaMarket parsed;
  if (!parse_gamma_market_response(response, value.request.slug.view(),
                                   parsed, error)) {
    return false;
  }
  value.market = parsed;
  value.has_market = true;
  value.status = ResolveStatus::Resolved;
  return true;
}

void RollingMarketResolver::apply_not_found(ResolveSlot slot) noexcept {
  auto &value = entry(slot);
  value.has_market = false;
  value.status = ResolveStatus::NotFound;
}

ResolveStatus RollingMarketResolver::status(ResolveSlot slot) const noexcept {
  return entry(slot).status;
}

const GammaMarket *RollingMarketResolver::market(
    ResolveSlot slot) const noexcept {
  const auto &value = entry(slot);
  return value.has_market ? &value.market : nullptr;
}

class PolymarketAdapter::Impl {
 public:
  struct TokenState {
    FixedText<78> asset;
    FixedText<32> canonical;
    std::uint64_t book_sequence{};
    std::uint64_t ticker_sequence{};
    std::int64_t tick_size{10'000};
    MarketLifecycle lifecycle{MarketLifecycle::Unknown};
    bool active{};
  };

  struct PendingEvent {
    AdapterEventType type{AdapterEventType::Ignored};
    InputSide input_side{InputSide::None};
    FixedText<32> symbol;
    std::uint64_t sequence{};
    std::uint64_t timestamp{};
    utils::md::Level bid{};
    utils::md::Level ask{};
    std::array<utils::md::Level, kMaximumBookLevels> bids{};
    std::array<utils::md::Level, kMaximumBookLevels> asks{};
    std::size_t bid_count{};
    std::size_t ask_count{};
    bool sequence_reset{};
    std::int64_t tick_size{};
  };

  explicit Impl(std::size_t maximum)
      : maximum_levels(std::clamp<std::size_t>(
            maximum == 0 ? 1 : maximum, 1, kMaximumBookLevels))
#ifdef MDS_HAS_SIMDJSON
        ,
        json_buffer(kMaximumJsonBytes + simdjson::SIMDJSON_PADDING),
        parser(kMaximumJsonBytes)
#endif
  {
    const auto now = std::chrono::duration_cast<std::chrono::seconds>(
                         std::chrono::system_clock::now().time_since_epoch())
                         .count();
    resolver.reset(now);
  }

  TokenState *find(std::string_view asset) noexcept {
    for (std::size_t index = 0; index < token_count; ++index) {
      if (tokens[index].asset.view() == asset) {
        return &tokens[index];
      }
    }
    return nullptr;
  }

  const TokenState *find(std::string_view asset) const noexcept {
    for (std::size_t index = 0; index < token_count; ++index) {
      if (tokens[index].asset.view() == asset) {
        return &tokens[index];
      }
    }
    return nullptr;
  }

  TokenState *remember(std::string_view asset, std::string_view canonical,
                       std::string &error) noexcept {
    if (auto *known = find(asset)) {
      if (!canonical.empty() && known->canonical.view() != canonical &&
          !known->canonical.assign(canonical)) {
        error = "Polymarket canonical symbol exceeds fixed capacity";
        return nullptr;
      }
      return known;
    }
    if (!decimal_token(asset) || canonical.empty() ||
        token_count == tokens.size() ||
        !tokens[token_count].asset.assign(asset) ||
        !tokens[token_count].canonical.assign(canonical)) {
      error = "Polymarket token state is invalid or capacity is exhausted";
      return nullptr;
    }
    return &tokens[token_count++];
  }

  std::string_view canonical(std::string_view asset) const noexcept {
    const auto *state = find(asset);
    return state == nullptr ? asset : state->canonical.view();
  }

  std::uint64_t next_sequence(std::string_view asset, bool book,
                              std::string &error) noexcept {
    auto *state = find(asset);
    if (state == nullptr) {
      state = remember(asset, asset, error);
    }
    if (state == nullptr) {
      return 0;
    }
    return book ? ++state->book_sequence : ++state->ticker_sequence;
  }

  void clear_queue() noexcept {
    queue_head = 0;
    queue_size = 0;
    gap_pending = false;
  }

  bool enqueue(const PendingEvent &event) noexcept {
    if (queue_size == queue.size()) {
      gap_pending = true;
      gap_symbol = event.symbol;
      return false;
    }
    queue[(queue_head + queue_size) % queue.size()] = event;
    ++queue_size;
    return true;
  }

  bool drain(NormalizedEvent &event, std::string &error) {
    event.reset();
    if (queue_size == 0) {
      if (!gap_pending) {
        return false;
      }
      event.type = AdapterEventType::BookGap;
      if (!mds::exchange::copy_symbol(gap_symbol.view(), event)) {
        error = "Polymarket overflow symbol exceeds event capacity";
        return false;
      }
      gap_pending = false;
      error.clear();
      return true;
    }
    const auto &pending = queue[queue_head];
    queue_head = (queue_head + 1) % queue.size();
    --queue_size;
    event.type = pending.type;
    event.input_side = pending.input_side;
    event.first_sequence = pending.sequence;
    event.final_sequence = pending.sequence;
    event.previous_sequence =
        pending.sequence == 0 ? 0 : pending.sequence - 1;
    event.exchange_time_ms = pending.timestamp;
    event.bid = pending.bid;
    event.ask = pending.ask;
    event.sequence_reset = pending.sequence_reset;
    event.tick_size = pending.tick_size;
    event.strict_previous_sequence = !pending.sequence_reset;
    if (!mds::exchange::copy_symbol(pending.symbol.view(), event)) {
      error = "Polymarket event symbol exceeds capacity";
      return false;
    }
    if (pending.bid_count > event.bids.capacity() ||
        pending.ask_count > event.asks.capacity()) {
      event.reset();
      event.type = AdapterEventType::BookGap;
      (void)mds::exchange::copy_symbol(pending.symbol.view(), event);
      error.clear();
      return true;
    }
    event.bids.insert(event.bids.end(), pending.bids.begin(),
                      pending.bids.begin() + pending.bid_count);
    event.asks.insert(event.asks.end(), pending.asks.begin(),
                      pending.asks.begin() + pending.ask_count);
    error.clear();
    return true;
  }

#ifdef MDS_HAS_SIMDJSON
  simdjson::dom::element parse(std::string_view json) {
    if (json.size() > kMaximumJsonBytes) {
      throw simdjson::simdjson_error(simdjson::CAPACITY);
    }
    std::memcpy(json_buffer.data(), json.data(), json.size());
    std::memset(json_buffer.data() + json.size(), 0,
                simdjson::SIMDJSON_PADDING);
    return parser.parse(json_buffer.data(), json.size(), false).value();
  }

  bool set_identity(PendingEvent &pending, std::string_view asset,
                    std::uint64_t timestamp, bool book,
                    std::string &error) {
    const auto symbol = canonical(asset);
    if (!pending.symbol.assign(symbol)) {
      error = "Polymarket event symbol exceeds fixed capacity";
      return false;
    }
    pending.timestamp = timestamp;
    pending.sequence = next_sequence(asset, book, error);
    return pending.sequence != 0;
  }

  bool parse_level(simdjson::dom::element raw,
                   utils::md::Level &level) {
    auto object_result = raw.get_object();
    if (object_result.error()) {
      return false;
    }
    auto object = object_result.value();
    auto price = object["price"];
    auto size = object["size"];
    return !price.error() && !size.error() &&
           decimal_fixed(price.value(), kPriceScale, level.price) &&
           decimal_fixed(size.value(), kQuantityScale, level.quantity) &&
           level.price >= 0 && level.quantity >= 0;
  }

  bool parse_side(simdjson::dom::element raw,
                  std::array<utils::md::Level, kMaximumBookLevels> &output,
                  std::size_t &count) {
    auto array = raw.get_array();
    if (array.error()) {
      return false;
    }
    count = 0;
    for (auto value : array.value()) {
      if (count == maximum_levels || !parse_level(value, output[count])) {
        return false;
      }
      ++count;
    }
    return true;
  }

  std::uint64_t timestamp(simdjson::dom::object object) noexcept {
    auto value = object["timestamp"];
    return value.error() ? 0 : unsigned_value(value.value());
  }

  bool queue_gap(std::string_view asset, std::uint64_t time,
                 std::string &error) {
    PendingEvent pending;
    if (!set_identity(pending, asset, time, true, error)) {
      return false;
    }
    pending.type = AdapterEventType::BookGap;
    enqueue(pending);
    error.clear();
    return true;
  }

  bool parse_book(simdjson::dom::object object, std::string &error) {
    const auto asset = optional_string(object, "asset_id", "assetId");
    if (!decimal_token(asset)) {
      error = "Polymarket book is missing a decimal asset id";
      return false;
    }
    if (const auto *known = find(asset); known != nullptr && !known->active) {
      ++stale_message_count;
      error.clear();
      return true;
    }
    auto raw_tick = object["tick_size"];
    if (raw_tick.error()) {
      raw_tick = object["tickSize"];
    }
    if (!raw_tick.error()) {
      PendingEvent instrument;
      if (!set_identity(instrument, asset, timestamp(object), false, error) ||
          !decimal_fixed(raw_tick.value(), kPriceScale,
                         instrument.tick_size) ||
          instrument.tick_size <= 0) {
        error = "invalid Polymarket book tick size";
        return false;
      }
      instrument.type = AdapterEventType::InstrumentUpdate;
      if (auto *state = find(asset)) {
        state->tick_size = instrument.tick_size;
      }
      enqueue(instrument);
    }
    PendingEvent pending;
    if (!set_identity(pending, asset, timestamp(object), true, error)) {
      return false;
    }
    pending.type = AdapterEventType::BookSnapshot;
    pending.sequence_reset = true;
    auto bids = object["bids"];
    auto asks = object["asks"];
    if (bids.error() || asks.error() ||
        !parse_side(bids.value(), pending.bids, pending.bid_count) ||
        !parse_side(asks.value(), pending.asks, pending.ask_count)) {
      return queue_gap(asset, timestamp(object), error);
    }
    enqueue(pending);
    error.clear();
    return true;
  }

  bool queue_bbo(simdjson::dom::object object, std::string_view asset,
                 std::uint64_t time, std::string &error) {
    auto raw_bid = object["best_bid"];
    if (raw_bid.error()) {
      raw_bid = object["bestBid"];
    }
    auto raw_ask = object["best_ask"];
    if (raw_ask.error()) {
      raw_ask = object["bestAsk"];
    }
    if (raw_bid.error() && raw_ask.error()) {
      return true;
    }
    PendingEvent pending;
    if (!set_identity(pending, asset, time, false, error)) {
      return false;
    }
    pending.type = AdapterEventType::Bbo;
    if (!raw_bid.error() &&
        !decimal_fixed(raw_bid.value(), kPriceScale,
                       pending.bid.price)) {
      error = "invalid Polymarket best bid";
      return false;
    }
    if (!raw_ask.error() &&
        !decimal_fixed(raw_ask.value(), kPriceScale,
                       pending.ask.price)) {
      error = "invalid Polymarket best ask";
      return false;
    }
    enqueue(pending);
    return true;
  }

  bool parse_price_change(simdjson::dom::object object,
                          std::string &error) {
    auto changes_result = object["price_changes"].get_array();
    if (changes_result.error()) {
      changes_result = object["priceChanges"].get_array();
    }
    if (changes_result.error()) {
      error = "Polymarket price_change has no changes";
      return false;
    }
    const auto time = timestamp(object);
    for (auto raw : changes_result.value()) {
      auto change = raw.get_object();
      if (change.error()) {
        error = "invalid Polymarket price_change entry";
        return false;
      }
      auto value = change.value();
      const auto asset = optional_string(value, "asset_id", "assetId");
      const auto side = optional_string(value, "side");
      auto price = value["price"];
      auto size = value["size"];
      if (!decimal_token(asset) || price.error() || size.error() ||
          (!iequals(side, "BUY") && !iequals(side, "SELL"))) {
        error = "incomplete Polymarket price_change entry";
        return false;
      }
      if (const auto *known = find(asset); known != nullptr && !known->active) {
        ++stale_message_count;
        continue;
      }
      PendingEvent pending;
      if (!set_identity(pending, asset, time, true, error)) {
        return false;
      }
      pending.type = AdapterEventType::BookDelta;
      utils::md::Level level;
      if (!decimal_fixed(price.value(), kPriceScale, level.price) ||
          !decimal_fixed(size.value(), kQuantityScale, level.quantity)) {
        error = "invalid Polymarket price_change price or size";
        return false;
      }
      if (iequals(side, "BUY")) {
        pending.bids[0] = level;
        pending.bid_count = 1;
        pending.input_side = InputSide::Bid;
      } else {
        pending.asks[0] = level;
        pending.ask_count = 1;
        pending.input_side = InputSide::Ask;
      }
      enqueue(pending);
      if (!queue_bbo(value, asset, time, error)) {
        return false;
      }
    }
    error.clear();
    return true;
  }

  bool parse_bbo(simdjson::dom::object object, std::string &error) {
    const auto asset = optional_string(object, "asset_id", "assetId");
    if (!decimal_token(asset)) {
      error = "Polymarket BBO is missing a decimal asset id";
      return false;
    }
    if (const auto *known = find(asset); known != nullptr && !known->active) {
      ++stale_message_count;
      error.clear();
      return true;
    }
    if (!queue_bbo(object, asset, timestamp(object), error)) {
      return false;
    }
    error.clear();
    return true;
  }

  bool parse_lifecycle(simdjson::dom::object object,
                       std::string_view type, std::string &error) {
    const auto asset = optional_string(object, "asset_id", "assetId");
    if (!asset.empty() && !decimal_token(asset)) {
      error = "Polymarket lifecycle event has invalid asset id";
      return false;
    }
    if (auto *state = find(asset)) {
      if (type == "tick_size_change") {
        auto raw = object["new_tick_size"];
        if (raw.error()) {
          raw = object["newTickSize"];
        }
        std::int64_t tick{};
        if (raw.error() ||
            !decimal_fixed(raw.value(), kPriceScale, tick) ||
            tick <= 0) {
          error = "invalid Polymarket tick size change";
          return false;
        }
        state->tick_size = tick;
        PendingEvent pending;
        if (!set_identity(pending, asset, timestamp(object), false, error)) {
          return false;
        }
        pending.type = AdapterEventType::InstrumentUpdate;
        pending.tick_size = tick;
        enqueue(pending);
      } else if (type == "new_market") {
        state->lifecycle = MarketLifecycle::Active;
      } else {
        const auto outcome =
            optional_string(object, "winning_outcome", "winningOutcome");
        state->lifecycle =
            iequals(outcome, "up") || iequals(outcome, "yes")
                ? MarketLifecycle::ResolvedUp
                : MarketLifecycle::ResolvedDown;
      }
    }
    if (!asset.empty() && type != "tick_size_change") {
      PendingEvent pending;
      if (!set_identity(pending, asset, timestamp(object), false, error)) {
        return false;
      }
      pending.type = AdapterEventType::Ignored;
      enqueue(pending);
    }
    error.clear();
    return true;
  }

  bool parse_object(simdjson::dom::object object, std::string &error) {
    auto outer_type = optional_string(object, "event_type", "eventType");
    if (outer_type.empty()) {
      outer_type = optional_string(object, "type");
    }
    auto payload = object["data"].get_object();
    if (!payload.error()) {
      object = payload.value();
    } else {
      payload = object["payload"].get_object();
      if (!payload.error()) {
        object = payload.value();
      }
    }
    auto type = optional_string(object, "event_type", "eventType");
    if (type.empty()) {
      type = optional_string(object, "type");
    }
    if (type.empty()) {
      type = outer_type;
    }
    if (type == "priceChange") {
      type = "price_change";
    } else if (type == "bestBidAsk") {
      type = "best_bid_ask";
    } else if (type == "tickSizeChange") {
      type = "tick_size_change";
    } else if (type == "newMarket") {
      type = "new_market";
    } else if (type == "marketResolved") {
      type = "market_resolved";
    }
    if (type == "book") {
      return parse_book(object, error);
    }
    if (type == "price_change") {
      return parse_price_change(object, error);
    }
    if (type == "best_bid_ask") {
      return parse_bbo(object, error);
    }
    if (type == "tick_size_change" || type == "new_market" ||
        type == "market_resolved") {
      return parse_lifecycle(object, type, error);
    }
    const auto asset = optional_string(object, "asset_id", "assetId");
    if (!asset.empty() &&
        (!object["best_bid"].error() || !object["bestBid"].error() ||
         !object["best_ask"].error() || !object["bestAsk"].error())) {
      return parse_bbo(object, error);
    }
    if (!object["price_changes"].error() ||
        !object["priceChanges"].error()) {
      return parse_price_change(object, error);
    }
    error.clear();
    return true;
  }

  bool parse_document(simdjson::dom::element document,
                      std::string &error) {
    auto array = document.get_array();
    if (!array.error()) {
      for (auto raw : array.value()) {
        auto object = raw.get_object();
        if (object.error() || !parse_object(object.value(), error)) {
          if (object.error()) {
            error = "Polymarket WSS array contains a non-object";
          }
          return false;
        }
      }
      return true;
    }
    auto object = document.get_object();
    if (object.error()) {
      error = "Polymarket WSS message must be an object or array";
      return false;
    }
    return parse_object(object.value(), error);
  }
#endif

  std::size_t maximum_levels;
  RollingMarketResolver resolver;
  std::array<TokenState, kMaximumTokens> tokens{};
  std::size_t token_count{};
  std::array<PendingEvent, kMaximumPendingEvents> queue{};
  std::size_t queue_head{};
  std::size_t queue_size{};
  bool gap_pending{};
  FixedText<32> gap_symbol{};
  std::uint64_t stale_message_count{};
#ifdef MDS_HAS_SIMDJSON
  std::vector<char> json_buffer;
  simdjson::dom::parser parser;
#endif
};

PolymarketAdapter::PolymarketAdapter(std::size_t max_levels_per_side)
    : impl_(std::make_unique<Impl>(max_levels_per_side)) {}

PolymarketAdapter::~PolymarketAdapter() = default;
PolymarketAdapter::PolymarketAdapter(PolymarketAdapter &&) noexcept = default;
PolymarketAdapter &PolymarketAdapter::operator=(
    PolymarketAdapter &&) noexcept = default;

utils::md::Venue PolymarketAdapter::venue() const noexcept {
  return utils::md::Venue::Polymarket;
}

utils::md::ProductType PolymarketAdapter::product() const noexcept {
  return utils::md::ProductType::BinaryOption;
}

HeartbeatSpec PolymarketAdapter::heartbeat() const {
  return {HeartbeatKind::TextPing, "PING", 10'000};
}

void PolymarketAdapter::reset_connection_state() noexcept {
  impl_->clear_queue();
  for (std::size_t index = 0; index < impl_->token_count; ++index) {
    impl_->tokens[index].book_sequence = 0;
    impl_->tokens[index].ticker_sequence = 0;
  }
}

std::size_t PolymarketAdapter::expected_subscription_acks(
    std::string_view) const noexcept {
  return 0;
}

bool PolymarketAdapter::build_dynamic_operation(
    std::span<const std::string_view> asset_ids, bool subscribe,
    std::string &payload, std::string &error) const {
  if (asset_ids.empty()) {
    error = "Polymarket operation requires at least one asset id";
    return false;
  }
  std::string built{"{\"assets_ids\":["};
  for (std::size_t index = 0; index < asset_ids.size(); ++index) {
    if (!decimal_token(asset_ids[index])) {
      error = "Polymarket operation asset id must be a decimal token id";
      return false;
    }
    if (index != 0) {
      built.push_back(',');
    }
    built.push_back('"');
    built.append(asset_ids[index]);
    built.push_back('"');
  }
  built.append("],\"operation\":\"");
  built.append(subscribe ? "subscribe" : "unsubscribe");
  built.append("\"}");
  payload = std::move(built);
  error.clear();
  return true;
}

bool PolymarketAdapter::build_subscription_batches(
    std::span<const StreamRequest> requests,
    std::vector<std::string> &batches, std::string &error) const {
  if (requests.empty()) {
    error = "Polymarket subscription requires at least one symbol";
    return false;
  }
  std::array<std::string_view, kMaximumTokens> assets{};
  std::size_t count = 0;
  for (const auto &request : requests) {
    auto asset = request.venue_symbol;
    auto canonical = request.canonical_symbol.empty()
                         ? request.venue_symbol
                         : request.canonical_symbol;
    auto outcome = logical_outcome(asset);
    if (!outcome) {
      outcome = logical_outcome(canonical);
    }
    if (outcome) {
      const auto *market = impl_->resolver.market(ResolveSlot::Current);
      if (market == nullptr) {
        error = "logical Polymarket symbol requires resolved Gamma metadata";
        return false;
      }
      asset = market->token(*outcome);
    }
    if (!decimal_token(asset) ||
        impl_->remember(asset, canonical, error) == nullptr) {
      if (error.empty()) {
        error = "Polymarket subscription has an invalid token id";
      }
      return false;
    }
    if (std::find(assets.begin(), assets.begin() + count, asset) ==
        assets.begin() + count) {
      if (count == assets.size()) {
        error = "Polymarket subscription token capacity exhausted";
        return false;
      }
      assets[count++] = asset;
    }
  }
  std::string batch{"{\"assets_ids\":["};
  for (std::size_t index = 0; index < count; ++index) {
    if (index != 0) {
      batch.push_back(',');
    }
    batch.push_back('"');
    batch.append(assets[index]);
    batch.push_back('"');
  }
  batch.append("],\"type\":\"market\",\"custom_feature_enabled\":true}");
  batches = {std::move(batch)};
  error.clear();
  return true;
}

bool PolymarketAdapter::build_unsubscription_batches(
    std::span<const StreamRequest> requests,
    std::vector<std::string> &batches, std::string &error) const {
  const auto *market = impl_->resolver.market(ResolveSlot::Current);
  if (market == nullptr || requests.empty()) {
    error = "Polymarket unsubscribe requires resolved current metadata";
    return false;
  }
  std::array<std::string_view, 2> assets{};
  std::size_t count = 0;
  for (const auto &request : requests) {
    auto outcome = logical_outcome(request.canonical_symbol);
    if (!outcome) {
      outcome = logical_outcome(request.venue_symbol);
    }
    if (!outcome) {
      error = "Polymarket unsubscribe requires a logical outcome";
      return false;
    }
    const auto asset = market->token(*outcome);
    if (std::find(assets.begin(), assets.begin() + count, asset) ==
        assets.begin() + count) {
      assets[count++] = asset;
    }
  }
  std::array<std::string_view, 2> selected{};
  std::copy_n(assets.begin(), count, selected.begin());
  std::string payload;
  if (!build_dynamic_operation(
          std::span<const std::string_view>(selected.data(), count), false,
          payload, error)) {
    return false;
  }
  batches = {std::move(payload)};
  return true;
}

bool PolymarketAdapter::parse_ws(std::string_view json,
                                 NormalizedEvent &event,
                                 std::string &error) {
  event.reset();
  if (json.empty()) {
    impl_->drain(event, error);
    error.clear();
    return true;
  }
  auto control = json;
  while (!control.empty() &&
         std::isspace(static_cast<unsigned char>(control.front()))) {
    control.remove_prefix(1);
  }
  while (!control.empty() &&
         std::isspace(static_cast<unsigned char>(control.back()))) {
    control.remove_suffix(1);
  }
  if (control == "PONG" || control == "pong") {
    event.type = AdapterEventType::Pong;
    error.clear();
    return true;
  }
#ifndef MDS_HAS_SIMDJSON
  error = "simdjson support was not compiled";
  return false;
#else
  try {
    if (!impl_->parse_document(impl_->parse(json), error)) {
      event.reset();
      return false;
    }
    if (!impl_->drain(event, error)) {
      event.reset();
      error.clear();
    }
    return true;
  } catch (const simdjson::simdjson_error &exception) {
    event.reset();
    error = exception.what();
    error.append(" payload=");
    error.append(json.substr(0, std::min<std::size_t>(json.size(), 64)));
    return false;
  }
#endif
}

bool PolymarketAdapter::has_pending_events() const noexcept {
  return impl_->queue_size != 0 || impl_->gap_pending;
}

HttpRequestSpec PolymarketAdapter::metadata_request() const {
  auto request = impl_->resolver.next_request();
  return {HttpRequestSpec::Method::Get,
          request ? std::string(request->target.view()) : "/events?slug=",
          {}, {}};
}

HttpRequestSpec PolymarketAdapter::metadata_request(
    std::span<const StreamRequest>) const {
  return metadata_request();
}

bool PolymarketAdapter::build_metadata_request_batches(
    std::span<const StreamRequest> requests,
    std::vector<MetadataRequestBatch> &batches,
    std::string &error) const {
  if (requests.empty()) {
    error = "Polymarket metadata requires at least one logical symbol";
    return false;
  }
  auto request = impl_->resolver.next_request();
  if (!request) {
    error = "Polymarket resolver has no Gamma request ready";
    return false;
  }
  batches = {{metadata_request(requests), 0, requests.size(), false, {}}};
  error.clear();
  return true;
}

bool PolymarketAdapter::parse_metadata(
    std::string_view json, std::span<const StreamRequest> requests,
    std::vector<InstrumentMetadata> &metadata, std::string &error) {
  auto request = impl_->resolver.next_request();
  ResolveSlot slot =
      request ? request->slot : ResolveSlot::Current;
  const auto *resolved = impl_->resolver.market(slot);
  if (resolved == nullptr &&
      !impl_->resolver.apply_result(slot, json, error)) {
    return false;
  }
  resolved = impl_->resolver.market(slot);
  if (resolved == nullptr) {
    error = "Polymarket Gamma metadata did not resolve a market";
    return false;
  }
  return build_resolved_metadata(slot, requests, metadata, error);
}

bool PolymarketAdapter::build_resolved_metadata(
    ResolveSlot slot, std::span<const StreamRequest> requests,
    std::vector<InstrumentMetadata> &metadata, std::string &error) {
  const auto *resolved = impl_->resolver.market(slot);
  if (resolved == nullptr) {
    error = "Polymarket resolver slot has no market";
    return false;
  }
  for (std::size_t index = 0; index < impl_->token_count; ++index) {
    impl_->tokens[index].active = false;
  }
  std::vector<InstrumentMetadata> parsed;
  parsed.reserve(requests.size());
  for (const auto &request_item : requests) {
    auto outcome = logical_outcome(request_item.venue_symbol);
    if (!outcome) {
      outcome = logical_outcome(request_item.canonical_symbol);
    }
    std::string_view token = request_item.venue_symbol;
    if (outcome) {
      token = resolved->token(*outcome);
    } else if (token == resolved->up_token_id.view()) {
      outcome = Outcome::Up;
    } else if (token == resolved->down_token_id.view()) {
      outcome = Outcome::Down;
    } else {
      error = "Polymarket metadata request is not BTC5MUP or BTC5MDOWN";
      return false;
    }
    const auto canonical =
        request_item.canonical_symbol.empty()
            ? (*outcome == Outcome::Up ? std::string_view{"BTC5MUP"}
                                       : std::string_view{"BTC5MDOWN"})
            : request_item.canonical_symbol;
    auto *token_state = impl_->remember(token, canonical, error);
    if (token_state == nullptr) {
      return false;
    }
    token_state->active = true;
    InstrumentMetadata value;
    value.canonical_symbol = canonical;
    value.venue_symbol = std::string(resolved->slug.view());
    value.venue_symbol.append(*outcome == Outcome::Up ? ":UP" : ":DOWN");
    value.base_asset = *outcome == Outcome::Up ? "BTC5MUP" : "BTC5MDOWN";
    value.quote_asset = "USDC";
    value.settle_asset = "USDC";
    value.price_scale = kPriceScale;
    value.quantity_scale = kQuantityScale;
    value.tick_size = 10'000;
    value.lot_size = resolved->minimum_order_size;
    value.contract_multiplier = 1;
    value.signature_type = resolved->signature_type;
    value.negative_risk = resolved->negative_risk;
    parsed.push_back(std::move(value));
  }
  metadata = std::move(parsed);
  error.clear();
  return true;
}

void PolymarketAdapter::prepare_resolution(
    std::int64_t unix_seconds) noexcept {
  impl_->resolver.advance(unix_seconds);
}

std::optional<GammaRequest> PolymarketAdapter::next_gamma_request() const
    noexcept {
  return impl_->resolver.next_request();
}

bool PolymarketAdapter::mark_gamma_requested(ResolveSlot slot) noexcept {
  return impl_->resolver.mark_requested(slot);
}

void PolymarketAdapter::retry_gamma(ResolveSlot slot) noexcept {
  impl_->resolver.retry(slot);
}

bool PolymarketAdapter::apply_gamma_response(ResolveSlot slot,
                                             std::string_view response,
                                             std::string &error) {
  return impl_->resolver.apply_result(slot, response, error);
}

const GammaMarket *PolymarketAdapter::resolved_market(
    ResolveSlot slot) const noexcept {
  return impl_->resolver.market(slot);
}

MarketLifecycle PolymarketAdapter::lifecycle(
    std::string_view asset_id) const noexcept {
  const auto *state = impl_->find(asset_id);
  return state == nullptr ? MarketLifecycle::Unknown : state->lifecycle;
}

std::uint64_t PolymarketAdapter::stale_messages() const noexcept {
  return impl_->stale_message_count;
}

std::unique_ptr<PolymarketAdapter> make_polymarket_adapter(
    std::size_t max_levels_per_side) {
  return std::make_unique<PolymarketAdapter>(max_levels_per_side);
}

}  // namespace mds::exchange::polymarket
