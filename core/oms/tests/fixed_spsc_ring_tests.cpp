#include <atomic>
#include <cassert>
#include <cstddef>
#include <cstdint>
#include <cstdlib>
#include <new>
#include <stdexcept>
#include <thread>

#include "oms/api/runtime_types.h"
#include "oms/runtime/fixed_spsc_ring.h"
#include "oms/runtime/runtime_command.h"

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
  for (const std::size_t invalid : {0U, 1U, 3U, 6U}) {
    bool rejected = false;
    try {
      oms::runtime::FixedSpscRing<std::uint64_t> ring(invalid);
    } catch (const std::invalid_argument&) {
      rejected = true;
    }
    assert(rejected);
  }

  oms::runtime::FixedSpscRing<std::uint64_t> ring(4);
  allocation_count.store(0, std::memory_order_relaxed);
  track_allocations.store(true, std::memory_order_relaxed);

  auto canceled = ring.try_reserve();
  assert(canceled.has_value());
  assert(!ring.try_reserve().has_value());
  canceled->emplace(99);
  canceled->cancel();
  assert(ring.empty());

  for (std::uint64_t value = 1; value <= 4; ++value) {
    auto producer = ring.try_reserve();
    assert(producer.has_value());
    producer->emplace(value);
    assert(producer->commit());
  }
  assert(ring.full());
  assert(ring.depth() == 4);
  assert(ring.high_water() == 4);
  assert(!ring.try_reserve().has_value());

  for (std::uint64_t expected = 1; expected <= 2; ++expected) {
    auto consumer = ring.try_peek();
    assert(consumer.has_value());
    assert(!ring.try_peek().has_value());
    assert(**consumer == expected);
    if (expected == 1) {
      consumer->cancel();
      consumer = ring.try_peek();
      assert(consumer.has_value());
      assert(**consumer == expected);
    }
    consumer->release();
  }

  for (std::uint64_t value = 5; value <= 6; ++value) {
    auto producer = ring.try_reserve();
    assert(producer.has_value());
    producer->emplace(value);
    assert(producer->commit());
  }

  for (std::uint64_t expected = 3; expected <= 6; ++expected) {
    auto consumer = ring.try_peek();
    assert(consumer.has_value());
    assert(**consumer == expected);
    consumer->release();
  }

  assert(ring.empty());
  track_allocations.store(false, std::memory_order_relaxed);
  assert(allocation_count.load(std::memory_order_relaxed) == 0);

  oms::runtime::FixedSpscRing<std::uint64_t> concurrent(1024);
  constexpr std::uint64_t kTransferCount = 250'000;
  std::thread producer([&] {
    for (std::uint64_t value = 0; value < kTransferCount;) {
      auto lease = concurrent.try_reserve();
      if (!lease.has_value()) {
        std::this_thread::yield();
        continue;
      }
      lease->emplace(value++);
      assert(lease->commit());
    }
  });
  std::thread consumer([&] {
    for (std::uint64_t expected = 0; expected < kTransferCount;) {
      auto lease = concurrent.try_peek();
      if (!lease.has_value()) {
        std::this_thread::yield();
        continue;
      }
      assert(**lease == expected++);
      lease->release();
    }
  });
  producer.join();
  consumer.join();
  assert(concurrent.empty());
  assert(concurrent.high_water() <= concurrent.capacity());

  // Ensure the concrete runtime payload types are usable as ring elements.
  oms::runtime::FixedSpscRing<oms::runtime::RuntimeCommand> commands(2);
  auto command = commands.try_reserve();
  assert(command.has_value());
  command->emplace();
  assert(command->commit());

  return 0;
}
