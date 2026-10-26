#include "tradefimm/normalize.h"

#include <charconv>
#include <limits>

namespace tradefimm {
namespace {

bool mul(std::int64_t left, std::int64_t right, std::int64_t &out) noexcept {
  if (left != 0 && right > std::numeric_limits<std::int64_t>::max() / left)
    return false;
  if (left != 0 && right < std::numeric_limits<std::int64_t>::min() / left)
    return false;
  out = left * right;
  return true;
}

}  // namespace

bool parse_decimal(std::string_view text, std::uint8_t scale,
                   std::int64_t &out) noexcept {
  if (text.empty() || scale > 18) return false;
  std::size_t begin = 0;
  if (text.front() == '+') {
    begin = 1;
    if (begin == text.size()) return false;
  }
  const auto dot = text.find('.');
  const std::string_view whole =
      dot == std::string_view::npos ? text.substr(begin)
                                    : text.substr(begin, dot - begin);
  const std::string_view fraction =
      dot == std::string_view::npos ? std::string_view{} : text.substr(dot + 1);
  if (whole.empty() && fraction.empty()) return false;
  if (fraction.size() > scale) {
    for (std::size_t index = scale; index < fraction.size(); ++index) {
      if (fraction[index] != '0') return false;
    }
  }
  std::int64_t value = 0;
  if (!whole.empty()) {
    const auto parsed =
        std::from_chars(whole.data(), whole.data() + whole.size(), value);
    if (parsed.ec != std::errc{} || parsed.ptr != whole.data() + whole.size() ||
        value < 0)
      return false;
  }
  std::uint8_t seen = 0;
  for (const char digit : fraction) {
    if (digit < '0' || digit > '9') return false;
    if (seen >= scale) break;
    if (value > std::numeric_limits<std::int64_t>::max() / 10) return false;
    value = value * 10 + (digit - '0');
    ++seen;
  }
  while (seen < scale) {
    if (value > std::numeric_limits<std::int64_t>::max() / 10) return false;
    value *= 10;
    ++seen;
  }
  out = value;
  return true;
}

bool rescale_exact(std::int64_t value, std::uint8_t from, std::uint8_t to,
                   std::int64_t &out) noexcept {
  if (from > 18 || to > 18) return false;
  if (from == to) {
    out = value;
    return true;
  }
  if (to > from) {
    std::int64_t scaled = value;
    for (std::uint8_t index = from; index < to; ++index) {
      if (scaled > std::numeric_limits<std::int64_t>::max() / 10) return false;
      scaled *= 10;
    }
    out = scaled;
    return true;
  }
  std::int64_t divisor = 1;
  for (std::uint8_t index = to; index < from; ++index) {
    if (divisor > std::numeric_limits<std::int64_t>::max() / 10) return false;
    divisor *= 10;
  }
  if (value % divisor != 0) return false;
  out = value / divisor;
  return true;
}

bool restored_multiplier(std::int64_t mantissa, std::uint8_t scale,
                         std::int64_t &out) noexcept {
  if (mantissa <= 0 || scale > 18) return false;
  std::int64_t value = mantissa;
  for (std::uint8_t index = 0; index < scale; ++index) {
    if (value % 10 != 0) return false;
    value /= 10;
  }
  out = value;
  return out > 0;
}

bool normalized_price(std::int64_t raw_price, std::int64_t price_base_units,
                      std::int64_t &out) noexcept {
  if (raw_price <= 0 || price_base_units <= 0) return false;
  out = raw_price / price_base_units;
  return out > 0;
}

bool effective_units(const Units &units, std::int64_t &out) noexcept {
  std::int64_t restored = 0;
  if (!restored_multiplier(units.contract_multiplier,
                           units.contract_multiplier_scale, restored) ||
      units.quantity_unit_factor <= 0)
    return false;
  return mul(restored, units.quantity_unit_factor, out) && out > 0;
}

bool normalized_position(std::int64_t venue_quantity,
                         std::int64_t units_per_order,
                         std::int64_t &out) noexcept {
  if (units_per_order <= 0) return false;
  return mul(venue_quantity, units_per_order, out);
}

bool commission(const CostModel &model, bool sell, std::int64_t native_quantity,
                std::int64_t notional_usd, std::int64_t &out) noexcept {
  if (native_quantity == 0 || notional_usd < 0 || model.fixed_per_order < 0 ||
      model.per_order_unit < 0 || model.notional_bps < 0 ||
      model.minimum_per_order < 0 || model.sell_regulatory_fee_bps < 0)
    return false;
  const std::int64_t magnitude =
      native_quantity < 0 ? -native_quantity : native_quantity;
  std::int64_t variable = 0;
  if (!mul(model.per_order_unit, magnitude, variable)) return false;
  const std::int64_t notional_fee =
      (notional_usd * model.notional_bps) / 10000;
  std::int64_t variable_total = model.fixed_per_order + variable + notional_fee;
  if (variable_total < model.minimum_per_order)
    variable_total = model.minimum_per_order;
  const std::int64_t regulatory =
      sell ? (notional_usd * model.sell_regulatory_fee_bps) / 10000 : 0;
  out = variable_total + regulatory;
  return true;
}

bool quote_currency_allowed(bool ib_currency_usd,
                            bool perp_stable_quote) noexcept {
  return ib_currency_usd && perp_stable_quote;
}

}  // namespace tradefimm
