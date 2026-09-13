#include "mds/instrument/instrument_manager.h"

#include <exception>
#include <utility>

namespace mds::instrument {
namespace {

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
    case utils::md::Venue::Aster:
      return "aster";
    case utils::md::Venue::Lighter:
      return "lighter";
    case utils::md::Venue::Unknown:
      break;
  }
  return {};
}

std::string_view product_name(utils::md::ProductType product) noexcept {
  switch (product) {
    case utils::md::ProductType::Spot:
      return "spot";
    case utils::md::ProductType::Perpetual:
      return "perpetual";
    case utils::md::ProductType::Future:
      return "future";
    case utils::md::ProductType::BinaryOption:
      return "binary_option";
    case utils::md::ProductType::Equity:
      return "equity";
    case utils::md::ProductType::Unknown:
      break;
  }
  return {};
}

utils::md::InstrumentId fnv1a64(std::string_view value) noexcept {
  constexpr std::uint64_t kOffset = 14695981039346656037ULL;
  constexpr std::uint64_t kPrime = 1099511628211ULL;
  std::uint64_t hash = kOffset;
  for (const char character : value) {
    hash ^= static_cast<unsigned char>(character);
    hash *= kPrime;
  }
  return hash;
}

}  // namespace

std::string InstrumentManager::canonical_key(
    utils::md::Venue venue, utils::md::ProductType product,
    std::string_view canonical_symbol) {
  const auto venue_text = venue_name(venue);
  const auto product_text = product_name(product);
  if (venue_text.empty() || product_text.empty() ||
      canonical_symbol.empty()) {
    return {};
  }
  std::string key;
  key.reserve(venue_text.size() + product_text.size() +
              canonical_symbol.size() + 2);
  key.append(venue_text);
  key.push_back('|');
  key.append(product_text);
  key.push_back('|');
  key.append(canonical_symbol);
  return key;
}

utils::md::InstrumentId InstrumentManager::hash_key(
    std::string_view key) noexcept {
  auto value = fnv1a64(key);
  if (value != 0) {
    return value;
  }
  // Domain-separated deterministic retries for the reserved zero value.
  // The loop is bounded only defensively; finding even one zero is already
  // cryptographically unlikely for the configured universe.
  std::string retry(key);
  for (unsigned domain = 1; domain != 0; ++domain) {
    retry.resize(key.size());
    retry.push_back('|');
    retry.append(std::to_string(domain));
    value = fnv1a64(retry);
    if (value != 0) {
      return value;
    }
  }
  std::terminate();
}

bool InstrumentManager::resolve(utils::md::Venue venue,
                                utils::md::ProductType product,
                                std::string_view canonical_symbol,
                                utils::md::InstrumentId &instrument_id,
                                std::string &error) {
  return resolve_key(canonical_key(venue, product, canonical_symbol),
                     instrument_id, error);
}

bool InstrumentManager::resolve_instance(
    utils::md::Venue venue, utils::md::ProductType product,
    std::string_view canonical_symbol, std::string_view instance_identity,
    utils::md::InstrumentId &instrument_id, std::string &error) {
  auto key = canonical_key(venue, product, canonical_symbol);
  if (key.empty() || instance_identity.empty()) {
    error = "instrument instance identity is incomplete";
    return false;
  }
  key.push_back('|');
  key.append(instance_identity);
  return resolve_key(std::move(key), instrument_id, error);
}

bool InstrumentManager::resolve_key(
    std::string key, utils::md::InstrumentId &instrument_id,
    std::string &error) {
  if (key.empty()) {
    error = "instrument canonical identity is incomplete";
    return false;
  }
  const auto id = hash_(key);
  if (id == 0) {
    error = "instrument hash function returned reserved id 0 for " + key;
    return false;
  }
  std::lock_guard lock(mutex_);
  const auto known_key = by_key_.find(key);
  if (known_key != by_key_.end() && known_key->second != id) {
    error = "instrument identity remap key=" + key +
            " existing_id=" + std::to_string(known_key->second) +
            " incoming_id=" + std::to_string(id);
    return false;
  }
  const auto known_id = by_id_.find(id);
  if (known_id != by_id_.end() && known_id->second != key) {
    error = "instrument id collision id=" + std::to_string(id) +
            " existing=" + known_id->second + " incoming=" + key;
    return false;
  }
  by_id_.try_emplace(id, key);
  by_key_.try_emplace(key, id);
  instrument_id = id;
  return true;
}

bool InstrumentManager::retire(
    utils::md::InstrumentId instrument_id) noexcept {
  std::lock_guard lock(mutex_);
  const auto found = by_id_.find(instrument_id);
  if (found == by_id_.end()) return false;
  by_key_.erase(found->second);
  by_id_.erase(found);
  return true;
}

}  // namespace mds::instrument
