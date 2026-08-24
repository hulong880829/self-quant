#define main mds_shm_consumer_program_main
#include "../examples/shm_consumer.cpp"
#undef main

#include <array>
#include <cassert>
#include <chrono>

namespace {

utils::md::wire::RecordHeader header(utils::md::MessageType type,
                                     utils::md::InstrumentId instrument_id,
                                     std::uint64_t bus_sequence) {
  auto value = utils::md::wire::MakeHeader(type, 0);
  value.instrument_id = instrument_id;
  value.bus_seq = bus_sequence;
  value.source_seq = bus_sequence;
  value.book_generation = 1;
  value.state = static_cast<std::uint8_t>(utils::md::BookState::Live);
  return value;
}

void build_interleaved_books() {
  Segment segment;
  segment.name = "/test.multiplex";
  Options options;
  options.bbo_only = true;
  std::uint64_t ring_sequence = 0;
  const auto view = [&](utils::md::MessageType type) {
    return transport::RecordView{
        static_cast<std::uint32_t>(type), ++ring_sequence, 1, {}};
  };

  for (const auto instrument_id : {utils::md::InstrumentId{11},
                                   utils::md::InstrumentId{22}}) {
    wire::SnapshotBeginRecord begin{};
    begin.header =
        header(md::MessageType::SnapshotBegin, instrument_id, ring_sequence + 1);
    begin.item_count = 2;
    begin.chunk_count_or_checksum = 2;
    process_record(segment, view(md::MessageType::SnapshotBegin),
                   SnapshotBeginDecoded{begin}, options);
  }

  for (const auto instrument_id : {utils::md::InstrumentId{11},
                                   utils::md::InstrumentId{22}}) {
    wire::SnapshotChunkRecord bid{};
    bid.header =
        header(md::MessageType::SnapshotChunk, instrument_id, ring_sequence + 1);
    bid.side = static_cast<std::uint8_t>(md::Side::Bid);
    bid.level_count = 1;
    bid.levels[0] = {
        instrument_id == 11 ? 100 : 200,
        instrument_id == 11 ? 10 : 20};
    process_record(segment, view(md::MessageType::SnapshotChunk), bid, options);

    auto ask = bid;
    ask.header.bus_seq = ring_sequence + 1;
    ask.side = static_cast<std::uint8_t>(md::Side::Ask);
    ask.levels[0] = {
        instrument_id == 11 ? 101 : 201,
        instrument_id == 11 ? 11 : 21};
    process_record(segment, view(md::MessageType::SnapshotChunk), ask, options);
  }

  for (const auto instrument_id : {utils::md::InstrumentId{22},
                                   utils::md::InstrumentId{11}}) {
    wire::SnapshotEndRecord end{};
    end.header =
        header(md::MessageType::SnapshotEnd, instrument_id, ring_sequence + 1);
    end.item_count = 2;
    process_record(segment, view(md::MessageType::SnapshotEnd),
                   SnapshotEndDecoded{end}, options);
  }

  assert(segment.snapshots.size() == 2);
  const auto &first = segment.snapshots.at(11);
  const auto &second = segment.snapshots.at(22);
  assert(first.state == md::BookState::Live);
  assert(second.state == md::BookState::Live);
  assert(first.bids.at(100) == 10 && first.asks.at(101) == 11);
  assert(second.bids.at(200) == 20 && second.asks.at(201) == 21);

  wire::DeltaRecord delta{};
  delta.header = header(md::MessageType::BookDelta, 11, ring_sequence + 1);
  delta.side = static_cast<std::uint8_t>(md::Side::Bid);
  delta.price = 100;
  delta.quantity = 15;
  process_record(segment, view(md::MessageType::BookDelta), delta, options);
  assert(segment.snapshots.at(11).bids.at(100) == 15);
  assert(segment.snapshots.at(22).bids.at(200) == 20);
}

void unknown_types_advance_transport_only() {
  mds::consume::SequenceTracker sequences;
  auto known = header(md::MessageType::Bbo, 11, 1);
  sequences.accept(1, known);
  auto unknown = header(static_cast<md::MessageType>(9999), 22, 2);
  assert(sequences.check_transport(2, unknown) ==
         mds::consume::SequenceError::None);
  sequences.accept_transport(2, unknown);
  known.bus_seq = 3;
  known.source_seq = 2;
  assert(sequences.check(3, known) == mds::consume::SequenceError::None);
  sequences.accept(3, known);
  unknown.bus_seq = 5;
  assert(sequences.check_transport(4, unknown) ==
         mds::consume::SequenceError::Bus);
}

void aggregate_overrun_retains_last_good() {
  const auto unique =
      std::to_string(::getpid()) + "." +
      std::to_string(std::chrono::steady_clock::now()
                         .time_since_epoch()
                         .count());
  transport::RingOptions options;
  options.name = "/mds.consumer.catchup." + unique;
  options.ring_bytes = 4096;
  options.max_record_bytes = 512;
  options.max_readers = 2;
  options.unlink_on_close = true;
  auto opened = transport::SharedRing::open(options);
  assert(opened);

  Segment segment;
  segment.name = options.name;
  segment.ring = std::move(opened.value);
  segment.aggregate_topic = consume::AggregateTopic::AggBbo;
  segment.latest_state = std::make_unique<consume::AggregateLatestState>(0);
  const auto marker =
      transport::process_start_marker(static_cast<std::uint32_t>(::getpid()));
  assert(register_reader(segment, marker));

  wire::AggBboRecord record{};
  record.raw_cross_bps = 7;
  segment.latest_state->publish(
      record, {.ring_epoch = segment.epoch,
               .ring_sequence = 10,
               .receive_mono_ns = 100});
  segment.last_validated_ring_sequence = 10;

  std::array<std::byte, 400> payload{};
  for (std::size_t index = 0; index < 32; ++index) {
    assert(segment.ring.publish(999, payload));
  }
  transport::ReadLease lease;
  const auto read_error = segment.ring.try_read(segment.reader, lease);
  assert(read_error == mds::api::ErrorCode::SubscriptionRejected ||
         read_error == mds::api::ErrorCode::RecordOverwritten);
  assert(resync_after_overrun(segment, "test-overrun"));

  consume::AggBboSnapshot snapshot{};
  assert(segment.latest_state->snapshot(snapshot));
  assert(snapshot.status.ready);
  assert(snapshot.status.receive.ring_sequence == 10);
  assert(snapshot.cross_window.sample_count == 0);
  assert(segment.aggregate_catchups == 1);
  assert(segment.aggregate_window_gaps == 1);

  invalidate_and_resync(segment, "test-corruption");
  assert(segment.latest_state->snapshot(snapshot));
  assert(!snapshot.status.ready);
  assert(segment.aggregate_hard_resets == 1);
  (void)segment.ring.unregister_reader(segment.reader);
}

}  // namespace

int main() {
  build_interleaved_books();
  unknown_types_advance_transport_only();
  aggregate_overrun_retains_last_good();
  return 0;
}
