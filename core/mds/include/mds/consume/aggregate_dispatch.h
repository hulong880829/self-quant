#pragma once

#include "utils/md/wire.h"

#include <array>
#include <atomic>
#include <bit>
#include <cstddef>
#include <cstdint>
#include <string_view>
#include <type_traits>

namespace mds::consume {

enum class AggregateTopic : std::uint8_t {
  AggBbo,
  AggOrderBook,
  Unsupported,
};

enum class AggregateWatchdogAction : std::uint8_t {
  None,
  Stale,
  HardReset,
};

class AggregateWatchdog {
 public:
  AggregateWatchdog(std::uint64_t stale_after_ns = 5'000'000'000ULL,
                    std::uint64_t hard_reset_after_ns =
                        10'000'000'000ULL) noexcept
      : stale_after_ns_(stale_after_ns),
        hard_reset_after_ns_(hard_reset_after_ns) {}

  void on_ready(std::uint64_t receive_mono_ns) noexcept;
  void on_hard_reset() noexcept;
  [[nodiscard]] AggregateWatchdogAction poll(std::uint64_t now_ns) noexcept;
  [[nodiscard]] bool started() const noexcept { return started_; }
  [[nodiscard]] bool stale() const noexcept { return stale_; }
  [[nodiscard]] bool hard_reset_latched() const noexcept {
    return hard_reset_latched_;
  }

 private:
  std::uint64_t stale_after_ns_{};
  std::uint64_t hard_reset_after_ns_{};
  std::uint64_t last_ready_mono_ns_{};
  bool started_{};
  bool stale_{};
  bool hard_reset_latched_{};
};

struct AggregateReceiveInfo {
  std::uint64_t ring_epoch{};
  std::uint64_t ring_sequence{};
  std::uint64_t receive_mono_ns{};
  std::uint64_t receive_wall_ns{};
};

struct TopicStatus {
  AggregateReceiveInfo receive{};
  std::uint64_t generation{};
  std::uint64_t reset_generation{};
  bool ready{};
  std::uint8_t reserved[7]{};
};

struct CrossBpsWindow {
  std::uint64_t start_mono_ns{};
  std::uint64_t end_mono_ns{};
  std::int32_t raw_min{};
  std::int32_t raw_max{};
  std::int32_t gated_min{};
  std::int32_t gated_max{};
  std::uint64_t sample_count{};
};

struct alignas(8) AggBboSnapshot {
  utils::md::wire::AggBboRecord record{};
  TopicStatus status{};
  CrossBpsWindow cross_window{};
};

struct alignas(8) AggOrderBookSnapshot {
  utils::md::wire::AggOrderBookRecord record{};
  TopicStatus status{};
};

struct alignas(8) AggBboWindowSnapshot {
  TopicStatus status{};
  CrossBpsWindow cross_window{};
  std::uint64_t rollover_request{};
};

template <typename Value>
class AtomicDoubleBuffer {
 public:
  static_assert(std::is_trivially_copyable_v<Value>);
  static_assert(sizeof(Value) % sizeof(std::uint64_t) == 0);
  static_assert(std::atomic<std::uint64_t>::is_always_lock_free);

  AtomicDoubleBuffer() noexcept = default;
  AtomicDoubleBuffer(const AtomicDoubleBuffer &) = delete;
  AtomicDoubleBuffer &operator=(const AtomicDoubleBuffer &) = delete;

  // Single-writer only. Readers may call snapshot concurrently.
  [[nodiscard]] std::uint64_t publish(Value &value) noexcept {
    const auto generation = ++writer_generation_;
    value.status.generation = generation;
    if (!value.status.ready) {
      value.status.reset_generation = generation;
    }
    const auto slot = generation & 1U;
    const auto words =
        std::bit_cast<std::array<std::uint64_t, kWordCount>>(value);
    for (std::size_t index = 0; index < kWordCount; ++index) {
      slots_[slot][index].store(words[index], std::memory_order_relaxed);
    }
    published_generation_.store(generation, std::memory_order_release);
    return generation;
  }

