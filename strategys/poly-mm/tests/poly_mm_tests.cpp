#include <algorithm>
#include <array>
#include <cmath>
#include <cstdio>
#include <cstring>
#include <cstdlib>
#include <iostream>
#include <string_view>

#include "polymm/catalog_rollover.h"
#include "polymm/halt_policy.h"
#include "polymm/market_window.h"
#include "polymm/order_lifecycle.h"
#include "polymm/pnl_ledger.h"
#include "polymm/risk.h"
#include "polymm/signal.h"
#include "polymm/startup_reconcile.h"

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

strategyframe::TradeId TradeN(int value) {
  char text[16];
  const int written =
      std::snprintf(text, sizeof(text), "t%d", value);
  Require(written > 0, "trade id format");
  return Trade(std::string_view(
      text, static_cast<std::size_t>(written)));
}

void TestPnlSlots() {
  polymm::PnlLedger ledger(7, 10, 11);
  for (int round = 1; round <= 1000; ++round) {
    const strategyframe::OrderToken buy{
        1, 1, static_cast<std::uint64_t>(round * 2)};
    const strategyframe::OrderToken sell{
        1, 1, static_cast<std::uint64_t>(round * 2 + 1)};
    Require(ledger.remember_order(buy, 10, strategyframe::Side::Buy,
                                  polymm::OrderPurpose::Open, 1),
            "remember buy slot");
    strategyframe::ExecutionUpdate fill;
    fill.kind = strategyframe::ExecutionUpdate::Kind::Fill;
    fill.account_id = 7;
    fill.instrument_id = 10;
    fill.token = buy;
    fill.trade_id = TradeN(round * 2);
    fill.fill_quantity = {5, 0, {}};
    fill.fill_price = {40, 2, {}};
    Require(ledger.apply_fill(fill), "buy fill slot");
    ledger.clear_terminal_order(buy);
    Require(ledger.remember_order(sell, 10, strategyframe::Side::Sell,
                                  polymm::OrderPurpose::Close, 1),
            "remember sell slot");
    fill.token = sell;
    fill.trade_id = TradeN(round * 2 + 1);
    fill.fill_price = {50, 2, {}};
    Require(ledger.apply_fill(fill), "sell fill slot");
    ledger.clear_terminal_order(sell);
    Require(ledger.used_order_slots() == 0, "live slots released");
  }
  Require(ledger.flat(), "1000 rounds flatten");

  const strategyframe::OrderToken late{1, 1, 9'001};
  Require(ledger.remember_order(late, 10, strategyframe::Side::Buy,
                                polymm::OrderPurpose::Open, 1),
          "late remember");
  strategyframe::ExecutionUpdate fill;
  fill.kind = strategyframe::ExecutionUpdate::Kind::Fill;
  fill.account_id = 7;
  fill.instrument_id = 10;
  fill.token = late;
  fill.trade_id = Trade("late-a");
  fill.fill_quantity = {5, 0, {}};
  fill.fill_price = {40, 2, {}};
  Require(ledger.apply_fill(fill), "first late fill");
  Require(!ledger.apply_fill(fill), "duplicate after live");
  Require(ledger.duplicate_fills() >= 1, "duplicate counted");
  ledger.clear_terminal_order(late);
  Require(ledger.used_order_slots() == 0, "cleared live slot");
  fill.trade_id = Trade("late-b");
  Require(ledger.apply_fill(fill), "tombstone late fill");
  const strategyframe::OrderToken next{1, 1, 9'002};
  Require(ledger.remember_order(next, 10, strategyframe::Side::Buy,
                                polymm::OrderPurpose::Open, 1),
          "reuse live slot");
  Require(ledger.used_order_slots() == 1, "new token occupies live slot");
}

void TestLifecycle() {
  polymm::LegOrder leg;
  leg.state = polymm::OrderState::PendingOpen;
  leg.purpose = polymm::OrderPurpose::Open;
  leg.token = {1, 1, 1};
  strategyframe::ExecutionUpdate fill;
  fill.kind = strategyframe::ExecutionUpdate::Kind::Fill;
  fill.status = strategyframe::OrderStatus::Filled;
  fill.token = leg.token;
  fill.remaining_quantity = {0, 0, {}};
  auto result =
      polymm::apply_order_event(leg, fill, 5.0, 5.0);
  Require(result.kind == polymm::LifecycleKind::GoIdle, "full fill idle");
  Require(result.release_ledger, "full fill releases");
  polymm::commit_lifecycle(leg, result);
  Require(leg.state == polymm::OrderState::Idle, "committed idle");
  Require(leg.token.sequence == 0, "token cleared");

  leg.state = polymm::OrderState::PendingOpen;
  leg.purpose = polymm::OrderPurpose::Open;
  leg.token = {1, 1, 2};
  fill.status = strategyframe::OrderStatus::PartiallyFilled;
  fill.token = leg.token;
  fill.remaining_quantity = {3, 0, {}};
  result = polymm::apply_order_event(leg, fill, 2.0, 5.0);
  Require(result.kind == polymm::LifecycleKind::KeepPending,
          "partial stays pending");
  polymm::commit_lifecycle(leg, result);
  Require(leg.saw_fill, "partial marked fill");
  strategyframe::ExecutionUpdate canceled;
  canceled.kind = strategyframe::ExecutionUpdate::Kind::Order;
  canceled.status = strategyframe::OrderStatus::Canceled;
  canceled.token = leg.token;
  result = polymm::apply_order_event(leg, canceled, 2.0, 5.0);
  Require(result.kind == polymm::LifecycleKind::GoDust, "partial cancel dust");
  polymm::commit_lifecycle(leg, result);
  Require(leg.state == polymm::OrderState::Dust, "dust state");
  result = polymm::apply_order_event(leg, canceled, 2.0, 5.0);
  Require(result.kind == polymm::LifecycleKind::Ignore, "duplicate terminal");

  polymm::LegOrder rejected;
  rejected.state = polymm::OrderState::PendingOpen;
  rejected.purpose = polymm::OrderPurpose::Open;
  rejected.token = {1, 1, 3};
  strategyframe::ExecutionUpdate reject;
  reject.kind = strategyframe::ExecutionUpdate::Kind::Order;
  reject.status = strategyframe::OrderStatus::Rejected;
  reject.token = rejected.token;
  result = polymm::apply_order_event(rejected, reject, 0.0, 5.0);
  Require(result.kind == polymm::LifecycleKind::GoIdle, "rejected idle");

  polymm::LegOrder canceling;
  canceling.state = polymm::OrderState::PendingCancel;
  canceling.purpose = polymm::OrderPurpose::Close;
  canceling.token = {1, 1, 4};
  strategyframe::ExecutionUpdate cancel_reject;
  cancel_reject.kind = strategyframe::ExecutionUpdate::Kind::Order;
  cancel_reject.update_type = polymm::kOrderCancelRejected;
  cancel_reject.status = strategyframe::OrderStatus::Open;
  cancel_reject.token = canceling.token;
  result = polymm::apply_order_event(canceling, cancel_reject, 5.0, 5.0);
  Require(result.kind == polymm::LifecycleKind::RestorePending,
          "cancel reject restores");
  Require(result.restore_state == polymm::OrderState::PendingClose,
          "restore close");
  polymm::commit_lifecycle(canceling, result);
  Require(canceling.state == polymm::OrderState::PendingClose,
          "pending close restored");
  Require(canceling.token.sequence == 4, "token kept on reject");
}

void TestCommandResults() {
  polymm::LegOrder leg;
  leg.state = polymm::OrderState::PendingOpen;
  leg.purpose = polymm::OrderPurpose::Open;
  leg.token = {1, 1, 8};
  strategyframe::ExecutionUpdate place;
  place.kind = strategyframe::ExecutionUpdate::Kind::CommandResult;
  place.update_type = polymm::kCommandPlace;
  place.token = leg.token;
  place.error = static_cast<std::int32_t>(strategyframe::Error::InvalidArgument);
  auto result = polymm::apply_order_event(leg, place, 0.0, 5.0);
  Require(result.kind == polymm::LifecycleKind::PlaceFailed, "place failed");
  Require(result.stop_opening, "place fail stops opening");
  Require(result.release_ledger, "failed place releases");
  polymm::commit_lifecycle(leg, result);
  Require(leg.state == polymm::OrderState::Idle, "failed place idle");

  polymm::LegOrder uncertain;
  uncertain.state = polymm::OrderState::PendingOpen;
  uncertain.purpose = polymm::OrderPurpose::Open;
  uncertain.token = {1, 1, 9};
  place.token = uncertain.token;
  place.error = static_cast<std::int32_t>(strategyframe::Error::OmsFailure);
  result = polymm::apply_order_event(uncertain, place, 0.0, 5.0);
  Require(result.kind == polymm::LifecycleKind::NeedsReconcile,
          "uncertain place");
  polymm::commit_lifecycle(uncertain, result);
  Require(uncertain.state == polymm::OrderState::NeedsReconcile,
          "needs reconcile");
  Require(!polymm::allows_close_submit(uncertain.state),
          "no close while uncertain");

  polymm::LegOrder canceling;
  canceling.state = polymm::OrderState::PendingCancel;
  canceling.purpose = polymm::OrderPurpose::Open;
  canceling.token = {1, 1, 10};
  strategyframe::ExecutionUpdate cancel;
  cancel.kind = strategyframe::ExecutionUpdate::Kind::CommandResult;
  cancel.update_type = polymm::kCommandCancel;
  cancel.token = canceling.token;
  cancel.error = static_cast<std::int32_t>(strategyframe::Error::NotFound);
  result = polymm::apply_order_event(canceling, cancel, 0.0, 5.0);
  Require(result.kind == polymm::LifecycleKind::RestorePending,
          "cancel command restore");
  Require(result.restore_state == polymm::OrderState::PendingOpen,
          "restore open");
}

void TestStartupReconcile() {
  polymm::StartupReconcile state;
  state.started_ns = 1;
  state.timeout_ns = 10;
  Require(polymm::reconcile_verdict(state, 5) ==
              polymm::ReconcileVerdict::Pending,
          "pending reconcile");
  Require(polymm::reconcile_verdict(state, 20) ==
              polymm::ReconcileVerdict::Ready,
          "empty timeout ready");

  strategyframe::OmsStatusUpdate status;
  status.kind = strategyframe::OmsStatusUpdate::Kind::ReconcileComplete;
  polymm::note_oms_reconcile(state, status);
  Require(polymm::reconcile_verdict(state, 5) ==
              polymm::ReconcileVerdict::Ready,
          "reconcile complete empty");

  strategyframe::OrderView leftover{};
  leftover.account_id = 7;
  leftover.remaining_quantity = {1, 0, {}};
  std::array<strategyframe::OrderView, 1> orders{leftover};
  polymm::inspect_local_account(state, orders, {}, 7, 10, 11);
  Require(state.saw_open_order, "historical order");
  Require(polymm::reconcile_verdict(state, 5) ==
              polymm::ReconcileVerdict::Reject,
          "open order reject");

  polymm::StartupReconcile positions_state;
  positions_state.oms_reconcile_seen = true;
  strategyframe::PositionView held{};
  held.account_id = 7;
  held.instrument_id = 10;
  held.quantity = {3, 0, {}};
  std::array<strategyframe::PositionView, 1> positions{held};
  polymm::inspect_local_account(positions_state, {}, positions, 7, 10, 11);
  Require(positions_state.saw_position, "historical position");
  Require(polymm::reconcile_verdict(positions_state, 5) ==
              polymm::ReconcileVerdict::Reject,
          "position reject");

  polymm::StartupReconcile failed;
  strategyframe::OmsStatusUpdate error_status;
  error_status.kind = strategyframe::OmsStatusUpdate::Kind::ReconcileComplete;
  error_status.error = 13;
  polymm::note_oms_reconcile(failed, error_status);
  Require(polymm::reconcile_verdict(failed, 1) ==
              polymm::ReconcileVerdict::Reject,
          "query error reject");
}

void TestDustAndHalt() {
  Require(polymm::below_minimum(4.0, 5.0), "dust quantity");
  Require(!polymm::below_minimum(5.0, 5.0), "min is tradable");
  Require(!polymm::allows_close_submit(polymm::OrderState::Dust),
          "dust cannot close");
  Require(!polymm::allows_new_open(polymm::OrderState::Dust),
          "dust cannot open");
  Require(polymm::allows_open(polymm::HaltPhase::Running), "running opens");
  Require(!polymm::allows_open(polymm::HaltPhase::StopOpening),
          "stop opening");
  Require(polymm::allows_close(polymm::HaltPhase::StopOpening),
          "stop opening still closes");
  Require(polymm::can_complete_halt(polymm::HaltPhase::Flattening, false,
                                    true),
          "flatten complete");
  Require(polymm::confirm_halt(polymm::HaltPhase::Flattening, false, true) ==
              polymm::HaltPhase::Halted,
          "confirm halt");
  Require(polymm::begin_stop_opening(polymm::HaltPhase::Running) ==
              polymm::HaltPhase::StopOpening,
          "enter stop opening");
  Require(polymm::book_tradable(0.4, 0.41), "sane book");
  Require(!polymm::book_tradable(0.41, 0.40), "crossed book");
  Require(polymm::bbo_fresh(2'000'000ULL, 1'000'000ULL, 2), "fresh bbo");
  Require(!polymm::bbo_fresh(5'000'000'000ULL, 1, 2), "stale bbo");
}

void TestBboActivityKeepsBooksReady() {
  constexpr std::uint32_t max_age_ms = 2'000;
  std::uint64_t last_up = 1'000'000'000ULL;
  std::uint64_t last_down = 1'000'000'000ULL;
  for (int sample = 0; sample < 5; ++sample) {
    last_up += 1'000'000'000ULL;
    last_down += 1'000'000'000ULL;
    const std::uint64_t now = last_up + 1'999'000'000ULL;
    Require(polymm::book_tradable(0.40, 0.41), "same-price book");
    Require(polymm::bbo_fresh(now, last_up, max_age_ms),
            "advancing up last_bbo stays fresh past max_bbo_age");
    Require(polymm::bbo_fresh(now, last_down, max_age_ms),
            "advancing down last_bbo stays fresh past max_bbo_age");
    Require(polymm::bbo_skew_ok(last_up, last_down, 500),
            "same-tick legs stay aligned");
  }
  const std::uint64_t frozen = last_up;
  Require(!polymm::bbo_fresh(frozen + 2'001'000'000ULL, frozen, max_age_ms),
          "swallowed callbacks make books_ready false");
}

void TestExecutableEdge() {
  polymm::Parameters parameters;
  parameters.vol_lookback_ms = 3'000;
  parameters.signal_horizon_ms = 1'000;
  parameters.vol_threshold = 0.0;
  parameters.min_market_price = 0.1;
  parameters.max_market_price = 0.9;
  parameters.min_edge_ticks = 0.0;
  parameters.fairprice_timer_us = 1'000;
  Require(polymm::signal_sample_capacity(parameters) >= 16, "capacity");
  polymm::SignalEngine signal(parameters, 16);
  Require(signal.update({100.0, 0.0, 1'000'000'000ULL, 1'000'000'000ULL}),
          "fp1");
  Require(signal.update({100.1, 0.0, 2'000'000'000ULL, 2'000'000'000ULL}),
          "fp2");
  Require(signal.update({100.3, 0.0, 3'000'000'000ULL, 3'000'000'000ULL}),
          "fp3");
  Require(signal.update({100.6, 0.0, 4'000'000'000ULL, 4'000'000'000ULL}),
          "fp4");
  const auto evaluation = signal.evaluate_executable(
      0.499, 0.50, 0.499, 0.50, 0.01, 60'000'000'000ULL);
  Require(evaluation.volatility_ready, "executable vol");
  Require(evaluation.direction == polymm::SignalEvaluation::Direction::Up,
          "executable up ask");
}

}  // namespace

int main() {
  TestWindow();
  TestSignal();
  TestPnl();
  TestPnlSlots();
  TestRisk();
  TestCatalogRollover();
  TestLifecycle();
  TestCommandResults();
  TestStartupReconcile();
  TestDustAndHalt();
  TestBboActivityKeepsBooksReady();
  TestExecutableEdge();
  return 0;
}
