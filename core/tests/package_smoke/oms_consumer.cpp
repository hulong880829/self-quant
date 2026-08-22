#include <type_traits>

#include "oms/api/oms_api.h"

int main() {
  static_assert(std::is_trivially_copyable_v<oms::exchange::AdapterIdentity>);
  oms::api::RuntimeConfig config{};
  config.mode = oms::api::ExecutionMode::Inline;
  oms::api::AdapterStatusSnapshot status{};
  return config.mode == oms::api::ExecutionMode::Inline &&
                 status.status == oms::exchange::AdapterStatus::Stopped
             ? 0
             : 1;
}
