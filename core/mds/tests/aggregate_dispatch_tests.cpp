#include "mds/consume/aggregate_dispatch.h"

#include <array>
#include <atomic>
#include <cassert>
#include <cstdint>
#include <cstring>
#include <thread>
#include <vector>

namespace {

template <typename Record>
Record patterned(std::uint64_t value) {
  static_assert(sizeof(Record) % sizeof(value) == 0);
  std::array<std::uint64_t, sizeof(Record) / sizeof(value)> words{};
  words.fill(value);
  Record record{};
  std::memcpy(&record, words.data(), sizeof(record));
  return record;
}

template <typename Record>
bool has_single_pattern(const Record &record) {
  std::array<std::uint64_t, sizeof(Record) / sizeof(std::uint64_t)> words{};
  std::memcpy(words.data(), &record, sizeof(record));
  for (const auto word : words) {
    if (word != words.front()) {
      return false;
    }
  }
  return true;
}

template <typename Record, typename Snapshot>
void concurrent_no_torn(std::uint64_t publications) {
  mds::consume::AggregateLatestState state;
  std::atomic<bool> done{};
  std::atomic<bool> failed{};
  std::thread writer([&] {
    for (std::uint64_t generation = 1; generation <= publications;
         ++generation) {
      auto record = patterned<Record>(generation);
      state.publish(record, {.ring_epoch = 8,
                             .ring_sequence = generation,
                             .receive_mono_ns = generation + 100,
                             .receive_wall_ns = generation + 200});
    }
    done.store(true, std::memory_order_release);
  });

  std::vector<std::thread> readers;
  for (int index = 0; index < 4; ++index) {
    readers.emplace_back([&] {
      Snapshot snapshot{};
      do {
        if (state.snapshot(snapshot, 16)) {
          if (!snapshot.status.ready ||
              snapshot.status.receive.ring_sequence == 0 ||
              !has_single_pattern(snapshot.record)) {
            failed.store(true, std::memory_order_relaxed);
            return;
          }
        }
      } while (!done.load(std::memory_order_acquire));
    });
  }
  writer.join();
  for (auto &reader : readers) {
    reader.join();
  }
  assert(!failed.load(std::memory_order_relaxed));

  Snapshot final_snapshot{};
  assert(state.snapshot(final_snapshot, 16));
  assert(final_snapshot.status.receive.ring_sequence == publications);
  assert(has_single_pattern(final_snapshot.record));
}

void concurrent_rollover_requests() {
  mds::consume::AggregateLatestState state(0);
  std::atomic<bool> recorder_done{};
  std::thread writer([&] {
    std::uint64_t sequence = 0;
    do {
      ++sequence;
      utils::md::wire::AggBboRecord record{};
      record.raw_cross_bps = static_cast<std::int32_t>(sequence);
      record.gated_cross_bps = -static_cast<std::int32_t>(sequence);
      state.publish(record, {.ring_sequence = sequence,
                             .receive_mono_ns = sequence,
                             .receive_wall_ns = sequence + 100});
      state.service_bbo_window_rollover(
          {.ring_sequence = sequence,
           .receive_mono_ns = sequence,
           .receive_wall_ns = sequence + 100});
    } while (sequence < 5000 ||
             !recorder_done.load(std::memory_order_acquire));
  });

  for (std::uint64_t expected = 1; expected <= 100; ++expected) {
    const auto request = state.request_bbo_window_rollover();
    assert(request == expected);
    mds::consume::AggBboWindowSnapshot snapshot{};
    while (!state.snapshot_bbo_window(request, snapshot, 16)) {
      std::this_thread::yield();
    }
    assert(snapshot.status.ready);
    assert(snapshot.rollover_request >= request);
  }
  recorder_done.store(true, std::memory_order_release);
  writer.join();
}

}  // namespace

