#pragma once

#include <cstdint>
#include <string_view>

namespace utils::md {

[[nodiscard]] bool decimal_scale(std::string_view value,
                                 std::uint8_t &scale) noexcept;
[[nodiscard]] bool decimal_to_fixed(std::string_view value,
                                    std::uint8_t scale,
                                    std::int64_t &result) noexcept;

}  // namespace utils::md
