#include "mds/ibkr/ibkr_universe.h"

#include <algorithm>
#include <stdexcept>

namespace mds::ibkr {
namespace {

bool below(const std::optional<double> &actual,
           const std::optional<double> &minimum) noexcept {
  return actual.has_value() && minimum.has_value() && *actual < *minimum;
}

bool is_stock(const InstrumentSpec &spec) noexcept {
  return spec.sec_type == "STK";
}

}  // namespace

SubscriptionSelection select_subscriptions(
    const std::vector<InstrumentSpec> &candidates,
    const std::vector<SubscriptionMetrics> &metrics,
    const SubscriptionFilterOptions &options) {
  if (candidates.size() != metrics.size()) {
    throw std::invalid_argument(
        "IBKR candidate and metric counts must match");
  }

  SubscriptionSelection result;
  std::vector<std::size_t> non_stocks;
  std::vector<std::size_t> stocks;
  non_stocks.reserve(candidates.size());
  stocks.reserve(candidates.size());
  for (std::size_t index = 0; index < candidates.size(); ++index) {
    if (!is_stock(candidates[index])) {
      non_stocks.push_back(index);
      ++result.non_stock_exempt;
      continue;
    }
    const auto &value = metrics[index];
    if (below(value.average_volume_90d, options.min_average_volume_90d) ||
        below(value.market_cap_usd, options.min_market_cap_usd)) {
      ++result.below_threshold;
      continue;
    }
    if ((options.min_average_volume_90d.has_value() &&
         !value.average_volume_90d.has_value()) ||
        (options.min_market_cap_usd.has_value() &&
         !value.market_cap_usd.has_value())) {
      ++result.missing_metric_rejected;
      continue;
    }
    stocks.push_back(index);
  }

  std::stable_sort(non_stocks.begin(), non_stocks.end(),
                   [&](std::size_t left, std::size_t right) {
                     return candidates[left].con_id < candidates[right].con_id;
                   });
  std::stable_sort(
      stocks.begin(), stocks.end(),
      [&](std::size_t left, std::size_t right) {
        const auto &left_value = metrics[left].average_volume_90d;
        const auto &right_value = metrics[right].average_volume_90d;
        if (left_value.has_value() != right_value.has_value()) {
          return left_value.has_value();
        }
        if (left_value.has_value() && *left_value != *right_value) {
          return *left_value > *right_value;
        }
        return candidates[left].con_id < candidates[right].con_id;
      });

  result.indexes.reserve(non_stocks.size() + stocks.size());
  result.indexes.insert(result.indexes.end(), non_stocks.begin(),
                         non_stocks.end());
  result.indexes.insert(result.indexes.end(), stocks.begin(), stocks.end());
  if (result.indexes.size() > options.max_subscriptions) {
    result.truncated = result.indexes.size() - options.max_subscriptions;
    result.indexes.resize(options.max_subscriptions);
  }
  result.selected_non_stock =
      std::min(non_stocks.size(), result.indexes.size());
  result.selected_stock = result.indexes.size() - result.selected_non_stock;
  return result;
}

}  // namespace mds::ibkr
