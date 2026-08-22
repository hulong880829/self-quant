#include "polymm/market_window.h"

#include <string>

namespace polymm {

std::int64_t current_window_start(std::int64_t unix_seconds) noexcept {
  if (unix_seconds < 0) return 0;
  return unix_seconds - unix_seconds % kWindowSeconds;
}

std::string btc_five_minute_slug(std::int64_t window_start) {
  return "btc-updown-5m-" + std::to_string(window_start);
}

std::uint64_t remaining_ns(std::int64_t window_end,
                           std::uint64_t wall_ns) noexcept {
  if (window_end <= 0) return 0;
  const std::uint64_t end_ns =
      static_cast<std::uint64_t>(window_end) * 1'000'000'000ULL;
  return end_ns > wall_ns ? end_ns - wall_ns : 0;
}

}  // namespace polymm
