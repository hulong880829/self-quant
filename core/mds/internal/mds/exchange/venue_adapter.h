#pragma once

#include "mds/book/book_bridge.h"
#include "mds/exchange/capabilities.h"
#include "utils/md/decimal.h"

#include <algorithm>
#include <array>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <span>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

namespace mds::exchange {

enum class AdapterEventType : std::uint8_t {
  Ignored,
  SubscribeAck,
  SubscribeError,
  Pong,
  Bbo,
  BookSnapshot,
  BookDelta,
  BookGap,
  InstrumentUpdate,
};

enum class InputSide : std::uint8_t { None, Bid, Ask };

enum class IgnoreReason : std::uint8_t {
  None,
  OneSidedBook,
};

enum class SubscribeErrorKind : std::uint8_t {
  Unknown,
  SymbolUnavailable,
  TransientRateLimit,
};

enum class SubscriptionStream : std::uint8_t {
  Unknown,
  Ticker,
  Orderbook,
};

enum class ParseFailureCategory : std::uint8_t {
  None,
  DirtyData,
  SequenceGap,
  Protocol,
  ConfigurationMetadata,
};

enum class ParseFailureScope : std::uint8_t {
  None,
  Symbol,
  Shard,
  Venue,
};

enum class ParseFailureCode : std::uint16_t {
  None,
  MalformedPayload,
  InvalidSymbol,
  InvalidPrice,
  InvalidQuantity,
  CapacityExceeded,
  SequenceDiscontinuity,
  UnsupportedMessage,
  MetadataUnavailable,
  ScaleMismatch,
};

struct ParseFailure {
  static constexpr std::size_t kSymbolCapacity = 32;
  static constexpr std::size_t kDiagnosticCapacity = 384;

  ParseFailureCategory category{ParseFailureCategory::None};
  ParseFailureScope scope{ParseFailureScope::None};
  ParseFailureCode code{ParseFailureCode::None};
  std::array<char, kSymbolCapacity + 1> symbol{};
  std::uint8_t symbol_size{};
  std::array<char, kDiagnosticCapacity + 1> diagnostic{};
  std::uint16_t diagnostic_size{};

  void clear() noexcept { *this = {}; }

  [[nodiscard]] bool set_symbol(std::string_view value) noexcept {
    if (value.empty() || value.size() > kSymbolCapacity) {
      symbol.fill('\0');
      symbol_size = 0;
      return false;
    }
    std::copy(value.begin(), value.end(), symbol.begin());
    symbol[value.size()] = '\0';
    symbol_size = static_cast<std::uint8_t>(value.size());
    return true;
  }

  void set_diagnostic(std::string_view value) noexcept {
    const auto size = std::min(value.size(), kDiagnosticCapacity);
    std::copy_n(value.begin(), size, diagnostic.begin());
    diagnostic[size] = '\0';
    diagnostic_size = static_cast<std::uint16_t>(size);
  }

  [[nodiscard]] bool has_symbol() const noexcept {
    return symbol_size != 0;
  }

  [[nodiscard]] std::string_view symbol_view() const noexcept {
    return {symbol.data(), symbol_size};
  }

