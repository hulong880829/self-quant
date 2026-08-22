#pragma once

#include "utils/md/types.h"

#include <cstddef>
#include <cstdint>
#include <limits>
#include <optional>

namespace mds::service {

enum class WsPreSnapshotDeltaDecision : std::uint8_t {
  Fail,
  IgnoreDuringRecovery,
};

[[nodiscard]] inline bool ShouldIgnorePreSnapshotDelta(
    utils::md::Venue venue,
    WsPreSnapshotDeltaDecision decision) noexcept {
  return venue == utils::md::Venue::Bybit ||
         decision == WsPreSnapshotDeltaDecision::IgnoreDuringRecovery;
}

class WsSnapshotRecovery {
 public:
  void snapshot_ready() noexcept { phase_ = Phase::Live; }

  void begin_recovery() noexcept { phase_ = Phase::WaitingSnapshot; }

  void reset_connection() noexcept { phase_ = Phase::Cold; }

  [[nodiscard]] WsPreSnapshotDeltaDecision pre_snapshot_delta() const
      noexcept {
    return phase_ == Phase::WaitingSnapshot
               ? WsPreSnapshotDeltaDecision::IgnoreDuringRecovery
               : WsPreSnapshotDeltaDecision::Fail;
  }

  [[nodiscard]] bool waiting_snapshot() const noexcept {
    return phase_ == Phase::WaitingSnapshot;
  }

 private:
  enum class Phase : std::uint8_t {
    Cold,
    Live,
    WaitingSnapshot,
  };

  Phase phase_{Phase::Cold};
};

enum class SnapshotBridgeDecision : std::uint8_t {
  Stale,
  Accepted,
  SnapshotTooOld,
  Gap,
  Invalid,
};

class LaggingSnapshotRetries {
 public:
  static constexpr std::uint8_t kMaximumAttempts = 32;

  [[nodiscard]] bool allow_retry() noexcept {
    if (attempts_ >= kMaximumAttempts) {
      return false;
    }
    ++attempts_;
    return true;
  }

  void reset() noexcept { attempts_ = 0; }

  [[nodiscard]] std::uint8_t attempts() const noexcept {
    return attempts_;
  }

 private:
  std::uint8_t attempts_{};
};

[[nodiscard]] inline bool ShouldRetryLaggingSnapshot(
    SnapshotBridgeDecision decision, bool retry_enabled,
    LaggingSnapshotRetries &retries) noexcept {
  return decision == SnapshotBridgeDecision::SnapshotTooOld &&
         retry_enabled && retries.allow_retry();
}

class SnapshotBridgeValidator {
 public:
  explicit SnapshotBridgeValidator(
      std::uint64_t snapshot_sequence) noexcept
      : sequence_(snapshot_sequence) {}

  [[nodiscard]] SnapshotBridgeDecision Observe(
      std::uint64_t first, std::uint64_t final,
      bool strict_previous_sequence,
      std::uint64_t previous_sequence = 0) noexcept {
    if (first > final) {
      return SnapshotBridgeDecision::Invalid;
    }
    if (final <= sequence_) {
      return SnapshotBridgeDecision::Stale;
    }
    if (sequence_ == std::numeric_limits<std::uint64_t>::max()) {
      return SnapshotBridgeDecision::Gap;
    }
    const auto expected = sequence_ + 1;
    if (first_relevant_) {
      const bool previous_is_snapshot =
          strict_previous_sequence &&
          previous_sequence != 0 &&
          previous_sequence == sequence_;
      if (!previous_is_snapshot && first > expected) {
        return SnapshotBridgeDecision::SnapshotTooOld;
      }
      if (!previous_is_snapshot && final < expected) {
        return SnapshotBridgeDecision::Gap;
      }
      first_relevant_ = false;
      sequence_ = final;
      return SnapshotBridgeDecision::Accepted;
    }
    if ((strict_previous_sequence &&
         previous_sequence != 0 &&
         previous_sequence != sequence_) ||
        (strict_previous_sequence &&
         previous_sequence == 0 && first != expected) ||
        (!strict_previous_sequence && first > expected)) {
      return SnapshotBridgeDecision::Gap;
    }
    sequence_ = final;
    return SnapshotBridgeDecision::Accepted;
  }

  [[nodiscard]] std::uint64_t sequence() const noexcept {
    return sequence_;
  }

  [[nodiscard]] bool has_accepted() const noexcept {
    return !first_relevant_;
  }

 private:
  std::uint64_t sequence_{};
  bool first_relevant_{true};
};

template <typename Predicate>
[[nodiscard]] std::optional<std::size_t> NextRoundRobin(
    std::size_t count, std::size_t start,
    Predicate &&eligible) {
  if (count == 0) {
    return std::nullopt;
  }
  start %= count;
  for (std::size_t offset = 0; offset < count; ++offset) {
    const auto index = (start + offset) % count;
    if (eligible(index)) {
      return index;
    }
  }
  return std::nullopt;
}

}  // namespace mds::service
