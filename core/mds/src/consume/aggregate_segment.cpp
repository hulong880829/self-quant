#include "mds/consume/aggregate_segment.h"

#include "mds/publish/wire_publisher.h"

#include <algorithm>
#include <cctype>
#include <vector>

namespace mds::consume {
namespace {

std::string canonical_part(std::string_view value) {
  std::string result;
  result.reserve(value.size());
  for (const char raw : value) {
    const auto character = static_cast<unsigned char>(raw);
    if (std::isalnum(character) != 0) {
      result.push_back(static_cast<char>(std::tolower(character)));
    } else if (character == '-' || character == '_') {
      result.push_back(static_cast<char>(character));
    } else {
      return {};
    }
  }
  return result;
}

}  // namespace

std::string_view venue_name(utils::md::Venue venue) noexcept {
  switch (venue) {
    case utils::md::Venue::Binance:
      return "binance";
    case utils::md::Venue::Okx:
      return "okx";
    case utils::md::Venue::Bybit:
      return "bybit";
    case utils::md::Venue::Gate:
      return "gate";
    case utils::md::Venue::Bitget:
      return "bitget";
    case utils::md::Venue::Polymarket:
      return "polymarket";
    case utils::md::Venue::Sse:
      return "sse";
    case utils::md::Venue::Hyperliquid:
      return "hyperliquid";
    case utils::md::Venue::Unknown:
      return "unknown";
  }
  return "unknown";
}

std::string_view product_name(utils::md::ProductType product) noexcept {
  switch (product) {
    case utils::md::ProductType::Spot:
      return "spot";
    case utils::md::ProductType::Perpetual:
      return "perp";
    case utils::md::ProductType::Future:
      return "future";
    case utils::md::ProductType::BinaryOption:
      return "binary-option";
    case utils::md::ProductType::Equity:
      return "equity";
    case utils::md::ProductType::Unknown:
      return {};
  }
  return {};
}

std::string make_aggregate_profile(
    utils::md::ProductType product, std::string_view quote_asset,
    std::span<const std::string_view> venues) {
  const auto product_part = product_name(product);
  const auto quote_part = canonical_part(quote_asset);
  if (product_part.empty() || quote_part.empty() || venues.empty()) {
    return {};
  }
  std::vector<std::string> venue_parts;
  venue_parts.reserve(venues.size());
  for (const auto venue : venues) {
    auto canonical = canonical_part(venue);
    if (canonical.empty()) {
      return {};
    }
    venue_parts.push_back(std::move(canonical));
  }
  std::sort(venue_parts.begin(), venue_parts.end());
  venue_parts.erase(std::unique(venue_parts.begin(), venue_parts.end()),
                    venue_parts.end());

  std::string profile = "agg_";
  profile += product_part;
  profile += '_';
  profile += quote_part;
  profile += '_';
  for (std::size_t index = 0; index < venue_parts.size(); ++index) {
    if (index != 0) {
      profile += '-';
    }
    profile += venue_parts[index];
  }
  return profile;
}

std::string make_aggregate_segment_name(
    std::string_view prefix, utils::md::ProductType product,
    std::string_view quote_asset, std::span<const std::string_view> venues,
    std::string_view symbol, std::string_view stream) {
  const auto profile =
      make_aggregate_profile(product, quote_asset, venues);
  if (profile.empty()) {
    return {};
  }
  return publish::make_publisher_segment_name(prefix, profile, symbol, stream);
}

}  // namespace mds::consume
