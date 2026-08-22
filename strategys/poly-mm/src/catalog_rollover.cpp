#include "polymm/catalog_rollover.h"

namespace polymm {

bool is_next_physical_catalog(
    const strategyframe::InstrumentCatalogInfo& current,
    const strategyframe::InstrumentCatalogInfo& candidate) noexcept {
  return candidate.instrument_id != 0 &&
         candidate.instrument_id != current.instrument_id &&
         candidate.venue == strategyframe::Venue::Polymarket &&
         candidate.product == strategyframe::ProductType::BinaryOption &&
         candidate.canonical_symbol == current.canonical_symbol &&
         candidate.expiry_time_ns > current.expiry_time_ns;
}

bool catalog_rollover_ready(
    const std::array<strategyframe::InstrumentCatalogInfo, 2>&
        candidates) noexcept {
  return candidates[0].instrument_id != 0 &&
         candidates[1].instrument_id != 0 &&
         candidates[0].instrument_id != candidates[1].instrument_id &&
         candidates[0].venue == strategyframe::Venue::Polymarket &&
         candidates[1].venue == strategyframe::Venue::Polymarket &&
         candidates[0].product ==
             strategyframe::ProductType::BinaryOption &&
         candidates[1].product ==
             strategyframe::ProductType::BinaryOption &&
         candidates[0].expiry_time_ns != 0 &&
         candidates[0].expiry_time_ns ==
             candidates[1].expiry_time_ns;
}

}  // namespace polymm
