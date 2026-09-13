#pragma once

#include "polymm/types.h"

namespace polymm {

[[nodiscard]] inline bool allows_open(HaltPhase phase) noexcept {
  return phase == HaltPhase::Running;
}

[[nodiscard]] inline bool allows_close(HaltPhase phase) noexcept {
  return phase != HaltPhase::Halted;
}

[[nodiscard]] inline bool should_cancel_working(HaltPhase phase) noexcept {
  return phase == HaltPhase::Flattening;
}

[[nodiscard]] inline bool can_complete_halt(HaltPhase phase, bool active_orders,
                                            bool position_safe) noexcept {
  return phase == HaltPhase::Flattening && !active_orders && position_safe;
}

[[nodiscard]] inline HaltPhase begin_stop_opening(HaltPhase phase) noexcept {
  if (phase == HaltPhase::Flattening || phase == HaltPhase::Halted)
    return phase;
  return HaltPhase::StopOpening;
}

[[nodiscard]] inline HaltPhase begin_flattening(HaltPhase) noexcept {
  return HaltPhase::Flattening;
}

[[nodiscard]] inline HaltPhase confirm_halt(HaltPhase phase, bool active_orders,
                                            bool position_safe) noexcept {
  return can_complete_halt(phase, active_orders, position_safe)
             ? HaltPhase::Halted
             : phase;
}

}  // namespace polymm
