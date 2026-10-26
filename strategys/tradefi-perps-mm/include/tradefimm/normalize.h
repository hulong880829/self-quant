#pragma once

#include <cstdint>
#include <string_view>

namespace tradefimm {

struct Units {
  std::int64_t price_base_units{1};
  std::int64_t quantity_unit_factor{1};
  std::int64_t contract_multiplier{1};
  std::uint8_t contract_multiplier_scale{};
};

struct CostModel {
  std::int64_t fixed_per_order{};
  std::int64_t per_order_unit{};
  std::int64_t notional_bps{};
  std::int64_t minimum_per_order{};
  std::int64_t sell_regulatory_fee_bps{};
  std::int64_t hyperliquid_maker_fee_bps{};
};

[[nodiscard]] bool parse_decimal(std::string_view text, std::uint8_t scale,
                                std::int64_t &out) noexcept;

[[nodiscard]] bool rescale_exact(std::int64_t value, std::uint8_t from,
                                 std::uint8_t to, std::int64_t &out) noexcept;

[[nodiscard]] bool restored_multiplier(std::int64_t mantissa,
                                       std::uint8_t scale,
                                       std::int64_t &out) noexcept;

[[nodiscard]] bool normalized_price(std::int64_t raw_price,
                                    std::int64_t price_base_units,
                                    std::int64_t &out) noexcept;

[[nodiscard]] bool effective_units(const Units &units,
                                   std::int64_t &out) noexcept;

[[nodiscard]] bool normalized_position(std::int64_t venue_quantity,
                                       std::int64_t units_per_order,
                                       std::int64_t &out) noexcept;

// per_order_unit is charged on the IB native quantity.
[[nodiscard]] bool commission(const CostModel &model, bool sell,
                              std::int64_t native_quantity,
                              std::int64_t notional_usd,
                              std::int64_t &out) noexcept;

[[nodiscard]] bool quote_currency_allowed(bool ib_currency_usd,
                                          bool perp_stable_quote) noexcept;

}  // namespace tradefimm
