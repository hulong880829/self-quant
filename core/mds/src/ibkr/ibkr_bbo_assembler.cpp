#include "mds/ibkr/ibkr_bbo_assembler.h"

#include "utils/md/decimal.h"

#include <charconv>
#include <cmath>
#include <cstring>
#include <limits>
#include <string>

namespace mds::ibkr {
namespace {

constexpr std::size_t kDecimalDigitLimit = 79;

// Integer std::from_chars does not accept a leading '+', so the sign is
// consumed here and only the digit text is parsed.
bool parse_exponent(std::string_view text, std::size_t index,
                    int &exponent) noexcept {
  if (index >= text.size()) {
    return false;
  }
  const bool negative = text[index] == '-';
  if (text[index] == '+' || text[index] == '-') {
    ++index;
  }
  if (index >= text.size() || text[index] < '0' || text[index] > '9') {
    return false;
  }
  unsigned long magnitude = 0;
  const char *const begin = text.data() + index;
  const char *const end = text.data() + text.size();
  const auto parsed = std::from_chars(begin, end, magnitude);
  if (parsed.ec != std::errc{} || parsed.ptr != end) {
    return false;
  }
  constexpr auto kExponentLimit =
      static_cast<unsigned long>(std::numeric_limits<int>::max());
  if (magnitude > kExponentLimit) {
    return false;
  }
  exponent = negative ? -static_cast<int>(magnitude) : static_cast<int>(magnitude);
  return true;
}

bool shift_decimal_point(char *integer_digits, std::size_t &integer_count,
                         char *fraction_digits, std::size_t &fraction_count,
                         int exponent) noexcept {
  if (exponent == 0) {
    return true;
  }
  std::size_t integer_begin = 0;
  while (integer_begin < integer_count &&
         integer_digits[integer_begin] == '0') {
    ++integer_begin;
  }
  const std::size_t significant_integer = integer_count - integer_begin;
  if (significant_integer > kDecimalDigitLimit ||
      fraction_count > kDecimalDigitLimit) {
    return false;
  }

  char digits[kDecimalDigitLimit * 2]{};
  const std::size_t significand_length = significant_integer + fraction_count;
  if (significant_integer > 0) {
    std::memcpy(digits, integer_digits + integer_begin, significant_integer);
  }
  if (fraction_count > 0) {
    std::memcpy(digits + significant_integer, fraction_digits, fraction_count);
  }

  long point = static_cast<long>(significant_integer);
  if (exponent > 0) {
    point += exponent;
  } else {
    point -= static_cast<long>(static_cast<unsigned int>(-exponent));
  }

  std::size_t length = significand_length;
  if (point < 0) {
    const auto zeros = static_cast<std::size_t>(-point);
    if (zeros > kDecimalDigitLimit || zeros > sizeof(digits) - length) {
      return false;
    }
    if (length > 0) {
      std::memmove(digits + zeros, digits, length);
    }
    std::memset(digits, '0', zeros);
    length += zeros;
    point = 0;
  } else if (static_cast<std::size_t>(point) > length) {
    const auto zeros = static_cast<std::size_t>(point) - length;
    if (static_cast<std::size_t>(point) > kDecimalDigitLimit ||
        zeros > sizeof(digits) - length) {
      return false;
    }
    std::memset(digits + length, '0', zeros);
    length += zeros;
  }

  if (point < 0 || static_cast<std::size_t>(point) > kDecimalDigitLimit ||
      static_cast<std::size_t>(point) > length) {
    return false;
  }
  const auto point_index = static_cast<std::size_t>(point);
  const auto new_fraction = length - point_index;
  if (new_fraction > kDecimalDigitLimit) {
    return false;
  }

  std::size_t start = 0;
  while (start + 1 < point_index && digits[start] == '0') {
    ++start;
  }
  const auto new_integer = point_index - start;
  if (new_integer == 0) {
    integer_digits[0] = '0';
    integer_count = 1;
  } else if (new_integer > kDecimalDigitLimit) {
    return false;
  } else {
    std::memcpy(integer_digits, digits + start, new_integer);
    integer_count = new_integer;
  }
  if (new_fraction > 0) {
    std::memcpy(fraction_digits, digits + point_index, new_fraction);
  }
  fraction_count = new_fraction;
  return true;
}

bool format_rounded_decimal(std::string_view text, std::uint8_t scale,
                            char *destination, std::size_t capacity,
                            std::size_t &length) noexcept {
  if (text.empty() || destination == nullptr || capacity < 4) {
    return false;
  }
  std::size_t index = 0;
  const bool negative = text[index] == '-';
  if (text[index] == '+' || text[index] == '-') {
    ++index;
    if (index == text.size()) {
      return false;
    }
  }
  char integer_digits[80]{};
  char fraction_digits[80]{};
  std::size_t integer_count = 0;
  std::size_t fraction_count = 0;
  bool seen_dot = false;
  bool seen_digit = false;
  bool has_exponent = false;
  for (; index < text.size(); ++index) {
    const char character = text[index];
    if (character == 'e' || character == 'E') {
      has_exponent = true;
      ++index;
      break;
    }
    if (character == '.') {
      if (seen_dot) {
        return false;
      }
      seen_dot = true;
      continue;
    }
    if (character < '0' || character > '9') {
      return false;
    }
    seen_digit = true;
    if (!seen_dot) {
      if (integer_count + 1 >= sizeof(integer_digits)) {
        return false;
      }
      if (!(integer_count == 0 && character == '0')) {
        integer_digits[integer_count++] = character;
      } else if (integer_count == 0) {
        integer_digits[0] = '0';
        integer_count = 1;
      }
    } else {
      if (fraction_count + 1 >= sizeof(fraction_digits)) {
        return false;
      }
      fraction_digits[fraction_count++] = character;
    }
  }
  if (!seen_digit) {
    return false;
  }
  if (integer_count == 0) {
    integer_digits[0] = '0';
    integer_count = 1;
  }
  if (has_exponent) {
    int exponent = 0;
    if (!parse_exponent(text, index, exponent) ||
        !shift_decimal_point(integer_digits, integer_count, fraction_digits,
                             fraction_count, exponent)) {
      return false;
    }
  }

  if (fraction_count > scale) {
    const bool round_up = fraction_digits[scale] >= '5';
    fraction_count = scale;
    if (round_up) {
      int cursor = static_cast<int>(fraction_count) - 1;
      while (cursor >= 0) {
        if (fraction_digits[cursor] < '9') {
          ++fraction_digits[cursor];
          break;
        }
        fraction_digits[cursor] = '0';
        --cursor;
      }
      if (cursor < 0) {
        int integer_cursor = static_cast<int>(integer_count) - 1;
        while (integer_cursor >= 0) {
          if (integer_digits[integer_cursor] < '9') {
            ++integer_digits[integer_cursor];
            break;
          }
          integer_digits[integer_cursor] = '0';
          --integer_cursor;
        }
        if (integer_cursor < 0) {
          if (integer_count + 1 >= sizeof(integer_digits)) {
            return false;
          }
          std::memmove(integer_digits + 1, integer_digits, integer_count);
          integer_digits[0] = '1';
          ++integer_count;
        }
      }
    }
  }

  std::size_t used = 0;
  if (negative) {
    if (used + 1 >= capacity) {
      return false;
    }
    destination[used++] = '-';
  }
  if (used + integer_count >= capacity) {
    return false;
  }
  std::memcpy(destination + used, integer_digits, integer_count);
  used += integer_count;
  if (scale > 0) {
    if (used + 1 + scale >= capacity) {
      return false;
    }
    destination[used++] = '.';
    const auto present = fraction_count < scale ? fraction_count : scale;
    std::memcpy(destination + used, fraction_digits, present);
    used += present;
    for (std::size_t pad = present; pad < scale; ++pad) {
      destination[used++] = '0';
    }
  }
  destination[used] = '\0';
  length = used;
  return true;
}

}  // namespace

bool double_price_to_fixed(double value, std::uint8_t scale,
                           std::int64_t &out) noexcept {
  if (!std::isfinite(value) || !(value > 0.0)) {
    return false;
  }
  char buffer[512];
  const auto converted =
      std::to_chars(buffer, buffer + sizeof(buffer), value,
                    std::chars_format::fixed, static_cast<int>(scale));
  if (converted.ec != std::errc{}) {
    return false;
  }
  if (!utils::md::decimal_to_fixed(
          std::string_view(buffer, static_cast<std::size_t>(converted.ptr -
                                                            buffer)),
          scale, out)) {
    return false;
  }
  return out > 0;
}

namespace {

[[nodiscard]] bool shortest_tick_scale(double min_tick,
                                       int &required_scale) noexcept {
  if (!std::isfinite(min_tick) || !(min_tick > 0.0)) {
    return false;
  }
  char buffer[128];
  const auto converted =
      std::to_chars(buffer, buffer + sizeof(buffer), min_tick,
                    std::chars_format::general);
  if (converted.ec != std::errc{}) {
    return false;
  }
  const std::string_view text(buffer, static_cast<std::size_t>(converted.ptr - buffer));
  if (text.empty() || text.front() == '-' || text.front() == '+') {
    return false;
  }
  std::size_t exponent_pos = text.find_first_of("eE");
  const auto mantissa = text.substr(0, exponent_pos);
  int exponent = 0;
  if (exponent_pos != std::string_view::npos) {
    const auto exponent_text = text.substr(exponent_pos + 1);
    if (exponent_text.empty()) {
      return false;
    }
    const auto parsed = std::from_chars(exponent_text.data(),
                                        exponent_text.data() + exponent_text.size(),
                                        exponent);
    if (parsed.ec != std::errc{} ||
        parsed.ptr != exponent_text.data() + exponent_text.size()) {
      return false;
    }
  }
  const auto dot = mantissa.find('.');
  std::string digits;
  int point_from_right = 0;
  if (dot == std::string_view::npos) {
    digits.assign(mantissa.begin(), mantissa.end());
  } else {
    digits.assign(mantissa.begin(), mantissa.begin() + static_cast<std::ptrdiff_t>(dot));
    digits.append(mantissa.begin() + static_cast<std::ptrdiff_t>(dot + 1), mantissa.end());
    point_from_right = static_cast<int>(mantissa.size() - dot - 1);
  }
  if (digits.empty() ||
      digits.find_first_not_of("0123456789") != std::string::npos) {
    return false;
  }
  while (digits.size() > 1 && digits.back() == '0') {
    digits.pop_back();
    --point_from_right;
  }
  std::size_t first_nonzero = 0;
  while (first_nonzero + 1 < digits.size() && digits[first_nonzero] == '0') {
    ++first_nonzero;
  }
  if (first_nonzero > 0) {
    digits.erase(0, first_nonzero);
  }
  required_scale = point_from_right - exponent;
  if (required_scale < 0) {
    required_scale = 0;
  }
  return !digits.empty() && digits != "0";
}

}  // namespace

bool price_scale_for_tick(double min_tick, std::uint8_t &scale) noexcept {
  int required = 0;
  if (!shortest_tick_scale(min_tick, required) || required < 0 ||
      required > static_cast<int>(kMaxDerivedPriceScale)) {
    return false;
  }
  scale = static_cast<std::uint8_t>(required);
  return true;
}

bool min_tick_fits_scale(double min_tick, std::uint8_t scale) noexcept {
  int required = 0;
  return shortest_tick_scale(min_tick, required) && required >= 0 &&
         required <= static_cast<int>(scale);
}

bool decimal_size_to_fixed(std::string_view text, std::uint8_t scale,
                           std::int64_t &out, bool &non_positive) noexcept {
  non_positive = false;
  char buffer[192];
  std::size_t length = 0;
  if (!format_rounded_decimal(text, scale, buffer, sizeof(buffer), length)) {
    return false;
  }
  if (!utils::md::decimal_to_fixed(std::string_view(buffer, length), scale,
                                   out)) {
    return false;
  }
  non_positive = out <= 0;
  return true;
}

BboAssembler::BboAssembler(std::uint8_t price_scale,
                           std::uint8_t quantity_scale) noexcept
    : price_scale_(price_scale), quantity_scale_(quantity_scale) {}

void BboAssembler::reset(std::uint64_t generation) noexcept {
  generation_ = generation;
  clear_price(true);
  clear_price(false);
  clear_size(true);
  clear_size(false);
  publishable_ = false;
  unhealthy_ = false;
  data_kind_ = MarketDataKind::Unset;
}

void BboAssembler::clear_price(bool bid) noexcept {
  if (bid) {
    bid_price_valid_ = false;
    bid_price_ = 0;
  } else {
    ask_price_valid_ = false;
    ask_price_ = 0;
  }
}

void BboAssembler::clear_size(bool bid) noexcept {
  if (bid) {
    bid_size_valid_ = false;
    bid_size_ = 0;
  } else {
    ask_size_valid_ = false;
    ask_size_ = 0;
  }
}

FixedQuote BboAssembler::quote() const noexcept {
  return {.bid_price = bid_price_,
          .ask_price = ask_price_,
          .bid_size = bid_size_,
          .ask_size = ask_size_};
}

AssemblerUpdate BboAssembler::refresh(bool changed) noexcept {
  publishable_ = false;
  if (data_kind_ != MarketDataKind::Live) {
    unhealthy_ = data_kind_ != MarketDataKind::Unset;
    return {.changed = changed, .publishable = false};
  }
  unhealthy_ = false;
  if (!bid_price_valid_ || !ask_price_valid_ || !bid_size_valid_ ||
      !ask_size_valid_) {
    return {.changed = changed, .publishable = false};
  }
  if (bid_price_ <= 0 || ask_price_ <= 0 || bid_size_ <= 0 || ask_size_ <= 0 ||
      bid_price_ > ask_price_) {
    return {.changed = changed, .publishable = false};
  }
  publishable_ = true;
  return {.changed = changed, .publishable = true};
}

AssemblerUpdate BboAssembler::on_price(bool bid, double price,
                                       std::uint64_t generation) noexcept {
  if (generation != generation_) {
    return {};
  }
  if (!std::isfinite(price) || price <= 0.0) {
    clear_price(bid);
    if (!std::isfinite(price)) {
      invalid_ticks_.fetch_add(1, std::memory_order_relaxed);
    }
    return refresh(true);
  }
  std::int64_t fixed = 0;
  if (!double_price_to_fixed(price, price_scale_, fixed)) {
    clear_price(bid);
    ++invalid_ticks_;
    return refresh(true);
  }
  if (bid) {
    bid_price_ = fixed;
    bid_price_valid_ = true;
  } else {
    ask_price_ = fixed;
    ask_price_valid_ = true;
  }
  return refresh(true);
}

AssemblerUpdate BboAssembler::on_size(bool bid, std::string_view size,
                                      std::uint64_t generation) noexcept {
  if (generation != generation_) {
    return {};
  }
  std::int64_t fixed = 0;
  bool non_positive = false;
  if (!decimal_size_to_fixed(size, quantity_scale_, fixed, non_positive) ||
      non_positive) {
    clear_size(bid);
    if (!non_positive) {
      invalid_ticks_.fetch_add(1, std::memory_order_relaxed);
    }
    return refresh(true);
  }
  if (bid) {
    bid_size_ = fixed;
    bid_size_valid_ = true;
  } else {
    ask_size_ = fixed;
    ask_size_valid_ = true;
  }
  return refresh(true);
}

AssemblerUpdate BboAssembler::on_market_data_type(
    MarketDataKind kind, std::uint64_t generation) noexcept {
  if (generation != generation_) {
    return {};
  }
  data_kind_ = kind;
  if (kind != MarketDataKind::Live) {
    non_live_ticks_.fetch_add(1, std::memory_order_relaxed);
  }
  return refresh(true);
}

}  // namespace mds::ibkr
