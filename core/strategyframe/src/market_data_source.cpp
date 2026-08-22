#include "strategyframe/market_data_source.h"

#include <algorithm>
#include <chrono>
#include <cstring>
#include <mutex>
#include <string>
#include <thread>
#include <utility>

#include <unistd.h>

#include "mds/api/mds_api.h"
#include "mds/producer/producer_runtime.h"
#include "mds/transport/shared_ring.h"
#include "utils/md/wire.h"
#include "utils/md/wire_codec.h"

namespace strategyframe {
namespace {

constexpr std::size_t kBookCapacity = 4096;
constexpr std::size_t kInstrumentCapacity = 4096;

std::uint64_t NowNs() noexcept {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::steady_clock::now().time_since_epoch())
          .count());
}

FixedPoint Fixed(std::int64_t value, std::uint8_t scale) noexcept {
  return {value, scale, {}};
}

MarketUpdateHeader Header(const utils::md::wire::RecordHeader& value,
                          Venue venue, ProductType product) noexcept {
  return {value.instrument_id,
          venue,
          product,
          value.source_id,
          value.state,
          value.flags,
          value.bus_seq,
          value.source_seq,
          value.exchange_ts_ns,
          value.receive_tsc,
          value.publish_tsc,
          value.book_generation};
}

Venue ToVenue(utils::md::Venue value) noexcept {
  return static_cast<Venue>(value);
}

ProductType ToProduct(utils::md::ProductType value) noexcept {
  return static_cast<ProductType>(value);
}

std::mutex& SelfHostedMutex() {
  static std::mutex value;
  return value;
}

bool& SelfHostedClaimed() {
  static bool value{};
  return value;
}

struct FixedBook {
  std::array<MarketLevel, kBookCapacity> bids{};
  std::array<MarketLevel, kBookCapacity> asks{};
  std::size_t bid_count{};
  std::size_t ask_count{};
  InstrumentId instrument_id{};
  std::uint32_t generation{};
  bool snapshot{};

  void reset(std::uint32_t next_generation = 0) noexcept {
    bid_count = ask_count = 0;
    generation = next_generation;
    snapshot = false;
  }

  bool begin(const utils::md::wire::SnapshotBeginRecord& record) noexcept {
    if (record.item_count > kBookCapacity * 2U) {
      reset(record.header.book_generation);
      return false;
    }
    reset(record.header.book_generation);
    instrument_id = record.header.instrument_id;
    snapshot = true;
    return true;
  }

  bool chunk(const utils::md::wire::SnapshotChunkRecord& record,
             std::uint8_t price_scale, std::uint8_t quantity_scale) noexcept {
    if (!snapshot || record.header.instrument_id != instrument_id ||
        record.header.book_generation != generation ||
        record.level_count > record.levels.size()) {
      reset(record.header.book_generation);
      return false;
    }
    auto* output = record.side == static_cast<std::uint8_t>(utils::md::Side::Bid)
                       ? bids.data()
                       : asks.data();
    std::size_t& count =
        record.side == static_cast<std::uint8_t>(utils::md::Side::Bid)
            ? bid_count
            : ask_count;
    if (count + record.level_count > kBookCapacity) {
      reset(record.header.book_generation);
      return false;
    }
    for (std::size_t i = 0; i < record.level_count; ++i) {
      output[count++] = {Fixed(record.levels[i].price, price_scale),
                         Fixed(record.levels[i].quantity, quantity_scale)};
    }
    return true;
  }

  bool finish(const utils::md::wire::SnapshotEndRecord& record) noexcept {
    if (!snapshot || record.header.instrument_id != instrument_id ||
        record.header.book_generation != generation ||
        record.item_count != bid_count + ask_count) {
      reset(record.header.book_generation);
      return false;
    }
    snapshot = false;
    return true;
  }

