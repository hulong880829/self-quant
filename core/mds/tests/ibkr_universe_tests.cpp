#include "mds/ibkr/ibkr_universe.h"

#include <optional>
#include <stdexcept>
#include <string>
#include <string_view>
#include <vector>

namespace {

void check(bool condition, std::string_view message) {
  if (!condition) {
    throw std::runtime_error(std::string(message));
  }
}

mds::ibkr::InstrumentSpec candidate(std::int64_t con_id,
                                    std::string sec_type = "STK") {
  mds::ibkr::InstrumentSpec result;
  result.con_id = con_id;
  result.sec_type = std::move(sec_type);
  result.canonical_symbol = "S" + std::to_string(con_id);
  result.request_symbol = result.canonical_symbol;
  result.primary_exchange = "NASDAQ";
  return result;
}

mds::ibkr::SubscriptionMetrics metric(std::optional<double> volume,
                                      std::optional<double> market_cap) {
  return {.average_volume_90d = volume, .market_cap_usd = market_cap};
}

void test_missing_configured_metrics_are_rejected() {
  const std::vector candidates{candidate(30), candidate(10), candidate(20)};
  const std::vector<mds::ibkr::SubscriptionMetrics> metrics{
      metric(3000.0, std::nullopt), metric(std::nullopt, 5.0e9),
      metric(2000.0, 3.0e9)};
  mds::ibkr::SubscriptionFilterOptions options;
  options.min_average_volume_90d = 2500.0;
  options.min_market_cap_usd = 4.0e9;
  options.max_subscriptions = 10;

  const auto selected =
      mds::ibkr::select_subscriptions(candidates, metrics, options);
  check(selected.below_threshold == 1 && selected.missing_metric_rejected == 2 &&
            selected.indexes.empty(),
        "missing or below-threshold stock metrics were not rejected");
}

void test_both_known_thresholds_use_and_semantics() {
  const std::vector candidates{candidate(3), candidate(2), candidate(1)};
  const std::vector<mds::ibkr::SubscriptionMetrics> metrics{
      metric(1000.0, 10.0e9), metric(3000.0, 1.0e9), metric(3000.0, 10.0e9)};
  mds::ibkr::SubscriptionFilterOptions options;
  options.min_average_volume_90d = 2000.0;
  options.min_market_cap_usd = 5.0e9;
  options.max_subscriptions = 10;

  const auto selected =
      mds::ibkr::select_subscriptions(candidates, metrics, options);
  check(selected.below_threshold == 2 && selected.indexes.size() == 1 &&
            selected.indexes[0] == 2 && selected.selected_stock == 1,
        "configured thresholds did not require both known values to pass");
}

void test_non_stock_is_prioritized_and_can_exhaust_cap() {
  const std::vector candidates{candidate(30, "CASH"), candidate(9),
                               candidate(10, "CMDTY"), candidate(2)};
  const std::vector<mds::ibkr::SubscriptionMetrics> metrics{
      metric(std::nullopt, std::nullopt), metric(100.0, std::nullopt),
      metric(std::nullopt, std::nullopt), metric(500.0, std::nullopt)};
  mds::ibkr::SubscriptionFilterOptions options;
  options.min_average_volume_90d = 1000.0;
  options.max_subscriptions = 2;

  const auto selected =
      mds::ibkr::select_subscriptions(candidates, metrics, options);
  check(selected.non_stock_exempt == 2 && selected.selected_non_stock == 2 &&
            selected.selected_stock == 0 && selected.truncated == 0 &&
            selected.indexes.size() == 2 && selected.indexes[0] == 2 &&
            selected.indexes[1] == 0,
        "non-stock contracts were not ordered by con_id ahead of stocks");
}

void test_stocks_use_remaining_cap_by_volume() {
  const std::vector candidates{candidate(8), candidate(40, "CASH"),
                               candidate(3), candidate(7),
                               candidate(20, "CMDTY")};
  const std::vector<mds::ibkr::SubscriptionMetrics> metrics{
      metric(100.0, 1.0), metric(std::nullopt, std::nullopt),
      metric(300.0, 1.0), metric(300.0, 1.0),
      metric(std::nullopt, std::nullopt)};
  mds::ibkr::SubscriptionFilterOptions options;
  options.max_subscriptions = 4;

  const auto selected =
      mds::ibkr::select_subscriptions(candidates, metrics, options);
  check(selected.selected_non_stock == 2 && selected.selected_stock == 2 &&
            selected.truncated == 1 && selected.indexes.size() == 4 &&
            selected.indexes[0] == 4 && selected.indexes[1] == 1 &&
            selected.indexes[2] == 2 && selected.indexes[3] == 3,
        "remaining stock capacity was not ranked by volume and con_id");
}

void test_cap_truncates_without_thresholds() {
  std::vector<mds::ibkr::InstrumentSpec> candidates;
  std::vector<mds::ibkr::SubscriptionMetrics> metrics;
  candidates.reserve(120);
  metrics.reserve(120);
  for (int index = 0; index < 120; ++index) {
    candidates.push_back(candidate(index + 1));
    metrics.push_back(metric(static_cast<double>(index + 1), std::nullopt));
  }
  mds::ibkr::SubscriptionFilterOptions options;
  options.max_subscriptions = 90;

  const auto selected =
      mds::ibkr::select_subscriptions(candidates, metrics, options);
  check(selected.below_threshold == 0 && selected.missing_metric_rejected == 0 &&
            selected.indexes.size() == 90 && selected.truncated == 30 &&
            selected.indexes.front() == 119 && selected.indexes.back() == 30,
        "stocks above the subscription cap were not truncated by volume");

  options.max_subscriptions = 90;
  candidates.resize(70);
  metrics.resize(70);
  const auto smaller =
      mds::ibkr::select_subscriptions(candidates, metrics, options);
  check(smaller.indexes.size() == 70 && smaller.truncated == 0,
        "a candidate set below the cap was truncated");
}

}  // namespace

int main() {
  test_missing_configured_metrics_are_rejected();
  test_both_known_thresholds_use_and_semantics();
  test_non_stock_is_prioritized_and_can_exhaust_cap();
  test_stocks_use_remaining_cap_by_volume();
  test_cap_truncates_without_thresholds();
  return 0;
}
