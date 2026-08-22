#include "mds/instrument/instrument_manager.h"

#include <cassert>
#include <cstdint>
#include <string>

namespace {
utils::md::InstrumentId collide(std::string_view) noexcept { return 42; }
std::uint64_t unstable_id = 100;
utils::md::InstrumentId unstable(std::string_view) noexcept {
  return unstable_id++;
}
}

int main() {
  using utils::md::ProductType;
  using utils::md::Venue;

  const auto key = mds::instrument::InstrumentManager::canonical_key(
      Venue::Binance, ProductType::Perpetual, "BTCUSDT");
  assert(key == "binance|perpetual|BTCUSDT");
  assert(mds::instrument::InstrumentManager::hash_key(key) ==
         14695648017863674520ULL);

  mds::instrument::InstrumentManager manager;
  utils::md::InstrumentId first{};
  utils::md::InstrumentId second{};
  std::string error;
  assert(manager.resolve(Venue::Binance, ProductType::Perpetual, "BTCUSDT",
                         first, error));
  assert(manager.resolve(Venue::Binance, ProductType::Perpetual, "BTCUSDT",
                         second, error));
  assert(first == second);
  assert(first != 0);

  utils::md::InstrumentId polymarket{};
  assert(manager.resolve(Venue::Polymarket, ProductType::BinaryOption,
                         "btc5mup", polymarket, error));
  assert(polymarket != first);
  assert(mds::instrument::InstrumentManager::canonical_key(
             Venue::Polymarket, ProductType::BinaryOption, "btc5mup") ==
         "polymarket|binary_option|btc5mup");
  utils::md::InstrumentId first_window{};
  utils::md::InstrumentId second_window{};
  assert(manager.resolve_instance(
      Venue::Polymarket, ProductType::BinaryOption, "btc5mup",
      "btc-updown-5m-100|UP", first_window, error));
  assert(manager.resolve_instance(
      Venue::Polymarket, ProductType::BinaryOption, "btc5mup",
      "btc-updown-5m-200|UP", second_window, error));
  assert(first_window != second_window);
  utils::md::InstrumentId repeat_window{};
  assert(manager.resolve_instance(
      Venue::Polymarket, ProductType::BinaryOption, "btc5mup",
      "btc-updown-5m-100|UP", repeat_window, error));
  assert(repeat_window == first_window);
  assert(manager.retire(first_window));
  assert(!manager.retire(first_window));
  assert(manager.resolve_instance(
      Venue::Polymarket, ProductType::BinaryOption, "btc5mup",
      "btc-updown-5m-100|UP", repeat_window, error));
  assert(repeat_window == first_window);

  mds::instrument::InstrumentManager collision_manager(&collide);
  utils::md::InstrumentId collision{};
  assert(collision_manager.resolve(Venue::Binance, ProductType::Spot,
                                   "BTCUSDT", collision, error));
  assert(!collision_manager.resolve(Venue::Binance, ProductType::Spot,
                                    "ETHUSDT", collision, error));
  assert(error.find("existing=binance|spot|BTCUSDT") != std::string::npos);
  assert(error.find("incoming=binance|spot|ETHUSDT") != std::string::npos);

  mds::instrument::InstrumentManager unstable_manager(&unstable);
  assert(unstable_manager.resolve(Venue::Binance, ProductType::Spot,
                                  "BTCUSDT", collision, error));
  assert(!unstable_manager.resolve(Venue::Binance, ProductType::Spot,
                                   "BTCUSDT", collision, error));
  assert(error.find("instrument identity remap") != std::string::npos);
  return 0;
}