int main() {
  using mds::consume::AggBboSnapshot;
  using mds::consume::AggBboWindowSnapshot;
  using mds::consume::AggOrderBookSnapshot;
  using mds::consume::AggregateLatestState;
  using mds::consume::AggregateTopic;
  using utils::md::wire::AggBboRecord;
  using utils::md::wire::AggOrderBookRecord;

  AggregateLatestState state(100);
  AggBboRecord bbo{};
  bbo.raw_cross_bps = 7;
  bbo.gated_cross_bps = -2;
  state.publish(bbo, {.ring_epoch = 3,
                      .ring_sequence = 10,
                      .receive_mono_ns = 100,
                      .receive_wall_ns = 200});
  bbo.raw_cross_bps = -4;
  bbo.gated_cross_bps = 9;
  state.publish(bbo, {.ring_epoch = 3,
                      .ring_sequence = 11,
                      .receive_mono_ns = 150,
                      .receive_wall_ns = 250});

  AggBboSnapshot bbo_snapshot{};
  assert(state.snapshot(bbo_snapshot));
  assert(bbo_snapshot.status.ready);
  assert(bbo_snapshot.status.receive.ring_epoch == 3);
  assert(bbo_snapshot.status.receive.ring_sequence == 11);
  assert(bbo_snapshot.cross_window.raw_min == -4);
  assert(bbo_snapshot.cross_window.raw_max == 7);
  assert(bbo_snapshot.cross_window.gated_min == -2);
  assert(bbo_snapshot.cross_window.gated_max == 9);
  assert(bbo_snapshot.cross_window.sample_count == 2);

  bbo.raw_cross_bps = 3;
  bbo.gated_cross_bps = 4;
  state.publish(bbo, {.ring_epoch = 3,
                      .ring_sequence = 12,
                      .receive_mono_ns = 201,
                      .receive_wall_ns = 301});
  AggBboWindowSnapshot completed_window{};
  assert(state.snapshot_bbo_window(0, completed_window));
  assert(completed_window.status.ready);
  assert(completed_window.cross_window.start_mono_ns == 100);
  assert(completed_window.cross_window.end_mono_ns == 200);
  assert(completed_window.cross_window.sample_count == 2);

  const auto rollover = state.request_bbo_window_rollover();
  state.service_bbo_window_rollover(
      {.ring_epoch = 3,
       .ring_sequence = 12,
       .receive_mono_ns = 250,
       .receive_wall_ns = 350});
  assert(state.snapshot_bbo_window(rollover, completed_window));
  assert(completed_window.rollover_request == rollover);
  assert(completed_window.cross_window.start_mono_ns == 201);
  assert(completed_window.cross_window.end_mono_ns == 250);
  assert(completed_window.cross_window.sample_count == 1);
  assert(state.snapshot(bbo_snapshot));
  assert(bbo_snapshot.status.ready);
  assert(bbo_snapshot.cross_window.sample_count == 0);

  bbo.raw_cross_bps = 8;
  bbo.gated_cross_bps = -8;
  state.publish(bbo, {.ring_epoch = 3,
                      .ring_sequence = 13,
                      .receive_mono_ns = 260,
                      .receive_wall_ns = 360});
  state.interrupt_bbo_window();
  assert(state.snapshot(bbo_snapshot));
  assert(bbo_snapshot.status.ready);
  assert(bbo_snapshot.status.receive.ring_sequence == 13);
  assert(bbo_snapshot.cross_window.sample_count == 0);
  bbo.raw_cross_bps = 6;
  bbo.gated_cross_bps = -6;
  state.publish(bbo, {.ring_epoch = 3,
                      .ring_sequence = 20,
                      .receive_mono_ns = 300,
                      .receive_wall_ns = 400});
  assert(state.snapshot(bbo_snapshot));
  assert(bbo_snapshot.cross_window.start_mono_ns == 300);
  assert(bbo_snapshot.cross_window.sample_count == 1);

  state.reset(AggregateTopic::AggBbo,
              {.ring_epoch = 4, .ring_sequence = 99});
  assert(state.snapshot(bbo_snapshot));
  assert(!bbo_snapshot.status.ready);
  const auto reset_generation = bbo_snapshot.status.reset_generation;
  assert(reset_generation == bbo_snapshot.status.generation);
  assert(bbo_snapshot.status.receive.ring_epoch == 4);

  bbo = {};
  bbo.raw_cross_bps = 1;
  bbo.gated_cross_bps = 2;
  state.publish(bbo, {.ring_epoch = 4, .ring_sequence = 100});
  assert(state.snapshot(bbo_snapshot));
  assert(bbo_snapshot.status.ready);
  assert(bbo_snapshot.status.reset_generation == reset_generation);
  assert(bbo_snapshot.cross_window.sample_count == 1);

  AggregateLatestState btc_bbo;
  AggregateLatestState eth_bbo;
  AggregateLatestState btc_book;
  AggregateLatestState eth_book;
  AggBboRecord btc_bbo_record{};
  btc_bbo_record.header.instrument_id = 101;
  AggBboRecord eth_bbo_record{};
  eth_bbo_record.header.instrument_id = 202;
  AggOrderBookRecord btc_book_record{};
  btc_book_record.header.instrument_id = 303;
  btc_book_record.bid_count = 11;
  AggOrderBookRecord eth_book_record{};
  eth_book_record.header.instrument_id = 404;
  eth_book_record.bid_count = 22;
  btc_bbo.publish(btc_bbo_record, {.ring_sequence = 1});
  eth_bbo.publish(eth_bbo_record, {.ring_sequence = 2});
  btc_book.publish(btc_book_record, {.ring_sequence = 3});
  eth_book.publish(eth_book_record, {.ring_sequence = 4});

  AggBboSnapshot isolated_bbo{};
  assert(btc_bbo.snapshot(isolated_bbo));
  assert(isolated_bbo.record.header.instrument_id == 101);
  assert(eth_bbo.snapshot(isolated_bbo));
  assert(isolated_bbo.record.header.instrument_id == 202);
  AggOrderBookSnapshot isolated_book{};
  assert(btc_book.snapshot(isolated_book));
  assert(isolated_book.record.header.instrument_id == 303);
  assert(isolated_book.record.bid_count == 11);
  assert(eth_book.snapshot(isolated_book));
  assert(isolated_book.record.header.instrument_id == 404);
  assert(isolated_book.record.bid_count == 22);
  assert(!btc_bbo.snapshot(isolated_book));
  assert(!eth_bbo.snapshot(isolated_book));
  assert(!btc_book.snapshot(isolated_bbo));
  assert(!eth_book.snapshot(isolated_bbo));

  mds::consume::AggregateWatchdog watchdog(5'000, 10'000);
  assert(watchdog.poll(100'000) ==
         mds::consume::AggregateWatchdogAction::None);
  assert(!watchdog.started());
  watchdog.on_ready(1'000);
  assert(watchdog.poll(5'999) ==
         mds::consume::AggregateWatchdogAction::None);
  assert(watchdog.poll(6'000) ==
         mds::consume::AggregateWatchdogAction::Stale);
  assert(watchdog.stale());
  assert(watchdog.poll(10'999) ==
         mds::consume::AggregateWatchdogAction::None);
  assert(watchdog.poll(11'000) ==
         mds::consume::AggregateWatchdogAction::HardReset);
  assert(watchdog.hard_reset_latched());
  assert(watchdog.poll(20'000) ==
         mds::consume::AggregateWatchdogAction::None);
  watchdog.on_ready(21'000);
  assert(!watchdog.stale());
  assert(!watchdog.hard_reset_latched());
  assert(watchdog.poll(31'000) ==
         mds::consume::AggregateWatchdogAction::HardReset);
  watchdog.on_ready(40'000);
  watchdog.on_hard_reset();
  assert(watchdog.hard_reset_latched());
  assert(watchdog.poll(60'000) ==
         mds::consume::AggregateWatchdogAction::None);

  concurrent_no_torn<AggBboRecord, AggBboSnapshot>(10'000);
  concurrent_no_torn<AggOrderBookRecord, AggOrderBookSnapshot>(4'000);
  concurrent_rollover_requests();
  return 0;
}
