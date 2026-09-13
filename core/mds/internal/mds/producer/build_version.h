#pragma once

#include <string_view>

namespace mds::producer {

struct BuildVersion {
  std::string_view git_sha;
  bool dirty{};
  std::string_view build_utc;
  std::string_view build_id;
};

[[nodiscard]] BuildVersion build_version() noexcept;

}  // namespace mds::producer
