#pragma once

#include <cstdint>

namespace oms::runtime {

struct NotifierResult {
  std::uint64_t value{};
  int error{};

  [[nodiscard]] constexpr explicit operator bool() const noexcept {
    return error == 0;
  }
};

}  // namespace oms::runtime
