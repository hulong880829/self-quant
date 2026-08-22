#pragma once

#include "mds/exchange/venue_adapter.h"

#include <cstddef>
#include <memory>

namespace mds::exchange::bybit {

[[nodiscard]] std::unique_ptr<VenueAdapter>
make_bybit_adapter(utils::md::ProductType product,
                   std::size_t max_levels_per_side = 1000);

}  // namespace mds::exchange::bybit