  [[nodiscard]] bool snapshot(Value &value,
                              std::size_t max_attempts = 8) const noexcept {
    std::array<std::uint64_t, kWordCount> words{};
    for (std::size_t attempt = 0; attempt < max_attempts; ++attempt) {
      const auto before =
          published_generation_.load(std::memory_order_acquire);
      if (before == 0) {
        return false;
      }
      const auto slot = before & 1U;
      for (std::size_t index = 0; index < kWordCount; ++index) {
        words[index] = slots_[slot][index].load(std::memory_order_relaxed);
      }
      const auto after =
          published_generation_.load(std::memory_order_acquire);
      if (before == after) {
        value = std::bit_cast<Value>(words);
        return value.status.generation == before;
      }
    }
    return false;
  }

  [[nodiscard]] std::uint64_t generation() const noexcept {
    return published_generation_.load(std::memory_order_acquire);
  }

 private:
  static constexpr std::size_t kWordCount =
      sizeof(Value) / sizeof(std::uint64_t);
  using Slot = std::array<std::atomic<std::uint64_t>, kWordCount>;

  std::array<Slot, 2> slots_{};
  alignas(64) std::atomic<std::uint64_t> published_generation_{};
  std::uint64_t writer_generation_{};
};

class AggregateLatestState {
 public:
  explicit AggregateLatestState(
      std::uint64_t bbo_window_interval_ns = 200'000'000ULL) noexcept
      : bbo_window_interval_ns_(bbo_window_interval_ns) {}
  AggregateLatestState(const AggregateLatestState &) = delete;
  AggregateLatestState &operator=(const AggregateLatestState &) = delete;

  // publish, reset, and service_bbo_window_rollover are single-writer methods.
  void publish(const utils::md::wire::AggBboRecord &record,
               AggregateReceiveInfo receive) noexcept;
  bool publish(const utils::md::wire::AggOrderBookRecord &record,
               AggregateReceiveInfo receive) noexcept;

  // Resets are publications: readers observe ready=false and reset_generation.
  void reset(AggregateTopic topic,
             AggregateReceiveInfo receive = {}) noexcept;

  // Recorder/readers request a consume-and-reset without becoming writers.
  // The ingest writer services requests here and also rolls elapsed monotonic
  // windows. Call this from its event loop even when no BBO arrives.
  [[nodiscard]] std::uint64_t request_bbo_window_rollover() noexcept;
  void service_bbo_window_rollover(AggregateReceiveInfo receive) noexcept;
  // Drops an incomplete cross window after a safe aggregate catch-up while
  // retaining the last-good BBO publication.
  void interrupt_bbo_window() noexcept;

  [[nodiscard]] bool snapshot(AggBboSnapshot &snapshot,
                              std::size_t max_attempts = 8) const noexcept {
    return bbo_.snapshot(snapshot, max_attempts);
  }
  [[nodiscard]] bool snapshot(AggOrderBookSnapshot &snapshot,
                              std::size_t max_attempts = 8) const noexcept {
    return book_.snapshot(snapshot, max_attempts);
  }
  [[nodiscard]] bool snapshot_bbo_window(
      std::uint64_t rollover_request, AggBboWindowSnapshot &snapshot,
      std::size_t max_attempts = 8) const noexcept {
    if (!bbo_windows_.snapshot(snapshot, max_attempts)) {
      return false;
    }
    return snapshot.rollover_request >= rollover_request;
  }

 private:
  void complete_bbo_window(AggregateReceiveInfo receive,
                           std::uint64_t rollover_request) noexcept;

  AtomicDoubleBuffer<AggBboSnapshot> bbo_{};
  AtomicDoubleBuffer<AggOrderBookSnapshot> book_{};
  AtomicDoubleBuffer<AggBboWindowSnapshot> bbo_windows_{};
  AggBboSnapshot bbo_writer_{};
  AggOrderBookSnapshot book_writer_{};
  AggBboWindowSnapshot bbo_window_writer_{};
  alignas(64) std::atomic<std::uint64_t> requested_rollover_{};
  std::uint64_t served_rollover_{};
  std::uint64_t bbo_window_interval_ns_{};
  bool bbo_window_initialized_{};
};

[[nodiscard]] AggregateTopic
aggregate_topic_from_segment(std::string_view segment) noexcept;

}  // namespace mds::consume
