#pragma once

#include "mds/exchange/venue_adapter.h"

#include <algorithm>
#include <array>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <optional>
#include <span>
#include <string>
#include <string_view>
#include <vector>

namespace mds::exchange::polymarket {

template <std::size_t Capacity>
struct FixedText {
  std::array<char, Capacity + 1> bytes{};
  std::size_t size{};

  [[nodiscard]] bool assign(std::string_view value) noexcept {
    if (value.empty() || value.size() > Capacity) {
      return false;
    }
    std::copy(value.begin(), value.end(), bytes.begin());
    bytes[value.size()] = '\0';
    size = value.size();
    return true;
  }

  [[nodiscard]] std::string_view view() const noexcept {
    return {bytes.data(), size};
  }
};

struct MarketWindow {
  std::int64_t start_unix{};
  std::int64_t end_unix{};
};

[[nodiscard]] MarketWindow btc_five_minute_window(
    std::int64_t unix_seconds, std::int32_t offset = 0) noexcept;
[[nodiscard]] bool btc_five_minute_slug(MarketWindow window,
                                        FixedText<64> &slug) noexcept;

enum class Outcome : std::uint8_t { Up, Down };

struct GammaMarket {
  FixedText<64> slug;
  FixedText<66> condition_id;
  FixedText<78> up_token_id;
  FixedText<78> down_token_id;
  MarketWindow window{};
  std::int64_t minimum_order_size{1};
  std::uint8_t signature_type{};
  bool negative_risk{};
  bool active{};
  bool closed{};

  [[nodiscard]] std::string_view token(Outcome outcome) const noexcept {
    return outcome == Outcome::Up ? up_token_id.view() : down_token_id.view();
  }
};

[[nodiscard]] bool parse_gamma_market_response(
    std::string_view json, std::string_view expected_slug,
    GammaMarket &market, std::string &error);

enum class ResolveSlot : std::uint8_t { Current, Next };
enum class ResolveStatus : std::uint8_t {
  Idle,
  RequestReady,
  AwaitingResponse,
  Resolved,
  NotFound,
};

struct GammaRequest {
  ResolveSlot slot{ResolveSlot::Current};
  MarketWindow window{};
  FixedText<64> slug;
  FixedText<96> target;
};

class RollingMarketResolver {
 public:
  void reset(std::int64_t unix_seconds) noexcept;
  void advance(std::int64_t unix_seconds) noexcept;

  [[nodiscard]] std::optional<GammaRequest> next_request() const noexcept;
  bool mark_requested(ResolveSlot slot) noexcept;
  void retry(ResolveSlot slot) noexcept;
  bool apply_result(ResolveSlot slot, std::string_view response,
                    std::string &error);
  void apply_not_found(ResolveSlot slot) noexcept;

  [[nodiscard]] ResolveStatus status(ResolveSlot slot) const noexcept;
  [[nodiscard]] const GammaMarket *market(ResolveSlot slot) const noexcept;

 private:
  struct Entry {
    GammaRequest request{};
    GammaMarket market{};
    ResolveStatus status{ResolveStatus::Idle};
    bool has_market{};
  };

  void set_entry(Entry &entry, ResolveSlot slot,
                 MarketWindow window) noexcept;
  [[nodiscard]] Entry &entry(ResolveSlot slot) noexcept;
  [[nodiscard]] const Entry &entry(ResolveSlot slot) const noexcept;

  Entry current_{};
  Entry next_{};
};

enum class MarketLifecycle : std::uint8_t {
  Unknown,
  Active,
  Closed,
  ResolvedUp,
  ResolvedDown,
};

class PolymarketAdapter final : public VenueAdapter {
 public:
  explicit PolymarketAdapter(std::size_t max_levels_per_side = 1000);
  ~PolymarketAdapter() override;
  PolymarketAdapter(PolymarketAdapter &&) noexcept;
  PolymarketAdapter &operator=(PolymarketAdapter &&) noexcept;
  PolymarketAdapter(const PolymarketAdapter &) = delete;
  PolymarketAdapter &operator=(const PolymarketAdapter &) = delete;

  [[nodiscard]] utils::md::Venue venue() const noexcept override;
  [[nodiscard]] utils::md::ProductType product() const noexcept override;
  [[nodiscard]] HeartbeatSpec heartbeat() const override;
  void reset_connection_state() noexcept override;
  [[nodiscard]] std::size_t expected_subscription_acks(
      std::string_view batch) const noexcept override;

  bool build_subscription_batches(
      std::span<const StreamRequest> requests,
      std::vector<std::string> &batches, std::string &error) const override;
  bool build_unsubscription_batches(
      std::span<const StreamRequest> requests,
      std::vector<std::string> &batches, std::string &error) const override;
  bool parse_ws(std::string_view json, NormalizedEvent &event,
                std::string &error) override;
  [[nodiscard]] bool has_pending_events() const noexcept override;

  [[nodiscard]] HttpRequestSpec metadata_request() const override;
  [[nodiscard]] HttpRequestSpec metadata_request(
      std::span<const StreamRequest> requests) const override;
  bool build_metadata_request_batches(
      std::span<const StreamRequest> requests,
      std::vector<MetadataRequestBatch> &batches,
      std::string &error) const override;
  bool parse_metadata(std::string_view json,
                      std::span<const StreamRequest> requests,
                      std::vector<InstrumentMetadata> &metadata,
                      std::string &error) override;

  void prepare_resolution(std::int64_t unix_seconds) noexcept;
  [[nodiscard]] std::optional<GammaRequest> next_gamma_request() const noexcept;
  bool mark_gamma_requested(ResolveSlot slot) noexcept;
  void retry_gamma(ResolveSlot slot) noexcept;
  bool apply_gamma_response(ResolveSlot slot, std::string_view response,
                            std::string &error);
  [[nodiscard]] const GammaMarket *resolved_market(
      ResolveSlot slot) const noexcept;
  bool build_resolved_metadata(
      ResolveSlot slot, std::span<const StreamRequest> requests,
      std::vector<InstrumentMetadata> &metadata, std::string &error);

  bool build_dynamic_operation(std::span<const std::string_view> asset_ids,
                               bool subscribe, std::string &payload,
                               std::string &error) const;
  [[nodiscard]] MarketLifecycle lifecycle(
      std::string_view asset_id) const noexcept;
  [[nodiscard]] std::uint64_t stale_messages() const noexcept;

 private:
  class Impl;
  std::unique_ptr<Impl> impl_;
};

[[nodiscard]] std::unique_ptr<PolymarketAdapter>
make_polymarket_adapter(std::size_t max_levels_per_side = 1000);

}  // namespace mds::exchange::polymarket
