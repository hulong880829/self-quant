#pragma once

#include <concepts>

#include "strategyframe/strategy_context.h"
#include "strategyframe/types.h"

namespace strategyframe {

template <typename T>
concept Strategy = requires(
    T& strategy, StrategyContext& context, const BboUpdate& bbo,
    const OrderBookUpdate& book, const AggBboUpdate& aggregate_bbo,
    const AggOrderBookUpdate& aggregate_book,
    const ExecutionUpdate& execution, const OmsStatusUpdate& status,
    const TimerEvent& timer) {
  { strategy.init(context) } -> std::same_as<void>;
  { strategy.on_bbo_update(bbo) } -> std::same_as<void>;
  { strategy.on_orderbook_update(book) } -> std::same_as<void>;
  { strategy.on_agg_bbo_update(aggregate_bbo) } -> std::same_as<void>;
  { strategy.on_agg_orderbook_update(aggregate_book) } -> std::same_as<void>;
  { strategy.on_order_update(execution) } -> std::same_as<void>;
  { strategy.on_oms_status(status) } -> std::same_as<void>;
  { strategy.on_timer(timer) } -> std::same_as<void>;
};

}  // namespace strategyframe
