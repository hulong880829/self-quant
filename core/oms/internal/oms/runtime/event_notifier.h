#pragma once

#include <atomic>

#include "oms/runtime/notifier_result.h"

namespace oms::runtime {

class EventNotifier {
 public:
  EventNotifier();
  ~EventNotifier();

  EventNotifier(const EventNotifier&) = delete;
  EventNotifier& operator=(const EventNotifier&) = delete;
  EventNotifier(EventNotifier&&) = delete;
  EventNotifier& operator=(EventNotifier&&) = delete;

  [[nodiscard]] int fd() const noexcept { return fd_; }

  // Returns zero on success. EAGAIN is success because a readable event is
  // already pending in the eventfd counter.
  [[nodiscard]] int notify() noexcept;

  // Drains all currently observable counter values. EAGAIN terminates a
  // successful drain; other failures are returned in error.
  [[nodiscard]] NotifierResult drain() noexcept;

  // Empty-to-nonempty protocol:
  // 1. The consumer calls arm() after observing the queue empty, then checks
  //    the queue once more before sleeping.
  // 2. A producer that changes the queue from empty to nonempty calls
  //    notify_if_armed(). Only one producer writes for each arm cycle.
  void arm() noexcept { armed_.store(true, std::memory_order_release); }
  [[nodiscard]] bool armed() const noexcept {
    return armed_.load(std::memory_order_acquire);
  }
  [[nodiscard]] int notify_if_armed() noexcept;

 private:
  int fd_{-1};
  std::atomic<bool> armed_{false};
};

}  // namespace oms::runtime
