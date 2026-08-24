#include "mds/agg/aggregation_engine.h"
#include "mds/agg/venue_ingest.h"
#include "utils/md/wire_codec.h"

#include <algorithm>
#include <array>
#include <atomic>
#include <cassert>
#include <cstddef>
#include <cstdint>
#include <cstring>
#include <cstdlib>
#include <limits>
#include <new>
#include <string_view>
#include <utility>

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

using mds::agg::AggregationEngine;
using utils::md::BookState;
using utils::md::Venue;
using utils::md::wire::BboRecord;

BboRecord bbo(std::int64_t bid, std::int64_t ask,
              std::uint64_t exchange_ns, std::uint64_t source = 1) {
  BboRecord result{};
  result.header.message_type =
      static_cast<std::uint16_t>(utils::md::MessageType::Bbo);
  result.header.state = static_cast<std::uint8_t>(BookState::Live);
  result.header.book_generation = 1;
  result.header.exchange_ts_ns = exchange_ns;
  result.header.source_seq = source;
  result.bid_price = bid;
  result.bid_quantity = 10;
  result.ask_price = ask;
  result.ask_quantity = 20;
  return result;
}

mds::agg::EngineConfig config(bool observe = true) {
  mds::agg::EngineConfig result{};
  result.instrument_id = 1;
  result.base_asset[0] = 'B';
  result.quote_asset[0] = 'U';
  result.price_scale = 2;
  result.quantity_scale = 2;
  result.cross_skew_observe_only = observe;
  result.cross_skew_threshold_us = 50'000;
  return result;
}

