#include <algorithm>
#include <iostream>
#include <string_view>
#include <utility>

#include "polymm/strategy.h"
#include "strategyframe/strategyframe.h"

int main(int argc, char** argv) {
  if (argc != 2 && argc != 3) {
    std::cerr << "usage: poly-mm CONFIG.yaml [--accept-live-trading]\n";
    return 2;
  }
  const bool live_accepted =
      argc == 3 && std::string_view(argv[2]) == "--accept-live-trading";
  if (argc == 3 && !live_accepted) {
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
  if (live_enabled && !live_accepted) {
    std::cerr << "live OMS venue requires --accept-live-trading\n";
    return 2;
  }
  strategyframe::StrategyRunner<polymm::PolyMm> runner(
      std::move(loaded.value), polymm::PolyMm{});
  const auto result = runner.run();
  if (result != strategyframe::Error::Ok) {
    std::cerr << "poly-mm stopped with error "
              << static_cast<unsigned>(result) << '\n';
    return 1;
  }
  return 0;
}
