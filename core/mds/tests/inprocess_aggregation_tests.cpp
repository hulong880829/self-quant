#include "mds/api/mds_api.h"
#include "mds/exchange/capabilities.h"
#include "mds/publish/wire_publisher.h"
#include "mds/transport/shared_ring.h"

#include <algorithm>
#include <array>
#include <cassert>
#include <chrono>
#include <cstring>
#include <iostream>
#include <string>
#include <thread>
#include <unistd.h>

namespace {

void copy_text(auto &destination, const char *text) {
  std::memcpy(destination.data(), text,
              std::min(destination.size() - 1, std::strlen(text)));
}

utils::md::Instrument instrument(utils::md::Venue venue,
                                 std::uint32_t id,
                                 const char *quote = "USDT",
                                 const char *symbol = "BTCUSDT") {
  utils::md::Instrument value{};
  value.instrument_id = id;
  value.venue = venue;
  value.product_type = utils::md::ProductType::Spot;
  value.price_scale = 2;
  value.quantity_scale = 3;
  value.tick_size = 1;
  value.lot_size = 1;
  value.contract_multiplier = 1;
  copy_text(value.base_asset, "BTC");
  copy_text(value.quote_asset, quote);
  copy_text(value.canonical_symbol, symbol);
  return value;
}

utils::md::EventHeader event(std::uint32_t id, std::uint64_t sequence,
                             utils::md::BookState state,
                             utils::md::BboOrigin origin =
                                 utils::md::BboOrigin::Unknown) {
  utils::md::EventHeader value{};
  value.instrument_id = id;
  value.book_generation = 1;
  value.source_seq = sequence;
  value.exchange_ts_ns = sequence * 1'000;
  value.state = state;
  value.bbo_origin = origin;
  return value;
}

mds::publish::WirePublisher publisher(
    const std::string &prefix, utils::md::Venue venue,
    std::string_view stream,
    std::string_view symbol = "BTCUSDT") {
  mds::transport::RingOptions options;
  options.name = mds::publish::make_publisher_segment_name(
      prefix,
      mds::exchange::segment_profile(venue,
                                     utils::md::ProductType::Spot),
      symbol, stream);
  options.ring_bytes = 1U << 20U;
  options.max_record_bytes = 64U << 10U;
  options.max_readers = 4;
  options.unlink_on_close = true;
  auto opened = mds::transport::SharedRing::open(options);
  assert(opened);
  return mds::publish::WirePublisher(std::move(opened.value));
}

void test_hyperliquid_usdc_conversion() {
  using namespace mds::api;
  const std::string prefix =
      "/mds.sdk.fx." + std::to_string(::getpid()) + "." +
      std::to_string(std::chrono::steady_clock::now()
                         .time_since_epoch()
                         .count());
  MdsConfig config;
  VenueProfile binance;
  binance.venue = "binance";
  binance.product = ProductType::Spot;
  binance.shm_prefix = prefix;
  VenueProfile hyperliquid = binance;
  hyperliquid.venue = "hyperliquid";
  config.venues = {binance, hyperliquid};
  assert(init(config));

  AggregateSubscription request;
  request.venues = {"binance", "hyperliquid"};
  request.product = ProductType::Spot;
  request.symbol = "BTCUSDT";
  request.ttl_us = 300'000'000;
  request.fx_ttl_us = 300'000'000;
  auto aggregate = register_agg_bbo(request);
  assert(aggregate);

  auto binance_ticker =
      publisher(prefix, utils::md::Venue::Binance, "ticker");
  auto hyperliquid_ticker = publisher(
      prefix, utils::md::Venue::Hyperliquid, "ticker", "BTCUSDC");
  auto fx_ticker = publisher(
      prefix, utils::md::Venue::Binance, "ticker", "USDCUSDT");
  assert(start());

  const auto binance_instrument =
      instrument(utils::md::Venue::Binance, 1);
  assert(binance_ticker.publish_instrument(
      event(1, 0, utils::md::BookState::Building),
      binance_instrument));
  assert(binance_ticker.publish_bbo(
      {event(1, 1, utils::md::BookState::Live,
             utils::md::BboOrigin::TickerStream),
       {9'900, 1'000}, {10'200, 2'000}}));

  const auto hyperliquid_instrument = instrument(
      utils::md::Venue::Hyperliquid, 2, "USDC", "BTCUSDC");
  assert(hyperliquid_ticker.publish_instrument(
      event(2, 0, utils::md::BookState::Building),
      hyperliquid_instrument));
  assert(hyperliquid_ticker.publish_bbo(
      {event(2, 1, utils::md::BookState::Live,
             utils::md::BboOrigin::TickerStream),
       {10'000, 1'000}, {10'000, 2'000}}));

  auto fx_instrument =
      instrument(utils::md::Venue::Binance, 3, "USDT", "USDCUSDT");
  copy_text(fx_instrument.base_asset, "USDC");
  assert(fx_ticker.publish_instrument(
      event(3, 0, utils::md::BookState::Building), fx_instrument));
  assert(fx_ticker.publish_bbo(
      {event(3, 1, utils::md::BookState::Live,
             utils::md::BboOrigin::TickerStream),
       {99, 1'000}, {101, 2'000}}));

  AggBboRecord record{};
  auto error = ErrorCode::AggregateNotReady;
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(10);
  while (error != ErrorCode::Ok &&
         std::chrono::steady_clock::now() < deadline) {
    error = try_read_agg_bbo(aggregate.value, record);
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
  assert(error == ErrorCode::Ok);
  assert(record.gated_bid.price == 9'900);
  assert(record.gated_ask.price == 10'100);
  assert(record.quote_asset[0] == 'U' &&
         record.quote_asset[1] == 'S' &&
         record.quote_asset[2] == 'D' &&
         record.quote_asset[3] == 'T');
  shutdown();
}

void publish_source(mds::publish::WirePublisher &ticker,
                    mds::publish::WirePublisher &book,
                    utils::md::Venue venue, std::uint32_t id,
                    std::int64_t bid, std::int64_t ask) {
  const auto metadata = instrument(venue, id);
  assert(ticker.publish_instrument(
      event(id, 0, utils::md::BookState::Building), metadata));
  assert(book.publish_instrument(
      event(id, 0, utils::md::BookState::Building), metadata));
  assert(ticker.publish_bbo(
      {event(id, 1, utils::md::BookState::Live,
             utils::md::BboOrigin::TickerStream),
       {bid, 1'000}, {ask, 2'000}}));
  const std::array<utils::md::Level, 2> bids{
      utils::md::Level{bid, 1'000},
      utils::md::Level{bid - 1, 3'000}};
  const std::array<utils::md::Level, 2> asks{
      utils::md::Level{ask, 2'000},
      utils::md::Level{ask + 1, 4'000}};
  assert(book.publish_snapshot(
      event(id, 1, utils::md::BookState::Live), bids, asks));
}

void publish_book_update(mds::publish::WirePublisher &book,
                         std::uint32_t id, std::uint64_t sequence,
                         std::int64_t bid, std::int64_t ask) {
  const std::array<utils::md::Level, 2> bids{
      utils::md::Level{bid, 1'000},
      utils::md::Level{bid - 1, 3'000}};
  const std::array<utils::md::Level, 2> asks{
      utils::md::Level{ask, 2'000},
      utils::md::Level{ask + 1, 4'000}};
  assert(book.publish_snapshot(
      event(id, sequence, utils::md::BookState::Live), bids, asks));
}

}  // namespace

int main() {
  using namespace mds::api;
  static_assert(static_cast<std::uint16_t>(ErrorCode::AlreadyStarted) == 15);
  static_assert(static_cast<std::uint16_t>(ErrorCode::AggregateNotReady) ==
                16);

  AggregateSubscription request;
  request.venues = {"binance", "okx"};
  request.product = ProductType::Spot;
  request.symbol = "BTCUSDT";
  request.ttl_us = 300'000'000;
  assert(register_agg_bbo(request).error == ErrorCode::NotInitialized);

  const std::string prefix =
      "/mds.sdk.agg." + std::to_string(::getpid()) + "." +
      std::to_string(std::chrono::steady_clock::now()
                         .time_since_epoch()
                         .count());
  MdsConfig config;
  VenueProfile binance;
  binance.venue = "binance";
  binance.product = ProductType::Spot;
  binance.shm_prefix = prefix;
  VenueProfile okx = binance;
  okx.venue = "okx";
  config.venues = {binance, okx};
  auto initialized = init(config);
  assert(initialized);

  auto invalid = request;
  invalid.venues = {"binance", "BINANCE"};
  assert(register_agg_bbo(invalid).error == ErrorCode::InvalidConfig);

  auto bbo = register_agg_bbo(request);
  auto depth = register_agg_orderbook(request);
  auto short_ttl_request = request;
  short_ttl_request.ttl_us = 20'000;
  auto short_ttl_depth = register_agg_orderbook(short_ttl_request);
  assert(bbo && depth && short_ttl_depth && bbo.value != depth.value &&
         depth.value != short_ttl_depth.value);
  AggBboRecord bbo_record{};
  AggOrderBookRecord book_record{};
  assert(try_read_agg_bbo(bbo.value, bbo_record) ==
         ErrorCode::AggregateNotReady);
  assert(try_read_agg_orderbook(bbo.value, book_record) ==
         ErrorCode::SubscriptionTypeMismatch);

  auto binance_ticker =
      publisher(prefix, utils::md::Venue::Binance, "ticker");
  auto binance_book =
      publisher(prefix, utils::md::Venue::Binance, "orderbook");
  auto okx_ticker = publisher(prefix, utils::md::Venue::Okx, "ticker");
  auto okx_book = publisher(prefix, utils::md::Venue::Okx, "orderbook");
  auto started = start();
  assert(started);
  assert(register_agg_bbo(request).error == ErrorCode::AlreadyStarted);
  publish_source(binance_ticker, binance_book,
                 utils::md::Venue::Binance, 1, 10'000, 10'020);
  publish_source(okx_ticker, okx_book, utils::md::Venue::Okx, 2, 10'010,
                 10'030);

  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(10);
  ErrorCode bbo_error = ErrorCode::AggregateNotReady;
  ErrorCode book_error = ErrorCode::AggregateNotReady;
  while (std::chrono::steady_clock::now() < deadline &&
         (bbo_error != ErrorCode::Ok || book_error != ErrorCode::Ok ||
          bbo_record.live_mask != 3 || book_record.active_mask != 3)) {
    bbo_error = try_read_agg_bbo(bbo.value, bbo_record);
    book_error =
        try_read_agg_orderbook(depth.value, book_record);
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
  if (bbo_error != ErrorCode::Ok || book_error != ErrorCode::Ok ||
      bbo_record.live_mask != 3 || book_record.active_mask != 3) {
    std::cerr << "aggregate readiness timeout bbo_error="
              << static_cast<unsigned>(bbo_error)
              << " book_error=" << static_cast<unsigned>(book_error)
              << " bbo_state="
              << static_cast<unsigned>(query_state(bbo.value))
              << " book_state="
              << static_cast<unsigned>(query_state(depth.value))
              << " live_mask=" << bbo_record.live_mask
              << " active_mask=" << book_record.active_mask << '\n';
  }
  assert(bbo_error == ErrorCode::Ok);
  assert(book_error == ErrorCode::Ok);
  assert(bbo_record.gated_bid.price == 10'010);
  assert(bbo_record.gated_ask.price == 10'020);
  assert(bbo_record.member_count == 2);
  assert((bbo_record.header.flags & utils::md::wire::kBboOriginMask) == 0);
  assert(book_record.bid_count >= 2 && book_record.ask_count >= 2);
  assert((book_record.header.flags & utils::md::wire::kBboOriginMask) == 0);
  assert(book_record.bids[0].price == 10'010);
  assert(book_record.asks[0].price == 10'020);
  assert(query_state(bbo.value) == SubscriptionState::Live);
  assert(query_state(depth.value) == SubscriptionState::Live);

  AggOrderBookRecord short_ttl_book{};
  ErrorCode short_ttl_error = ErrorCode::AggregateNotReady;
  const auto short_ttl_deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(10);
  while (std::chrono::steady_clock::now() < short_ttl_deadline &&
         (short_ttl_error != ErrorCode::Ok ||
          short_ttl_book.active_mask != 3)) {
    short_ttl_error =
        try_read_agg_orderbook(short_ttl_depth.value, short_ttl_book);
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
  assert(short_ttl_error == ErrorCode::Ok);
  assert(short_ttl_book.active_mask == 3);
  const auto last_good_sequence = short_ttl_book.header.source_seq;

  std::this_thread::sleep_for(std::chrono::milliseconds(60));
  assert(try_read_agg_orderbook(short_ttl_depth.value, short_ttl_book) ==
         ErrorCode::Ok);
  assert(short_ttl_book.header.source_seq == last_good_sequence);
  assert(short_ttl_book.active_mask == 3);
  assert(query_state(short_ttl_depth.value) == SubscriptionState::Live);

  publish_book_update(binance_book, 1, 2, 10'001, 10'021);
  const auto recovery_deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(10);
  while (std::chrono::steady_clock::now() < recovery_deadline &&
         short_ttl_book.header.source_seq == last_good_sequence) {
    assert(try_read_agg_orderbook(short_ttl_depth.value, short_ttl_book) ==
           ErrorCode::Ok);
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
  assert(short_ttl_book.header.source_seq != last_good_sequence);
  assert(short_ttl_book.active_mask == 1);
  assert(short_ttl_book.bids[0].price == 10'001);

  shutdown();
  assert(try_read_agg_bbo(bbo.value, bbo_record) ==
         ErrorCode::NotInitialized);
  test_hyperliquid_usdc_conversion();
  return 0;
}
