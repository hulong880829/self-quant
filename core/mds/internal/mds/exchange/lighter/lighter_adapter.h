#pragma once

#include "mds/exchange/venue_adapter.h"

#include <cstddef>
#include <memory>

namespace mds::exchange {

namespace lighter {
inline constexpr std::size_t kMaximumConnectionsPerIp = 255;
inline constexpr std::size_t kMaximumNewConnectionsPerMinute = 255;
inline constexpr std::size_t kMaximumSubscriptionsPerConnection = 500;
inline constexpr std::size_t kMaximumInflightMessages = 50;
}  // namespace lighter

[[nodiscard]] std::unique_ptr<VenueAdapter>
make_lighter_adapter(utils::md::ProductType product,
                     std::size_t max_levels_per_side = 5000);

}  // namespace mds::exchange
