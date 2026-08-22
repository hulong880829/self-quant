#include <stdexcept>

#include "strategyframe/strategyframe.h"

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
