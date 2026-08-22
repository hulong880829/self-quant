#pragma once

#include "strategyframe/managers.h"

namespace strategyframe::detail {

struct ManagerAccess {
  static Error InsertPending(OrderManager& manager,
                             const OrderRequest& request,
                             OrderToken token) noexcept {
    return manager.insert_pending(request, token);
  }
  static Error Apply(OrderManager& manager,
                     const ExecutionUpdate& update) noexcept {
    return manager.apply(update);
  }
  static Error ReconcileOpen(OrderManager& manager,
                             const OrderView& order) noexcept {
    return manager.reconcile_open(order);
  }
  static void Clear(OrderManager& manager) noexcept { manager.clear(); }

  static Error ApplyFill(PositionManager& manager, AccountId account,
                         InstrumentId instrument, Side side,
                         PositionSide position_side,
                         const FixedPoint& quantity,
                         const TradeId& trade_id,
                         std::uint64_t generation) noexcept {
    return manager.apply_fill(account, instrument, side, position_side,
                              quantity, trade_id, generation);
  }
  static Error ReplaceSnapshot(
      PositionManager& manager,
      std::span<const PositionView> positions) noexcept {
    return manager.replace_snapshot(positions);
  }
  static void RetireInstrument(PositionManager& manager,
                               InstrumentId instrument) noexcept {
    manager.retire_instrument(instrument);
  }
};

}  // namespace strategyframe::detail