  [[nodiscard]] std::string_view diagnostic_view() const noexcept {
    return {diagnostic.data(), diagnostic_size};
  }
};

static_assert(sizeof(ParseFailure) <= 432);

struct StreamRequest {
  std::string_view canonical_symbol;
  std::string_view venue_symbol;
  std::string_view ticker_channel;
  std::string_view orderbook_channel;
  bool ticker{};
  bool orderbook{};
  std::uint32_t update_interval_ms{};
};

struct InstrumentMetadata {
  std::string canonical_symbol;
  std::string venue_symbol;
  std::string base_asset;
  std::string quote_asset;
  std::string settle_asset;
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
  std::int64_t tick_size{};
  std::int64_t lot_size{};
  std::int64_t contract_multiplier{1};
  std::uint64_t turnover_24h{};
  std::uint8_t contract_multiplier_scale{};
  std::uint8_t signature_type{};
  bool negative_risk{};
  bool refine_book_tick{};
};

[[nodiscard]] bool decimal_to_turnover(std::string_view value,
                                       std::uint64_t &turnover) noexcept;
[[nodiscard]] bool decimal_product_to_turnover(
    std::string_view quantity, std::string_view price,
    std::uint64_t &turnover) noexcept;

struct NormalizedEvent {
  AdapterEventType type{AdapterEventType::Ignored};
  std::array<char, 33> symbol{};
  std::size_t symbol_size{};
  std::uint64_t first_sequence{};
  std::uint64_t final_sequence{};
  std::uint64_t previous_sequence{};
  std::uint64_t exchange_time_ms{};
  IgnoreReason ignore_reason{IgnoreReason::None};
  SubscribeErrorKind subscribe_error_kind{SubscribeErrorKind::Unknown};
  SubscriptionStream subscription_stream{SubscriptionStream::Unknown};
  std::uint8_t normalized_tail_fields{};
  utils::md::Level bid{};
  utils::md::Level ask{};
  std::vector<utils::md::Level> bids;
  std::vector<utils::md::Level> asks;
  bool sequence_reset{};
  bool strict_previous_sequence{};
  InputSide input_side{InputSide::None};
  std::size_t input_capacity{};
  std::int64_t tick_size{};

  explicit NormalizedEvent(std::size_t max_levels_per_side = 1000) {
    bids.reserve(max_levels_per_side);
    asks.reserve(max_levels_per_side);
  }

  void reset() noexcept {
    type = AdapterEventType::Ignored;
    symbol_size = 0;
    first_sequence = 0;
    final_sequence = 0;
    previous_sequence = 0;
    exchange_time_ms = 0;
    ignore_reason = IgnoreReason::None;
    subscribe_error_kind = SubscribeErrorKind::Unknown;
    subscription_stream = SubscriptionStream::Unknown;
    normalized_tail_fields = 0;
    bid = {};
    ask = {};
    bids.clear();
    asks.clear();
    sequence_reset = false;
    strict_previous_sequence = false;
    input_side = InputSide::None;
    input_capacity = 0;
    tick_size = 0;
  }

  [[nodiscard]] std::string_view symbol_view() const noexcept {
    return {symbol.data(), symbol_size};
  }
};

[[nodiscard]] bool decimal_scale_mismatch(
    std::string_view value, std::uint8_t configured_scale) noexcept;
[[nodiscard]] bool classify_scale_mismatch(
    const NormalizedEvent &event, ParseFailure &failure) noexcept;

struct HttpRequestSpec {
  enum class Method : std::uint8_t { Get, Post };
  Method method{Method::Get};
  std::string target;
  std::string content_type;
  std::string body;
};

struct MetadataRequestBatch {
  HttpRequestSpec http;
  std::size_t request_offset{};
  std::size_t request_count{};
  bool cursor_paginated{};
  std::string page_cursor;
};

enum class MetadataResponseKind : std::uint8_t {
  Success,
  SymbolUnavailable,
  ConnectionFailure,
};

struct MetadataResponse {
  MetadataResponseKind kind{MetadataResponseKind::ConnectionFailure};
  std::string reason;
};

enum class HeartbeatKind : std::uint8_t {
  Rfc6455Ping,
  TextPing,
  JsonPing,
};

struct HeartbeatSpec {
  HeartbeatKind kind{HeartbeatKind::Rfc6455Ping};
  std::string payload;
  std::uint32_t interval_ms{20'000};
};

class VenueAdapter {
 public:
  virtual ~VenueAdapter() = default;

