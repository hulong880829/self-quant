#include <stdexcept>
#include <string>

#include "strategyframe/config.h"

namespace {

void Require(bool condition) {
  if (!condition) throw std::runtime_error("config requirement failed");
}

std::string Fixture(const char* name) {
  return std::string(STRATEGYFRAME_FIXTURES_DIR) + "/" + name;
}

void RequireInvalid(const char* name) {
  const auto loaded = strategyframe::load_config(Fixture(name));
  Require(!loaded);
  Require(loaded.error == strategyframe::Error::InvalidConfig);
}

}  // namespace

int main() {
  const auto loaded = strategyframe::load_config(
      STRATEGYFRAME_CONFIG_FIXTURE);
  Require(static_cast<bool>(loaded));
  Require(loaded.value.mds.source ==
          strategyframe::MdsSourceMode::ExternalSharedMemory);
  Require(loaded.value.mds.bbo_policy ==
          strategyframe::BboPolicy::TickerOnly);
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

  const auto extensions = strategyframe::load_config(
      STRATEGYFRAME_VENUE_EXTENSIONS_FIXTURE);
  Require(static_cast<bool>(extensions));
  Require(extensions.value.mds.required_instruments.size() == 2);
  Require(extensions.value.mds.required_instruments[0].venue ==
          strategyframe::Venue::Aster);
  Require(extensions.value.mds.required_instruments[0].product ==
          strategyframe::ProductType::Spot);
  Require(extensions.value.mds.required_instruments[0].canonical_symbol ==
          "BTCUSDT");
  Require(extensions.value.mds.required_instruments[1].venue ==
          strategyframe::Venue::Lighter);
  Require(extensions.value.mds.required_instruments[1].product ==
          strategyframe::ProductType::Perpetual);
  Require(extensions.value.mds.required_instruments[1].canonical_symbol ==
          "BTCUSDC");

  const auto canonical =
      strategyframe::load_config(Fixture("canonical_names.yaml"));
  Require(static_cast<bool>(canonical));
  const auto& names = canonical.value.mds.required_instruments;
  Require(names.size() == 10);
  Require(names[0].venue == strategyframe::Venue::Binance);
  Require(names[0].product == strategyframe::ProductType::Spot);
  Require(names[1].venue == strategyframe::Venue::Okx);
  Require(names[1].product == strategyframe::ProductType::Perpetual);
  Require(names[2].venue == strategyframe::Venue::Bybit);
  Require(names[2].product == strategyframe::ProductType::Future);
  Require(names[3].venue == strategyframe::Venue::Gate);
  Require(names[3].product == strategyframe::ProductType::BinaryOption);
  Require(names[4].venue == strategyframe::Venue::Bitget);
  Require(names[4].product == strategyframe::ProductType::Equity);
  Require(names[5].venue == strategyframe::Venue::Polymarket);
  Require(names[5].product == strategyframe::ProductType::BinaryOption);
  Require(names[6].venue == strategyframe::Venue::Sse);
  Require(names[6].product == strategyframe::ProductType::Equity);
  Require(names[7].venue == strategyframe::Venue::Hyperliquid);
  Require(names[7].product == strategyframe::ProductType::Perpetual);
  Require(names[8].venue == strategyframe::Venue::Aster);
  Require(names[8].product == strategyframe::ProductType::Spot);
  Require(names[9].venue == strategyframe::Venue::Lighter);
  Require(names[9].product == strategyframe::ProductType::Perpetual);

  RequireInvalid("reject_uppercase_venue.yaml");
  RequireInvalid("reject_mixed_case_product.yaml");
  RequireInvalid("reject_mds_perp_alias.yaml");
  RequireInvalid("reject_binary_hyphen.yaml");
  RequireInvalid("reject_bbo_policy.yaml");
  RequireInvalid("reject_implicit_dual_bbo_policy.yaml");
  return 0;
}
