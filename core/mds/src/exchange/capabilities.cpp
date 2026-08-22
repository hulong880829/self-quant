#include "mds/exchange/capabilities.h"

#include <algorithm>
#include <array>
#include <cctype>

namespace mds::exchange {
namespace {

using Bootstrap = BookBootstrap;
using Product = utils::md::ProductType;
using Venue = utils::md::Venue;

constexpr ChannelCapability channel(
    std::string_view name, std::size_t depth, std::uint32_t interval_ms,
    Bootstrap bootstrap, bool login = false,
    bool configurable_interval = false,
    std::size_t max_levels_per_message = 0) noexcept {
  return {name, depth, interval_ms, bootstrap, login, configurable_interval,
          max_levels_per_message == 0 ? depth : max_levels_per_message};
}

constexpr std::array<VenueCapabilities, 13> kCapabilities{{
    {Venue::Binance, Product::Spot,
     channel("bookTicker", 1, 0, Bootstrap::RestSnapshotThenDelta),
     channel("depth", 5000, 100, Bootstrap::RestSnapshotThenDelta, false,
             true)},
    {Venue::Binance, Product::Perpetual,
     channel("bookTicker", 1, 0, Bootstrap::RestSnapshotThenDelta),
     channel("depth", 1000, 100, Bootstrap::RestSnapshotThenDelta, false,
             true, 5000)},
    {Venue::Okx, Product::Spot,
     channel("bbo-tbt", 1, 0, Bootstrap::WsSnapshotThenDelta),
     channel("books-l2-tbt", 400, 10, Bootstrap::WsSnapshotThenDelta, true,
             false, 1024)},
    {Venue::Okx, Product::Perpetual,
     channel("bbo-tbt", 1, 0, Bootstrap::WsSnapshotThenDelta),
     channel("books-l2-tbt", 400, 10, Bootstrap::WsSnapshotThenDelta, true,
             false, 1024)},
    {Venue::Bybit, Product::Spot,
     channel("orderbook.1", 1, 10, Bootstrap::WsSnapshotThenDelta),
     channel("orderbook.50", 50, 20, Bootstrap::WsSnapshotThenDelta)},
    {Venue::Bybit, Product::Perpetual,
     channel("orderbook.1", 1, 10, Bootstrap::WsSnapshotThenDelta),
     channel("orderbook.50", 50, 20, Bootstrap::WsSnapshotThenDelta)},
    {Venue::Bitget, Product::Spot,
     channel("books1", 1, 10, Bootstrap::WsSnapshotThenDelta),
     channel("books", 1000, 200, Bootstrap::WsSnapshotThenDelta)},
    {Venue::Bitget, Product::Perpetual,
     channel("books1", 1, 10, Bootstrap::WsSnapshotThenDelta),
     channel("books", 1000, 150, Bootstrap::WsSnapshotThenDelta)},
    {Venue::Gate, Product::Spot,
     channel("spot.book_ticker", 1, 10, Bootstrap::RestSnapshotThenDelta),
     channel("spot.order_book_update", 20, 20,
             Bootstrap::RestSnapshotThenDelta, false, true, 1024)},
    {Venue::Gate, Product::Perpetual,
     channel("futures.book_ticker", 1, 10,
             Bootstrap::RestSnapshotThenDelta),
     channel("futures.order_book_update", 20, 20,
             Bootstrap::RestSnapshotThenDelta, false, true, 1024)},
    {Venue::Hyperliquid, Product::Spot,
     channel("bbo", 1, 0, Bootstrap::WsImageOnly),
     channel("l2Book", 20, 500, Bootstrap::WsImageOnly)},
    {Venue::Hyperliquid, Product::Perpetual,
     channel("bbo", 1, 0, Bootstrap::WsImageOnly),
     channel("l2Book", 20, 500, Bootstrap::WsImageOnly)},
    {Venue::Polymarket, Product::BinaryOption,
     channel("best_bid_ask", 1, 0, Bootstrap::WsSnapshotThenDelta),
     channel("market", 500, 0, Bootstrap::WsSnapshotThenDelta, false, false,
             1000)},
}};

bool iequals(std::string_view left, std::string_view right) noexcept {
  if (left.size() != right.size()) {
    return false;
  }
  for (std::size_t index = 0; index < left.size(); ++index) {
    if (std::tolower(static_cast<unsigned char>(left[index])) !=
        std::tolower(static_cast<unsigned char>(right[index]))) {
      return false;
    }
  }
  return true;
}

std::optional<ChannelCapability>
explicit_channel(Venue venue, Product product,
                 std::string_view requested) noexcept {
  const auto *base = capabilities(venue, product);
  if (base == nullptr) {
    return std::nullopt;
  }
  if (iequals(requested, base->fastest_top10.channel)) {
    return base->fastest_top10;
  }
  if (venue == Venue::Okx && iequals(requested, "books")) {
    return channel("books", 400, 100, Bootstrap::WsSnapshotThenDelta, false,
                   false, 1024);
  }
  if (venue == Venue::Bybit) {
    if (iequals(requested, "orderbook.50")) {
      return channel("orderbook.50", 50, 20,
                     Bootstrap::WsSnapshotThenDelta);
    }
    if (iequals(requested, "orderbook.200")) {
      return channel("orderbook.200", 200, 100,
                     Bootstrap::WsSnapshotThenDelta);
    }
    if (iequals(requested, "orderbook.1000")) {
      return channel("orderbook.1000", 1000, 200,
                     Bootstrap::WsSnapshotThenDelta);
    }
  }
  if (venue == Venue::Bitget &&
      (iequals(requested, "books15") || iequals(requested, "books"))) {
    return iequals(requested, "books15")
               ? channel("books15", 15,
                         product == Product::Spot ? 200 : 150,
                         Bootstrap::WsImageOnly)
               : base->fastest_top10;
  }
  if (venue == Venue::Gate &&
      iequals(requested, base->fastest_top10.channel)) {
    return base->fastest_top10;
  }
  if (venue == Venue::Hyperliquid &&
      iequals(requested, "l2Book")) {
    return base->fastest_top10;
  }
  if (venue == Venue::Binance &&
      (iequals(requested, "depth") ||
       iequals(requested, "fastest_top10"))) {
    return base->fastest_top10;
  }
  return std::nullopt;
}

bool valid_interval(Venue venue, Product product,
                    std::uint32_t interval) noexcept {
  if (venue == Venue::Binance) {
    return product == Product::Spot
               ? interval == 100 || interval == 1000
               : interval == 100 || interval == 250 || interval == 500;
  }
  if (venue == Venue::Gate) {
    return interval == 20 || interval == 100;
  }
  return false;
}

}  // namespace

std::string_view venue_name(Venue venue) noexcept {
  switch (venue) {
  case Venue::Binance:
    return "binance";
  case Venue::Okx:
    return "okx";
  case Venue::Bybit:
    return "bybit";
  case Venue::Gate:
    return "gate";
  case Venue::Bitget:
    return "bitget";
  case Venue::Hyperliquid:
    return "hyperliquid";
  case Venue::Polymarket:
    return "polymarket";
  default:
    return "unknown";
  }
}

std::string_view product_name(Product product) noexcept {
  switch (product) {
  case Product::Spot:
    return "spot";
  case Product::Perpetual:
    return "perpetual";
  case Product::Future:
    return "future";
  case Product::BinaryOption:
    return "binary-option";
  default:
    return "unknown";
  }
}

std::string segment_profile(Venue venue, Product product) {
  if (venue == Venue::Polymarket &&
      product == Product::BinaryOption) {
    return "polymarket_binary_option";
  }
  if (venue == Venue::Binance) {
    return product == Product::Spot ? "spot" : "usdm";
  }
  std::string result(venue_name(venue));
  result.push_back('_');
  result.append(product == Product::Perpetual ? "perp" : product_name(product));
  return result;
}

std::optional<Venue> parse_venue(std::string_view value) noexcept {
  for (const auto venue : {Venue::Binance, Venue::Okx, Venue::Bybit,
                           Venue::Bitget, Venue::Gate,
                           Venue::Hyperliquid, Venue::Polymarket}) {
    if (iequals(value, venue_name(venue))) {
      return venue;
    }
  }
  return std::nullopt;
}

std::optional<Product> parse_product(std::string_view value) noexcept {
  if (iequals(value, "spot")) {
    return Product::Spot;
  }
  if (iequals(value, "perpetual") || iequals(value, "perp") ||
      iequals(value, "swap") || iequals(value, "linear") ||
      iequals(value, "usdm") || iequals(value, "usd-m") ||
      iequals(value, "usdt-futures")) {
    return Product::Perpetual;
  }
  if (iequals(value, "binary-option") ||
      iequals(value, "binary_option") ||
      iequals(value, "binaryoption")) {
    return Product::BinaryOption;
  }
  return std::nullopt;
}

const VenueCapabilities *capabilities(Venue venue, Product product) noexcept {
  const auto found =
      std::find_if(kCapabilities.begin(), kCapabilities.end(),
                   [=](const VenueCapabilities &entry) {
                     return entry.venue == venue && entry.product == product;
                   });
  return found == kCapabilities.end() ? nullptr : &*found;
}

bool resolve_orderbook_channel(
    Venue venue, Product product, std::string_view requested_channel,
    std::optional<std::uint32_t> requested_interval_ms,
    ResolvedOrderBookChannel &resolved, std::string &error) {
  const auto *venue_capabilities = capabilities(venue, product);
  if (venue_capabilities == nullptr) {
    error = "unsupported venue/product combination";
    return false;
  }
  resolved.explicit_override = !requested_channel.empty();
  if (requested_channel.empty()) {
    resolved.capability = venue_capabilities->fastest_top10;
  } else {
    const auto selected =
        explicit_channel(venue, product, requested_channel);
    if (!selected) {
      error = "unsupported orderbook_channel '" +
              std::string(requested_channel) + "' for " +
              std::string(venue_name(venue)) + '/' +
              std::string(product_name(product));
      return false;
    }
    resolved.capability = *selected;
  }
  if (resolved.capability.depth_per_side < 10) {
    error = "orderbook_channel must provide at least 10 levels per side";
    return false;
  }
  if (requested_interval_ms) {
    if (!resolved.capability.configurable_interval) {
      error = "update_interval_ms is not supported by orderbook_channel '" +
              std::string(resolved.capability.channel) + "'";
      return false;
    }
    if (!valid_interval(venue, product, *requested_interval_ms)) {
      error = "unsupported update_interval_ms for " +
              std::string(venue_name(venue)) + '/' +
              std::string(product_name(product));
      return false;
    }
    resolved.capability.interval_ms = *requested_interval_ms;
    if (venue == Venue::Gate && product == Product::Spot &&
        *requested_interval_ms == 100) {
      resolved.capability.depth_per_side = 100;
    }
  }
  return true;
}

}  // namespace mds::exchange
