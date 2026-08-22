#include "utils/md/decimal.h"

#include <cctype>
#include <limits>

namespace utils::md {
namespace {

bool parse_decimal(std::string_view value, bool &negative,
                   std::uint64_t &digits, std::uint8_t &fractional,
                   int &exponent) noexcept {
  negative = false;
  digits = 0;
  fractional = 0;
  exponent = 0;
  if (value.empty()) {
    return false;
  }
  if (value.front() == '-' || value.front() == '+') {
    negative = value.front() == '-';
    value.remove_prefix(1);
  }
  bool seen_digit = false;
  bool seen_decimal = false;
  std::size_t index = 0;
  for (; index < value.size(); ++index) {
    const char character = value[index];
    if (character == '.') {
      if (seen_decimal) {
        return false;
      }
      seen_decimal = true;
      continue;
    }
    if (character == 'e' || character == 'E') {
      break;
    }
    if (!std::isdigit(static_cast<unsigned char>(character))) {
      return false;
    }
    const auto digit = static_cast<unsigned>(character - '0');
    if (digits >
        (std::numeric_limits<std::uint64_t>::max() - digit) / 10) {
      return false;
    }
    digits = digits * 10 + digit;
    if (seen_decimal) {
      if (fractional == std::numeric_limits<std::uint8_t>::max()) {
        return false;
      }
      ++fractional;
    }
    seen_digit = true;
  }
  if (!seen_digit) {
    return false;
  }
  if (index < value.size()) {
    ++index;
    bool exponent_negative = false;
    if (index < value.size() &&
        (value[index] == '-' || value[index] == '+')) {
      exponent_negative = value[index] == '-';
      ++index;
    }
    if (index == value.size()) {
      return false;
    }
    int parsed = 0;
    for (; index < value.size(); ++index) {
      if (!std::isdigit(static_cast<unsigned char>(value[index]))) {
        return false;
      }
      parsed = parsed * 10 + (value[index] - '0');
      if (parsed > 64) {
        return false;
      }
    }
    exponent = exponent_negative ? -parsed : parsed;
  }
  return true;
}

bool multiply_power10(std::uint64_t &value, unsigned power) noexcept {
  for (unsigned index = 0; index < power; ++index) {
    if (value > std::numeric_limits<std::uint64_t>::max() / 10) {
      return false;
    }
    value *= 10;
  }
  return true;
}

}  // namespace

bool decimal_scale(std::string_view value, std::uint8_t &scale) noexcept {
  bool negative{};
  std::uint64_t digits{};
  std::uint8_t fractional{};
  int exponent{};
  if (!parse_decimal(value, negative, digits, fractional, exponent)) {
    return false;
  }
  const int effective = static_cast<int>(fractional) - exponent;
  if (effective < 0 || effective > 18) {
    return false;
  }
  scale = static_cast<std::uint8_t>(effective);
  return true;
}

bool decimal_to_fixed(std::string_view value, std::uint8_t scale,
                      std::int64_t &result) noexcept {
  bool negative{};
  std::uint64_t digits{};
  std::uint8_t fractional{};
  int exponent{};
  if (!parse_decimal(value, negative, digits, fractional, exponent)) {
    return false;
  }
  const int source_scale = static_cast<int>(fractional) - exponent;
  if (source_scale < 0) {
    if (!multiply_power10(digits, static_cast<unsigned>(-source_scale))) {
      return false;
    }
    if (!multiply_power10(digits, scale)) {
      return false;
    }
  } else if (source_scale <= scale) {
    if (!multiply_power10(
            digits, static_cast<unsigned>(scale - source_scale))) {
      return false;
    }
  } else {
    for (int index = 0; index < source_scale - scale; ++index) {
      if (digits % 10 != 0) {
        return false;
      }
      digits /= 10;
    }
  }
  const auto positive_limit =
      static_cast<std::uint64_t>(std::numeric_limits<std::int64_t>::max());
  const auto negative_limit = positive_limit + 1;
  if ((!negative && digits > positive_limit) ||
      (negative && digits > negative_limit)) {
    return false;
  }
  if (negative && digits == negative_limit) {
    result = std::numeric_limits<std::int64_t>::min();
  } else {
    const auto signed_digits = static_cast<std::int64_t>(digits);
    result = negative ? -signed_digits : signed_digits;
  }
  return true;
}

}  // namespace utils::md
