#include <algorithm>
#include <array>
#include <cstddef>
#include <cstdint>
#include <stdexcept>
#include <string>
#include <string_view>
#include <vector>

#include <unistd.h>

#include "strategyframe/market_data_source.h"
#include "mds/transport/shared_ring.h"
#include "utils/md/wire_codec.h"

namespace {

void RequireAt(bool condition, int line) {
  if (!condition)
    throw std::runtime_error("MDS requirement failed at line " +
                             std::to_string(line));
}
#define Require(condition) RequireAt((condition), __LINE__)

struct Captured {
  std::uint32_t instruments{};
  std::uint32_t catalogs{};
  std::uint32_t bbo{};
  std::uint32_t books{};
  std::uint32_t aggregate_books{};
  std::uint32_t last_catalog_generation{};
  std::int64_t best_bid{};
  std::size_t bid_levels{};
  std::int64_t venue_quantity{};
};
std::size_t encode_ordinal{};

strategyframe::MarketDataSource::Sink Sink(Captured& captured) {
  return {
      &captured,
      [](void* value, const utils::md::Instrument&) noexcept {
        ++static_cast<Captured*>(value)->instruments;
        return true;
      },
      [](void* value, const utils::md::InstrumentCatalog&,
         std::uint32_t generation) noexcept {
        auto& out = *static_cast<Captured*>(value);
        ++out.catalogs;
        out.last_catalog_generation = generation;
        return true;
      },
      [](void* value, const strategyframe::BboUpdate& update) noexcept {
        auto& out = *static_cast<Captured*>(value);
        ++out.bbo;
        out.best_bid = update.bid.price.value;
        return update.bid.price.scale == 2;
      },
      [](void* value,
         const strategyframe::OrderBookUpdate& update) noexcept {
        auto& out = *static_cast<Captured*>(value);
        ++out.books;
        out.bid_levels = update.bids.size();
        return !update.bids.empty();
      },
      nullptr,
      [](void* value,
         const strategyframe::AggOrderBookUpdate& update) noexcept {
        auto& out = *static_cast<Captured*>(value);
        ++out.aggregate_books;
        if (update.bids.empty() ||
            update.bids.front().venue_quantities.empty())
          return false;
        out.venue_quantity =
            update.bids.front().venue_quantities.front();
        return true;
      },
      nullptr};
}

template <typename Encoder>
std::vector<std::byte> Encode(std::size_t capacity, Encoder encoder) {
  ++encode_ordinal;
  std::vector<std::byte> bytes(capacity);
  const auto result = encoder(bytes);
  if (!result)
    throw std::runtime_error("encode failed at ordinal " +
                             std::to_string(encode_ordinal));
  bytes.resize(result.size);
  return bytes;
}

utils::md::wire::HeaderFields Header(utils::md::MessageType,
                                    std::uint64_t sequence,
                                    std::uint32_t generation = 1,
                                    std::uint64_t instrument_id = 42) {
  utils::md::wire::HeaderFields result;
  result.instrument_id = instrument_id;
  result.bus_seq = sequence;
  result.source_seq = sequence;
  result.book_generation = generation;
  result.state = utils::md::BookState::Live;
  return result;
}

void ReplayMapping() {
  Captured captured;
  strategyframe::MdsConfig config;
  config.source = strategyframe::MdsSourceMode::Replay;
  strategyframe::MarketDataSource source(config, Sink(captured));

  utils::md::Instrument instrument;
  instrument.instrument_id = 42;
  instrument.venue = utils::md::Venue::Binance;
  instrument.product_type = utils::md::ProductType::Spot;
  instrument.price_scale = 2;
  instrument.quantity_scale = 3;
  instrument.tick_size = 1;
  instrument.lot_size = 1;
  utils::md::InstrumentCatalog catalog{};
  catalog.instrument_id = 42;
  catalog.venue = utils::md::Venue::Binance;
  catalog.product_type = utils::md::ProductType::Spot;
  catalog.price_scale = 2;
  catalog.quantity_scale = 3;
  catalog.tick_size = 1;
  catalog.lot_size = 1;
  constexpr std::string_view canonical_symbol{"BTCUSDT"};
  std::copy(canonical_symbol.begin(), canonical_symbol.end(),
            catalog.canonical_symbol.begin());
  auto catalog_bytes = Encode(
      sizeof(utils::md::wire::InstrumentCatalogRecord),
      [&](std::span<std::byte> output) {
        return utils::md::wire::EncodeInstrumentCatalog(
            output, Header(utils::md::MessageType::InstrumentCatalog, 1),
            catalog);
      });
  auto metadata = Encode(
      sizeof(utils::md::wire::InstrumentUpdateRecord),
      [&](std::span<std::byte> output) {
        return utils::md::wire::EncodeInstrument(output, Header(
            utils::md::MessageType::InstrumentUpdate, 1), instrument);
      });
  auto bbo = Encode(sizeof(utils::md::wire::BboRecord),
                    [&](std::span<std::byte> output) {
                      return utils::md::wire::EncodeBbo(
                          output, Header(utils::md::MessageType::Bbo, 2),
                          {10000, 5}, {10001, 7});
                    });
  auto begin = Encode(
      sizeof(utils::md::wire::SnapshotBeginRecord),
      [&](std::span<std::byte> output) {
        return utils::md::wire::EncodeSnapshotBegin(
            output, Header(utils::md::MessageType::SnapshotBegin, 3), 2, 2);
      });
  const std::array<utils::md::Level, 1> bid_levels{{{10000, 5}}};
  const std::array<utils::md::Level, 1> ask_levels{{{10001, 7}}};
  auto bid_chunk = Encode(
      sizeof(utils::md::wire::SnapshotChunkRecord),
      [&](std::span<std::byte> output) {
        return utils::md::wire::EncodeSnapshotChunk(
            output, Header(utils::md::MessageType::SnapshotChunk, 4), 0,
            utils::md::Side::Bid, bid_levels);
      });
  auto ask_chunk = Encode(
      sizeof(utils::md::wire::SnapshotChunkRecord),
      [&](std::span<std::byte> output) {
        return utils::md::wire::EncodeSnapshotChunk(
            output, Header(utils::md::MessageType::SnapshotChunk, 5), 0,
            utils::md::Side::Ask, ask_levels);
      });
  auto end = Encode(
      sizeof(utils::md::wire::SnapshotEndRecord),
      [&](std::span<std::byte> output) {
        return utils::md::wire::EncodeSnapshotEnd(
            output, Header(utils::md::MessageType::SnapshotEnd, 6), 2, 0);
      });

  utils::md::wire::AggOrderBookRecord aggregate{};
  aggregate.price_scale = 2;
  aggregate.quantity_scale = 3;
  aggregate.venue_slot_ids[0] = 1;
  aggregate.member_count = 1;
  aggregate.member_mask = 1;
  aggregate.active_mask = 1;
  aggregate.bid_count = 1;
  aggregate.bids[0].price = 9999;
  aggregate.bids[0].quantity = 9;
  aggregate.bids[0].venue_quantity[0] = 9;
  aggregate.bids[0].venue_mask = 1;
  aggregate.bids[0].contributor_count = 1;
  auto aggregate_bytes = Encode(
      sizeof(utils::md::wire::AggOrderBookRecord),
      [&](std::span<std::byte> output) {
        return utils::md::wire::EncodeAggOrderBook(
            output, Header(utils::md::MessageType::AggOrderBook, 7),
            aggregate);
      });

  for (const auto* record :
       {&catalog_bytes, &metadata, &bbo, &begin, &bid_chunk, &ask_chunk,
        &end, &aggregate_bytes})
    Require(source.enqueue_replay(*record) == strategyframe::Error::Ok);
  Require(source.start() == strategyframe::Error::Ok);
  std::size_t dispatched{};
  Require(source.poll(32, dispatched) == strategyframe::Error::Ok);
  Require(dispatched == 8 && captured.catalogs == 1 &&
          captured.instruments == 1 &&
          captured.bbo == 1 && captured.best_bid == 10000 &&
          captured.books == 1 && captured.bid_levels == 1 &&
          captured.aggregate_books == 1 &&
          captured.venue_quantity == 9);
}

void ReaderBudgetAndUnregister() {
  mds::transport::RingOptions options;
  options.name =
      "/strategyframe.test." + std::to_string(::getpid());
  options.ring_bytes = 4096;
  options.max_record_bytes = 512;
  options.max_readers = 4;
  options.create = true;
  options.unlink_on_close = true;
  auto opened = mds::transport::SharedRing::open(options);
  Require(static_cast<bool>(opened));

  strategyframe::MdsConfig config;
  config.source = strategyframe::MdsSourceMode::ExternalSharedMemory;
  config.segments.push_back({options.name, 4096, 512, 1, 1});
  Captured captured;
  auto mismatched = config;
  mismatched.segments[0].ring_bytes = 8192;
  strategyframe::MarketDataSource invalid(mismatched, Sink(captured));
  Require(invalid.start() == strategyframe::Error::InvalidConfig);
  {
    strategyframe::MarketDataSource first(config, Sink(captured));
    Require(first.start() == strategyframe::Error::Ok);
    Require(opened.value.active_reader_count() == 1);
    strategyframe::MarketDataSource second(config, Sink(captured));
    Require(second.start() == strategyframe::Error::CapacityExceeded);
  }
  Require(opened.value.active_reader_count() == 0);
}

void ExternalSharedMemoryLoopback() {
  mds::transport::RingOptions options;
  options.name =
      "/strategyframe.loopback." + std::to_string(::getpid());
  options.ring_bytes = 8192;
  options.max_record_bytes = 1024;
  options.max_readers = 2;
  options.create = true;
  options.unlink_on_close = true;
  auto opened = mds::transport::SharedRing::open(options);
  Require(static_cast<bool>(opened));

  strategyframe::MdsConfig config;
  config.source = strategyframe::MdsSourceMode::ExternalSharedMemory;
  config.segments.push_back({options.name, 8192, 1024, 2, 1});
  Captured captured;
  strategyframe::MarketDataSource source(config, Sink(captured));
  Require(source.start() == strategyframe::Error::Ok);

  utils::md::Instrument instrument;
  instrument.instrument_id = 42;
  instrument.venue = utils::md::Venue::Binance;
  instrument.product_type = utils::md::ProductType::Spot;
  instrument.price_scale = 2;
  instrument.quantity_scale = 3;
  utils::md::InstrumentCatalog catalog{};
  catalog.instrument_id = 42;
  catalog.venue = utils::md::Venue::Binance;
  catalog.product_type = utils::md::ProductType::Spot;
  catalog.price_scale = 2;
  catalog.quantity_scale = 3;
  catalog.tick_size = 1;
  catalog.lot_size = 1;
  constexpr std::string_view canonical{"BTCUSDT"};
  std::copy(canonical.begin(), canonical.end(),
            catalog.canonical_symbol.begin());
  auto catalog_generation_1 = Encode(
      sizeof(utils::md::wire::InstrumentCatalogRecord),
      [&](std::span<std::byte> output) {
        return utils::md::wire::EncodeInstrumentCatalog(
            output,
            Header(utils::md::MessageType::InstrumentCatalog, 1, 1),
            catalog);
      });
  catalog.expiry_unix_ns = 10;
  auto catalog_generation_2 = Encode(
      sizeof(utils::md::wire::InstrumentCatalogRecord),
      [&](std::span<std::byte> output) {
        return utils::md::wire::EncodeInstrumentCatalog(
            output,
            Header(utils::md::MessageType::InstrumentCatalog, 3, 2),
            catalog);
      });
  auto metadata = Encode(
      sizeof(utils::md::wire::InstrumentUpdateRecord),
      [&](std::span<std::byte> output) {
        return utils::md::wire::EncodeInstrument(
            output, Header(utils::md::MessageType::InstrumentUpdate, 1),
            instrument);
      });
  auto bbo = Encode(sizeof(utils::md::wire::BboRecord),
                    [&](std::span<std::byte> output) {
                      return utils::md::wire::EncodeBbo(
                          output, Header(utils::md::MessageType::Bbo, 2),
                          {10000, 5}, {10001, 7});
                    });
  Require(static_cast<bool>(
      opened.value.publish(1, catalog_generation_1)));
  Require(static_cast<bool>(
      opened.value.publish(2, catalog_generation_1)));
  Require(static_cast<bool>(
      opened.value.publish(3, catalog_generation_2)));
  Require(static_cast<bool>(opened.value.publish(4, metadata)));
  Require(static_cast<bool>(opened.value.publish(5, bbo)));
  std::size_t dispatched{};
  Require(source.poll(8, dispatched) == strategyframe::Error::Ok);
  Require(dispatched == 5 && captured.catalogs == 2 &&
          captured.last_catalog_generation == 2 &&
          captured.instruments == 1 &&
          captured.bbo == 1 && captured.best_bid == 10000);
  source.stop();
  Require(opened.value.active_reader_count() == 0);
}

void RetirementReusesProbeSlots() {
  Captured captured;
  strategyframe::MdsConfig config;
  config.source = strategyframe::MdsSourceMode::Replay;
  strategyframe::MarketDataSource source(config, Sink(captured));
  Require(source.start() == strategyframe::Error::Ok);

  const auto publish_identity = [&](std::uint64_t id,
                                    std::uint64_t sequence) {
    utils::md::InstrumentCatalog catalog{};
    catalog.instrument_id = id;
    catalog.venue = utils::md::Venue::Polymarket;
    catalog.product_type = utils::md::ProductType::BinaryOption;
    catalog.price_scale = 2;
    catalog.quantity_scale = 2;
    constexpr std::string_view symbol{"btc5mup"};
    std::copy(symbol.begin(), symbol.end(),
              catalog.canonical_symbol.begin());
    utils::md::Instrument instrument{};
    instrument.instrument_id = id;
    instrument.venue = catalog.venue;
    instrument.product_type = catalog.product_type;
    instrument.price_scale = catalog.price_scale;
    instrument.quantity_scale = catalog.quantity_scale;
    auto catalog_bytes = Encode(
        sizeof(utils::md::wire::InstrumentCatalogRecord),
        [&](std::span<std::byte> output) {
          return utils::md::wire::EncodeInstrumentCatalog(
              output,
              Header(utils::md::MessageType::InstrumentCatalog,
                     sequence, 1, id),
              catalog);
        });
    auto instrument_bytes = Encode(
        sizeof(utils::md::wire::InstrumentUpdateRecord),
        [&](std::span<std::byte> output) {
          return utils::md::wire::EncodeInstrument(
              output,
              Header(utils::md::MessageType::InstrumentUpdate,
                     sequence + 1, 1, id),
              instrument);
        });
    Require(source.enqueue_replay(catalog_bytes) ==
            strategyframe::Error::Ok);
    Require(source.enqueue_replay(instrument_bytes) ==
            strategyframe::Error::Ok);
  };
  const auto publish_bbo = [&](std::uint64_t id,
                               std::uint64_t sequence) {
    auto bytes = Encode(
        sizeof(utils::md::wire::BboRecord),
        [&](std::span<std::byte> output) {
          return utils::md::wire::EncodeBbo(
              output,
              Header(utils::md::MessageType::Bbo, sequence, 1, id),
              {10000, 1}, {10001, 1});
        });
    Require(source.enqueue_replay(bytes) == strategyframe::Error::Ok);
  };

  publish_identity(42, 1);
  publish_bbo(42, 3);
  std::size_t dispatched{};
  Require(source.poll(8, dispatched) == strategyframe::Error::Ok);
  Require(captured.bbo == 1);
  source.retire_instrument(42);

  constexpr std::uint64_t replacement = 42 + 4096;
  publish_identity(replacement, 4);
  publish_bbo(42, 6);
  publish_bbo(replacement, 7);
  Require(source.poll(8, dispatched) == strategyframe::Error::Ok);
  Require(captured.bbo == 2);
  source.retire_instrument(replacement);

  std::uint64_t sequence = 8;
  for (std::uint64_t window = 0; window < 32; ++window) {
    const std::uint64_t id = 42 + (window + 2) * 4096;
    publish_identity(id, sequence);
    publish_bbo(id, sequence + 2);
    Require(source.poll(8, dispatched) == strategyframe::Error::Ok);
    source.retire_instrument(id);
    sequence += 3;
  }
  Require(captured.bbo == 34);
}

}  // namespace

int main() {
  ReplayMapping();
  ReaderBudgetAndUnregister();
  ExternalSharedMemoryLoopback();
  RetirementReusesProbeSlots();
  return 0;
}
