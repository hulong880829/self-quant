#pragma once

#include "polymm/types.h"
#include "strategyframe/error.h"
#include "strategyframe/types.h"

namespace polymm {

inline constexpr std::uint8_t kCommandPlace = 1;
inline constexpr std::uint8_t kCommandCancel = 2;
inline constexpr std::uint8_t kOrderCancelRejected = 6;

struct LegOrder {
  OrderState state{OrderState::Idle};
  OrderPurpose purpose{OrderPurpose::Open};
  strategyframe::OrderToken token{};
  strategyframe::FixedPoint price{};
  std::uint64_t started_ns{};
  bool saw_fill{};
  std::uint32_t cancel_attempts{};
  std::int32_t last_error{};
};

enum class LifecycleKind : std::uint8_t {
  Ignore = 0,
  KeepPending = 1,
  GoIdle = 2,
  GoDust = 3,
  RestorePending = 4,
  NeedsReconcile = 5,
  PlaceFailed = 6,
};

struct LifecycleResult {
  LifecycleKind kind{LifecycleKind::Ignore};
  OrderState restore_state{OrderState::Idle};
  bool apply_fill{};
  bool release_ledger{};
  bool stop_opening{};
  bool matched{};
};

[[nodiscard]] bool same_token(strategyframe::OrderToken lhs,
                              strategyframe::OrderToken rhs) noexcept;
[[nodiscard]] bool is_terminal_status(
    strategyframe::OrderStatus status) noexcept;
[[nodiscard]] bool is_active_order(OrderState state) noexcept;
[[nodiscard]] bool allows_new_open(OrderState state) noexcept;
[[nodiscard]] bool allows_close_submit(OrderState state) noexcept;
[[nodiscard]] bool below_minimum(double quantity,
                                 double minimum_order_size) noexcept;
[[nodiscard]] bool uncertain_command_error(std::int32_t error) noexcept;
[[nodiscard]] OrderState pending_state_for(OrderPurpose purpose) noexcept;
[[nodiscard]] OrderState after_inventory(double position_quantity,
                                         double minimum_order_size) noexcept;
[[nodiscard]] LifecycleResult apply_order_event(
    const LegOrder& leg, const strategyframe::ExecutionUpdate& update,
    double position_quantity, double minimum_order_size) noexcept;
void commit_lifecycle(LegOrder& leg, const LifecycleResult& result) noexcept;
void clear_leg_order(LegOrder& leg) noexcept;

}  // namespace polymm
