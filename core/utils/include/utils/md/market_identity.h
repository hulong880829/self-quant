#pragma once

#include <optional>
#include <string_view>

#include "utils/md/types.h"

namespace utils::md {

[[nodiscard]] inline std::optional<Venue>
parse_canonical_venue(std::string_view value) noexcept {
  if (value == "binance") {
    return Venue::Binance;
  }
  if (value == "okx") {
    return Venue::Okx;
  }
  if (value == "bybit") {
    return Venue::Bybit;
  }
  if (value == "gate") {
    return Venue::Gate;
  }
  if (value == "bitget") {
    return Venue::Bitget;
  }
  if (value == "polymarket") {
    return Venue::Polymarket;
  }
  if (value == "sse") {
    return Venue::Sse;
  }
  if (value == "hyperliquid") {
    return Venue::Hyperliquid;
  }
  if (value == "aster") {
    return Venue::Aster;
  }
  if (value == "lighter") {
    return Venue::Lighter;
  }
  return std::nullopt;
}

[[nodiscard]] inline std::optional<ProductType>
parse_canonical_product(std::string_view value) noexcept {
  if (value == "spot") {
    return ProductType::Spot;
  }
  if (value == "perpetual") {
    return ProductType::Perpetual;
  }
  if (value == "future") {
    return ProductType::Future;
  }
  if (value == "binary_option") {
    return ProductType::BinaryOption;
  }
  if (value == "equity") {
    return ProductType::Equity;
  }
  return std::nullopt;
}

}  // namespace utils::md
