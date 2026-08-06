#include "mds/book/bbo_overlay.h"
#include "mds/book/book_bridge.h"
#include "mds/exchange/binance/binance_adapter.h"
#include "mds/exchange/binance/binance_rest.h"
#include "mds/exchange/binance/binance_streams.h"
#include "mds/network/tls_websocket.h"
#include "mds/publish/wire_publisher.h"
#include "mds/redundancy/sequence_arbiter.h"
#include "mds/service/binance_session.h"
#include "mds/transport/shared_ring.h"

#include <array>
#include <atomic>
#include <bit>
#include <chrono>
#include <cctype>
#include <cstdlib>
#include <cstring>
#include <fcntl.h>
#include <fstream>
#include <iostream>
#include <limits>
#include <new>
#include <stdexcept>
#include <string>
#include <sys/mman.h>
#include <sys/stat.h>
#include <type_traits>
#include <unistd.h>
#include <vector>

namespace {
std::atomic_bool track_allocations{};
std::atomic<std::size_t> tracked_allocations{};
}

void *operator new(std::size_t size) {
  if (track_allocations.load(std::memory_order_relaxed)) {
    tracked_allocations.fetch_add(1, std::memory_order_relaxed);
  }
  if (void *memory = std::malloc(size)) {
    return memory;
  }
  throw std::bad_alloc();
}

void *operator new[](std::size_t size) {
  return ::operator new(size);
}

void operator delete(void *memory) noexcept { std::free(memory); }
void operator delete[](void *memory) noexcept { std::free(memory); }
void operator delete(void *memory, std::size_t) noexcept { std::free(memory); }
void operator delete[](void *memory, std::size_t) noexcept {
  std::free(memory);
}

