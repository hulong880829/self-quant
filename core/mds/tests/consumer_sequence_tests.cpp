#include "mds/consume/sequence_tracker.h"

#include <cassert>

int main() {
  using mds::consume::SequenceError;
  using mds::consume::SequenceTracker;
  using utils::md::MessageType;
  using utils::md::wire::MakeHeader;

  SequenceTracker tracker;
  auto first = MakeHeader(MessageType::SnapshotBegin,
                          sizeof(utils::md::wire::SnapshotBeginRecord));
  first.bus_seq = 10;
  first.source_seq = 100;
  first.book_generation = 1;
  assert(tracker.check(50, first) == SequenceError::None);
  tracker.accept(50, first);

  auto next = MakeHeader(MessageType::SnapshotChunk,
                         sizeof(utils::md::wire::SnapshotChunkRecord));
  next.bus_seq = 11;
  next.source_seq = 100;
  next.book_generation = 1;
  assert(tracker.check(51, next) == SequenceError::None);

  auto invalid = next;
  invalid.bus_seq = 12;
  assert(tracker.check(51, invalid) == SequenceError::Bus);
  invalid = next;
  assert(tracker.check(52, invalid) == SequenceError::Ring);
  invalid = next;
  invalid.source_seq = 99;
  assert(tracker.check(51, invalid) == SequenceError::Source);

  auto instrument = MakeHeader(
      MessageType::InstrumentUpdate,
      sizeof(utils::md::wire::InstrumentUpdateRecord));
  instrument.bus_seq = 11;
  instrument.source_seq = 0;
  instrument.book_generation = 1;
  assert(tracker.check(51, instrument) == SequenceError::None);
  tracker.accept(51, instrument);

  tracker.reset();
  invalid.bus_seq = 500;
  invalid.source_seq = 1;
  assert(tracker.check(900, invalid) == SequenceError::None);
  return 0;
}
