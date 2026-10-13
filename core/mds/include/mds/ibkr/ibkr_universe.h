#pragma once

#include "mds/ibkr/ibkr_runtime.h"

#include <cstddef>
#include <optional>
#include <vector>

namespace mds::ibkr {

struct SubscriptionMetrics {
  std::optional<double> average_volume_90d;
  std::optional<double> market_cap_usd;
};

struct SubscriptionSelection {
  std::vector<std::size_t> indexes;
  std::size_t below_threshold{};
  std::size_t missing_metric_rejected{};
  std::size_t non_stock_exempt{};
  std::size_t truncated{};
  std::size_t selected_non_stock{};
  std::size_t selected_stock{};
};

[[nodiscard]] SubscriptionSelection select_subscriptions(
    const std::vector<InstrumentSpec> &candidates,
    const std::vector<SubscriptionMetrics> &metrics,
    const SubscriptionFilterOptions &options);

}  // namespace mds::ibkr
