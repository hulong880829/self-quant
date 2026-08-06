#pragma once

#include <array>
#include <atomic>
#include <bit>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <new>
#include <optional>
#include <type_traits>
#include <utility>

namespace utils::queue {

inline constexpr std::size_t kCacheLine = 64;

template <typename T, std::size_t Capacity>
class SpscRing {
  static_assert(Capacity >= 2 && std::has_single_bit(Capacity), "capacity must be power-of-two");
  struct Slot { alignas(T) std::byte storage[sizeof(T)]; };

 public:
  class ProducerLease {
   public:
    ProducerLease() = default;
    ProducerLease(const ProducerLease&) = delete;
    ProducerLease& operator=(const ProducerLease&) = delete;
    ProducerLease(ProducerLease&& other) noexcept { Move(other); }
    ProducerLease& operator=(ProducerLease&& other) noexcept {
      if (this != &other) { Cancel(); Move(other); }
      return *this;
    }
    ~ProducerLease() { Cancel(); }

    template <typename... Args>
    T& emplace(Args&&... args) noexcept(std::is_nothrow_constructible_v<T, Args...>) {
      if (constructed_) {
        std::destroy_at(ptr_);
        constructed_ = false;
      }
      std::construct_at(ptr_, std::forward<Args>(args)...);
      constructed_ = true;
      return *ptr_;
    }
    [[nodiscard]] T* get() noexcept { return constructed_ ? ptr_ : nullptr; }
    bool commit() noexcept {
      if (!ring_ || !constructed_) return false;
      ring_->producer_head_.value.store(sequence_ + 1, std::memory_order_release);
      ring_->producer_lease_active_ = false;
      ring_ = nullptr;
      ptr_ = nullptr;
      constructed_ = false;
      return true;
    }

   private:
    friend class SpscRing;
    ProducerLease(SpscRing* ring, T* ptr, std::uint64_t sequence) noexcept
        : ring_(ring), ptr_(ptr), sequence_(sequence) {}
    void Cancel() noexcept {
      if (constructed_) std::destroy_at(ptr_);
      if (ring_) ring_->producer_lease_active_ = false;
      ring_ = nullptr; ptr_ = nullptr; constructed_ = false;
    }
    void Move(ProducerLease& other) noexcept {
      ring_ = other.ring_; ptr_ = other.ptr_; sequence_ = other.sequence_;
      constructed_ = other.constructed_;
      other.ring_ = nullptr; other.ptr_ = nullptr; other.constructed_ = false;
    }
    SpscRing* ring_{};
    T* ptr_{};
    std::uint64_t sequence_{};
    bool constructed_{};
  };

  class ConsumerLease {
   public:
    ConsumerLease() = default;
    ConsumerLease(const ConsumerLease&) = delete;
    ConsumerLease& operator=(const ConsumerLease&) = delete;
    ConsumerLease(ConsumerLease&& other) noexcept { Move(other); }
    ConsumerLease& operator=(ConsumerLease&& other) noexcept {
      if (this != &other) { release(); Move(other); }
      return *this;
    }
    ~ConsumerLease() { release(); }
    [[nodiscard]] const T& operator*() const noexcept { return *ptr_; }
    [[nodiscard]] const T* operator->() const noexcept { return ptr_; }
    [[nodiscard]] const T* get() const noexcept { return ptr_; }
    void release() noexcept {
      if (!ring_) return;
      std::destroy_at(ptr_);
      ring_->consumer_tail_.value.store(sequence_ + 1, std::memory_order_release);
      ring_->consumer_lease_active_ = false;
      ring_ = nullptr; ptr_ = nullptr;
    }

   private:
    friend class SpscRing;
    ConsumerLease(SpscRing* ring, T* ptr, std::uint64_t sequence) noexcept
        : ring_(ring), ptr_(ptr), sequence_(sequence) {}
    void Move(ConsumerLease& other) noexcept {
      ring_ = other.ring_; ptr_ = other.ptr_; sequence_ = other.sequence_;
      other.ring_ = nullptr; other.ptr_ = nullptr;
    }
    SpscRing* ring_{};
    T* ptr_{};
    std::uint64_t sequence_{};
  };

