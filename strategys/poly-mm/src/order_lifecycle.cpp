#include "polymm/order_lifecycle.h"

namespace polymm {

bool same_token(strategyframe::OrderToken lhs,
                strategyframe::OrderToken rhs) noexcept {
  return lhs.sequence != 0 && lhs == rhs;
}

bool is_terminal_status(strategyframe::OrderStatus status) noexcept {
  return status == strategyframe::OrderStatus::Filled ||
         status == strategyframe::OrderStatus::Canceled ||
         status == strategyframe::OrderStatus::Rejected ||
         status == strategyframe::OrderStatus::Expired;
}

bool is_active_order(OrderState state) noexcept {
  return state == OrderState::PendingOpen ||
         state == OrderState::PendingClose ||
         state == OrderState::PendingForce ||
         state == OrderState::PendingCancel ||
         state == OrderState::NeedsReconcile;
}

bool allows_new_open(OrderState state) noexcept {
  return state == OrderState::Idle;
}

bool allows_close_submit(OrderState state) noexcept {
  return state == OrderState::Idle;
}

bool below_minimum(double quantity, double minimum_order_size) noexcept {
  return quantity > 0.0 && quantity + 1e-12 < minimum_order_size;
}

bool uncertain_command_error(std::int32_t error) noexcept {
  const auto value = static_cast<strategyframe::Error>(error);
  return value == strategyframe::Error::OmsFailure ||
         value == strategyframe::Error::NotReady ||
         value == strategyframe::Error::Internal;
}

OrderState pending_state_for(OrderPurpose purpose) noexcept {
  switch (purpose) {
    case OrderPurpose::Close:
      return OrderState::PendingClose;
    case OrderPurpose::ForceFlatten:
      return OrderState::PendingForce;
    case OrderPurpose::Open:
    default:
      return OrderState::PendingOpen;
  }
}

OrderState after_inventory(double position_quantity,
                           double minimum_order_size) noexcept {
  if (below_minimum(position_quantity, minimum_order_size))
    return OrderState::Dust;
  return OrderState::Idle;
}

namespace {

LifecycleResult FinishInventory(double position_quantity,
                                double minimum_order_size,
                                bool release_ledger) noexcept {
  LifecycleResult result;
  result.matched = true;
  result.release_ledger = release_ledger;
  result.kind = below_minimum(position_quantity, minimum_order_size)
                    ? LifecycleKind::GoDust
                    : LifecycleKind::GoIdle;
  return result;
}

LifecycleResult Restore(const LegOrder& leg) noexcept {
  LifecycleResult result;
  result.matched = true;
  result.kind = LifecycleKind::RestorePending;
  result.restore_state = pending_state_for(leg.purpose);
  return result;
}

}  // namespace

LifecycleResult apply_order_event(const LegOrder& leg,
                                  const strategyframe::ExecutionUpdate& update,
                                  double position_quantity,
                                  double minimum_order_size) noexcept {
  LifecycleResult result;
  if (update.kind == strategyframe::ExecutionUpdate::Kind::Fill) {
    if (!same_token(update.token, leg.token)) return result;
    result.matched = true;
    result.apply_fill = true;
    if (update.remaining_quantity.value == 0 ||
        update.status == strategyframe::OrderStatus::Filled) {
      return FinishInventory(position_quantity, minimum_order_size, true);
    }
    result.kind = LifecycleKind::KeepPending;
    return result;
  }

  if (update.kind == strategyframe::ExecutionUpdate::Kind::CommandResult) {
    if (!same_token(update.token, leg.token)) return result;
    result.matched = true;
    if (update.error == 0) return result;
    result.stop_opening = true;
    if (update.update_type == kCommandCancel) return Restore(leg);
    if (update.update_type != kCommandPlace) return result;
    if (uncertain_command_error(update.error) || leg.saw_fill) {
      result.kind = LifecycleKind::NeedsReconcile;
      return result;
    }
    result.kind = LifecycleKind::PlaceFailed;
    result.release_ledger = true;
    return result;
  }

  if (update.kind != strategyframe::ExecutionUpdate::Kind::Order)
    return result;
  if (!same_token(update.token, leg.token)) return result;
  result.matched = true;
  if (update.update_type == kOrderCancelRejected) {
    if (leg.state == OrderState::PendingCancel ||
        leg.state == OrderState::NeedsReconcile) {
      return Restore(leg);
    }
    if (is_active_order(leg.state)) {
      result.kind = LifecycleKind::KeepPending;
      return result;
    }
    return result;
  }
  if (!is_terminal_status(update.status)) return result;
  return FinishInventory(position_quantity, minimum_order_size, true);
}

void clear_leg_order(LegOrder& leg) noexcept {
  leg.token = {};
  leg.price = {};
  leg.started_ns = 0;
  leg.saw_fill = false;
  leg.cancel_attempts = 0;
  leg.last_error = 0;
}

void commit_lifecycle(LegOrder& leg, const LifecycleResult& result) noexcept {
  switch (result.kind) {
    case LifecycleKind::Ignore:
      return;
    case LifecycleKind::KeepPending:
      if (result.apply_fill) leg.saw_fill = true;
      return;
    case LifecycleKind::GoIdle:
      clear_leg_order(leg);
      leg.state = OrderState::Idle;
      return;
    case LifecycleKind::GoDust:
      clear_leg_order(leg);
      leg.state = OrderState::Dust;
      return;
    case LifecycleKind::RestorePending:
      leg.state = result.restore_state;
      return;
    case LifecycleKind::NeedsReconcile:
      leg.state = OrderState::NeedsReconcile;
      return;
    case LifecycleKind::PlaceFailed:
      clear_leg_order(leg);
      leg.state = OrderState::Idle;
      return;
  }
}

}  // namespace polymm
