#include "aggregate_dashboard.h"

#include "utils/md/types.h"

#include <algorithm>
#include <cassert>
#include <string>
#include <string_view>

namespace {

template <std::size_t Size>
void copy_text(std::array<char, Size> &output, std::string_view value) {
  std::copy(value.begin(), value.end(), output.begin());
}

}  // namespace

int main() {
  namespace md = utils::md;
  namespace wire = utils::md::wire;

  wire::AggBboRecord bbo{};
  copy_text(bbo.base_asset, "BTC");
  copy_text(bbo.quote_asset, "USDT");
  bbo.price_scale = 1;
  bbo.quantity_scale = 3;
  bbo.member_count = 3;
  bbo.member_mask = 7;
  bbo.live_mask = 5;
  bbo.venue_slot_ids[0] = static_cast<std::uint8_t>(md::Venue::Binance);
  bbo.venue_slot_ids[1] = static_cast<std::uint8_t>(md::Venue::Okx);
  bbo.venue_slot_ids[2] = static_cast<std::uint8_t>(md::Venue::Gate);
  bbo.header.book_generation = 11;
  bbo.header.bus_seq = 90;
  bbo.gated_bid.price = 652'050;
  bbo.gated_bid.quantity = 2'000;
  bbo.gated_bid.venue_mask = 1;
  bbo.gated_bid.venue_quantity[0] = 2'000;
  bbo.gated_ask.price = 652'060;
  bbo.gated_ask.quantity = 3'000;
  bbo.gated_ask.venue_mask = 4;
  bbo.gated_ask.venue_quantity[2] = 3'000;
  bbo.raw_bid = {652'050, 2'000, 1, 0, {}};
  bbo.raw_ask = {652'060, 3'000, 4, 2, {}};
  bbo.cross_skew_threshold_us = 50'000;

  wire::AggOrderBookRecord book{};
  copy_text(book.base_asset, "BTC");
  copy_text(book.quote_asset, "USDT");
  book.price_scale = 1;
  book.quantity_scale = 3;
  book.member_count = 3;
  book.member_mask = 7;
  book.active_mask = 5;
  book.venue_slot_ids = bbo.venue_slot_ids;
  book.header.book_generation = 12;
  book.header.bus_seq = 91;
  book.bid_count = 2;
  book.ask_count = 2;
  book.bids[0].price = 652'050;
  book.bids[0].quantity = 3'000;
  book.bids[0].venue_mask = 5;
  book.bids[0].venue_quantity[0] = 1'000;
  book.bids[0].venue_quantity[2] = 2'000;
  book.bids[1].price = 652'040;
  book.bids[1].quantity = 2'000;
  book.bids[1].venue_mask = 1;
  book.bids[1].venue_quantity[0] = 2'000;
  book.asks[0].price = 652'060;
  book.asks[0].quantity = 3'000;
  book.asks[0].venue_mask = 4;
  book.asks[0].venue_quantity[2] = 3'000;
  book.asks[1].price = 652'070;
  book.asks[1].quantity = 1'500;
  book.asks[1].venue_mask = 1;
  book.asks[1].venue_quantity[0] = 1'500;

  const mds::examples::AggregateDashboardView view{
      &bbo,
      &book,
      "/selfquant.mds.agg_perp_usdt_binance-gate-okx.btcusdt.aggbbo.2",
      "/selfquant.mds.agg_perp_usdt_binance-gate-okx.btcusdt."
      "aggorderbook.2",
      100,
      101,
      2,
      120,
      false};
  const auto rendered = mds::examples::render_aggregate_dashboard(view);
  assert(rendered.find("BTC/USDT PERP    BBO LIVE 2/3    BOOK ACTIVE 2/3") !=
         std::string::npos);
  assert(rendered.find("RAW BBO") != std::string::npos);
  assert(rendered.find("GATED BBO") != std::string::npos);
  assert(rendered.find("Binance 1.000 | Gate 2.000") != std::string::npos);
  assert(rendered.find("bid={Binance 2.000}  ask={Gate 3.000}") !=
         std::string::npos);
  assert(rendered.find("OKX STALE") != std::string::npos);
  assert(rendered.find("ring_seq=100") != std::string::npos);
  assert(rendered.find("BOOK ring_seq=101") != std::string::npos);
  const auto far_ask = rendered.find("65207.0");
  const auto best_ask = rendered.find("65206.0", far_ask + 1);
  const auto best_bid = rendered.find("65205.0", best_ask + 1);
  assert(far_ask < best_ask);
  assert(best_ask < best_bid);

  bbo.gated_bid.price = 652'070;
  const auto crossed = mds::examples::render_aggregate_dashboard(view);
  assert(crossed.find("CROSSED") != std::string::npos);

  auto narrow_view = view;
  narrow_view.terminal_width = 72;
  const auto narrow = mds::examples::render_aggregate_dashboard(narrow_view);
  assert(narrow.find("BN 1.000 | Gate 2.000") != std::string::npos);

  auto book_only_view = view;
  book_only_view.bbo = nullptr;
  book_only_view.expect_bbo = false;
  const auto book_only =
      mds::examples::render_aggregate_dashboard(book_only_view);
  assert(book_only.find("Waiting for AggBbo") == std::string::npos);
  assert(book_only.find("ASK - SELL") != std::string::npos);

  const mds::examples::AggregateDashboardView waiting{};
  assert(mds::examples::render_aggregate_dashboard(waiting) ==
         "Waiting for aggregate records...\n");
  return 0;
}
