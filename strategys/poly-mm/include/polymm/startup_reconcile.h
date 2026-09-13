#pragma once

#include <span>

#include "strategyframe/types.h"

namespace polymm {

enum class ReconcileVerdict : std::uint8_t {
  Pending = 0,
  Ready = 1,
  Reject = 2,
};

struct StartupReconcile {
  bool oms_reconcile_seen{};
  bool oms_reconcile_failed{};
  bool saw_open_order{};
  bool saw_position{};
  std::uint64_t started_ns{};
  std::uint64_t timeout_ns{};
};

inline void note_oms_reconcile(
    StartupReconcile& state,
    const strategyframe::OmsStatusUpdate& update) noexcept {
  if (update.kind != strategyframe::OmsStatusUpdate::Kind::ReconcileComplete)
    return;
  state.oms_reconcile_seen = true;
  if (update.error != 0) state.oms_reconcile_failed = true;
}

inline void inspect_local_account(
    StartupReconcile& state,
    std::span<const strategyframe::OrderView> orders,
    std::span<const strategyframe::PositionView> positions,
    strategyframe::AccountId account_id,
    strategyframe::InstrumentId up_instrument,
    strategyframe::InstrumentId down_instrument) noexcept {
  for (const auto& order : orders) {
    if (order.account_id == account_id &&
        order.remaining_quantity.value != 0) {
      state.saw_open_order = true;
      break;
    }
  }
  for (const auto& position : positions) {
    if (position.account_id != account_id) continue;
    if (position.instrument_id != up_instrument &&
        position.instrument_id != down_instrument) {
      continue;
    }
    if (position.quantity.value != 0) {
      state.saw_position = true;
      break;
    }
  }
}

[[nodiscard]] inline ReconcileVerdict reconcile_verdict(
    const StartupReconcile& state, std::uint64_t now_ns) noexcept {
  if (state.oms_reconcile_failed || state.saw_open_order ||
      state.saw_position) {
    return ReconcileVerdict::Reject;
  }
  const bool timed_out =
      state.timeout_ns != 0 && now_ns >= state.started_ns + state.timeout_ns;
  if (state.oms_reconcile_seen || timed_out) return ReconcileVerdict::Ready;
  return ReconcileVerdict::Pending;
}

}  // namespace polymm