void test_raw_liveness_formula() {
  using mds::agg::derive_raw_liveness_us;
  constexpr std::array<std::pair<std::uint64_t, std::uint64_t>, 4>
      boundaries{{{10'000, 100'000},
                  {500'000, 5'000'000},
                  {1'000'000, 5'000'000},
                  {10'000'000, 10'000'000}}};
  for (const auto [ttl, expected] : boundaries) {
    assert(derive_raw_liveness_us(ttl) == expected);
    assert(derive_raw_liveness_us(ttl) >= ttl);
  }
  const auto huge = std::numeric_limits<std::uint64_t>::max() - 1;
  assert(derive_raw_liveness_us(huge) == huge);
  assert(derive_raw_liveness_us(huge) >= huge);
}

void test_ttl_raw_and_expiry_dedup() {
  AggregationEngine engine(config());
  assert(engine.add_member({Venue::Binance, 10'000, 2, 2, false}));
  assert(engine.update_bbo(0, bbo(10'000, 10'100, 1'000), 1'000'000));

  const auto live = engine.build_bbo(5'000'000);
  assert(live.record.live_mask == 1);
  assert(live.record.raw_bid.venue_mask == 1);

  const auto raw_only = engine.build_bbo(12'000'000);
  assert(raw_only.changed);
  assert(raw_only.record.live_mask == 0);
  assert(raw_only.record.raw_bid.venue_mask == 1);
  assert((raw_only.record.live_mask & ~raw_only.record.raw_bid.venue_mask) ==
         0);

  const auto expired = engine.build_bbo(102'000'000);
  assert(expired.changed);
  assert(expired.record.member_mask == 1);
  assert(expired.record.live_mask == 0);
  assert(expired.record.raw_bid.venue_mask == 0);
  assert(!engine.build_bbo(103'000'000).changed);
}

void test_timer_boundaries_preserve_raw_superset() {
  constexpr std::array<std::uint64_t, 4> ttls{
      10'000, 500'000, 1'000'000, 10'000'000};
  for (const auto ttl : ttls) {
    AggregationEngine engine(config());
    assert(engine.add_member({Venue::Binance, ttl, 2, 2, false}));
    constexpr std::uint64_t ingress_ns = 1'000'000;
    assert(engine.update_bbo(
        0, bbo(10'000, 10'100, 1'000), ingress_ns));

    const auto at_ttl =
        engine.build_bbo(ingress_ns + ttl * 1'000U);
    assert(at_ttl.record.live_mask == 1);
    assert(at_ttl.record.raw_bid.venue_mask == 1);
    assert((at_ttl.record.live_mask &
            ~at_ttl.record.raw_bid.venue_mask) == 0);

    const auto raw_liveness = mds::agg::derive_raw_liveness_us(ttl);
    if (raw_liveness > ttl) {
      const auto raw_only = engine.build_bbo(
          ingress_ns + (ttl + 1) * 1'000U);
      assert(raw_only.record.live_mask == 0);
      assert(raw_only.record.raw_bid.venue_mask == 1);
    }

    const auto expired = engine.build_bbo(
        ingress_ns + (raw_liveness + 1) * 1'000U);
    assert(expired.record.member_mask == 1);
    assert(expired.record.live_mask == 0);
    assert(expired.record.raw_bid.venue_mask == 0);
    assert((expired.record.live_mask &
            ~expired.record.raw_bid.venue_mask) == 0);
  }
}

void test_time_only_fields_do_not_publish() {
  AggregationEngine engine(config());
  assert(engine.add_member({Venue::Binance, 1'000'000, 2, 2, false}));
  assert(engine.update_bbo(0, bbo(10'000, 10'100, 1'000), 1'000'000));
  assert(engine.build_bbo(2'000'000).changed);
  const auto age_only = engine.build_bbo(3'000'000);
  assert(!age_only.changed);
  assert(age_only.record.gated_bid.worst_ingress_age_us == 2'000);

  AggregationEngine fx_engine(config());
  assert(fx_engine.add_member(
      {Venue::Hyperliquid, 1'000'000, 2, 2, true}));
  fx_engine.update_fx(
      bbo(100, 100, 1'000), 2, 1'000'000, Venue::Binance);
  assert(fx_engine.update_bbo(
      0, bbo(10'000, 10'100, 1'000), 1'000'000));
  assert(fx_engine.build_bbo(2'000'000).changed);
  const auto fx_age_only = fx_engine.build_bbo(3'000'000);
  assert(!fx_age_only.changed);
  assert(fx_age_only.record.fx_age_us == 2'000);
}

void test_cross_skew_observe_and_enforce() {
  AggregationEngine observe(config(true));
  assert(observe.add_member({Venue::Binance, 2'000'000, 2, 2, false}));
  assert(observe.add_member({Venue::Okx, 2'000'000, 2, 2, false}));
  assert(observe.update_bbo(0, bbo(11'000, 12'000, 1'000'000'000),
                            1'000'000));
  assert(observe.update_bbo(1, bbo(10'000, 10'500, 2'000'000'000),
                            1'000'000));
  const auto observed = observe.build_bbo(2'000'000);
  assert(observed.record.gated_cross_bps > 0);
  assert(observed.record.skew_us == 1'000'000);
  assert(observed.flags == 0);

  AggregationEngine enforce(config(false));
  assert(enforce.add_member({Venue::Binance, 2'000'000, 2, 2, false}));
  assert(enforce.add_member({Venue::Okx, 2'000'000, 2, 2, false}));
  assert(enforce.update_bbo(0, bbo(11'000, 12'000, 1'000'000'000),
                            1'000'000));
  assert(enforce.update_bbo(1, bbo(10'000, 10'500, 2'000'000'000),
                            1'000'000));
  const auto enforced = enforce.build_bbo(2'000'000);
  assert(enforced.flags == utils::md::wire::kAggSkewEnforced);
  assert(enforced.record.live_mask == 2);
  assert(enforced.record.gated_bid.price == 10'000);
  assert(enforced.record.gated_ask.price == 10'500);
}

void test_cross_skew_evidence_iteration_and_recovery() {
  AggregationEngine small(config(false));
  assert(small.add_member({Venue::Binance, 2'000'000, 2, 2, false}));
  assert(small.add_member({Venue::Okx, 2'000'000, 2, 2, false}));
  assert(small.update_bbo(
      0, bbo(11'000, 12'000, 1'000'000'000), 1'000'000));
  assert(small.update_bbo(
      1, bbo(10'000, 10'500, 1'010'000'000), 1'000'000));
  const auto within_threshold = small.build_bbo(2'000'000);
  assert(within_threshold.record.gated_cross_bps > 0);
  assert(within_threshold.record.skew_us == 10'000);
  assert(within_threshold.flags == 0);
  assert(within_threshold.record.live_mask == 3);

  AggregationEngine missing(config(false));
  assert(missing.add_member({Venue::Binance, 2'000'000, 2, 2, false}));
  assert(missing.add_member({Venue::Okx, 2'000'000, 2, 2, false}));
  assert(missing.update_bbo(0, bbo(11'000, 12'000, 0), 1'000'000));
  assert(missing.update_bbo(
      1, bbo(10'000, 10'500, 2'000'000'000), 1'000'000));
  const auto no_evidence = missing.build_bbo(2'000'000);
  assert(no_evidence.record.gated_cross_bps > 0);
  assert(no_evidence.flags == 0);
  assert(no_evidence.record.live_mask == 3);

  AggregationEngine iterative(config(false));
  assert(iterative.add_member(
      {Venue::Binance, 2'000'000, 2, 2, false}));
  assert(iterative.add_member({Venue::Okx, 2'000'000, 2, 2, false}));
  assert(iterative.add_member({Venue::Bybit, 2'000'000, 2, 2, false}));
  assert(iterative.update_bbo(
      0, bbo(13'000, 14'000, 1'000'000'000), 1'000'000));
  assert(iterative.update_bbo(
      1, bbo(12'000, 12'500, 2'000'000'000), 1'000'000));
  assert(iterative.update_bbo(
      2, bbo(10'000, 11'000, 3'000'000'000), 1'000'000));
  const auto filtered = iterative.build_bbo(2'000'000);
  assert(filtered.flags == utils::md::wire::kAggSkewEnforced);
  assert(filtered.record.live_mask == 4);
  assert(filtered.record.gated_bid.price == 10'000);
  assert(filtered.record.gated_ask.price == 11'000);

  assert(iterative.update_bbo(
      0, bbo(13'000, 14'000, 4'000'000'000), 3'000'000));
  const auto recovered = iterative.build_bbo(4'000'000);
  assert((recovered.record.live_mask & 1U) != 0);
}

void test_cross_skew_seven_round_bound() {
  AggregationEngine engine(config(false));
  for (std::size_t slot = 0; slot < mds::agg::kMaxMembers; ++slot) {
    assert(engine.add_member(
        {static_cast<Venue>(slot + 1), 2'000'000, 2, 2, false}));
    assert(engine.update_bbo(
        slot,
        bbo(18'000 - static_cast<std::int64_t>(slot) * 1'000,
            18'500 - static_cast<std::int64_t>(slot) * 1'000,
            (slot + 1) * 1'000'000'000ULL),
        1'000'000));
  }
  const auto built = engine.build_bbo(2'000'000);
  assert(built.flags == utils::md::wire::kAggSkewEnforced);
  assert(built.record.live_mask == (1U << 7U));
  assert(built.record.gated_bid.price == 11'000);
  assert(built.record.gated_ask.price == 11'500);
}

void test_timestamp_venue_differs_from_best_venue() {
  AggregationEngine engine(config(true));
  assert(engine.add_member({Venue::Binance, 1'000'000, 2, 2, false}));
  assert(engine.add_member({Venue::Okx, 1'000'000, 2, 2, false}));
  assert(engine.update_bbo(
      0, bbo(10'000, 10'200, 2'000'000'000), 1'000'000));
  assert(engine.update_bbo(
      1, bbo(10'000, 10'300, 1'000'000'000), 1'000'000));
  const auto built = engine.build_bbo(2'000'000);
  assert(built.record.gated_bid.best_venue == 0);
  assert(built.record.gated_bid.timestamp_venue == 1);
  assert(built.record.gated_bid.venue_mask == 3);
  assert(built.record.gated_bid.venue_quantity[0] == 10);
  assert(built.record.gated_bid.venue_quantity[1] == 10);
}

void test_same_member_cross_is_reported() {
  AggregationEngine engine(config(false));
  assert(engine.add_member({Venue::Binance, 1'000'000, 2, 2, false}));
  assert(engine.update_bbo(0, bbo(10'100, 10'000, 0),
                           1'000'000));
  const auto built = engine.build_bbo(2'000'000);
  assert(built.member_data_error);
  assert(built.flags == utils::md::wire::kAggMemberDataError);
  assert(built.record.gated_cross_bps > 0);
}

void test_fx_and_orderbook_merge() {
  AggregationEngine engine(config());
  assert(engine.add_member(
      {Venue::Hyperliquid, 1'000'000, 2, 2, true}));
  engine.update_fx(bbo(99, 101, 1'000), 2, 1'000'000, Venue::Binance);
  assert(engine.update_bbo(0, bbo(10'000, 10'000, 2'000), 1'000'000));
  const auto converted = engine.build_bbo(2'000'000);
  assert(converted.record.gated_bid.price == 9'900);
  assert(converted.record.gated_ask.price == 10'100);

  AggregationEngine book_engine(config());
  assert(book_engine.add_member({Venue::Binance, 1'000'000, 2, 2, false}));
  assert(book_engine.add_member({Venue::Okx, 1'000'000, 2, 2, false}));
  const std::array<utils::md::Level, 2> bids{{{10'000, 3}, {9'900, 4}}};
  const std::array<utils::md::Level, 2> asks{{{10'100, 5}, {10'200, 6}}};
  assert(book_engine.update_book(
      0, {bids, asks, 1'000, 1'000'000, 1}));
  assert(book_engine.update_book(
      1, {bids, asks, 2'000, 1'000'000, 1}));
  const auto book = book_engine.build_orderbook(2'000'000);
  assert(book.record.active_mask == 3);
  assert(book.record.bid_count == 2);
  assert(book.record.bids[0].price == 10'000);
  assert(book.record.bids[0].quantity == 6);
  assert(book.record.bids[0].venue_quantity[0] == 3);
  assert(book.record.bids[0].venue_quantity[1] == 3);
}

void test_fixed_capacity_exact_price_k_way_merge() {
  AggregationEngine engine(config());
  assert(engine.add_member({Venue::Binance, 1'000'000, 2, 2, false}));
  assert(engine.add_member({Venue::Okx, 1'000'000, 2, 2, false}));

  std::array<utils::md::Level, 12> first_bids{};
  std::array<utils::md::Level, 12> first_asks{};
  std::array<utils::md::Level, 10> second_bids{};
  std::array<utils::md::Level, 10> second_asks{};
  for (std::size_t index = 0; index < first_bids.size(); ++index) {
    first_bids[index] = {10'000 - static_cast<std::int64_t>(index) * 10, 1};
    first_asks[index] = {10'100 + static_cast<std::int64_t>(index) * 10, 2};
  }
  for (std::size_t index = 0; index < second_bids.size(); ++index) {
    second_bids[index] = {9'995 - static_cast<std::int64_t>(index) * 10, 3};
    second_asks[index] = {10'105 + static_cast<std::int64_t>(index) * 10, 4};
  }
  assert(engine.update_book(
      0, {first_bids, first_asks, 1'000, 1'000'000, 1}));
  assert(engine.update_book(
      1, {second_bids, second_asks, 2'000, 1'000'000, 1}));

  const auto built = engine.build_orderbook(2'000'000);
  assert(built.record.member_mask == 3);
  assert(built.record.active_mask == 3);
  assert(built.record.bid_count == 20);
  assert(built.record.ask_count == 20);
  assert(built.record.bids[0].price == 10'000);
  assert(built.record.bids[1].price == 9'995);
  assert(built.record.bids[19].price == 9'905);
  assert(built.record.asks[0].price == 10'100);
  assert(built.record.asks[19].price == 10'195);
  for (std::size_t index = 1; index < built.record.bid_count; ++index) {
    assert(built.record.bids[index - 1].price >
           built.record.bids[index].price);
  }
  for (std::size_t index = 1; index < built.record.ask_count; ++index) {
    assert(built.record.asks[index - 1].price <
           built.record.asks[index].price);
  }

  AggregationEngine exact(config());
  assert(exact.add_member({Venue::Binance, 1'000'000, 2, 2, false}));
  assert(exact.add_member({Venue::Okx, 1'000'000, 2, 2, false}));
  const std::array<utils::md::Level, 2> bids_a{{{10'000, 2}, {9'900, 1}}};
  const std::array<utils::md::Level, 2> asks_a{{{10'100, 2}, {10'200, 1}}};
  const std::array<utils::md::Level, 2> bids_b{{{10'000, 3}, {9'800, 1}}};
  const std::array<utils::md::Level, 2> asks_b{{{10'100, 4}, {10'300, 1}}};
  assert(exact.update_book(0, {bids_a, asks_a, 0, 1'000'000, 1}));
  assert(exact.update_book(1, {bids_b, asks_b, 0, 1'000'000, 1}));
  const auto merged = exact.build_orderbook(2'000'000);
  assert(merged.record.bids[0].quantity == 5);
  assert(merged.record.bids[0].venue_quantity[0] == 2);
  assert(merged.record.bids[0].venue_quantity[1] == 3);
  assert(merged.record.bids[0].venue_mask == 3);
  assert(merged.record.bids[0].contributor_count == 2);
}

void test_mixed_scale_exact_price_identity() {
  AggregationEngine exact(config());
  assert(exact.add_member({Venue::Binance, 1'000'000, 2, 2, false}));
  assert(exact.add_member({Venue::Okx, 1'000'000, 1, 1, false}));
  assert(exact.update_bbo(0, bbo(10'000, 10'100, 1'000), 1'000'000));
  auto scaled = bbo(1'000, 1'010, 2'000);
  scaled.bid_quantity = 1;
  scaled.ask_quantity = 2;
  assert(exact.update_bbo(1, scaled, 1'000'000));
  const auto merged = exact.build_bbo(2'000'000);
  assert(merged.record.gated_bid.price == 10'000);
  assert(merged.record.gated_bid.venue_mask == 3);
  assert(merged.record.gated_bid.quantity == 20);
  assert(merged.record.gated_bid.venue_quantity[0] == 10);
  assert(merged.record.gated_bid.venue_quantity[1] == 10);

  AggregationEngine distinct(config());
  assert(distinct.add_member({Venue::Binance, 1'000'000, 2, 2, false}));
  assert(distinct.add_member({Venue::Okx, 1'000'000, 1, 1, false}));
  assert(distinct.update_bbo(
      0, bbo(10'000, 10'200, 1'000), 1'000'000));
  assert(distinct.update_bbo(
      1, bbo(1'001, 1'030, 2'000), 1'000'000));
  const auto separated = distinct.build_bbo(2'000'000);
  assert(separated.record.gated_bid.price == 10'010);
  assert(separated.record.gated_bid.venue_mask == 2);
  assert(separated.record.gated_bid.contributor_count == 1);
}

void test_eight_members_fill_eighty_levels() {
  AggregationEngine engine(config());
  for (std::size_t slot = 0; slot < mds::agg::kMaxMembers; ++slot) {
    assert(engine.add_member(
        {static_cast<Venue>(slot + 1), 1'000'000, 2, 2, false}));
    std::array<utils::md::Level, mds::agg::kLevelsPerMember> bids{};
    std::array<utils::md::Level, mds::agg::kLevelsPerMember> asks{};
    for (std::size_t level = 0; level < bids.size(); ++level) {
      bids[level] = {
          100'000 - static_cast<std::int64_t>(level * 8 + slot), 1};
      asks[level] = {
          110'000 + static_cast<std::int64_t>(level * 8 + slot), 1};
    }
    assert(engine.update_book(
        slot, {bids, asks, 1'000 + slot, 1'000'000, 1}));
  }
  const auto built = engine.build_orderbook(2'000'000);
  assert(built.record.member_mask == 0xff);
  assert(built.record.active_mask == 0xff);
  assert(built.record.bid_count == utils::md::wire::kAggMaxLevelsPerSide);
  assert(built.record.ask_count == utils::md::wire::kAggMaxLevelsPerSide);
  for (std::size_t index = 1; index < built.record.bid_count; ++index) {
    assert(built.record.bids[index - 1].price >
           built.record.bids[index].price);
    assert(built.record.asks[index - 1].price <
           built.record.asks[index].price);
  }
}

void test_orderbook_active_mask_and_legal_cross() {
  AggregationEngine engine(config());
  assert(engine.add_member({Venue::Binance, 100, 2, 2, false}));
  assert(engine.add_member({Venue::Okx, 1'000, 2, 2, false}));
  const std::array<utils::md::Level, 1> first_bids{{{11'000, 2}}};
  const std::array<utils::md::Level, 1> first_asks{{{12'000, 3}}};
  const std::array<utils::md::Level, 1> second_bids{{{10'000, 4}}};
  const std::array<utils::md::Level, 1> second_asks{{{10'500, 5}}};
  assert(engine.update_book(
      0, {first_bids, first_asks, 1'000, 1'000'000, 1}));
  assert(engine.update_book(
      1, {second_bids, second_asks, 2'000, 1'000'000, 1}));

  const auto crossed = engine.build_orderbook(1'050'000);
  assert(crossed.record.active_mask == 3);
  assert(crossed.record.bids[0].price >
         crossed.record.asks[0].price);
  assert(crossed.record.bids[0].venue_quantity[0] == 2);
  assert(crossed.record.bids[0].venue_quantity[1] == 0);

  const auto first_expired = engine.build_orderbook(1'101'000);
  assert(first_expired.record.member_mask == 3);
  assert(first_expired.record.active_mask == 2);
  for (std::size_t level = 0; level < first_expired.record.bid_count;
       ++level) {
    assert(first_expired.record.bids[level].venue_quantity[0] == 0);
  }
  for (std::size_t level = 0; level < first_expired.record.ask_count;
       ++level) {
    assert(first_expired.record.asks[level].venue_quantity[0] == 0);
  }
}

void test_member_rejoins_after_new_generation() {
  AggregationEngine engine(config());
  assert(engine.add_member({Venue::Binance, 1'000'000, 2, 2, false}));
  assert(engine.add_member({Venue::Okx, 1'000'000, 2, 2, false}));
  const std::array<utils::md::Level, 1> binance_bids{{{10'000, 2}}};
  const std::array<utils::md::Level, 1> binance_asks{{{10'100, 2}}};
  const std::array<utils::md::Level, 1> okx_bids{{{9'990, 3}}};
  const std::array<utils::md::Level, 1> okx_asks{{{10'110, 3}}};
  assert(engine.update_book(
      0, {binance_bids, binance_asks, 1, 1'000'000, 1}));
  assert(engine.update_book(
      1, {okx_bids, okx_asks, 1, 1'000'000, 1}));
  const auto initial = engine.build_orderbook(1'100'000);
  assert(initial.record.active_mask == 3);

  engine.invalidate_member(1);
  const auto invalidated = engine.build_orderbook(1'200'000);
  assert(invalidated.record.active_mask == 1);
  const auto invalidated_generation =
      invalidated.record.header.book_generation;

  assert(engine.update_book(
      1, {okx_bids, okx_asks, 2, 1'300'000, 2}));
  const auto rejoined = engine.build_orderbook(1'400'000);
  assert(rejoined.record.active_mask == 3);
  assert(rejoined.record.header.book_generation >
         invalidated_generation);
  bool okx_contribution = false;
  for (std::size_t index = 0; index < rejoined.record.bid_count; ++index) {
    okx_contribution =
        okx_contribution ||
        rejoined.record.bids[index].venue_quantity[1] != 0;
  }
  assert(okx_contribution);
}

void test_five_members_degrade_and_generation_rejoin() {
  AggregationEngine engine(config());
  constexpr std::array<Venue, 5> venues{
      Venue::Binance, Venue::Okx, Venue::Bybit, Venue::Bitget, Venue::Gate};
  for (std::size_t slot = 0; slot < venues.size(); ++slot) {
    const auto ttl = slot == 3 ? 100U : 1'000U;
    assert(engine.add_member({venues[slot], ttl, 2, 2, false}));
  }
  const std::array<utils::md::Level, 1> bids{{{10'000, 2}}};
  const std::array<utils::md::Level, 1> asks{{{10'100, 3}}};
  for (std::size_t slot = 0; slot < venues.size(); ++slot) {
    auto quote = bbo(10'000, 10'100, 1'000 + slot);
    assert(engine.update_bbo(slot, quote, 1'000'000));
    assert(engine.update_book(
        slot, {bids, asks, 1'000 + slot, 1'000'000, 1}));
  }
  assert(engine.build_bbo(1'050'000).record.live_mask == 0x1f);
  assert(engine.build_orderbook(1'050'000).record.active_mask == 0x1f);

  const auto degraded_bbo = engine.build_bbo(1'101'000);
  const auto degraded_book = engine.build_orderbook(1'101'000);
  assert(degraded_bbo.record.live_mask == 0x17);
  assert(degraded_book.record.active_mask == 0x17);
  for (std::size_t level = 0; level < degraded_book.record.bid_count;
       ++level) {
    assert(degraded_book.record.bids[level].venue_quantity[3] == 0);
  }
  for (std::size_t level = 0; level < degraded_book.record.ask_count;
       ++level) {
    assert(degraded_book.record.asks[level].venue_quantity[3] == 0);
  }
  const auto degraded_generation =
      degraded_book.record.header.book_generation;

  auto recovered_quote = bbo(10'010, 10'110, 2'000);
  recovered_quote.header.book_generation = 2;
  assert(engine.update_bbo(3, recovered_quote, 1'200'000));
  const auto bbo_before_image = engine.build_bbo(1'210'000);
  const auto book_before_image = engine.build_orderbook(1'210'000);
  assert((bbo_before_image.record.live_mask & (1U << 3U)) != 0);
  assert((book_before_image.record.active_mask & (1U << 3U)) == 0);

  assert(engine.update_book(
      3, {bids, asks, 2'000, 1'220'000, 2}));
  const auto rejoined = engine.build_orderbook(1'230'000);
  assert(rejoined.record.active_mask == 0x1f);
  assert(rejoined.record.header.book_generation > degraded_generation);
}

void test_slot_identity_metadata_and_fx_expiry() {
  AggregationEngine slots(config(false));
  assert(slots.add_member({Venue::Binance, 1'000'000, 2, 2, false}));
  assert(slots.add_member({Venue::Okx, 1'000'000, 2, 2, false}));
  assert(slots.update_bbo(0, bbo(10'000, 10'200, 1'000'000), 1'000'000));
  assert(slots.update_bbo(1, bbo(10'000, 10'200, 2'000'000), 1'000'000));
  const auto tied = slots.build_bbo(2'000'000);
  assert(tied.record.gated_bid.best_venue == 0);
  assert(tied.record.gated_bid.timestamp_venue == 0);
  assert(tied.record.gated_bid.venue_mask == 3);

  const auto generation = tied.record.header.book_generation;
  slots.invalidate_member(0);
  const auto invalidated = slots.build_bbo(2'000'000);
  assert(invalidated.record.header.book_generation == generation + 1);
  assert(invalidated.record.member_mask == 3);
  assert(invalidated.record.live_mask == 2);

  auto fx_config = config();
  fx_config.fx_ttl_us = 100;
  AggregationEngine fx(fx_config);
  assert(fx.add_member(
      {Venue::Hyperliquid, 1'000'000, 2, 2, true}));
  assert(!fx.add_member({Venue::Okx, 1'000'000, 2, 2, true}));
  fx.update_fx(bbo(100, 100, 0), 2, 1'000'000, Venue::Binance);
  assert(fx.update_bbo(0, bbo(10'000, 10'100, 0), 1'000'000));
  const std::array<utils::md::Level, 1> bids{{{10'000, 1}}};
  const std::array<utils::md::Level, 1> asks{{{10'100, 1}}};
  assert(fx.update_book(0, {bids, asks, 0, 1'000'000, 1}));
  assert(fx.build_bbo(1'050'000).record.live_mask == 1);
  assert(fx.build_orderbook(1'050'000).record.active_mask == 1);
  const auto stale_bbo = fx.build_bbo(1'101'000);
  const auto stale_book = fx.build_orderbook(1'101'000);
  assert(stale_bbo.record.live_mask == 0);
  assert(stale_bbo.record.raw_bid.venue_mask == 0);
  assert(stale_book.record.active_mask == 0);
}

void test_member_metadata_reconfiguration_keeps_scales_frozen() {
  AggregationEngine engine(config());
  assert(engine.add_member({Venue::Binance, 1'000'000, 2, 2, false}));
  assert(engine.update_bbo(0, bbo(10'000, 10'100, 1'000), 1'000'000));
  const auto initial = engine.build_bbo(2'000'000);
  const auto generation = initial.record.header.book_generation;

  assert(engine.reconfigure_member(
      0, {Venue::Binance, 1'000'000, 1, 1, false}));
  const auto excluded = engine.build_bbo(2'000'000);
  assert(excluded.record.header.book_generation == generation + 1);
  assert(excluded.record.member_mask == 1);
  assert(excluded.record.live_mask == 0);

  assert(engine.update_bbo(0, bbo(1'000, 1'010, 2'000), 3'000'000));
  const auto recovered = engine.build_bbo(4'000'000);
  assert(recovered.record.price_scale == 2);
  assert(recovered.record.quantity_scale == 2);
  assert(recovered.record.gated_bid.price == 10'000);
  assert(recovered.record.live_mask == 1);

  assert(!engine.reconfigure_member(
      0, {Venue::Binance, 1'000'000, 3, 2, false}));
  const auto incompatible = engine.build_bbo(4'000'000);
  assert(incompatible.record.member_mask == 0);
  assert(incompatible.record.live_mask == 0);
  assert(engine.reconfigure_member(
      0, {Venue::Binance, 1'000'000, 2, 2, false}));
  assert(engine.update_bbo(0, bbo(10'000, 10'100, 3'000), 5'000'000));
  const auto revalidated = engine.build_bbo(6'000'000);
  assert(revalidated.record.member_mask == 1);
  assert(revalidated.record.live_mask == 1);
}

void test_agg_bbo_abi_and_codec_flags() {
  static_assert(sizeof(utils::md::wire::AggBboRecord) == 416);
  AggregationEngine engine(config());
  assert(engine.add_member({Venue::Binance, 1'000'000, 2, 2, false}));
  assert(engine.update_bbo(0, bbo(10'000, 10'100, 1'000), 1'000'000));
  const auto built = engine.build_bbo(2'000'000);
  utils::md::wire::HeaderFields header{};
  header.instrument_id = 1;
  header.state = BookState::Live;
  std::array<std::byte, sizeof(utils::md::wire::AggBboRecord)> bytes{};
  auto encoded =
      utils::md::wire::EncodeAggBbo(bytes, header, built.record);
  assert(encoded && encoded.size == 416);
  utils::md::wire::AggBboRecord decoded{};
  assert(utils::md::wire::DecodeAggBbo(bytes, decoded) ==
         utils::md::wire::CodecError::Ok);
  header.flags = utils::md::wire::kAggSkewEnforced;
  assert(utils::md::wire::EncodeAggBbo(bytes, header, built.record));
  header.flags = utils::md::wire::kAggMemberDataError;
  assert(utils::md::wire::EncodeAggBbo(bytes, header, built.record));
  header.flags =
      utils::md::wire::kAggSkewEnforced |
      utils::md::wire::kAggMemberDataError;
  assert(utils::md::wire::EncodeAggBbo(bytes, header, built.record));
  header.flags = 1U << 2U;
  assert(!utils::md::wire::EncodeAggBbo(bytes, header, built.record));
}

void test_timestamp_slot_and_hot_path_allocations() {
  AggregationEngine engine(config());
  assert(engine.add_member({Venue::Binance, 1'000'000, 2, 2, false}));
  assert(engine.add_member({Venue::Okx, 1'000'000, 2, 2, false}));
  assert(engine.update_bbo(0, bbo(9'900, 10'300, 2'000), 1'000'000));
  assert(engine.update_bbo(1, bbo(10'000, 10'200, 0), 1'000'000));
  const std::array<utils::md::Level, 2> bids{
      utils::md::Level{10'000, 1}, utils::md::Level{9'900, 2}};
  const std::array<utils::md::Level, 2> asks{
      utils::md::Level{10'200, 1}, utils::md::Level{10'300, 2}};
  assert(engine.update_book(0, {bids, asks, 2'000, 1'000'000, 1}));
  assert(engine.update_book(1, {bids, asks, 0, 1'000'000, 1}));

  tracked_allocations.store(0, std::memory_order_relaxed);
  track_allocations.store(true, std::memory_order_release);
  const auto built_bbo = engine.build_bbo(2'000'000);
  const auto built_book = engine.build_orderbook(2'000'000);
  track_allocations.store(false, std::memory_order_release);
  assert(tracked_allocations.load(std::memory_order_relaxed) == 0);
  assert(built_bbo.record.gated_bid.best_venue == 1);
  assert(built_bbo.record.gated_bid.timestamp_venue == 1);
  assert(built_bbo.record.gated_bid.exchange_ts_ns == 0);
  assert(built_book.record.active_mask == 3);
}

void test_venue_ingest_bbo_and_sequence_gap() {
  mds::agg::VenueIngest ingest(32);
  utils::md::Instrument instrument{};
  instrument.instrument_id = 1;
  instrument.tick_size = 1;
  instrument.price_scale = 2;
  instrument.quantity_scale = 2;
  utils::md::wire::HeaderFields fields{};
  fields.instrument_id = 1;
  fields.bus_seq = 1;
  fields.book_generation = 1;
  fields.state = BookState::Live;
  std::array<std::byte,
             sizeof(utils::md::wire::InstrumentUpdateRecord)>
      instrument_bytes{};
  const auto encoded_instrument = utils::md::wire::EncodeInstrument(
      instrument_bytes, fields, instrument);
  assert(encoded_instrument);
  assert(ingest.consume(
             1, static_cast<std::uint32_t>(
                    utils::md::MessageType::InstrumentUpdate),
             instrument_bytes, 1'000) == mds::agg::IngestResult::Instrument);

  constexpr std::size_t kUnknownLength =
      sizeof(utils::md::wire::RecordHeader) +
      alignof(utils::md::wire::RecordHeader);
  std::array<std::byte, kUnknownLength> unknown_bytes{};
  auto unknown_header = utils::md::wire::MakeHeader(
      static_cast<utils::md::MessageType>(9999),
      static_cast<std::uint16_t>(kUnknownLength));
  unknown_header.instrument_id = 1;
  unknown_header.bus_seq = 2;
  std::memcpy(unknown_bytes.data(), &unknown_header, sizeof(unknown_header));
  assert(ingest.consume(2, 9999, unknown_bytes, 1'500) ==
         mds::agg::IngestResult::Ignored);

  fields.bus_seq = 3;
  fields.source_seq = 1;
  std::array<std::byte, sizeof(BboRecord)> bbo_bytes{};
  const auto encoded_bbo = utils::md::wire::EncodeBbo(
      bbo_bytes, fields, {10'000, 10}, {10'100, 20});
  assert(encoded_bbo);
  assert(ingest.consume(
             3, static_cast<std::uint32_t>(utils::md::MessageType::Bbo),
             bbo_bytes, 2'000) == mds::agg::IngestResult::Bbo);
  assert(ingest.bbo() != nullptr);
  assert(ingest.consume(
             5, static_cast<std::uint32_t>(utils::md::MessageType::Bbo),
             bbo_bytes, 3'000) == mds::agg::IngestResult::NeedResync);
}

void test_venue_ingest_generation_barrier_and_stale_rejection() {
  using namespace utils::md;
  using namespace utils::md::wire;
  mds::agg::VenueIngest ingest(32);
  std::uint64_t ring_sequence{};
  std::uint64_t bus_sequence{};

  Instrument instrument{};
  instrument.instrument_id = 7;
  instrument.venue = Venue::Bitget;
  instrument.product_type = ProductType::Spot;
  instrument.price_scale = 2;
  instrument.quantity_scale = 3;
  instrument.tick_size = 1;
  instrument.lot_size = 1;
  std::copy_n("BTCUSDT", 7, instrument.canonical_symbol.begin());

  HeaderFields fields{};
  fields.instrument_id = instrument.instrument_id;
  fields.bus_seq = ++bus_sequence;
  fields.book_generation = 1;
  fields.state = BookState::Building;
  std::array<std::byte, sizeof(InstrumentUpdateRecord)> instrument_bytes{};
  assert(EncodeInstrument(instrument_bytes, fields, instrument));
  assert(ingest.consume(
             ++ring_sequence,
             static_cast<std::uint32_t>(MessageType::InstrumentUpdate),
             instrument_bytes, 1'000) == mds::agg::IngestResult::Instrument);

  fields.bus_seq = ++bus_sequence;
  fields.source_seq = 1;
  fields.state = BookState::Live;
  std::array<std::byte, sizeof(BboRecord)> bbo_bytes{};
  assert(EncodeBbo(bbo_bytes, fields, {10'000, 1}, {10'100, 2}));
  assert(ingest.consume(
             ++ring_sequence, static_cast<std::uint32_t>(MessageType::Bbo),
             bbo_bytes, 2'000) == mds::agg::IngestResult::Bbo);
  assert(ingest.bbo() != nullptr);

  InstrumentCatalog catalog{};
  catalog.instrument_id = instrument.instrument_id;
  catalog.venue = instrument.venue;
  catalog.product_type = instrument.product_type;
  catalog.price_scale = instrument.price_scale;
  catalog.quantity_scale = instrument.quantity_scale;
  catalog.tick_size = instrument.tick_size;
  std::copy_n("BTCUSDT", 7, catalog.canonical_symbol.begin());
  fields.bus_seq = ++bus_sequence;
  fields.source_seq = 0;
  fields.book_generation = 2;
  fields.state = BookState::Building;
  std::array<std::byte, sizeof(InstrumentCatalogRecord)> catalog_bytes{};
  assert(EncodeInstrumentCatalog(catalog_bytes, fields, catalog));
  assert(ingest.consume(
             ++ring_sequence,
             static_cast<std::uint32_t>(MessageType::InstrumentCatalog),
             catalog_bytes, 3'000) == mds::agg::IngestResult::Ignored);
  assert(ingest.bbo() == nullptr);

  fields.bus_seq = ++bus_sequence;
  assert(EncodeInstrument(instrument_bytes, fields, instrument));
  assert(ingest.consume(
             ++ring_sequence,
             static_cast<std::uint32_t>(MessageType::InstrumentUpdate),
             instrument_bytes, 4'000) == mds::agg::IngestResult::Instrument);

  fields.bus_seq = ++bus_sequence;
  fields.source_seq = 2;
  fields.book_generation = 1;
  fields.state = BookState::Live;
  assert(EncodeBbo(bbo_bytes, fields, {9'000, 1}, {9'100, 2}));
  assert(ingest.consume(
             ++ring_sequence, static_cast<std::uint32_t>(MessageType::Bbo),
             bbo_bytes, 5'000) == mds::agg::IngestResult::NeedResync);
  assert(ingest.bbo() == nullptr);

  fields.bus_seq = ++bus_sequence;
  fields.source_seq = 1;
  fields.book_generation = 2;
  assert(EncodeBbo(bbo_bytes, fields, {10'010, 1}, {10'110, 2}));
  assert(ingest.consume(
             ++ring_sequence, static_cast<std::uint32_t>(MessageType::Bbo),
             bbo_bytes, 6'000) == mds::agg::IngestResult::Bbo);
  assert(ingest.bbo() != nullptr);
  assert(ingest.bbo()->header.book_generation == 2);
}

void test_venue_ingest_refines_flagged_snapshot_tick() {
  const auto load_snapshot = [](mds::agg::VenueIngest &ingest,
                                std::uint8_t instrument_flags) {
    utils::md::Instrument instrument{};
    instrument.instrument_id = 1;
    instrument.price_scale = 10;
    instrument.quantity_scale = 2;
    instrument.tick_size = 1'000'000;
    instrument.flags = instrument_flags;

    utils::md::wire::HeaderFields fields{};
    fields.instrument_id = 1;
    fields.bus_seq = 1;
    fields.source_seq = 1;
    fields.book_generation = 1;
    fields.state = BookState::Live;

    std::array<std::byte,
               sizeof(utils::md::wire::InstrumentUpdateRecord)>
        instrument_bytes{};
    assert(utils::md::wire::EncodeInstrument(instrument_bytes, fields,
                                             instrument));
    assert(ingest.consume(
               1, static_cast<std::uint32_t>(
                      utils::md::MessageType::InstrumentUpdate),
               instrument_bytes, 1'000) ==
           mds::agg::IngestResult::Instrument);

    fields.bus_seq = 2;
    std::array<std::byte,
               sizeof(utils::md::wire::SnapshotBeginRecord)>
        begin_bytes{};
    assert(utils::md::wire::EncodeSnapshotBegin(begin_bytes, fields, 4, 2));
    assert(ingest.consume(
               2, static_cast<std::uint32_t>(
                      utils::md::MessageType::SnapshotBegin),
               begin_bytes, 2'000) == mds::agg::IngestResult::Ignored);

    const std::array<utils::md::Level, 2> bids{
        utils::md::Level{5'738'300'000, 100},
        utils::md::Level{5'738'200'000, 90}};
    const std::array<utils::md::Level, 2> asks{
        utils::md::Level{5'738'400'000, 200},
        utils::md::Level{5'738'500'000, 210}};
    std::array<std::byte,
               sizeof(utils::md::wire::SnapshotChunkRecord)>
        chunk_bytes{};
    fields.bus_seq = 3;
    assert(utils::md::wire::EncodeSnapshotChunk(
        chunk_bytes, fields, 0, utils::md::Side::Bid, bids));
    assert(ingest.consume(
               3, static_cast<std::uint32_t>(
                      utils::md::MessageType::SnapshotChunk),
               chunk_bytes, 3'000) == mds::agg::IngestResult::Ignored);
    fields.bus_seq = 4;
    assert(utils::md::wire::EncodeSnapshotChunk(
        chunk_bytes, fields, 1, utils::md::Side::Ask, asks));
    assert(ingest.consume(
               4, static_cast<std::uint32_t>(
                      utils::md::MessageType::SnapshotChunk),
               chunk_bytes, 4'000) == mds::agg::IngestResult::Ignored);

    fields.bus_seq = 5;
    std::array<std::byte, sizeof(utils::md::wire::SnapshotEndRecord)>
        end_bytes{};
    assert(utils::md::wire::EncodeSnapshotEnd(end_bytes, fields, 4, 0));
    return ingest.consume(
        5,
        static_cast<std::uint32_t>(
            utils::md::MessageType::SnapshotEnd),
        end_bytes, 5'000);
  };

  mds::agg::VenueIngest refined(32);
  assert(load_snapshot(refined, utils::md::kInstrumentRefineBookTick) ==
         mds::agg::IngestResult::Book);
  mds::agg::BookInput input{};
  assert(refined.book_input(input));
  assert(input.bids[0].price == 5'738'300'000);
  assert(input.asks[0].price == 5'738'400'000);

  utils::md::wire::HeaderFields fields{};
  fields.instrument_id = 1;
  fields.bus_seq = 6;
  fields.source_seq = 2;
  fields.book_generation = 1;
  fields.state = BookState::Live;
  std::array<std::byte, sizeof(utils::md::wire::DeltaRecord)> delta_bytes{};
  assert(utils::md::wire::EncodeDelta(
      delta_bytes, fields, utils::md::Side::Bid,
      {5'738'200'000, 300}));
  assert(refined.consume(
             6, static_cast<std::uint32_t>(
                    utils::md::MessageType::BookDelta),
             delta_bytes, 6'000) == mds::agg::IngestResult::Book);

  // One exchange update is published as several wire records with the same
  // source sequence. Every record must update the reconstructed venue book.
  fields.source_seq = 3;
  fields.bus_seq = 7;
  assert(utils::md::wire::EncodeDelta(
      delta_bytes, fields, utils::md::Side::Bid,
      {5'738'300'000, 0}));
  assert(refined.consume(
             7, static_cast<std::uint32_t>(
                    utils::md::MessageType::BookDelta),
             delta_bytes, 7'000) == mds::agg::IngestResult::Book);
  fields.bus_seq = 8;
  assert(utils::md::wire::EncodeDelta(
      delta_bytes, fields, utils::md::Side::Bid,
      {5'738'200'000, 400}));
  assert(refined.consume(
             8, static_cast<std::uint32_t>(
                    utils::md::MessageType::BookDelta),
             delta_bytes, 8'000) == mds::agg::IngestResult::Book);
  fields.bus_seq = 9;
  assert(utils::md::wire::EncodeDelta(
      delta_bytes, fields, utils::md::Side::Ask,
      {5'738'400'000, 0}));
  assert(refined.consume(
             9, static_cast<std::uint32_t>(
                    utils::md::MessageType::BookDelta),
             delta_bytes, 9'000) == mds::agg::IngestResult::Book);
  fields.bus_seq = 10;
  assert(utils::md::wire::EncodeDelta(
      delta_bytes, fields, utils::md::Side::Ask,
      {5'738'700'000, 220}));
  assert(refined.consume(
             10, static_cast<std::uint32_t>(
                     utils::md::MessageType::BookDelta),
             delta_bytes, 10'000) == mds::agg::IngestResult::Book);

  assert(refined.book_input(input));
  assert(input.bids.size() == 1);
  assert(input.bids[0].price == 5'738'200'000);
  assert(input.bids[0].quantity == 400);
  assert(input.asks.size() == 2);
  assert(input.asks[0].price == 5'738'500'000);
  assert(input.asks[1].price == 5'738'700'000);

  fields.bus_seq = 11;
  fields.source_seq = 2;
  assert(utils::md::wire::EncodeDelta(
      delta_bytes, fields, utils::md::Side::Bid,
      {5'738'100'000, 500}));
  assert(refined.consume(
             11, static_cast<std::uint32_t>(
                     utils::md::MessageType::BookDelta),
             delta_bytes, 11'000) == mds::agg::IngestResult::NeedResync);

  mds::agg::VenueIngest strict(32);
  assert(load_snapshot(strict, 0) == mds::agg::IngestResult::NeedResync);
}

struct AggregatePair {
  utils::md::wire::AggBboRecord bbo;
  utils::md::wire::AggOrderBookRecord book;
};

AggregatePair aggregate_from_layout(bool multiplex) {
  using namespace utils::md;
  using namespace utils::md::wire;
  mds::agg::VenueIngest ingest(
      {Venue::Binance, ProductType::Perpetual, "BTCUSDT"}, 64);
  std::uint64_t ring_sequence = 0;
  std::uint64_t bus_sequence = 0;

  const auto publish_instrument = [&](Instrument value) {
    HeaderFields fields{};
    fields.instrument_id = value.instrument_id;
    fields.bus_seq = ++bus_sequence;
    fields.book_generation = 1;
    fields.state = BookState::Building;
    std::array<std::byte, sizeof(InstrumentUpdateRecord)> bytes{};
    assert(EncodeInstrument(bytes, fields, value));
    return ingest.consume(
        ++ring_sequence,
        static_cast<std::uint32_t>(MessageType::InstrumentUpdate), bytes,
        1'000);
  };
  const auto make_instrument = [](InstrumentId id, std::string_view symbol) {
    Instrument value{};
    value.instrument_id = id;
    value.venue = Venue::Binance;
    value.product_type = ProductType::Perpetual;
    value.price_scale = 2;
    value.quantity_scale = 2;
    value.tick_size = 1;
    value.lot_size = 1;
    std::copy(symbol.begin(), symbol.end(),
              value.canonical_symbol.begin());
    std::copy_n("BTC", 3, value.base_asset.begin());
    std::copy_n("USDT", 4, value.quote_asset.begin());
    return value;
  };

  assert(publish_instrument(make_instrument(11, "BTCUSDT")) ==
         mds::agg::IngestResult::Instrument);
  if (multiplex) {
    assert(publish_instrument(make_instrument(22, "ETHUSDT")) ==
           mds::agg::IngestResult::Ignored);
  }

  HeaderFields fields{};
  fields.instrument_id = 11;
  fields.bus_seq = ++bus_sequence;
  fields.source_seq = 1;
  fields.book_generation = 1;
  fields.exchange_ts_ns = 123'000;
  fields.state = BookState::Live;
  std::array<std::byte, sizeof(BboRecord)> bbo_bytes{};
  assert(EncodeBbo(bbo_bytes, fields, {10'000, 10}, {10'100, 20}));
  assert(ingest.consume(++ring_sequence,
                        static_cast<std::uint32_t>(MessageType::Bbo),
                        bbo_bytes, 10'000) ==
         mds::agg::IngestResult::Bbo);

  if (multiplex) {
    fields.instrument_id = 22;
    fields.bus_seq = ++bus_sequence;
    fields.source_seq = 1;
    std::array<std::byte, sizeof(BboRecord)> noise{};
    assert(EncodeBbo(noise, fields, {20'000, 1}, {20'100, 1}));
    assert(ingest.consume(++ring_sequence,
                          static_cast<std::uint32_t>(MessageType::Bbo),
                          noise, 11'000) ==
           mds::agg::IngestResult::Ignored);
  }

  fields.instrument_id = 11;
  fields.bus_seq = ++bus_sequence;
  fields.source_seq = 2;
  std::array<std::byte, sizeof(SnapshotBeginRecord)> begin{};
  assert(EncodeSnapshotBegin(begin, fields, 4, 2));
  assert(ingest.consume(++ring_sequence,
                        static_cast<std::uint32_t>(MessageType::SnapshotBegin),
                        begin, 20'000) ==
         mds::agg::IngestResult::Ignored);
  const std::array<Level, 2> bids{{{10'000, 10}, {9'900, 30}}};
  const std::array<Level, 2> asks{{{10'100, 20}, {10'200, 40}}};
  std::array<std::byte, sizeof(SnapshotChunkRecord)> chunk{};
  fields.bus_seq = ++bus_sequence;
  assert(EncodeSnapshotChunk(chunk, fields, 0, Side::Bid, bids));
  assert(ingest.consume(++ring_sequence,
                        static_cast<std::uint32_t>(MessageType::SnapshotChunk),
                        chunk, 21'000) ==
         mds::agg::IngestResult::Ignored);
  fields.bus_seq = ++bus_sequence;
  assert(EncodeSnapshotChunk(chunk, fields, 1, Side::Ask, asks));
  assert(ingest.consume(++ring_sequence,
                        static_cast<std::uint32_t>(MessageType::SnapshotChunk),
                        chunk, 22'000) ==
         mds::agg::IngestResult::Ignored);
  std::array<std::byte, sizeof(SnapshotEndRecord)> end{};
  fields.bus_seq = ++bus_sequence;
  assert(EncodeSnapshotEnd(end, fields, 4, 0));
  assert(ingest.consume(++ring_sequence,
                        static_cast<std::uint32_t>(MessageType::SnapshotEnd),
                        end, 23'000) ==
         mds::agg::IngestResult::Book);

  AggregationEngine engine(config());
  assert(engine.add_member({Venue::Binance, 1'000'000, 2, 2, false}));
  assert(engine.update_bbo(0, *ingest.bbo(), ingest.bbo_ingress_ns()));
  mds::agg::BookInput book{};
  assert(ingest.book_input(book));
  assert(engine.update_book(0, book));
  return {engine.build_bbo(100'000).record,
          engine.build_orderbook(100'000).record};
}

void normalize_transport_fields(utils::md::wire::RecordHeader &header) {
  header.bus_seq = 0;
  header.receive_tsc = 0;
  header.publish_tsc = 0;
}

void test_per_symbol_and_multiplex_aggregate_parity() {
  auto per_symbol = aggregate_from_layout(false);
  auto multiplex = aggregate_from_layout(true);
  normalize_transport_fields(per_symbol.bbo.header);
  normalize_transport_fields(multiplex.bbo.header);
  normalize_transport_fields(per_symbol.book.header);
  normalize_transport_fields(multiplex.book.header);
  assert(std::memcmp(&per_symbol.bbo, &multiplex.bbo,
                     sizeof(per_symbol.bbo)) == 0);
  assert(std::memcmp(&per_symbol.book, &multiplex.book,
                     sizeof(per_symbol.book)) == 0);
}

}  // namespace

int main() {
  test_raw_liveness_formula();
  test_ttl_raw_and_expiry_dedup();
  test_timer_boundaries_preserve_raw_superset();
  test_time_only_fields_do_not_publish();
  test_cross_skew_observe_and_enforce();
  test_cross_skew_evidence_iteration_and_recovery();
  test_cross_skew_seven_round_bound();
  test_timestamp_venue_differs_from_best_venue();
  test_same_member_cross_is_reported();
  test_fx_and_orderbook_merge();
  test_fixed_capacity_exact_price_k_way_merge();
  test_mixed_scale_exact_price_identity();
  test_eight_members_fill_eighty_levels();
  test_orderbook_active_mask_and_legal_cross();
  test_member_rejoins_after_new_generation();
  test_five_members_degrade_and_generation_rejoin();
  test_slot_identity_metadata_and_fx_expiry();
  test_member_metadata_reconfiguration_keeps_scales_frozen();
  test_agg_bbo_abi_and_codec_flags();
  test_timestamp_slot_and_hot_path_allocations();
  test_venue_ingest_bbo_and_sequence_gap();
  test_venue_ingest_generation_barrier_and_stale_rejection();
  test_venue_ingest_refines_flagged_snapshot_tick();
  test_per_symbol_and_multiplex_aggregate_parity();
  return 0;
}