  bool delta(const utils::md::wire::DeltaRecord& record,
             std::uint8_t price_scale, std::uint8_t quantity_scale) noexcept {
    if (record.header.instrument_id != instrument_id ||
        record.header.book_generation != generation) {
      reset(record.header.book_generation);
      instrument_id = record.header.instrument_id;
      return false;
    }
    const bool bid =
        record.side == static_cast<std::uint8_t>(utils::md::Side::Bid);
    auto* values = bid ? bids.data() : asks.data();
    std::size_t& count = bid ? bid_count : ask_count;
    std::size_t at = 0;
    while (at < count &&
           (bid ? values[at].price.value > record.price
                : values[at].price.value < record.price))
      ++at;
    if (at < count && values[at].price.value == record.price) {
      if (record.quantity == 0) {
        std::move(values + at + 1, values + count, values + at);
        --count;
      } else {
        values[at].quantity = Fixed(record.quantity, quantity_scale);
      }
      return true;
    }
    if (record.quantity == 0) return true;
    if (count == kBookCapacity) return false;
    std::move_backward(values + at, values + count, values + count + 1);
    values[at] = {Fixed(record.price, price_scale),
                  Fixed(record.quantity, quantity_scale)};
    ++count;
    return true;
  }
};

}  // namespace

struct MarketDataSource::Impl {
  struct InstrumentMeta {
    utils::md::Instrument value{};
    utils::md::InstrumentCatalog catalog{};
    std::uint32_t catalog_generation{};
  };
  enum class CatalogUpsert : std::uint8_t {
    Failed,
    Unchanged,
    Changed,
  };
  struct InstrumentSlot {
    InstrumentId id{};
    InstrumentMeta meta{};
    bool tombstone{};
  };
  struct BookSlot {
    InstrumentId id{};
    std::unique_ptr<FixedBook> book;
    bool tombstone{};
  };

  struct Stream {
    SegmentConfig config{};
    mds::transport::SharedRing ring{};
    mds::transport::ReaderHandle reader{};
    std::uint64_t last_heartbeat{};
    std::array<BookSlot, kInstrumentCapacity> books{};
    utils::md::wire::AggBboRecord agg_bbo{};
    utils::md::wire::AggOrderBookRecord agg_book{};
    std::array<AggregateLevel, utils::md::wire::kAggMaxLevelsPerSide>
        aggregate_bids{};
    std::array<AggregateLevel, utils::md::wire::kAggMaxLevelsPerSide>
        aggregate_asks{};
  };

  MdsConfig config;
  Sink sink;
  std::vector<Stream> streams;
  Stream replay_stream;
  std::array<InstrumentSlot, kInstrumentCapacity> instruments{};
  std::deque<std::vector<std::byte>> replay;
  std::unique_ptr<mds::producer::ProducerRuntime> producer_runtime;
  bool started{};
  bool self_hosted{};

  Impl(MdsConfig value, Sink target)
      : config(std::move(value)), sink(target) {}

  ~Impl() { stop(); }

  Error open_segment(const SegmentConfig& config_value) noexcept {
    try {
      mds::transport::RingOptions options;
      options.name = config_value.name;
      options.create = false;
      auto opened = mds::transport::SharedRing::open(options);
      if (!opened) return Error::MdsFailure;
      if (opened.value.ring_bytes() != config_value.ring_bytes ||
          opened.value.max_record_bytes() !=
              config_value.max_record_bytes)
        return Error::InvalidConfig;
      if (opened.value.active_reader_count() >=
          config_value.expected_reader_budget)
        return Error::CapacityExceeded;
      const std::uint64_t now = NowNs();
      auto reader = opened.value.register_reader(
          mds::transport::process_start_marker(
              static_cast<std::uint32_t>(::getpid())),
          now);
      if (!reader) return Error::MdsFailure;
      Stream stream;
      stream.config = config_value;
      stream.ring = std::move(opened.value);
      stream.reader = reader.value;
      stream.last_heartbeat = now;
      streams.push_back(std::move(stream));
      return Error::Ok;
    } catch (...) {
      return Error::MdsFailure;
    }
  }

