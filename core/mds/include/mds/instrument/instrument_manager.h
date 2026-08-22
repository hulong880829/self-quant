#pragma once

#include "utils/md/types.h"

#include <mutex>
#include <string>
#include <string_view>
#include <unordered_map>

namespace mds::instrument {

class InstrumentManager {
 public:
  using HashFunction =
      utils::md::InstrumentId (*)(std::string_view) noexcept;

  [[nodiscard]] bool resolve(utils::md::Venue venue,
                             utils::md::ProductType product,
                             std::string_view canonical_symbol,
                             utils::md::InstrumentId &instrument_id,
                             std::string &error);
  [[nodiscard]] bool resolve_instance(
      utils::md::Venue venue, utils::md::ProductType product,
      std::string_view canonical_symbol, std::string_view instance_identity,
      utils::md::InstrumentId &instrument_id, std::string &error);
  [[nodiscard]] bool retire(utils::md::InstrumentId instrument_id) noexcept;

  [[nodiscard]] static std::string canonical_key(
      utils::md::Venue venue, utils::md::ProductType product,
      std::string_view canonical_symbol);
  [[nodiscard]] static utils::md::InstrumentId hash_key(
      std::string_view canonical_key) noexcept;

  explicit InstrumentManager(HashFunction hash = &hash_key) noexcept
      : hash_(hash) {}

 private:
  [[nodiscard]] bool resolve_key(std::string key,
                                 utils::md::InstrumentId &instrument_id,
                                 std::string &error);

  HashFunction hash_;
  std::mutex mutex_;
  std::unordered_map<utils::md::InstrumentId, std::string> by_id_;
  std::unordered_map<std::string, utils::md::InstrumentId> by_key_;
};

}  // namespace mds::instrument
