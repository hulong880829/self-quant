#include "mds/publish/wire_publisher.h"

#include <array>
#include <atomic>
#include <chrono>
#include <cstring>
#include <cstdlib>
#include <fcntl.h>
#include <iostream>
#include <new>
#include <stdexcept>
#include <string>
#include <sys/mman.h>
#include <sys/stat.h>
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
void *operator new[](std::size_t size) { return ::operator new(size); }
void *operator new(std::size_t size, const std::nothrow_t &) noexcept {
  try {
    return ::operator new(size);
  } catch (...) {
    return nullptr;
  }
}
void *operator new[](std::size_t size,
                     const std::nothrow_t &) noexcept {
  return ::operator new(size, std::nothrow);
}
void operator delete(void *memory) noexcept { std::free(memory); }
void operator delete[](void *memory) noexcept { std::free(memory); }
void operator delete(void *memory, std::size_t) noexcept {
  std::free(memory);
}
void operator delete[](void *memory, std::size_t) noexcept {
  std::free(memory);
}
void operator delete(void *memory,
                     const std::nothrow_t &) noexcept {
  std::free(memory);
}
void operator delete[](void *memory,
                       const std::nothrow_t &) noexcept {
  std::free(memory);
}

namespace {

void require(bool condition, const char *message) {
  if (!condition) {
    throw std::runtime_error(message);
  }
}

std::uint64_t now_ns() {
  return static_cast<std::uint64_t>(
      std::chrono::steady_clock::now().time_since_epoch().count());
}

mds::transport::RingOptions ring_options(std::string name,
                                         std::size_t ring_bytes = 16384) {
  mds::transport::RingOptions options;
  options.name = std::move(name);
  options.ring_bytes = ring_bytes;
  options.max_record_bytes = 512;
  options.max_readers = 2;
  options.unlink_on_close = true;
  return options;
}

utils::md::EventHeader event_header(std::uint64_t bus_seq = 100) {
  return {.instrument_id = 42,
          .book_generation = 9,
          .source_seq = 777,
          .bus_seq = bus_seq,
          .exchange_ts_ns = 1'000'000,
          .receive_tsc = 2'000'000,
          .publish_tsc = 3'000'000,
          .state = utils::md::BookState::Live,
          .source_id = 2};
}

struct Visitor final : utils::md::wire::RecordVisitor {
  std::vector<utils::md::MessageType> types;
  std::vector<std::uint16_t> chunk_sizes;
  std::vector<utils::md::Side> chunk_sides;
  std::vector<std::uint32_t> begin_counts;
  std::vector<std::uint64_t> bus_sequences;
  utils::md::wire::RecordHeader last_header{};

