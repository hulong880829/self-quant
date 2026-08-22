#include <cassert>
#include <chrono>
#include <cstdint>
#include <thread>

#include <fcntl.h>
#include <time.h>

#include "oms/runtime/deadline_scheduler.h"
#include "oms/runtime/event_notifier.h"
#include "oms/runtime/timer_notifier.h"

namespace {

void AssertDescriptorFlags(int fd) {
  assert((::fcntl(fd, F_GETFL) & O_NONBLOCK) != 0);
  assert((::fcntl(fd, F_GETFD) & FD_CLOEXEC) != 0);
}

std::uint64_t MonotonicNowNs() {
  timespec now{};
  const int result = ::clock_gettime(CLOCK_MONOTONIC, &now);
  assert(result == 0);
  return static_cast<std::uint64_t>(now.tv_sec) * 1'000'000'000ULL +
         static_cast<std::uint64_t>(now.tv_nsec);
}

void TestEventNotifier() {
  oms::runtime::EventNotifier notifier;
  assert(notifier.fd() >= 0);
  AssertDescriptorFlags(notifier.fd());
  assert(notifier.notify() == 0);
  assert(notifier.notify() == 0);
  const auto drained = notifier.drain();
  assert(drained);
  assert(drained.value == 2);

  notifier.arm();
  assert(notifier.notify_if_armed() == 0);
  assert(!notifier.armed());
  assert(notifier.notify_if_armed() == 0);
  assert(notifier.drain().value == 1);
}

void TestTimerNotifier() {
  oms::runtime::TimerNotifier timer;
  assert(timer.fd() >= 0);
  AssertDescriptorFlags(timer.fd());
  assert(timer.arm(MonotonicNowNs() + 1'000'000ULL) == 0);
  std::this_thread::sleep_for(std::chrono::milliseconds(3));
  const auto drained = timer.drain();
  assert(drained);
  assert(drained.value == 1);
  assert(timer.disarm() == 0);
}

void TestDeadlineScheduler() {
  oms::runtime::TimerNotifier timer;
  oms::runtime::DeadlineScheduler scheduler(3, timer);
  const std::uint64_t now = MonotonicNowNs();

  const auto first = scheduler.schedule(
      oms::runtime::DeadlineType::FakeHeartbeat, now + 1'000'000ULL, 11);
  const auto second = scheduler.schedule(
      oms::runtime::DeadlineType::FutureReconcile, now + 1'000'000ULL, 22);
  const auto canceled = scheduler.schedule(
      oms::runtime::DeadlineType::GTD, now + 2'000'000ULL, 33);
  assert(first && second && canceled);
  assert(scheduler.earliest_deadline() == now + 1'000'000ULL);
  assert(!scheduler.schedule(oms::runtime::DeadlineType::FakeReconnect,
                             now + 3'000'000ULL));
  assert(scheduler.cancel(canceled.value));
  assert(!scheduler.cancel(canceled.value));

  const auto first_due = scheduler.pop_due(now + 1'000'000ULL);
  const auto second_due = scheduler.pop_due(now + 1'000'000ULL);
  assert(first_due && first_due.has_value && first_due.value.user_data == 11);
  assert(second_due && second_due.has_value &&
         second_due.value.user_data == 22);
  assert(scheduler.empty());

  const auto reused = scheduler.schedule(
      oms::runtime::DeadlineType::FutureListenKey, now + 4'000'000ULL);
  assert(reused);
  assert(!scheduler.cancel(canceled.value));
}

}  // namespace

int main() {
  TestEventNotifier();
  TestTimerNotifier();
  TestDeadlineScheduler();
}
