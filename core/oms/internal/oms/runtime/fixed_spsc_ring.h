#pragma once

#include <atomic>
#include <bit>
#include <cstddef>
#include <cstdint>
#include <limits>
#include <memory>
#include <new>
#include <optional>
#include <stdexcept>
#include <type_traits>
#include <utility>

namespace oms::runtime {

template <typename T>
class FixedSpscRing {
  static_assert(std::is_nothrow_destructible_v<T>,
                "ring elements must be nothrow destructible");

  struct Slot {
    alignas(T) std::byte storage[sizeof(T)];
  };

 public:
  class ProducerLease {
   public:
    ProducerLease() noexcept = default;
    ProducerLease(const ProducerLease&) = delete;
    ProducerLease& operator=(const ProducerLease&) = delete;

    ProducerLease(ProducerLease&& other) noexcept { MoveFrom(other); }

    ProducerLease& operator=(ProducerLease&& other) noexcept {
      if (this != &other) {
        cancel();
        MoveFrom(other);
      }
      return *this;
    }

    ~ProducerLease() { cancel(); }

    template <typename... Args>
    T& emplace(Args&&... args) noexcept(
        std::is_nothrow_constructible_v<T, Args...>) {
      if (constructed_) {
        std::destroy_at(value_);
        constructed_ = false;
      }
      std::construct_at(value_, std::forward<Args>(args)...);
      constructed_ = true;
      return *value_;
    }

    [[nodiscard]] T* get() noexcept {
      return constructed_ ? value_ : nullptr;
    }

    [[nodiscard]] const T* get() const noexcept {
      return constructed_ ? value_ : nullptr;
    }

    [[nodiscard]] bool commit() noexcept {
      if (ring_ == nullptr || !constructed_) return false;
      ring_->Commit(sequence_);
      ring_ = nullptr;
      value_ = nullptr;
      constructed_ = false;
      return true;
    }

    void cancel() noexcept {
      if (constructed_) std::destroy_at(value_);
      if (ring_ != nullptr) ring_->producer_.lease_active = false;
      ring_ = nullptr;
      value_ = nullptr;
      constructed_ = false;
    }

   private:
    friend class FixedSpscRing;

    ProducerLease(FixedSpscRing* ring, T* value,
                  std::uint64_t sequence) noexcept
        : ring_(ring), value_(value), sequence_(sequence) {}

    void MoveFrom(ProducerLease& other) noexcept {
      ring_ = other.ring_;
      value_ = other.value_;
      sequence_ = other.sequence_;
      constructed_ = other.constructed_;
      other.ring_ = nullptr;
      other.value_ = nullptr;
      other.constructed_ = false;
    }

    FixedSpscRing* ring_{};
    T* value_{};
    std::uint64_t sequence_{};
    bool constructed_{};
  };

  class ConsumerLease {
   public:
    ConsumerLease() noexcept = default;
    ConsumerLease(const ConsumerLease&) = delete;
    ConsumerLease& operator=(const ConsumerLease&) = delete;

    ConsumerLease(ConsumerLease&& other) noexcept { MoveFrom(other); }

    ConsumerLease& operator=(ConsumerLease&& other) noexcept {
      if (this != &other) {
        release();
        MoveFrom(other);
      }
      return *this;
    }

    ~ConsumerLease() { release(); }

    [[nodiscard]] const T& operator*() const noexcept { return *value_; }
    [[nodiscard]] const T* operator->() const noexcept { return value_; }
    [[nodiscard]] const T* get() const noexcept { return value_; }

    void release() noexcept {
      if (ring_ == nullptr) return;
      std::destroy_at(value_);
      ring_->consumer_.tail.store(sequence_ + 1, std::memory_order_release);
      ring_->consumer_.lease_active = false;
      ring_ = nullptr;
      value_ = nullptr;
    }

    // Abandons the peek without consuming the element. This is used by a
    // backpressured owner so the command remains at the FIFO head.
    void cancel() noexcept {
      if (ring_ == nullptr) return;
      ring_->consumer_.lease_active = false;
      ring_ = nullptr;
      value_ = nullptr;
    }

   private:
    friend class FixedSpscRing;

    ConsumerLease(FixedSpscRing* ring, T* value,
                  std::uint64_t sequence) noexcept
        : ring_(ring), value_(value), sequence_(sequence) {}

    void MoveFrom(ConsumerLease& other) noexcept {
      ring_ = other.ring_;
      value_ = other.value_;
      sequence_ = other.sequence_;
      other.ring_ = nullptr;
      other.value_ = nullptr;
    }

