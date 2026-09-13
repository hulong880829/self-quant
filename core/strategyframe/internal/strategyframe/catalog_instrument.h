#pragma once

#include "oms/api/oms_api.h"
#include "utils/md/types.h"

namespace strategyframe::detail {

// Control-plane conversion used when a catalog becomes executable. Polymarket
// token text is parsed from the 80-byte catalog field into the binary route;
// it is never copied into Instrument::venue_symbol.
[[nodiscard]] oms::api::InstrumentInit CatalogInstrument(
    const utils::md::InstrumentCatalog& source);
[[nodiscard]] oms::api::ResolvedInstrument CatalogRouting(
    const utils::md::InstrumentCatalog& source, std::uint32_t generation);

}  // namespace strategyframe::detail

