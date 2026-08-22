#pragma once

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

}  // namespace polymm
