#pragma once

#include <cstddef>
#include <cstdint>
#include <string_view>

#include "polymm/types.h"

namespace polymm {

inline constexpr std::uint32_t kGammaHttpTimeoutMs = 5'000;
inline constexpr std::string_view kGammaMarketsQuerySuffix =
    "&closed=any&limit=1";

enum class GammaResolutionStatus : std::uint8_t {
  Pending = 0,
  Resolved = 1,
  InvalidResponse = 2,
};

struct ParsedGammaResolution {
  GammaResolutionStatus status{GammaResolutionStatus::Pending};
  Outcome winner{Outcome::Up};
};

[[nodiscard]] ParsedGammaResolution parse_gamma_resolution(
    std::string_view body,
    const strategyframe::ConditionId& expected_condition) noexcept;

[[nodiscard]] inline bool format_gamma_markets_target(
    const strategyframe::ConditionId& condition, char* output,
    std::size_t capacity, std::size_t& length) noexcept {
  constexpr std::string_view kPrefix = "/markets?condition_ids=";
  if (output == nullptr || condition.length == 0 ||
      condition.length > sizeof(condition.value)) {
    return false;
  }
  for (std::uint16_t index = 0; index < condition.length; ++index) {
    const char ch = condition.value[index];
    const bool ok = (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'z') ||
                    (ch >= 'A' && ch <= 'Z');
    if (!ok) return false;
  }
  const std::size_t need =
      kPrefix.size() + condition.length + kGammaMarketsQuerySuffix.size();
  if (need >= capacity) return false;
  std::size_t cursor = 0;
  for (const char ch : kPrefix) output[cursor++] = ch;
  for (std::uint16_t index = 0; index < condition.length; ++index) {
    output[cursor++] = condition.value[index];
  }
  for (const char ch : kGammaMarketsQuerySuffix) output[cursor++] = ch;
  output[cursor] = '\0';
  length = cursor;
  return true;
}

enum class ResolutionDisposition : std::uint8_t {
  DropLate = 0,
  FailActive = 1,
  Apply = 2,
};

[[nodiscard]] inline ResolutionDisposition resolution_disposition(
    std::uint64_t active_id,
    const strategyframe::ConditionId& active_condition,
    std::uint64_t result_id,
    const strategyframe::ConditionId& result_condition) noexcept {
  if (result_id == 0 || result_id != active_id) {
    return ResolutionDisposition::DropLate;
  }
  if (!(result_condition == active_condition)) {
    return ResolutionDisposition::FailActive;
  }
  return ResolutionDisposition::Apply;
}

[[nodiscard]] inline bool resolution_http_allowed(
    bool holding_result, std::uint32_t unconsumed,
    bool http_active) noexcept {
  return !holding_result && unconsumed == 0 && !http_active;
}

[[nodiscard]] inline std::uint64_t resolution_backoff_ns(
    std::uint32_t retry_index) noexcept {
  constexpr std::uint64_t kDelaysNs[] = {
      2'000'000'000ULL, 5'000'000'000ULL, 10'000'000'000ULL,
      30'000'000'000ULL};
  const std::uint32_t index = retry_index < 3 ? retry_index : 3;
  return kDelaysNs[index];
}

}  // namespace polymm
