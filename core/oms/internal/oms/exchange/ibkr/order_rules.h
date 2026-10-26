#pragma once

#include <cstdint>

namespace oms::exchange::ibkr {

enum class ClientIdFault : std::uint8_t { None = 0, Duplicate = 1 };

struct QuantityPlan {
  std::int64_t native_quantity{};
  std::int64_t residual_base_units{};
  bool tradable{};
};

// Round toward the smaller absolute hedge. native units are IB totalQuantity.
[[nodiscard]] QuantityPlan plan_hedge_quantity(
    std::int64_t target_base_units, std::int64_t units_per_native,
    std::int64_t size_increment, std::int64_t min_size) noexcept;

[[nodiscard]] bool ioc_supported(bool venue_accepts_ioc) noexcept;

[[nodiscard]] ClientIdFault classify_client_id_error(int code) noexcept;

}  // namespace oms::exchange::ibkr
