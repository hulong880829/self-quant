#pragma once

#include <cstdint>
#include <deque>
#include <optional>
#include <string>
#include <string_view>
#include <unordered_map>
#include <vector>

#include "utils/md/types.h"

namespace utils::md {

struct SymbolParts {
  std::string base;
  std::string quote;
  std::string canonical;
};

class SymbolNormalizer {
 public:
  SymbolNormalizer();
  void AddAssetAlias(std::string alias, std::string canonical);
  [[nodiscard]] std::optional<SymbolParts> Normalize(
      std::string_view venue_symbol, std::string_view quote_hint = {}) const;
  [[nodiscard]] std::string InstrumentKey(Venue venue, ProductType product,
                                          const SymbolParts& parts,
                                          std::string_view contract_spec = {}) const;

 private:
  [[nodiscard]] std::string NormalizeAsset(std::string_view asset) const;
  std::unordered_map<std::string, std::string> aliases_;
  std::vector<std::string> known_quotes_;
};

enum class RegistryResult : std::uint8_t { Ok, Invalid, DuplicateId, DuplicateKey };

struct TransparentStringHash {
  using is_transparent = void;

  [[nodiscard]] std::size_t operator()(std::string_view value) const noexcept {
    return std::hash<std::string_view>{}(value);
  }
};

class InstrumentRegistry {
 public:
  RegistryResult Register(const Instrument& instrument);
  [[nodiscard]] const Instrument* Find(InstrumentId id) const noexcept;
  [[nodiscard]] const Instrument* Find(std::string_view key) const noexcept;
  [[nodiscard]] std::size_t size() const noexcept { return instruments_.size(); }

 private:
  std::deque<Instrument> instruments_;
  std::unordered_map<InstrumentId, std::size_t> by_id_;
  std::unordered_map<std::string, std::size_t, TransparentStringHash, std::equal_to<>> by_key_;
};

}  // namespace utils::md
