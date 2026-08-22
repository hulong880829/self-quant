#include <array>
#include <cstdint>
#include <utility>

#include "strategyframe/strategyframe.h"

class CrossVenueArbitrage {
 public:
  void init(strategyframe::StrategyContext& context) {
    context_ = &context;
    first_ = static_cast<strategyframe::InstrumentId>(
        context.params().optional_int("first_instrument", 0));
    second_ = static_cast<strategyframe::InstrumentId>(
        context.params().optional_int("second_instrument", 0));
    quantity_ = {context.params().optional_int("quantity", 1), 0, {}};
  }

  void on_bbo_update(const strategyframe::BboUpdate& update) {
    if (update.header.instrument_id == first_)
      bbo_[0] = update;
    else if (update.header.instrument_id == second_)
      bbo_[1] = update;
    else
      return;
    if (!context_ || active_ || bbo_[0].header.instrument_id == 0 ||
        bbo_[1].header.instrument_id == 0)
      return;
    if (bbo_[0].ask.price.value < bbo_[1].bid.price.value) {
      strategyframe::OrderRequest buy_request;
      buy_request.account_id = 1;
      buy_request.instrument_id = first_;
      buy_request.side = strategyframe::Side::Buy;
      buy_request.time_in_force = strategyframe::TimeInForce::IOC;
      buy_request.quantity = quantity_;
      buy_request.price = bbo_[0].ask.price;
      strategyframe::OrderRequest sell_request = buy_request;
      sell_request.instrument_id = second_;
      sell_request.side = strategyframe::Side::Sell;
      sell_request.time_in_force = strategyframe::TimeInForce::FOK;
      sell_request.price = bbo_[1].bid.price;
      const auto buy = context_->place_order(buy_request);
      const auto sell = context_->place_order(sell_request);
      active_ = buy && sell;
      if (buy && !sell) (void)context_->cancel(buy.value);
    }
  }
  void on_orderbook_update(const strategyframe::OrderBookUpdate&) {}
  void on_agg_bbo_update(const strategyframe::AggBboUpdate&) {}
  void on_agg_orderbook_update(
      const strategyframe::AggOrderBookUpdate&) {}
  void on_order_update(const strategyframe::ExecutionUpdate&) {
    active_ = !context_->open_orders().empty();
  }
  void on_oms_status(const strategyframe::OmsStatusUpdate&) {}
  void on_timer(const strategyframe::TimerEvent&) {}

 private:
  strategyframe::StrategyContext* context_{};
  strategyframe::InstrumentId first_{};
  strategyframe::InstrumentId second_{};
  strategyframe::FixedPoint quantity_{};
  std::array<strategyframe::BboUpdate, 2> bbo_{};
  bool active_{};
};

static_assert(strategyframe::Strategy<CrossVenueArbitrage>);

int main(int argc, char** argv) {
  if (argc != 2) return 2;
  auto loaded = strategyframe::load_config(argv[1]);
  if (!loaded) return 1;
  strategyframe::StrategyRunner<CrossVenueArbitrage> runner(
      std::move(loaded.value), CrossVenueArbitrage{});
  return runner.run() == strategyframe::Error::Ok ? 0 : 1;
}
