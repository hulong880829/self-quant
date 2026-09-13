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

[[nodiscard]] inline double taker_fee(
    double price, double quantity, double taker_fee_bps) noexcept {
  if (!(price > 0.0) || !(quantity > 0.0) || !(taker_fee_bps > 0.0))
    return 0.0;
  return price * quantity * taker_fee_bps / 10'000.0;
}

[[nodiscard]] inline double slippage_cost(
    double quantity, double tick_size, double slippage_ticks) noexcept {
  if (!(quantity > 0.0) || !(tick_size > 0.0) || !(slippage_ticks > 0.0))
    return 0.0;
  return quantity * tick_size * slippage_ticks;
}

[[nodiscard]] inline bool stop_loss_triggered(
    const PnlView& position, double tick_size, double stop_loss_ticks,
    double taker_fee_bps = 0.0, double slippage_ticks = 0.0) noexcept {
  if (!(position.quantity > 0.0) || !(tick_size > 0.0)) return false;
  const double costs =
      taker_fee(position.average_cost, position.quantity, taker_fee_bps) +
      slippage_cost(position.quantity, tick_size, slippage_ticks);
  return position.unrealized_exit - costs <=
         -stop_loss_ticks * tick_size * position.quantity;
}

[[nodiscard]] inline double close_target_price(
    double average_cost, double tick_size, std::int64_t profit_spread_ticks,
    double taker_fee_bps, double slippage_ticks) noexcept {
  const double fee_per_share =
      average_cost > 0.0 ? average_cost * taker_fee_bps / 10'000.0 : 0.0;
  return average_cost +
         static_cast<double>(profit_spread_ticks) * tick_size +
         slippage_ticks * tick_size + fee_per_share;
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