  [[nodiscard]] virtual utils::md::Venue venue() const noexcept = 0;
  [[nodiscard]] virtual utils::md::ProductType product() const noexcept = 0;
  [[nodiscard]] virtual HeartbeatSpec heartbeat() const = 0;
  virtual void reset_connection_state() noexcept {}
  [[nodiscard]] virtual std::size_t expected_subscription_acks(
      std::string_view batch) const noexcept {
    constexpr std::string_view marker{"\"channel\""};
    std::size_t count = 0;
    for (std::size_t offset = 0;
         (offset = batch.find(marker, offset)) != std::string_view::npos;
         offset += marker.size()) {
      ++count;
    }
    return count == 0 ? 1 : count;
  }
  [[nodiscard]] virtual std::size_t subscription_send_window()
      const noexcept {
    return 1;
  }

  virtual bool build_subscription_batches(
      std::span<const StreamRequest> requests,
      std::vector<std::string> &batches, std::string &error) const = 0;
  virtual bool build_unsubscription_batches(
      std::span<const StreamRequest> requests,
      std::vector<std::string> &batches, std::string &error) const {
    std::vector<std::string> subscribe;
    if (!build_subscription_batches(requests, subscribe, error)) {
      return false;
    }
    std::vector<std::string> built;
    built.reserve(subscribe.size());
    for (auto &message : subscribe) {
      const auto operation = message.find("\"subscribe\"");
      if (operation == std::string::npos) {
        error = "venue subscription cannot be converted to unsubscribe";
        return false;
      }
      message.replace(operation, std::string("\"subscribe\"").size(),
                      "\"unsubscribe\"");
      built.push_back(std::move(message));
    }
    batches = std::move(built);
    error.clear();
    return true;
  }
  virtual bool parse_ws(std::string_view json, NormalizedEvent &event,
                        std::string &error) = 0;
  bool parse_ws(std::string_view json, NormalizedEvent &event,
                ParseFailure &failure) {
    failure.clear();
    std::string diagnostic;
    const bool parsed = parse_ws(json, event, diagnostic);
    failure.set_diagnostic(diagnostic);
    if (parsed) {
      return true;
    }
    failure.category = ParseFailureCategory::Protocol;
    failure.scope = ParseFailureScope::Shard;
    failure.code = ParseFailureCode::MalformedPayload;
    classify_parse_failure(json, event, failure);
    return false;
  }
  [[nodiscard]] virtual bool has_pending_events() const noexcept {
    return false;
  }

