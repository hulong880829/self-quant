#include <chrono>
#include <cstdint>
#include <stdexcept>
#include <thread>

#include "strategyframe/strategyframe.h"

namespace {

void Require(bool condition) {
  if (!condition) throw std::runtime_error("runtime requirement failed");
}

struct State {
  strategyframe::StrategyContext* context{};
  std::uint32_t timers{};
  bool valid_handle{};
  bool stale_handle_rejected{};
};

struct TimerStrategy {
  State* state{};

  void init(strategyframe::StrategyContext& context) {
    state->context = &context;
    const auto timer =
        context.schedule_timer(context.now_ns() + 1'000'000ULL);
    state->valid_handle = static_cast<bool>(timer);
  }
  void on_bbo_update(const strategyframe::BboUpdate&) {}
  void on_orderbook_update(const strategyframe::OrderBookUpdate&) {}
  void on_agg_bbo_update(const strategyframe::AggBboUpdate&) {}
  void on_agg_orderbook_update(
      const strategyframe::AggOrderBookUpdate&) {}
  void on_order_update(const strategyframe::ExecutionUpdate&) {}
  void on_oms_status(const strategyframe::OmsStatusUpdate&) {}
  void on_timer(const strategyframe::TimerEvent& event) {
    Require(event.expiration_count == 1);
    state->stale_handle_rejected =
        state->context->cancel_timer(event.handle) ==
        strategyframe::Error::NotFound;
    ++state->timers;
    state->context->request_stop();
  }
};

struct OrderState {
  strategyframe::StrategyContext* context{};
  bool submitted{};
  bool instrument_found{};
  std::uint32_t updates{};
};

struct ConfiguredOrderStrategy {
  OrderState* state{};

  void init(strategyframe::StrategyContext& context) {
    state->context = &context;
    const auto instrument = context.find_instrument(42);
    state->instrument_found =
        instrument &&
        std::string_view(instrument.value.symbol.value,
                         instrument.value.symbol.length) == "BTCUSDT";
    strategyframe::OrderRequest request;
    request.account_id = 7;
    request.instrument_id = 42;
    request.side = strategyframe::Side::Buy;
    request.quantity = {1, 0, {}};
    request.price = {100, 0, {}};
    state->submitted =
        static_cast<bool>(context.place_order(request));
    Require(static_cast<bool>(
        context.schedule_timer(context.now_ns() + 1'000'000ULL)));
  }
  void on_bbo_update(const strategyframe::BboUpdate&) {}
  void on_orderbook_update(const strategyframe::OrderBookUpdate&) {}
  void on_agg_bbo_update(const strategyframe::AggBboUpdate&) {}
  void on_agg_orderbook_update(
      const strategyframe::AggOrderBookUpdate&) {}
  void on_order_update(const strategyframe::ExecutionUpdate&) {
    ++state->updates;
  }
  void on_oms_status(const strategyframe::OmsStatusUpdate&) {}
  void on_timer(const strategyframe::TimerEvent&) {
    state->context->request_stop();
  }
};

strategyframe::StrategyFrameConfig Config() {
  strategyframe::StrategyFrameConfig result;
  result.mds.source = strategyframe::MdsSourceMode::Replay;
  result.idle_policy = strategyframe::IdlePolicy::LowCpu;
  result.capacities.timer_table = 8;
  result.capacities.order_table = 8;
  result.capacities.position_table = 8;
  result.capacities.command_queue = 8;
  result.capacities.update_queue = 8;
  result.capacities.fill_dedup = 8;
  result.event_budget = 8;
  return result;
}

}  // namespace

int main() {
  State state;
  strategyframe::StrategyRunner<TimerStrategy> runner(Config(),
                                                       TimerStrategy{&state});
  Require(runner.run() == strategyframe::Error::Ok);
  Require(state.valid_handle && state.stale_handle_rejected &&
          state.timers == 1);
  Require(runner.metrics().timer_events == 1);
  for (const auto policy : {strategyframe::IdlePolicy::BusySpin,
                            strategyframe::IdlePolicy::Adaptive}) {
    State policy_state;
    auto policy_config = Config();
    policy_config.idle_policy = policy;
    strategyframe::StrategyRunner<TimerStrategy> policy_runner(
        std::move(policy_config), TimerStrategy{&policy_state});
    Require(policy_runner.run() == strategyframe::Error::Ok);
    Require(policy_state.timers == 1);
  }

  auto configured = Config();
  strategyframe::InstrumentConfig instrument;
  instrument.instrument_id = 42;
  instrument.venue = strategyframe::Venue::Binance;
  instrument.product = strategyframe::ProductType::Spot;
  instrument.symbol = "BTCUSDT";
  instrument.tick_size = 1;
  instrument.lot_size = 1;
  configured.instruments.push_back(instrument);
  OrderState order_state;
  strategyframe::StrategyRunner<ConfiguredOrderStrategy> order_runner(
      std::move(configured), ConfiguredOrderStrategy{&order_state});
  strategyframe::Error order_result{strategyframe::Error::Internal};
  std::thread order_thread(
      [&] { order_result = order_runner.run(); });
  order_thread.join();
  Require(order_result == strategyframe::Error::Ok);
  // oms.instruments is a compatibility validation list only. Without an MDS
  // catalog it must not seed lookup or executable routing.
  Require(!order_state.instrument_found && !order_state.submitted);
  Require(order_state.updates == 0);

  State first_state;
  State second_state;
  strategyframe::StrategyRunner<TimerStrategy> first_runner(
      Config(), TimerStrategy{&first_state});
  strategyframe::StrategyRunner<TimerStrategy> second_runner(
      Config(), TimerStrategy{&second_state});
  strategyframe::Error first_result{strategyframe::Error::Internal};
  strategyframe::Error second_result{strategyframe::Error::Internal};
  std::thread first([&] { first_result = first_runner.run(); });
  std::thread second([&] { second_result = second_runner.run(); });
  first.join();
  second.join();
  Require(first_result == strategyframe::Error::Ok);
  Require(second_result == strategyframe::Error::Ok);
  Require(first_state.timers == 1 && second_state.timers == 1);
  return 0;
}
