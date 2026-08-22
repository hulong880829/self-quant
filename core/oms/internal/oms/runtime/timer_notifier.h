#pragma once

#include <cstdint>

#include "oms/runtime/notifier_result.h"

namespace oms::runtime {

class TimerNotifier {
 public:
  TimerNotifier();
  ~TimerNotifier();

  TimerNotifier(const TimerNotifier&) = delete;
  TimerNotifier& operator=(const TimerNotifier&) = delete;
  TimerNotifier(TimerNotifier&&) = delete;
  TimerNotifier& operator=(TimerNotifier&&) = delete;

  [[nodiscard]] int fd() const noexcept { return fd_; }

  // Arms a one-shot timer at an absolute CLOCK_MONOTONIC timestamp.
  // A zero timestamp is normalized to 1 ns because timerfd interprets an
  // all-zero it_value as disarmed. Returns zero or an errno value.
  [[nodiscard]] int arm(std::uint64_t monotonic_ns) noexcept;
  [[nodiscard]] int disarm() noexcept;

  // Drains and sums all currently observable expiration counts.
  [[nodiscard]] NotifierResult drain() noexcept;

 private:
  int fd_{-1};
};

}  // namespace oms::runtime
