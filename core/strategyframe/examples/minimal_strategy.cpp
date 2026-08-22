#include <iostream>
#include <utility>

#include "strategyframe/strategyframe.h"

class MinimalStrategy {
 public:
  void init(strategyframe::StrategyContext& context) {
    context_ = &context;
    const auto spread = context.params().require_int("quote_spread_bps");
    if (spread) spread_bps_ = spread.value;
  }
  void on_bbo_update(const strategyframe::BboUpdate&) {}
  void on_orderbook_update(const strategyframe::OrderBookUpdate&) {}
  void on_agg_bbo_update(const strategyframe::AggBboUpdate&) {}
  void on_agg_orderbook_update(
      const strategyframe::AggOrderBookUpdate&) {}
  void on_order_update(const strategyframe::ExecutionUpdate&) {
    if (context_) {
      std::cout << "open_orders=" << context_->open_orders().size() << '\n';
    }
  }
  void on_oms_status(const strategyframe::OmsStatusUpdate&) {}
  void on_timer(const strategyframe::TimerEvent&) {}

 private:
  strategyframe::StrategyContext* context_{};
  std::int64_t spread_bps_{};
};

static_assert(strategyframe::Strategy<MinimalStrategy>);

int main(int argc, char** argv) {
  if (argc != 2) {
    std::cerr << "usage: strategyframe_minimal_example CONFIG.yaml\n";
    return 2;
  }
  auto loaded = strategyframe::load_config(argv[1]);
  if (!loaded) return 1;
  strategyframe::StrategyRunner<MinimalStrategy> runner(
      std::move(loaded.value), MinimalStrategy{});
  return runner.run() == strategyframe::Error::Ok ? 0 : 1;
}
