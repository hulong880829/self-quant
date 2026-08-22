#pragma once

#include <string_view>
#include <vector>

#include "oms/api/oms_api.h"

namespace oms::runtime {

// Test/control-plane representation consumed by the normalized FakeTransport
// in OmsApi. It deliberately contains no exchange wire payloads.
class ReplayScript {
 public:
  static api::Result<ReplayScript> Load(std::string_view path);
  [[nodiscard]] const std::vector<api::ReplayStep>& steps() const noexcept {
    return steps_;
  }

 private:
  std::vector<api::ReplayStep> steps_;
};

}  // namespace oms::runtime
