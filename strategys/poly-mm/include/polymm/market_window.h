#pragma once

#include <cstdint>
#include <string>

namespace polymm {

inline constexpr std::int64_t kWindowSeconds = 300;

[[nodiscard]] std::int64_t current_window_start(
    std::int64_t unix_seconds) noexcept;
[[nodiscard]] std::string btc_five_minute_slug(
    std::int64_t window_start);
[[nodiscard]] std::uint64_t remaining_ns(
    std::int64_t window_end, std::uint64_t wall_ns) noexcept;

}  // namespace polymm
