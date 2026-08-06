#pragma once

#include <cstdint>
#include <vector>

namespace mds::redundancy {

enum class Feed : std::uint8_t { A, B };
enum class ArbiterAction : std::uint8_t {
  Publish,
  Duplicate,
  BufferedGap,
  Divergence,
  Regression,
  Resync
};

struct NativeEvent {
  std::uint64_t sequence{};
  std::uint64_t payload_hash{};
  Feed feed{};
};

struct ArbiterDecision {
  ArbiterAction action{};
  NativeEvent event{};
  bool filled_gap{};
};

class SequenceArbiter {
public:
  using DecisionSink =
      bool (*)(void *context, const ArbiterDecision &decision) noexcept;

  explicit SequenceArbiter(std::size_t max_gap = 1024);
  void reset(std::uint64_t next_sequence) noexcept;
  // Allocation-free hot-path API. Storage is allocated by the constructor.
  bool submit_each(NativeEvent event, void *context,
                   DecisionSink sink) noexcept;
  // Control/test convenience API; callers must not use it on the hot path.
  std::vector<ArbiterDecision> submit(NativeEvent event);
  [[nodiscard]] std::uint64_t next_sequence() const noexcept {
    return next_sequence_;
  }
  [[nodiscard]] bool divergent() const noexcept { return divergent_; }

private:
  struct Pending {
    NativeEvent first{};
    bool has_second{};
    std::uint64_t second_hash{};
  };
  struct PendingSlot {
    std::uint64_t sequence{};
    Pending pending{};
    bool occupied{};
  };
  struct HistorySlot {
    std::uint64_t sequence{};
    std::uint64_t payload_hash{};
    bool occupied{};
  };

  std::uint64_t next_sequence_{};
  std::size_t max_gap_{};
  bool divergent_{};
  std::vector<PendingSlot> pending_{};
  std::vector<HistorySlot> published_hashes_{};
};

} // namespace mds::redundancy
