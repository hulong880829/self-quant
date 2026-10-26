#pragma once

#include <cmath>
#include <cstdint>

#include "polymm/pnl_ledger.h"
#include "polymm/types.h"

namespace polymm {

[[nodiscard]] inline bool allows_open(
    double current_side, double opposite_side, double order_shares,
    double maximum_side, double tolerance) noexcept {
  const double projected = current_side + order_shares;
  if (projected <= maximum_side) return true;
  return std::abs(projected - opposite_side) + tolerance <
         std::abs(current_side - opposite_side);
}

enum class FeeRounding : std::uint8_t { Nearest = 0, Up = 1 };

[[nodiscard]] inline double polymarket_taker_fee(
    double price, double quantity, double fee_rate,
    FeeRounding rounding = FeeRounding::Nearest) noexcept {
  if (!(price > 0.0) || !(price < 1.0) || !(quantity > 0.0) ||
      !(fee_rate > 0.0) || fee_rate > 1.0) {
    return 0.0;
  }
  const double raw = quantity * fee_rate * price * (1.0 - price);
  constexpr double kScale = 100'000.0;
  const double scaled = raw * kScale;
  const double units = rounding == FeeRounding::Up
                           ? std::ceil(scaled - 1e-9)
                           : std::round(scaled);
  return units / kScale;
}

[[nodiscard]] inline double polymarket_taker_fee_per_share(
    double price, double quantity, double fee_rate,
    FeeRounding rounding = FeeRounding::Nearest) noexcept {
  const double fee = polymarket_taker_fee(price, quantity, fee_rate, rounding);
  if (!(quantity > 0.0)) return 0.0;
  return fee / quantity;
}

[[nodiscard]] inline bool order_terms_ok(double price, double quantity,
                                         double tick_size,
                                         double fee_rate) noexcept {
  return price > 0.0 && price < 1.0 && quantity > 0.0 && tick_size > 0.0 &&
         fee_rate >= 0.0 && fee_rate <= 1.0;
}

enum class CloseLiquidity : std::uint8_t { PostOnly = 0, Ioc = 1 };

[[nodiscard]] inline CloseLiquidity select_close_liquidity(
    double bid, double quantity, double maker_target,
    double fee_rate) noexcept {
  if (!order_terms_ok(bid, quantity, 1.0, fee_rate))
    return CloseLiquidity::PostOnly;
  const double fee_per_share = polymarket_taker_fee_per_share(
      bid, quantity, fee_rate, FeeRounding::Up);
  if (bid - fee_per_share >= maker_target) return CloseLiquidity::Ioc;
  return CloseLiquidity::PostOnly;
}

[[nodiscard]] inline double slippage_cost(
    double quantity, double tick_size, double slippage_ticks) noexcept {
  if (!(quantity > 0.0) || !(tick_size > 0.0) || !(slippage_ticks > 0.0))
    return 0.0;
  return quantity * tick_size * slippage_ticks;
}

[[nodiscard]] inline bool stop_loss_triggered(
    const PnlView& position, double bid, double tick_size,
    double stop_loss_ticks, double fee_rate,
    double slippage_ticks = 0.0) noexcept {
  if (!(position.quantity > 0.0) ||
      !order_terms_ok(bid, position.quantity, tick_size, fee_rate)) {
    return false;
  }
  const double exit_fee = polymarket_taker_fee(
      bid, position.quantity, fee_rate, FeeRounding::Up);
  const double net = bid * position.quantity - exit_fee -
                     slippage_cost(position.quantity, tick_size,
                                   slippage_ticks) -
                     position.average_cost * position.quantity;
  return net <= -stop_loss_ticks * tick_size * position.quantity;
}

[[nodiscard]] inline double close_target_price(
    double average_cost, double tick_size,
    std::int64_t profit_spread_ticks) noexcept {
  return average_cost +
         static_cast<double>(profit_spread_ticks) * tick_size;
}

[[nodiscard]] inline bool near_expiry(
    std::uint64_t remaining, std::uint32_t no_trade_ms) noexcept {
  return remaining <=
         static_cast<std::uint64_t>(no_trade_ms) * 1'000'000ULL;
}

[[nodiscard]] inline bool book_tradable(double bid, double ask) noexcept {
  return bid > 0.0 && ask > 0.0 && bid < ask;
}

[[nodiscard]] inline bool bbo_fresh(std::uint64_t now_ns,
                                    std::uint64_t last_bbo_ns,
                                    std::uint32_t max_age_ms) noexcept {
  if (last_bbo_ns == 0 || now_ns < last_bbo_ns) return false;
  return now_ns - last_bbo_ns <=
         static_cast<std::uint64_t>(max_age_ms) * 1'000'000ULL;
}

[[nodiscard]] inline bool bbo_skew_ok(std::uint64_t left_ns,
                                      std::uint64_t right_ns,
                                      std::uint32_t max_skew_ms) noexcept {
  const std::uint64_t delta = left_ns > right_ns ? left_ns - right_ns
                                                 : right_ns - left_ns;
  return delta <= static_cast<std::uint64_t>(max_skew_ms) * 1'000'000ULL;
}

}  // namespace polymm
