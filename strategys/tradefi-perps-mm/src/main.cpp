#include <algorithm>
#include <iostream>
#include <string_view>
#include <utility>

#include "strategyframe/strategyframe.h"
#include "tradefimm/strategy.h"

int main(int argc, char** argv) {
  if (argc != 2 && argc != 3) {
    std::cerr << "usage: tradefi-perps-mm CONFIG.yaml "
                 "[--accept-live-trading|--validate-only]\n";
    return 2;
  }
  const bool live_accepted =
      argc == 3 && std::string_view(argv[2]) == "--accept-live-trading";
  const bool validate_only =
      argc == 3 && std::string_view(argv[2]) == "--validate-only";
  if (argc == 3 && !live_accepted && !validate_only) {
    std::cerr << "unknown argument\n";
    return 2;
  }
  auto loaded = strategyframe::load_config(argv[1]);
  if (!loaded) {
    std::cerr << "invalid strategy configuration\n";
    return 1;
  }
  const bool live_enabled =
      std::any_of(loaded.value.venues.begin(), loaded.value.venues.end(),
                  [](const auto& venue) { return venue.enabled; });
  if (validate_only) {
    if (!loaded.value.strategy.contains("maker.venue") ||
        !loaded.value.strategy.contains("hedge.venue") ||
        !loaded.value.strategy.contains("basis_offset") ||
        !loaded.value.strategy.contains("basis_buffer") ||
        !loaded.value.strategy.contains("strategy_instance") ||
        !loaded.value.strategy.contains("ibkr_short_policy") ||
        loaded.value.strategy.contains("allow_short")) {
      std::cerr << "invalid strategy configuration\n";
      return 1;
    }
    return 0;
  }
  const bool shadow = !live_accepted;
  if (shadow)
    std::cerr << "tradefi-perps-mm shadow: order commands stay in strategy\n";
  else if (!live_enabled) {
    std::cerr << "live acceptance requires an enabled OMS venue\n";
    return 2;
  }
  int exit_code = 0;
  strategyframe::StrategyRunner<tradefimm::TradefiPerpsMm> runner(
      std::move(loaded.value),
      tradefimm::TradefiPerpsMm{&exit_code, shadow});
  const auto result = runner.run();
  if (exit_code != 0) return exit_code;
  if (result != strategyframe::Error::Ok) return 1;
  return 0;
}
