#pragma once

#include "mds/ibkr/ibkr_runtime.h"

#include <cstddef>
#include <string>
#include <string_view>
#include <vector>

namespace mds::ibkr {

struct CandidateLoad {
  std::vector<InstrumentSpec> subscriptions;
  std::size_t skipped{};
};

[[nodiscard]] std::string_view postgres_candidate_query() noexcept;
[[nodiscard]] std::string_view postgres_turnover_update_query() noexcept;

[[nodiscard]] bool load_postgres_candidates(std::string_view url_env,
                                              CandidateLoad &loaded,
                                              std::string &error);

[[nodiscard]] bool persist_postgres_turnover_metrics(
    std::string_view url_env,
    const std::vector<InstrumentTurnoverMetric> &metrics,
    std::size_t &updated, std::string &error);

}  // namespace mds::ibkr
