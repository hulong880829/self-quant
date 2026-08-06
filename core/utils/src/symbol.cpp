#include "utils/md/symbol.h"

#include <algorithm>
#include <cctype>
#include <cstring>

namespace utils::md {
namespace {
std::string UpperAlnum(std::string_view value) {
  std::string out;
  out.reserve(value.size());
  for (const char raw : value) {
    const auto c = static_cast<unsigned char>(raw);
    if (std::isalnum(c)) out.push_back(static_cast<char>(std::toupper(c)));
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
  aliases_[UpperAlnum(alias)] = UpperAlnum(canonical);
}

std::string SymbolNormalizer::NormalizeAsset(std::string_view asset) const {
  std::string normalized = UpperAlnum(asset);
  if (auto it = aliases_.find(normalized); it != aliases_.end()) return it->second;
  return normalized;
}

std::optional<SymbolParts> SymbolNormalizer::Normalize(
    std::string_view venue_symbol, std::string_view quote_hint) const {
  const auto delimiter = venue_symbol.find_first_of("-/_:");
  std::string base;
  std::string quote;
  if (delimiter != std::string_view::npos) {
    base = NormalizeAsset(venue_symbol.substr(0, delimiter));
    quote = NormalizeAsset(venue_symbol.substr(delimiter + 1));
  } else {
    const std::string joined = UpperAlnum(venue_symbol);
    if (!quote_hint.empty()) {
      quote = NormalizeAsset(quote_hint);
      if (joined.size() <= quote.size() ||
          joined.compare(joined.size() - quote.size(), quote.size(), quote) != 0) return std::nullopt;
      base = NormalizeAsset(std::string_view(joined).substr(0, joined.size() - quote.size()));
    } else {
      for (const auto& candidate : known_quotes_) {
        if (joined.size() > candidate.size() &&
            joined.compare(joined.size() - candidate.size(), candidate.size(), candidate) == 0) {
          base = NormalizeAsset(std::string_view(joined).substr(0, joined.size() - candidate.size()));
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
  if (!contract_spec.empty()) key += ":" + UpperAlnum(contract_spec);
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

const Instrument* InstrumentRegistry::Find(std::uint32_t id) const noexcept {
  const auto it = by_id_.find(id);
  return it == by_id_.end() ? nullptr : &instruments_[it->second];
}

const Instrument* InstrumentRegistry::Find(std::string_view key) const noexcept {
  const auto it = by_key_.find(key);
  return it == by_key_.end() ? nullptr : &instruments_[it->second];
}

}  // namespace utils::md