  Error start_self_hosted() noexcept {
    {
      std::lock_guard lock(SelfHostedMutex());
      if (SelfHostedClaimed()) return Error::InvalidState;
      SelfHostedClaimed() = true;
      self_hosted = true;
    }
    try {
      producer_runtime =
          std::make_unique<mds::producer::ProducerRuntime>(
              mds::producer::ProducerRuntimeOptions{
                  config.producer_config_path});
    } catch (...) {
      return Error::MdsFailure;
    }
    const auto started_runtime = producer_runtime->start();
    if (!started_runtime) return Error::MdsFailure;
    for (const auto& resolved : producer_runtime->resolved_segments()) {
      SegmentConfig segment;
      segment.name = resolved.name;
      segment.ring_bytes = resolved.ring_bytes;
      segment.max_record_bytes = resolved.max_record_bytes;
      segment.expected_reader_budget = 64;
      if (open_segment(segment) != Error::Ok)
        return Error::MdsFailure;
    }
    return Error::Ok;
  }

  Error start() noexcept {
    if (started) return Error::InvalidState;
    started = true;
    if (config.source == MdsSourceMode::Replay) return Error::Ok;
    if (config.source == MdsSourceMode::SelfHosted) {
      const Error error = start_self_hosted();
      if (error != Error::Ok) stop();
      return error;
    }
    for (const auto& segment : config.segments) {
      const Error error = open_segment(segment);
      if (error != Error::Ok) {
        stop();
        return error;
      }
    }
    return Error::Ok;
  }

  void stop() noexcept {
    for (auto& stream : streams) {
      if (stream.reader) {
        (void)stream.ring.unregister_reader(stream.reader);
        stream.reader = {};
      }
    }
    streams.clear();
    if (producer_runtime) {
      producer_runtime->stop();
      producer_runtime.reset();
    }
    if (self_hosted) {
      std::lock_guard lock(SelfHostedMutex());
      SelfHostedClaimed() = false;
      self_hosted = false;
    }
    started = false;
  }

  const InstrumentMeta* meta(InstrumentId id) const noexcept {
    if (id == 0) return nullptr;
    std::size_t index = id % instruments.size();
    for (std::size_t probe = 0; probe < instruments.size(); ++probe) {
      const auto& slot = instruments[index];
      if (slot.id == id) return &slot.meta;
      if (slot.id == 0 && !slot.tombstone) return nullptr;
      index = (index + 1) % instruments.size();
    }
    return nullptr;
  }

  bool upsert(const utils::md::Instrument& value) noexcept {
    if (value.instrument_id == 0) return false;
    std::size_t index = value.instrument_id % instruments.size();
    InstrumentSlot* reusable = nullptr;
    for (std::size_t probe = 0; probe < instruments.size(); ++probe) {
      auto& slot = instruments[index];
      if (slot.id == value.instrument_id) {
        slot.meta.value = value;
        return true;
      }
      if (slot.id == 0 && slot.tombstone && reusable == nullptr)
        reusable = &slot;
      if (slot.id == 0 && !slot.tombstone) {
        auto& target = reusable == nullptr ? slot : *reusable;
        target.id = value.instrument_id;
        target.meta.value = value;
        target.tombstone = false;
        return true;
      }
      index = (index + 1) % instruments.size();
    }
    if (reusable != nullptr) {
      reusable->id = value.instrument_id;
      reusable->meta.value = value;
      reusable->tombstone = false;
      return true;
    }
    return false;
  }

  CatalogUpsert upsert_catalog(
      const utils::md::InstrumentCatalog& value,
      std::uint32_t generation) noexcept {
    if (value.instrument_id == 0 || generation == 0)
      return CatalogUpsert::Failed;
    std::size_t index = value.instrument_id % instruments.size();
    InstrumentSlot* reusable = nullptr;
    for (std::size_t probe = 0; probe < instruments.size(); ++probe) {
      auto& slot = instruments[index];
      if (slot.id == value.instrument_id) {
        if (generation < slot.meta.catalog_generation)
          return CatalogUpsert::Unchanged;
        if (generation == slot.meta.catalog_generation) {
          return std::memcmp(&slot.meta.catalog, &value, sizeof(value)) == 0
                     ? CatalogUpsert::Unchanged
                     : CatalogUpsert::Failed;
        }
        slot.meta.catalog = value;
        slot.meta.catalog_generation = generation;
        return CatalogUpsert::Changed;
      }
      if (slot.id == 0 && slot.tombstone && reusable == nullptr)
        reusable = &slot;
      if (slot.id == 0 && !slot.tombstone) {
        auto& target = reusable == nullptr ? slot : *reusable;
        target.id = value.instrument_id;
        target.meta.catalog = value;
        target.meta.catalog_generation = generation;
        target.tombstone = false;
        return CatalogUpsert::Changed;
      }
      index = (index + 1) % instruments.size();
    }
    if (reusable != nullptr) {
      reusable->id = value.instrument_id;
      reusable->meta.catalog = value;
      reusable->meta.catalog_generation = generation;
      reusable->tombstone = false;
      return CatalogUpsert::Changed;
    }
    return CatalogUpsert::Failed;
  }

