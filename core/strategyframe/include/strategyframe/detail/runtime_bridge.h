#pragma once

#include <memory>

#include "strategyframe/config.h"
#include "strategyframe/error.h"
#include "strategyframe/strategy_context.h"
#include "strategyframe/types.h"

namespace strategyframe::detail {

struct CallbackTable {
  bool (*init)(void*, StrategyContext&) noexcept{};
  bool (*bbo)(void*, const BboUpdate&) noexcept{};
  bool (*book)(void*, const OrderBookUpdate&) noexcept{};
  bool (*agg_bbo)(void*, const AggBboUpdate&) noexcept{};
  bool (*agg_book)(void*, const AggOrderBookUpdate&) noexcept{};
  bool (*execution)(void*, const ExecutionUpdate&) noexcept{};
  bool (*status)(void*, const OmsStatusUpdate&) noexcept{};
  bool (*timer)(void*, const TimerEvent&) noexcept{};
  bool (*catalog)(void*, const InstrumentCatalogInfo&) noexcept{};
  bool (*open_orders_snapshot)(void*, const OpenOrdersSnapshot&) noexcept{};
  bool (*positions_snapshot)(void*, const PositionsSnapshot&) noexcept{};
  bool (*query_complete)(void*, const QueryComplete&) noexcept{};
};

class Runtime {
 public:
  virtual ~Runtime() = default;
  [[nodiscard]] virtual Error run() noexcept = 0;
  virtual void request_stop() noexcept = 0;
  [[nodiscard]] virtual RuntimeMetrics metrics() const noexcept = 0;
};

[[nodiscard]] Result<std::unique_ptr<Runtime>> make_runtime(
    StrategyFrameConfig config, const CallbackTable& callbacks,
    void* strategy) noexcept;

}  // namespace strategyframe::detail
