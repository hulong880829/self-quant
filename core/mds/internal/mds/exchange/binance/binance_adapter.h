#pragma once

#include "mds/api/mds_api.h"

#include <array>
#include <cstddef>
#include <cstdint>
#include <deque>
#include <span>
#include <string>
#include <string_view>
#include <vector>

namespace mds::exchange::binance {

enum class Profile : std::uint8_t { Spot, UsdM };
enum class Availability : std::uint8_t { Unavailable, Available };
enum class DepthParseResult : std::uint8_t {
  Ok,
  CapacityExceeded,
  Invalid,
};
enum class DepthSide : std::uint8_t { None, Bid, Ask };
inline constexpr std::size_t kDefaultMaxDepthLevelsPerSide = 5000;

struct Capability {
  Profile profile{};
  std::string_view websocket_host{};
  std::string_view rest_host{};
  std::string_view websocket_combined_path{};
  std::string_view exchange_info_path{};
  std::string_view depth_path{};
  bool book_ticker{true};
  bool diff_depth{true};
  bool json{true};
  Availability sbe{Availability::Unavailable};
  bool requires_previous_final_id{};
};

Capability capability(Profile profile) noexcept;

struct SymbolMetadata {
  std::string venue_symbol{};
  std::string base_asset{};
  std::string quote_asset{};
  std::string settle_asset{};
  std::int64_t tick_mantissa{};
  std::int64_t lot_mantissa{};
  std::int8_t price_scale{};
  std::int8_t quantity_scale{};
};

using PriceLevel = utils::md::Level;

struct BookTicker {
  std::uint64_t update_id{};
  std::int64_t bid_price{};
  std::int64_t bid_quantity{};
  std::int64_t ask_price{};
  std::int64_t ask_quantity{};
  std::int8_t price_exponent{};
  std::int8_t quantity_exponent{};
  std::uint64_t event_time_ms{};
  std::uint64_t transaction_time_ms{};
  std::array<char, 32> symbol{};
};

struct DepthUpdate {
  std::uint64_t first_update_id{};
  std::uint64_t final_update_id{};
  std::uint64_t previous_final_update_id{};
  std::uint64_t event_time_ms{};
  std::uint64_t transaction_time_ms{};
  std::int8_t price_exponent{};
  std::int8_t quantity_exponent{};
  std::array<char, 32> symbol{};
  std::vector<PriceLevel> bids{};
  std::vector<PriceLevel> asks{};
  DepthSide capacity_side{DepthSide::None};
  std::size_t capacity_limit{};
};

class JsonParser {
public:
  explicit JsonParser(std::size_t max_levels_per_side =
                          kDefaultMaxDepthLevelsPerSide);
  ~JsonParser();
  JsonParser(const JsonParser &) = delete;
  JsonParser &operator=(const JsonParser &) = delete;
  bool parse_book_ticker(std::string_view json, std::int8_t price_scale,
                         std::int8_t quantity_scale, BookTicker &out,
                         std::string &error);
  bool parse_depth(std::string_view json, std::int8_t price_scale,
                   std::int8_t quantity_scale, DepthUpdate &out,
                   std::string &error);
  DepthParseResult parse_depth_classified(std::string_view json,
                                          std::int8_t price_scale,
                                          std::int8_t quantity_scale,
                                          DepthUpdate &out,
                                          std::string &error);

private:
  std::size_t max_levels_per_side_{};
#ifdef MDS_HAS_SIMDJSON
  struct Impl;
  Impl *impl_{};
#endif
};

class SpotSbeDecoder {
public:
  static constexpr Availability availability = Availability::Available;
  static constexpr std::uint16_t schema_id = 1;
  static constexpr std::uint16_t schema_version = 0;
  static constexpr std::uint16_t best_bid_ask_template_id = 10001;
  static constexpr std::uint16_t depth_snapshot_template_id = 10002;
  static constexpr std::uint16_t depth_diff_template_id = 10003;

  bool decode_book_ticker(std::span<const std::byte> message, BookTicker &out,
                          std::string &error) const noexcept;
  bool decode_depth(std::span<const std::byte> message, DepthUpdate &out,
                    std::string &error) const;
};

enum class BookSyncState : std::uint8_t {
  WaitingSnapshot,
  Bridging,
  Live,
  Invalid
};

enum class SyncAction : std::uint8_t {
  Buffer,
  Drop,
  Apply,
  BecameLive,
  Resnapshot
};

class DepthSynchronizer {
public:
  using ApplyBuffered =
      bool (*)(void *context, const DepthUpdate &update) noexcept;

  explicit DepthSynchronizer(Profile profile,
                             std::size_t max_buffered_updates = 4096)
      : profile_(profile), max_buffered_updates_(max_buffered_updates) {}
  void reset() noexcept;
  void inject_snapshot(std::uint64_t last_update_id) noexcept;
  SyncAction drain_buffered(void *context, ApplyBuffered apply) noexcept;
  SyncAction on_update(const DepthUpdate &update) noexcept;
  [[nodiscard]] BookSyncState state() const noexcept { return state_; }
  [[nodiscard]] std::uint64_t last_update_id() const noexcept {
    return last_update_id_;
  }
  [[nodiscard]] std::size_t buffered_updates() const noexcept {
    return buffered_.size();
  }

private:
  bool bridge(const DepthUpdate &update) noexcept;
  bool continuous(const DepthUpdate &update) const noexcept;

  Profile profile_;
  std::size_t max_buffered_updates_{};
  BookSyncState state_{BookSyncState::WaitingSnapshot};
  std::uint64_t last_update_id_{};
  std::deque<DepthUpdate> buffered_{};
};

class ConnectionHealth {
public:
  explicit ConnectionHealth(std::uint64_t opened_ms) : opened_ms_(opened_ms) {}
  void on_ping(std::uint64_t now_ms) noexcept { last_ping_ms_ = now_ms; }
  void on_pong(std::uint64_t now_ms) noexcept { last_pong_ms_ = now_ms; }
  [[nodiscard]] bool pong_due(std::uint64_t now_ms,
                              std::uint64_t timeout_ms) const noexcept;
  [[nodiscard]] bool rotation_due(std::uint64_t now_ms) const noexcept;

private:
  std::uint64_t opened_ms_{};
  std::uint64_t last_ping_ms_{};
  std::uint64_t last_pong_ms_{};
};

} // namespace mds::exchange::binance
