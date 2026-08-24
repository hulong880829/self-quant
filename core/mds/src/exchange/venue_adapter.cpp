#include "mds/exchange/venue_adapter.h"

#include "mds/exchange/bitget/bitget_adapter.h"
#include "mds/exchange/binance/binance_venue_adapter.h"
#include "mds/exchange/bybit/bybit_adapter.h"
#include "mds/exchange/gate/gate_adapter.h"
#include "mds/exchange/hyperliquid/hyperliquid_adapter.h"
#include "mds/exchange/okx/okx_adapter.h"
#include "mds/exchange/polymarket/polymarket_adapter.h"
#include "utils/md/decimal.h"

#include <charconv>
#include <cstring>
#include <limits>

namespace mds::exchange {

bool decimal_to_turnover(std::string_view value,
                         std::uint64_t &turnover) noexcept {
  if (value.empty() || value.front() == '-' || value.front() == '+') {
    return false;
  }
  const auto dot = value.find('.');
  const auto integer = value.substr(0, dot);
  if (integer.empty()) {
    return false;
  }
  if (dot != std::string_view::npos) {
    if (value.find('.', dot + 1) != std::string_view::npos ||
        dot + 1 == value.size()) {
      return false;
    }
    for (const char digit : value.substr(dot + 1)) {
      if (digit < '0' || digit > '9') {
        return false;
      }
    }
  }
  const auto parsed =
      std::from_chars(integer.data(), integer.data() + integer.size(),
                      turnover);
  return parsed.ec == std::errc{} &&
         parsed.ptr == integer.data() + integer.size();
}

bool decimal_product_to_turnover(std::string_view quantity,
                                 std::string_view price,
                                 std::uint64_t &turnover) noexcept {
  std::uint8_t quantity_scale{};
  std::uint8_t price_scale{};
  std::int64_t fixed_quantity{};
  std::int64_t fixed_price{};
  if (!utils::md::decimal_scale(quantity, quantity_scale) ||
      !utils::md::decimal_scale(price, price_scale) ||
      !utils::md::decimal_to_fixed(quantity, quantity_scale,
                                   fixed_quantity) ||
      !utils::md::decimal_to_fixed(price, price_scale, fixed_price) ||
      fixed_quantity < 0 || fixed_price < 0) {
    return false;
  }
  __extension__ typedef __int128 Int128;
  Int128 divisor = 1;
  for (unsigned index = 0;
       index < static_cast<unsigned>(quantity_scale) +
                   static_cast<unsigned>(price_scale);
       ++index) {
    divisor *= 10;
  }
  const auto result =
      static_cast<Int128>(fixed_quantity) * static_cast<Int128>(fixed_price) /
      divisor;
  if (result < 0 ||
      result > static_cast<Int128>(
                   std::numeric_limits<std::uint64_t>::max())) {
    return false;
  }
  turnover = static_cast<std::uint64_t>(result);
  return true;
}

bool copy_symbol(std::string_view value,
                 NormalizedEvent &event) noexcept {
  if (value.empty() || value.size() >= event.symbol.size()) {
    return false;
  }
  std::memcpy(event.symbol.data(), value.data(), value.size());
  event.symbol[value.size()] = '\0';
  event.symbol_size = value.size();
  return true;
}

// The concrete factory is completed by the venue adapter translation units.
// Keeping this weak fallback makes capability/config-only builds usable when
// simdjson adapters are disabled.
std::unique_ptr<VenueAdapter>
make_venue_adapter(utils::md::Venue venue,
                   utils::md::ProductType product,
                   std::size_t max_levels_per_side) {
  switch (venue) {
  case utils::md::Venue::Binance:
    return make_binance_venue_adapter(product, max_levels_per_side);
  case utils::md::Venue::Okx:
    return make_okx_adapter(product, max_levels_per_side);
  case utils::md::Venue::Bybit:
    return bybit::make_bybit_adapter(product, max_levels_per_side);
  case utils::md::Venue::Bitget:
    return make_bitget_adapter(product, max_levels_per_side);
  case utils::md::Venue::Gate:
    return make_gate_adapter(product, max_levels_per_side);
  case utils::md::Venue::Hyperliquid:
    return make_hyperliquid_adapter(product, max_levels_per_side);
  case utils::md::Venue::Polymarket:
    return product == utils::md::ProductType::BinaryOption
               ? polymarket::make_polymarket_adapter(max_levels_per_side)
               : nullptr;
  default:
    return nullptr;
  }
}

}  // namespace mds::exchange
