#pragma once

#include "utils/md/types.h"

#include <span>
#include <string>
#include <string_view>

namespace mds::consume {

[[nodiscard]] std::string_view venue_name(utils::md::Venue venue) noexcept;
[[nodiscard]] std::string_view
product_name(utils::md::ProductType product) noexcept;

// Returns agg_<product>_<quote>_<sorted-unique-venues>, or an empty string
// when any component is invalid.
[[nodiscard]] std::string
make_aggregate_profile(utils::md::ProductType product,
                       std::string_view quote_asset,
                       std::span<const std::string_view> venues);

[[nodiscard]] std::string
make_aggregate_segment_name(std::string_view prefix,
                            utils::md::ProductType product,
                            std::string_view quote_asset,
                            std::span<const std::string_view> venues,
                            std::string_view symbol,
                            std::string_view stream);

}  // namespace mds::consume
