#include "tradefimm/normalize.h"

#include <cassert>
#include <iostream>

int main() {
  using namespace tradefimm;
  std::int64_t price = 0;
  assert(normalized_price(20000, 100, price));
  assert(price == 200);
  Units units;
  units.contract_multiplier = 100;
  std::int64_t effective = 0;
  assert(effective_units(units, effective));
  assert(effective == 100);
  std::int64_t position = 0;
  assert(normalized_position(2, effective, position));
  assert(position == 200);
  CostModel minimum;
  minimum.minimum_per_order = 10000;
  std::int64_t fee = 0;
  assert(commission(minimum, false, 1, 200, fee));
  assert(fee == 10000);
  CostModel native;
  native.per_order_unit = 5;
  assert(commission(native, false, 3, 0, fee));
  assert(fee == 15);
  assert(quote_currency_allowed(true, true));
  assert(!quote_currency_allowed(false, true));
  std::cout << "tradefi normalize tests passed\n";
  return 0;
}
