#include "oms/exchange/hyperliquid/metadata.h"

namespace oms::exchange::hyperliquid {

bool AssetTable::insert(std::string name, AssetId id) {
  if (ambiguous_.contains(name)) return false;
  const auto found = assets_.find(name);
  if (found == assets_.end()) {
    assets_.emplace(std::move(name), id);
    return true;
  }
  if (found->second.value == id.value && found->second.spot == id.spot &&
      found->second.size_decimals == id.size_decimals)
    return true;
  assets_.erase(found);
  ambiguous_.insert(std::move(name));
  return false;
}

bool AssetTable::add_perp(std::string_view coin, std::int64_t coin_index,
                          std::int64_t dex_index, std::string_view dex_name,
                          std::uint8_t size_decimals) {
  AssetId id;
  id.spot = false;
  id.size_decimals = size_decimals;
  id.value = dex_index == 0 ? coin_index
                            : 100000 + dex_index * 10000 + coin_index;
  bool unique = insert(std::string(coin), id);
  if (!dex_name.empty() && coin.find(':') == std::string_view::npos) {
    unique = insert(std::string(dex_name) + ":" + std::string(coin), id) &&
             unique;
  }
  return unique;
}

void AssetTable::add_spot(std::string_view pair, std::int64_t index) {
  AssetId id;
  id.spot = true;
  id.value = 10000 + index;
  (void)insert(std::string(pair), id);
}

bool AssetTable::find(std::string_view name, AssetId &out) const {
  if (ambiguous_.contains(std::string(name))) return false;
  const auto found = assets_.find(std::string(name));
  if (found == assets_.end()) return false;
  out = found->second;
  return true;
}

}  // namespace oms::exchange::hyperliquid
