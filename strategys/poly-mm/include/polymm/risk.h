#pragma once

#include <cmath>
#include <cstdint>

#include "polymm/pnl_ledger.h"

namespace polymm {

[[nodiscard]] inline bool allows_open(
    double current_side, double opposite_side, double order_shares,
    double maximum_side, double tolerance) noexcept {
  const double projected = current_side + order_shares;
  if (projected <= maximum_side) return true;
  return std::abs(projected - opposite_side) + tolerance <
         std::abs(current_side - opposite_side);
}

[[nodiscard]] inline bool stop_loss_triggered(
    const PnlView& position, double tick_size,
    double stop_loss_ticks) noexcept {
  return position.quantity > 0.0 && tick_size > 0.0 &&
         position.unrealized_exit <=
             -stop_loss_ticks * tick_size * position.quantity;
}

[[nodiscard]] inline bool near_expiry(
    std::uint64_t remaining, std::uint32_t no_trade_ms) noexcept {
  return remaining <=
         static_cast<std::uint64_t>(no_trade_ms) * 1'000'000ULL;
}

}  // namespace polymm
