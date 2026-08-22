#include "mds/exchange/venue_adapter.h"

#include "mds/exchange/bitget/bitget_adapter.h"
#include "mds/exchange/binance/binance_venue_adapter.h"
#include "mds/exchange/bybit/bybit_adapter.h"
#include "mds/exchange/gate/gate_adapter.h"
#include "mds/exchange/hyperliquid/hyperliquid_adapter.h"
#include "mds/exchange/okx/okx_adapter.h"
#include "mds/exchange/polymarket/polymarket_adapter.h"

#include <cstring>

namespace mds::exchange {

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
