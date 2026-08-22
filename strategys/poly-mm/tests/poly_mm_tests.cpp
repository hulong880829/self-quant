#include <algorithm>
#include <array>
#include <cmath>
#include <cstring>
#include <cstdlib>
#include <iostream>
#include <string_view>

#include "polymm/catalog_rollover.h"
#include "polymm/market_window.h"
#include "polymm/pnl_ledger.h"
#include "polymm/risk.h"
#include "polymm/signal.h"

namespace {

void Require(bool condition, std::string_view message) {
  if (!condition) {
    std::cerr << "require failed: " << message << '\n';
    std::abort();
  }
}

strategyframe::TradeId Trade(std::string_view text) {
  strategyframe::TradeId result;
  result.length = static_cast<std::uint16_t>(text.size());
  std::copy(text.begin(), text.end(), result.value);
  return result;
}

void TestWindow() {
  Require(polymm::current_window_start(301) == 300, "window floor");
  Require(polymm::btc_five_minute_slug(300) ==
              "btc-updown-5m-300",
          "window slug");
  Require(polymm::remaining_ns(301, 300'000'000'000ULL) ==
              1'000'000'000ULL,
          "window remaining");
}

void TestSignal() {
  polymm::Parameters parameters;
  parameters.vol_lookback_ms = 3'000;
  parameters.signal_horizon_ms = 1'000;
  parameters.vol_threshold = 0.0;
  parameters.min_market_price = 0.1;
  parameters.max_market_price = 0.9;
  parameters.min_edge_ticks = 0.0;
  polymm::SignalEngine signal(parameters, 16);
  Require(signal.update({100.0, 0.0, 1'000'000'000ULL,
                         1'000'000'000ULL}),
          "first fair price");
  Require(signal.update({100.1, 0.0, 2'000'000'000ULL,
                         2'000'000'000ULL}),
          "second fair price");
  Require(signal.update({100.3, 0.0, 3'000'000'000ULL,
                         3'000'000'000ULL}),
          "third fair price");
  Require(signal.update({100.6, 0.0, 4'000'000'000ULL,
                         4'000'000'000ULL}),
          "fourth fair price");
  const auto evaluation =
      signal.evaluate(0.5, 0.0, 0.01, 60'000'000'000ULL);
  Require(evaluation.volatility_ready, "volatility ready");
  Require(evaluation.direction ==
              polymm::SignalEvaluation::Direction::Up,
          "up signal");
}

void TestPnl() {
  polymm::PnlLedger ledger(7, 10, 11);
  const strategyframe::OrderToken buy{1, 1, 1};
  const strategyframe::OrderToken sell{1, 1, 2};
  Require(ledger.remember_order(buy, 10, strategyframe::Side::Buy,
                                polymm::OrderPurpose::Open, 1),
          "remember buy");
  strategyframe::ExecutionUpdate fill;
  fill.kind = strategyframe::ExecutionUpdate::Kind::Fill;
  fill.account_id = 7;
  fill.instrument_id = 10;
  fill.token = buy;
  fill.trade_id = Trade("buy");
  fill.fill_quantity = {2, 0, {}};
  fill.fill_price = {40, 2, {}};
  Require(ledger.apply_fill(fill), "apply buy");
  Require(ledger.remember_order(sell, 10, strategyframe::Side::Sell,
                                polymm::OrderPurpose::Close, 1),
          "remember sell");
  fill.token = sell;
  fill.trade_id = Trade("sell");
  fill.fill_quantity = {1, 0, {}};
  fill.fill_price = {60, 2, {}};
  Require(ledger.apply_fill(fill), "apply sell");
  const auto view = ledger.view(10);
  Require(std::abs(view.quantity - 1.0) < 1e-12,
          "remaining quantity");
  Require(std::abs(view.average_cost - 0.4) < 1e-12,
          "average cost");
  Require(std::abs(view.realized - 0.2) < 1e-12,
          "realized pnl");
  Require(!ledger.apply_fill(fill), "deduplicate fill");
  Require(ledger.duplicate_fills() == 1, "duplicate metric");
  fill.trade_id = Trade("sell-rest");
  fill.fill_quantity = {1, 0, {}};
  Require(ledger.apply_fill(fill), "flatten old window");
  Require(ledger.flat(), "ledger is flat");
  Require(ledger.activate_window(20, 21), "activate physical window");
  Require(ledger.quantity(10) == 0.0 && ledger.quantity(20) == 0.0,
          "old window archived");
}

void TestRisk() {
  Require(polymm::allows_open(4.0, 0.0, 1.0, 5.0, 0.1),
          "position below limit");
  Require(!polymm::allows_open(5.0, 0.0, 1.0, 5.0, 0.1),
          "same-direction limit");
  Require(polymm::allows_open(5.0, 10.0, 1.0, 5.0, 0.1),
          "balancing exception");
  polymm::PnlView position;
  position.quantity = 2.0;
  position.unrealized_exit = -0.11;
  Require(polymm::stop_loss_triggered(position, 0.01, 5.0),
          "stop loss");
  Require(polymm::near_expiry(10'000'000ULL, 10),
          "expiry boundary");
}

strategyframe::InstrumentCatalogInfo Catalog(
    strategyframe::InstrumentId instrument_id, std::uint32_t generation,
    std::uint64_t expiry, std::string_view symbol) {
  strategyframe::InstrumentCatalogInfo result;
  result.instrument_id = instrument_id;
  result.venue = strategyframe::Venue::Polymarket;
  result.product = strategyframe::ProductType::BinaryOption;
  result.generation = generation;
  result.expiry_time_ns = expiry;
  std::memcpy(result.canonical_symbol.value, symbol.data(), symbol.size());
  result.canonical_symbol.length =
      static_cast<std::uint16_t>(symbol.size());
  return result;
}

void TestCatalogRollover() {
  constexpr strategyframe::InstrumentId up_id = 0x1'0000'0011ULL;
  constexpr strategyframe::InstrumentId down_id = 0x1'0000'0022ULL;
  const auto current_up =
      Catalog(up_id, 7, 300'000'000'000ULL, "btc5mup");
  const auto next_up =
      Catalog(up_id + 1, 1, 600'000'000'000ULL, "btc5mup");
  const auto next_down =
      Catalog(down_id + 1, 1, 600'000'000'000ULL, "btc5mdown");
  Require(polymm::is_next_physical_catalog(current_up, next_up),
          "new physical catalog");
  Require(!polymm::is_next_physical_catalog(current_up, current_up),
          "duplicate physical catalog ignored");
  auto stale_expiry = next_up;
  stale_expiry.expiry_time_ns = current_up.expiry_time_ns;
  Require(!polymm::is_next_physical_catalog(current_up, stale_expiry),
          "physical catalog cannot reuse expiry");

  std::array pair{next_up, next_down};
  Require(polymm::catalog_rollover_ready(pair),
          "matching catalog pair rolls over");
  pair[1].generation = 9;
  Require(polymm::catalog_rollover_ready(pair),
          "generation is only a book recovery epoch");
  pair[1] = next_down;
  pair[1].expiry_time_ns += 1;
  Require(!polymm::catalog_rollover_ready(pair),
          "mixed expiries do not roll over");
}

}  // namespace

int main() {
  TestWindow();
  TestSignal();
  TestPnl();
  TestRisk();
  TestCatalogRollover();
  return 0;
}
