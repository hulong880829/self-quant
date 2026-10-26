#include <algorithm>
#include <array>
#include <cmath>
#include <cstdio>
#include <cstring>
#include <cstdlib>
#include <iostream>
#include <string>
#include <string_view>

#include "polymm/fairprice_client.h"
#include "polymm/gamma_resolution.h"
#include "polymm/halt_policy.h"
#include "polymm/market_window.h"
#include "polymm/order_lifecycle.h"
#include "polymm/pnl_ledger.h"
#include "polymm/risk.h"
#include "polymm/rolling_pair_binding.h"
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
                         1'000'000'000ULL}) == polymm::FairUpdate::Accepted,
          "first fair price");
  Require(signal.update({100.1, 0.0, 2'000'000'000ULL,
                         2'000'000'000ULL}) == polymm::FairUpdate::Accepted,
          "second fair price");
  Require(signal.update({100.3, 0.0, 3'000'000'000ULL,
                         3'000'000'000ULL}) == polymm::FairUpdate::Accepted,
          "third fair price");
  Require(signal.update({100.6, 0.0, 4'000'000'000ULL,
                         4'000'000'000ULL}) == polymm::FairUpdate::Accepted,
          "fourth fair price");
  Require(signal.update({100.6, 0.0, 4'000'000'000ULL, 9'000'000'000ULL}) ==
              polymm::FairUpdate::Duplicate,
          "duplicate wall does not advance source time");
  Require(signal.latest_wall_ns() == 4'000'000'000ULL, "source time unchanged");
  Require(signal.update({100.6, 0.0, 3'000'000'000ULL, 9'000'000'000ULL}) ==
              polymm::FairUpdate::OutOfOrder,
          "older wall is discarded");
  Require(signal.update({0.0, 0.0, 5'000'000'000ULL, 5'000'000'000ULL}) ==
              polymm::FairUpdate::Invalid,
          "nonpositive price is invalid");
  Require(polymm::fairprice_gates_fresh(10'000'000'000ULL, 9'000'000'000ULL,
                                        9'000'000'000ULL),
          "both gates fresh");
  Require(!polymm::fairprice_gates_fresh(10'000'000'000ULL, 9'000'000'000ULL,
                                         7'000'000'000ULL),
          "stale source blocks opening");
  Require(!polymm::fairprice_gates_fresh(10'000'000'000ULL, 7'000'000'000ULL,
                                         9'000'000'000ULL),
          "stale receive blocks opening");
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
  position.average_cost = 0.50;
  Require(polymm::stop_loss_triggered(position, 0.44, 0.01, 5.0, 0.0),
          "stop loss");
  Require(!polymm::stop_loss_triggered(position, 0.46, 0.01, 5.0, 0.0),
          "smaller net loss stays open");
  Require(polymm::allows_open(0.0, 0.0, 5.0, 5.0, 0.1), "empty side buys 5");
  Require(!polymm::allows_open(5.0, 0.0, 5.0, 5.0, 0.1),
          "same leg cannot add another 5");
  Require(polymm::allows_open(0.0, 5.0, 5.0, 5.0, 0.1),
          "opposite leg can still buy 5");
  Require(!polymm::allows_open(5.0, 5.0, 5.0, 5.0, 0.1),
          "full opposite leg stays capped");
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
  result.tick_size = 1;
  result.lot_size = 5;
  result.price_scale = 2;
  result.quantity_scale = 0;
  std::memcpy(result.canonical_symbol.value, symbol.data(), symbol.size());
  result.canonical_symbol.length =
      static_cast<std::uint16_t>(symbol.size());
  return result;
}

void TestRollingPairBinding() {
  strategyframe::InstrumentSymbol up_symbol{};
  strategyframe::InstrumentSymbol down_symbol{};
  std::memcpy(up_symbol.value, "btc5mup", 7);
  up_symbol.length = 7;
  std::memcpy(down_symbol.value, "btc5mdown", 9);
  down_symbol.length = 9;
  constexpr std::uint64_t kWindowNs = 300'000'000'000ULL;
  polymm::RollingPairBinding binding({
      strategyframe::Venue::Polymarket,
      strategyframe::ProductType::BinaryOption, up_symbol, down_symbol,
      kWindowNs});
  const auto current_up =
      Catalog(11, 1, 300'000'000'000ULL, "btc5mup");
  const auto current_down =
      Catalog(12, 1, 300'000'000'000ULL, "btc5mdown");
  const auto next_up =
      Catalog(21, 1, 600'000'000'000ULL, "btc5mup");
  const auto next_down =
      Catalog(22, 1, 600'000'000'000ULL, "btc5mdown");
  Require(binding.on_catalog(current_up, 1) == polymm::CatalogAdmit::Cached,
          "cache up");
  Require(binding.dirty(), "catalog sets dirty");
  Require(!binding.active().valid, "catalog does not activate");
  Require(binding.on_catalog(current_down, 1) == polymm::CatalogAdmit::Cached,
          "cache down");
  const auto not_ready = binding.propose_live_window(
      1, [](strategyframe::InstrumentId) { return false; });
  Require(!not_ready.valid, "not ready does not propose");
  Require(binding.dirty(), "not ready keeps dirty");
  const auto ready = binding.propose_live_window(
      1, [](strategyframe::InstrumentId) { return true; });
  Require(ready.valid && ready.up.instrument_id == 11, "ready proposes live");
  binding.commit_activation(ready);
  Require(binding.active().valid, "commit activates");
  Require(!binding.dirty(), "commit clears dirty");
  Require(binding.on_catalog(next_up, 1) == polymm::CatalogAdmit::Cached,
          "cache next up");
  Require(binding.on_catalog(next_down, 1) == polymm::CatalogAdmit::Cached,
          "cache next down");
  Require(binding.dirty(), "next catalog dirties");
  Require(binding.active().valid, "current stays valid before expiry");
  Require(!binding.propose_expiry(1), "not expired");
  Require(binding.propose_expiry(300'000'000'000ULL), "expired");
  const auto kept = binding.active();
  Require(kept.valid, "binding does not clear active itself");
  binding.commit_deactivation();
  Require(!binding.active().valid, "commit_deactivation clears");
  Require(binding.dirty(), "deactivation dirties");

  auto rejected = current_up;
  rejected.lot_size = 0;
  Require(binding.on_catalog(rejected, 1) == polymm::CatalogAdmit::Rejected,
          "illegal catalog rejected");
}

strategyframe::TradeId TradeN(int value);

void TestPnlRetiredCapacity() {
  polymm::PnlLedger ledger(7);
  strategyframe::ExecutionUpdate fill;
  fill.kind = strategyframe::ExecutionUpdate::Kind::Fill;
  fill.account_id = 7;
  fill.fill_quantity = {1, 0, {}};
  fill.fill_price = {40, 2, {}};
  for (int round = 0; round < 17; ++round) {
    const strategyframe::InstrumentId up =
        100 + static_cast<strategyframe::InstrumentId>(round) * 2;
    const strategyframe::InstrumentId down = up + 1;
    Require(ledger.commit_active_window(up, down), "commit window");
    const strategyframe::OrderToken buy{
        1, 1, static_cast<std::uint64_t>(round + 1)};
    Require(ledger.remember_order(buy, up, strategyframe::Side::Buy,
                                  polymm::OrderPurpose::Open, 1),
            "remember capacity buy");
    fill.token = buy;
    fill.instrument_id = up;
    fill.trade_id = TradeN(round + 1);
    Require(ledger.apply_fill(fill), "capacity fill");
  }
  Require(!ledger.commit_active_window(200, 201), "retired rows full");
  Require(ledger.quantity(100 + 16 * 2) == 1.0, "old active retained");
  Require(!ledger.can_archive_active(), "archive blocked");
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
              polymm::ReconcileVerdict::Reject,
          "timeout without reconcile rejects");

  strategyframe::OmsStatusUpdate status;
  status.kind = strategyframe::OmsStatusUpdate::Kind::ReconcileComplete;
  polymm::note_oms_reconcile(state, status);
  Require(polymm::reconcile_verdict(state, 5) ==
              polymm::ReconcileVerdict::Ready,
          "reconcile complete empty");

  strategyframe::OrderView leftover{};
  leftover.account_id = 7;
  leftover.instrument_id = 10;
  leftover.remaining_quantity = {1, 0, {}};
  std::array<strategyframe::OrderView, 1> orders{leftover};
  polymm::inspect_local_account(state, orders, {}, 7, 10, 11);
  Require(state.saw_open_order, "historical order");
  Require(polymm::reconcile_verdict(state, 5) ==
              polymm::ReconcileVerdict::Reject,
          "current window order rejects reconcile");

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
          "current window position rejects reconcile");

  polymm::StartupReconcile window;
  strategyframe::PositionView old_window{};
  old_window.account_id = 7;
  old_window.instrument_id = 99;
  old_window.quantity = {3, 0, {}};
  std::array<strategyframe::PositionView, 1> old_positions{old_window};
  polymm::inspect_window_inventory(window, {}, old_positions, 7, 10, 11);
  Require(!window.saw_position, "old window does not block");
  polymm::inspect_window_inventory(window, {}, positions, 7, 10, 11);
  Require(window.saw_position, "current window inventory blocks start");

  polymm::StartupReconcile failed;
  strategyframe::OmsStatusUpdate error_status;
  error_status.kind = strategyframe::OmsStatusUpdate::Kind::ReconcileComplete;
  error_status.error = 13;
  polymm::note_oms_reconcile(failed, error_status);
  Require(polymm::reconcile_verdict(failed, 1) ==
              polymm::ReconcileVerdict::Reject,
          "query error reject");

  strategyframe::OmsStatusUpdate connecting{};
  connecting.kind = strategyframe::OmsStatusUpdate::Kind::VenueStatus;
  connecting.adapter_status = 1;
  connecting.error = 0;
  Require(polymm::classify_startup_oms_status(connecting) ==
              polymm::StartupOmsClass::Continue,
          "connecting continue");

  strategyframe::OmsStatusUpdate transient{};
  transient.kind = strategyframe::OmsStatusUpdate::Kind::VenueStatus;
  transient.adapter_status = polymm::kAdapterReconnecting;
  transient.error = polymm::kOmsNotReady;
  Require(polymm::classify_startup_oms_status(transient) ==
              polymm::StartupOmsClass::Transient,
          "reconnecting not-ready transient");

  strategyframe::OmsStatusUpdate reconnecting_other{};
  reconnecting_other.kind = strategyframe::OmsStatusUpdate::Kind::VenueStatus;
  reconnecting_other.adapter_status = polymm::kAdapterReconnecting;
  reconnecting_other.error = 1;
  Require(polymm::classify_startup_oms_status(reconnecting_other) ==
              polymm::StartupOmsClass::Fatal,
          "reconnecting other error fatal");

  strategyframe::OmsStatusUpdate connecting_not_ready{};
  connecting_not_ready.kind = strategyframe::OmsStatusUpdate::Kind::VenueStatus;
  connecting_not_ready.adapter_status = 1;
  connecting_not_ready.error = polymm::kOmsNotReady;
  Require(polymm::classify_startup_oms_status(connecting_not_ready) ==
              polymm::StartupOmsClass::Fatal,
          "connecting not-ready fatal");

  strategyframe::OmsStatusUpdate ready_not_ready{};
  ready_not_ready.kind = strategyframe::OmsStatusUpdate::Kind::VenueStatus;
  ready_not_ready.adapter_status = 3;
  ready_not_ready.error = polymm::kOmsNotReady;
  Require(polymm::classify_startup_oms_status(ready_not_ready) ==
              polymm::StartupOmsClass::Fatal,
          "ready not-ready fatal");

  strategyframe::OmsStatusUpdate failed_status{};
  failed_status.kind = strategyframe::OmsStatusUpdate::Kind::VenueStatus;
  failed_status.adapter_status = polymm::kAdapterFailed;
  failed_status.error = polymm::kOmsNotReady;
  Require(polymm::classify_startup_oms_status(failed_status) ==
              polymm::StartupOmsClass::Fatal,
          "failed not-ready fatal");

  polymm::StartupReconcile sequence;
  polymm::note_oms_reconcile(sequence, connecting);
  Require(polymm::classify_startup_oms_status(transient) ==
              polymm::StartupOmsClass::Transient,
          "sequence reconnecting stays transient");
  Require(!sequence.oms_reconcile_failed, "transient did not fail reconcile");
  strategyframe::OmsStatusUpdate complete{};
  complete.kind = strategyframe::OmsStatusUpdate::Kind::ReconcileComplete;
  complete.error = 0;
  polymm::note_oms_reconcile(sequence, complete);
  strategyframe::OmsStatusUpdate ready{};
  ready.kind = strategyframe::OmsStatusUpdate::Kind::VenueStatus;
  ready.adapter_status = 3;
  ready.error = 0;
  Require(polymm::classify_startup_oms_status(ready) ==
              polymm::StartupOmsClass::Continue,
          "ready continue");
  Require(!sequence.oms_reconcile_failed, "sequence stayed healthy");
  Require(polymm::reconcile_verdict(sequence, 1) ==
              polymm::ReconcileVerdict::Ready,
          "sequence ready");

  polymm::StartupReconcile reject;
  reject.oms_reconcile_failed = true;
  Require(polymm::reconcile_verdict(reject, 1) ==
              polymm::ReconcileVerdict::Reject,
          "failed status reject");
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
  Require(polymm::resume_after_fairprice(polymm::HaltPhase::StopOpening,
                                         true) ==
              polymm::HaltPhase::Running,
          "fairprice pause resumes opening");
  Require(polymm::resume_after_fairprice(polymm::HaltPhase::StopOpening,
                                         false) ==
              polymm::HaltPhase::StopOpening,
          "non-fairprice stop remains stopped");
  Require(polymm::resume_after_fairprice(polymm::HaltPhase::Flattening,
                                         true) ==
              polymm::HaltPhase::Flattening,
          "fairprice cannot cancel flattening");
  Require(polymm::resume_after_fairprice(polymm::HaltPhase::Halted, true) ==
              polymm::HaltPhase::Halted,
          "fairprice cannot resume halted strategy");
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

void TestExpiryRelease() {
  polymm::SubscribeGate gate;
  Require(gate.should_send(true, true), "idle may subscribe");
  gate.mark_sent(1'000, 1'000);
  Require(!gate.should_send(true, true), "pending ack does not resend");
  Require(!gate.timed_out(1'000), "ack wait has not expired");
  Require(gate.timed_out(1'000 + 1'000'000'000ULL), "ack deadline expires");
  polymm::SubscribeGate acked;
  acked.mark_sent(1, 1'000);
  Require(!acked.note_ack(true), "ok subscribe does not disconnect");
  Require(acked.phase == polymm::SubscribePhase::Acked, "subscribe acked");
  Require(!acked.should_send(true, true), "acked does not resend");

  Require(polymm::cancel_on_stop_opening(polymm::OrderPurpose::Open,
                                         polymm::OrderState::PendingOpen),
          "stop opening cancels open");
  Require(!polymm::cancel_on_stop_opening(polymm::OrderPurpose::Close,
                                          polymm::OrderState::PendingClose),
          "stop opening keeps close");
  Require(polymm::classify_open_order_query(true, true, false) ==
              polymm::OpenOrderQueryResult::EmptyUnknown,
          "empty open orders are not a fill");
  Require(polymm::classify_open_order_query(true, true, true) ==
              polymm::OpenOrderQueryResult::StillOpen,
          "visible open order keeps canceling");

  strategyframe::ConditionId up;
  strategyframe::ConditionId down;
  const char* market = "0xabc";
  up.length = 5;
  down.length = 5;
  std::memcpy(up.value, market, 5);
  std::memcpy(down.value, market, 5);
  Require(polymm::same_market_condition(up, down), "same condition");
  down.value[0] = '1';
  Require(!polymm::same_market_condition(up, down), "different condition");

  polymm::PnlLedger ledger(7);
  Require(ledger.commit_active_window(10, 11), "window");
  const strategyframe::OrderToken buy{1, 1, 1};
  const strategyframe::OrderToken sell{1, 1, 2};
  Require(ledger.remember_order(buy, 10, strategyframe::Side::Buy,
                                polymm::OrderPurpose::Open, 1),
          "remember release buy");
  strategyframe::ExecutionUpdate fill;
  fill.kind = strategyframe::ExecutionUpdate::Kind::Fill;
  fill.account_id = 7;
  fill.instrument_id = 10;
  fill.token = buy;
  fill.trade_id = Trade("rel-buy");
  fill.fill_quantity = {2, 0, {}};
  fill.fill_price = {40, 2, {}};
  Require(ledger.apply_fill(fill), "release buy");
  Require(ledger.remember_order(sell, 10, strategyframe::Side::Sell,
                                polymm::OrderPurpose::Close, 1),
          "remember release sell");
  fill.token = sell;
  fill.trade_id = Trade("rel-sell");
  fill.fill_quantity = {1, 0, {}};
  fill.fill_price = {60, 2, {}};
  Require(ledger.apply_fill(fill), "release sell");
  ledger.clear_terminal_order(buy);
  ledger.clear_terminal_order(sell);
  Require(ledger.archive_active(), "park rows");
  const auto up_stored = ledger.stored(10);
  const auto down_stored = ledger.stored(11);
  Require(up_stored.present && std::abs(up_stored.quantity - 1.0) < 1e-12,
          "remaining quantity is logged");
  Require(std::abs(up_stored.cost - 0.4) < 1e-12, "remaining cost is logged");
  Require(std::abs(up_stored.realized - 0.2) < 1e-12, "realized is logged");
  Require(!ledger.release_expired_window(10, 11), "nonzero release rejected");
  Require(ledger.has_instrument(10), "nonzero row remains");
  Require(std::abs(ledger.archived_realized()) < 1e-12,
          "rejected release does not archive");
  const strategyframe::OrderToken sell_rest{1, 1, 3};
  Require(ledger.remember_order(sell_rest, 10, strategyframe::Side::Sell,
                                polymm::OrderPurpose::Close, 1),
          "remember flatten");
  fill.token = sell_rest;
  fill.trade_id = Trade("rel-flat");
  fill.fill_quantity = {1, 0, {}};
  fill.fill_price = {60, 2, {}};
  Require(ledger.apply_fill(fill), "flatten remainder");
  Require(ledger.release_expired_window(10, 11), "release");
  Require(std::abs(ledger.archived_realized() - 0.4) < 1e-12,
          "realized is kept");
  Require(!ledger.has_instrument(10) && !ledger.has_instrument(11),
          "rows cleared");
  Require(ledger.release_expired_window(10, 11), "release is idempotent");
  Require(std::abs(ledger.archived_realized() - 0.4) < 1e-12,
          "second release does not add realized");
  fill.trade_id = Trade("late-released");
  fill.token = buy;
  Require(!ledger.apply_fill(fill), "deleted row does not apply");
  Require(ledger.remember_late_fill(7, 10, fill.trade_id), "first late fill");
  Require(!ledger.remember_late_fill(7, 10, fill.trade_id),
          "duplicate late fill");
  Require(!ledger.has_instrument(10), "late fill does not recreate a row");

  double archived = 0.0;
  for (int round = 0; round < 1000; ++round) {
    const strategyframe::InstrumentId up_id =
        1'000 + static_cast<strategyframe::InstrumentId>(round) * 2;
    const strategyframe::InstrumentId down_id = up_id + 1;
    Require(ledger.commit_active_window(up_id, down_id), "rolling window");
    Require(ledger.release_expired_window(up_id, down_id), "rolling release");
    archived = ledger.archived_realized();
  }
  Require(std::abs(archived - 0.4) < 1e-12, "flat windows add no loss");
  Require(!ledger.has_instrument(1000) && !ledger.retired_nonzero(),
          "released windows do not accumulate");
}

void TestResolutionPnl() {
  const auto buy = [](polymm::PnlLedger& ledger,
                      strategyframe::InstrumentId instrument,
                      strategyframe::OrderToken token, std::int64_t quantity,
                      std::int64_t price) {
    Require(ledger.remember_order(token, instrument, strategyframe::Side::Buy,
                                  polymm::OrderPurpose::Open, 1),
            "remember settlement order");
    strategyframe::ExecutionUpdate fill;
    fill.kind = strategyframe::ExecutionUpdate::Kind::Fill;
    fill.account_id = 7;
    fill.instrument_id = instrument;
    fill.token = token;
    fill.trade_id = Trade("settle");
    fill.trade_id.value[6] = static_cast<char>('0' + token.sequence);
    fill.trade_id.length = 7;
    fill.fill_quantity = {quantity, 0, {}};
    fill.fill_price = {price, 2, {}};
    Require(ledger.apply_fill(fill), "settlement fill");
    ledger.clear_terminal_order(token);
  };
  const auto expect_settlement = [](polymm::PnlLedger& ledger,
                                    strategyframe::InstrumentId up,
                                    strategyframe::InstrumentId down,
                                    polymm::Outcome winner, double payout_up,
                                    double payout_down) {
    const auto up_row = ledger.stored(up);
    const auto down_row = ledger.stored(down);
    const double expected = up_row.realized + down_row.realized + payout_up -
                            up_row.cost + payout_down - down_row.cost;
    const double before = ledger.archived_realized();
    Require(ledger.settle_expired_window(up, down, winner), "settle");
    Require(std::abs(ledger.last_window_pnl() - expected) < 1e-9,
            "window pnl");
    Require(std::abs(ledger.archived_realized() - (before + expected)) < 1e-9,
            "archived settlement");
    Require(ledger.settle_expired_window(up, down, winner), "settle once");
    Require(std::abs(ledger.archived_realized() - (before + expected)) < 1e-9,
            "settlement is idempotent");
  };

  polymm::PnlLedger up_wins(7);
  Require(up_wins.commit_active_window(10, 11), "up window");
  buy(up_wins, 10, {1, 1, 1}, 5, 40);
  Require(up_wins.archive_active(), "park up");
  expect_settlement(up_wins, 10, 11, polymm::Outcome::Up, 5.0, 0.0);
  Require(std::abs(up_wins.last_window_pnl() - 3.0) < 1e-9, "up wins +3");

  polymm::PnlLedger down_held(7);
  Require(down_held.commit_active_window(12, 13), "down held window");
  buy(down_held, 13, {1, 1, 2}, 5, 60);
  Require(down_held.archive_active(), "park down held");
  expect_settlement(down_held, 12, 13, polymm::Outcome::Up, 0.0, 0.0);
  Require(std::abs(down_held.last_window_pnl() - (-3.0)) < 1e-9,
          "down held while up wins is -3");

  polymm::PnlLedger down_wins(7);
  Require(down_wins.commit_active_window(20, 21), "down window");
  buy(down_wins, 21, {1, 1, 3}, 5, 60);
  Require(down_wins.archive_active(), "park down");
  expect_settlement(down_wins, 20, 21, polymm::Outcome::Down, 0.0, 5.0);
  Require(std::abs(down_wins.last_window_pnl() - 2.0) < 1e-9,
          "down wins pays 5 against cost 3");

  polymm::PnlLedger both(7);
  Require(both.commit_active_window(30, 31), "both window");
  buy(both, 30, {1, 1, 4}, 5, 40);
  buy(both, 31, {1, 1, 5}, 5, 60);
  Require(both.archive_active(), "park both");
  expect_settlement(both, 30, 31, polymm::Outcome::Up, 5.0, 0.0);
  Require(std::abs(both.last_window_pnl() - 0.0) < 1e-9,
          "both legs net to zero when up wins");

  polymm::PnlLedger partial(7);
  Require(partial.commit_active_window(32, 33), "partial window");
  buy(partial, 32, {1, 1, 6}, 5, 40);
  Require(partial.remember_order({1, 1, 7}, 32, strategyframe::Side::Sell,
                                 polymm::OrderPurpose::Close, 1),
          "remember partial sell");
  strategyframe::ExecutionUpdate sold;
  sold.kind = strategyframe::ExecutionUpdate::Kind::Fill;
  sold.account_id = 7;
  sold.instrument_id = 32;
  sold.token = {1, 1, 7};
  sold.trade_id = Trade("partial");
  sold.fill_quantity = {2, 0, {}};
  sold.fill_price = {50, 2, {}};
  Require(partial.apply_fill(sold), "partial close");
  partial.clear_terminal_order({1, 1, 7});
  Require(partial.archive_active(), "park partial");
  expect_settlement(partial, 32, 33, polymm::Outcome::Up, 3.0, 0.0);

  polymm::PnlLedger flat(7);
  Require(flat.commit_active_window(40, 41), "flat window");
  Require(flat.archive_active(), "park flat");
  expect_settlement(flat, 40, 41, polymm::Outcome::Up, 0.0, 0.0);

  for (int round = 0; round < 1000; ++round) {
    const strategyframe::InstrumentId up =
        2'000 + static_cast<strategyframe::InstrumentId>(round) * 2;
    Require(flat.commit_active_window(up, up + 1), "capacity window");
    Require(flat.settle_expired_window(up, up + 1, polymm::Outcome::Down),
            "capacity settle");
  }
  Require(!flat.retired_nonzero(), "settled windows stay bounded");

  const auto condition = [](std::string_view text) {
    strategyframe::ConditionId id{};
    std::memcpy(id.value, text.data(), text.size());
    id.length = static_cast<std::uint16_t>(text.size());
    return id;
  };
  const auto market = [](std::string_view id, std::string_view outcomes,
                         std::string_view prices, bool closed,
                         std::string_view status) {
    std::string body = "[{\"conditionId\":\"";
    body += id;
    body += "\",\"closed\":";
    body += closed ? "true" : "false";
    body += ",\"umaResolutionStatus\":\"";
    body += status;
    body += "\",\"outcomes\":\"";
    body += outcomes;
    body += "\",\"outcomePrices\":\"";
    body += prices;
    body += "\"}]";
    return body;
  };
  const auto expected = condition("0xabc");
  const auto up = polymm::parse_gamma_resolution(
      market("0xabc", R"([\"Up\", \"Down\"])", R"([\"1\", \"0\"])", true,
             "resolved"),
      expected);
  Require(up.status == polymm::GammaResolutionStatus::Resolved &&
              up.winner == polymm::Outcome::Up,
          "up index wins");
  const auto reversed = polymm::parse_gamma_resolution(
      market("0xabc", R"([\"Down\", \"Up\"])", R"([\"1\", \"0\"])", true,
             "resolved"),
      expected);
  Require(reversed.status == polymm::GammaResolutionStatus::Resolved &&
              reversed.winner == polymm::Outcome::Down,
          "reversed outcomes are not assumed up");
  const auto yes_no = polymm::parse_gamma_resolution(
      market("0xabc", R"([\"Yes\", \"No\"])", R"([\"1\", \"0\"])", true,
             "resolved"),
      expected);
  Require(yes_no.status == polymm::GammaResolutionStatus::InvalidResponse,
          "yes no is not a winner");
  const auto duplicate = polymm::parse_gamma_resolution(
      market("0xabc", R"([\"Up\", \"Up\"])", R"([\"1\", \"0\"])", true,
             "resolved"),
      expected);
  Require(duplicate.status == polymm::GammaResolutionStatus::InvalidResponse,
          "duplicate outcome");
  const auto short_prices = polymm::parse_gamma_resolution(
      market("0xabc", R"([\"Up\", \"Down\"])", R"([\"1\"])", true, "resolved"),
      expected);
  Require(short_prices.status == polymm::GammaResolutionStatus::InvalidResponse,
          "price length mismatch");
  const auto near = polymm::parse_gamma_resolution(
      market("0xabc", R"([\"Up\", \"Down\"])", R"([\"0.005\", \"0.995\"])",
             true, "resolved"),
      expected);
  Require(near.status == polymm::GammaResolutionStatus::InvalidResponse,
          "near prices are not payouts");
  const auto proposed = polymm::parse_gamma_resolution(
      market("0xabc", R"([\"Up\", \"Down\"])", R"([\"0.005\", \"0.995\"])",
             true, "proposed"),
      expected);
  Require(proposed.status == polymm::GammaResolutionStatus::Pending,
          "unresolved market stays pending");
  const auto wrong = polymm::parse_gamma_resolution(
      market("0xother", R"([\"Up\", \"Down\"])", R"([\"1\", \"0\"])", true,
             "resolved"),
      expected);
  Require(wrong.status == polymm::GammaResolutionStatus::InvalidResponse,
          "wrong condition");
  const auto empty = polymm::parse_gamma_resolution("[]", expected);
  Require(empty.status == polymm::GammaResolutionStatus::Pending,
          "empty gamma list is pending");
  const auto many = polymm::parse_gamma_resolution(
      "[{\"conditionId\":\"0xabc\",\"closed\":true,\"umaResolutionStatus\":\"resolved\"},{\"conditionId\":\"0xabc\",\"closed\":true,\"umaResolutionStatus\":\"resolved\"}]",
      expected);
  Require(many.status == polymm::GammaResolutionStatus::InvalidResponse,
          "multiple markets");
  char target[160];
  std::size_t target_length = 0;
  Require(polymm::format_gamma_markets_target(expected, target, sizeof(target),
                                              target_length),
          "gamma target");
  const auto target_text = std::string_view(target, target_length);
  Require(target_text.find("&closed=any&limit=1") != std::string_view::npos,
          "closed any query");
  Require(polymm::kGammaHttpTimeoutMs == 5'000, "gamma timeout is 5s");
  Require(polymm::resolution_backoff_ns(0) == 2'000'000'000ULL, "backoff 2s");
  Require(polymm::resolution_backoff_ns(1) == 5'000'000'000ULL, "backoff 5s");
  Require(polymm::resolution_backoff_ns(2) == 10'000'000'000ULL, "backoff 10s");
  Require(polymm::resolution_backoff_ns(3) == 30'000'000'000ULL, "backoff 30s");
  Require(polymm::resolution_backoff_ns(9) == 30'000'000'000ULL, "backoff cap");
  Require(polymm::resolution_disposition(7, expected, 8, expected) ==
              polymm::ResolutionDisposition::DropLate,
          "late request id is dropped");
  strategyframe::ConditionId other = condition("0xdef");
  Require(polymm::resolution_disposition(7, expected, 7, other) ==
              polymm::ResolutionDisposition::FailActive,
          "condition mismatch fails the active request");
  Require(polymm::resolution_disposition(7, expected, 7, expected) ==
              polymm::ResolutionDisposition::Apply,
          "matching result applies");
  Require(!polymm::resolution_http_allowed(true, 0, false),
          "held result blocks the next http request");
  Require(!polymm::resolution_http_allowed(false, 1, false),
          "unconsumed result blocks the next http request");
  Require(polymm::resolution_http_allowed(false, 0, false),
          "idle gamma client can start");
  polymm::SidecarConfig sidecar_config;
  sidecar_config.fairprice_service = "1";
  polymm::FairPriceClient sidecar(sidecar_config);
  polymm::ResolutionRequest request;
  request.condition_id = expected;
  for (std::uint64_t id = 1; id <= 16; ++id) {
    request.request_id = id;
    Require(sidecar.try_push_resolution(request), "resolution queue accepts");
  }
  request.request_id = 17;
  Require(!sidecar.try_push_resolution(request),
          "full resolution queue does not become inflight");
  Require(polymm::kMaxRetiredWindows == 16, "sixteen retired slots");
}

void TestExecutableEdge() {
  polymm::Parameters parameters;
  parameters.vol_lookback_ms = 3'000;
  parameters.signal_horizon_ms = 1'000;
  parameters.vol_threshold = 0.0;
  parameters.min_market_price = 0.1;
  parameters.max_market_price = 0.9;
  parameters.min_edge_ticks = 0.0;
  parameters.polymarket_taker_fee_rate = 0.0;
  parameters.order_shares = 5.0;
  parameters.fairprice_timer_us = 1'000;
  Require(polymm::signal_sample_capacity(parameters) >= 16, "capacity");
  polymm::SignalEngine signal(parameters, 16);
  Require(signal.update({100.0, 0.0, 1'000'000'000ULL, 1'000'000'000ULL}) ==
              polymm::FairUpdate::Accepted,
          "fp1");
  Require(signal.update({100.1, 0.0, 2'000'000'000ULL, 2'000'000'000ULL}) ==
              polymm::FairUpdate::Accepted,
          "fp2");
  Require(signal.update({100.3, 0.0, 3'000'000'000ULL, 3'000'000'000ULL}) ==
              polymm::FairUpdate::Accepted,
          "fp3");
  Require(signal.update({100.6, 0.0, 4'000'000'000ULL, 4'000'000'000ULL}) ==
              polymm::FairUpdate::Accepted,
          "fp4");
  const auto evaluation = signal.evaluate_executable(
      0.499, 0.50, 0.499, 0.50, 0.01, 60'000'000'000ULL);
  Require(evaluation.volatility_ready, "executable vol");
  Require(evaluation.direction == polymm::SignalEvaluation::Direction::Up,
          "executable up ask");
}

polymm::SignalEvaluation ExecutableEdge(double fee_rate, double min_edge) {
  polymm::Parameters parameters;
  parameters.vol_lookback_ms = 3'000;
  parameters.signal_horizon_ms = 1'000;
  parameters.vol_threshold = 0.0;
  parameters.min_market_price = 0.1;
  parameters.max_market_price = 0.9;
  parameters.min_edge_ticks = min_edge;
  parameters.polymarket_taker_fee_rate = fee_rate;
  parameters.order_shares = 5.0;
  parameters.slippage_ticks = 1.0;
  parameters.fairprice_timer_us = 1'000;
  polymm::SignalEngine signal(parameters, 16);
  Require(signal.update({100.0, 0.0, 1'000'000'000ULL, 1'000'000'000ULL}) ==
              polymm::FairUpdate::Accepted,
          "edge fp1");
  Require(signal.update({100.1, 0.0, 2'000'000'000ULL, 2'000'000'000ULL}) ==
              polymm::FairUpdate::Accepted,
          "edge fp2");
  Require(signal.update({100.3, 0.0, 3'000'000'000ULL, 3'000'000'000ULL}) ==
              polymm::FairUpdate::Accepted,
          "edge fp3");
  Require(signal.update({100.6, 0.0, 4'000'000'000ULL, 4'000'000'000ULL}) ==
              polymm::FairUpdate::Accepted,
          "edge fp4");
  return signal.evaluate_executable(0.499, 0.50, 0.499, 0.50, 0.01,
                                    60'000'000'000ULL);
}

void TestFeeEdgeAndPnl() {
  const double mid = polymm::polymarket_taker_fee(0.5, 5.0, 0.07);
  const double low = polymm::polymarket_taker_fee(0.2, 5.0, 0.07);
  const double high = polymm::polymarket_taker_fee(0.8, 5.0, 0.07);
  Require(std::abs(mid - 0.0875) < 1e-12, "mid fee");
  Require(std::abs(low - high) < 1e-12, "fee symmetry");
  Require(std::abs(low - 0.056) < 1e-12, "wing fee");
  const double rounded = polymm::polymarket_taker_fee(0.004, 5.0, 0.07);
  const double raised = polymm::polymarket_taker_fee(
      0.004, 5.0, 0.07, polymm::FeeRounding::Up);
  Require(std::abs(rounded - 0.00139) < 1e-12, "fee rounds to 5 decimals");
  Require(std::abs(raised - 0.00140) < 1e-12, "signal fee rounds up");
  Require(raised > rounded, "ceil is stricter than nearest");
  Require(polymm::polymarket_taker_fee(0.0, 5.0, 0.07) == 0.0, "bad price");
  Require(polymm::polymarket_taker_fee(1.0, 5.0, 0.07) == 0.0, "price one");
  Require(!polymm::order_terms_ok(0.0, 5.0, 0.01, 0.07), "reject bad price");
  Require(!polymm::order_terms_ok(0.5, 0.0, 0.01, 0.07), "reject bad quantity");
  Require(!polymm::order_terms_ok(0.5, 5.0, 0.0, 0.07), "reject bad tick");
  Require(!polymm::order_terms_ok(0.5, 5.0, 0.01, 1.1), "reject bad fee rate");

  const auto gross = ExecutableEdge(0.0, 0.0);
  Require(gross.direction == polymm::SignalEvaluation::Direction::Up,
          "zero fee can open");
  Require(gross.edge_ticks > 4.0, "gross edge exceeds four ticks");
  const auto short_edge = ExecutableEdge(0.07, gross.edge_ticks);
  Require(short_edge.direction == polymm::SignalEvaluation::Direction::None,
          "edge below spread fee slippage and min edge does not open");
  Require(short_edge.entry_fee_ticks > 0.0, "entry fee is visible");
  Require(std::abs(short_edge.required_ticks -
                   (short_edge.spread_ticks + short_edge.entry_fee_ticks +
                    short_edge.slippage_ticks + gross.edge_ticks)) < 1e-9,
          "required ticks add the cost");
  const auto covered = ExecutableEdge(0.07, 0.0);
  Require(covered.edge_ticks >= covered.required_ticks, "covered edge");
  Require(covered.direction == polymm::SignalEvaluation::Direction::Up,
          "covered edge opens");

  polymm::PnlLedger ledger(7, 10, 11);
  ledger.set_fee_rate(0.07);
  const strategyframe::OrderToken buy{1, 1, 1};
  const strategyframe::OrderToken maker_sell{1, 1, 2};
  const strategyframe::OrderToken taker_sell{1, 1, 3};
  Require(ledger.remember_order(buy, 10, strategyframe::Side::Buy,
                                polymm::OrderPurpose::Open, 1, true),
          "taker buy");
  strategyframe::ExecutionUpdate fill;
  fill.kind = strategyframe::ExecutionUpdate::Kind::Fill;
  fill.account_id = 7;
  fill.instrument_id = 10;
  fill.token = buy;
  fill.trade_id = Trade("fee-buy");
  fill.fill_quantity = {5, 0, {}};
  fill.fill_price = {50, 2, {}};
  Require(ledger.apply_fill(fill), "apply taker buy");
  const auto bought = ledger.stored(10);
  const double entry_fee = polymm::polymarket_taker_fee(0.5, 5.0, 0.07);
  Require(std::abs(bought.cost - (2.5 + entry_fee)) < 1e-9, "fee in cost");
  Require(std::abs(bought.entry_fee_basis - entry_fee) < 1e-9, "fee basis");
  Require(std::abs(bought.gross_realized - bought.realized_fees -
                   bought.realized) < 1e-9,
          "buy identity");
  Require(std::abs(bought.total_estimated_fees - bought.realized_fees -
                   bought.entry_fee_basis) < 1e-9,
          "buy fee split");

  Require(ledger.remember_order(maker_sell, 10, strategyframe::Side::Sell,
                                polymm::OrderPurpose::Close, 1, false),
          "maker sell");
  fill.token = maker_sell;
  fill.trade_id = Trade("fee-maker");
  fill.fill_quantity = {2, 0, {}};
  fill.fill_price = {60, 2, {}};
  Require(ledger.apply_fill(fill), "partial maker sell");
  const auto partial = ledger.stored(10);
  Require(std::abs(partial.quantity - 3.0) < 1e-9, "three shares remain");
  Require(partial.entry_fee_basis > 0.0, "open fee stays with the remainder");
  Require(std::abs(partial.gross_realized - partial.realized_fees -
                   partial.realized) < 1e-9,
          "partial identity");
  Require(std::abs(partial.total_estimated_fees - partial.realized_fees -
                   partial.entry_fee_basis) < 1e-9,
          "partial fee split");

  Require(!ledger.release_expired_window(10, 11), "open inventory is not released");
  Require(ledger.has_instrument(10), "row remains after rejected release");

  Require(ledger.remember_order(taker_sell, 10, strategyframe::Side::Sell,
                                polymm::OrderPurpose::ForceFlatten, 1, true),
          "taker flatten");
  fill.token = taker_sell;
  fill.trade_id = Trade("fee-ioc");
  fill.fill_quantity = {3, 0, {}};
  fill.fill_price = {55, 2, {}};
  Require(ledger.apply_fill(fill), "taker flatten fill");
  fill.trade_id = Trade("fee-dup");
  Require(!ledger.apply_fill(fill), "duplicate taker fill");
  const auto flat = ledger.stored(10);
  Require(std::abs(flat.quantity) < 1e-9, "flat after taker sell");
  Require(std::abs(flat.entry_fee_basis) < 1e-9, "entry fee fully allocated");
  Require(std::abs(flat.gross_realized - flat.realized_fees - flat.realized) <
              1e-9,
          "flat identity");
  Require(std::abs(flat.total_estimated_fees - flat.realized_fees) < 1e-9,
          "flat fees are realized");
  Require(ledger.release_expired_window(10, 11), "flat release");
  Require(std::abs(ledger.archived_gross() - ledger.archived_realized_fees() -
                   ledger.archived_net()) < 1e-9,
          "archived flat identity");
  Require(std::abs(ledger.archived_total_fees() -
                   ledger.archived_realized_fees()) < 1e-9,
          "archived flat fees");

  polymm::PnlLedger settled(7, 12, 13);
  settled.set_fee_rate(0.07);
  Require(settled.remember_order(buy, 12, strategyframe::Side::Buy,
                                 polymm::OrderPurpose::Open, 1, true),
          "settlement buy");
  fill.instrument_id = 12;
  fill.token = buy;
  fill.trade_id = Trade("fee-settle");
  fill.fill_quantity = {5, 0, {}};
  fill.fill_price = {50, 2, {}};
  Require(settled.apply_fill(fill), "settlement taker buy");
  Require(settled.settle_expired_window(12, 13, polymm::Outcome::Up),
          "settle remaining inventory");
  Require(std::abs(settled.archived_gross() - settled.archived_realized_fees() -
                   settled.archived_net()) < 1e-9,
          "settled identity");
  Require(std::abs(settled.archived_total_fees() -
                   settled.archived_realized_fees()) < 1e-9,
          "settled fees include the open basis");
  Require(!settled.has_instrument(12), "settled row cleared");

  const double target = polymm::close_target_price(0.50, 0.01, 4);
  Require(polymm::select_close_liquidity(target, 5.0, target, 0.07) ==
              polymm::CloseLiquidity::PostOnly,
          "gross bid does not pay the exit fee");
  const double exit_per = polymm::polymarket_taker_fee_per_share(
      0.70, 5.0, 0.07, polymm::FeeRounding::Up);
  Require(polymm::select_close_liquidity(0.70, 5.0, target, 0.07) ==
              polymm::CloseLiquidity::Ioc,
          "bid covering the exit fee can take");
  Require(0.70 - exit_per >= target, "covered example really clears the fee");

  polymm::SignalHorizonTracker tracker;
  const std::uint64_t signal_ns = 1'000'000'000ULL;
  Require(tracker.record(signal_ns, 1, 10, polymm::SignalEvaluation::Direction::Up,
                         0.50, 0.49, 0.51, 8.0, 6.0, 0.01, 0.01, 0.07, 5.0),
          "record horizon");
  tracker.sample(signal_ns + 60'000'000ULL, 10, 1, 0.56, 0.57,
                 signal_ns + 55'000'000ULL);
  Require(tracker.summary().horizons[0].valid == 0, "100ms waits for its target");
  tracker.sample(signal_ns + 100'000'000ULL, 10, 1, 0.56, 0.57,
                 signal_ns + 95'000'000ULL);
  Require(tracker.summary().horizons[0].valid == 1, "100ms accepts a quote 5ms early");
  Require(tracker.summary().horizons[0].mark_to_bid_sum > 0.0, "up move is positive");
  tracker.sample(signal_ns + 200'000'000ULL, 10, 1, 0.56, 0.57,
                 signal_ns + 190'000'000ULL);
  tracker.sample(signal_ns + 500'000'000ULL, 10, 1, 0.40, 0.41,
                 signal_ns + 500'000'000ULL);
  Require(tracker.summary().horizons[1].valid == 1, "200ms completed on time");
  Require(tracker.summary().horizons[1].missing == 0, "500ms quote does not fill 200ms");
  tracker.sample(signal_ns + 1'000'000'000ULL, 10, 1, 0.40, 0.41,
                 signal_ns + 1'000'000'000ULL);
  Require(tracker.summary().completed == 1, "all horizons complete one sample");
  Require(tracker.inflight() == 0, "completed sample leaves the slot");

  polymm::SignalHorizonTracker late;
  Require(late.record(signal_ns, 1, 10, polymm::SignalEvaluation::Direction::Down,
                      0.50, 0.49, 0.51, 8.0, 6.0, 0.0, 0.01, 0.0, 5.0),
          "late record");
  late.sample(signal_ns + 600'000'000ULL, 10, 1, 0.20, 0.21,
              signal_ns + 500'000'000ULL);
  Require(late.summary().horizons[0].missing == 1, "100ms is missing");
  Require(late.summary().horizons[1].missing == 1, "200ms is missing");
  Require(late.summary().horizons[2].valid == 1, "500ms quote fills only 500ms");

  polymm::SignalHorizonTracker full;
  for (std::uint32_t index = 0; index < polymm::kSignalHorizonSlots; ++index) {
    Require(full.record(signal_ns, 1, 10, polymm::SignalEvaluation::Direction::Up,
                        0.5, 0.49, 0.51, 1.0, 1.0, 0.0, 0.01, 0.0, 5.0),
            "fill inflight slots");
  }
  Require(!full.record(signal_ns, 1, 10, polymm::SignalEvaluation::Direction::Up,
                       0.5, 0.49, 0.51, 1.0, 1.0, 0.0, 0.01, 0.0, 5.0),
          "inflight overflow drops");
  Require(full.summary().dropped == 1, "dropped counter");
  full.discard_generation(1);
  Require(full.inflight() == 0, "generation discard frees slots");
  Require(full.summary().horizons[0].missing == 0,
          "discard does not invent missing horizons");
  Require(full.record(signal_ns, 2, 11, polymm::SignalEvaluation::Direction::Up,
                      0.5, 0.49, 0.51, 1.0, 1.0, 0.0, 0.01, 0.0, 5.0),
          "next generation records");
  full.sample(signal_ns + 100'000'000ULL, 10, 1, 0.9, 0.91,
              signal_ns + 100'000'000ULL);
  Require(full.summary().horizons[0].valid == 0,
          "old instrument does not complete the new sample");
}

}  // namespace

int main() {
  TestWindow();
  TestSignal();
  TestPnl();
  TestPnlSlots();
  TestPnlRetiredCapacity();
  TestRisk();
  TestRollingPairBinding();
  TestLifecycle();
  TestCommandResults();
  TestStartupReconcile();
  TestDustAndHalt();
  TestBboActivityKeepsBooksReady();
  TestExpiryRelease();
  TestResolutionPnl();
  TestExecutableEdge();
  TestFeeEdgeAndPnl();
  return 0;
}
