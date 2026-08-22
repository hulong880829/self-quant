#include <cassert>
#include <chrono>
#include <cstring>
#include <stdexcept>
#include <thread>
#include <type_traits>
#include <utility>
#include <vector>

#include "utils/md/decimal.h"
#include "utils/md/order_book.h"
#include "utils/md/symbol.h"
#include "utils/md/wire.h"
#include "utils/md/wire_codec.h"
#include "utils/queue/spsc_ring.h"
#include "utils/runtime/hardware.h"
#include "utils/runtime/timestamp.h"

using namespace utils;

#undef assert
#define assert(condition)                                                      \
  do {                                                                         \
    if (!(condition)) {                                                        \
      throw std::runtime_error("requirement failed: " #condition);             \
    }                                                                          \
  } while (false)

struct LeaseValue {
  static inline int live = 0;
  int value;

  explicit LeaseValue(int input) noexcept : value(input) { ++live; }
  ~LeaseValue() noexcept { --live; }
};

struct QueueValue {
  static inline int live = 0;
  int value;

  QueueValue() = delete;
  explicit QueueValue(int input) noexcept : value(input) { ++live; }
  QueueValue(QueueValue&& other) noexcept : value(other.value) { ++live; }
  QueueValue& operator=(QueueValue&& other) noexcept {
    value = other.value;
    return *this;
  }
  ~QueueValue() noexcept { --live; }
};

struct ThrowingQueueValue {
  explicit ThrowingQueueValue(int) noexcept(false) {}
  ThrowingQueueValue& operator=(ThrowingQueueValue&&) noexcept = default;
  ~ThrowingQueueValue() noexcept = default;
};

template <typename Queue>
concept CanEmplaceInt = requires(Queue& queue) { queue.try_emplace(1); };

static_assert(!CanEmplaceInt<queue::BoundedMpscQueue<ThrowingQueueValue, 4>>);
static_assert(CanEmplaceInt<queue::BoundedMpscQueue<QueueValue, 4>>);

void TestLayout() {
  static_assert(sizeof(md::wire::RecordHeader) == 72);
  static_assert(sizeof(md::wire::TickerRecord) == 176);
  static_assert(offsetof(md::wire::TickerRecord, open_price) == 144);
  static_assert(offsetof(md::wire::TickerRecord, close_price) == 168);
  const auto header = md::wire::MakeHeader(md::MessageType::Bbo, sizeof(md::wire::BboRecord));
  assert(header.magic == md::wire::kMagic);
  assert(header.schema_major == 2);
  assert(header.schema_minor == 0);
  assert(header.record_length == sizeof(md::wire::BboRecord));
  const auto instrument_header = md::wire::MakeHeader(
      md::MessageType::InstrumentUpdate, sizeof(md::wire::InstrumentUpdateRecord));
  assert(instrument_header.message_type ==
         static_cast<std::uint16_t>(md::MessageType::InstrumentUpdate));
  assert(instrument_header.record_length == sizeof(md::wire::InstrumentUpdateRecord));
}

void TestSymbol() {
  md::SymbolNormalizer normalizer;
  const auto a = normalizer.Normalize("xbt-usdt");
  const auto b = normalizer.Normalize("BTC/USDT");
  const auto c = normalizer.Normalize("BTCUSDT");
  assert(a && b && c);
  assert(a->canonical == "BTCUSDT" && a->canonical == b->canonical);
  assert(normalizer.InstrumentKey(md::Venue::Binance, md::ProductType::Spot, *a) !=
         normalizer.InstrumentKey(md::Venue::Binance, md::ProductType::Perpetual, *a, "USDT"));

  md::Instrument instrument{};
  instrument.instrument_id = 7;
  instrument.tick_size = 1;
  instrument.lot_size = 1;
  const char key[] = "1:1:BTCUSDT";
  std::memcpy(instrument.instrument_key.data(), key, sizeof(key));
  md::InstrumentRegistry registry;
  assert(registry.Register(instrument) == md::RegistryResult::Ok);
  const md::Instrument* first = registry.Find(7);
  assert(first != nullptr);
  assert(registry.Find("1:1:BTCUSDT") != nullptr);
  assert(registry.Register(instrument) == md::RegistryResult::DuplicateId);
  for (std::uint32_t id = 8; id < 4096; ++id) {
    instrument.instrument_id = id;
    const std::string next_key = "1:1:TEST" + std::to_string(id);
    instrument.instrument_key.fill('\0');
    std::memcpy(instrument.instrument_key.data(), next_key.data(), next_key.size());
    assert(registry.Register(instrument) == md::RegistryResult::Ok);
  }
  assert(first == registry.Find(7));
  assert(first->instrument_id == 7);
}

void TestLadder() {
  md::OrderBook book(130);
  book.Reset(100, 100, 5, 9);
  assert(book.Apply(md::Side::Bid, 500, 10) == md::LadderResult::Ok);
  assert(book.Apply(md::Side::Bid, 820, 20) == md::LadderResult::Ok);  // bitmap word boundary
  assert(book.Apply(md::Side::Ask, 510, 30) == md::LadderResult::Ok);
  book.SetLive();
  auto bbo = book.Bbo(7);
  assert(bbo && bbo->bid.price == 820 && bbo->ask.price == 510);
  assert(book.Apply(md::Side::Bid, 820, 0) == md::LadderResult::Ok);
  assert(book.bids().Best()->price == 500);
  assert(book.Apply(md::Side::Ask, 2000, 1) == md::LadderResult::OutOfWindow);
  assert(book.state() == md::BookState::Invalid);

  book.Reset(100, 100, 5, 10);
  assert(book.ChangeTickSize(10) == md::LadderResult::NeedsRestart);
  assert(book.state() == md::BookState::NeedsRestart);

  md::OrderBook medium(8192);
  medium.Reset(0, 0, 1, 11);
  assert(medium.Apply(md::Side::Bid, 8191, 1) ==
         md::LadderResult::Ok);
  assert(medium.Apply(md::Side::Bid, 8192, 1) ==
         md::LadderResult::OutOfWindow);
  assert(md::OrderBook{}.bids().capacity() == md::kMaxLadderLevels);
  md::OrderBook large(16384);
  large.Reset(0, 0, 1, 12);
  assert(large.Apply(md::Side::Bid, 16383, 1) ==
         md::LadderResult::Ok);
  assert(large.Apply(md::Side::Ask, 16383, 1) ==
         md::LadderResult::Ok);
  assert(large.Apply(md::Side::Ask, 16384, 1) ==
         md::LadderResult::OutOfWindow);
}

struct WireVisitor final : md::wire::RecordVisitor {
  int instruments{};
  int catalogs{};
  int bbos{};
  int tickers{};
  int deltas{};
  int begins{};
  int chunks{};
  int ends{};
  std::uint32_t levels{};

  bool OnInstrument(const md::wire::InstrumentUpdateRecord &) noexcept override {
    ++instruments;
    return true;
  }
  bool OnInstrumentCatalog(
      const md::wire::InstrumentCatalogRecord &) noexcept override {
    ++catalogs;
    return true;
  }
  bool OnBbo(const md::wire::BboRecord &record) noexcept override {
    ++bbos;
    return record.bid_price == 100 && record.ask_price == 101;
  }
  bool OnTicker(const md::wire::TickerRecord &record) noexcept override {
    ++tickers;
    return record.bid_price == 100 && record.ask_price == 101 &&
           record.last_price == 100 && record.last_quantity == 5 &&
           record.mark_price == 102 && record.index_price == 103 &&
           record.funding_rate == 4 && record.open_price == 95 &&
           record.high_price == 110 && record.low_price == 90 &&
           record.close_price == 105;
  }
  bool OnDelta(const md::wire::DeltaRecord &) noexcept override {
    ++deltas;
    return true;
  }
  bool OnSnapshotBegin(
      const md::wire::SnapshotBeginRecord &) noexcept override {
    ++begins;
    return true;
  }
  bool OnSnapshotChunk(
      const md::wire::SnapshotChunkRecord &record) noexcept override {
    ++chunks;
    levels += record.level_count;
    return true;
  }
  bool OnSnapshotEnd(
      const md::wire::SnapshotEndRecord &) noexcept override {
    ++ends;
    return true;
  }
};

void TestWireCodec() {
  md::wire::HeaderFields header{
      .instrument_id = 7,
      .bus_seq = 8,
      .source_seq = 9,
      .exchange_ts_ns = 10,
      .receive_tsc = 11,
      .publish_tsc = 12,
      .book_generation = 13,
      .state = md::BookState::Live,
      .source_id = 1,
  };
  WireVisitor visitor;
  std::array<std::byte, sizeof(md::wire::InstrumentCatalogRecord)> buffer{};

  md::Instrument instrument{};
  instrument.instrument_id = 7;
  auto encoded =
      md::wire::EncodeInstrument(buffer, header, instrument);
  assert(encoded && encoded.size == sizeof(md::wire::InstrumentUpdateRecord));
  md::wire::InstrumentUpdateRecord instrument_record{};
  assert(md::wire::DecodeInstrument(
             std::span<const std::byte>(buffer.data(), encoded.size),
             instrument_record) == md::wire::CodecError::Ok);
  assert(instrument_record.instrument.instrument_id == 7);
  assert(md::wire::Decode(
             std::span<const std::byte>(buffer.data(), encoded.size), visitor) ==
         md::wire::CodecError::Ok);

  md::InstrumentCatalog catalog{};
  catalog.instrument_id = header.instrument_id;
  encoded = md::wire::EncodeInstrumentCatalog(buffer, header, catalog);
  assert(encoded &&
         encoded.size == sizeof(md::wire::InstrumentCatalogRecord));
  md::wire::InstrumentCatalogRecord catalog_record{};
  assert(md::wire::DecodeInstrumentCatalog(
             std::span<const std::byte>(buffer.data(), encoded.size),
             catalog_record) == md::wire::CodecError::Ok);
  assert(md::wire::Decode(
             std::span<const std::byte>(buffer.data(), encoded.size), visitor) ==
         md::wire::CodecError::Ok);

  encoded = md::wire::EncodeBbo(buffer, header, {100, 2}, {101, 3});
  assert(encoded);
  md::wire::BboRecord bbo_record{};
  assert(md::wire::DecodeBbo(
             std::span<const std::byte>(buffer.data(), encoded.size),
             bbo_record) == md::wire::CodecError::Ok);
  assert(bbo_record.bid_price == 100 && bbo_record.ask_price == 101);
  assert(md::wire::Decode(
             std::span<const std::byte>(buffer.data(), encoded.size), visitor) ==
         md::wire::CodecError::Ok);
  auto future_minor = bbo_record.header;
  ++future_minor.schema_minor;
  std::memcpy(buffer.data(), &future_minor, sizeof(future_minor));
  assert(md::wire::DecodeBbo(
             std::span<const std::byte>(buffer.data(), encoded.size),
             bbo_record) == md::wire::CodecError::Ok);
  md::TickerEvent ticker{};
  ticker.bid = {100, 2};
  ticker.ask = {101, 3};
  ticker.last_price = 100;
  ticker.last_quantity = 5;
  ticker.mark_price = 102;
  ticker.index_price = 103;
  ticker.funding_rate = 4;
  ticker.open_price = 95;
  ticker.high_price = 110;
  ticker.low_price = 90;
  ticker.close_price = 105;
  encoded = md::wire::EncodeTicker(buffer, header, ticker);
  assert(encoded && encoded.size == sizeof(md::wire::TickerRecord));
  md::wire::TickerRecord ticker_record{};
  assert(md::wire::DecodeTicker(
             std::span<const std::byte>(buffer.data(), encoded.size),
             ticker_record) == md::wire::CodecError::Ok);
  assert(ticker_record.close_price == 105);
  assert(md::wire::Decode(
             std::span<const std::byte>(buffer.data(), encoded.size), visitor) ==
         md::wire::CodecError::Ok);

  md::wire::RecordHeader ticker_header{};
  std::memcpy(&ticker_header, buffer.data(), sizeof(ticker_header));
  --ticker_header.record_length;
  std::memcpy(buffer.data(), &ticker_header, sizeof(ticker_header));
  assert(md::wire::Decode(
             std::span<const std::byte>(buffer.data(), encoded.size), visitor) ==
         md::wire::CodecError::LengthMismatch);

  encoded = md::wire::EncodeDelta(buffer, header, md::Side::Bid, {100, 4});
  assert(encoded);
  md::wire::DeltaRecord delta_record{};
  assert(md::wire::DecodeDelta(
             std::span<const std::byte>(buffer.data(), encoded.size),
             delta_record) == md::wire::CodecError::Ok);
  assert(delta_record.price == 100 && delta_record.quantity == 4);
  assert(md::wire::Decode(
             std::span<const std::byte>(buffer.data(), encoded.size), visitor) ==
         md::wire::CodecError::Ok);

  std::array<md::Level, 25> levels{};
  for (std::size_t index = 0; index < levels.size(); ++index) {
    levels[index] = {static_cast<std::int64_t>(100 + index), 1};
  }
  encoded = md::wire::EncodeSnapshotBegin(buffer, header, 25, 2);
  assert(encoded);
  md::wire::SnapshotBeginRecord begin_record{};
  assert(md::wire::DecodeSnapshotBegin(
             std::span<const std::byte>(buffer.data(), encoded.size),
             begin_record) == md::wire::CodecError::Ok);
  assert(begin_record.item_count == 25);
  assert(md::wire::Decode(
             std::span<const std::byte>(buffer.data(), encoded.size), visitor) ==
         md::wire::CodecError::Ok);
  encoded = md::wire::EncodeSnapshotChunk(
      buffer, header, 0, md::Side::Bid,
      std::span<const md::Level>(levels.data(), 24));
  assert(encoded);
  md::wire::SnapshotChunkRecord chunk_record{};
  assert(md::wire::DecodeSnapshotChunk(
             std::span<const std::byte>(buffer.data(), encoded.size),
             chunk_record) == md::wire::CodecError::Ok);
  assert(chunk_record.level_count == 24);
  assert(md::wire::Decode(
             std::span<const std::byte>(buffer.data(), encoded.size), visitor) ==
         md::wire::CodecError::Ok);
  encoded = md::wire::EncodeSnapshotChunk(
      buffer, header, 1, md::Side::Bid,
      std::span<const md::Level>(levels.data() + 24, 1));
  assert(encoded);
  assert(md::wire::Decode(
             std::span<const std::byte>(buffer.data(), encoded.size), visitor) ==
         md::wire::CodecError::Ok);
  encoded = md::wire::EncodeSnapshotEnd(buffer, header, 25, 0x1234);
  assert(encoded);
  md::wire::SnapshotEndRecord end_record{};
  assert(md::wire::DecodeSnapshotEnd(
             std::span<const std::byte>(buffer.data(), encoded.size),
             end_record) == md::wire::CodecError::Ok);
  assert(end_record.chunk_count_or_checksum == 0x1234);
  assert(md::wire::Decode(
             std::span<const std::byte>(buffer.data(), encoded.size), visitor) ==
         md::wire::CodecError::Ok);
  assert(visitor.instruments == 1 && visitor.catalogs == 1 &&
         visitor.bbos == 1 &&
         visitor.tickers == 1 &&
         visitor.deltas == 1 && visitor.begins == 1 &&
         visitor.chunks == 2 && visitor.levels == 25 && visitor.ends == 1);

  encoded = md::wire::EncodeSnapshotBegin(buffer, header, 24, 1);
  assert(encoded);
  auto malformed =
      std::span<std::byte>(buffer.data(), encoded.size);
  malformed[0] = std::byte{0};
  assert(md::wire::ValidateHeader(malformed) ==
         md::wire::CodecError::BadMagic);
  encoded = md::wire::EncodeSnapshotBegin(buffer, header, 24, 1);
  assert(encoded);
  md::wire::RecordHeader wire_header{};
  std::memcpy(&wire_header, buffer.data(), sizeof(wire_header));
  wire_header.schema_major =
      static_cast<std::uint16_t>(md::wire::kSchemaMajor + 1);
  std::memcpy(buffer.data(), &wire_header, sizeof(wire_header));
  assert(md::wire::ValidateHeader(
             std::span<const std::byte>(buffer.data(), encoded.size)) ==
         md::wire::CodecError::UnsupportedSchema);
  wire_header.schema_major = md::wire::kSchemaMajor;
  --wire_header.record_length;
  std::memcpy(buffer.data(), &wire_header, sizeof(wire_header));
  assert(md::wire::ValidateHeader(
             std::span<const std::byte>(buffer.data(), encoded.size)) ==
         md::wire::CodecError::LengthMismatch);

  constexpr std::size_t kUnknownLength =
      sizeof(md::wire::RecordHeader) + alignof(md::wire::RecordHeader);
  std::array<std::byte, kUnknownLength> unknown{};
  auto unknown_header = md::wire::MakeHeader(
      static_cast<md::MessageType>(9999), kUnknownLength);
  unknown_header.schema_minor =
      static_cast<std::uint16_t>(md::wire::kSchemaMinor + 1);
  std::memcpy(unknown.data(), &unknown_header, sizeof(unknown_header));
  assert(md::wire::ValidateHeader(unknown) ==
         md::wire::CodecError::UnknownMessageType);
}

void TestSpsc() {
  queue::SpscRing<LeaseValue, 4> ring;
  {
    auto producer = ring.try_reserve();
    assert(producer);
    assert(!ring.try_reserve());
    producer->emplace(41);
    producer->emplace(42);
    assert(LeaseValue::live == 1);
  }
  assert(LeaseValue::live == 0);

  auto producer = ring.try_reserve();
  assert(producer);
  producer->emplace(42);
  assert(producer->commit());
  assert(ring.try_reserve());
  auto consumer = ring.try_peek();
  assert(consumer && (*consumer)->value == 42);
  assert(!ring.try_peek());
  {
    auto moved = std::move(*consumer);
    assert(!ring.try_peek());
  }
  assert(LeaseValue::live == 0);
  assert(!ring.try_peek());
}

void TestMpsc() {
  queue::BoundedMpscQueue<int, 1024> queue;
  constexpr int kCount = 200;
  std::thread first([&] { for (int i = 0; i < kCount; ++i) while (!queue.try_emplace(i)) {} });
  std::thread second([&] { for (int i = 0; i < kCount; ++i) while (!queue.try_emplace(i)) {} });
  int received = 0;
  int value = 0;
  while (received < kCount * 2) if (queue.try_dequeue(value)) ++received;
  first.join();
  second.join();
  assert(received == kCount * 2);

  {
    queue::BoundedMpscQueue<QueueValue, 4> non_default_queue;
    assert(non_default_queue.try_emplace(7));
    assert(QueueValue::live == 1);
  }
  assert(QueueValue::live == 0);
}

void TestDecimal() {
  std::uint8_t scale = 99;
  std::int64_t value = 0;

  assert(md::decimal_scale("123", scale) && scale == 0);
  assert(md::decimal_scale("001.2300", scale) && scale == 4);
  assert(md::decimal_scale("1.25e-2", scale) && scale == 4);
  assert(!md::decimal_scale("125e2", scale));

  assert(md::decimal_to_fixed("123.45", 2, value) && value == 12'345);
  assert(md::decimal_to_fixed("001.2300", 4, value) && value == 12'300);
  assert(md::decimal_to_fixed("-0.125", 3, value) && value == -125);
  assert(md::decimal_to_fixed("+42", 0, value) && value == 42);
  assert(md::decimal_to_fixed("1.230000000000000000", 2, value) &&
         value == 123);
  assert(md::decimal_to_fixed("125e2", 0, value) && value == 12'500);
  assert(md::decimal_to_fixed("1e3", 2, value) && value == 100'000);
  assert(md::decimal_to_fixed("9223372036854775807", 0, value) &&
         value == 9'223'372'036'854'775'807LL);
  assert(md::decimal_to_fixed("-9223372036854775808", 0, value) &&
         value == (-9'223'372'036'854'775'807LL - 1));

  assert(!md::decimal_to_fixed("", 0, value));
  assert(!md::decimal_to_fixed(".", 0, value));
  assert(!md::decimal_to_fixed("12x", 0, value));
  assert(!md::decimal_to_fixed("1.234", 2, value));
  assert(!md::decimal_to_fixed("9223372036854775808", 0, value));
  assert(!md::decimal_to_fixed("-9223372036854775809", 0, value));
  assert(!md::decimal_scale("0.0000000000000000001", scale));
}

void TestTimestampAndHardware() {
  const auto before = runtime::Timestamp::NowMono();
  const auto sample = runtime::Timestamp::NowTSC();
  const auto after = runtime::Timestamp::NowMono();
  assert(before > 0 && after >= before);
  const auto detected = runtime::Timestamp::Detect();
  if (detected.rdtscp && detected.invariant_tsc)
    assert(sample.cycles > 0);
  else
    assert(sample.cycles == 0);
  const auto capability = runtime::Timestamp::Calibrate(std::chrono::milliseconds(2));
  if (capability.calibrated) {
    assert(capability.state == runtime::TscCalibrationState::Calibrated);
    assert(capability.calibration_tsc > 0);
    assert(capability.calibration_mono_ns > 0);
    assert(runtime::Timestamp::TscToMonoNs(capability.calibration_tsc, capability) ==
           capability.calibration_mono_ns);
  }
  const auto invalid = runtime::Timestamp::Calibrate(std::chrono::milliseconds(0));
  if (invalid.rdtscp && invalid.invariant_tsc)
    assert(invalid.state == runtime::TscCalibrationState::InvalidInterval);
  runtime::TscCapability synthetic{};
  synthetic.calibrated = true;
  synthetic.cycles_per_ns = 2.0;
  synthetic.calibration_tsc = 100;
  synthetic.calibration_mono_ns = 1'000;
  assert(runtime::Timestamp::CyclesToNs(20, synthetic) == 10);
  assert(runtime::Timestamp::TscToMonoNs(120, synthetic) == 1'010);
  assert(runtime::Timestamp::TscToMonoNs(80, synthetic) == 990);
  const auto topology = runtime::DetectHardware();
  assert(!topology.cpus.empty());
  const auto plan = runtime::MakeAutomaticPlan(topology, 0);
  if (topology.cpus.size() >= 4) assert(plan.valid || !plan.warnings.empty());
}

int main() {
  TestLayout();
  TestSymbol();
  TestLadder();
  TestWireCodec();
  TestSpsc();
  TestMpsc();
  TestDecimal();
  TestTimestampAndHardware();
}
