#include "mds/redundancy/sequence_arbiter.h"

namespace mds::redundancy {

SequenceArbiter::SequenceArbiter(std::size_t max_gap)
    : max_gap_(max_gap == 0 ? 1 : max_gap),
      pending_(max_gap_ + 1U),
      published_hashes_(max_gap_ * 2U + 1U) {}

void SequenceArbiter::reset(std::uint64_t next_sequence) noexcept {
  next_sequence_ = next_sequence;
  divergent_ = false;
  for (auto &slot : pending_) {
    slot = {};
  }
  for (auto &slot : published_hashes_) {
    slot = {};
  }
}

bool SequenceArbiter::submit_each(NativeEvent event, void *context,
                                  DecisionSink sink) noexcept {
  if (sink == nullptr) {
    return false;
  }
  const auto emit = [&](ArbiterAction action, NativeEvent emitted,
                        bool filled_gap) noexcept {
    return sink(context, {action, emitted, filled_gap});
  };
  if (divergent_) {
    return emit(ArbiterAction::Resync, event, false);
  }
  if (next_sequence_ == 0) {
    next_sequence_ = event.sequence;
  }
  if (event.sequence < next_sequence_) {
    const auto &history =
        published_hashes_[event.sequence % published_hashes_.size()];
    if (history.occupied && history.sequence == event.sequence) {
      const auto action = history.payload_hash == event.payload_hash
                              ? ArbiterAction::Duplicate
                              : ArbiterAction::Divergence;
      divergent_ = action == ArbiterAction::Divergence;
      return emit(action, event, false);
    } else {
      return emit(ArbiterAction::Regression, event, false);
    }
  }
  if (event.sequence - next_sequence_ > max_gap_) {
    divergent_ = true;
    return emit(ArbiterAction::Resync, event, false);
  }

  auto &position = pending_[event.sequence % pending_.size()];
  if (position.occupied) {
    if (position.sequence != event.sequence) {
      divergent_ = true;
      return emit(ArbiterAction::Resync, event, false);
    }
    if (position.pending.first.payload_hash != event.payload_hash) {
      divergent_ = true;
      return emit(ArbiterAction::Divergence, event, false);
    } else {
      position.pending.has_second = true;
      position.pending.second_hash = event.payload_hash;
      return emit(ArbiterAction::Duplicate, event, false);
    }
  }
  position.sequence = event.sequence;
  position.pending = Pending{event, false, 0};
  position.occupied = true;
  if (event.sequence != next_sequence_) {
    return emit(ArbiterAction::BufferedGap, event, false);
  }

  const bool gap_filled = [&] {
    for (const auto &slot : pending_) {
      if (slot.occupied && slot.sequence > next_sequence_) {
        return true;
      }
    }
    return false;
  }();
  for (;;) {
    auto &current = pending_[next_sequence_ % pending_.size()];
    if (!current.occupied || current.sequence != next_sequence_) {
      break;
    }
    const NativeEvent publish = current.pending.first;
    auto &history =
        published_hashes_[next_sequence_ % published_hashes_.size()];
    history = {next_sequence_, publish.payload_hash, true};
    current = {};
    if (!emit(ArbiterAction::Publish, publish, gap_filled)) {
      return false;
    }
    ++next_sequence_;
  }
  return true;
}

std::vector<ArbiterDecision> SequenceArbiter::submit(NativeEvent event) {
  std::vector<ArbiterDecision> decisions;
  decisions.reserve(max_gap_ + 2U);
  (void)submit_each(
      event, &decisions,
      [](void *context, const ArbiterDecision &decision) noexcept {
        // reserve() above guarantees no allocation for the maximum possible
        // batch (max_gap + current event).
        static_cast<std::vector<ArbiterDecision> *>(context)
            ->push_back(decision);
        return true;
      });
  return decisions;
}

} // namespace mds::redundancy
