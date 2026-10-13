#pragma once

#include "utils/md/types.h"

namespace mds::record {

// Shared by consumer config parsing and ClickHouseBboRecorder::start so the
// legal venue/product matrix cannot drift between the two call sites.
[[nodiscard]] inline bool is_clickhouse_bbo_supported(
    utils::md::Venue venue, utils::md::ProductType product) noexcept {
  using Venue = utils::md::Venue;
  using Product = utils::md::ProductType;
  if (venue == Venue::Ibkr && product == Product::Equity) {
    return true;
  }
  const bool crypto_venue =
      venue == Venue::Binance || venue == Venue::Okx || venue == Venue::Bybit ||
      venue == Venue::Gate || venue == Venue::Bitget ||
      venue == Venue::Hyperliquid || venue == Venue::Aster ||
      venue == Venue::Lighter;
  return crypto_venue &&
         (product == Product::Spot || product == Product::Perpetual);
}

}  // namespace mds::record
