#include "oms/runtime/event_notifier.h"

#include <cerrno>
#include <cstdint>
#include <limits>
#include <system_error>

#include <sys/eventfd.h>
#include <unistd.h>

namespace oms::runtime {

EventNotifier::EventNotifier()
    : fd_(::eventfd(0, EFD_NONBLOCK | EFD_CLOEXEC)) {
  if (fd_ < 0) {
    throw std::system_error(errno, std::generic_category(), "eventfd");
  }
}

EventNotifier::~EventNotifier() {
  if (fd_ >= 0) {
    ::close(fd_);
  }
}

int EventNotifier::notify() noexcept {
  constexpr std::uint64_t kIncrement = 1;
  for (;;) {
    const ssize_t written = ::write(fd_, &kIncrement, sizeof(kIncrement));
    if (written == static_cast<ssize_t>(sizeof(kIncrement))) {
      return 0;
    }
    if (written < 0 && errno == EINTR) {
      continue;
    }
    if (written < 0 && errno == EAGAIN) {
      return 0;
    }
    return written < 0 ? errno : EIO;
  }
}

NotifierResult EventNotifier::drain() noexcept {
  std::uint64_t total = 0;
  for (;;) {
    std::uint64_t value = 0;
    const ssize_t bytes = ::read(fd_, &value, sizeof(value));
    if (bytes == static_cast<ssize_t>(sizeof(value))) {
      if (value > std::numeric_limits<std::uint64_t>::max() - total) {
        total = std::numeric_limits<std::uint64_t>::max();
      } else {
        total += value;
      }
      continue;
    }
    if (bytes < 0 && errno == EINTR) {
      continue;
    }
    if (bytes < 0 && errno == EAGAIN) {
      return {total, 0};
    }
    return {total, bytes < 0 ? errno : EIO};
  }
}

int EventNotifier::notify_if_armed() noexcept {
  if (!armed_.exchange(false, std::memory_order_acq_rel)) {
    return 0;
  }
  const int error = notify();
  if (error != 0) {
    armed_.store(true, std::memory_order_release);
  }
  return error;
}

}  // namespace oms::runtime