namespace {

void require(bool condition, const char *message) {
  if (!condition) {
    throw std::runtime_error(message);
  }
}

std::string read_fixture(const char *name) {
  std::ifstream input(std::string(MDS_TEST_FIXTURE_DIR) + "/" + name,
                      std::ios::binary);
  require(input.good(), "failed to open Binance fixture");
  return {std::istreambuf_iterator<char>(input),
          std::istreambuf_iterator<char>()};
}

std::uint64_t now_ns() {
  return static_cast<std::uint64_t>(
      std::chrono::steady_clock::now().time_since_epoch().count());
}

template <typename T>
void write_le(std::span<std::byte> destination, std::size_t offset, T value) {
  using Unsigned = std::make_unsigned_t<T>;
  const Unsigned raw = [&] {
    if constexpr (std::is_signed_v<T>) {
      return std::bit_cast<Unsigned>(value);
    } else {
      return value;
    }
  }();
  require(offset <= destination.size() &&
              sizeof(T) <= destination.size() - offset,
          "fixture write overflow");
  for (std::size_t index = 0; index < sizeof(T); ++index) {
    destination[offset + index] =
        static_cast<std::byte>((raw >> (index * 8U)) & 0xffU);
  }
}

void test_shared_ring_and_lease() {
  using namespace mds::transport;
  RingOptions options;
  options.name = "/mds.test." + std::to_string(::getpid());
  options.mode = mds::api::RingMode::Lossless;
  options.ring_bytes = 4096;
  options.max_record_bytes = 256;
  options.unlink_on_close = true;
  auto opened = SharedRing::open(options);
  require(bool(opened), opened.message.c_str());
  auto ring = std::move(opened.value);
  const auto marker = process_start_marker(::getpid());
  auto registration = ring.register_reader(marker, now_ns());
  require(bool(registration), "reader registration failed");
  auto reader = registration.value;

  std::array<std::byte, 200> payload{};
  payload[0] = std::byte{0x42};
  for (int i = 0; i < 18; ++i) {
    auto published = ring.publish(7, payload);
    require(bool(published), "publish failed");
    auto record = ring.read(reader);
    require(bool(record), "read failed");
    require(record.value->type == 7 &&
                record.value->payload.size() == payload.size() &&
                record.value->payload[0] == payload[0],
            "ring payload mismatch");
    require(static_cast<bool>(record.value.commit()), "read commit failed");
  }

  auto published = ring.publish(8, payload);
  require(bool(published), "lease test publish failed");
  auto held = ring.read(reader);
  require(bool(held), "lease test read failed");
  bool backpressured = false;
  for (int i = 0; i < 64; ++i) {
    if (!ring.publish(9, payload)) {
      backpressured = true;
      break;
    }
  }
  require(backpressured, "writer overwrote an outstanding read lease");
  require(held.value->type == 8 && held.value->payload[0] == payload[0],
          "leased record changed while writer filled the ring");
  held.value.release();
  auto repeated = ring.read(reader);
  require(bool(repeated) && repeated.value->sequence == published.value,
          "released record was not offered again");
  require(static_cast<bool>(repeated.value.commit()),
          "repeated read commit failed");
  require(bool(ring.publish(10, payload)),
          "committing a lease did not release writer capacity");

  require(static_cast<bool>(ring.heartbeat(reader, now_ns())),
          "heartbeat failed");
  require(static_cast<bool>(ring.unregister_reader(reader)),
          "unregister failed");

  auto stale = ring.register_reader(marker, 1);
  require(bool(stale), "stale reader registration failed");
  const auto reclaimed =
      ring.reclaim_stale(now_ns(), 1,
                         [](std::uint32_t, std::uint64_t) { return false; });
  require(reclaimed == 1, "stale reader was not reclaimed");
}

void test_shared_ring_try_read() {
  using namespace mds::transport;
  static_assert(std::is_same_v<
                decltype(std::declval<SharedRing &>().try_read(
                    std::declval<ReaderHandle &>(),
                    std::declval<ReadLease &>())),
                mds::api::ErrorCode>);

  RingOptions options;
  options.name = "/mds.try-read-test." + std::to_string(::getpid());
  options.ring_bytes = 4096;
  options.max_record_bytes = 256;
  options.unlink_on_close = true;
  auto opened = SharedRing::open(options);
  require(bool(opened), opened.message.c_str());
  auto ring = std::move(opened.value);
  auto registration =
      ring.register_reader(process_start_marker(::getpid()), now_ns());
  require(bool(registration), "try_read reader registration failed");
  auto reader = registration.value;

  ReadLease lease;
  tracked_allocations.store(0, std::memory_order_relaxed);
  track_allocations.store(true, std::memory_order_release);
  bool all_empty = true;
  for (int iteration = 0; iteration < 100'000; ++iteration) {
    if (ring.try_read(reader, lease) !=
            mds::api::ErrorCode::QuotaExceeded ||
        lease) {
      all_empty = false;
      break;
    }
  }
  track_allocations.store(false, std::memory_order_release);
  require(all_empty, "empty try_read changed its output lease");
  require(tracked_allocations.load(std::memory_order_relaxed) == 0,
          "empty try_read performed a heap allocation");

  std::array<std::byte, 8> payload{std::byte{0x42}};
  require(bool(ring.publish(7, payload)), "try_read publish failed");
  require(ring.try_read(reader, lease) == mds::api::ErrorCode::Ok && lease &&
              lease->type == 7 && lease->payload[0] == payload[0],
          "try_read did not return the published record");
  require(static_cast<bool>(lease.commit()), "try_read lease commit failed");

  for (int iteration = 0; iteration < 128; ++iteration) {
    require(bool(ring.publish(8, payload)),
            "overwrite try_read setup publish failed");
  }
  require(ring.try_read(reader, lease) ==
                  mds::api::ErrorCode::SubscriptionRejected &&
              !lease,
          "try_read did not report reader overrun");
  require(static_cast<bool>(ring.resync_to_latest(reader)),
          "try_read reader resync failed");
}

void test_shared_ring_overwrite_and_resync() {
  using namespace mds::transport;
  RingOptions options;
  options.name = "/mds.overwrite-test." + std::to_string(::getpid());
  options.ring_bytes = 4096;
  options.max_record_bytes = 256;
  options.unlink_on_close = true;
  auto opened = SharedRing::open(options);
  require(bool(opened), opened.message.c_str());
  auto ring = std::move(opened.value);
  require(ring.mode() == mds::api::RingMode::OverwriteOldest,
          "overwrite-oldest is not the default ring mode");
  auto registration =
      ring.register_reader(process_start_marker(::getpid()), now_ns());
  require(bool(registration), "overwrite reader registration failed");
  auto reader = registration.value;

  std::array<std::byte, 200> payload{};
  require(bool(ring.publish(7, payload)), "initial overwrite publish failed");
  auto held = ring.read(reader);
  require(bool(held), "failed to acquire overwrite read lease");
  for (int i = 0; i < 32; ++i) {
    require(bool(ring.publish(8, payload)),
            "overwrite mode unexpectedly backpressured publisher");
  }
  const auto overwritten = held.value.commit();
  require(!overwritten &&
              overwritten.error == mds::api::ErrorCode::RecordOverwritten,
          "overwritten read lease passed atomic sequence validation");

  const auto overrun = ring.read(reader);
  require(!overrun &&
              overrun.error == mds::api::ErrorCode::SubscriptionRejected,
          "overrun reader did not request resynchronization");
  const auto generation = ring.registry_generation();
  require(static_cast<bool>(ring.resync_to_latest(reader)),
          "reader resync to latest failed");
  require(ring.registry_generation() == generation + 1U,
          "reader resync did not notify the producer");
  require(bool(ring.publish(9, payload)), "post-resync publish failed");
  auto resumed = ring.read(reader);
  require(bool(resumed) && resumed.value->type == 9,
          "reader did not resume from latest cursor");
  require(static_cast<bool>(resumed.value.commit()),
          "post-resync read commit failed");
}

void test_shared_ring_initialization_and_header_validation() {
  using namespace mds::transport;
  const auto name = "/mds.header-test." + std::to_string(::getpid());

  const int half_fd = ::shm_open(name.c_str(), O_RDWR | O_CREAT | O_EXCL, 0600);
  require(half_fd >= 0, "failed to create half-initialized segment");
  const std::size_t half_bytes = sizeof(SegmentHeader) + 4096;
  require(::ftruncate(half_fd, static_cast<off_t>(half_bytes)) == 0,
          "failed to size half-initialized segment");
  void *half_mapping =
      ::mmap(nullptr, half_bytes, PROT_READ | PROT_WRITE, MAP_SHARED, half_fd, 0);
  require(half_mapping != MAP_FAILED, "failed to map half-initialized segment");
  std::memset(half_mapping, 0, half_bytes);
  new (half_mapping) SegmentHeader();
  ::munmap(half_mapping, half_bytes);
  ::close(half_fd);

  RingOptions attach;
  attach.name = name;
  attach.create = false;
  require(!SharedRing::open(attach), "half-initialized segment was attached");
  ::shm_unlink(name.c_str());

  RingOptions options;
  options.name = name;
  options.mode = mds::api::RingMode::Lossless;
  options.ring_bytes = 4096;
  options.max_record_bytes = 256;
  options.unlink_on_close = true;
  auto opened = SharedRing::open(options);
  require(bool(opened), opened.message.c_str());
  auto ring = std::move(opened.value);

  const int fd = ::shm_open(name.c_str(), O_RDWR, 0600);
  require(fd >= 0, "failed to reopen test segment");
  struct stat status {};
  require(::fstat(fd, &status) == 0, "failed to stat test segment");
  void *mapping = ::mmap(nullptr, static_cast<std::size_t>(status.st_size),
                         PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
  require(mapping != MAP_FAILED, "failed to map test segment");
  auto *header = static_cast<SegmentHeader *>(mapping);

  header->readers[0].pid.store(999999, std::memory_order_relaxed);
  header->readers[0].process_start_marker.store(1, std::memory_order_relaxed);
  header->readers[0].heartbeat_ns.store(1, std::memory_order_relaxed);
  header->readers[0].state.store(
      static_cast<std::uint32_t>(ReaderState::Initializing),
      std::memory_order_release);
  std::array<std::byte, 8> payload{};
  require(!ring.publish(1, payload),
          "writer ignored an initializing reader slot");
  require(ring.reclaim_stale(
              now_ns(), 1,
              [](std::uint32_t, std::uint64_t) { return false; }) == 1,
          "crashed initializing reader was not reclaimed");
  require(bool(ring.publish(1, payload)),
          "writer remained blocked after crashed reader recovery");

  const auto schema = header->schema_major;
  header->schema_major = schema + 1;
  require(!SharedRing::open(attach), "corrupt schema was accepted");
  header->schema_major = schema;

  const auto ring_bytes = header->ring_bytes;
  header->ring_bytes = ring_bytes - 1;
  require(!SharedRing::open(attach), "non-power-of-two ring was accepted");
  header->ring_bytes = ring_bytes;

  const auto epoch = header->epoch;
  header->epoch = 0;
  require(!SharedRing::open(attach), "zero epoch was accepted");
  header->epoch = epoch;

  const auto max_readers = header->max_readers;
  header->max_readers = static_cast<std::uint32_t>(kMaxReaders + 1);
  require(!SharedRing::open(attach), "invalid reader bound was accepted");
  header->max_readers = max_readers;

  ::munmap(mapping, static_cast<std::size_t>(status.st_size));
  ::close(fd);
}

void test_shared_ring_crc_detection() {
  using namespace mds::transport;
  RingOptions options;
  options.name = "/mds.crc-test." + std::to_string(::getpid());
  options.ring_bytes = 4096;
  options.max_record_bytes = 256;
  options.unlink_on_close = true;
  auto opened = SharedRing::open(options);
  require(bool(opened), opened.message.c_str());
  auto ring = std::move(opened.value);
  auto registered =
      ring.register_reader(process_start_marker(::getpid()), now_ns());
  require(bool(registered), "CRC reader registration failed");
  auto reader = registered.value;
  std::array<std::byte, 8> payload{std::byte{0x11}};
  require(bool(ring.publish(7, payload)), "CRC test publish failed");

  const int fd = ::shm_open(options.name.c_str(), O_RDWR, 0600);
  require(fd >= 0, "CRC test shm reopen failed");
  struct stat status {};
  require(::fstat(fd, &status) == 0, "CRC test stat failed");
  void *mapping = ::mmap(nullptr, static_cast<std::size_t>(status.st_size),
                         PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
  require(mapping != MAP_FAILED, "CRC test mmap failed");
  auto *segment = static_cast<SegmentHeader *>(mapping);
  auto *record = reinterpret_cast<RecordHeader *>(
      static_cast<std::byte *>(mapping) + segment->header_bytes);
  auto *record_payload = reinterpret_cast<std::byte *>(record + 1);
  record_payload[0] ^= std::byte{0xff};

  const auto corrupted = ring.read(reader);
  require(!corrupted &&
              corrupted.error == mds::api::ErrorCode::InternalError,
          "corrupt shared-ring payload passed CRC32C validation");
  ::munmap(mapping, static_cast<std::size_t>(status.st_size));
  ::close(fd);
}

void test_arbiter() {
  using namespace mds::redundancy;
  SequenceArbiter arbiter;
  arbiter.reset(10);
  require(arbiter.submit({11, 11, Feed::A})[0].action ==
              ArbiterAction::BufferedGap,
          "gap not buffered");
  auto decisions = arbiter.submit({10, 10, Feed::B});
  require(decisions.size() == 2 &&
              decisions[0].action == ArbiterAction::Publish &&
              decisions[1].event.sequence == 11,
          "backup did not fill gap");
  require(arbiter.submit({10, 10, Feed::A})[0].action ==
              ArbiterAction::Duplicate,
          "duplicate not detected");
  require(arbiter.submit({11, 99, Feed::B})[0].action ==
              ArbiterAction::Divergence,
          "divergence not detected");

  SequenceArbiter hot_path(4);
  hot_path.reset(20);
  struct DecisionBuffer {
    std::array<ArbiterDecision, 4> values{};
    std::size_t size{};
  } output;
  require(hot_path.submit_each(
              {20, 200, Feed::A}, &output,
              [](void *context, const ArbiterDecision &decision) noexcept {
                auto &buffer = *static_cast<DecisionBuffer *>(context);
                if (buffer.size == buffer.values.size()) {
                  return false;
                }
                buffer.values[buffer.size++] = decision;
                return true;
              }),
          "allocation-free arbiter callback failed");
  require(output.size == 1 &&
              output.values[0].action == ArbiterAction::Publish,
          "allocation-free arbiter emitted wrong decision");
}

void test_snapshot_bridge() {
  using namespace mds::book;
  utils::md::OrderBook order_book(4);
  BookBridge bridge(order_book, 1);
  DepthSnapshot snapshot;
  snapshot.last_update_id = 10;
  snapshot.bids = {{100, 5}, {99, 4}, {96, 3}};
  snapshot.asks = {{101, 6}, {102, 7}, {105, 8}};
  const auto loaded = bridge.LoadSnapshot(snapshot, 2);
  require(loaded.action == BridgeAction::Applied &&
              loaded.loaded_bids == 2 && loaded.loaded_asks == 2 &&
              loaded.outside_bids == 1 && loaded.outside_asks == 1 &&
              loaded.bid_base_ticks == 98 && loaded.ask_base_ticks == 100,
          "snapshot window was not loaded correctly");
  bridge.SetLive();
  auto bbo = order_book.Bbo(7);
  require(bbo && bbo->bid.price == 100 && bbo->ask.price == 101,
          "snapshot BBO mismatch");

  const std::array<utils::md::Level, 1> bids{{{100, 0}}};
  const std::array<utils::md::Level, 1> asks{{{103, 9}}};
  require(bridge.Apply({11, 11, bids, asks}) == BridgeAction::Applied,
          "in-window depth update failed");
  bbo = order_book.Bbo(7);
  require(bbo && bbo->bid.price == 99 && bbo->ask.price == 101,
          "depth update did not mutate canonical ladder");

  const std::array<utils::md::Level, 1> outside_worse{{{106, 1}}};
  require(bridge.Apply({12, 12, {}, outside_worse}) ==
              BridgeAction::Applied &&
              bridge.outside_updates_ignored() == 1,
          "remote worse ask was not safely ignored");
  bbo = order_book.Bbo(7);
  require(bbo && bbo->bid.price == 99 && bbo->ask.price == 101,
          "ignored remote ask changed the maintained BBO");

  const std::array<utils::md::Level, 1> outside_better{{{101, 1}}};
  require(bridge.Apply({13, 13, outside_better, {}}) ==
              BridgeAction::Resync,
          "outside improving bid did not request resync");

  utils::md::OrderBook narrow_book(2);
  BookBridge narrow_bridge(narrow_book, 1);
  DepthSnapshot narrow_snapshot;
  narrow_snapshot.last_update_id = 20;
  narrow_snapshot.bids = {{100, 5}, {99, 4}};
  narrow_snapshot.asks = {{101, 6}, {102, 7}};
  require(narrow_bridge.LoadSnapshot(narrow_snapshot, 3).action ==
              BridgeAction::Applied,
          "narrow snapshot failed to load");
  const std::array<utils::md::Level, 1> delete_best{{{100, 0}}};
  require(narrow_bridge.Apply({21, 21, delete_best, {}}) ==
              BridgeAction::Resync,
          "deletion exposing a ladder boundary did not resync");
}

utils::md::BboEvent make_bbo(std::uint64_t sequence, std::int64_t bid,
                             std::int64_t ask,
                             std::uint32_t generation = 3) {
  utils::md::BboEvent event{};
  event.header.instrument_id = 7;
  event.header.book_generation = generation;
  event.header.source_seq = sequence;
  event.header.receive_tsc = sequence * 10;
  event.header.state = utils::md::BookState::Live;
  event.bid = {bid, 2};
  event.ask = {ask, 3};
  return event;
}

void test_bbo_overlay() {
  using namespace mds::book;
  BboOverlay overlay(SequenceDomain::Shared);
  auto canonical = make_bbo(10, 100, 101);
  auto leading = make_bbo(11, 101, 102);
  require(overlay.OnTicker(leading, canonical) == OverlayAction::Updated,
          "leading ticker did not update overlay");
  const auto effective = overlay.Effective(canonical);
  require(effective && effective->header.source_seq == 11 &&
              effective->bid.price == 101,
          "effective BBO did not prefer leading overlay");

  auto caught_up = make_bbo(11, 101, 102);
  require(overlay.OnCanonical(caught_up) == OverlayAction::Cleared &&
              !overlay.valid(),
          "matching canonical BBO did not clear overlay");

  auto divergent = make_bbo(12, 102, 103);
  require(overlay.OnTicker(divergent, caught_up) == OverlayAction::Updated,
          "second leading ticker was rejected");
  auto mismatched = make_bbo(12, 101, 103);
  require(overlay.OnCanonical(mismatched) == OverlayAction::Divergence &&
              overlay.divergence_count() == 1,
          "overlay divergence was not detected");

  BboOverlay overtaken(SequenceDomain::Shared);
  auto sequence_13 = make_bbo(13, 102, 103);
  require(overtaken.OnTicker(sequence_13, mismatched) ==
              OverlayAction::Updated &&
              overtaken.OnCanonical(make_bbo(14, 101, 102)) ==
                  OverlayAction::Cleared &&
              overtaken.divergence_count() == 0,
          "canonical sequence overtaking stale overlay caused divergence");

  BboOverlay independent(SequenceDomain::Independent);
  require(independent.OnTicker(leading, canonical) ==
              OverlayAction::TickerOnly &&
              independent.Effective(canonical)->bid.price ==
                  canonical.bid.price,
          "independent sequence domain polluted canonical BBO");
}

void test_binance_sync_and_sbe() {
  using namespace mds::exchange::binance;
  DepthSynchronizer spot(Profile::Spot);
  require(spot.on_update({100, 101}) == SyncAction::Buffer,
          "pre-snapshot update not buffered");
  require(spot.buffered_updates() == 1, "depth update was not retained");
  spot.inject_snapshot(100);
  require(spot.state() == BookSyncState::Bridging,
          "snapshot injection published Live before applying buffered data");
  int applied_updates = 0;
  require(spot.drain_buffered(
              &applied_updates,
              [](void *context, const DepthUpdate &) noexcept {
                ++*static_cast<int *>(context);
                return true;
              }) == SyncAction::BecameLive,
          "buffered spot updates were not applied");
  require(applied_updates == 1 && spot.state() == BookSyncState::Live &&
              spot.last_update_id() == 101,
          "buffered spot bridge failed");
  require(spot.on_update({102, 102}) == SyncAction::Apply,
          "spot continuation failed");
  require(spot.on_update({104, 104}) == SyncAction::Resnapshot,
          "spot gap not detected");

  DepthSynchronizer futures(Profile::UsdM);
  futures.inject_snapshot(20);
  require(futures.on_update({19, 20, 999}) == SyncAction::BecameLive,
          "USD-M first bridge incorrectly required pu to match snapshot");
  require(futures.on_update({21, 22, 20}) == SyncAction::Apply,
          "futures pu continuation failed");
  require(futures.on_update({23, 23, 19}) == SyncAction::Resnapshot,
          "futures pu gap not detected");

  DepthSynchronizer independent_spot(Profile::Spot);
  independent_spot.inject_snapshot(20);
  require(independent_spot.on_update({21, 22, 999}) ==
              SyncAction::BecameLive,
          "spot incorrectly applied USD-M pu sequencing");
  require(independent_spot.on_update({22, 23, 1}) == SyncAction::Apply,
          "spot U/u overlap continuation failed");

  DepthSynchronizer exhausted(Profile::Spot);
  exhausted.inject_snapshot(std::numeric_limits<std::uint64_t>::max());
  require(exhausted.on_update(
              {std::numeric_limits<std::uint64_t>::max(),
               std::numeric_limits<std::uint64_t>::max()}) ==
              SyncAction::Drop,
          "spot maximum sequence was not handled without overflow");

  DepthSynchronizer bounded(Profile::Spot, 2);
  require(bounded.on_update({1, 1}) == SyncAction::Buffer &&
              bounded.on_update({2, 2}) == SyncAction::Buffer &&
              bounded.on_update({3, 3}) == SyncAction::Resnapshot,
          "depth buffer bound was not enforced");

#ifdef MDS_HAS_SIMDJSON
  JsonParser json_parser(2, 3);
  BookTicker json_ticker;
  std::string error;
  require(json_parser.parse_book_ticker(
              R"({"u":42,"s":"BTCUSDT","b":"123.45","B":"1.000","a":"123.46","A":"2.000","E":1234})",
              json_ticker, error),
          error.c_str());
  require(json_ticker.price_exponent == -2 &&
              json_ticker.quantity_exponent == -3 &&
              std::string_view(json_ticker.symbol.data()) == "BTCUSDT" &&
              json_ticker.bid_price == 12'345 &&
              json_ticker.ask_quantity == 2'000,
          "JSON book ticker normalization failed");

  DepthUpdate json_depth;
  require(json_parser.parse_depth(
              R"({"U":100,"u":101,"s":"BTCUSDT","b":[["123.45","1.000"]],"a":[["123.46","2.000"]],"E":2000})",
              json_depth, error),
          error.c_str());
  require(json_depth.price_exponent == -2 &&
              json_depth.quantity_exponent == -3 &&
              std::string_view(json_depth.symbol.data()) == "BTCUSDT" &&
              json_depth.bids.size() == 1 && json_depth.asks.size() == 1,
          "JSON depth normalization failed");
#else
  std::string error;
#endif

  std::vector<std::byte> fixture(66);
  write_le<std::uint16_t>(fixture, 0, 50);
  write_le<std::uint16_t>(fixture, 2,
                          SpotSbeDecoder::best_bid_ask_template_id);
  write_le<std::uint16_t>(fixture, 4, SpotSbeDecoder::schema_id);
  write_le<std::uint16_t>(fixture, 6, SpotSbeDecoder::schema_version);
  write_le<std::int64_t>(fixture, 8, 1'234'000);
  write_le<std::int64_t>(fixture, 16, 42);
  write_le<std::int8_t>(fixture, 24, -2);
  write_le<std::int8_t>(fixture, 25, -3);
  write_le<std::int64_t>(fixture, 26, 12'345);
  write_le<std::int64_t>(fixture, 34, 1'000);
  write_le<std::int64_t>(fixture, 42, 12'346);
  write_le<std::int64_t>(fixture, 50, 2'000);
  fixture[58] = std::byte{7};
  std::memcpy(fixture.data() + 59, "BTCUSDT", 7);
  SpotSbeDecoder decoder;
  BookTicker ticker;
  require(decoder.decode_book_ticker(fixture, ticker, error),
          error.c_str());
  require(capability(Profile::Spot).sbe == Availability::Available &&
              ticker.update_id == 42 && ticker.bid_price == 12'345 &&
              ticker.ask_price == 12'346 && ticker.price_exponent == -2 &&
              ticker.quantity_exponent == -3 &&
              ticker.event_time_ms == 1'234 &&
              std::string_view(ticker.symbol.data()) == "BTCUSDT",
          "official SBE best-bid-ask fixture decoded incorrectly");

  std::vector<std::byte> depth_fixture(82);
  write_le<std::uint16_t>(depth_fixture, 0, 26);
  write_le<std::uint16_t>(depth_fixture, 2,
                          SpotSbeDecoder::depth_diff_template_id);
  write_le<std::uint16_t>(depth_fixture, 4, SpotSbeDecoder::schema_id);
  write_le<std::uint16_t>(depth_fixture, 6,
                          SpotSbeDecoder::schema_version);
  write_le<std::int64_t>(depth_fixture, 8, 2'000'000);
  write_le<std::int64_t>(depth_fixture, 16, 100);
  write_le<std::int64_t>(depth_fixture, 24, 101);
  write_le<std::int8_t>(depth_fixture, 32, -2);
  write_le<std::int8_t>(depth_fixture, 33, -3);
  write_le<std::uint16_t>(depth_fixture, 34, 16);
  write_le<std::uint16_t>(depth_fixture, 36, 1);
  write_le<std::int64_t>(depth_fixture, 38, 12'345);
  write_le<std::int64_t>(depth_fixture, 46, 500);
  write_le<std::uint16_t>(depth_fixture, 54, 16);
  write_le<std::uint16_t>(depth_fixture, 56, 1);
  write_le<std::int64_t>(depth_fixture, 58, 12'346);
  write_le<std::int64_t>(depth_fixture, 66, 600);
  depth_fixture[74] = std::byte{7};
  std::memcpy(depth_fixture.data() + 75, "BTCUSDT", 7);
  DepthUpdate decoded_depth;
  require(decoder.decode_depth(depth_fixture, decoded_depth, error),
          error.c_str());
  require(std::string_view(decoded_depth.symbol.data()) == "BTCUSDT" &&
              decoded_depth.first_update_id == 100 &&
              decoded_depth.final_update_id == 101 &&
              decoded_depth.bids.size() == 1 &&
              decoded_depth.asks.size() == 1 &&
              decoded_depth.bids[0].price == 12'345 &&
              decoded_depth.asks[0].quantity == 600 &&
              decoded_depth.price_exponent == -2 &&
              decoded_depth.quantity_exponent == -3,
          "official SBE depth fixture decoded incorrectly");

  fixture[4] = std::byte{2};
  require(!decoder.decode_book_ticker(fixture, ticker, error),
          "unsupported SBE schema was accepted");
}

void test_binance_rest_and_stream_profiles() {
  using namespace mds::exchange::binance;
#ifdef MDS_HAS_SIMDJSON
  RestParser rest;
  std::string error;
  InstrumentMetadata spot;
  require(rest.parse_exchange_info(
              Profile::Spot,
              read_fixture("binance_spot_exchange_info.json"), "BTCUSDT",
              spot, error),
          error.c_str());
  require(spot.profile == Profile::Spot && spot.price_scale == 2 &&
              spot.quantity_scale == 5 &&
              spot.price_filter.tick_size == 1 &&
              spot.price_filter.max_price == 100'000'000 &&
              spot.lot_size.step_size == 1 &&
              spot.settle_asset == "USDT",
          "spot exchangeInfo metadata normalization failed");

  DepthSnapshot spot_snapshot;
  require(rest.parse_depth(Profile::Spot,
                           read_fixture("binance_spot_depth.json"), spot,
                           spot_snapshot, error),
          error.c_str());
  require(spot_snapshot.last_update_id == 1'027'024 &&
              spot_snapshot.price_exponent == -2 &&
              spot_snapshot.quantity_exponent == -5 &&
              spot_snapshot.bids.size() == 2 &&
              spot_snapshot.bids[0].price == 400 &&
              spot_snapshot.bids[0].quantity == 43'100'000,
          "spot depth snapshot normalization failed");

  InstrumentMetadata usdm;
  require(rest.parse_exchange_info(
              Profile::UsdM,
              read_fixture("binance_usdm_exchange_info.json"), "BTCUSDT",
              usdm, error),
          error.c_str());
  require(usdm.profile == Profile::UsdM && usdm.price_scale == 1 &&
              usdm.quantity_scale == 3 &&
              usdm.contract_type == "PERPETUAL" &&
              usdm.onboard_time_ms == 1'569'398'400'000ULL &&
              usdm.delivery_time_ms == 4'133'404'800'000ULL &&
              usdm.contract_size == 1 && usdm.settle_asset == "USDT",
          "USD-M contract metadata normalization failed");

  DepthSnapshot usdm_snapshot;
  require(rest.parse_depth(Profile::UsdM,
                           read_fixture("binance_usdm_depth.json"), usdm,
                           usdm_snapshot, error),
          error.c_str());
  require(usdm_snapshot.last_update_id == 160 &&
              usdm_snapshot.bids[0].price == 1 &&
              usdm_snapshot.bids[0].quantity == 10'000,
          "USD-M depth snapshot normalization failed");

  require(!rest.parse_depth(Profile::UsdM,
                            read_fixture("binance_overflow_depth.json"), usdm,
                            usdm_snapshot, error),
          "overflowing fixed-point depth was accepted");
  require(!rest.parse_depth(
              Profile::UsdM,
              R"({"lastUpdateId":162,"bids":[["0.11","1.000"]],"asks":[]})",
              usdm, usdm_snapshot, error),
          "depth finer than PRICE_FILTER tick precision was accepted");
  InstrumentMetadata invalid;
  require(!rest.parse_exchange_info(
              Profile::Spot,
              R"({"symbols":[{"symbol":"BTCUSDT","status":"TRADING","baseAsset":"BTC","quoteAsset":"USDT","filters":[]}]})",
              "BTCUSDT", invalid, error),
          "exchangeInfo without required filters was accepted");

  CombinedStreamParser combined;
  std::string path;
  require(build_combined_stream_path(Profile::Spot, "BTCUSDT", true, true,
                                     path, error) &&
              path ==
                  "/stream?streams=btcusdt@bookTicker/"
                  "btcusdt@depth@100ms",
          "spot combined stream path was built incorrectly");
  require(build_combined_stream_path(Profile::UsdM, "ethusdt", false, true,
                                     path, error, "250ms") &&
              path == "/stream?streams=ethusdt@depth@250ms",
          "USD-M combined stream path was built incorrectly");

  CombinedMessageView view;
  const std::string envelope =
      R"({"stream":"btcusdt@bookTicker","data":{"u":400900217,"s":"BTCUSDT","b":"4.00000000","B":"1.00000000","a":"4.01000000","A":"2.00000000"}})";
  require(combined.unpack(envelope, view, error), error.c_str());
  require(view.route.symbol == "btcusdt" &&
              view.route.kind == StreamKind::BookTicker &&
              view.data ==
                  R"({"u":400900217,"s":"BTCUSDT","b":"4.00000000","B":"1.00000000","a":"4.01000000","A":"2.00000000"})" &&
              view.data.data() != envelope.data(),
          "combined stream envelope did not expose parser-owned data view");
  require(!combined.unpack(
              R"({"stream":"BTCUSDT@depth@100ms","data":{}})", view, error),
          "non-canonical combined stream route was accepted");
#endif

  const auto spot_capability = capability(Profile::Spot);
  const auto usdm_capability = capability(Profile::UsdM);
  require(spot_capability.exchange_info_path == "/api/v3/exchangeInfo" &&
              spot_capability.depth_path == "/api/v3/depth" &&
              spot_capability.json &&
              !spot_capability.requires_previous_final_id &&
              usdm_capability.exchange_info_path == "/fapi/v1/exchangeInfo" &&
              usdm_capability.depth_path == "/fapi/v1/depth" &&
              usdm_capability.requires_previous_final_id &&
              usdm_capability.sbe == Availability::Unavailable,
          "Binance profile endpoint/capability declaration is incorrect");
}

void test_websocket() {
  using namespace mds::network;
  std::string error;
  require(!validate_websocket_extensions(
              "HTTP/1.1 101\r\nSec-WebSocket-Extensions: "
              "permessage-deflate\r\n\r\n",
              error),
          "permessage-deflate was accepted");
  error.clear();
  require(validate_websocket_extensions("HTTP/1.1 101\r\n\r\n", error),
          "extension-free response rejected");

  WebSocketParser parser(128);
  const std::array<std::byte, 7> frame{
      std::byte{0x81}, std::byte{0x05}, std::byte{'h'}, std::byte{'e'},
      std::byte{'l'},  std::byte{'l'},  std::byte{'o'}};
  int frames = 0;
  require(parser.feed({frame.data(), 3},
                      [&frames](const WsFrameView &) {
                        ++frames;
                        return true;
                      },
                      error),
          "partial frame rejected");
  require(frames == 0, "partial frame emitted");
  require(parser.feed({frame.data() + 3, 4},
                      [&frames](const WsFrameView &view) {
                        ++frames;
                        return view.opcode == WsOpcode::Text &&
                               view.payload.size() == 5;
                      },
                      error),
          "complete frame rejected");
  require(frames == 1, "complete frame not emitted");

  WebSocketParser fragmented(128);
  const std::array<std::byte, 13> fragments{
      std::byte{0x01}, std::byte{0x03}, std::byte{'h'}, std::byte{'e'},
      std::byte{'l'},  std::byte{0x89}, std::byte{0x01}, std::byte{'?'},
      std::byte{0x80}, std::byte{0x03}, std::byte{'l'}, std::byte{'o'},
      std::byte{'!'}};
  int messages = 0;
  require(fragmented.feed(
              fragments,
              [&messages](const WsFrameView &view) {
                if (view.opcode == WsOpcode::Ping) {
                  return view.payload.size() == 1;
                }
                ++messages;
                return view.opcode == WsOpcode::Text &&
                       view.payload.size() == 6;
              },
              error),
          "fragmented message with interleaved ping rejected");
  require(messages == 1, "continuations were not reassembled");

  const std::array<std::byte, 2> compressed{std::byte{0xC1}, std::byte{0}};
  require(!parser.feed(compressed, [](const WsFrameView &) { return true; },
                       error),
          "RSV1 compressed frame accepted");

  WebSocketParser protocol(128);
  const std::array<std::byte, 2> reserved{std::byte{0x83}, std::byte{0}};
  require(!protocol.feed(reserved, [](const WsFrameView &) { return true; },
                         error),
          "reserved opcode accepted");
  protocol.reset();
  const std::array<std::byte, 10> invalid_length{
      std::byte{0x82}, std::byte{0x7f}, std::byte{0x80}, std::byte{0},
      std::byte{0},    std::byte{0},    std::byte{0},    std::byte{0},
      std::byte{0},    std::byte{0}};
  require(!protocol.feed(invalid_length,
                         [](const WsFrameView &) { return true; }, error),
          "invalid 64-bit length accepted");
}

void test_service_readiness_and_sbe_rejection() {
  using namespace mds::api;
  MdsConfig config;
  config.venues.push_back({});
  config.venues.front().websocket_endpoint = "wss://127.0.0.1:1/ws";
  config.venues.front().rest_endpoint = "https://127.0.0.1:1";
  config.venues.front().protocol = WireProtocol::Sbe;
  config.shm.unlink_on_shutdown = true;
  const auto symbol = "TEST" + std::to_string(::getpid()) + "USDT";
  auto initialized = init(config);
  require(bool(initialized), initialized.message.c_str());
  auto ticker = subticker({.venue = "binance",
                           .product = ProductType::Spot,
                           .symbol = symbol});
  require(bool(ticker), ticker.message.c_str());
  OrderBookSubscription book_request;
  book_request.venue = "binance";
  book_request.product = ProductType::Spot;
  book_request.symbol = symbol;
  book_request.depth = 100;
  book_request.ladder_levels_per_side = 64;
  auto book = suborderbook(book_request);
  require(bool(book), book.message.c_str());
  mds::transport::RingOptions attach;
  attach.name = mds::publish::make_publisher_segment_name(
      "/selfquant.mds", "spot", symbol, "ticker");
  attach.create = false;
  require(bool(mds::transport::SharedRing::open(attach)),
          "public API did not start a publishing session");
  require(query_state(ticker.value) != SubscriptionState::Stopped &&
              query_state(book.value) != SubscriptionState::Stopped,
          "started public subscriptions were unexpectedly stopped");
  require(bool(unsubscribe(ticker.value)) &&
              query_state(ticker.value) == SubscriptionState::Stopped &&
              query_state(book.value) != SubscriptionState::Stopped,
          "unsubscribing one handle stopped a reused session");
  shutdown();
}

void test_binance_session_offline_bridge() {
#ifdef MDS_HAS_SIMDJSON
  using mds::network::WebSocketClientState;
  require(!mds::service::BinanceSession::snapshot_allowed(
              WebSocketClientState::SendingUpgrade) &&
              !mds::service::BinanceSession::snapshot_allowed(
                  WebSocketClientState::ReadingUpgrade) &&
              mds::service::BinanceSession::snapshot_allowed(
                  WebSocketClientState::Open),
          "snapshot gate did not defer until WebSocket Open");

  mds::service::SessionManager manager;
  mds::service::BinanceSessionOptions options;
  options.profile = mds::exchange::binance::Profile::Spot;
  options.symbol = "BTCUSDT";
  options.publish = false;
  auto created = manager.create(std::move(options), false);
  require(bool(created), created.message.c_str());
  auto *session = created.value;
  std::string error;
  require(session->ingest_websocket_message(
              R"({"stream":"btcusdt@depth@100ms","data":{"U":1027025,"u":1027025,"s":"BTCUSDT","b":[["4.00000000","430.00000000"]],"a":[],"E":2000}})",
              error),
          error.c_str());
  require(session->state() != mds::service::BinanceSessionState::Live,
          "session became Live while depth was only raw-buffered");
  require(session->ingest_exchange_info(
              read_fixture("binance_spot_exchange_info.json"), error),
          error.c_str());
  require(session->state() != mds::service::BinanceSessionState::Live,
          "session became Live before a REST snapshot");
  require(session->ingest_depth_snapshot(
              read_fixture("binance_spot_depth.json"), error),
          error.c_str());
  require(session->state() == mds::service::BinanceSessionState::Live &&
              session->metrics().depth_updates == 1,
          "session did not apply buffered depth before entering Live");

  mds::service::SessionManager waiting_manager;
  mds::service::BinanceSessionOptions waiting_options;
  waiting_options.profile = mds::exchange::binance::Profile::Spot;
  waiting_options.symbol = "BTCUSDT";
  waiting_options.publish = false;
  auto waiting_created =
      waiting_manager.create(std::move(waiting_options), false);
  require(bool(waiting_created), waiting_created.message.c_str());
  auto *waiting = waiting_created.value;
  require(waiting->ingest_exchange_info(
              read_fixture("binance_spot_exchange_info.json"), error),
          error.c_str());
  require(waiting->ingest_depth_snapshot(
              read_fixture("binance_spot_depth.json"), error),
          error.c_str());
  require(waiting->state() == mds::service::BinanceSessionState::Bridging,
          "snapshot without a bridging delta was treated as Live");
#endif
}

void test_session_resync_and_overlay_divergence() {
#ifdef MDS_HAS_SIMDJSON
  using mds::service::BinanceSessionState;
  mds::service::SessionManager manager;
  mds::service::BinanceSessionOptions options;
  options.profile = mds::exchange::binance::Profile::Spot;
  options.symbol = "BTCUSDT";
  options.publish = false;
  options.rest_endpoint = "https://127.0.0.1:1";
  auto created = manager.create(std::move(options), false);
  require(bool(created), created.message.c_str());
  auto *session = created.value;
  std::string error;
  require(session->ingest_exchange_info(
              read_fixture("binance_spot_exchange_info.json"), error) &&
              session->ingest_depth_snapshot(
                  read_fixture("binance_spot_depth.json"), error),
          error.c_str());
  require(session->state() == BinanceSessionState::Bridging,
          "snapshot entered Live before the first delta");
  require(session->ingest_websocket_message(
              R"({"stream":"btcusdt@depth@100ms","data":{"U":1027025,"u":1027025,"s":"BTCUSDT","b":[["4.00","431.00000"]],"a":[],"E":2000}})",
              error) &&
              session->state() == BinanceSessionState::Live,
          "first bridging delta did not make the session Live");
  const auto live_generation = session->generation();
  require(session->ingest_websocket_message(
              R"({"stream":"btcusdt@bookTicker","data":{"u":1027026,"s":"BTCUSDT","b":"4.02","B":"1.00000","a":"4.03","A":"2.00000","E":2001}})",
              error),
          error.c_str());
  require(session->state() == BinanceSessionState::Live,
          "leading same-domain ticker unexpectedly changed session state");
  const auto depth_updates_before_catchup = session->metrics().depth_updates;
  require(session->ingest_websocket_message(
              R"({"stream":"btcusdt@depth@100ms","data":{"U":1027026,"u":1027026,"s":"BTCUSDT","b":[["4.00","432.00000"]],"a":[],"E":2002}})",
              error),
          error.c_str());
  require(session->metrics().depth_updates ==
              depth_updates_before_catchup + 1U,
          "canonical catch-up delta was not applied");
  require(session->metrics().resyncs == 1,
          "overlay divergence did not request a resync");
  require(session->generation() == live_generation + 1U,
          "overlay divergence did not advance exactly one generation");
  require(session->state() != BinanceSessionState::Live,
          "overlay divergence left the session Live");

  const auto first_resync_generation = session->generation();
  require(session->ingest_depth_snapshot(
              read_fixture("binance_spot_depth.json"), error) &&
              session->ingest_websocket_message(
              R"({"stream":"btcusdt@depth@100ms","data":{"U":1027030,"u":1027030,"s":"BTCUSDT","b":[],"a":[],"E":2003}})",
              error),
          error.c_str());
  require(session->generation() == first_resync_generation + 1U &&
              session->state() != BinanceSessionState::Live &&
              session->metrics().resyncs == 2,
          "a later resync did not advance exactly one non-Live generation");
#endif
}

void test_session_publish_failure_and_late_reader() {
#ifdef MDS_HAS_SIMDJSON
  using namespace mds;
  service::SessionManager manager;
  service::BinanceSessionOptions options;
  options.profile = exchange::binance::Profile::Spot;
  options.symbol = "BTCUSDT";
  options.shm_prefix = "/mds.session." + std::to_string(::getpid());
  options.websocket_endpoint = "wss://127.0.0.1:1/ws";
  options.rest_endpoint = "https://127.0.0.1:1";
  options.ladder_levels_per_side = 16;
  options.max_ladder_levels_per_side = 16;
  options.ring.mode = mds::api::RingMode::Lossless;
  options.ring.ring_bytes = 4096;
  options.ring.max_record_bytes = 512;
  options.ring.max_readers = 4;
  options.ring.unlink_on_close = true;
  auto created = manager.create(std::move(options), true);
  require(bool(created), created.message.c_str());
  auto *session = created.value;
  require(session->ticker_segment() ==
                  publish::make_publisher_segment_name(
                      "/mds.session." + std::to_string(::getpid()), "spot",
                      "BTCUSDT", "ticker") &&
              session->orderbook_segment() ==
                  publish::make_publisher_segment_name(
                      "/mds.session." + std::to_string(::getpid()), "spot",
                      "BTCUSDT", "orderbook"),
          "Binance session did not preserve its custom shared-memory prefix");
  std::string error;
  require(session->ingest_exchange_info(
              read_fixture("binance_spot_exchange_info.json"), error) &&
              session->ingest_depth_snapshot(
                  read_fixture("binance_spot_depth.json"), error) &&
              session->ingest_websocket_message(
                  R"({"stream":"btcusdt@depth@100ms","data":{"U":1027025,"u":1027025,"s":"BTCUSDT","b":[["4.00","431.00000"]],"a":[],"E":2000}})",
                  error) &&
              session->state() == service::BinanceSessionState::Live,
          error.c_str());

  transport::RingOptions ticker_attach;
  ticker_attach.name = std::string(session->ticker_segment());
  ticker_attach.create = false;
  auto ticker_opened = transport::SharedRing::open(ticker_attach);
  require(bool(ticker_opened), ticker_opened.message.c_str());
  auto ticker_ring = std::move(ticker_opened.value);
  auto ticker_registration = ticker_ring.register_reader(
      transport::process_start_marker(::getpid()), now_ns());
  require(bool(ticker_registration), ticker_registration.message.c_str());
  auto ticker_reader = ticker_registration.value;

  transport::RingOptions book_attach = ticker_attach;
  book_attach.name = std::string(session->orderbook_segment());
  auto book_opened = transport::SharedRing::open(book_attach);
  require(bool(book_opened), book_opened.message.c_str());
  auto book_ring = std::move(book_opened.value);
  auto book_registration = book_ring.register_reader(
      transport::process_start_marker(::getpid()), now_ns());
  require(bool(book_registration), book_registration.message.c_str());
  auto book_reader = book_registration.value;

  require(session->poll_reader_changes(),
          "late-reader image republish failed");
  auto ticker_instrument = ticker_ring.read(ticker_reader);
  require(bool(ticker_instrument) &&
              ticker_instrument.value->type ==
                  static_cast<std::uint32_t>(
                      utils::md::MessageType::InstrumentUpdate),
          "late ticker reader did not receive Instrument first");
  require(bool(ticker_instrument.value.commit()),
          "late ticker Instrument commit failed");
  auto ticker_bbo = ticker_ring.read(ticker_reader);
  require(bool(ticker_bbo) &&
              ticker_bbo.value->type ==
                  static_cast<std::uint32_t>(utils::md::MessageType::Bbo),
          "late ticker reader did not receive BBO");
  require(bool(ticker_bbo.value.commit()), "late ticker BBO commit failed");

  auto book_instrument = book_ring.read(book_reader);
  require(bool(book_instrument) &&
              book_instrument.value->type ==
                  static_cast<std::uint32_t>(
                      utils::md::MessageType::InstrumentUpdate),
          "late order-book reader did not receive Instrument first");
  require(bool(book_instrument.value.commit()),
          "late order-book Instrument commit failed");
  auto snapshot_begin = book_ring.read(book_reader);
  require(bool(snapshot_begin) &&
              snapshot_begin.value->type ==
                  static_cast<std::uint32_t>(
                      utils::md::MessageType::SnapshotBegin),
          "late order-book reader did not receive a Live snapshot");

  const auto live_generation = session->generation();
  for (std::uint64_t sequence = 1027027;
       sequence < 1027127 && session->generation() == live_generation;
       ++sequence) {
    const auto message =
        std::string(R"({"stream":"btcusdt@bookTicker","data":{"u":)") +
        std::to_string(sequence) +
        R"(,"s":"BTCUSDT","b":"4.00","B":"1.00000","a":"4.01","A":"2.00000","E":2004}})";
    require(session->ingest_websocket_message(message, error), error.c_str());
  }
  require(session->metrics().publish_errors != 0 &&
              session->generation() > live_generation &&
              session->state() != service::BinanceSessionState::Live,
          "publisher backpressure left the session Live");
#endif
}

} // namespace

int main() {
  try {
    test_shared_ring_and_lease();
    test_shared_ring_try_read();
    test_shared_ring_overwrite_and_resync();
    test_shared_ring_initialization_and_header_validation();
    test_shared_ring_crc_detection();
    test_arbiter();
    test_snapshot_bridge();
    test_bbo_overlay();
    test_binance_sync_and_sbe();
    test_binance_rest_and_stream_profiles();
    test_websocket();
    test_service_readiness_and_sbe_rejection();
    test_binance_session_offline_bridge();
    test_session_resync_and_overlay_divergence();
    test_session_publish_failure_and_late_reader();
    std::cout << "all mds tests passed\n";
    return 0;
  } catch (const std::exception &exception) {
    std::cerr << "test failure: " << exception.what() << '\n';
    return 1;
  }
}
