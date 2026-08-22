#include <iostream>
#include <string>
#include <string_view>

#include "strategyframe/config.h"

int main(int argc, char** argv) {
  std::string path;
  bool validate_only = false;
  for (int index = 1; index < argc; ++index) {
    const std::string_view argument(argv[index]);
    if (argument == "--config" && index + 1 < argc) {
      path = argv[++index];
    } else if (argument == "--validate-only") {
      validate_only = true;
    } else {
      std::cerr << "usage: strategyframe_config_tool --config PATH "
                   "--validate-only\n";
      return 2;
    }
  }
  if (path.empty() || !validate_only) return 2;
  const auto loaded = strategyframe::load_config(path);
  if (!loaded) {
    std::cerr << "invalid StrategyFrame configuration\n";
    return 1;
  }
  std::cout << "StrategyFrame configuration valid\n";
  return 0;
}
