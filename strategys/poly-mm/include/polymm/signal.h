#pragma once

#include <algorithm>
#include <array>
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
  double spread_ticks{};
  double entry_fee_ticks{};
  double slippage_ticks{};
  double required_ticks{};
  bool price_in_range{};
  bool volatility_ready{};
};

enum class FairUpdate : std::uint8_t {
  Accepted,
  Duplicate,
  OutOfOrder,
  Invalid,
};

inline constexpr std::uint64_t kFairpriceFreshNs = 2'000'000'000ULL;

[[nodiscard]] inline bool fairprice_gates_fresh(
    std::uint64_t now_ns, std::uint64_t received_ns,
    std::uint64_t accepted_wall_ns) noexcept {
  return received_ns != 0 && accepted_wall_ns != 0 &&
         now_ns < received_ns + kFairpriceFreshNs &&
         now_ns < accepted_wall_ns + kFairpriceFreshNs;
}

class SignalEngine {
 public:
  explicit SignalEngine(const Parameters& parameters,
                        std::size_t capacity = 8192);

  void reset() noexcept;
  FairUpdate update(const FairPriceEvent& event) noexcept;
  [[nodiscard]] std::uint64_t latest_wall_ns() const noexcept;
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

inline constexpr std::uint64_t kSignalHorizonNs[4] = {
    100'000'000ULL, 200'000'000ULL, 500'000'000ULL, 1'000'000'000ULL};
inline constexpr std::uint64_t kSignalHorizonSlackNs = 50'000'000ULL;
inline constexpr std::size_t kSignalHorizonSlots = 64;

struct HorizonAggregate {
  std::uint64_t valid{};
  std::uint64_t missing{};
  double mid_move_sum{};
  double mark_to_bid_sum{};
  double taker_exit_net_sum{};
  std::uint64_t mark_positive{};
  std::uint64_t taker_positive{};
};

struct HorizonSummary {
  std::uint64_t dropped{};
  std::uint64_t completed{};
  std::array<HorizonAggregate, 4> horizons{};
};

class SignalHorizonTracker {
 public:
  bool record(std::uint64_t signal_ns, std::uint32_t window_generation,
              strategyframe::InstrumentId instrument_id,
              SignalEvaluation::Direction direction, double initial_mid,
              double initial_bid, double entry_ask, double edge_ticks,
              double required_ticks, double entry_fee_per_share,
              double tick_size, double fee_rate, double quantity) noexcept;
  void sample(std::uint64_t now_ns,
              strategyframe::InstrumentId instrument_id,
              std::uint32_t window_generation, double bid, double ask,
              std::uint64_t bbo_ns) noexcept;
  void discard_generation(std::uint32_t window_generation) noexcept;
  [[nodiscard]] HorizonSummary summary() const noexcept;
  [[nodiscard]] bool take_summary_request() noexcept;
  [[nodiscard]] std::uint64_t inflight() const noexcept;

 private:
  struct Observation {
    bool used{};
    std::uint64_t signal_ns{};
    std::uint32_t window_generation{};
    strategyframe::InstrumentId instrument_id{};
    SignalEvaluation::Direction direction{SignalEvaluation::Direction::None};
    double initial_mid{};
    double initial_bid{};
    double entry_ask{};
    double edge_ticks{};
    double required_ticks{};
    double entry_fee_per_share{};
    double tick_size{};
    double fee_rate{};
    double quantity{};
    std::uint8_t resolved{};
  };

  void resolve(Observation& observation, int horizon, bool valid, double bid,
               double ask) noexcept;
  void finish_if_complete(Observation& observation) noexcept;

  std::array<Observation, kSignalHorizonSlots> slots_{};
  std::array<HorizonAggregate, 4> horizons_{};
  std::uint64_t inflight_{};
  std::uint64_t dropped_{};
  std::uint64_t completed_{};
  bool summary_due_{};
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
