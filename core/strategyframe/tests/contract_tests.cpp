#include <cstdint>
#include <stdexcept>
#include <type_traits>

#include "strategyframe/strategyframe.h"
#include "utils/md/types.h"

namespace {

struct CompleteStrategy {
  void init(strategyframe::StrategyContext&) {}
  void on_bbo_update(const strategyframe::BboUpdate&) {}
  void on_orderbook_update(const strategyframe::OrderBookUpdate&) {}
  void on_agg_bbo_update(const strategyframe::AggBboUpdate&) {}
  void on_agg_orderbook_update(
      const strategyframe::AggOrderBookUpdate&) {}
  void on_order_update(const strategyframe::ExecutionUpdate&) {}
  void on_oms_status(const strategyframe::OmsStatusUpdate&) {}
  void on_timer(const strategyframe::TimerEvent&) {}
};

struct IncompleteStrategy {
  void init(strategyframe::StrategyContext&) {}
};

static_assert(strategyframe::Strategy<CompleteStrategy>);
static_assert(!strategyframe::Strategy<IncompleteStrategy>);
static_assert(std::is_same_v<strategyframe::Venue, utils::md::Venue>);
static_assert(
    std::is_same_v<strategyframe::ProductType, utils::md::ProductType>);
static_assert(
    std::is_same_v<strategyframe::InstrumentId, utils::md::InstrumentId>);
static_assert(std::is_same_v<std::underlying_type_t<strategyframe::Venue>,
                             std::uint16_t>);
static_assert(
    std::is_same_v<std::underlying_type_t<strategyframe::ProductType>,
                   std::uint8_t>);
static_assert(static_cast<std::uint16_t>(strategyframe::Venue::Unknown) ==
              0);
static_assert(static_cast<std::uint16_t>(strategyframe::Venue::Binance) ==
              1);
static_assert(static_cast<std::uint16_t>(strategyframe::Venue::Okx) == 2);
static_assert(static_cast<std::uint16_t>(strategyframe::Venue::Bybit) == 3);
static_assert(static_cast<std::uint16_t>(strategyframe::Venue::Gate) == 4);
static_assert(static_cast<std::uint16_t>(strategyframe::Venue::Bitget) ==
              5);
static_assert(static_cast<std::uint16_t>(
                  strategyframe::Venue::Polymarket) == 6);
static_assert(static_cast<std::uint16_t>(strategyframe::Venue::Sse) == 7);
static_assert(static_cast<std::uint16_t>(
                  strategyframe::Venue::Hyperliquid) == 8);
static_assert(static_cast<std::uint16_t>(strategyframe::Venue::Aster) ==
              9);
static_assert(static_cast<std::uint16_t>(strategyframe::Venue::Lighter) ==
              10);
static_assert(static_cast<std::uint8_t>(
                  strategyframe::ProductType::Unknown) == 0);
static_assert(static_cast<std::uint8_t>(strategyframe::ProductType::Spot) ==
              1);
static_assert(static_cast<std::uint8_t>(
                  strategyframe::ProductType::Perpetual) == 2);
static_assert(
    static_cast<std::uint8_t>(strategyframe::ProductType::Future) == 3);
static_assert(static_cast<std::uint8_t>(
                  strategyframe::ProductType::BinaryOption) == 4);
static_assert(
    static_cast<std::uint8_t>(strategyframe::ProductType::Equity) == 5);

void Require(bool condition) {
  if (!condition) throw std::runtime_error("contract requirement failed");
}

}  // namespace

int main() {
  strategyframe::FixedPoint value{42, 2, {}};
  Require(value.value == 42);
  Require(sizeof(strategyframe::OrderToken) == 16);
  return 0;
}
