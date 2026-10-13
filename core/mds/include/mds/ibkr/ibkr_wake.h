#pragma once

#include <atomic>
#include <cstdint>
#include <unistd.h>

namespace mds::ibkr {

// Coalesces eventfd wakes. wake_pending only means "a wakeup is already armed".
class WakeGate {
 public:
  explicit WakeGate(int event_fd = -1) noexcept : event_fd_(event_fd) {}

  void set_fd(int event_fd) noexcept { event_fd_ = event_fd; }

  // Returns true when this call transitioned pending from false to true and
  // therefore must write the eventfd. Later calls until clear() skip the syscall.
  [[nodiscard]] bool arm() noexcept {
    if (pending_.exchange(true, std::memory_order_acq_rel)) {
      return false;
    }
    if (event_fd_ < 0) {
      syscalls_.fetch_add(1, std::memory_order_relaxed);
      return true;
    }
    const std::uint64_t one = 1;
    if (::write(event_fd_, &one, sizeof(one)) !=
        static_cast<ssize_t>(sizeof(one))) {
      pending_.store(false, std::memory_order_release);
      return false;
    }
    syscalls_.fetch_add(1, std::memory_order_relaxed);
    return true;
  }

  void force() noexcept {
    pending_.store(false, std::memory_order_release);
    (void)arm();
  }

  void clear() noexcept { pending_.store(false, std::memory_order_release); }

  [[nodiscard]] bool pending() const noexcept {
    return pending_.load(std::memory_order_acquire);
  }

  [[nodiscard]] std::uint64_t syscalls() const noexcept {
    return syscalls_.load(std::memory_order_relaxed);
  }

 private:
  int event_fd_{-1};
  std::atomic<bool> pending_{false};
  std::atomic<std::uint64_t> syscalls_{0};
};

}  // namespace mds::ibkr
