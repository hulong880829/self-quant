#pragma once

#include <array>

#include "strategyframe/types.h"

namespace polymm {

[[nodiscard]] bool is_next_physical_catalog(
    const strategyframe::InstrumentCatalogInfo& current,
    const strategyframe::InstrumentCatalogInfo& candidate) noexcept;

[[nodiscard]] bool catalog_rollover_ready(
    const std::array<strategyframe::InstrumentCatalogInfo, 2>&
        candidates) noexcept;

}  // namespace polymm
