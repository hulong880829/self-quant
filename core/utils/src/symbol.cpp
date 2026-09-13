#include "utils/md/symbol.h"

#include <algorithm>
#include <cctype>
#include <cstring>
#include <stdexcept>

namespace utils::md {
namespace {
std::optional<std::string> UpperAlnum(std::string_view value) {
  std::string out;
  out.reserve(value.size());
  for (const char raw : value) {
    const auto c = static_cast<unsigned char>(raw);
    if (c >= 0x80U) return std::nullopt;
    if (c >= 'a' && c <= 'z') {
      out.push_back(static_cast<char>(c - ('a' - 'A')));
    } else if ((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
      out.push_back(static_cast<char>(c));
    }
  }
  return out;
}

std::string FixedString(const std::array<char, 64>& value) {
  const auto end = std::find(value.begin(), value.end(), '\0');
  return {value.begin(), end};
}
}  // namespace

SymbolNormalizer::SymbolNormalizer()
    : aliases_{{"XBT", "BTC"}}, known_quotes_{"USDT", "USDC", "FDUSD", "BUSD", "USD", "BTC", "ETH", "EUR"} {}

void SymbolNormalizer::AddAssetAlias(std::string alias, std::string canonical) {
  auto normalized_alias = UpperAlnum(alias);
  auto normalized_canonical = UpperAlnum(canonical);
  if (!normalized_alias || normalized_alias->empty() ||
      !normalized_canonical || normalized_canonical->empty()) {
    throw std::invalid_argument("asset aliases must be non-empty ASCII");
  }
  aliases_[std::move(*normalized_alias)] = std::move(*normalized_canonical);
}

std::optional<std::string>
SymbolNormalizer::NormalizeAsset(std::string_view asset) const {
  auto normalized = UpperAlnum(asset);
  if (!normalized || normalized->empty()) return std::nullopt;
  if (auto it = aliases_.find(*normalized); it != aliases_.end())
    return it->second;
  return normalized;
}

std::optional<SymbolParts> SymbolNormalizer::Normalize(
    std::string_view venue_symbol, std::string_view quote_hint) const {
  const auto delimiter = venue_symbol.find_first_of("-/_:");
  std::string base;
  std::string quote;
  if (delimiter != std::string_view::npos) {
    auto parsed_base = NormalizeAsset(venue_symbol.substr(0, delimiter));
    auto parsed_quote = NormalizeAsset(venue_symbol.substr(delimiter + 1));
    if (!parsed_base || !parsed_quote) return std::nullopt;
    base = std::move(*parsed_base);
    quote = std::move(*parsed_quote);
  } else {
    auto parsed_joined = UpperAlnum(venue_symbol);
    if (!parsed_joined || parsed_joined->empty()) return std::nullopt;
    const std::string &joined = *parsed_joined;
    if (!quote_hint.empty()) {
      auto parsed_quote = NormalizeAsset(quote_hint);
      if (!parsed_quote) return std::nullopt;
      quote = std::move(*parsed_quote);
      if (joined.size() <= quote.size() ||
          joined.compare(joined.size() - quote.size(), quote.size(), quote) != 0) return std::nullopt;
      auto parsed_base = NormalizeAsset(
          std::string_view(joined).substr(0, joined.size() - quote.size()));
      if (!parsed_base) return std::nullopt;
      base = std::move(*parsed_base);
    } else {
      for (const auto& candidate : known_quotes_) {
        if (joined.size() > candidate.size() &&
            joined.compare(joined.size() - candidate.size(), candidate.size(), candidate) == 0) {
          auto parsed_base = NormalizeAsset(std::string_view(joined).substr(
              0, joined.size() - candidate.size()));
          if (!parsed_base) return std::nullopt;
          base = std::move(*parsed_base);
          quote = candidate;
          break;
        }
      }
    }
  }
  if (base.empty() || quote.empty()) return std::nullopt;
  return SymbolParts{base, quote, base + quote};
}

std::string SymbolNormalizer::InstrumentKey(Venue venue, ProductType product,
                                             const SymbolParts& parts,
                                             std::string_view contract_spec) const {
  std::string key = std::to_string(static_cast<std::uint16_t>(venue)) + ":" +
                    std::to_string(static_cast<std::uint8_t>(product)) + ":" + parts.canonical;
  if (!contract_spec.empty()) {
    auto normalized = UpperAlnum(contract_spec);
    if (!normalized || normalized->empty()) {
      throw std::invalid_argument("contract spec must be non-empty ASCII");
    }
    key += ":" + *normalized;
  }
  return key;
}

RegistryResult InstrumentRegistry::Register(const Instrument& instrument) {
  const std::string key = FixedString(instrument.instrument_key);
  if (instrument.instrument_id == 0 || key.empty() || instrument.tick_size <= 0 ||
      instrument.lot_size <= 0) return RegistryResult::Invalid;
  if (by_id_.contains(instrument.instrument_id)) return RegistryResult::DuplicateId;
  if (by_key_.contains(key)) return RegistryResult::DuplicateKey;
  const std::size_t index = instruments_.size();
  instruments_.push_back(instrument);
  by_id_.emplace(instrument.instrument_id, index);
  by_key_.emplace(key, index);
  return RegistryResult::Ok;
}

const Instrument* InstrumentRegistry::Find(InstrumentId id) const noexcept {
  const auto it = by_id_.find(id);
  return it == by_id_.end() ? nullptr : &instruments_[it->second];
}

const Instrument* InstrumentRegistry::Find(std::string_view key) const noexcept {
  const auto it = by_key_.find(key);
  return it == by_key_.end() ? nullptr : &instruments_[it->second];
}

}  // namespace utils::md
