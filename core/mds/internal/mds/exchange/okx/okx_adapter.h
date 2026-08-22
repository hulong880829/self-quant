#pragma once

#include "mds/exchange/venue_adapter.h"

#include <cstddef>
#include <memory>

namespace mds::exchange {

[[nodiscard]] std::unique_ptr<VenueAdapter>
make_okx_adapter(utils::md::ProductType product,
                 std::size_t max_levels_per_side = 400);

}  // namespace mds::exchange