  ~SpscRing() {
    while (auto lease = try_peek()) lease->release();
  }
  [[nodiscard]] std::optional<ProducerLease> try_reserve() noexcept {
    if (producer_lease_active_) return std::nullopt;
    const auto head = producer_head_.value.load(std::memory_order_relaxed);
    if (head - producer_cached_tail_ == Capacity) {
      producer_cached_tail_ = consumer_tail_.value.load(std::memory_order_acquire);
      if (head - producer_cached_tail_ == Capacity) return std::nullopt;
    }
    producer_lease_active_ = true;
    return ProducerLease(this, Ptr(head), head);
  }
  [[nodiscard]] std::optional<ConsumerLease> try_peek() noexcept {
    if (consumer_lease_active_) return std::nullopt;
    const auto tail = consumer_tail_.value.load(std::memory_order_relaxed);
    if (tail == consumer_cached_head_) {
      consumer_cached_head_ = producer_head_.value.load(std::memory_order_acquire);
      if (tail == consumer_cached_head_) return std::nullopt;
    }
    consumer_lease_active_ = true;
    return ConsumerLease(this, Ptr(tail), tail);
  }
  [[nodiscard]] std::size_t capacity() const noexcept { return Capacity; }

 private:
  T* Ptr(std::uint64_t sequence) noexcept {
    return std::launder(reinterpret_cast<T*>(slots_[sequence & (Capacity - 1)].storage));
  }
  struct alignas(kCacheLine) Cursor { std::atomic<std::uint64_t> value{0}; };
  std::array<Slot, Capacity> slots_{};
  Cursor producer_head_{};
  std::uint64_t producer_cached_tail_{};
  bool producer_lease_active_{};
  Cursor consumer_tail_{};
  std::uint64_t consumer_cached_head_{};
  bool consumer_lease_active_{};
};

template <typename T, std::size_t Capacity>
class BoundedMpscQueue {
  static_assert(Capacity >= 2 && std::has_single_bit(Capacity), "capacity must be power-of-two");
  static_assert(std::is_nothrow_destructible_v<T>, "T must be nothrow destructible");
  struct Cell {
    std::atomic<std::uint64_t> sequence{0};
    alignas(T) std::byte storage[sizeof(T)];
  };

 public:
  BoundedMpscQueue() noexcept {
    for (std::size_t i = 0; i < Capacity; ++i) cells_[i].sequence.store(i, std::memory_order_relaxed);
  }
  ~BoundedMpscQueue() {
    auto position = dequeue_.load(std::memory_order_relaxed);
    for (;;) {
      Cell& cell = cells_[position & (Capacity - 1)];
      if (cell.sequence.load(std::memory_order_acquire) != position + 1) break;
      std::destroy_at(std::launder(reinterpret_cast<T*>(cell.storage)));
      ++position;
    }
  }

  template <typename... Args>
    requires std::is_nothrow_constructible_v<T, Args...>
  bool try_emplace(Args&&... args) noexcept {
    std::uint64_t position = enqueue_.load(std::memory_order_relaxed);
    Cell* cell;
    for (;;) {
      cell = &cells_[position & (Capacity - 1)];
      const auto sequence = cell->sequence.load(std::memory_order_acquire);
      const auto difference = static_cast<std::int64_t>(sequence) - static_cast<std::int64_t>(position);
      if (difference == 0) {
        if (enqueue_.compare_exchange_weak(position, position + 1, std::memory_order_relaxed)) break;
      } else if (difference < 0) {
        return false;
      } else {
        position = enqueue_.load(std::memory_order_relaxed);
      }
    }
    std::construct_at(std::launder(reinterpret_cast<T*>(cell->storage)), std::forward<Args>(args)...);
    cell->sequence.store(position + 1, std::memory_order_release);
    return true;
  }
  bool try_dequeue(T& output) noexcept
    requires std::is_nothrow_move_assignable_v<T>
  {
    const auto position = dequeue_.load(std::memory_order_relaxed);
    Cell& cell = cells_[position & (Capacity - 1)];
    if (cell.sequence.load(std::memory_order_acquire) != position + 1) return false;
    T* value = std::launder(reinterpret_cast<T*>(cell.storage));
    output = std::move(*value);
    std::destroy_at(value);
    cell.sequence.store(position + Capacity, std::memory_order_release);
    dequeue_.store(position + 1, std::memory_order_relaxed);
    return true;
  }

 private:
  std::array<Cell, Capacity> cells_{};
  alignas(kCacheLine) std::atomic<std::uint64_t> enqueue_{0};
  alignas(kCacheLine) std::atomic<std::uint64_t> dequeue_{0};
};

}  // namespace utils::queue
