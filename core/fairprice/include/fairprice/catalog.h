#pragma once

#include "mds/api/mds_api.h"

#include <array>
#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

namespace fairprice {

class MarketDataRepository;

enum class Kind : std::uint8_t { Unknown = 0, Bbo = 1, Book = 2, Reset = 3 };

struct CatalogMarket {
  std::string profile;
  std::string symbol;
  std::string bbo_segment;
  std::string book_segment;
  bool recording{};
};

struct CatalogSnapshot {
  std::string state;
  std::array<unsigned char, 32> hash{};
  std::uint64_t sample_interval_ms{};
  std::uint32_t retention_hours{};
  std::uint32_t depth{};
  std::vector<CatalogMarket> markets;
};

struct CatalogSettings {
  std::uint64_t sample_interval_ms{};
  std::uint32_t retention_hours{24};
  std::uint32_t depth{};
};

class Catalog {
 public:
  static mds::api::Result<Catalog> open(MarketDataRepository *repository,
                                        CatalogSettings settings);
  mds::api::Result<bool> refresh();
  mds::api::Result<bool> wait_for_change(std::uint64_t timeout_ms);
  [[nodiscard]] const CatalogSnapshot *snapshot() const noexcept {
    return loaded_ ? &current_ : nullptr;
  }

 private:
  MarketDataRepository *repository_{};
  CatalogSettings settings_{};
  CatalogSnapshot current_{};
  bool loaded_{};
};

std::string market_key(std::string_view profile, std::string_view symbol);
std::string canonical_symbol(std::string_view value);
bool parse_segment(std::string_view segment, std::string &profile,
                   std::string &symbol, Kind &kind, std::string &error);

}  // namespace fairprice
