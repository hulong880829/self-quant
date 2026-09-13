#include "polymm/strategy.h"

#include <algorithm>
#include <chrono>
#include <cmath>
#include <cstdlib>
#include <string>
#include <string_view>

#include "polymm/catalog_rollover.h"
#include "polymm/market_window.h"
#include "polymm/risk.h"

namespace polymm {
namespace {

std::uint64_t WallNowNs() noexcept {
  return static_cast<std::uint64_t>(
      std::chrono::duration_cast<std::chrono::nanoseconds>(
          std::chrono::system_clock::now().time_since_epoch())
          .count());
}

double Fixed(strategyframe::FixedPoint value) noexcept {
  double divisor = 1.0;
  for (std::uint8_t index = 0; index < value.scale; ++index) divisor *= 10.0;
  return static_cast<double>(value.value) / divisor;
}

}  // namespace

PolyMm::PolyMm(int* exit_code) noexcept : exit_code_(exit_code) {}

PolyMm::~PolyMm() {
  if (sidecar_) sidecar_->stop();
}

bool PolyMm::load_parameters() {
  const auto& params = context_->params();
  const auto account = params.require_int("account_id");
  if (!account || account.value <= 0) return false;
  parameters_.account_id = static_cast<std::uint32_t>(account.value);
  parameters_.vol_lookback_ms = static_cast<std::uint32_t>(
      params.optional_int("vol_lookback_ms", 30'000));
  parameters_.signal_horizon_ms = static_cast<std::uint32_t>(
      params.optional_int("signal_horizon_ms", 250));
  parameters_.vol_threshold =
      params.optional_double("vol_threshold", 0.20);
  parameters_.min_market_price =
      params.optional_double("min_market_price", 0.15);
  parameters_.max_market_price =
      params.optional_double("max_market_price", 0.85);
  parameters_.min_edge_ticks =
      params.optional_double("min_edge_ticks", 1.0);
  parameters_.order_shares =
      params.optional_double("order_shares", 1.0);
  parameters_.max_single_side_shares =
      params.optional_double("max_single_side_shares", 10.0);
  parameters_.imbalance_tolerance_shares =
      params.optional_double("imbalance_tolerance_shares", 0.1);
  parameters_.profit_spread_ticks =
      params.optional_int("profit_spread_ticks", 1);
  parameters_.close_improve_ticks =
      params.optional_int("close_improve_ticks", 0);
  parameters_.max_order_resting_ms = static_cast<std::uint32_t>(
      params.optional_int("max_order_resting_ms", 1'000));
  parameters_.requote_deviation_ticks =
      params.optional_int("requote_deviation_ticks", 1);
  parameters_.no_trade_before_expiry_ms = static_cast<std::uint32_t>(
      params.optional_int("no_trade_before_expiry_ms", 30'000));
  parameters_.stop_loss_ticks =
      params.optional_double("stop_loss_ticks", 5.0);
  parameters_.fairprice_timer_us = static_cast<std::uint32_t>(
      params.optional_int("fairprice_timer_us", 200));
  parameters_.minimum_order_size =
      params.optional_double("minimum_order_size", 5.0);
  parameters_.startup_reconcile_timeout_ms = static_cast<std::uint32_t>(
      params.optional_int("startup_reconcile_timeout_ms", 10'000));
  parameters_.max_bbo_age_ms = static_cast<std::uint32_t>(
      params.optional_int("max_bbo_age_ms", 2'000));
  parameters_.max_bbo_skew_ms = static_cast<std::uint32_t>(
      params.optional_int("max_bbo_skew_ms", 500));
  parameters_.taker_fee_bps =
      params.optional_double("taker_fee_bps", 0.0);
  parameters_.slippage_ticks =
      params.optional_double("slippage_ticks", 0.0);
  sidecar_config_.fairprice_host =
      params.optional_string("fairprice.host", "127.0.0.1");
  sidecar_config_.fairprice_service =
      params.optional_string("fairprice.service", "8080");
  sidecar_config_.fairprice_target =
      params.optional_string("fairprice.target", "/v1/stream");
  sidecar_config_.fairprice_origin =
      params.optional_string("fairprice.origin", "");
  sidecar_config_.fairprice_secure =
      params.optional_bool("fairprice.secure", true);
  sidecar_config_.fairprice_profile =
      params.optional_string("fairprice.profile", "");
  sidecar_config_.fairprice_symbol =
      params.optional_string("fairprice.symbol", "BTCUSDT");
  if (const char* token = std::getenv("AGGDATA_BROWSER_TOKEN"))
    sidecar_config_.fairprice_token = token;

  return parameters_.vol_lookback_ms > parameters_.signal_horizon_ms &&
         parameters_.signal_horizon_ms > 0 &&
         parameters_.vol_threshold >= 0.0 &&
         parameters_.min_market_price > 0.0 &&
         parameters_.max_market_price < 1.0 &&
         parameters_.min_market_price < parameters_.max_market_price &&
         parameters_.order_shares > 0.0 &&
         parameters_.max_single_side_shares > 0.0 &&
         parameters_.fairprice_timer_us > 0 &&
         parameters_.minimum_order_size > 0.0 &&
         parameters_.order_shares + 1e-9 >= parameters_.minimum_order_size &&
         parameters_.startup_reconcile_timeout_ms > 0 &&
         parameters_.max_bbo_age_ms > 0 &&
         parameters_.max_bbo_skew_ms > 0 &&
         parameters_.taker_fee_bps >= 0.0 &&
         parameters_.slippage_ticks >= 0.0;
}

void PolyMm::fatal_startup() noexcept {
  halt_phase_ = HaltPhase::Halted;
  phase_ = Phase::Halted;
  fairprice_live_ = false;
  if (exit_code_ != nullptr) *exit_code_ = 1;
  if (context_) context_->request_stop();
}

void PolyMm::begin_stop_opening() noexcept {
  halt_phase_ = polymm::begin_stop_opening(halt_phase_);
}

void PolyMm::begin_flattening() noexcept {
  halt_phase_ = polymm::begin_flattening(halt_phase_);
  cancel_all();
}

void PolyMm::enter_settlement_manual() noexcept {
  halt_phase_ = HaltPhase::Halted;
  phase_ = Phase::SettlementManual;
  fairprice_live_ = false;
  if (exit_code_ != nullptr) *exit_code_ = 1;
  if (context_) context_->request_stop();
}

bool PolyMm::has_active_orders() const noexcept {
  for (const Leg& value : legs_)
    if (is_active_order(value.order.state)) return true;
  if (context_ && !context_->open_orders().empty()) return true;
  return false;
}

bool PolyMm::position_safe() const noexcept {
  if (pnl_ == nullptr) return true;
  for (const Leg& value : legs_) {
    const double quantity = pnl_->quantity(value.instrument_id);
    if (quantity > 0.0 &&
        !below_minimum(quantity, parameters_.minimum_order_size) &&
        value.order.state != OrderState::Dust) {
      return false;
    }
  }
  return !has_active_orders();
}

void PolyMm::maybe_complete_halt() noexcept {
  if (confirm_halt(halt_phase_, has_active_orders(), position_safe()) !=
      HaltPhase::Halted) {
    return;
  }
  halt_phase_ = HaltPhase::Halted;
  if (phase_ != Phase::SettlementManual) phase_ = Phase::Halted;
  if (exit_code_ != nullptr && !pnl_->flat()) *exit_code_ = 1;
  if (context_) context_->request_stop();
}

void PolyMm::init(strategyframe::StrategyContext& context) {
  context_ = &context;
  if (!load_parameters()) {
    fatal_startup();
    return;
  }
  const auto up = context.find_instrument(
      strategyframe::Venue::Polymarket,
      strategyframe::ProductType::BinaryOption, "btc5mup");
  const auto down = context.find_instrument(
      strategyframe::Venue::Polymarket,
      strategyframe::ProductType::BinaryOption, "btc5mdown");
  if (!up || !down ||
      up.value.venue != strategyframe::Venue::Polymarket ||
      down.value.venue != strategyframe::Venue::Polymarket ||
      up.value.expiry_time_ns == 0 ||
      up.value.expiry_time_ns != down.value.expiry_time_ns ||
      up.value.lot_size <= 0 || down.value.lot_size <= 0) {
    fatal_startup();
    return;
  }
  const auto meets_minimum = [this](const auto& catalog) {
    double factor = 1.0;
    for (std::uint8_t index = 0; index < catalog.quantity_scale; ++index)
      factor *= 10.0;
    return parameters_.order_shares * factor + 1e-9 >=
           static_cast<double>(catalog.lot_size);
  };
  if (!meets_minimum(up.value) || !meets_minimum(down.value)) {
    fatal_startup();
    return;
  }
  parameters_.up_instrument_id = up.value.instrument_id;
  parameters_.down_instrument_id = down.value.instrument_id;
  catalogs_[0] = up.value;
  catalogs_[1] = down.value;
  legs_[0] = {Outcome::Up,
              up.value.instrument_id,
              up.value.price_scale,
              up.value.quantity_scale,
              up.value.tick_size,
              Fixed({up.value.tick_size, up.value.price_scale, {}})};
  legs_[1] = {Outcome::Down,
              down.value.instrument_id,
              down.value.price_scale,
              down.value.quantity_scale,
              down.value.tick_size,
              Fixed({down.value.tick_size, down.value.price_scale, {}})};
  signal_ = std::make_unique<SignalEngine>(
      parameters_, signal_sample_capacity(parameters_));
  pnl_ = std::make_unique<PnlLedger>(
      parameters_.account_id, parameters_.up_instrument_id,
      parameters_.down_instrument_id);
  sidecar_ = std::make_unique<FairPriceClient>(sidecar_config_);
  if (!sidecar_->start()) {
    fatal_startup();
    return;
  }
  const std::uint64_t interval =
      static_cast<std::uint64_t>(parameters_.fairprice_timer_us) * 1'000ULL;
  if (!context.schedule_timer(context.now_ns() + interval, interval)) {
    fatal_startup();
    return;
  }
  reconcile_ = {};
  reconcile_.started_ns = context.now_ns();
  reconcile_.timeout_ns =
      static_cast<std::uint64_t>(parameters_.startup_reconcile_timeout_ms) *
      1'000'000ULL;
  phase_ = Phase::WaitingReconcile;
}

void PolyMm::inspect_startup_account() {
  if (context_ == nullptr) return;
  inspect_local_account(reconcile_, context_->open_orders(),
                        context_->positions(), parameters_.account_id,
                        parameters_.up_instrument_id,
                        parameters_.down_instrument_id);
}

void PolyMm::finish_reconcile() {
  inspect_startup_account();
  const auto verdict = reconcile_verdict(reconcile_, context_->now_ns());
  if (verdict == ReconcileVerdict::Pending) return;
  if (verdict == ReconcileVerdict::Reject) {
    fatal_startup();
    return;
  }
  phase_ = Phase::WaitingMarket;
}

PolyMm::Leg* PolyMm::leg(
    strategyframe::InstrumentId instrument_id) noexcept {
  for (Leg& value : legs_)
    if (value.instrument_id == instrument_id) return &value;
  return nullptr;
}

const PolyMm::Leg* PolyMm::leg(
    strategyframe::InstrumentId instrument_id) const noexcept {
  for (const Leg& value : legs_)
    if (value.instrument_id == instrument_id) return &value;
  return nullptr;
}

void PolyMm::on_bbo_update(const strategyframe::BboUpdate& update) {
  if (halt_phase_ == HaltPhase::Halted && phase_ == Phase::Halted) return;
  drain_sidecar();
  Leg* value = leg(update.header.instrument_id);
  bool pending = false;
  if (value == nullptr) {
    for (Leg& candidate : pending_legs_) {
      if (candidate.instrument_id == update.header.instrument_id) {
        value = &candidate;
        pending = true;
        break;
      }
    }
  }
  if (value == nullptr) return;
  value->bid = update.bid.price;
  value->ask = update.ask.price;
  value->bid_value = Fixed(update.bid.price);
  value->ask_value = Fixed(update.ask.price);
  value->book_generation = update.header.book_generation;
  value->last_bbo_ns = context_->now_ns();
  if (!pending) pnl_->update_bbo(update);
  drive();
}

void PolyMm::drain_sidecar() {
  if (!sidecar_) return;
  SidecarEvent event;
  std::size_t budget = 128;
  while (budget-- != 0 && sidecar_->try_pop(event)) {
    switch (event.kind) {
      case SidecarEvent::Kind::FairPrice:
        fairprice_live_ = signal_->update(event.fair_price);
        latest_fair_wall_ns_ = event.fair_price.wall_ns;
        break;
      case SidecarEvent::Kind::FairPriceDisconnected:
        fairprice_live_ = false;
        begin_stop_opening();
        cancel_all();
        break;
    }
  }
}

void PolyMm::on_instrument_catalog(
    const strategyframe::InstrumentCatalogInfo& catalog) {
  handle_catalog(catalog);
}

void PolyMm::handle_catalog(
    const strategyframe::InstrumentCatalogInfo& catalog) {
  if (catalog.venue != strategyframe::Venue::Polymarket ||
      catalog.product != strategyframe::ProductType::BinaryOption) {
    return;
  }
  for (std::size_t index = 0; index < catalogs_.size(); ++index) {
    if (!is_next_physical_catalog(catalogs_[index], catalog)) continue;
    const bool new_physical =
        pending_catalogs_[index].instrument_id != catalog.instrument_id;
    pending_catalogs_[index] = catalog;
    if (new_physical) {
      auto& pending = pending_legs_[index];
      pending = {};
      pending.outcome = index == 0 ? Outcome::Up : Outcome::Down;
      pending.instrument_id = catalog.instrument_id;
      pending.price_scale = catalog.price_scale;
      pending.quantity_scale = catalog.quantity_scale;
      pending.tick_units = catalog.tick_size;
      pending.tick_size =
          Fixed({catalog.tick_size, catalog.price_scale, {}});
    }
    begin_rollover();
    return;
  }
}

void PolyMm::begin_rollover() {
  if (halt_phase_ == HaltPhase::Halted ||
      phase_ == Phase::RolloverDraining ||
      phase_ == Phase::SettlementManual) {
    return;
  }
  phase_ = Phase::RolloverDraining;
  cancel_all();
}

bool PolyMm::books_ready(std::uint64_t now_ns) const noexcept {
  return book_tradable(legs_[0].bid_value, legs_[0].ask_value) &&
         book_tradable(legs_[1].bid_value, legs_[1].ask_value) &&
         bbo_fresh(now_ns, legs_[0].last_bbo_ns, parameters_.max_bbo_age_ms) &&
         bbo_fresh(now_ns, legs_[1].last_bbo_ns, parameters_.max_bbo_age_ms) &&
         bbo_skew_ok(legs_[0].last_bbo_ns, legs_[1].last_bbo_ns,
                     parameters_.max_bbo_skew_ms);
}

bool PolyMm::pending_window_ready(std::uint64_t now_ns) const noexcept {
  if (!context_ || !catalog_rollover_ready(pending_catalogs_)) return false;
  for (std::size_t index = 0; index < pending_legs_.size(); ++index) {
    const Leg& leg = pending_legs_[index];
    if (!context_->execution_ready(
            pending_catalogs_[index].instrument_id) ||
        leg.book_generation == 0 ||
        !book_tradable(leg.bid_value, leg.ask_value) ||
        !bbo_fresh(now_ns, leg.last_bbo_ns,
                   parameters_.max_bbo_age_ms)) {
      return false;
    }
  }
  return bbo_skew_ok(pending_legs_[0].last_bbo_ns,
                     pending_legs_[1].last_bbo_ns,
                     parameters_.max_bbo_skew_ms);
}

void PolyMm::drive() {
  if (!context_ || halt_phase_ == HaltPhase::Halted) return;
  if (phase_ == Phase::WaitingReconcile) {
    finish_reconcile();
    return;
  }
  if (latest_fair_wall_ns_ != 0 &&
      WallNowNs() > latest_fair_wall_ns_ + 2'000'000'000ULL) {
    fairprice_live_ = false;
    begin_stop_opening();
    cancel_all();
  }
  if (halt_phase_ == HaltPhase::Flattening) {
    drive_flattening();
    maybe_complete_halt();
    return;
  }
  if (phase_ == Phase::RolloverDraining) {
    drive_rollover();
    return;
  }
  if (phase_ == Phase::WaitingMarket) {
    if (legs_[0].book_generation != 0 &&
        legs_[1].book_generation != 0 &&
        context_->execution_ready(legs_[0].instrument_id) &&
        context_->execution_ready(legs_[1].instrument_id) &&
        books_ready(context_->now_ns())) {
      phase_ = Phase::Trading;
    } else {
      return;
    }
  }
  if (phase_ != Phase::Trading) return;
  const auto left =
      catalogs_[0].expiry_time_ns > WallNowNs()
          ? catalogs_[0].expiry_time_ns - WallNowNs()
          : 0;
  if (near_expiry(left, parameters_.no_trade_before_expiry_ms)) {
    begin_rollover();
    drive_rollover();
    return;
  }
  for (Leg& value : legs_) drive_leg(value);
  if (halt_phase_ == HaltPhase::Running) evaluate_signal();
}

void PolyMm::drive_flattening() {
  bool active = false;
  for (Leg& value : legs_) {
    if (is_active_order(value.order.state)) {
      active = true;
      cancel_leg(value);
      continue;
    }
    const double quantity = pnl_->quantity(value.instrument_id);
    if (below_minimum(quantity, parameters_.minimum_order_size)) {
      value.order.state = OrderState::Dust;
      continue;
    }
    if (quantity > 0.0 && allows_close_submit(value.order.state))
      submit_close(value, true);
    if (is_active_order(value.order.state)) active = true;
  }
  if (!active &&
      catalogs_[0].expiry_time_ns != 0 &&
      WallNowNs() >= catalogs_[0].expiry_time_ns && !pnl_->flat()) {
    enter_settlement_manual();
  }
}

void PolyMm::drive_leg(Leg& value) {
  if (value.order.state == OrderState::NeedsReconcile) {
    poll_uncertain(value, context_->now_ns());
    return;
  }
  if (!allows_close(halt_phase_) ||
      !allows_close_submit(value.order.state)) {
    return;
  }
  const PnlView position = pnl_->view(value.instrument_id);
  if (below_minimum(position.quantity, parameters_.minimum_order_size)) {
    value.order.state = OrderState::Dust;
    return;
  }
  if (position.quantity <= 0.0) return;
  const bool stop = stop_loss_triggered(
      position, value.tick_size, parameters_.stop_loss_ticks,
      parameters_.taker_fee_bps, parameters_.slippage_ticks);
  submit_close(value, stop || halt_phase_ != HaltPhase::Running);
}

void PolyMm::evaluate_signal() {
  if (!fairprice_live_ || !signal_->ready() ||
      !allows_open(halt_phase_) || !books_ready(context_->now_ns())) {
    return;
  }
  for (const Leg& value : legs_)
    if (is_active_order(value.order.state)) return;
  const auto evaluation = signal_->evaluate_executable(
      legs_[0].bid_value, legs_[0].ask_value, legs_[1].bid_value,
      legs_[1].ask_value, legs_[0].tick_size,
      catalogs_[0].expiry_time_ns > WallNowNs()
          ? catalogs_[0].expiry_time_ns - WallNowNs()
          : 0);
  Leg* selected = nullptr;
  if (evaluation.direction == SignalEvaluation::Direction::Up)
    selected = &legs_[0];
  else if (evaluation.direction == SignalEvaluation::Direction::Down)
    selected = &legs_[1];
  if (selected != nullptr && allows_new_open(selected->order.state) &&
      risk_allows_open(*selected)) {
    submit_open(*selected);
  }
}

bool PolyMm::risk_allows_open(const Leg& value) const noexcept {
  if (value.order.state == OrderState::Dust) return false;
  const double up = pnl_->quantity(parameters_.up_instrument_id);
  const double down = pnl_->quantity(parameters_.down_instrument_id);
  const double current =
      value.outcome == Outcome::Up ? up : down;
  const double other =
      value.outcome == Outcome::Up ? down : up;
  if (below_minimum(current, parameters_.minimum_order_size)) return false;
  return allows_open(
      current, other, parameters_.order_shares,
      parameters_.max_single_side_shares,
      parameters_.imbalance_tolerance_shares);
}

strategyframe::FixedPoint PolyMm::quantity_fixed(
    const Leg& value, double quantity) const noexcept {
  double factor = 1.0;
  for (std::uint8_t index = 0; index < value.quantity_scale; ++index)
    factor *= 10.0;
  return {static_cast<std::int64_t>(std::llround(quantity * factor)),
          value.quantity_scale, {}};
}

strategyframe::FixedPoint PolyMm::price_fixed(
    const Leg& value, double price) const noexcept {
  double factor = 1.0;
  for (std::uint8_t index = 0; index < value.price_scale; ++index)
    factor *= 10.0;
  std::int64_t units =
      static_cast<std::int64_t>(std::llround(price * factor));
  if (value.tick_units > 0)
    units = units / value.tick_units * value.tick_units;
  return {units, value.price_scale, {}};
}

strategyframe::OrderRequest PolyMm::make_order(
    const Leg& value, strategyframe::Side side,
    strategyframe::TimeInForce tif,
    strategyframe::FixedPoint quantity,
    strategyframe::FixedPoint price, bool post_only) const {
  strategyframe::OrderRequest request;
  request.account_id = parameters_.account_id;
  request.instrument_id = value.instrument_id;
  request.side = side;
  request.type = strategyframe::OrderType::Limit;
  request.time_in_force = tif;
  request.flags =
      post_only ? strategyframe::OrderFlag::PostOnly : 0;
  request.quantity = quantity;
  request.price = price;
  return request;
}

void PolyMm::submit_open(Leg& value) {
  if (!allows_open(halt_phase_) || !allows_new_open(value.order.state) ||
      value.ask_value <= 0.0) {
    return;
  }
  auto request = make_order(
      value, strategyframe::Side::Buy, strategyframe::TimeInForce::FOK,
      quantity_fixed(value, parameters_.order_shares), value.ask, false);
  const auto submitted = context_->place_order(request);
  if (!submitted) {
    value.order.last_error = static_cast<std::int32_t>(submitted.error);
    begin_stop_opening();
    return;
  }
  value.order.state = OrderState::PendingOpen;
  value.order.purpose = OrderPurpose::Open;
  value.order.token = submitted.value;
  value.order.price = request.price;
  value.order.started_ns = context_->now_ns();
  value.order.saw_fill = false;
  if (!pnl_->remember_order(
          submitted.value, value.instrument_id, request.side,
          OrderPurpose::Open, window_generation_)) {
    (void)context_->cancel(submitted.value);
    begin_flattening();
  }
}

void PolyMm::submit_close(Leg& value, bool force) {
  if (!allows_close(halt_phase_) ||
      value.order.state == OrderState::NeedsReconcile) {
    return;
  }
  const PnlView position = pnl_->view(value.instrument_id);
  if (below_minimum(position.quantity, parameters_.minimum_order_size)) {
    value.order.state = OrderState::Dust;
    return;
  }
  if (position.quantity <= 0.0 || value.bid_value <= 0.0 ||
      !allows_close_submit(value.order.state)) {
    return;
  }
  strategyframe::TimeInForce tif = strategyframe::TimeInForce::GTC;
  strategyframe::FixedPoint price{};
  bool post_only = true;
  if (force) {
    tif = strategyframe::TimeInForce::IOC;
    price = value.bid;
    post_only = false;
  } else {
    const double target = close_target_price(
        position.average_cost, value.tick_size,
        parameters_.profit_spread_ticks, parameters_.taker_fee_bps,
        parameters_.slippage_ticks);
    if (value.bid_value >= target) {
      tif = strategyframe::TimeInForce::IOC;
      price = value.bid;
      post_only = false;
    } else {
      const double improved =
          value.ask_value -
          static_cast<double>(parameters_.close_improve_ticks) *
              value.tick_size;
      const double desired = std::max(target, improved);
      price = price_fixed(value, desired);
      if (Fixed(price) <= value.bid_value) return;
    }
  }
  auto request = make_order(
      value, strategyframe::Side::Sell, tif,
      quantity_fixed(value, position.quantity), price, post_only);
  const auto submitted = context_->place_order(request);
  if (!submitted) {
    value.order.last_error = static_cast<std::int32_t>(submitted.error);
    if (halt_phase_ == HaltPhase::Flattening) {
      if (++value.order.cancel_attempts >= 5) enter_settlement_manual();
    } else {
      begin_flattening();
    }
    return;
  }
  value.order.state =
      force ? OrderState::PendingForce : OrderState::PendingClose;
  value.order.purpose =
      force ? OrderPurpose::ForceFlatten : OrderPurpose::Close;
  value.order.token = submitted.value;
  value.order.price = request.price;
  value.order.started_ns = context_->now_ns();
  value.order.saw_fill = false;
  if (!pnl_->remember_order(
          submitted.value, value.instrument_id, request.side,
          value.order.purpose, window_generation_)) {
    (void)context_->cancel(submitted.value);
    begin_flattening();
  }
}

void PolyMm::cancel_leg(Leg& value) {
  if (!is_active_order(value.order.state) ||
      value.order.state == OrderState::PendingCancel ||
      value.order.state == OrderState::NeedsReconcile) {
    return;
  }
  if (context_->cancel(value.order.token)) {
    value.order.state = OrderState::PendingCancel;
    ++value.order.cancel_attempts;
    return;
  }
  if (++value.order.cancel_attempts >= 5) begin_stop_opening();
}

void PolyMm::cancel_all() {
  for (Leg& value : legs_) cancel_leg(value);
}

bool PolyMm::position_reconciled() const {
  if (!pnl_->flat()) return false;
  for (const Leg& value : legs_) {
    const auto position = context_->find_position(
        parameters_.account_id, value.instrument_id,
        strategyframe::PositionSide::Net);
    if (position && position.value.quantity.value != 0) return false;
    if (!position && position.error != strategyframe::Error::NotFound)
      return false;
  }
  return true;
}

void PolyMm::drive_rollover() {
  bool active_order = false;
  for (Leg& value : legs_) {
    if (is_active_order(value.order.state)) {
      active_order = true;
      cancel_leg(value);
      continue;
    }
    const double quantity = pnl_->quantity(value.instrument_id);
    if (below_minimum(quantity, parameters_.minimum_order_size)) {
      value.order.state = OrderState::Dust;
      if (catalogs_[0].expiry_time_ns != 0 &&
          WallNowNs() >= catalogs_[0].expiry_time_ns) {
        enter_settlement_manual();
        return;
      }
      continue;
    }
    if (quantity > 0.0) {
      submit_close(value, true);
      if (is_active_order(value.order.state)) {
        active_order = true;
      } else if (catalogs_[0].expiry_time_ns != 0 &&
                 WallNowNs() >= catalogs_[0].expiry_time_ns) {
        enter_settlement_manual();
        return;
      }
    }
  }
  if (active_order || (context_ && !context_->open_orders().empty())) return;
  if (!position_reconciled()) {
    if (catalogs_[0].expiry_time_ns != 0 &&
        WallNowNs() >= catalogs_[0].expiry_time_ns) {
      enter_settlement_manual();
    } else {
      begin_flattening();
    }
    return;
  }
  if (!pending_window_ready(context_->now_ns())) {
    if (catalogs_[0].expiry_time_ns != 0 &&
        WallNowNs() >= catalogs_[0].expiry_time_ns) {
      enter_settlement_manual();
    }
    return;
  }
  (void)activate_next_window();
}

bool PolyMm::activate_next_window() {
  if (!pending_window_ready(context_->now_ns())) return false;
  if (!pnl_->activate_window(pending_catalogs_[0].instrument_id,
                             pending_catalogs_[1].instrument_id)) {
    begin_flattening();
    return false;
  }
  catalogs_ = pending_catalogs_;
  pending_catalogs_ = {};
  parameters_.up_instrument_id = catalogs_[0].instrument_id;
  parameters_.down_instrument_id = catalogs_[1].instrument_id;
  ++window_generation_;
  signal_->reset();
  fairprice_live_ = false;
  for (std::size_t index = 0; index < legs_.size(); ++index) {
    Leg& value = legs_[index];
    const Leg market = pending_legs_[index];
    value.instrument_id = catalogs_[index].instrument_id;
    value.price_scale = catalogs_[index].price_scale;
    value.quantity_scale = catalogs_[index].quantity_scale;
    value.tick_units = catalogs_[index].tick_size;
    value.tick_size = Fixed(
        {catalogs_[index].tick_size, catalogs_[index].price_scale, {}});
    value.bid = market.bid;
    value.ask = market.ask;
    value.bid_value = market.bid_value;
    value.ask_value = market.ask_value;
    value.book_generation = market.book_generation;
    value.last_bbo_ns = market.last_bbo_ns;
    clear_leg_order(value.order);
    value.order.state = OrderState::Idle;
  }
  pending_legs_ = {};
  phase_ = Phase::WaitingMarket;
  return true;
}

void PolyMm::apply_lifecycle(Leg& value, const LifecycleResult& result) {
  if (result.release_ledger)
    pnl_->clear_terminal_order(value.order.token);
  commit_lifecycle(value.order, result);
  if (result.stop_opening) begin_stop_opening();
}

void PolyMm::poll_uncertain(Leg& value, std::uint64_t now_ns) {
  const auto found = context_->find_order(value.order.token);
  if (found && is_terminal_status(found.value.status)) {
    strategyframe::ExecutionUpdate update;
    update.kind = strategyframe::ExecutionUpdate::Kind::Order;
    update.status = found.value.status;
    update.token = found.value.token;
    update.instrument_id = found.value.instrument_id;
    update.remaining_quantity = found.value.remaining_quantity;
    const auto decision = apply_order_event(
        value.order, update, pnl_->quantity(value.instrument_id),
        parameters_.minimum_order_size);
    apply_lifecycle(value, decision);
    return;
  }
  const std::uint64_t timeout =
      static_cast<std::uint64_t>(parameters_.startup_reconcile_timeout_ms) *
      1'000'000ULL;
  if (value.order.started_ns != 0 &&
      now_ns > value.order.started_ns + timeout) {
    begin_flattening();
  }
}

void PolyMm::on_order_update(
    const strategyframe::ExecutionUpdate& update) {
  if (update.kind == strategyframe::ExecutionUpdate::Kind::Fill && pnl_) {
    const std::uint64_t duplicates_before = pnl_->duplicate_fills();
    if (!pnl_->apply_fill(update)) {
      if (pnl_->duplicate_fills() != duplicates_before) return;
      if (halt_phase_ != HaltPhase::Halted) begin_flattening();
      return;
    }
  }
  Leg* value = leg(update.instrument_id);
  if (value == nullptr &&
      update.kind == strategyframe::ExecutionUpdate::Kind::CommandResult) {
    for (Leg& candidate : legs_) {
      if (same_token(candidate.order.token, update.token)) {
        value = &candidate;
        break;
      }
    }
  }
  if (value != nullptr) {
    const auto decision = apply_order_event(
        value->order, update, pnl_->quantity(value->instrument_id),
        parameters_.minimum_order_size);
    apply_lifecycle(*value, decision);
  }
  if (halt_phase_ != HaltPhase::Halted) drive();
}

void PolyMm::on_oms_status(
    const strategyframe::OmsStatusUpdate& update) {
  note_oms_reconcile(reconcile_, update);
  if (update.error != 0) {
    if (phase_ == Phase::WaitingReconcile) {
      reconcile_.oms_reconcile_failed = true;
      finish_reconcile();
      return;
    }
    begin_flattening();
    return;
  }
  if (phase_ == Phase::WaitingReconcile) finish_reconcile();
}

void PolyMm::on_timer(const strategyframe::TimerEvent&) {
  if (halt_phase_ == HaltPhase::Halted) return;
  drain_sidecar();
  const std::uint64_t now = context_->now_ns();
  if (phase_ == Phase::WaitingReconcile) {
    finish_reconcile();
    return;
  }
  if (now - last_position_check_ns_ >= 100'000'000ULL) {
    last_position_check_ns_ = now;
    for (const Leg& value : legs_) {
      if (catalogs_[value.outcome == Outcome::Up ? 0U : 1U]
                  .expiry_time_ns <= WallNowNs()) {
        continue;
      }
      const auto position = context_->find_position(
          parameters_.account_id, value.instrument_id,
          strategyframe::PositionSide::Net);
      const double framework =
          position ? Fixed(position.value.quantity) : 0.0;
      if ((!position &&
           position.error != strategyframe::Error::NotFound) ||
          std::abs(framework - pnl_->quantity(value.instrument_id)) >
              1e-9) {
        begin_flattening();
        return;
      }
    }
  }
  for (Leg& value : legs_) {
    if (value.order.state == OrderState::NeedsReconcile) {
      poll_uncertain(value, now);
      continue;
    }
    if ((value.order.state == OrderState::PendingClose ||
         value.order.state == OrderState::PendingOpen ||
         value.order.state == OrderState::PendingForce) &&
        now > value.order.started_ns +
                  static_cast<std::uint64_t>(
                      parameters_.max_order_resting_ms) *
                      1'000'000ULL) {
      cancel_leg(value);
    } else if (value.order.state == OrderState::PendingClose) {
      const PnlView position = pnl_->view(value.instrument_id);
      const double target = close_target_price(
          position.average_cost, value.tick_size,
          parameters_.profit_spread_ticks, parameters_.taker_fee_bps,
          parameters_.slippage_ticks);
      const double improved =
          value.ask_value -
          static_cast<double>(parameters_.close_improve_ticks) *
              value.tick_size;
      const auto desired = price_fixed(value, std::max(target, improved));
      if (std::llabs(desired.value - value.order.price.value) >
          parameters_.requote_deviation_ticks * value.tick_units) {
        cancel_leg(value);
      }
    }
  }
  drive();
}

}  // namespace polymm
