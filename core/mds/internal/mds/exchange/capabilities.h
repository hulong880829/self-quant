#pragma once

#include "utils/md/types.h"

#include <cstddef>
#include <cstdint>
#include <optional>
#include <string>
#include <string_view>

namespace mds::exchange {

enum class BookBootstrap : std::uint8_t {
  RestSnapshotThenDelta,
  WsSnapshotThenDelta,
  WsImageOnly,
};

struct ChannelCapability {
  std::string_view channel;
  std::size_t depth_per_side{};
  std::uint32_t interval_ms{};
  BookBootstrap bootstrap{BookBootstrap::WsSnapshotThenDelta};
  bool requires_public_ws_login{};
  bool configurable_interval{};
  std::size_t max_levels_per_message{};
  bool requires_first_data_before_ready{};
};

struct VenueCapabilities {
  utils::md::Venue venue{utils::md::Venue::Unknown};
  utils::md::ProductType product{utils::md::ProductType::Unknown};
  ChannelCapability ticker;
  ChannelCapability fastest_top10;
};

struct ResolvedOrderBookChannel {
  ChannelCapability capability;
  bool explicit_override{};
};

[[nodiscard]] std::string_view
venue_name(utils::md::Venue venue) noexcept;
[[nodiscard]] std::string_view
product_name(utils::md::ProductType product) noexcept;
[[nodiscard]] std::string
segment_profile(utils::md::Venue venue,
                utils::md::ProductType product);

[[nodiscard]] std::optional<utils::md::Venue>
parse_venue(std::string_view value) noexcept;
[[nodiscard]] std::optional<utils::md::ProductType>
parse_product(std::string_view value) noexcept;

[[nodiscard]] const VenueCapabilities *
capabilities(utils::md::Venue venue,
             utils::md::ProductType product) noexcept;

// An empty channel resolves to the venue's fastest channel that carries at
// least ten levels per side. Explicit overrides are limited to known public
// channels that also satisfy the top-10 contract.
[[nodiscard]] bool resolve_orderbook_channel(
    utils::md::Venue venue, utils::md::ProductType product,
    std::string_view requested_channel,
    std::optional<std::uint32_t> requested_interval_ms,
    ResolvedOrderBookChannel &resolved, std::string &error);

}  // namespace mds::exchange
