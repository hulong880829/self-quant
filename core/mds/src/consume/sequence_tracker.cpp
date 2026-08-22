#include "mds/consume/sequence_tracker.h"

namespace mds::consume {

SequenceError SequenceTracker::check(
    std::uint64_t ring_sequence,
    const utils::md::wire::RecordHeader &header) const noexcept {
  const auto transport = check_transport(ring_sequence, header);
  if (transport != SequenceError::None) {
    return transport;
  }
  if (!is_instrument(header)) {
    const auto found = sources_.find(header.instrument_id);
    if (found != sources_.end() && found->second.baseline &&
        header.book_generation == found->second.generation &&
        header.source_seq < found->second.sequence) {
      return SequenceError::Source;
    }
  }
  return SequenceError::None;
}

SequenceError SequenceTracker::check_transport(
    std::uint64_t ring_sequence,
    const utils::md::wire::RecordHeader &header) const noexcept {
  if (sequence_baseline_) {
    if (ring_sequence != last_ring_sequence_ + 1) {
      return SequenceError::Ring;
    }
    if (header.bus_seq != last_bus_sequence_ + 1) {
      return SequenceError::Bus;
    }
  }
  return SequenceError::None;
}

void SequenceTracker::accept(
    std::uint64_t ring_sequence,
    const utils::md::wire::RecordHeader &header) noexcept {
  last_ring_sequence_ = ring_sequence;
  last_bus_sequence_ = header.bus_seq;
  sequence_baseline_ = true;
  if (header.message_type == static_cast<std::uint16_t>(
                                 utils::md::MessageType::InstrumentCatalog)) {
    // A catalog is the event-driven identity barrier for rolling instruments.
    // Clearing source baselines is conservative for multiplex rings and keeps
    // the per-segment tracker bounded as physical IDs rotate.
    sources_.clear();
  } else if (!is_instrument(header)) {
    sources_[header.instrument_id] = {
        header.source_seq, header.book_generation, true};
  }
}

void SequenceTracker::accept_transport(
    std::uint64_t ring_sequence,
    const utils::md::wire::RecordHeader &header) noexcept {
  last_ring_sequence_ = ring_sequence;
  last_bus_sequence_ = header.bus_seq;
  sequence_baseline_ = true;
}

void SequenceTracker::reset() noexcept {
  sequence_baseline_ = false;
  sources_.clear();
}

bool SequenceTracker::is_instrument(
    const utils::md::wire::RecordHeader &header) noexcept {
  return header.message_type == static_cast<std::uint16_t>(
                                    utils::md::MessageType::InstrumentUpdate) ||
         header.message_type == static_cast<std::uint16_t>(
                                    utils::md::MessageType::InstrumentCatalog);
}

}  // namespace mds::consume
