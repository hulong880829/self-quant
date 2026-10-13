#pragma once

#include <array>
#include <cstdint>
#include <optional>
#include <span>
#include <string>
#include <string_view>

namespace fairprice {

inline constexpr int kMaxVenues = 8;
inline constexpr int kMaxDepth = 50;
inline constexpr std::uint32_t kDegradedBookCrossed = 1U << 0U;
inline constexpr std::uint32_t kDegradedInsufficientDepth = 1U << 1U;
inline constexpr std::uint32_t kDegradedImpactDepth = 1U << 2U;
inline constexpr std::uint32_t kDegradedInactiveVenues = 1U << 3U;
inline constexpr std::uint32_t kDegradedVenueDominant = 1U << 4U;

struct FairPriceConfig {
  bool enabled{true};
  int depth_k{20};
  double lambda_per_bp{0.10};
  double imbalance_alpha{1.0};
  std::uint64_t ewma_tau_ns{500'000'000};
  double impact_notional{0.0};
  double venue_dominance_ratio{0.90};
};

struct ResidualLevel {
  std::int64_t price{};
  std::int64_t quantity{};
  std::array<std::int64_t, kMaxVenues> venue_quantity{};
  std::uint32_t venue_mask{};
  std::uint8_t contributor_count{};
};

struct BookView {
  std::uint64_t exchange_ts_ns{};
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
  std::uint8_t member_count{};
  std::uint32_t member_mask{};
  std::uint32_t active_mask{};
  std::span<const ResidualLevel> bids{};
  std::span<const ResidualLevel> asks{};
};

struct SnapshotView {
  bool ready{};
  std::uint64_t ring_epoch{};
  std::uint64_t ring_sequence{};
  std::uint64_t generation{};
  std::uint64_t wall_ns{};
  const BookView *book{};
};

struct ModelState {
  std::uint64_t wall_ns{};
  double price{};
};

struct ComputeStats {
  int actual_bid_depth{};
  int actual_ask_depth{};
  bool target_depth_shortfall{};
  bool recorded{};
};

struct alignas(8) FairPricePod {
  std::uint64_t ring_epoch{};
  std::uint64_t ring_sequence{};
  std::uint64_t generation{};
  std::uint64_t wall_ns{};
  std::uint64_t exchange_ts_ns{};
  std::int64_t price{};
  std::int64_t price_raw{};
  std::int64_t mid{};
  std::int64_t microprice{};
  std::int64_t best_bid{};
  std::int64_t best_ask{};
  std::int64_t effective_bid{};
  std::int64_t effective_ask{};
  std::int64_t impact_bid{};
  std::int64_t impact_ask{};
  std::int64_t crossed_quantity{};
  double imbalance{};
  double spread_bps{};
  double cross_bps{};
  double weighted_bid_depth{};
  double weighted_ask_depth{};
  double depth_bid_notional{};
  double depth_ask_notional{};
  std::uint32_t active_venue_mask{};
  std::uint32_t degraded_mask{};
  std::uint8_t price_scale{};
  std::uint8_t quantity_scale{};
  std::uint8_t output_scale{};
  std::uint8_t ready{};
  std::uint8_t crossed{};
  std::uint8_t degraded{};
  std::uint8_t has_impact_bid{};
  std::uint8_t has_impact_ask{};
  std::uint8_t has_crossed_quantity{};
  std::uint8_t has_cross_bps{};
  std::array<char, 32> model_id{};
  std::array<char, 32> reset_reason{};
};

class Engine {
 public:
  static std::optional<Engine> create(const FairPriceConfig &config,
                                      std::string &error);
  [[nodiscard]] const std::string &model_id() const noexcept {
    return model_id_;
  }
  [[nodiscard]] const FairPriceConfig &config() const noexcept {
    return config_;
  }

  FairPricePod compute(const SnapshotView &snapshot, const ModelState *previous,
                       ModelState *next, ComputeStats *stats = nullptr) const;

 private:
  FairPriceConfig config_{};
  std::string model_id_{};
};

void copy_cstr(std::array<char, 32> &dest, std::string_view value) noexcept;
std::string_view cstr_view(const std::array<char, 32> &value) noexcept;
void append_degraded_reasons(std::uint32_t mask, std::string &out);

}  // namespace fairprice