  FixedBook* book_for(Stream& stream, InstrumentId id,
                      bool create) noexcept {
    if (id == 0) return nullptr;
    std::size_t index = id % stream.books.size();
    BookSlot* reusable = nullptr;
    for (std::size_t probe = 0; probe < stream.books.size(); ++probe) {
      auto& slot = stream.books[index];
      if (slot.id == id) return slot.book.get();
      if (slot.id == 0 && slot.tombstone && reusable == nullptr)
        reusable = &slot;
      if (slot.id == 0 && !slot.tombstone) {
        if (!create) return nullptr;
        try {
          auto& target = reusable == nullptr ? slot : *reusable;
          target.book = std::make_unique<FixedBook>();
          target.id = id;
          target.tombstone = false;
          return target.book.get();
        } catch (...) {
          return nullptr;
        }
      }
      index = (index + 1) % stream.books.size();
    }
    if (create && reusable != nullptr) {
      try {
        reusable->book = std::make_unique<FixedBook>();
        reusable->id = id;
        reusable->tombstone = false;
        return reusable->book.get();
      } catch (...) {
        return nullptr;
      }
    }
    return nullptr;
  }

  void retire_instrument(InstrumentId id) noexcept {
    if (id == 0) return;
    std::size_t index = id % instruments.size();
    for (std::size_t probe = 0; probe < instruments.size(); ++probe) {
      auto& slot = instruments[index];
      if (slot.id == id) {
        slot.id = 0;
        slot.meta = {};
        slot.tombstone = true;
        break;
      }
      if (slot.id == 0 && !slot.tombstone) break;
      index = (index + 1) % instruments.size();
    }
    const auto retire_book = [id](Stream& stream) {
      std::size_t book_index = id % stream.books.size();
      for (std::size_t probe = 0; probe < stream.books.size(); ++probe) {
        auto& slot = stream.books[book_index];
        if (slot.id == id) {
          slot.id = 0;
          slot.book.reset();
          slot.tombstone = true;
          return;
        }
        if (slot.id == 0 && !slot.tombstone) return;
        book_index = (book_index + 1) % stream.books.size();
      }
    };
    retire_book(replay_stream);
    for (auto& stream : streams) retire_book(stream);
  }

  void reset_books(Stream& stream) noexcept {
    for (auto& slot : stream.books) {
      if (slot.book) slot.book->reset();
    }
  }

  bool emit_book(FixedBook& book,
                 const utils::md::wire::RecordHeader& header) noexcept {
    const auto* value = meta(header.instrument_id);
    if (!value) return true;
    OrderBookUpdate update{
        Header(header, ToVenue(value->value.venue),
               ToProduct(value->value.product_type)),
        {book.bids.data(), book.bid_count},
        {book.asks.data(), book.ask_count}};
    return !sink.book || sink.book(sink.context, update);
  }

