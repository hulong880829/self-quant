#pragma once

#include <cstdint>

namespace strategyframe {

[[nodiscard]] inline bool expired_with_successor(
    std::uint64_t expiry_unix_ns, std::uint64_t now_wall_ns,
    bool has_later_successor) noexcept {
  return expiry_unix_ns != 0 && now_wall_ns >= expiry_unix_ns &&
         has_later_successor;
}

}  // namespace strategyframe
