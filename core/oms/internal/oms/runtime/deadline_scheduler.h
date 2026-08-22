#pragma once

#include <cstddef>
#include <cstdint>
#include <limits>
#include <optional>
#include <vector>

#include "oms/runtime/timer_notifier.h"

namespace oms::runtime {

enum class DeadlineType : std::uint8_t {
  GTD = 1,
  FakeHeartbeat = 2,
  FakeReconnect = 3,
  FutureListenKey = 4,
  FutureReconcile = 5,
};

struct DeadlineHandle {
  std::uint32_t slot{};
  std::uint32_t reserved{};
  std::uint64_t generation{};

  friend constexpr bool operator==(const DeadlineHandle&,
                                   const DeadlineHandle&) = default;
};

struct Deadline {
  DeadlineType type{DeadlineType::GTD};
  std::uint64_t monotonic_ns{};
  std::uint64_t user_data{};
  DeadlineHandle handle{};
};

enum class DeadlineError : std::uint8_t {
  Ok = 0,
  CapacityExceeded = 1,
  InvalidHandle = 2,
  SequenceExhausted = 3,
  TimerFailure = 4,
};

template <typename T>
struct DeadlineResult {
  T value{};
  DeadlineError error{DeadlineError::Ok};
  int system_error{};

  [[nodiscard]] constexpr explicit operator bool() const noexcept {
    return error == DeadlineError::Ok;
  }
};

template <>
struct DeadlineResult<void> {
  DeadlineError error{DeadlineError::Ok};
  int system_error{};

  [[nodiscard]] constexpr explicit operator bool() const noexcept {
    return error == DeadlineError::Ok;
  }
};

struct PopDueResult {
  Deadline value{};
  bool has_value{};
  DeadlineError error{DeadlineError::Ok};
  int system_error{};

  [[nodiscard]] constexpr explicit operator bool() const noexcept {
    return error == DeadlineError::Ok;
  }
};

class DeadlineScheduler {
 public:
  // timer must outlive the scheduler and should be dedicated to it.
  DeadlineScheduler(std::size_t capacity, TimerNotifier& timer);
  ~DeadlineScheduler() = default;

  DeadlineScheduler(const DeadlineScheduler&) = delete;
  DeadlineScheduler& operator=(const DeadlineScheduler&) = delete;
  DeadlineScheduler(DeadlineScheduler&&) = delete;
  DeadlineScheduler& operator=(DeadlineScheduler&&) = delete;

  [[nodiscard]] std::size_t capacity() const noexcept {
    return nodes_.size();
  }
  [[nodiscard]] std::size_t size() const noexcept { return heap_size_; }
  [[nodiscard]] bool empty() const noexcept { return heap_size_ == 0; }
  [[nodiscard]] int timer_fd() const noexcept { return timer_.fd(); }

  // A TimerFailure reports a failed re-arm after the requested mutation has
  // succeeded. schedule() returns the live handle in that case.
  [[nodiscard]] DeadlineResult<DeadlineHandle> schedule(
      DeadlineType type, std::uint64_t monotonic_ns,
      std::uint64_t user_data = 0) noexcept;
  [[nodiscard]] DeadlineResult<void> cancel(DeadlineHandle handle) noexcept;
  [[nodiscard]] PopDueResult pop_due(std::uint64_t now_monotonic_ns) noexcept;
  [[nodiscard]] std::optional<std::uint64_t> earliest_deadline()
      const noexcept;

 private:
  static constexpr std::uint32_t kNotInHeap =
      std::numeric_limits<std::uint32_t>::max();

  struct Node {
    Deadline deadline{};
    std::uint64_t sequence{};
    std::uint32_t heap_index{kNotInHeap};
    std::uint64_t generation{};
    bool active{};
  };

  [[nodiscard]] bool earlier(std::uint32_t lhs_slot,
                             std::uint32_t rhs_slot) const noexcept;
  void swap_heap(std::size_t lhs, std::size_t rhs) noexcept;
  void sift_up(std::size_t index) noexcept;
  void sift_down(std::size_t index) noexcept;
  void remove_at(std::size_t index) noexcept;
  [[nodiscard]] int sync_timer() noexcept;
  [[nodiscard]] DeadlineResult<void> sync_result() noexcept;

  TimerNotifier& timer_;
  std::vector<Node> nodes_;
  std::vector<std::uint32_t> heap_;
  std::vector<std::uint32_t> free_slots_;
  std::size_t heap_size_{};
  std::size_t free_count_{};
  std::uint64_t next_sequence_{};
  std::optional<std::uint64_t> armed_deadline_{};
};

}  // namespace oms::runtime
