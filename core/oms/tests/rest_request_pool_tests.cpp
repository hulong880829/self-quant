#include "oms/exchange/rest_request_pool.h"

#include <atomic>
#ifdef NDEBUG
#undef NDEBUG
#endif
#include <cassert>
#include <cstddef>
#include <cstdlib>
#include <new>
#include <utility>

namespace {
std::atomic<bool> track_allocations{false};
std::atomic<std::size_t> allocation_count{0};
}  // namespace

void* operator new(std::size_t size) {
  if (track_allocations.load(std::memory_order_relaxed)) {
    allocation_count.fetch_add(1, std::memory_order_relaxed);
  }
  if (void* memory = std::malloc(size == 0 ? 1 : size); memory != nullptr) {
    return memory;
  }
  throw std::bad_alloc();
}

void* operator new[](std::size_t size) { return ::operator new(size); }
void operator delete(void* memory) noexcept { std::free(memory); }
void operator delete[](void* memory) noexcept { std::free(memory); }
void operator delete(void* memory, std::size_t) noexcept { std::free(memory); }
void operator delete[](void* memory, std::size_t) noexcept {
  std::free(memory);
}

int main() {
  using oms::exchange::RestRequestCorrelation;
  using oms::exchange::RestRequestHandle;
  using oms::exchange::RestRequestPool;

  RestRequestPool pool(2);
  RestRequestPool zero_capacity(0);
  allocation_count.store(0, std::memory_order_relaxed);
  track_allocations.store(true, std::memory_order_relaxed);

  assert(!zero_capacity.reserve({0, 0}, 1));
  assert(zero_capacity.capacity() == 0);
  assert(zero_capacity.empty());

  auto abandoned = pool.reserve({1, 11}, 100);
  assert(abandoned);
  const auto abandoned_handle = abandoned->handle();
  assert(pool.size() == 1);
  assert(pool.lookup(abandoned_handle) != nullptr);
  const RestRequestHandle malformed_handle{
      abandoned_handle.slot, 1, abandoned_handle.generation};
  assert(pool.lookup(malformed_handle) == nullptr);
  assert(!pool.cancel(malformed_handle));
  abandoned->cancel();
  assert(pool.empty());
  assert(pool.lookup(abandoned_handle) == nullptr);

  auto first = pool.reserve({2, 22}, 200);
  auto second = pool.reserve({3, 33}, 150);
  assert(first && second);
  const auto first_handle = first->handle();
  const auto second_handle = second->handle();
  assert(first_handle.generation != abandoned_handle.generation);
  assert(!pool.reserve({4, 44}, 300));
  assert(first->commit());
  assert(second->commit());
  assert(pool.size() == 2);
  assert(pool.lookup(first_handle)->correlation ==
         (RestRequestCorrelation{2, 22}));

  const auto expired = pool.pop_expired(175);
  assert(expired && expired->handle == second_handle);
  assert(expired->correlation == (RestRequestCorrelation{3, 33}));
  assert(!pool.cancel(second_handle));
  assert(pool.complete(first_handle));
  assert(!pool.complete(first_handle));
  assert(pool.empty());

  auto movable = pool.reserve({5, 55}, 400);
  assert(movable);
  const auto movable_handle = movable->handle();
  RestRequestPool::Lease moved = std::move(*movable);
  assert(!static_cast<bool>(*movable));
  assert(moved.commit());
  assert(pool.cancel(movable_handle));

  auto expires_while_reserved = pool.reserve({6, 66}, 500);
  assert(expires_while_reserved);
  const auto reserved_handle = expires_while_reserved->handle();
  assert(pool.pop_expired(500)->handle == reserved_handle);
  assert(!expires_while_reserved->commit());
  expires_while_reserved->cancel();

  {
    auto automatic = pool.reserve({7, 77}, 600);
    assert(automatic);
    assert(pool.size() == 1);
  }
  assert(pool.empty());

  auto move_target = pool.reserve({8, 88}, 700);
  auto move_source = pool.reserve({9, 99}, 700);
  assert(move_target && move_source);
  const auto released_by_move = move_target->handle();
  const auto retained_by_move = move_source->handle();
  *move_target = std::move(*move_source);
  assert(pool.lookup(released_by_move) == nullptr);
  assert(!static_cast<bool>(*move_source));
  assert(move_target->handle() == retained_by_move);
  assert(move_target->commit());
  assert(pool.size() == 1);
  assert(pool.complete(retained_by_move));

  auto later_slot = pool.reserve({10, 100}, 900);
  auto earlier_slot = pool.reserve({11, 110}, 800);
  assert(later_slot && earlier_slot);
  const auto later_handle = later_slot->handle();
  const auto earlier_handle = earlier_slot->handle();
  assert(later_slot->commit());
  assert(earlier_slot->commit());
  assert(pool.pop_expired(900)->handle == earlier_handle);
  assert(pool.pop_expired(900)->handle == later_handle);
  assert(!pool.pop_expired(900));

  assert(!pool.reserve({7, 77}, 0));
  assert(pool.empty());
  track_allocations.store(false, std::memory_order_relaxed);
  assert(allocation_count.load(std::memory_order_relaxed) == 0);
}