  bool OnInstrument(
      const utils::md::wire::InstrumentUpdateRecord &record) noexcept override {
    types.push_back(utils::md::MessageType::InstrumentUpdate);
    last_header = record.header;
    bus_sequences.push_back(record.header.bus_seq);
    return record.instrument.instrument_id == record.header.instrument_id;
  }
  bool OnBbo(const utils::md::wire::BboRecord &record) noexcept override {
    types.push_back(utils::md::MessageType::Bbo);
    last_header = record.header;
    bus_sequences.push_back(record.header.bus_seq);
    return record.bid_price == 100 && record.ask_price == 101;
  }
  bool OnTicker(const utils::md::wire::TickerRecord &record) noexcept override {
    types.push_back(utils::md::MessageType::Ticker);
    last_header = record.header;
    bus_sequences.push_back(record.header.bus_seq);
    return record.bid_price == 100 && record.ask_price == 101 &&
           record.open_price == 95 && record.high_price == 110 &&
           record.low_price == 90 && record.close_price == 105;
  }
  bool OnDelta(const utils::md::wire::DeltaRecord &record) noexcept override {
    types.push_back(utils::md::MessageType::BookDelta);
    last_header = record.header;
    bus_sequences.push_back(record.header.bus_seq);
    return record.side == static_cast<std::uint8_t>(utils::md::Side::Bid);
  }
  bool OnSnapshotBegin(
      const utils::md::wire::SnapshotBeginRecord &record) noexcept override {
    types.push_back(utils::md::MessageType::SnapshotBegin);
    begin_counts.push_back(record.item_count);
    last_header = record.header;
    bus_sequences.push_back(record.header.bus_seq);
    return true;
  }
  bool OnSnapshotChunk(
      const utils::md::wire::SnapshotChunkRecord &record) noexcept override {
    types.push_back(utils::md::MessageType::SnapshotChunk);
    chunk_sizes.push_back(record.level_count);
    chunk_sides.push_back(static_cast<utils::md::Side>(record.side));
    last_header = record.header;
    bus_sequences.push_back(record.header.bus_seq);
    return true;
  }
  bool OnSnapshotEnd(
      const utils::md::wire::SnapshotEndRecord &record) noexcept override {
    types.push_back(utils::md::MessageType::SnapshotEnd);
    last_header = record.header;
    bus_sequences.push_back(record.header.bus_seq);
    return true;
  }
};

void test_publish_decode_order_and_fragmentation() {
  using namespace mds;
  const auto name = "/mds.publisher." + std::to_string(::getpid());
  auto opened = transport::SharedRing::open(ring_options(name));
  require(bool(opened), opened.message.c_str());
  auto ring = std::move(opened.value);
  auto registration = ring.register_reader(
      transport::process_start_marker(::getpid()), now_ns());
  require(bool(registration), registration.message.c_str());
  auto reader = registration.value;
  publish::WirePublisher publisher(ring);
  require(publisher.producer_epoch() == ring.epoch() &&
              publisher.producer_epoch() != 0,
          "publisher did not expose the ring producer epoch");

  auto header = event_header();
  utils::md::Instrument instrument{};
  instrument.instrument_id = header.instrument_id;
  instrument.venue = utils::md::Venue::Binance;
  instrument.product_type = utils::md::ProductType::Spot;
  instrument.tick_size = 1;
  instrument.lot_size = 1;
  require(bool(publisher.publish_instrument(header, instrument)),
          "instrument publish failed");

  utils::md::BboEvent bbo{header, {100, 2}, {101, 3}};
  require(bool(publisher.publish_bbo(bbo)), "BBO publish failed");
  utils::md::TickerEvent ticker{};
  ticker.header = header;
  ticker.bid = {100, 2};
  ticker.ask = {101, 3};
  ticker.last_price = 100;
  ticker.last_quantity = 4;
  ticker.mark_price = 102;
  ticker.index_price = 103;
  ticker.funding_rate = 5;
  ticker.open_price = 95;
  ticker.high_price = 110;
  ticker.low_price = 90;
  ticker.close_price = 105;
  require(bool(publisher.publish_ticker(ticker)), "ticker publish failed");
  utils::md::BookDelta delta{header, utils::md::Side::Bid, {}, {99, 4}};
  require(bool(publisher.publish_delta(delta)), "delta publish failed");

  std::array<utils::md::Level, 24> levels24{};
  for (std::size_t i = 0; i < levels24.size(); ++i) {
    levels24[i] = {static_cast<std::int64_t>(200 + i), 1};
  }
  require(bool(publisher.publish_snapshot(header, utils::md::Side::Bid,
                                          levels24, 0x12345678U)),
          "24-level snapshot publish failed");

  utils::md::OrderBook book(13);
  book.Reset(88, 101, 1, header.book_generation);
  for (std::int64_t price = 88; price <= 100; ++price) {
    require(book.Apply(utils::md::Side::Bid, price, 1) ==
                utils::md::LadderResult::Ok,
            "bid setup failed");
  }
  for (std::int64_t price = 101; price <= 112; ++price) {
    require(book.Apply(utils::md::Side::Ask, price, 1) ==
                utils::md::LadderResult::Ok,
            "ask setup failed");
  }
  require(bool(publisher.publish_snapshot(header, book, 0xabcdef01U)),
          "25-level OrderBook snapshot publish failed");

  const std::array expected{
      utils::md::MessageType::InstrumentUpdate,
      utils::md::MessageType::Bbo,
      utils::md::MessageType::Ticker,
      utils::md::MessageType::BookDelta,
      utils::md::MessageType::SnapshotBegin,
      utils::md::MessageType::SnapshotChunk,
      utils::md::MessageType::SnapshotEnd,
      utils::md::MessageType::SnapshotBegin,
      utils::md::MessageType::SnapshotChunk,
      utils::md::MessageType::SnapshotChunk,
      utils::md::MessageType::SnapshotEnd};
  Visitor visitor;
  for (const auto expected_type : expected) {
    auto read = ring.read(reader);
    require(bool(read), read.message.c_str());
    utils::md::wire::RecordHeader inner{};
    require(utils::md::wire::ValidateHeader(read.value->payload, &inner) ==
                utils::md::wire::CodecError::Ok,
            "published wire header did not validate");
    require(read.value->type == inner.message_type &&
                read.value->type == static_cast<std::uint32_t>(expected_type),
            "outer ring type differs from inner wire type");
    require(utils::md::wire::Decode(read.value->payload, visitor) ==
                utils::md::wire::CodecError::Ok,
            "published record did not decode");
    require(inner.instrument_id == header.instrument_id &&
                inner.source_seq == header.source_seq &&
                inner.exchange_ts_ns == header.exchange_ts_ns &&
                inner.receive_tsc == header.receive_tsc &&
                inner.publish_tsc == header.publish_tsc &&
                inner.book_generation == header.book_generation &&
                inner.state == static_cast<std::uint8_t>(header.state) &&
                inner.source_id == header.source_id,
            "wire publisher omitted header fields");
    require(bool(read.value.commit()), "record commit failed");
  }
  require(visitor.types.size() == expected.size() &&
              visitor.begin_counts == std::vector<std::uint32_t>({24, 25}) &&
              visitor.chunk_sizes ==
                  std::vector<std::uint16_t>({24, 13, 12}) &&
              visitor.chunk_sides ==
                  std::vector<utils::md::Side>({utils::md::Side::Bid,
                                                utils::md::Side::Bid,
                                                utils::md::Side::Ask}),
          "snapshot records were not split into 24-level chunks");
  require(visitor.bus_sequences ==
              std::vector<std::uint64_t>(
                  {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}) &&
              publisher.current_bus_seq() == 11,
          "publisher bus sequences were not unique and monotonic");
}

void test_late_reader_detection() {
  using namespace mds;
  const auto name = "/mds.publisher-reader." + std::to_string(::getpid());
  auto opened = transport::SharedRing::open(ring_options(name));
  require(bool(opened), opened.message.c_str());
  auto ring = std::move(opened.value);
  publish::WirePublisher publisher(ring);
  int notifications = 0;
  publisher.set_reader_change_hook(
      &notifications,
      [](void *context, publish::WirePublisher &) noexcept {
        ++*static_cast<int *>(context);
      });
  const auto initial = publisher.reader_registry_generation();
  auto registration = ring.register_reader(
      transport::process_start_marker(::getpid()), now_ns());
  require(bool(registration), registration.message.c_str());
  require(ring.active_reader_count() == 1 &&
              publisher.reader_registry_generation() != initial &&
              publisher.poll_reader_change() && notifications == 1 &&
              !publisher.poll_reader_change(),
          "publisher did not detect a late reader attachment");
}

void test_crc_and_backpressure() {
  using namespace mds;
  const auto crc_name = "/mds.publisher-crc." + std::to_string(::getpid());
  auto opened = transport::SharedRing::open(ring_options(crc_name, 4096));
  require(bool(opened), opened.message.c_str());
  auto ring = std::move(opened.value);
  auto registered = ring.register_reader(
      transport::process_start_marker(::getpid()), now_ns());
  require(bool(registered), registered.message.c_str());
  auto reader = registered.value;
  publish::WirePublisher publisher(ring);
  utils::md::BboEvent bbo{event_header(), {100, 2}, {101, 3}};
  require(bool(publisher.publish_bbo(bbo)), "CRC BBO publish failed");

  const int fd = ::shm_open(crc_name.c_str(), O_RDWR, 0600);
  require(fd >= 0, "failed to reopen publisher segment");
  struct stat status {};
  require(::fstat(fd, &status) == 0, "failed to stat publisher segment");
  void *mapping = ::mmap(nullptr, static_cast<std::size_t>(status.st_size),
                         PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
  require(mapping != MAP_FAILED, "failed to map publisher segment");
  auto *segment = static_cast<transport::SegmentHeader *>(mapping);
  auto *record = reinterpret_cast<transport::RecordHeader *>(
      static_cast<std::byte *>(mapping) + segment->header_bytes);
  reinterpret_cast<std::byte *>(record + 1)[0] ^= std::byte{0xff};
  const auto corrupt = ring.read(reader);
  require(!corrupt && corrupt.error == api::ErrorCode::InternalError,
          "publisher payload CRC corruption was not detected");
  ::munmap(mapping, static_cast<std::size_t>(status.st_size));
  ::close(fd);

  const auto quota_name =
      "/mds.publisher-quota." + std::to_string(::getpid());
  auto quota_options = ring_options(quota_name, 4096);
  quota_options.mode = api::RingMode::Lossless;
  auto quota_opened = transport::SharedRing::open(quota_options);
  require(bool(quota_opened), quota_opened.message.c_str());
  auto quota_ring = std::move(quota_opened.value);
  auto quota_registered = quota_ring.register_reader(
      transport::process_start_marker(::getpid()), now_ns());
  require(bool(quota_registered), quota_registered.message.c_str());
  publish::WirePublisher quota_publisher(quota_ring);
  api::Result<std::uint64_t> result;
  do {
    result = quota_publisher.publish_bbo(bbo);
  } while (result);
  require(result.error == api::ErrorCode::QuotaExceeded,
          "publisher did not explicitly propagate ring backpressure");
}

void test_dual_segment_factory() {
  using namespace mds;
  require(publish::make_publisher_segment_name("SPOT", "BTCUSDT", "TICKER") ==
              "/selfquant.mds.spot.btcusdt.ticker.2",
          "default publisher segment name was not canonical");
  require(publish::make_publisher_segment_name(
              "/Custom.Namespace...", "Binance Spot", "BTC/USDT", "ORDERBOOK") ==
              "/Custom.Namespace.binance_spot.btc_usdt.orderbook.2",
          "custom prefix or component sanitization changed unexpectedly");
  require(publish::make_publisher_segment_name("", "spot", "BTCUSDT",
                                               "ticker")
                  .empty() &&
              publish::make_publisher_segment_name(
                  "missing-slash", "spot", "BTCUSDT", "ticker")
                  .empty() &&
              publish::make_publisher_segment_name(
                  "/", "spot", "BTCUSDT", "ticker")
                  .empty() &&
              publish::make_publisher_segment_name(
                  "/selfquant.mds", "", "BTCUSDT", "ticker")
                  .empty() &&
              publish::make_publisher_segment_name(
                  "/selfquant.mds", "spot", "", "ticker")
                  .empty() &&
              publish::make_publisher_segment_name(
                  "/selfquant.mds", "spot", "BTCUSDT", "depth")
                  .empty() &&
              publish::make_publisher_segment_name(
                  "/" + std::string(230, 'x'), "spot", "BTCUSDT", "ticker")
                  .empty(),
          "invalid publisher segment name was accepted");
  auto options =
      ring_options("/unused." + std::to_string(::getpid()), 4096);
  const std::string profile = "binance-spot-" + std::to_string(::getpid());
  auto opened =
      publish::TickerOrderBookPublishers::open(profile, "BTCUSDT", options);
  require(bool(opened), opened.message.c_str());
  require(opened.value.ticker().segment_name() ==
              publish::make_publisher_segment_name(profile, "BTCUSDT",
                                                   "ticker") &&
              opened.value.order_book().segment_name() ==
                  publish::make_publisher_segment_name(profile, "BTCUSDT",
                                                       "orderbook") &&
              opened.value.ticker().producer_epoch() !=
                  opened.value.order_book().producer_epoch(),
          "dual publisher factory created incorrect segments");
}

void test_aggregate_orderbook_preallocated_publish() {
  using namespace mds;
  auto options = ring_options(
      "/mds.publisher-aggregate." + std::to_string(::getpid()),
      1U << 20U);
  options.max_record_bytes = 32U << 10U;
  auto opened = transport::SharedRing::open(options);
  require(bool(opened), opened.message.c_str());
  auto ring = std::move(opened.value);
  auto registered = ring.register_reader(
      transport::process_start_marker(::getpid()), now_ns());
  require(bool(registered), registered.message.c_str());
  auto reader = registered.value;
  publish::WirePublisher publisher(ring);

  utils::md::wire::AggOrderBookRecord record{};
  record.member_count = 1;
  record.member_mask = 1;
  record.active_mask = 1;
  record.venue_slot_ids[0] =
      static_cast<std::uint8_t>(utils::md::Venue::Binance);
  record.bid_count = 1;
  record.ask_count = 1;
  record.bids[0].price = 100;
  record.bids[0].quantity = 2;
  record.bids[0].venue_quantity[0] = 2;
  record.bids[0].venue_mask = 1;
  record.bids[0].contributor_count = 1;
  record.asks[0].price = 101;
  record.asks[0].quantity = 3;
  record.asks[0].venue_quantity[0] = 3;
  record.asks[0].venue_mask = 1;
  record.asks[0].contributor_count = 1;

  const auto unavailable =
      publisher.publish_agg_orderbook(event_header(), record);
  require(!unavailable &&
              unavailable.error == api::ErrorCode::InternalError,
          "aggregate orderbook published without a prepared buffer");
  require(bool(publisher.prepare_aggregate_orderbook()),
          "aggregate orderbook buffer preparation failed");
  tracked_allocations.store(0, std::memory_order_relaxed);
  track_allocations.store(true, std::memory_order_release);
  const auto published =
      publisher.publish_agg_orderbook(event_header(), record);
  track_allocations.store(false, std::memory_order_release);
  require(bool(published),
          "aggregate orderbook publish failed");
  require(tracked_allocations.load(std::memory_order_relaxed) == 0,
          "aggregate orderbook publish allocated on the hot path");

  auto read = ring.read(reader);
  require(bool(read), read.message.c_str());
  utils::md::wire::AggOrderBookRecord decoded{};
  require(utils::md::wire::DecodeAggOrderBook(
              read.value->payload, decoded) ==
              utils::md::wire::CodecError::Ok &&
              decoded.bids[0].venue_quantity[0] == 2 &&
              decoded.asks[0].venue_quantity[0] == 3,
          "aggregate orderbook did not round-trip through the ring");
}

} // namespace

int main() {
  try {
    test_publish_decode_order_and_fragmentation();
    test_late_reader_detection();
    test_crc_and_backpressure();
    test_dual_segment_factory();
    test_aggregate_orderbook_preallocated_publish();
    std::cout << "all wire publisher tests passed\n";
    return 0;
  } catch (const std::exception &exception) {
    std::cerr << "test failure: " << exception.what() << '\n';
    return 1;
  }
}