  bool dispatch(Stream& stream, std::span<const std::byte> bytes) noexcept {
    using namespace utils::md::wire;
    RecordHeader generic{};
    const auto validation = ValidateHeader(bytes, &generic);
    if (validation == CodecError::UnknownMessageType) return true;
    if (validation != CodecError::Ok) return false;
    const auto type = static_cast<utils::md::MessageType>(generic.message_type);
    if (type == utils::md::MessageType::InstrumentCatalog) {
      InstrumentCatalogRecord record{};
      if (DecodeInstrumentCatalog(bytes, record) != CodecError::Ok)
        return false;
      const auto upsert =
          upsert_catalog(record.catalog, record.header.book_generation);
      if (upsert == CatalogUpsert::Failed) return false;
      if (upsert == CatalogUpsert::Unchanged) return true;
      return !sink.catalog ||
             sink.catalog(sink.context, record.catalog,
                          record.header.book_generation);
    }
    if (type == utils::md::MessageType::InstrumentUpdate) {
      InstrumentUpdateRecord record{};
      if (DecodeInstrument(bytes, record) != CodecError::Ok) return false;
      if (!upsert(record.instrument)) return false;
      return !sink.instrument ||
             sink.instrument(sink.context, record.instrument);
    }
    const InstrumentMeta* instrument = meta(generic.instrument_id);
    if (type == utils::md::MessageType::Bbo ||
        type == utils::md::MessageType::Ticker) {
      if (!instrument) return true;
      std::int64_t bid_price{}, bid_quantity{}, ask_price{}, ask_quantity{};
      if (type == utils::md::MessageType::Bbo) {
        BboRecord record{};
        if (DecodeBbo(bytes, record) != CodecError::Ok) return false;
        bid_price = record.bid_price;
        bid_quantity = record.bid_quantity;
        ask_price = record.ask_price;
        ask_quantity = record.ask_quantity;
      } else {
        TickerRecord record{};
        if (DecodeTicker(bytes, record) != CodecError::Ok) return false;
        bid_price = record.bid_price;
        bid_quantity = record.bid_quantity;
        ask_price = record.ask_price;
        ask_quantity = record.ask_quantity;
      }
      BboUpdate update{
          Header(generic, ToVenue(instrument->value.venue),
                 ToProduct(instrument->value.product_type)),
          {Fixed(bid_price, instrument->value.price_scale),
           Fixed(bid_quantity, instrument->value.quantity_scale)},
          {Fixed(ask_price, instrument->value.price_scale),
           Fixed(ask_quantity, instrument->value.quantity_scale)}};
      return !sink.bbo || sink.bbo(sink.context, update);
    }
    if (type == utils::md::MessageType::SnapshotBegin) {
      SnapshotBeginRecord record{};
      if (DecodeSnapshotBegin(bytes, record) != CodecError::Ok) return false;
      auto* book = book_for(stream, record.header.instrument_id, true);
      return book != nullptr && book->begin(record);
    }
    if (type == utils::md::MessageType::SnapshotChunk) {
      SnapshotChunkRecord record{};
      if (!instrument ||
          DecodeSnapshotChunk(bytes, record) != CodecError::Ok)
        return false;
      auto* book = book_for(stream, record.header.instrument_id, false);
      return book != nullptr &&
             book->chunk(record, instrument->value.price_scale,
                         instrument->value.quantity_scale);
    }
    if (type == utils::md::MessageType::SnapshotEnd) {
      SnapshotEndRecord record{};
      if (DecodeSnapshotEnd(bytes, record) != CodecError::Ok)
        return false;
      auto* book = book_for(stream, record.header.instrument_id, false);
      if (book == nullptr || !book->finish(record)) return false;
      return emit_book(*book, record.header);
    }
    if (type == utils::md::MessageType::BookDelta) {
      DeltaRecord record{};
      if (!instrument || DecodeDelta(bytes, record) != CodecError::Ok)
        return false;
      auto* book = book_for(stream, record.header.instrument_id, false);
      if (book == nullptr ||
          !book->delta(record, instrument->value.price_scale,
                       instrument->value.quantity_scale))
        return false;
      return emit_book(*book, record.header);
    }
    if (type == utils::md::MessageType::AggBbo) {
      if (DecodeAggBbo(bytes, stream.agg_bbo) != CodecError::Ok) return false;
      const auto& value = stream.agg_bbo;
      const auto side = [&](const utils::md::wire::AggBboSide& input) {
        return AggBboSide{
            Fixed(input.price, value.price_scale),
            Fixed(input.quantity, value.quantity_scale),
            input.venue_quantity,
            input.exchange_ts_ns,
            input.venue_mask,
            input.worst_ingress_age_us,
            input.best_venue};
      };
      AggBboUpdate update{Header(value.header, Venue::Unknown,
                                 ProductType::Unknown),
                          side(value.gated_bid),
                          side(value.gated_ask),
                          value.member_mask,
                          value.live_mask,
                          value.gated_cross_bps,
                          value.skew_us};
      return !sink.agg_bbo || sink.agg_bbo(sink.context, update);
    }
    if (type == utils::md::MessageType::AggOrderBook) {
      if (DecodeAggOrderBook(bytes, stream.agg_book) != CodecError::Ok)
        return false;
      const auto& value = stream.agg_book;
      const auto map_levels =
          [&](const auto& input, auto& output, std::size_t count) {
            for (std::size_t i = 0; i < count; ++i) {
              output[i] = {Fixed(input[i].price, value.price_scale),
                           Fixed(input[i].quantity, value.quantity_scale),
                           input[i].venue_quantity,
                           input[i].venue_mask,
                           input[i].contributor_count};
            }
          };
      map_levels(value.bids, stream.aggregate_bids, value.bid_count);
      map_levels(value.asks, stream.aggregate_asks, value.ask_count);
      AggOrderBookUpdate update{
          Header(value.header, Venue::Unknown, ProductType::Unknown),
          {stream.aggregate_bids.data(), value.bid_count},
          {stream.aggregate_asks.data(), value.ask_count},
          value.member_mask,
          value.active_mask};
      return !sink.agg_book || sink.agg_book(sink.context, update);
    }
    return true;
  }