  [[nodiscard]] virtual HttpRequestSpec metadata_request() const = 0;
  [[nodiscard]] virtual HttpRequestSpec
  metadata_request(std::span<const StreamRequest>) const {
    return metadata_request();
  }
  [[nodiscard]] virtual HttpRequestSpec
  discovery_metadata_request(std::string_view cursor) const {
    (void)cursor;
    return metadata_request();
  }
  virtual bool discovery_metadata_next_cursor(
      std::string_view, std::string &cursor, std::string &error) {
    cursor.clear();
    error.clear();
    return true;
  }
  virtual bool discovery_metadata_next_cursor(
      std::string_view request_cursor, std::string_view json,
      std::string &cursor, std::string &error) {
    (void)request_cursor;
    return discovery_metadata_next_cursor(json, cursor, error);
  }
  [[nodiscard]] virtual bool discovery_page_is_optional(
      std::string_view cursor) const noexcept {
    (void)cursor;
    return false;
  }
  [[nodiscard]] virtual bool
  discovery_metadata_includes_turnover() const noexcept {
    return false;
  }
  virtual bool build_metadata_request_batches(
      std::span<const StreamRequest> requests,
      std::vector<MetadataRequestBatch> &batches,
      std::string &error) const {
    if (requests.empty()) {
      error = "metadata request requires at least one symbol";
      return false;
    }
    std::vector<MetadataRequestBatch> built;
    built.push_back({metadata_request(requests), 0, requests.size(), false,
                     {}});
    batches = std::move(built);
    error.clear();
    return true;
  }
  virtual bool build_bootstrap_metadata_request_batches(
      std::span<const StreamRequest> requests,
      std::vector<MetadataRequestBatch> &batches,
      std::string &error) const {
    return build_metadata_request_batches(requests, batches, error);
  }
  virtual bool apply_metadata_page_cursor(
      MetadataRequestBatch &batch, std::string_view cursor,
      std::string &error) const {
    (void)batch;
    (void)cursor;
    error = "metadata pagination is not supported";
    return false;
  }
  virtual bool metadata_page_list_empty(
      std::string_view json, bool &empty, std::string &error) {
    (void)json;
    empty = false;
    error.clear();
    return true;
  }
  virtual bool metadata_page_repeated_venue_symbol(
      std::string_view json, std::string &symbol, std::string &error) {
    (void)json;
    symbol.clear();
    error.clear();
    return true;
  }
  virtual bool parse_metadata(
      std::string_view json, std::span<const StreamRequest> requests,
      std::vector<InstrumentMetadata> &metadata,
      std::string &error) = 0;
  virtual bool upsert_metadata(
      std::string_view json, std::span<const StreamRequest> requests,
      std::vector<InstrumentMetadata> &metadata,
      std::string &error) {
    (void)json;
    (void)requests;
    (void)metadata;
    error = "incremental venue metadata refresh is unsupported";
    return false;
  }
  virtual bool parse_discovery_metadata(
      std::string_view json, std::span<const StreamRequest> requests,
      std::vector<InstrumentMetadata> &metadata, std::string &error) {
    return parse_metadata(json, requests, metadata, error);
  }
  virtual MetadataResponse parse_metadata_response(
      std::string_view json, std::span<const StreamRequest> requests,
      std::vector<InstrumentMetadata> &metadata) {
    std::string error;
    if (!parse_discovery_metadata(json, requests, metadata, error)) {
      return {MetadataResponseKind::ConnectionFailure,
              error.empty() ? "failed to parse venue metadata"
                            : std::move(error)};
    }
    if (requests.size() == 1) {
      const auto &request = requests.front();
      const bool found = std::any_of(
          metadata.begin(), metadata.end(),
          [&request](const InstrumentMetadata &instrument) {
            return instrument.venue_symbol == request.venue_symbol ||
                   instrument.canonical_symbol == request.canonical_symbol;
          });
      if (!found) {
        return {MetadataResponseKind::SymbolUnavailable,
                "successful exact metadata lookup omitted requested symbol"};
      }
    }
    return {MetadataResponseKind::Success, {}};
  }
  [[nodiscard]] virtual HttpRequestSpec
  discovery_turnover_request() const {
    return {};
  }
  virtual bool enrich_discovery_turnover(
      std::string_view, std::span<InstrumentMetadata>,
      std::string &error) {
    error = "24h turnover discovery is unsupported";
    return false;
  }

  [[nodiscard]] virtual bool needs_rest_snapshot() const noexcept {
    return false;
  }
  [[nodiscard]] virtual HttpRequestSpec
  snapshot_request(std::string_view, std::size_t) const {
    return {};
  }
  virtual bool parse_snapshot(std::string_view, std::string_view,
                              NormalizedEvent &, std::string &error) {
    error = "REST snapshot is not supported by this adapter";
    return false;
  }

 protected:
  virtual void classify_parse_failure(std::string_view,
                                      const NormalizedEvent &,
                                      ParseFailure &) const noexcept {}
};

[[nodiscard]] std::unique_ptr<VenueAdapter>
make_venue_adapter(utils::md::Venue venue,
                   utils::md::ProductType product,
                   std::size_t max_levels_per_side = 1000);

using utils::md::decimal_scale;
using utils::md::decimal_to_fixed;

[[nodiscard]] bool copy_symbol(std::string_view value,
                               NormalizedEvent &event) noexcept;

}  // namespace mds::exchange