    FixedSpscRing* ring_{};
    T* value_{};
    std::uint64_t sequence_{};
  };

  explicit FixedSpscRing(std::size_t capacity)
      : capacity_(ValidateCapacity(capacity)),
        mask_(capacity_ - 1),
        slots_(std::make_unique<Slot[]>(capacity_)) {}

  ~FixedSpscRing() {
    const auto tail = consumer_.tail.load(std::memory_order_relaxed);
    const auto head = producer_.head.load(std::memory_order_relaxed);
    for (auto sequence = tail; sequence != head; ++sequence) {
      std::destroy_at(Ptr(sequence));
    }
  }

  FixedSpscRing(const FixedSpscRing&) = delete;
  FixedSpscRing& operator=(const FixedSpscRing&) = delete;
  FixedSpscRing(FixedSpscRing&&) = delete;
  FixedSpscRing& operator=(FixedSpscRing&&) = delete;

  [[nodiscard]] std::optional<ProducerLease> try_reserve() noexcept {
    if (producer_.lease_active) return std::nullopt;

    const auto head = producer_.head.load(std::memory_order_relaxed);
    if (head - producer_.cached_tail >= capacity_) {
      producer_.cached_tail =
          consumer_.tail.load(std::memory_order_acquire);
      if (head - producer_.cached_tail >= capacity_) return std::nullopt;
    }

    producer_.lease_active = true;
    return ProducerLease(this, Ptr(head), head);
  }

  [[nodiscard]] std::optional<ConsumerLease> try_peek() noexcept {
    if (consumer_.lease_active) return std::nullopt;

    const auto tail = consumer_.tail.load(std::memory_order_relaxed);
    if (tail == consumer_.cached_head) {
      consumer_.cached_head =
          producer_.head.load(std::memory_order_acquire);
      if (tail == consumer_.cached_head) return std::nullopt;
    }

    consumer_.lease_active = true;
    return ConsumerLease(this, Ptr(tail), tail);
  }

  [[nodiscard]] std::size_t capacity() const noexcept { return capacity_; }

  [[nodiscard]] std::size_t depth() const noexcept {
    // Tail-first prevents an observer from combining an old head with a newer
    // tail and reporting an underflowed depth.
    const auto tail = consumer_.tail.load(std::memory_order_acquire);
    const auto head = producer_.head.load(std::memory_order_acquire);
    return static_cast<std::size_t>(head - tail);
  }

  [[nodiscard]] bool empty() const noexcept { return depth() == 0; }
  [[nodiscard]] bool full() const noexcept { return depth() == capacity_; }

  [[nodiscard]] std::size_t high_water() const noexcept {
    return static_cast<std::size_t>(
        producer_.high_water.load(std::memory_order_relaxed));
  }

 private:
  static std::size_t ValidateCapacity(std::size_t capacity) {
    constexpr auto kMaximum =
        std::numeric_limits<std::size_t>::max() / sizeof(Slot);
    if (capacity < 2 || !std::has_single_bit(capacity) ||
        capacity > kMaximum) {
      throw std::invalid_argument(
          "FixedSpscRing capacity must be a representable power of two >= 2");
    }
    return capacity;
  }

  [[nodiscard]] T* Ptr(std::uint64_t sequence) noexcept {
    const auto index = static_cast<std::size_t>(sequence) & mask_;
    return std::launder(reinterpret_cast<T*>(slots_[index].storage));
  }

  void Commit(std::uint64_t sequence) noexcept {
    const auto next = sequence + 1;
    const auto tail = consumer_.tail.load(std::memory_order_acquire);
    const auto new_depth = next - tail;
    const auto previous =
        producer_.high_water.load(std::memory_order_relaxed);
    if (new_depth > previous) {
      producer_.high_water.store(new_depth, std::memory_order_relaxed);
    }
    producer_.head.store(next, std::memory_order_release);
    producer_.lease_active = false;
  }

  static constexpr std::size_t kCacheLine = 64;

  struct alignas(kCacheLine) ProducerState {
    std::atomic<std::uint64_t> head{0};
    std::atomic<std::uint64_t> high_water{0};
    std::uint64_t cached_tail{};
    bool lease_active{};
  };

  struct alignas(kCacheLine) ConsumerState {
    std::atomic<std::uint64_t> tail{0};
    std::uint64_t cached_head{};
    bool lease_active{};
  };

  const std::size_t capacity_;
  const std::size_t mask_;
  std::unique_ptr<Slot[]> slots_;
  ProducerState producer_{};
  ConsumerState consumer_{};
};

}  // namespace oms::runtime
