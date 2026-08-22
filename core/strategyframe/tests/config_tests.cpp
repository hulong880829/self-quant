#include <stdexcept>

#include "strategyframe/config.h"

namespace {

void Require(bool condition) {
  if (!condition) throw std::runtime_error("config requirement failed");
}

}  // namespace

int main() {
  const auto loaded = strategyframe::load_config(
      STRATEGYFRAME_CONFIG_FIXTURE);
  Require(static_cast<bool>(loaded));
  Require(loaded.value.mds.source ==
          strategyframe::MdsSourceMode::ExternalSharedMemory);
  Require(loaded.value.mds.required_instruments.size() == 1);
  Require(loaded.value.mds.required_instruments[0].canonical_symbol ==
          "BTCUSDT");
  Require(loaded.value.threading ==
          strategyframe::ThreadingMode::SingleThread);
  const auto spread =
      loaded.value.strategy.require_int("quote_spread_bps");
  Require(spread && spread.value == 8);
  const auto maximum =
      loaded.value.strategy.require_int("max_position");
  Require(maximum && maximum.value == 500);
  const auto legs = loaded.value.strategy.sequence_size("legs");
  Require(legs && legs.value == 2);
  const auto first_leg = loaded.value.strategy.child("legs.0");
  const auto symbol = first_leg.require_string("symbol");
  Require(symbol && symbol.value == "BTCUSDT");
  Require(!loaded.value.strategy.contains("missing"));
  return 0;
}
