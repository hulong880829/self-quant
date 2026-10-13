#include "mds/ibkr/ibkr_bbo_assembler.h"

#include <cmath>
#include <cstdint>
#include <limits>
#include <stdexcept>
#include <string>
#include <string_view>

namespace {

void check(bool condition, std::string_view message) {
  if (!condition) {
    throw std::runtime_error(std::string(message));
  }
}

void publish_book(mds::ibkr::BboAssembler &assembler, std::uint64_t generation) {
  assembler.on_market_data_type(mds::ibkr::MarketDataKind::Live, generation);
  assembler.on_price(true, 190.5, generation);
  assembler.on_price(false, 190.625, generation);
  assembler.on_size(true, "100", generation);
  assembler.on_size(false, "200.5", generation);
}

}  // namespace

int main() {
  using mds::ibkr::BboAssembler;
  using mds::ibkr::MarketDataKind;

  std::int64_t fixed = 0;
  check(mds::ibkr::double_price_to_fixed(190.5, 4, fixed) && fixed == 1905000,
        "190.5 did not convert through to_chars at scale 4");
  check(!mds::ibkr::double_price_to_fixed(0.0, 4, fixed),
        "zero price was accepted");
  check(!mds::ibkr::double_price_to_fixed(-1.0, 4, fixed),
        "negative price was accepted");
  check(!mds::ibkr::double_price_to_fixed(
            std::numeric_limits<double>::quiet_NaN(), 4, fixed),
        "NaN price was accepted");
  check(!mds::ibkr::double_price_to_fixed(
            std::numeric_limits<double>::infinity(), 4, fixed),
        "infinite price was accepted");

  std::uint8_t tick_scale = 0;
  check(mds::ibkr::price_scale_for_tick(0.01, tick_scale) && tick_scale == 2,
        "0.01 did not derive scale 2");
  check(mds::ibkr::price_scale_for_tick(0.001, tick_scale) && tick_scale == 3,
        "0.001 did not derive scale 3");
  check(mds::ibkr::price_scale_for_tick(0.00001, tick_scale) && tick_scale == 5,
        "0.00001 did not derive scale 5");
  check(mds::ibkr::price_scale_for_tick(0.00005, tick_scale) && tick_scale == 5,
        "0.00005 did not derive scale 5");
  check(mds::ibkr::price_scale_for_tick(0.25, tick_scale) && tick_scale == 2,
        "0.25 did not derive scale 2");
  check(mds::ibkr::min_tick_fits_scale(0.01, 4) &&
            mds::ibkr::min_tick_fits_scale(0.001, 4) &&
            mds::ibkr::min_tick_fits_scale(0.25, 4),
        "coarse ticks were rejected at scale 4");
  check(!mds::ibkr::min_tick_fits_scale(0.00001, 4) &&
            !mds::ibkr::min_tick_fits_scale(0.00005, 4),
        "five-decimal FX ticks were accepted at scale 4");

  bool non_positive = false;
  check(mds::ibkr::decimal_size_to_fixed("100", 4, fixed, non_positive) &&
            !non_positive && fixed == 1000000,
        "integer size did not convert at scale 4");
  check(mds::ibkr::decimal_size_to_fixed("1.23456", 4, fixed, non_positive) &&
            !non_positive && fixed == 12346,
        "decimal size was not rounded before decimal_to_fixed");
  check(mds::ibkr::decimal_size_to_fixed("0.00005", 4, fixed, non_positive) &&
            !non_positive && fixed == 1,
        "tiny positive size was not rounded up from the decimal text");
  check(mds::ibkr::decimal_size_to_fixed("0", 4, fixed, non_positive) &&
            non_positive,
        "zero size was not classified as non-positive");
  check(mds::ibkr::decimal_size_to_fixed("0.00004", 4, fixed, non_positive) &&
            non_positive,
        "size that rounds to zero was not classified as non-positive");
  check(!mds::ibkr::decimal_size_to_fixed("nope", 4, fixed, non_positive),
        "non-decimal size was accepted");
  check(mds::ibkr::decimal_size_to_fixed("+100E+0", 4, fixed, non_positive) &&
            !non_positive && fixed == 1000000,
        "+100E+0 did not convert at scale 4");
  check(mds::ibkr::decimal_size_to_fixed("+1.23456E+0", 4, fixed, non_positive) &&
            !non_positive && fixed == 12346,
        "+1.23456E+0 was not rounded at scale 4");
  check(mds::ibkr::decimal_size_to_fixed("+5E-5", 4, fixed, non_positive) &&
            !non_positive && fixed == 1,
        "+5E-5 was not rounded up at scale 4");
  check(mds::ibkr::decimal_size_to_fixed("+1e+0", 4, fixed, non_positive) &&
            !non_positive && fixed == 10000,
        "lowercase exponent did not convert at scale 4");
  check(mds::ibkr::decimal_size_to_fixed("+1.25E+2", 4, fixed, non_positive) &&
            !non_positive && fixed == 1250000,
        "positive exponent did not shift the decimal point right");
  check(mds::ibkr::decimal_size_to_fixed("+1.25E-2", 4, fixed, non_positive) &&
            !non_positive && fixed == 125,
        "negative exponent did not shift the decimal point left");
  check(mds::ibkr::decimal_size_to_fixed("+0E+0", 4, fixed, non_positive) &&
            non_positive,
        "+0E+0 was not classified as non-positive");
  check(mds::ibkr::decimal_size_to_fixed("-1E+0", 4, fixed, non_positive) &&
            non_positive,
        "-1E+0 was not classified as non-positive");
  check(!mds::ibkr::decimal_size_to_fixed("+1E", 4, fixed, non_positive),
        "exponent without digits was accepted");
  check(!mds::ibkr::decimal_size_to_fixed("+1E+", 4, fixed, non_positive),
        "exponent sign without digits was accepted");
  check(!mds::ibkr::decimal_size_to_fixed("+1E999", 4, fixed, non_positive),
        "overflowing exponent was accepted");

  BboAssembler assembler(4, 4);
  publish_book(assembler, 1);
  check(assembler.publishable() && !assembler.unhealthy(),
        "complete live BBO was not publishable");
  const auto quote = assembler.quote();
  check(quote.bid_price == 1905000 && quote.ask_price == 1906250 &&
            quote.bid_size == 1000000 && quote.ask_size == 2005000,
        "assembled fixed-point quote did not match the tick order");

  const auto invalid_before = assembler.invalid_ticks();
  auto cleared = assembler.on_size(true, "0", 1);
  check(!cleared.publishable && !assembler.has_bid_size() &&
            assembler.has_bid_price() && assembler.has_ask_price() &&
            assembler.has_ask_size() &&
            assembler.invalid_ticks() == invalid_before,
        "zero size did not clear only that side");
  check(!assembler.on_size(true, "100", 1).publishable ||
            assembler.publishable(),
        "restoring bid size should publish again");
  check(assembler.publishable(), "book did not recover after a zero size");

  const auto before_negative = assembler.invalid_ticks();
  check(!assembler.on_price(true, -1.0, 1).publishable &&
            !assembler.has_bid_price() &&
            assembler.invalid_ticks() == before_negative,
        "negative price did not clear the side without an invalid count");
  check(!assembler.on_price(false, std::numeric_limits<double>::quiet_NaN(), 1)
             .publishable &&
            assembler.invalid_ticks() == before_negative + 1,
        "NaN price did not increment invalid ticks");
  check(!assembler.on_price(false, std::numeric_limits<double>::infinity(), 1)
             .publishable &&
            assembler.invalid_ticks() == before_negative + 2,
        "infinite price did not increment invalid ticks");

  assembler.reset(1);
  publish_book(assembler, 1);
  assembler.on_price(true, 191.0, 1);
  assembler.on_price(false, 190.0, 1);
  check(!assembler.publishable(), "crossed market was publishable");

  assembler.reset(1);
  assembler.on_market_data_type(MarketDataKind::Live, 1);
  assembler.on_price(true, 10.0, 1);
  assembler.on_size(true, "1", 1);
  check(!assembler.publishable() && assembler.has_bid_price() &&
            !assembler.has_ask_price(),
        "one-sided book was publishable");

  assembler.reset(1);
  publish_book(assembler, 1);
  const auto non_live_before = assembler.non_live_ticks();
  check(!assembler.on_market_data_type(MarketDataKind::Delayed, 1).publishable &&
            assembler.unhealthy() &&
            assembler.non_live_ticks() == non_live_before + 1 &&
            assembler.market_data_kind() == MarketDataKind::Delayed,
        "delayed market data stayed publishable");
  check(!assembler.on_market_data_type(MarketDataKind::Frozen, 1).publishable &&
            assembler.non_live_ticks() == non_live_before + 2,
        "frozen market data stayed publishable");
  check(!assembler.on_market_data_type(MarketDataKind::DelayedFrozen, 1)
             .publishable,
        "delayed-frozen market data stayed publishable");

  assembler.reset(7);
  const auto ignored = assembler.on_price(true, 10.0, 6);
  check(!ignored.changed && !ignored.publishable && !assembler.has_bid_price(),
        "a stale generation updated the assembler");
  check(assembler.on_price(true, 10.0, 7).changed && assembler.has_bid_price(),
        "the current generation was ignored");

  const auto bad_size = assembler.invalid_ticks();
  check(!assembler.on_size(true, "nope", 7).publishable &&
            assembler.invalid_ticks() == bad_size + 1,
        "unparseable size did not increment invalid ticks");

  BboAssembler scientific(4, 4);
  const auto scientific_invalid = scientific.invalid_ticks();
  scientific.on_market_data_type(MarketDataKind::Live, 1);
  scientific.on_price(true, 190.5, 1);
  scientific.on_price(false, 190.625, 1);
  scientific.on_size(true, "+100E+0", 1);
  scientific.on_size(false, "+200E+0", 1);
  check(scientific.has_bid_size() && scientific.has_ask_size() &&
            scientific.publishable() &&
            scientific.invalid_ticks() == scientific_invalid,
        "scientific bid and ask sizes did not publish a live BBO");
  const auto scientific_quote = scientific.quote();
  check(scientific_quote.bid_size == 1000000 &&
            scientific_quote.ask_size == 2000000,
        "scientific sizes did not convert to fixed quantities");
  return 0;
}
