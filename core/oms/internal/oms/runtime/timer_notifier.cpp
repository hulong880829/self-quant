#include "oms/runtime/timer_notifier.h"

#include <cerrno>
#include <cstdint>
#include <limits>
#include <system_error>

#include <sys/timerfd.h>
#include <time.h>
#include <unistd.h>

namespace oms::runtime {
namespace {

int SetTimer(int fd, int flags, const itimerspec& spec) noexcept {
  for (;;) {
    if (::timerfd_settime(fd, flags, &spec, nullptr) == 0) {
      return 0;
    }
    if (errno != EINTR) {
      return errno;
    }
  }
}

}  // namespace

TimerNotifier::TimerNotifier()
    : fd_(::timerfd_create(CLOCK_MONOTONIC, TFD_NONBLOCK | TFD_CLOEXEC)) {
  if (fd_ < 0) {
    throw std::system_error(errno, std::generic_category(), "timerfd_create");
  }
}

TimerNotifier::~TimerNotifier() {
  if (fd_ >= 0) {
    ::close(fd_);
  }
}

int TimerNotifier::arm(std::uint64_t monotonic_ns) noexcept {
  if (monotonic_ns == 0) {
    monotonic_ns = 1;
  }

  constexpr std::uint64_t kNanosPerSecond = 1'000'000'000ULL;
  const std::uint64_t seconds = monotonic_ns / kNanosPerSecond;
  if (seconds >
      static_cast<std::uint64_t>(std::numeric_limits<time_t>::max())) {
    return EOVERFLOW;
  }

  itimerspec spec{};
  spec.it_value.tv_sec = static_cast<time_t>(seconds);
  spec.it_value.tv_nsec =
      static_cast<long>(monotonic_ns % kNanosPerSecond);
  return SetTimer(fd_, TFD_TIMER_ABSTIME, spec);
}

int TimerNotifier::disarm() noexcept {
  const itimerspec spec{};
  return SetTimer(fd_, 0, spec);
}

NotifierResult TimerNotifier::drain() noexcept {
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

}  // namespace oms::runtime
