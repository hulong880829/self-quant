#pragma once

#include "mds/api/mds_api.h"
#include "mds/ibkr/ibkr_runtime.h"

#include <string>

namespace mds::ibkr {

inline constexpr std::size_t kHardMaxSubscriptions = 512;

struct IbkrConfig {
  RuntimeOptions runtime;
  std::string backend{"POSIX_SHM"};
  std::string mode{"OVERWRITE_OLDEST"};
  std::string ring_layout{"multiplex"};
  std::size_t shard_count{1};
  std::size_t max_subscriptions{kHardMaxSubscriptions};
  std::string market_data_mode{"LIVE_ONLY"};
  std::string postgres_url_env;
};

[[nodiscard]] api::Result<IbkrConfig> load_config(const std::string &path);

}  // namespace mds::ibkr
