#include <algorithm>
#include <stdexcept>
#include <string_view>

#include "strategyframe/catalog_instrument.h"

namespace {

template <std::size_t Capacity>
void Copy(std::string_view text, std::array<char, Capacity>& output) {
  if (text.size() >= output.size()) throw std::runtime_error("test overflow");
  std::copy(text.begin(), text.end(), output.begin());
}

void Require(bool condition) {
  if (!condition) throw std::runtime_error("catalog conversion failed");
}

}  // namespace

int main() {
  utils::md::InstrumentCatalog polymarket{};
  polymarket.instrument_id = 7;
  polymarket.venue = utils::md::Venue::Polymarket;
  polymarket.product_type = utils::md::ProductType::BinaryOption;
  polymarket.price_scale = 2;
  polymarket.quantity_scale = 2;
  polymarket.tick_size = 1;
  polymarket.lot_size = 1;
  Copy("BTC5MUP", polymarket.canonical_symbol);
  Copy("0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
       polymarket.condition_id);
  constexpr std::string_view token =
      "115792089237316195423570985008687907853269984665640564039457584007"
      "913129639935";
  static_assert(token.size() == 78);
  Copy(token, polymarket.venue_symbol);
  Copy("UP", polymarket.outcome);

  const auto converted =
      strategyframe::detail::CatalogInstrument(polymarket);
  Require(converted.instrument.venue_symbol[0] == '\0');
  Require(std::any_of(converted.polymarket_token_id.begin(),
                      converted.polymarket_token_id.end(),
                      [](std::uint8_t value) { return value != 0; }));
  Require(std::all_of(converted.polymarket_token_id.begin(),
                      converted.polymarket_token_id.end(),
                      [](std::uint8_t value) { return value == 0xffU; }));

  utils::md::InstrumentCatalog crypto{};
  crypto.instrument_id = 8;
  crypto.venue = utils::md::Venue::Binance;
  crypto.product_type = utils::md::ProductType::Spot;
  crypto.tick_size = 1;
  crypto.lot_size = 1;
  Copy("TOO_LONG", crypto.canonical_symbol);
  Copy("ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890", crypto.venue_symbol);
  bool rejected = false;
  try {
    (void)strategyframe::detail::CatalogInstrument(crypto);
  } catch (const std::length_error&) {
    rejected = true;
  }
  Require(rejected);
  return 0;
}