  Error poll(std::size_t budget, std::size_t& dispatched) noexcept {
    dispatched = 0;
    if (!started) return Error::InvalidState;
    if (budget == 0) return Error::Ok;
    if (producer_runtime && producer_runtime->run_once(0) < 0)
      return Error::MdsFailure;
    if (config.source == MdsSourceMode::Replay) {
      while (dispatched < budget && !replay.empty()) {
        auto record = std::move(replay.front());
        replay.pop_front();
        if (!dispatch(replay_stream, record)) return Error::MdsFailure;
        ++dispatched;
      }
      return Error::Ok;
    }
    const std::uint64_t now = NowNs();
    for (auto& stream : streams) {
      if (now - stream.last_heartbeat >= stream.config.heartbeat_interval_ns) {
        if (!stream.ring.heartbeat(stream.reader, now))
          return Error::MdsFailure;
        stream.last_heartbeat = now;
      }
    }
    bool progress = true;
    while (dispatched < budget && progress) {
      progress = false;
      for (std::size_t i = 0; i < streams.size() && dispatched < budget; ++i) {
        auto& stream = streams[i];
        mds::transport::ReadLease lease;
        const auto error = stream.ring.try_read(stream.reader, lease);
        if (error == mds::api::ErrorCode::QuotaExceeded) continue;
        if (error == mds::api::ErrorCode::RecordOverwritten ||
            error == mds::api::ErrorCode::SubscriptionRejected) {
          reset_books(stream);
          if (!stream.ring.resync_to_latest(stream.reader))
            return Error::MdsFailure;
          if (sink.gap) sink.gap(sink.context, i);
          continue;
        }
        if (error != mds::api::ErrorCode::Ok) return Error::MdsFailure;
        progress = true;
        if (!dispatch(stream, lease.view().payload)) {
          lease.release();
          return Error::MdsFailure;
        }
        if (!lease.commit()) return Error::MdsFailure;
        ++dispatched;
      }
    }
    return Error::Ok;
  }
};

MarketDataSource::MarketDataSource(MdsConfig config, Sink sink)
    : impl_(std::make_unique<Impl>(std::move(config), sink)) {}
MarketDataSource::~MarketDataSource() = default;

Error MarketDataSource::start() noexcept { return impl_->start(); }

Error MarketDataSource::poll(std::size_t budget,
                             std::size_t& dispatched) noexcept {
  return impl_->poll(budget, dispatched);
}

void MarketDataSource::stop() noexcept { impl_->stop(); }

bool MarketDataSource::ready() const noexcept { return impl_->started; }

void MarketDataSource::retire_instrument(
    InstrumentId instrument_id) noexcept {
  impl_->retire_instrument(instrument_id);
}

Error MarketDataSource::enqueue_replay(
    std::span<const std::byte> record) noexcept {
  if (impl_->config.source != MdsSourceMode::Replay)
    return Error::InvalidState;
  try {
    impl_->replay.emplace_back(record.begin(), record.end());
    return Error::Ok;
  } catch (...) {
    return Error::CapacityExceeded;
  }
}

}  // namespace strategyframe
