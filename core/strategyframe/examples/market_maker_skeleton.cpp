#include <cstdint>
#include <utility>

#include "strategyframe/strategyframe.h"

class MarketMaker {
 public:
  void init(strategyframe::StrategyContext& context) {
    context_ = &context;
    instrument_ = static_cast<strategyframe::InstrumentId>(
        context.params().optional_int("instrument", 0));
    quantity_ = {context.params().optional_int("quantity", 1), 0, {}};
    (void)context.schedule_timer(context.now_ns() + 1'000'000'000ULL,
                                 1'000'000'000ULL);
  }

  void on_bbo_update(const strategyframe::BboUpdate& update) {
    if (!context_ || update.header.instrument_id != instrument_ ||
        !context_->open_orders().empty())
      return;
    strategyframe::OrderRequest bid;
    bid.account_id = 1;
    bid.instrument_id = instrument_;
    bid.side = strategyframe::Side::Buy;
    bid.quantity = quantity_;
    bid.price = update.bid.price;
    (void)context_->place_order(bid);
    strategyframe::OrderRequest ask = bid;
    ask.side = strategyframe::Side::Sell;
    ask.price = update.ask.price;
    (void)context_->place_order(ask);
  }
  void on_orderbook_update(const strategyframe::OrderBookUpdate&) {}
  void on_agg_bbo_update(const strategyframe::AggBboUpdate&) {}
  void on_agg_orderbook_update(
      const strategyframe::AggOrderBookUpdate&) {}
  void on_order_update(const strategyframe::ExecutionUpdate& update) {
    if (update.kind == strategyframe::ExecutionUpdate::Kind::Fill &&
        update.status == strategyframe::OrderStatus::Filled) {
      (void)context_->find_position(1, instrument_);
    }
  }
  void on_oms_status(const strategyframe::OmsStatusUpdate& update) {
    if (update.error != 0) cancel_all();
  }
  void on_timer(const strategyframe::TimerEvent&) { cancel_all(); }

 private:
  void cancel_all() {
    if (!context_) return;
    for (const auto& order : context_->open_orders())
      (void)context_->cancel(order.token);
  }

  strategyframe::StrategyContext* context_{};
  strategyframe::InstrumentId instrument_{};
  strategyframe::FixedPoint quantity_{};
};

static_assert(strategyframe::Strategy<MarketMaker>);

int main(int argc, char** argv) {
  if (argc != 2) return 2;
  auto loaded = strategyframe::load_config(argv[1]);
  if (!loaded) return 1;
  strategyframe::StrategyRunner<MarketMaker> runner(
      std::move(loaded.value), MarketMaker{});
  return runner.run() == strategyframe::Error::Ok ? 0 : 1;
}
