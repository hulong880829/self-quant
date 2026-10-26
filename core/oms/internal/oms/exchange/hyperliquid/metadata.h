#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>
#include <unordered_map>
#include <unordered_set>

namespace oms::exchange::hyperliquid {

struct AssetId {
  std::int64_t value{};
  bool spot{};
  std::uint8_t size_decimals{};
};

// Perp asset ids come from the dex meta index. HIP-3 ids are
// 100000 + dex_index * 10000 + coin_index. Spot ids are 10000 + index.
class AssetTable {
 public:
  // dex_name is the HIP-3 prefix, such as the dex that publishes "xyz:GME".
  // An empty name stores only the coin. The dex name is never assumed.
  [[nodiscard]] bool add_perp(
      std::string_view coin, std::int64_t coin_index,
      std::int64_t dex_index = 0, std::string_view dex_name = {},
      std::uint8_t size_decimals = 0);
  void add_spot(std::string_view pair, std::int64_t index);

  [[nodiscard]] bool find(std::string_view name, AssetId &out) const;
  [[nodiscard]] std::size_t size() const noexcept { return assets_.size(); }

 private:
  [[nodiscard]] bool insert(std::string name, AssetId id);

  std::unordered_map<std::string, AssetId> assets_;
  std::unordered_set<std::string> ambiguous_;
};

}  // namespace oms::exchange::hyperliquid
