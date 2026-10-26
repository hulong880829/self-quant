#include "oms/exchange/ibkr/order_rules.h"

#include <cstdlib>

namespace oms::exchange::ibkr {

QuantityPlan plan_hedge_quantity(std::int64_t target_base_units,
                                 std::int64_t units_per_native,
                                 std::int64_t size_increment,
                                 std::int64_t min_size) noexcept {
  QuantityPlan plan;
  if (units_per_native <= 0 || size_increment <= 0 || min_size <= 0 ||
      target_base_units == 0) {
    plan.residual_base_units = target_base_units;
    return plan;
  }
  const std::int64_t sign = target_base_units < 0 ? -1 : 1;
  const std::int64_t magnitude = std::llabs(target_base_units);
  const std::int64_t raw_native = magnitude / units_per_native;
  const std::int64_t stepped =
      (raw_native / size_increment) * size_increment;
  if (stepped < min_size) {
    plan.residual_base_units = target_base_units;
    return plan;
  }
  plan.native_quantity = sign * stepped;
  plan.residual_base_units = target_base_units - plan.native_quantity *
                                                     units_per_native;
  plan.tradable = true;
  return plan;
}

bool ioc_supported(bool venue_accepts_ioc) noexcept {
  return venue_accepts_ioc;
}

ClientIdFault classify_client_id_error(int code) noexcept {
  // IB error 326 is "Unable to connect as the client id is already in use".
  if (code == 326) return ClientIdFault::Duplicate;
  return ClientIdFault::None;
}

}  // namespace oms::exchange::ibkr
