#include "oms/runtime/deadline_scheduler.h"

#include <limits>
#include <stdexcept>

namespace oms::runtime {
namespace {

std::size_t CheckedCapacity(std::size_t capacity) {
  if (capacity > std::numeric_limits<std::uint32_t>::max()) {
    throw std::length_error("deadline scheduler capacity exceeds handle range");
  }
  return capacity;
}

}  // namespace

DeadlineScheduler::DeadlineScheduler(std::size_t capacity,
                                     TimerNotifier& timer)
    : timer_(timer),
      nodes_(CheckedCapacity(capacity)),
      heap_(CheckedCapacity(capacity)),
      free_slots_(CheckedCapacity(capacity)),
      free_count_(capacity) {
  for (std::size_t i = 0; i < capacity; ++i) {
    free_slots_[i] = static_cast<std::uint32_t>(capacity - i - 1);
  }
}

DeadlineResult<DeadlineHandle> DeadlineScheduler::schedule(
    DeadlineType type, std::uint64_t monotonic_ns,
    std::uint64_t user_data) noexcept {
  if (free_count_ == 0) {
    return {{}, DeadlineError::CapacityExceeded, 0};
  }
  if (next_sequence_ == std::numeric_limits<std::uint64_t>::max()) {
    return {{}, DeadlineError::SequenceExhausted, 0};
  }

  const std::uint32_t slot = free_slots_[--free_count_];
  Node& node = nodes_[slot];
  ++node.generation;
  if (node.generation == 0) {
    ++node.generation;
  }

  const DeadlineHandle handle{slot, 0, node.generation};
  node.deadline = Deadline{type, monotonic_ns, user_data, handle};
  node.sequence = next_sequence_++;
  node.heap_index = static_cast<std::uint32_t>(heap_size_);
  node.active = true;
  heap_[heap_size_++] = slot;
  sift_up(node.heap_index);

  const int timer_error = sync_timer();
  if (timer_error != 0) {
    return {handle, DeadlineError::TimerFailure, timer_error};
  }
  return {handle, DeadlineError::Ok, 0};
}

DeadlineResult<void> DeadlineScheduler::cancel(
    DeadlineHandle handle) noexcept {
  if (handle.slot >= nodes_.size()) {
    return {DeadlineError::InvalidHandle, 0};
  }
  Node& node = nodes_[handle.slot];
  if (!node.active || node.generation != handle.generation) {
    return {DeadlineError::InvalidHandle, 0};
  }

  remove_at(node.heap_index);
  return sync_result();
}

PopDueResult DeadlineScheduler::pop_due(
    std::uint64_t now_monotonic_ns) noexcept {
  if (heap_size_ == 0 ||
      nodes_[heap_[0]].deadline.monotonic_ns > now_monotonic_ns) {
    return {};
  }

  const Deadline deadline = nodes_[heap_[0]].deadline;
  remove_at(0);
  const DeadlineResult<void> synced = sync_result();
  return {deadline, true, synced.error, synced.system_error};
}

std::optional<std::uint64_t> DeadlineScheduler::earliest_deadline()
    const noexcept {
  if (heap_size_ == 0) {
    return std::nullopt;
  }
  return nodes_[heap_[0]].deadline.monotonic_ns;
}

bool DeadlineScheduler::earlier(std::uint32_t lhs_slot,
                                std::uint32_t rhs_slot) const noexcept {
  const Node& lhs = nodes_[lhs_slot];
  const Node& rhs = nodes_[rhs_slot];
  if (lhs.deadline.monotonic_ns != rhs.deadline.monotonic_ns) {
    return lhs.deadline.monotonic_ns < rhs.deadline.monotonic_ns;
  }
  return lhs.sequence < rhs.sequence;
}

void DeadlineScheduler::swap_heap(std::size_t lhs, std::size_t rhs) noexcept {
  const std::uint32_t lhs_slot = heap_[lhs];
  const std::uint32_t rhs_slot = heap_[rhs];
  heap_[lhs] = rhs_slot;
  heap_[rhs] = lhs_slot;
  nodes_[rhs_slot].heap_index = static_cast<std::uint32_t>(lhs);
  nodes_[lhs_slot].heap_index = static_cast<std::uint32_t>(rhs);
}

void DeadlineScheduler::sift_up(std::size_t index) noexcept {
  while (index != 0) {
    const std::size_t parent = (index - 1) / 2;
    if (!earlier(heap_[index], heap_[parent])) {
      return;
    }
    swap_heap(index, parent);
    index = parent;
  }
}

void DeadlineScheduler::sift_down(std::size_t index) noexcept {
  for (;;) {
    const std::size_t left = index * 2 + 1;
    if (left >= heap_size_) {
      return;
    }
    const std::size_t right = left + 1;
    std::size_t child = left;
    if (right < heap_size_ && earlier(heap_[right], heap_[left])) {
      child = right;
    }
    if (!earlier(heap_[child], heap_[index])) {
      return;
    }
    swap_heap(index, child);
    index = child;
  }
}

void DeadlineScheduler::remove_at(std::size_t index) noexcept {
  const std::uint32_t removed_slot = heap_[index];
  const std::size_t last = heap_size_ - 1;
  if (index != last) {
    swap_heap(index, last);
  }
  --heap_size_;

  Node& removed = nodes_[removed_slot];
  removed.active = false;
  removed.heap_index = kNotInHeap;
  free_slots_[free_count_++] = removed_slot;

  if (index >= heap_size_) {
    return;
  }
  if (index != 0 && earlier(heap_[index], heap_[(index - 1) / 2])) {
    sift_up(index);
  } else {
    sift_down(index);
  }
}

int DeadlineScheduler::sync_timer() noexcept {
  const std::optional<std::uint64_t> desired = earliest_deadline();
  if (desired == armed_deadline_) {
    return 0;
  }

  const int error = desired ? timer_.arm(*desired) : timer_.disarm();
  if (error == 0) {
    armed_deadline_ = desired;
  }
  return error;
}

DeadlineResult<void> DeadlineScheduler::sync_result() noexcept {
  const int error = sync_timer();
  if (error != 0) {
    return {DeadlineError::TimerFailure, error};
  }
  return {};
}

}  // namespace oms::runtime
