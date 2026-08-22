#include "oms/exchange/rest_request_pool.h"

#include <limits>
#include <stdexcept>
#include <utility>

namespace oms::exchange {
namespace {

std::size_t CheckedCapacity(std::size_t capacity) {
  if (capacity > std::numeric_limits<std::uint32_t>::max()) {
    throw std::length_error("REST request pool capacity exceeds handle range");
  }
  return capacity;
}

}  // namespace

RestRequestPool::Lease::Lease(Lease&& other) noexcept { move_from(other); }

RestRequestPool::Lease& RestRequestPool::Lease::operator=(
    Lease&& other) noexcept {
  if (this != &other) {
    cancel();
    move_from(other);
  }
  return *this;
}

RestRequestPool::Lease::~Lease() { cancel(); }

bool RestRequestPool::Lease::commit() noexcept {
  if (pool_ == nullptr || !pool_->commit_reservation(handle_)) {
    return false;
  }
  pool_ = nullptr;
  return true;
}

void RestRequestPool::Lease::cancel() noexcept {
  if (pool_ != nullptr) {
    (void)pool_->release(handle_);
    pool_ = nullptr;
  }
}

void RestRequestPool::Lease::move_from(Lease& other) noexcept {
  pool_ = other.pool_;
  handle_ = other.handle_;
  other.pool_ = nullptr;
  other.handle_ = {};
}

RestRequestPool::RestRequestPool(std::size_t capacity)
    : slots_(CheckedCapacity(capacity)),
      free_slots_(CheckedCapacity(capacity)),
      free_count_(capacity) {
  for (std::size_t index = 0; index < capacity; ++index) {
    free_slots_[index] = static_cast<std::uint32_t>(capacity - index - 1);
  }
}

std::optional<RestRequestPool::Lease> RestRequestPool::reserve(
    RestRequestCorrelation correlation, std::uint64_t deadline_ns) noexcept {
  if (free_count_ == 0 || deadline_ns == 0) {
    return std::nullopt;
  }
  const std::uint32_t slot_index = free_slots_[--free_count_];
  Slot& slot = slots_[slot_index];
  ++slot.generation;
  if (slot.generation == 0) {
    ++slot.generation;
  }
  const RestRequestHandle handle{slot_index, 0, slot.generation};
  slot.request = {handle, correlation, deadline_ns};
  slot.state = SlotState::Reserved;
  ++active_count_;
  return Lease(this, handle);
}

bool RestRequestPool::cancel(RestRequestHandle handle) noexcept {
  return release(handle);
}

bool RestRequestPool::complete(RestRequestHandle handle) noexcept {
  return release(handle);
}

const RestRequest* RestRequestPool::lookup(RestRequestHandle handle) const
    noexcept {
  return matches(handle) ? &slots_[handle.slot].request : nullptr;
}

std::optional<RestRequest> RestRequestPool::pop_expired(
    std::uint64_t now_ns) noexcept {
  std::optional<std::uint32_t> expired_slot;
  for (std::uint32_t index = 0; index < slots_.size(); ++index) {
    const Slot& slot = slots_[index];
    if (slot.state == SlotState::Free || slot.request.deadline_ns > now_ns) {
      continue;
    }
    if (!expired_slot ||
        slot.request.deadline_ns <
            slots_[*expired_slot].request.deadline_ns ||
        (slot.request.deadline_ns ==
             slots_[*expired_slot].request.deadline_ns &&
         index < *expired_slot)) {
      expired_slot = index;
    }
  }
  if (!expired_slot) {
    return std::nullopt;
  }
  const RestRequest request = slots_[*expired_slot].request;
  (void)release(request.handle);
  return request;
}

bool RestRequestPool::commit_reservation(RestRequestHandle handle) noexcept {
  if (!matches(handle)) {
    return false;
  }
  Slot& slot = slots_[handle.slot];
  if (slot.state != SlotState::Reserved) {
    return false;
  }
  slot.state = SlotState::Active;
  return true;
}

bool RestRequestPool::release(RestRequestHandle handle) noexcept {
  if (!matches(handle)) {
    return false;
  }
  Slot& slot = slots_[handle.slot];
  slot.request = {};
  slot.state = SlotState::Free;
  free_slots_[free_count_++] = handle.slot;
  --active_count_;
  return true;
}

bool RestRequestPool::matches(RestRequestHandle handle) const noexcept {
  if (!handle || handle.reserved != 0 || handle.slot >= slots_.size()) {
    return false;
  }
  const Slot& slot = slots_[handle.slot];
  return slot.state != SlotState::Free &&
         slot.generation == handle.generation;
}

}  // namespace oms::exchange
