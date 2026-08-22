#include "mds/exchange/capabilities.h"

#include <cassert>
#include <optional>
#include <string>

int main() {
  using mds::exchange::ResolvedOrderBookChannel;
  using utils::md::ProductType;
  using utils::md::Venue;

  std::string error;
  ResolvedOrderBookChannel resolved;

  assert(mds::exchange::resolve_orderbook_channel(
      Venue::Binance, ProductType::Perpetual, "", std::nullopt,
      resolved, error));
  assert(resolved.capability.channel == "depth");
  assert(resolved.capability.depth_per_side == 1000);
  assert(resolved.capability.max_levels_per_message == 5000);
  assert(resolved.capability.interval_ms == 100);

  assert(mds::exchange::resolve_orderbook_channel(
      Venue::Okx, ProductType::Spot, "", std::nullopt, resolved, error));
  assert(resolved.capability.channel == "books-l2-tbt");
  assert(resolved.capability.interval_ms == 10);
  assert(resolved.capability.depth_per_side == 400);
  assert(resolved.capability.max_levels_per_message == 1024);
  assert(resolved.capability.requires_public_ws_login);
  assert(!resolved.explicit_override);

  assert(mds::exchange::resolve_orderbook_channel(
      Venue::Okx, ProductType::Spot, "books", std::nullopt, resolved, error));
  assert(resolved.capability.channel == "books");
  assert(resolved.capability.interval_ms == 100);
  assert(resolved.capability.depth_per_side == 400);
  assert(resolved.capability.max_levels_per_message == 1024);
  assert(!resolved.capability.requires_public_ws_login);
  assert(resolved.explicit_override);

  error.clear();
  assert(!mds::exchange::resolve_orderbook_channel(
      Venue::Okx, ProductType::Spot, "books", 100, resolved, error));
  assert(error.find("not supported") != std::string::npos);

  assert(mds::exchange::resolve_orderbook_channel(
      Venue::Bybit, ProductType::Perpetual, "", std::nullopt, resolved,
      error));
  assert(resolved.capability.channel == "orderbook.50");
  assert(resolved.capability.interval_ms == 20);
  assert(resolved.capability.max_levels_per_message == 50);

  error.clear();
  assert(!mds::exchange::resolve_orderbook_channel(
      Venue::Bybit, ProductType::Perpetual, "orderbook.1", std::nullopt,
      resolved, error));

  assert(mds::exchange::resolve_orderbook_channel(
      Venue::Gate, ProductType::Spot, "", 20, resolved, error));
  assert(resolved.capability.channel == "spot.order_book_update");
  assert(resolved.capability.interval_ms == 20);
  assert(resolved.capability.depth_per_side == 20);
  assert(resolved.capability.max_levels_per_message == 1024);
  assert(mds::exchange::resolve_orderbook_channel(
      Venue::Gate, ProductType::Spot, "", 100, resolved, error));
  assert(resolved.capability.depth_per_side == 100);
  assert(resolved.capability.max_levels_per_message == 1024);
  assert(mds::exchange::resolve_orderbook_channel(
      Venue::Gate, ProductType::Perpetual, "", 20, resolved, error));
  assert(resolved.capability.depth_per_side == 20);
  assert(resolved.capability.max_levels_per_message == 1024);

  assert(mds::exchange::resolve_orderbook_channel(
      Venue::Bitget, ProductType::Perpetual, "books15", std::nullopt,
      resolved, error));
  assert(resolved.capability.bootstrap ==
         mds::exchange::BookBootstrap::WsImageOnly);
  assert(!mds::exchange::resolve_orderbook_channel(
      Venue::Hyperliquid, ProductType::Perpetual, "l2Book-slow",
      std::nullopt, resolved, error));

  for (const auto venue :
       {Venue::Okx, Venue::Bybit, Venue::Bitget, Venue::Gate}) {
    assert(mds::exchange::capabilities(venue, ProductType::Spot));
    assert(mds::exchange::capabilities(venue,
                                       ProductType::Perpetual));
  }
  assert(mds::exchange::capabilities(Venue::Hyperliquid,
                                     ProductType::Spot));
  assert(mds::exchange::capabilities(Venue::Hyperliquid,
                                     ProductType::Perpetual));
  assert(mds::exchange::capabilities(Venue::Polymarket,
                                     ProductType::BinaryOption));
  assert(mds::exchange::parse_venue("polymarket") ==
         Venue::Polymarket);
  assert(mds::exchange::parse_product("BINARY_OPTION") ==
         ProductType::BinaryOption);
  assert(mds::exchange::resolve_orderbook_channel(
      Venue::Polymarket, ProductType::BinaryOption, "", std::nullopt,
      resolved, error));
  assert(resolved.capability.channel == "market");
  assert(resolved.capability.bootstrap ==
         mds::exchange::BookBootstrap::WsSnapshotThenDelta);

  assert(mds::exchange::segment_profile(Venue::Binance,
                                        ProductType::Perpetual) == "usdm");
  assert(mds::exchange::segment_profile(Venue::Okx, ProductType::Spot) ==
         "okx_spot");
  assert(mds::exchange::segment_profile(Venue::Hyperliquid,
                                        ProductType::Perpetual) ==
         "hyperliquid_perp");
  assert(mds::exchange::segment_profile(Venue::Polymarket,
                                        ProductType::BinaryOption) ==
         "polymarket_binary_option");
  return 0;
}
