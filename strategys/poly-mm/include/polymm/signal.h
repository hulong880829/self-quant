#pragma once

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <vector>

#include "polymm/types.h"

namespace polymm {

struct SignalEvaluation {
  enum class Direction : std::uint8_t { None = 0, Up = 1, Down = 2 };

  Direction direction{Direction::None};
  double annualized_vol{};
  double fair_move{};
  double option_edge{};
  double edge_ticks{};
  bool price_in_range{};
  bool volatility_ready{};
};

class SignalEngine {
 public:
  explicit SignalEngine(const Parameters& parameters,
                        std::size_t capacity = 8192);

  void reset() noexcept;
  bool update(const FairPriceEvent& event) noexcept;
  [[nodiscard]] SignalEvaluation evaluate(
      double market_mid, double spread, double tick_size,
      std::uint64_t remaining_ns) const noexcept;
  [[nodiscard]] SignalEvaluation evaluate_executable(
      double up_bid, double up_ask, double down_bid, double down_ask,
      double tick_size, std::uint64_t remaining_ns) const noexcept;
  [[nodiscard]] bool ready() const noexcept;
  [[nodiscard]] double latest_price() const noexcept;

 private:
  struct Sample {
    double price{};
    std::uint64_t time_ns{};
  };

  [[nodiscard]] const Sample* sample_from_ago(
      std::uint64_t duration_ns) const noexcept;
  [[nodiscard]] const Sample& at(std::size_t logical) const noexcept;

  Parameters parameters_{};
  std::vector<Sample> samples_;
  std::size_t begin_{};
  std::size_t size_{};
};

[[nodiscard]] inline std::size_t signal_sample_capacity(
    const Parameters& parameters) noexcept {
  if (parameters.fairprice_timer_us == 0) return 16;
  const std::uint64_t lookback_us =
      static_cast<std::uint64_t>(parameters.vol_lookback_ms) * 1'000ULL;
  const std::size_t samples = static_cast<std::size_t>(
      lookback_us / parameters.fairprice_timer_us + 8U);
  return std::max<std::size_t>(samples * 2U, 16U);
}

}  // namespace polymm
