#pragma once

#include "utils/md/types.h"

#include <charconv>
#include <chrono>
#include <cstdint>
#include <string_view>

namespace mds::service {

enum class SnapshotHttpAction : std::uint8_t {
  Parse,
  RetrySymbol,
  CooldownVenue,
  BanCooldownVenue,
  QuarantineSymbol,
};

[[nodiscard]] inline SnapshotHttpAction classify_snapshot_http(
    utils::md::Venue venue, unsigned status,
    std::string_view body) noexcept {
  if (status >= 200 && status < 300) return SnapshotHttpAction::Parse;
  if (status == 418) return SnapshotHttpAction::BanCooldownVenue;
  if (status == 429) return SnapshotHttpAction::CooldownVenue;
  if (venue == utils::md::Venue::Binance &&
      (body.find("-1121") != std::string_view::npos ||
       body.find("Invalid symbol") != std::string_view::npos))
    return SnapshotHttpAction::QuarantineSymbol;
  return SnapshotHttpAction::RetrySymbol;
}

[[nodiscard]] inline std::chrono::milliseconds parse_retry_after(
    std::string_view value) noexcept {
  std::uint64_t seconds{};
  const auto parsed =
      std::from_chars(value.data(), value.data() + value.size(), seconds);
  if (parsed.ec != std::errc{} || parsed.ptr != value.data() + value.size())
    return {};
  constexpr std::uint64_t kMaximumRetrySeconds = 3600;
  return std::chrono::milliseconds(
      (seconds > kMaximumRetrySeconds ? kMaximumRetrySeconds : seconds) *
      1000U);
}

}  // namespace mds::service
