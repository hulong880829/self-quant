#include "polymm/signal.h"

#include <algorithm>
#include <cmath>
#include <limits>

namespace polymm {
namespace {

constexpr double kSecondsPerYear = 31'536'000.0;

double InverseNormal(double probability) noexcept {
  if (!(probability > 0.0 && probability < 1.0)) return 0.0;
  constexpr double a[] = {-3.969683028665376e+01, 2.209460984245205e+02,
                          -2.759285104469687e+02, 1.383577518672690e+02,
                          -3.066479806614716e+01, 2.506628277459239e+00};
  constexpr double b[] = {-5.447609879822406e+01, 1.615858368580409e+02,
                          -1.556989798598866e+02, 6.680131188771972e+01,
                          -1.328068155288572e+01};
  constexpr double c[] = {-7.784894002430293e-03, -3.223964580411365e-01,
                          -2.400758277161838e+00, -2.549732539343734e+00,
                          4.374664141464968e+00, 2.938163982698783e+00};
  constexpr double d[] = {7.784695709041462e-03, 3.224671290700398e-01,
                          2.445134137142996e+00, 3.754408661907416e+00};
  constexpr double low = 0.02425;
  constexpr double high = 1.0 - low;
  if (probability < low) {
    const double q = std::sqrt(-2.0 * std::log(probability));
    return (((((c[0] * q + c[1]) * q + c[2]) * q + c[3]) * q + c[4]) *
                q +
            c[5]) /
           ((((d[0] * q + d[1]) * q + d[2]) * q + d[3]) * q + 1.0);
  }
  if (probability > high) {
    const double q = std::sqrt(-2.0 * std::log(1.0 - probability));
    return -(((((c[0] * q + c[1]) * q + c[2]) * q + c[3]) * q + c[4]) *
                 q +
             c[5]) /
           ((((d[0] * q + d[1]) * q + d[2]) * q + d[3]) * q + 1.0);
  }
  const double q = probability - 0.5;
  const double r = q * q;
  return (((((a[0] * r + a[1]) * r + a[2]) * r + a[3]) * r + a[4]) *
              r +
          a[5]) *
         q /
         (((((b[0] * r + b[1]) * r + b[2]) * r + b[3]) * r + b[4]) *
              r +
          1.0);
}

}  // namespace

SignalEngine::SignalEngine(const Parameters& parameters, std::size_t capacity)
    : parameters_(parameters), samples_(std::max<std::size_t>(capacity, 3)) {}

void SignalEngine::reset() noexcept {
  begin_ = 0;
  size_ = 0;
}

const SignalEngine::Sample& SignalEngine::at(std::size_t logical) const noexcept {
  return samples_[(begin_ + logical) % samples_.size()];
}

bool SignalEngine::update(const FairPriceEvent& event) noexcept {
  if (!std::isfinite(event.price_raw) || event.price_raw <= 0.0 ||
      event.wall_ns == 0) {
    return false;
  }
  if (size_ != 0 && event.wall_ns <= at(size_ - 1).time_ns) return false;
  const Sample sample{event.price_raw, event.wall_ns};
  if (size_ == samples_.size()) {
    samples_[begin_] = sample;
    begin_ = (begin_ + 1) % samples_.size();
  } else {
    samples_[(begin_ + size_) % samples_.size()] = sample;
    ++size_;
  }
  const std::uint64_t cutoff =
      event.wall_ns -
      std::min<std::uint64_t>(
          event.wall_ns,
          static_cast<std::uint64_t>(parameters_.vol_lookback_ms) *
              1'000'000ULL * 2ULL);
  while (size_ > 2 && at(1).time_ns < cutoff) {
    begin_ = (begin_ + 1) % samples_.size();
    --size_;
  }
  return true;
}

const SignalEngine::Sample* SignalEngine::sample_from_ago(
    std::uint64_t duration_ns) const noexcept {
  if (size_ < 2) return nullptr;
  const auto& latest = at(size_ - 1);
  const std::uint64_t target =
      latest.time_ns > duration_ns ? latest.time_ns - duration_ns : 0;
  for (std::size_t index = size_ - 1; index != 0; --index) {
    if (at(index - 1).time_ns <= target) return &at(index - 1);
  }
  return nullptr;
}

SignalEvaluation SignalEngine::evaluate(
    double market_mid, double spread, double tick_size,
    std::uint64_t remaining_ns) const noexcept {
  SignalEvaluation result;
  if (size_ < 3 || !(market_mid > 0.0 && market_mid < 1.0) ||
      !(tick_size > 0.0) || remaining_ns == 0) {
    return result;
  }
  const Sample* volatility_start = sample_from_ago(
      static_cast<std::uint64_t>(parameters_.vol_lookback_ms) * 1'000'000ULL);
  const Sample* signal_start = sample_from_ago(
      static_cast<std::uint64_t>(parameters_.signal_horizon_ms) * 1'000'000ULL);
  if (volatility_start == nullptr || signal_start == nullptr) return result;

  double sum_squares = 0.0;
  std::size_t returns = 0;
  for (std::size_t index = 1; index < size_; ++index) {
    const Sample& previous = at(index - 1);
    const Sample& current = at(index);
    if (current.time_ns < volatility_start->time_ns) continue;
    const double value = std::log(current.price / previous.price);
    if (std::isfinite(value)) {
      sum_squares += value * value;
      ++returns;
    }
  }
  const Sample& latest = at(size_ - 1);
  const double elapsed_seconds =
      static_cast<double>(latest.time_ns - volatility_start->time_ns) / 1e9;
  if (returns == 0 || elapsed_seconds <= 0.0) return result;
  result.annualized_vol =
      std::sqrt(sum_squares / elapsed_seconds * kSecondsPerYear);
  result.volatility_ready =
      std::isfinite(result.annualized_vol) &&
      result.annualized_vol >= parameters_.vol_threshold;
  result.price_in_range =
      market_mid >= parameters_.min_market_price &&
      market_mid <= parameters_.max_market_price;

  result.fair_move = latest.price - signal_start->price;
  const double remaining_seconds = static_cast<double>(remaining_ns) / 1e9;
  const double remaining_sigma =
      latest.price * result.annualized_vol *
      std::sqrt(remaining_seconds / kSecondsPerYear);
  if (!(remaining_sigma > std::numeric_limits<double>::epsilon())) {
    return result;
  }
  const double d = InverseNormal(market_mid);
  constexpr double inv_sqrt_2pi = 0.3989422804014327;
  const double density = inv_sqrt_2pi * std::exp(-0.5 * d * d);
  result.option_edge = result.fair_move * density / remaining_sigma;
  result.edge_ticks = std::abs(result.option_edge) / tick_size;
  const double required_ticks =
      std::max(0.0, spread / tick_size) + parameters_.min_edge_ticks;
  if (result.volatility_ready && result.price_in_range &&
      result.edge_ticks >= required_ticks) {
    result.direction = result.fair_move > 0.0
                           ? SignalEvaluation::Direction::Up
                           : result.fair_move < 0.0
                                 ? SignalEvaluation::Direction::Down
                                 : SignalEvaluation::Direction::None;
  }
  return result;
}

namespace {

double RequiredTicks(const Parameters& parameters, double market_price,
                     double spread, double tick_size) noexcept {
  const double fee_ticks =
      tick_size > 0.0
          ? (market_price * parameters.taker_fee_bps / 10'000.0) / tick_size
          : 0.0;
  return std::max(0.0, spread / tick_size) + parameters.min_edge_ticks +
         parameters.slippage_ticks + std::max(0.0, fee_ticks);
}

}  // namespace

SignalEvaluation SignalEngine::evaluate_executable(
    double up_bid, double up_ask, double down_bid, double down_ask,
    double tick_size, std::uint64_t remaining_ns) const noexcept {
  const double up_mid = (up_bid + up_ask) * 0.5;
  auto result = evaluate(up_mid, 0.0, tick_size, remaining_ns);
  if (!result.volatility_ready || result.fair_move == 0.0) {
    result.direction = SignalEvaluation::Direction::None;
    return result;
  }
  const bool buy_up = result.fair_move > 0.0;
  const double bid = buy_up ? up_bid : down_bid;
  const double ask = buy_up ? up_ask : down_ask;
  if (!(ask > 0.0 && ask < 1.0) || !(bid > 0.0 && bid < ask)) {
    result.direction = SignalEvaluation::Direction::None;
    return result;
  }
  result.price_in_range = ask >= parameters_.min_market_price &&
                          ask <= parameters_.max_market_price;
  const double remaining_seconds = static_cast<double>(remaining_ns) / 1e9;
  const Sample& latest = at(size_ - 1);
  const double remaining_sigma =
      latest.price * result.annualized_vol *
      std::sqrt(remaining_seconds / kSecondsPerYear);
  if (!(remaining_sigma > std::numeric_limits<double>::epsilon())) {
    result.direction = SignalEvaluation::Direction::None;
    return result;
  }
  const double d = InverseNormal(ask);
  constexpr double inv_sqrt_2pi = 0.3989422804014327;
  const double density = inv_sqrt_2pi * std::exp(-0.5 * d * d);
  result.option_edge = result.fair_move * density / remaining_sigma;
  result.edge_ticks = std::abs(result.option_edge) / tick_size;
  const double required =
      RequiredTicks(parameters_, ask, ask - bid, tick_size);
  if (result.price_in_range && result.edge_ticks >= required) {
    result.direction = buy_up ? SignalEvaluation::Direction::Up
                              : SignalEvaluation::Direction::Down;
  } else {
    result.direction = SignalEvaluation::Direction::None;
  }
  return result;
}

bool SignalEngine::ready() const noexcept { return size_ >= 3; }

double SignalEngine::latest_price() const noexcept {
  return size_ == 0 ? 0.0 : at(size_ - 1).price;
}

}  // namespace polymm
